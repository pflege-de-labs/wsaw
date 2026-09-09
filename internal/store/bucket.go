package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/gcerrors"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// This file, plus bucket_cloud.go, is the whole of wsaw's knowledge of where
// evidence physically lives (Story 8.1, AC1). Everything above it names an
// artifact by a reference and nothing else; no provider type and no
// filesystem call for an artifact belongs outside these two files.
//
// Which providers a binary can reach is a build-time decision, taken because
// the three cloud SDKs cost more than the rest of wsaw put together: the
// default build links fileblob alone and measures about 6 MB above the
// baseline, while adding S3, GCS and Azure takes it past 77 MB. The cloud
// drivers therefore live behind the "cloudblob" build tag (Story 8.8, AC3),
// and the default binary opens no cloud SDK and resolves no credential chain
// (Story 8.8, AC4).
//
// The in-memory driver is registered by bucket_test.go and by nothing else. A
// bucket that forgets everything at shutdown is the right thing for a test and
// the worst thing an operator could accidentally configure, so the shipped
// binary refuses "mem://" by name rather than accepting evidence into a void.

const (
	// artifactDirMode and artifactFileMode keep evidence readable only by the
	// account wsaw runs as. Screenshots and stored bodies can carry personal
	// data, so the move to a bucket does not widen what the directory-backed
	// implementation granted (Tenet 19).
	artifactDirMode  os.FileMode = 0o700
	artifactFileMode os.FileMode = 0o600

	// refSeparator joins the two halves of a reference. It is "/" for every
	// provider, including the local one — fileblob maps it to the platform's
	// separator itself.
	refSeparator = "/"

	// schemeSeparator is what tells a URL from a plain filesystem path. A
	// path is far more likely to contain a colon than a URL is to omit the
	// slashes, so this is the reliable direction to test.
	schemeSeparator = "://"

	// fileScheme is handled here rather than by gocloud's own opener; see
	// openBucket for why.
	fileScheme = "file"

	// cloudProbeScheme is what cloudBuildHint looks for to decide whether the
	// cloud drivers are linked in. Any of the three would answer the
	// question; s3 is the one an operator is likeliest to be reaching for.
	cloudProbeScheme = "s3"

	// digestLength is the hex length of the SHA-256 sum a reference carries.
	digestLength = 64

	// maxKindLength bounds the first segment. Kinds are written by wsaw, not
	// by a page, so this is a sanity limit rather than a defence.
	maxKindLength = 64

	// The kinds this store writes, which are also the only first segments a
	// reference may carry (validKind). They are named here rather than at
	// their call sites because retention reads the same list from the other
	// end: an object under any other prefix was not written by this store, and
	// a sweep must not treat it as garbage of its own making (Story 8.5, AC4).
	artifactKindBody = "body"
	// artifactKindScreenshot is a kind on its own and the prefix of the two
	// capture writes, "screenshot-before-consent" and
	// "screenshot-after-consent".
	artifactKindScreenshot = "screenshot"
	// kindSuffixSeparator joins a screenshot to the moment it was taken.
	kindSuffixSeparator = "-"

	// artifactContentType is written deliberately rather than sniffed. The
	// bytes come from a hostile page, and a bucket that advertises them as
	// text/html would let a provider's own URL serve a captured script as a
	// document. Every artifact is opaque; what a response looks like is the
	// HTTP layer's decision, from the kind (Story 5.17, AC5).
	artifactContentType = "application/octet-stream"

	// artifactListPageSize bounds one listing round trip. A bucket holding
	// every document of every scan has more keys than a sweep should hold in
	// memory, so listing pages rather than accumulates.
	artifactListPageSize = 256
)

// errInvalidRef marks a reference that is not one this store wrote. It is a
// sentinel so a caller can tell a malformed reference — which is a bug or an
// attack — from evidence that has been pruned.
var errInvalidRef = errors.New("invalid artifact reference")

// ErrSigningUnsupported marks a bucket whose provider cannot produce a signed
// URL — the local directory, which has no server to honour one, or an
// S3-compatible endpoint reached with credentials that cannot sign.
//
// It is exported and a sentinel because the HTTP layer has to tell it from a
// bucket that is broken: a provider that cannot sign is a reason to go on
// serving the bytes through wsaw, which is what an operator had before they
// asked for redirects, while a bucket that is unreachable is a failure worth
// reporting (Story 8.7, AC3).
var ErrSigningUnsupported = errors.New("this artifact bucket cannot sign URLs")

// bucket is the store's one door to where artifacts live.
//
// It exists so that "the artifact directory" becomes "the artifact bucket": a
// URL an operator points at object storage, with the local directory as one
// implementation rather than the only one. The methods are deliberately the
// small set the store actually needs — put, get, stream, stat, exists,
// delete, list — because a wider surface would be a second storage API to
// keep honest across four providers.
type bucket struct {
	b        *blob.Bucket
	location string
	retry    retryFunc

	// dir is the directory a local bucket is rooted at, and empty for every
	// other provider. It exists for one question — is this bucket still there
	// — which the local driver cannot be asked any other way; see reachable.
	dir string
}

