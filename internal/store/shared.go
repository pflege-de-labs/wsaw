package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file holds the defaults an Options gets when a field is left unset, the
// retry policy, the deadline one operation runs under, and the envelope a
// stored result document is written and read back through.
//
// Most of that is what every kind of store needs and none of them owns, because
// the index a store keeps is the only thing that differs between them. A store
// whose index is rows and a store whose index is objects in a bucket still retry
// the same way, still bound one call the same way, and still record the same
// three facts about a document — where it is, how big it should be, and what it
// must hash to. Keeping those here is what stops a second implementation from
// acquiring a second answer to a question the first one already settled, which
// is the drift Tenet 12's seam exists to prevent.
//
// **Four of the defaults are not shared, and are here anyway**: timeout (a
// SQLite busy_timeout), maxOpenConns, maxIdleConns and connMaxLifetime are a
// connection pool, and the bucket-index store opens no connection —
// config.validate refuses those settings for that driver precisely because they
// mean nothing to it. They live here because Options is one type across all the
// drivers, so its defaulting is one set of methods on that type; splitting them
// would put four of the methods of one struct in another file and leave a
// reader asking which file the fifth is in. Nothing but connect() and the
// dialects call them.

// Defaults for an Options with the field left unset. They live together, and
// apart from the fields they default, so that "what does an unset MaxIdleConns
// mean" has one place to be answered rather than one place per store — a
// question, per the note above, that only a SQL store can ask.

func (o *Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}

	return slog.Default()
}

func (o *Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}

	return 5 * time.Second
}

func (o *Options) maxOpenConns() int {
	if o.MaxOpenConns > 0 {
		return o.MaxOpenConns
	}

	// Enough for the browser pool's scans plus the web interface, small
	// enough that several wsaw instances on one shared database do not
	// exhaust its connection limit between them.
	return 8
}

func (o *Options) maxIdleConns() int {
	if o.MaxIdleConns > 0 {
		return o.MaxIdleConns
	}

	return min(2, o.maxOpenConns())
}

func (o *Options) connMaxLifetime() time.Duration {
	if o.ConnMaxLifetime > 0 {
		return o.ConnMaxLifetime
	}

	// Shorter than a typical server-side idle timeout, so wsaw retires a
	// connection before the server drops it under a scan.
	return 30 * time.Minute
}

func (o *Options) maxAttempts() int {
	if o.MaxAttempts > 0 {
		return o.MaxAttempts
	}

	// Three attempts covers a failover or a restart without turning a
	// genuinely broken database into a long wait.
	return 3
}

func (o *Options) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return o.RetryBackoff
	}

	return 200 * time.Millisecond
}

// retrier runs one store operation, trying again while the failure is
// transient. It exists because the thing a store talks to is reached over a
// network: a restart, a failover, a deadlock or a 503 from object storage is an
// ordinary event, and failing a scan's result on the first dropped packet would
// lose an observation for no good reason.
//
// A permanent failure — a constraint violation, a malformed statement, a
// missing table, a refused credential — is returned on the first attempt.
// Retrying one only makes the failure slower and hides its cause. Which
// failures those are is the one part that differs per store, so it arrives as
// a predicate rather than being decided here.
//
// The operations retried through it are idempotent by construction: the writes
// are upserts keyed by identity, or writes to a key derived from what is being
// written, and the deletes are by identity. The exception is an audit entry,
// which is an append: if a connection drops after the server committed but
// before wsaw heard so, a retry can write it twice. A duplicated audit line is
// visible and harmless; a lost approval record is neither, so this is the right
// way round.
//
// It is a value rather than an interface: there is one policy, configured three
// ways, and a second implementation of "try again" would be a second answer to
// a question already settled.
type retrier struct {
	// attempts is the total number of tries, not the number of retries. One
	// means the policy is off.
	attempts int
	// backoff is the delay before the second attempt; it doubles thereafter.
	backoff time.Duration
	// onRetry is called before each retry. A retry nobody can see is a
	// flapping dependency that looks healthy (Tenet 8). Optional.
	onRetry func(op string, attempt int, err error)
	// transient reports whether this failure is worth another attempt. Nil
	// means nothing is: every failure is returned as it arrives.
	transient func(err error) bool
}

