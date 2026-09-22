package store

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// This file is how compression meets the bucket (Story 4.8, against the seam
// Story 8.1 introduced).
//
// The rule the whole thing rests on is that **a reference names the evidence,
// never the object that holds it**. `body/<sha256 of the body>` is what a
// result records, and whether those bytes are sitting in the bucket gzipped is
// a storage decision taken later, possibly by a different build, and possibly
// differently for two artifacts of the same scan. Nothing above the bucket sees
// the difference (Story 4.8, AC2 and AC4).
//
// So the bucket keeps two spellings of one key: `<ref>` holds the bytes as they
// arrived, and `<ref>.gz` holds them gzipped. That is the same convention the
// directory implementation used before the bucket existed, and keeping it is
// not nostalgia — an installation that compressed its artifacts on local disk
// and then pointed this build at that directory must still be able to read
// them, because the artifact directory *is* the default bucket (Story 8.1, AC2).
//
// **Two spellings cost a request when the first guess is wrong**, which on a
// directory is a stat syscall and on object storage is a round trip. The guess
// is therefore made from what this deployment writes rather than fixed: see
// storedFirst. It is a guess about cost and never about correctness — both
// spellings are always tried before an artifact is reported absent.

// storedFirst is the spelling to try first for a reference.
//
// A wrong guess is one wasted request, so the order follows what this
// deployment actually writes. With compression on, a body, a probe and a result
// document are gzipped and a screenshot is not: PNG is already deflate, so it
// fails compressArtifact's ratio gate every time (Story 4.8, AC1). Guessing per
// kind rather than per deployment is what keeps the hot read path — a result
// document on every GetResult — at one request.
//
// With compression off the plain spelling is tried first for everything, which
// is right for a deployment that never wrote a compressed artifact and costs
// one extra request per read for one that used to.
func (b *bucket) storedFirst(ref string) (first, second string) {
	if b.compress && compressibleKind(ref) {
		return ref + compressedSuffix, ref
	}

	return ref, ref + compressedSuffix
}

// compressibleKind reports whether artifacts of this reference's kind are
// usually worth compressing. It is a guess about request counts, not a
// decision about what gets compressed — compressArtifact's ratio gate makes
// that one, per artifact, from the bytes themselves.
func compressibleKind(ref string) bool {
	kind, _, ok := strings.Cut(ref, refSeparator)

	return ok && !strings.HasPrefix(kind, artifactKindScreenshot)
}

// setCompression tells the bucket how to pack what it is given, how much an
// artifact may inflate to on the way back, and who is counting.
//
// It is set after the bucket is opened rather than passed to openBucket
// because a bucket opened on its own — by a test, or by a tool that has no
// store — reads compressed artifacts and writes plain ones, which is the safe
// default for something with no configuration behind it.
func (b *bucket) setCompression(on bool, maxBytes int64, onStored func(kind string, original, stored int64)) {
	b.compress = on
	b.maxArtifactBytes = maxBytes
	b.onArtifactStored = onStored
}

// artifactMaxBytes is the cap this bucket inflates an artifact against.
//
// A bucket nobody configured still has one: the bytes are untrusted on the way
// back in whoever opened it, and a gzip member's own claim about its size is
// worth nothing (Story 4.8, AC6).
func (b *bucket) artifactMaxBytes() int64 {
	if b.maxArtifactBytes > 0 {
		return b.maxArtifactBytes
	}

	return defaultMaxArtifactBytes
}

// pack decides how an artifact is stored: under which key, and as which bytes.
//
// The ratio gate is compressArtifact's, so a PNG that gzip cannot improve is
// stored as Chrome produced it and costs nothing to read back.
func (b *bucket) pack(ref string, data []byte) (key string, stored []byte) {
	if !b.compress {
		return ref, data
	}

	if packed, worth := compressArtifact(data); worth {
		return ref + compressedSuffix, packed
	}

	return ref, data
}

// storedKey finds which spelling of a reference the bucket actually holds.
//
// Both are tried before an artifact is called absent, so turning compression
// off never hides what was written while it was on, and turning it on never
// hides what came before (Story 4.8, AC5).
func (b *bucket) storedKey(ctx context.Context, ref string) (string, error) {
	first, second := b.storedFirst(ref)

	found, err := b.exists(ctx, first)
	if err != nil {
		return "", err
	}

	if found {
		return first, nil
	}

	found, err = b.exists(ctx, second)
	if err != nil {
		return "", err
	}

	if found {
		return second, nil
	}

	return "", fmt.Errorf("artifact %s: %w", ref, ErrNotFound)
}