// retryFunc has the shape of retrier.run on purpose.
//
// A bucket is a network dependency for every provider but the local one, so a
// dropped connection is an ordinary event — the same argument Story 4.7 made
// for a server database. The policy for how often and how long to try again
// already exists, tuned by Options.MaxAttempts and Options.RetryBackoff and
// observable through Options.OnRetry, so the bucket borrows it instead of
// growing a second one that would drift.
type retryFunc func(ctx context.Context, op string, fn func(context.Context) error) error

// retryOnce is what a bucket uses until a store lends it a policy. A bucket
// opened on its own — by a test, or by a tool that has no store — should
// still work, and one attempt is the honest default when nobody has said how
// many are wanted.
func retryOnce(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

// setRetry lends the bucket a retry policy, normally the store's own.
func (b *bucket) setRetry(fn retryFunc) {
	if fn != nil {
		b.retry = fn
	}
}

// transientBucketError marks a bucket failure another attempt could fix.
//
// It satisfies net.Error because that is the shape the store's existing
// classifier recognises as worth retrying: every dialect falls back to
// isTransientMessage, which treats a net.Error as transient. Presenting a
// retryable bucket failure in that shape is what lets the store's retrier drive
// bucket operations without a bucket-specific branch inside it, and without
// this package holding two ideas of what "try again" means.
//
// The wrapper is transparent: it reports the underlying message and unwraps
// to the provider's error, so gcerrors.Code and errors.Is still see through
// it.
type transientBucketError struct{ err error }

func (e *transientBucketError) Error() string { return e.err.Error() }

func (e *transientBucketError) Unwrap() error { return e.err }

// Timeout reports false because a retryable bucket failure is not necessarily
// a timeout, and nothing in wsaw distinguishes the two.
func (e *transientBucketError) Timeout() bool { return false }

// Temporary is the answer that matters: this failure is worth another go.
func (e *transientBucketError) Temporary() bool { return true }

// worthRetrying reads the provider's error code rather than its message.
//
// Message text differs between S3, GCS, Azure and the local filesystem, and
// changes when their SDKs do; gcerrors is the one classification every driver
// is obliged to produce (AC8). A missing key or a denied request is permanent
// — retrying it only makes the failure slower and hides its cause.
func worthRetrying(err error) bool {
	switch gcerrors.Code(err) {
	case gcerrors.ResourceExhausted, gcerrors.Internal, gcerrors.DeadlineExceeded:
		// Throttling, a 5xx from the service, or a request the service itself
		// gave up on. All three are what a second attempt is for.
		return true

	case gcerrors.NotFound, gcerrors.PermissionDenied, gcerrors.InvalidArgument,
		gcerrors.Unimplemented, gcerrors.FailedPrecondition, gcerrors.AlreadyExists,
		gcerrors.Canceled:
		return false

	default:
		// Unknown is where a transport failure arrives, because a driver that
		// could not reach its service has no service code to report. That is
		// the same case the dialects meet, so it gets the same answer.
		return isTransientMessage(err)
	}
}

// openBucket opens the artifact bucket named by location.
//
// A location with no scheme is a filesystem path, which is what an existing
// installation has configured and what the default deployment keeps using
// (AC2). Anything else is a URL, and which schemes resolve depends on how the
// binary was built.
func openBucket(ctx context.Context, location string) (*bucket, error) {
	if location == "" {
		// Refused rather than defaulted. An empty location resolves to the
		// process's working directory, and a bucket rooted there would let a
		// retention sweep enumerate — and eventually delete — files that are
		// not artifacts. Naming the directory is the caller's job.
		return nil, errors.New("store: no artifact location configured; give a directory path or a bucket URL")
	}

	if !IsArtifactURL(location) {
		return openFileBucket(location)
	}

	u, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("artifact location %s is not a valid URL: %w",
			secret.RedactURL(location), parseFailure(err))
	}

	// The "file" scheme is resolved here instead of through gocloud's URL
	// opener because AC2 makes the on-disk layout a promise: an existing
	// artifact directory has to stay readable and re-writable with no
	// migration. That layout depends on options the URL can otherwise set
	// wrongly — sidecar metadata files beside every artifact, staging through
	// os.TempDir, world-readable directories — so a file bucket is always
	// built the one way that reproduces it.
	if u.Scheme == fileScheme {
		path, err := filePathFromURL(u)
		if err != nil {
			return nil, err
		}

		return openFileBucket(path)
	}

	if !blob.DefaultURLMux().ValidBucketScheme(u.Scheme) {
		return nil, unsupportedSchemeError(location, u.Scheme)
	}

	b, err := blob.OpenBucket(ctx, location)
	if err != nil {
		return nil, fmt.Errorf("opening artifact bucket %s: %w", secret.RedactURL(location), err)
	}

	return &bucket{b: b, location: location, retry: retryOnce}, nil
}

// IsArtifactURL reports whether location names a bucket by URL rather than a
// directory on local disk.
//
// It is exported because configuration draws the same line and must draw it
// the same way: store.artifactDir is a path and store.artifactURL is a URL,
// and each has to refuse the other's value rather than pass it on to be
// misread here (Story 8.6, AC2).
func IsArtifactURL(location string) bool {
	return strings.Contains(location, schemeSeparator)
}