// run performs one operation under the policy.
func (r retrier) run(ctx context.Context, op string, fn func(context.Context) error) error {
	var lastErr error

	for attempt := 1; attempt <= r.attempts; attempt++ {
		if attempt > 1 {
			if err := r.wait(ctx, attempt); err != nil {
				return err
			}
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}

		// A cancelled caller is not a broken dependency, and retrying its work
		// would only delay the shutdown it asked for.
		if ctx.Err() != nil {
			return err
		}

		if r.transient == nil || !r.transient(err) {
			return err
		}

		lastErr = err

		// Reported only when another attempt actually follows: a "retrying"
		// line after the last attempt would overstate what happened.
		if r.onRetry != nil && attempt < r.attempts {
			r.onRetry(op, attempt, err)
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", op, r.attempts, lastErr)
}

// wait sleeps before an attempt, with exponential backoff, and gives up as
// soon as the context does.
func (r retrier) wait(ctx context.Context, attempt int) error {
	delay := r.backoff * (1 << (attempt - 2))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// opTimeout bounds one store operation, database or bucket. Every call has a
// deadline, for the same reason every browser interaction does: nothing waits
// for ever.
const opTimeout = 30 * time.Second

// opCtx bounds a single store operation started from outside any caller's
// context — which is every exported method that reads or writes a result, none
// of which takes one.
func opCtx() (context.Context, context.CancelFunc) {
	return opCtxFrom(context.Background())
}

// opCtxFrom bounds one operation inside a longer-running one. The document
// migration is the only such caller: it may run for minutes in total, and each
// statement and bucket call within it still gets the same deadline every other
// store operation has.
func opCtxFrom(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, opTimeout)
}

// resultRef is what an index carries in place of the document: where the bytes
// are, how many of them there should be, and what they must hash to.
//
// It is the whole of what a store has to record about a document, which is why
// it is here and not beside the columns one store happens to read it from: an
// index that recorded less could not tell truncated evidence from intact
// evidence, and an index that recorded more would be describing the document
// twice.
type resultRef struct {
	ref    string
	size   int64
	digest string
}

// documentDigest is what an index records for its document, and what a
// document read back out of the bucket is checked against.
func documentDigest(document []byte) string {
	sum := sha256.Sum256(document)

	return hex.EncodeToString(sum[:])
}

// decodeDocument turns the bytes of a stored document back into the result
// that was written, having first checked them against what the index recorded.
//
// Checked before decoding, and never after: JSON that parses is not evidence
// that the bytes are the ones written, and a result rebuilt from altered or
// truncated bytes would be reported as a scan rather than as damage (Tenet 5).
func decodeDocument(ref resultRef, document []byte) (*model.Result, error) {
	if err := verifyDocument(ref, document); err != nil {
		return nil, err
	}

	var res model.Result

	if err := json.Unmarshal(document, &res); err != nil {
		// The bytes are the ones that were written — the digest says so — and
		// they still do not decode, which leaves the stored evidence unusable
		// rather than absent.
		return nil, fmt.Errorf("decoding artifact %s: %w: %w", ref.ref, ErrCorrupt, err)
	}

	return &res, nil
}

// verifyDocument compares an artifact against the size and digest the index
// recorded when it was written. Size first, because it is free and catches the
// truncation that is the likeliest of the two.
func verifyDocument(ref resultRef, document []byte) error {
	if int64(len(document)) != ref.size {
		return fmt.Errorf("artifact %s holds %d bytes where the store recorded %d: %w",
			ref.ref, len(document), ref.size, ErrCorrupt)
	}

	if digest := documentDigest(document); digest != ref.digest {
		return fmt.Errorf("artifact %s hashes to %s where the store recorded %s: %w",
			ref.ref, digest, ref.digest, ErrCorrupt)
	}

	return nil
}

// defaultCheckpointGrace is how long a compaction checkpoint must have been
// visible before the loose entries it covers may be deleted.
//
// A day, because the deletion is irreversible and the thing it depends on —
// that every reader now sees the checkpoint — is a property no provider
// promises on any schedule. A day is far longer than the convergence window of
// any object store gocloud reaches, and the cost of being generous is a
// listing that carries a few hundred extra keys for a while (Story 8.10).
const defaultCheckpointGrace = 24 * time.Hour

// clock is the store's source of "now".
//
// Injectable rather than time.Now called in place, because the store whose
// index is in the bucket derives its key order and its retention decisions
// from a timestamp, and a test that had to wait for the real clock to reach a
// grace period would be a sleeping test (AGENTS §5).
func (o *Options) clock() func() time.Time {
	if o.Now != nil {
		return o.Now
	}

	return time.Now
}

func (o *Options) checkpointGrace() time.Duration {
	if o.CheckpointGrace > 0 {
		return o.CheckpointGrace
	}

	return defaultCheckpointGrace
}

// version identifies the build in the one object that records who wrote an
// index. Empty is recorded as unknown rather than as an empty string, so the
// object reads as a fact either way.
func (o *Options) version() string {
	if o.Version == "" {
		return "unknown"
	}

	return o.Version
}

// The four artifact reads, shaped the way the store interface hands them out.
//
// They live here, on the bucket, rather than on either store, because reading
// evidence is the one thing the two kinds of store do identically: the bytes
// are in the same bucket under the same content-addressed key whether the index
// that named them is rows or objects (Story 8.1, AC2). A second copy of them on
// the second store would be a second answer to questions the first already
// settled — the idle bound on a streaming read most of all, which is subtle
// enough that two of it would eventually differ.
//
// Writing an artifact is deliberately not here. The write is where the two
// stores genuinely differ: a SQL store also takes a claim row on the reference
// so retention cannot collect it before the scan commits, and a store whose
// index is objects has no row to take one in.

// artifactInfo reports what the bucket knows about an artifact without reading
// it.
func (b *bucket) artifactInfo(ctx context.Context, ref string) (ArtifactInfo, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	obj, err := b.stat(ctx, ref)
	if err != nil {
		return ArtifactInfo{}, absentArtifact(err)
	}

	return ArtifactInfo{Size: obj.size, Digest: artifactDigest(ref), ModTime: obj.modTime}, nil
}

// artifactBytes reads a stored artifact whole.
//
// It takes no context because neither caller has one: both are store methods
// that read evidence on behalf of a decode, and the deadline that bounds them
// is the store's own (see opCtx).
func (b *bucket) artifactBytes(ref string) ([]byte, error) {
	ctx, cancel := opCtx()
	defer cancel()

	data, err := b.get(ctx, ref)
	if err != nil {
		return nil, absentArtifact(err)
	}

	return data, nil
}

// artifactReader opens a stored artifact for streaming, under a bound on
// progress rather than on the transfer.
//
// The bucket has opTimeout to open the object and opTimeout to produce each
// further piece of it, and a read that stops producing bytes for that long is
// abandoned — but a transfer that keeps flowing runs as long as it takes. A
// fixed deadline on the whole read would be a size and bandwidth limit dressed
// as a timeout: a forty-megabyte document to a client on a slow link takes
// longer than any per-operation deadline, and cutting it off would abort the
// response after Content-Length had been declared and the status sent, with
// nothing left to tell the reader. The reader who has actually gone away is
// caught by their own request's context, which is this call's parent (Story
// 8.7, AC6).
func (b *bucket) artifactReader(ctx context.Context, ref string) (*ArtifactReader, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Armed before the open, so the open is bounded by it too, and re-armed by
	// every piece of the body that arrives.
	idle := time.AfterFunc(opTimeout, cancel)

	release := func() {
		idle.Stop()
		cancel()
	}

	stream, err := b.newReader(ctx, ref)
	if err != nil {
		release()

		return nil, absentArtifact(err)
	}

	return &ArtifactReader{
		ReadCloser: stream,
		ArtifactInfo: ArtifactInfo{
			Size:    stream.size,
			Digest:  artifactDigest(ref),
			ModTime: stream.modTime,
		},
		idle:    idle,
		release: release,
	}, nil
}

// artifactURL signs a URL a reader can fetch one artifact from directly.
func (b *bucket) artifactURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	signed, err := b.signedURL(ctx, ref, ttl)
	if err != nil {
		return "", absentArtifact(err)
	}

	return signed, nil
}

// resultDocument fetches a result document from the bucket and checks it
// against what the index recorded about it.
//
// It is here, on the bucket, because reading a document back is the same
// operation whichever kind of index named it: the reference, the size and the
// digest are one resultRef either way, and the three outcomes below are
// judgements about the bucket rather than about the index. A second copy of
// them on the second store would be a second opinion on when evidence is lost
// and when it is corrupt, which is exactly the pair a caller must never see
// merged.
//
// The check is the point. Once the payload is outside the index it is outside
// the index's guarantees too: a truncated upload, a lifecycle rule that
// replaced an object, or a key rewritten by something else would otherwise be
// returned as the scan (Story 8.2, AC6). A mismatch is corruption, and
// corruption is reported, never served as evidence.
//
// The two ways a reference can fail are told apart rather than merged into the
// bucket's own answer. A key that is gone while the index still names it is
// lost evidence (ErrEvidenceGone), not a scan that never happened; a reference
// that is not the shape this store writes came out of wsaw's own index, so it
// is a corrupt index entry (ErrCorrupt), not an absent artifact.
func (b *bucket) resultDocument(ctx context.Context, ref resultRef) (*model.Result, error) {
	document, err := b.get(ctx, ref.ref)

	switch {
	case errors.Is(err, errInvalidRef):
		return nil, fmt.Errorf("this result names artifact %q, which is not a reference this store wrote: %w: %w",
			truncateForMessage(ref.ref), ErrCorrupt, err)

	case errors.Is(err, ErrNotFound):
		// The index survived and the object did not — a lifecycle rule on the
		// bucket, a restore without its matching index, a sweep that collected
		// too much. Reported as its own fault so that a caller which skips a
		// result that does not exist cannot skip this one just as quietly
		// (Story 8.2, AC5). The bucket's own ErrNotFound is deliberately not
		// carried through: a caller testing for it would go on treating
		// deleted evidence as a scan that never happened.
		return nil, fmt.Errorf("artifact %s: %w", ref.ref, ErrEvidenceGone)

	case err != nil:
		return nil, err
	}

	return decodeDocument(ref, document)
}