// hasArtifact reports whether the bucket holds an artifact, under either
// spelling.
//
// It is what a caller asking "is this evidence still here" wants, and exists
// as its own method because bucket.exists asks about one key: a check against
// the bare reference alone would call every packed artifact missing, which on
// the paths that ask — a rebuild's evidence survey, a verify — reads as a
// bucket that has lost its evidence (Story 4.8, AC4).
func (b *bucket) hasArtifact(ctx context.Context, ref string) (bool, error) {
	switch _, err := b.storedKey(ctx, ref); {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// unpack turns what a key holds into the evidence the reference names.
//
// A compressed member is inflated under the cap and checked against the digest
// in its reference, which is what separates "this evidence has been corrupted"
// from "here is some evidence": a reference is a content address, so bytes that
// do not hash to it are not the artifact that was asked for, whatever they are
// (Story 4.8, AC6).
func (b *bucket) unpack(ref, key string, stored []byte) ([]byte, error) {
	if !isCompressedKey(key) {
		return stored, nil
	}

	return decompressArtifact(ref, stored, b.artifactMaxBytes())
}

// ArtifactRefOf turns a stored key back into the artifact it holds, by dropping
// the packing suffix if there is one.
//
// It is exported so that a test looking at raw objects can ask the same
// question the store asks, rather than spelling the suffix a second time.
func ArtifactRefOf(key string) string { return strings.TrimSuffix(key, compressedSuffix) }

// isCompressedKey reports whether a stored key holds a gzip member.
func isCompressedKey(key string) bool { return strings.HasSuffix(key, compressedSuffix) }

// validateStoredKey admits the two spellings of an artifact this bucket writes,
// and nothing else.
//
// It is validateRef with the packing suffix allowed, deliberately written as a
// trim and then the existing whitelist rather than as a second pattern: the
// check that a crafted reference cannot escape the bucket has to be right about
// one shape, and this adds a fixed suffix to that shape instead of widening it
// (Tenet 9).
func validateStoredKey(key string) error {
	return validateRef(strings.TrimSuffix(key, compressedSuffix))
}

// inflate wraps a reader over a stored gzip member so that a caller streaming
// an artifact is handed the evidence rather than the packing.
//
// The digest is deliberately not checked here, and that is the one place this
// file gives up a check the whole-read path keeps. Verifying a content address
// means having all of the bytes, and having all of the bytes is exactly what
// streaming exists to avoid: a forty-megabyte document would have to be
// buffered to prove itself before the first byte reached the client (Story 8.1,
// AC7). What still holds is that the bytes inflate — a truncated or corrupt
// member fails the read rather than being served as a short artifact — and the
// callers that decode what they read, which is every caller that could act on
// bad bytes, use the whole-read path.
func inflate(r io.ReadCloser) (io.ReadCloser, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		_ = r.Close()

		return nil, fmt.Errorf("reading a compressed artifact: %w", err)
	}

	return &inflater{Reader: zr, under: r}, nil
}

// inflater closes both halves of a streamed gzip member: the decompressor, and
// the bucket reader underneath it. Closing only the first would leak the
// connection the second holds.
type inflater struct {
	*gzip.Reader

	under io.ReadCloser
}

func (i *inflater) Close() error {
	err := i.Reader.Close()

	if under := i.under.Close(); under != nil && err == nil {
		err = under
	}

	if err != nil {
		return fmt.Errorf("closing a compressed artifact: %w", err)
	}

	return nil
}

// inflatedSize reads the size a stored gzip member declares in its trailer, so
// that a reader is told how much evidence there is rather than how much of it
// is on disk (Story 4.8, AC7).
//
// The last four bytes rather than the whole object: inflating a megabyte of
// JavaScript to count its bytes would cost the transfer this call exists to
// avoid. It is one ranged read, which every provider gocloud reaches supports,
// and on a directory it is a seek.
//
// The trailer is the member's own claim and is modulo 2^32, which is sound
// here because nothing wsaw writes approaches 4 GiB — and because the number
// describes evidence to a human or sizes a Content-Length, and is never used
// to allocate.
func (b *bucket) inflatedSize(ctx context.Context, key string, stored int64) (int64, error) {
	const trailerBytes = 4

	if stored < trailerBytes {
		return 0, fmt.Errorf("artifact %s: %w", key, errArtifactTruncated)
	}

	var trailer []byte

	if err := b.doN(ctx, "reading an artifact trailer", func(ctx context.Context) (int64, error) {
		read, err := b.b.NewRangeReader(ctx, key, stored-trailerBytes, trailerBytes, nil)
		if err != nil {
			return 0, err
		}

		defer func() { _ = read.Close() }()

		trailer, err = io.ReadAll(read)

		return int64(len(trailer)), err
	}); err != nil {
		return 0, artifactError("reading", key, err)
	}

	size, ok := gzipSize(trailer)
	if !ok {
		return 0, fmt.Errorf("artifact %s: %w", key, errArtifactTruncated)
	}

	return size, nil
}