// ValidateArtifactURL reports whether this binary could open the bucket raw
// names, without opening it.
//
// Configuration is validated completely at load, with the line an operator
// has to change (Tenet 15), and a URL whose scheme no provider in this build
// answers is exactly the mistake that would otherwise surface as a failed
// scan hours later. What this cannot answer is whether the bucket exists and
// accepts a write — that needs a request, and it is the startup probe's job
// (Story 8.6, AC4).
func ValidateArtifactURL(raw string) error {
	if !IsArtifactURL(raw) {
		return fmt.Errorf(
			"%q is not a URL; a directory on local disk is named by store.artifactDir instead",
			truncateForMessage(raw),
		)
	}

	u, err := url.Parse(raw)
	if err != nil {
		// The URL is redacted even while being rejected: a malformed URL can
		// still carry a working credential in the part that did parse. That is
		// also why the parse failure is unwrapped first — url.Error quotes the
		// whole input back, which would put the credential in the message this
		// line exists to keep it out of.
		return fmt.Errorf("%s is not a valid URL: %w", secret.RedactURL(raw), parseFailure(err))
	}

	if u.Scheme == fileScheme {
		path, err := filePathFromURL(u)
		if err != nil {
			return err
		}

		if path == "" {
			// Redacted like every branch around it: a file URL that names no
			// directory can still carry userinfo or a query string, and the
			// message this returns is printed by "wsaw config --check" and
			// logged at startup.
			return fmt.Errorf("%s names no directory; a file bucket is a path, as in \"file:///var/lib/wsaw/artifacts\"",
				secret.RedactURL(raw))
		}

		return nil
	}

	if !blob.DefaultURLMux().ValidBucketScheme(u.Scheme) {
		return unsupportedSchemeError(raw, u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("%s names no bucket; the host of the URL is the bucket, as in \"s3://my-bucket\"",
			secret.RedactURL(raw))
	}

	if path := strings.Trim(u.Path, refSeparator); path != "" {
		// Refused rather than ignored. Every cloud opener gocloud provides
		// takes the bucket or container from the URL's host alone and drops
		// its path — s3blob, gcsblob and azureblob all do — so
		// "s3://bucket/wsaw" would put evidence at the root of the bucket
		// while the operator read the configuration as scoping wsaw to a
		// subtree. A retention sweep walks what it is given, so the difference
		// is not cosmetic: configuration that is silently ignored is worse
		// than configuration that is refused (Tenet 15).
		return fmt.Errorf(
			"%s names a path within the bucket, which this provider ignores; give the bucket alone, as in \"%s://%s\"",
			secret.RedactURL(raw), u.Scheme, u.Host,
		)
	}

	return nil
}

// parseFailure returns why a URL would not parse, without the URL.
func parseFailure(err error) error {
	var perr *url.Error
	if errors.As(err, &perr) && perr.Err != nil {
		return perr.Err
	}

	return err
}

// unsupportedSchemeError names the URL, the build, and what that build can
// actually reach.
//
// All three, because "unsupported scheme" alone reads as a bug in wsaw: which
// schemes resolve is a build-time decision (Story 8.8, AC3), so an operator
// whose s3:// URL is refused needs to be told that they are running the build
// without the cloud providers rather than sent to the source to find out
// (Tenet 15).
func unsupportedSchemeError(location, scheme string) error {
	schemes := blob.DefaultURLMux().BucketSchemes()
	urls := make([]string, 0, len(schemes))

	for _, s := range schemes {
		urls = append(urls, s+schemeSeparator)
	}

	return fmt.Errorf(
		"%s uses the %q scheme, which %s cannot open; it opens %s, and a directory path written without a scheme%s",
		secret.RedactURL(location), scheme, buildDescription(), strings.Join(urls, ", "), cloudBuildHint(),
	)
}

// buildDescription names the binary the operator is running, in the words the
// release uses for it.
func buildDescription() string {
	if cloudProvidersLinked() {
		return "this build (made with -tags cloudblob)"
	}

	return "this build (the default one, made without -tags cloudblob)"
}

// cloudBuildHint tells an operator how to get the cloud providers when they
// are not compiled in, and says nothing when they already are.
func cloudBuildHint() string {
	if cloudProvidersLinked() {
		return ""
	}

	return "; s3://, gs:// and azblob:// need a build made with -tags cloudblob"
}

// cloudProvidersLinked reads the registered schemes rather than a build flag,
// so a message about what this binary supports cannot disagree with what it
// actually does.
func cloudProvidersLinked() bool {
	for _, s := range blob.DefaultURLMux().BucketSchemes() {
		if s == cloudProbeScheme {
			return true
		}
	}

	return false
}

// filePathFromURL applies the same host convention gocloud's file opener
// does, so a URL written from its documentation keeps working: an empty host
// or "localhost" means an absolute path, and "." signals a relative one.
func filePathFromURL(u *url.URL) (string, error) {
	switch u.Host {
	case "", "localhost":
		return u.Path, nil

	case ".":
		return strings.TrimPrefix(u.Path, refSeparator), nil

	default:
		return "", fmt.Errorf(
			"artifact location %s names host %q; a file bucket is local, so its host must be empty, \"localhost\" or \".\"",
			secret.RedactURL(u.String()), u.Host,
		)
	}
}

// openFileBucket roots a bucket at a local directory.
func openFileBucket(dir string) (*bucket, error) {
	if dir == "" {
		return nil, errors.New("store: the artifact directory is empty; give a path")
	}

	// Created up front and 0700, because evidence can carry personal data
	// (Tenet 19). fileblob's own create_dir would make the directory too, but
	// with a 0777 default, and an artifact tree that is world-readable
	// because nobody passed an option is not a default worth defending.
	if err := os.MkdirAll(dir, artifactDirMode); err != nil {
		return nil, fmt.Errorf("creating artifact directory %s: %w", dir, err)
	}

	b, err := fileblob.OpenBucket(dir, &fileblob.Options{
		CreateDir:   true,
		DirFileMode: artifactDirMode,
		// No sidecars. With metadata written to a ".attrs" file beside every
		// artifact, the directory would stop being the layout an existing
		// installation has, and reading one written by the old code would
		// start depending on a file that is not there (AC2). wsaw stores no
		// per-object metadata anyway.
		Metadata: fileblob.MetadataDontWrite,
		// Temporary files are created next to their final name rather than in
		// os.TempDir, so the rename that publishes a write is a rename within
		// one directory — atomic — instead of a copy across filesystems that
		// could fail half way, and so evidence never transits a world-visible
		// temporary directory.
		NoTempDir: true,
	})
	if err != nil {
		return nil, fmt.Errorf("opening artifact directory %s: %w", dir, err)
	}

	return &bucket{b: b, location: dir, retry: retryOnce, dir: dir}, nil
}

// String names the bucket for a log line or an error, with any credentials
// its URL carries removed.
func (b *bucket) String() string { return secret.RedactURL(b.location) }

// close releases the bucket.
func (b *bucket) close() error {
	if err := b.b.Close(); err != nil {
		return fmt.Errorf("closing artifact bucket %s: %w", b, err)
	}

	return nil
}

// do runs one bucket operation under the borrowed retry policy, having first
// decided whether its failure is worth another attempt. The decision is made
// here, where the provider's error codes are understood, and communicated to
// the policy in the shape it already recognises.
func (b *bucket) do(ctx context.Context, op string, fn func(context.Context) error) error {
	retry := b.retry
	if retry == nil {
		retry = retryOnce
	}

	return retry(ctx, op, func(ctx context.Context) error {
		err := fn(ctx)
		if err == nil {
			return nil
		}

		// A cancelled caller is not a flaky bucket, and its own deadline
		// arrives as a code that otherwise reads as retryable.
		if ctx.Err() != nil || !worthRetrying(err) {
			return err
		}

		return &transientBucketError{err: err}
	})
}

// put stores an artifact under its content address and returns the reference.
//
// The key layout is the one the directory implementation wrote — the kind, a
// slash, and the hex SHA-256 of the bytes — so the same screenshot stored
// twice costs one object and an existing directory keeps its shape (AC2, AC3).
func (b *bucket) put(ctx context.Context, kind string, data []byte) (string, error) {
	if !validKind(kind) {
		return "", fmt.Errorf("artifact kind %q is not one this store writes: %w",
			truncateForMessage(kind), errInvalidRef)
	}

	sum := sha256.Sum256(data)
	ref := kind + refSeparator + hex.EncodeToString(sum[:])

	// A key that exists already holds these exact bytes, so rewriting it
	// would spend a round trip to produce no change — and captured evidence
	// is immutable, so a write that could change it is not wanted at all
	// (Tenet 4, AC3).
	present, err := b.exists(ctx, ref)
	if err != nil {
		return "", err
	}

	if present {
		return ref, nil
	}

	if err := b.do(ctx, "storing an artifact", func(ctx context.Context) error {
		return b.write(ctx, ref, data)
	}); err != nil {
		return "", artifactError("storing", ref, err)
	}

	return ref, nil
}

// write performs one attempt at creating a key.
//
// The write is atomic from a reader's point of view (AC4), and each provider
// gives that differently: fileblob writes to a temporary file in the target
// directory and renames it into place on Close, which is why this code does
// not reimplement the dance the old PutArtifact did by hand; the cloud
// drivers publish an object only when the upload completes. In both cases a
// crash, a cancelled context or a failed transfer leaves no key rather than a
// truncated one.
func (b *bucket) write(ctx context.Context, ref string, data []byte) error {
	w, err := b.b.NewWriter(ctx, ref, &blob.WriterOptions{
		ContentType: artifactContentType,
		// Two scans of the same site finishing at once can content-address to
		// the same key. The condition makes the loser of that race a no-op
		// instead of a rewrite, on every provider that has one.
		IfNotExist:  true,
		BeforeWrite: restrictLocalFilePermissions,
	})
	if err != nil {
		return err
	}

	if _, err := w.Write(data); err != nil {
		// Close abandons the partial write; the error that matters is the one
		// from Write, so Close's is dropped deliberately here.
		_ = w.Close()

		return err
	}

	if err := w.Close(); err != nil {
		if gcerrors.Code(err) == gcerrors.FailedPrecondition {
			// Another writer got there first. Content addressing means the
			// key already holds what this call was about to write, so this is
			// the no-op AC3 asks for rather than a failure.
			return nil
		}

		return err
	}

	return nil
}

// restrictLocalFilePermissions tightens the file the local driver is about to
// write to 0600.
//
// fileblob creates its temporary file 0666 minus the umask and the rename
// that publishes it keeps that mode, so without this an artifact's
// permissions would depend on the umask rather than on wsaw's decision that
// evidence is private (Tenet 19) — a decision the directory implementation
// made explicitly and this one must not quietly drop.
//
// Every other driver rejects the *os.File, and the callback is a no-op there:
// permissions on a remote object are the bucket's own business.
func restrictLocalFilePermissions(as func(any) bool) error {
	var f *os.File

	if !as(&f) || f == nil {
		return nil
	}

	if err := f.Chmod(artifactFileMode); err != nil {
		return fmt.Errorf("restricting artifact permissions: %w", err)
	}

	return nil
}

// artifactKindProbe is the prefix a writability probe is written under. Its
// own kind, so a probe object can never be mistaken for evidence, and so a
// sweep that reasons about kinds knows what it is looking at.
const artifactKindProbe = "probe"

// probeBytes is the size of a probe object: enough that a bucket has to accept
// a real body, small enough to cost nothing.
const probeBytes = 16

// probeWritable checks that the bucket will accept a write, and removes what
// it wrote.
//
// It exists for the migration that moves every stored document into the bucket
// (Story 8.4, AC5): a bucket that is unreachable or read-only has to fail
// before the first row is touched, naming itself, rather than failing on row
// ninety thousand of a hundred thousand.
//
// The contents are random. A probe with fixed contents would content-address
// to a key that is already there after the first run, and put() would then
// answer from the existing object without writing anything — a read-only
// bucket would pass the check it exists to fail.
//
// The probe carries its own deadline. It is called from the document
// migration, whose context deliberately spans minutes, and it is a real
// network write for every provider but the local one: an endpoint that accepts
// the connection and never answers would otherwise hang the start of wsaw with
// no error and no timeout, which is the opposite of the actionable failure
// Story 8.4, AC5 asks for (Story 8.1, AC6).
func (b *bucket) probeWritable(ctx context.Context) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	unique := make([]byte, probeBytes)
	if _, err := rand.Read(unique); err != nil {
		return fmt.Errorf("preparing a write probe for artifact bucket %s: %w", b, err)
	}

	ref, err := b.put(ctx, artifactKindProbe, unique)
	if err != nil {
		return fmt.Errorf("artifact bucket %s did not accept a test write: %w", b, err)
	}

	// The removal is best effort and its error is deliberately dropped: a
	// bucket that took the write is writable, which is the question this
	// method asks. What a failed removal leaves behind is sixteen bytes under
	// a prefix nothing reads, and refusing to start over litter would block an
	// upgrade for no reason. A bucket that will not delete is the retention
	// sweep's problem (Story 8.5), not this one.
	_ = b.remove(ctx, ref)

	return nil
}

// reachable reports whether the bucket still answers.
//
// This is the readiness question rather than the startup one (Story 8.6, AC5):
// a wsaw whose evidence store has gone away is not ready, however healthy its
// browser is, and it is the same argument that puts the database in readiness
// (Story 4.7). It lists one key rather than writing one, for two reasons. A
// probe object per poll would cost a request, a delete and a piece of litter
// every few seconds against a bucket that charges per request; and whether the
// bucket accepts writes has already been established at startup by
// probeWritable, where paying for it once is worth it.
//
// It also does not retry. A readiness probe is polled and the caller bounds
// it: an answer of "not ready yet" arriving promptly is more useful to an
// orchestrator than a correct answer arriving after three backoffs.
func (b *bucket) reachable(ctx context.Context) error {
	if b.dir != "" {
		return b.directoryReachable()
	}

	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	found, err := b.b.IsAccessible(ctx)
	if err != nil {
		return fmt.Errorf("the artifact bucket %s is not reachable: %w", b, err)
	}

	if !found {
		// A bucket that answers "no such bucket" is a configuration mistake or
		// a bucket somebody deleted, and either way wsaw has nowhere to put
		// the next scan's evidence.
		return fmt.Errorf("the artifact bucket %s does not exist", b)
	}

	return nil
}

// directoryReachable asks the filesystem the question a listing cannot answer.
//
// The local driver's listing walks the tree and skips whatever it cannot read,
// returning no error, so an artifact directory that has gone away — an
// unmounted volume being the case that matters — lists as empty and looks
// perfectly healthy. That is the shape of failure Tenet 8 is about: wsaw would
// report itself ready while every scan's evidence went nowhere. A stat is the
// honest question to ask of a filesystem, and it costs nothing.
func (b *bucket) directoryReachable() error {
	info, err := os.Stat(b.dir)
	if err != nil {
		return fmt.Errorf("the artifact directory %s is not reachable: %w", b.dir, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("the artifact directory %s is not a directory any more", b.dir)
	}

	return nil
}

// get reads a whole artifact.
//
// It stays for the callers that genuinely need the bytes in hand — a stored
// body being diffed, a result document being decoded. Anything that only
// forwards the bytes should use newReader instead (AC7).
func (b *bucket) get(ctx context.Context, ref string) ([]byte, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}

	var data []byte

	if err := b.do(ctx, "reading an artifact", func(ctx context.Context) error {
		read, err := b.b.ReadAll(ctx, ref)
		if err != nil {
			return err
		}

		data = read

		return nil
	}); err != nil {
		return nil, artifactError("reading", ref, err)
	}

	return data, nil
}

// artifactStream is an artifact opened for reading, with what the object's
// own attributes said about it.
//
// The attributes travel with the stream because the provider reported them in
// the round trip that opened it. Asking the bucket again for a size and a
// modification time it has already given would cost a second request per
// artifact served, and against a bucket that charges per request that is a
// bill rather than a rounding error (Story 8.7, AC4).
type artifactStream struct {
	io.ReadCloser

	size    int64
	modTime time.Time
}

// newReader opens an artifact for streaming.
//
// This is what the HTTP layer serves from: a multi-megabyte document must not
// have to exist in memory on both sides of the call, and the size is needed
// for Content-Length before a byte has been read (AC7).
//
// Only opening the reader is retried. Once bytes are flowing there is nothing
// to retry — the response has begun — and pretending otherwise would mean
// buffering the object, which is the thing this method exists to avoid.
func (b *bucket) newReader(ctx context.Context, ref string) (*artifactStream, error) {
	if err := validateRef(ref); err != nil {
		return nil, err
	}

	var r *blob.Reader

	if err := b.do(ctx, "opening an artifact", func(ctx context.Context) error {
		opened, err := b.b.NewReader(ctx, ref, nil)
		if err != nil {
			return err
		}

		r = opened

		return nil
	}); err != nil {
		return nil, artifactError("reading", ref, err)
	}

	return &artifactStream{ReadCloser: r, size: r.Size(), modTime: r.ModTime()}, nil
}

// signedURL asks the provider for a URL that grants one GET of one artifact
// for ttl, so a reader can fetch it from the bucket without the bytes passing
// through wsaw at all.
//
// Whether that is possible is the provider's answer rather than a build-time
// fact: S3, GCS and Azure sign, the local directory has no signer and no
// server to honour one, and an S3-compatible endpoint reached with credentials
// that cannot sign refuses too. Each of those arrives as
// gcerrors.Unimplemented, and is translated into ErrSigningUnsupported so a
// caller can go back to serving the bytes itself rather than failing a request
// that an operator's opt-in was only ever meant to make cheaper.
func (b *bucket) signedURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	if err := validateRef(ref); err != nil {
		return "", err
	}

	var signed string

	if err := b.do(ctx, "signing an artifact URL", func(ctx context.Context) error {
		u, err := b.b.SignedURL(ctx, ref, &blob.SignedURLOptions{Expiry: ttl})
		if err != nil {
			return err
		}

		signed = u

		return nil
	}); err != nil {
		if gcerrors.Code(err) == gcerrors.Unimplemented {
			return "", fmt.Errorf("%w: %w", ErrSigningUnsupported, err)
		}

		return "", fmt.Errorf("signing a URL for artifact %s in bucket %s: %w", ref, b, err)
	}

	return signed, nil
}

// stat reports what the bucket knows about an artifact without reading it:
// how large it is, and when it was written.
//
// It exists so the interface can tell "this evidence has been pruned" from
// "this evidence is here" without fetching a megabyte of PNG to find out
// (Story 5.17, AC3) — which matters more against a bucket, where the fetch
// costs a round trip and a request.
func (b *bucket) stat(ctx context.Context, ref string) (artifactObject, error) {
	if err := validateRef(ref); err != nil {
		return artifactObject{}, err
	}

	obj := artifactObject{ref: ref}

	if err := b.do(ctx, "reading artifact attributes", func(ctx context.Context) error {
		attrs, err := b.b.Attributes(ctx, ref)
		if err != nil {
			return err
		}

		obj.size, obj.modTime = attrs.Size, attrs.ModTime

		return nil
	}); err != nil {
		return artifactObject{}, artifactError("reading", ref, err)
	}

	return obj, nil
}

// exists reports whether an artifact is stored, without distinguishing that
// from an error the way stat does.
func (b *bucket) exists(ctx context.Context, ref string) (bool, error) {
	if err := validateRef(ref); err != nil {
		return false, err
	}

	var present bool

	if err := b.do(ctx, "checking for an artifact", func(ctx context.Context) error {
		found, err := b.b.Exists(ctx, ref)
		if err != nil {
			return err
		}

		present = found

		return nil
	}); err != nil {
		return false, artifactError("checking for", ref, err)
	}

	return present, nil
}

// remove deletes one artifact. A key that is already gone is reported as
// ErrNotFound, so a retention sweep can treat "someone else collected it" as
// the success it is rather than as a failure.
func (b *bucket) remove(ctx context.Context, ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}

	if err := b.do(ctx, "deleting an artifact", func(ctx context.Context) error {
		return b.b.Delete(ctx, ref)
	}); err != nil {
		return artifactError("deleting", ref, err)
	}

	return nil
}

// artifactObject is one key a listing reported: what it is called, how many
// bytes it holds, and when the bucket last wrote it.
//
// The write time is here because the retention sweep cannot do without it. A
// key that nothing references is either garbage from an interrupted write or
// an artifact whose row has not been committed yet, and the only thing that
// tells those apart from outside is how long ago it was written (Story 8.5,
// AC3).
type artifactObject struct {
	ref     string
	size    int64
	modTime time.Time
}

// list walks the keys under a prefix, calling fn for each.
//
// A callback and a page at a time, rather than a slice: a bucket that holds
// every document of every scan has more keys than a caller should have to
// hold in memory, and the retention sweep that uses this (Story 8.5, AC3)
// cares about one key at a time. An error from fn stops the walk and is
// returned as is, so a caller can abandon a listing without inventing a
// sentinel.
func (b *bucket) list(ctx context.Context, prefix string, fn func(obj artifactObject) error) error {
	if err := validatePrefix(prefix); err != nil {
		return err
	}

	token := blob.FirstPageToken

	for len(token) > 0 {
		var (
			page []*blob.ListObject
			next []byte
		)

		if err := b.do(ctx, "listing artifacts", func(ctx context.Context) error {
			// Assigned only on success: a failed page must leave the token
			// pointing at the page still to be fetched, not at nothing.
			objects, nextToken, err := b.b.ListPage(ctx, token, artifactListPageSize,
				&blob.ListOptions{Prefix: prefix})
			if err != nil {
				return err
			}

			page, next = objects, nextToken

			return nil
		}); err != nil {
			return fmt.Errorf("listing artifacts under %q in bucket %s: %w",
				truncateForMessage(prefix), b, err)
		}

		for _, obj := range page {
			if obj.IsDir {
				continue
			}

			if err := fn(artifactObject{ref: obj.Key, size: obj.Size, modTime: obj.ModTime}); err != nil {
				return err
			}
		}

		token = next
	}

	return nil
}

// bucketEntry is one thing a raw listing reported: an object, or — when the
// listing asked for a delimiter — the common prefix standing for everything
// below one path segment.
//
// It is a separate type from artifactObject because a raw listing has not
// judged what it found. An artifactObject is a key this store wrote, in the
// shape validateRef admits; this is whatever is in the bucket, including the
// index tree, another tool's files and a directory that is not a kind at all.
// Keeping them apart is what stops an unjudged key from reaching a function
// that deletes.
type bucketEntry struct {
	key     string
	size    int64
	modTime time.Time

	// dir marks a common prefix rather than an object. It appears only when
	// the listing asked for a delimiter.
	dir bool
}

// walk lists what the bucket holds under prefix, without judging the prefix.
//
// It is the one listing in this file that does not put its prefix through
// validatePrefix, and the reason is that its callers are looking for exactly
// what that check excludes: the retention sweep of Story 8.10 has to be able
// to see the objects it must not touch in order to report them and leave them
// alone (Story 8.5, AC4), and it cannot ask "what is in the bucket that wsaw
// did not write" through a door that only admits what wsaw wrote. It is
// read-only, and every prefix it is given comes either from this package's own
// constants or from a name this same bucket reported — never from a stored
// document, so none of Tenet 9's untrusted input reaches it.
//
// A delimiter of "/" collapses everything below one path segment into a single
// dir entry, which is how the kinds present in a bucket are discovered without
// listing every object under them. The empty delimiter walks the whole subtree,
// as list does.
func (b *bucket) walk(ctx context.Context, prefix, delimiter string, fn func(entry bucketEntry) error) error {
	token := blob.FirstPageToken

	for len(token) > 0 {
		var (
			page []*blob.ListObject
			next []byte
		)

		if err := b.do(ctx, "listing bucket objects", func(ctx context.Context) error {
			// Assigned only on success, so a failed page leaves the token
			// pointing at the page still to be fetched rather than at nothing.
			objects, nextToken, err := b.b.ListPage(ctx, token, artifactListPageSize,
				&blob.ListOptions{Prefix: prefix, Delimiter: delimiter})
			if err != nil {
				return err
			}

			page, next = objects, nextToken

			return nil
		}); err != nil {
			return fmt.Errorf("listing %q in bucket %s: %w", truncateForMessage(prefix), b, err)
		}

		for _, obj := range page {
			entry := bucketEntry{key: obj.Key, size: obj.Size, modTime: obj.ModTime, dir: obj.IsDir}

			if err := fn(entry); err != nil {
				return err
			}
		}

		token = next
	}

	return nil
}

// artifactError gives a bucket failure the shape the rest of the store
// already understands.
//
// A missing key stays ErrNotFound so every caller that tells "pruned" from
// "broken" keeps that distinction (Story 5.17, AC3; AC9). The provider's code
// is read through gcerrors rather than matched against its message, because
// the message differs between four providers and changes when their SDKs do.
func artifactError(verb, ref string, err error) error {
	if gcerrors.Code(err) == gcerrors.NotFound {
		return fmt.Errorf("artifact %s: %w", ref, ErrNotFound)
	}

	return fmt.Errorf("%s artifact %s: %w", verb, ref, err)
}

// validateRef refuses anything that is not the shape this store writes.
//
// References come out of stored results, which are built from page-controlled
// data, so they are untrusted (Tenet 9). The check is a whitelist rather than
// a traversal test on purpose, and that is stricter than the filepath.Rel
// defence it replaces: "is this outside the root" has to be right about every
// escape a path can express — "..", an absolute path, a backslash a provider
// treats as a separator, a percent-encoded separator a provider decodes, a
// Unicode form a filesystem normalises — while "is this exactly
// <kind>/<sha256hex>" has to be right about one thing. A reference this store
// did not write names nothing this store should serve.
func validateRef(ref string) error {
	kind, digest, found := strings.Cut(ref, refSeparator)
	if !found || !validKind(kind) || !isDigest(digest) {
		return fmt.Errorf("artifact reference %q is not one this store wrote: %w",
			truncateForMessage(ref), errInvalidRef)
	}

	return nil
}

// isArtifactRef reports whether a key is one this store could have written.
//
// It is validateRef as a question rather than as a failure, for the retention
// sweep's walk of the bucket: a key that is not ours is not an error to
// explain to anybody, it is a key to leave alone and count (Story 8.5, AC4).
func isArtifactRef(ref string) bool { return validateRef(ref) == nil }

// validatePrefix accepts what a listing may be narrowed to: the whole bucket,
// one kind, or a partially typed reference within a kind. It is the same
// whitelist validateRef applies, relaxed only in that the digest may be
// incomplete.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}

	kind, digest, found := strings.Cut(prefix, refSeparator)

	if !validKind(kind) || (found && (len(digest) > digestLength || !isHex(digest))) {
		return fmt.Errorf("artifact prefix %q is not one this store wrote: %w",
			truncateForMessage(prefix), errInvalidRef)
	}

	return nil
}

// validKind accepts the kinds this store writes and nothing else: "body",
// "result", the write probe, and a screenshot with or without the moment it
// was taken ("screenshot-before-consent", "screenshot-after-consent").
//
// It is an allow-list rather than a character class because the retention
// sweep reads it as one. A sweep walks the bucket and deletes what no result
// references, so anything the shape test accepts is a key the sweep believes
// wsaw wrote — and a character class accepts every "<lowercase word>/<64 hex>"
// in the bucket, including another tool's objects and another wsaw
// deployment's. Naming the kinds keeps that decision to the four prefixes this
// store actually produces (Story 8.5, AC3 and AC4).
//
// The screenshot suffix is still checked byte-wise, which rejects every
// non-ASCII byte along with control characters, dots, slashes and backslashes
// rather than reasoning about what each of them might mean somewhere.
func validKind(kind string) bool {
	if len(kind) > maxKindLength {
		return false
	}

	switch kind {
	case artifactKindBody, artifactKindResult, artifactKindProbe, artifactKindScreenshot:
		return true
	}

	suffix, ok := strings.CutPrefix(kind, artifactKindScreenshot+kindSuffixSeparator)

	return ok && validKindSuffix(suffix)
}

// validKindSuffix accepts what may follow "screenshot-": the moment the shot
// was taken, in the same restricted alphabet a kind itself is written in.
func validKindSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}

	for i := range len(suffix) {
		c := suffix[i]

		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(suffix)-1:
		default:
			return false
		}
	}

	return true
}

// artifactDigest returns the SHA-256 sum a reference carries, and the empty
// string for one that carries none.
//
// The digest is the second half of every key this store writes, so it is
// already in hand wherever a reference is — which is what makes a strong
// cache validator for an artifact free to produce (Story 8.7, AC4). Reading it
// out of the reference belongs here, with the rest of what knows that layout.
func artifactDigest(ref string) string {
	_, digest, found := strings.Cut(ref, refSeparator)
	if !found || !isDigest(digest) {
		return ""
	}

	return digest
}

// isDigest reports whether s is a complete lowercase hex SHA-256 sum, which
// is the only second segment this store writes.
func isDigest(s string) bool { return len(s) == digestLength && isHex(s) }

// isHex reports whether s is lowercase hexadecimal. Lowercase only: hex.Encode
// produces lowercase, so an uppercase digest is a reference from somewhere
// else, and on a case-insensitive filesystem accepting both would make two
// references name one object.
func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// truncateForMessage keeps a hostile reference from turning an error message,
// and the log line that carries it, into a vehicle for a page's payload.
func truncateForMessage(s string) string {
	const limit = 80

	if len(s) <= limit {
		return s
	}

	return s[:limit] + "…"
}
