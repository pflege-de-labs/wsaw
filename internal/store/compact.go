package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gocloud.dev/gcerrors"
)

// CompactOptions configures a pass over the artifact bucket.
type CompactOptions struct {
	// DryRun reports what a real run would do and writes nothing.
	DryRun bool

	// OnArtifact is called for each artifact the run compressed, or would
	// have compressed in a dry run.
	OnArtifact func(ref string, original, stored int64)

	// OnProblem is called for each object the run refused to touch, with the
	// reason. A compaction that silently skipped half a bucket is
	// indistinguishable from one that had nothing to do (Tenet 5).
	OnProblem func(ref string, err error)
}

// CompactStats reports what one pass did.
type CompactStats struct {
	// Scanned counts the raw artifacts considered — objects that are already
	// compressed are not among them.
	Scanned int
	// Compressed counts the artifacts rewritten, or that a dry run would
	// have rewritten.
	Compressed int
	// AlreadyCompressed and NotWorthIt count what was left alone on purpose:
	// stored compressed already, or not meaningfully smaller gzipped.
	AlreadyCompressed int
	NotWorthIt        int
	// Problems counts objects that were not touched because something about
	// them was wrong — unreadable, or not the artifact their key claims.
	Problems int

	// BytesBefore and BytesAfter cover the artifacts this run compressed,
	// which is what makes the saving a measured number rather than a claim.
	BytesBefore int64
	BytesAfter  int64
}

// Saved reports the bytes this run freed.
func (s CompactStats) Saved() int64 { return s.BytesBefore - s.BytesAfter }

// errNotAnArtifactName reports an object whose key is not a content address.
var errNotAnArtifactName = errors.New("not an artifact name")

// inFlightPrefix is what the local driver names a write it has not published.
// fileblob writes to a temporary file in the target directory and renames it
// into place, so a listing taken mid-scan can show one.
const inFlightPrefix = ".wsaw-artifact-"

// isInFlightWrite reports whether a key is such a temporary file.
func isInFlightWrite(key string) bool {
	_, leaf, ok := strings.Cut(key, refSeparator)

	return ok && strings.HasPrefix(leaf, inFlightPrefix)
}

// CompactArtifacts compresses the artifacts already stored (Story 4.9).
//
// It is the deliberate counterpart to a write path that rewrites nothing: an
// upgrade leaves every stored artifact exactly where it was, and an operator
// who wants the saving applied to their history asks for it, at a time they
// choose.
//
// The invariant that makes it safe to run against live evidence, and against a
// running daemon, is the order: the compressed object is written and verified
// against the digest in the artifact's own key before the raw object is
// removed, so every artifact is readable in one form or the other at every
// instant (Story 4.9, AC3 and AC4). That order survived the move from a
// directory to a bucket unchanged, because it never depended on a rename.
//
// What did change is the cost. Against a directory this was a walk and a
// handful of syscalls; against object storage every artifact is a GET, a PUT
// and a DELETE, and the whole history is transferred twice. The command says so
// before it starts, and `--dry-run` prices it without writing (Story 8.9, AC6).
func (s *SQL) CompactArtifacts(ctx context.Context, opts CompactOptions) (CompactStats, error) {
	var stats CompactStats

	// Established before anything is rewritten, for the reason a prune
	// establishes it: a bucket that has gone away answers NotFound for every
	// key, and a run that read that as "nothing to compact" would report a
	// clean pass over evidence it never saw (Tenet 5).
	if err := s.bucket.reachable(ctx); err != nil {
		return stats, fmt.Errorf("compacting artifacts needs the artifact bucket: %w", err)
	}

	err := s.bucket.list(ctx, "", func(obj artifactObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if isCompressedKey(obj.key) {
			stats.AlreadyCompressed++

			return nil
		}

		// An in-flight write from a scan happening right now. It is not an
		// artifact until the driver has published it, and it is not this
		// command's business either way.
		if isInFlightWrite(obj.key) {
			return nil
		}

		stats.Scanned++

		s.compactOne(ctx, obj.ref, opts, &stats)

		return nil
	})
	if err != nil {
		// A cancelled walk still reports the work it completed: the artifacts
		// it compressed are compressed, and saying so is the difference
		// between a resumable operation and an unknown state.
		return stats, fmt.Errorf("compacting artifacts: %w", err)
	}

	return stats, nil
}

// compactOne compresses a single artifact, or records why it did not.
//
// A problem with one artifact is never fatal: a bucket with one corrupt object
// in it is exactly the bucket an operator most wants this command to get
// through (Story 4.9, AC6).
func (s *SQL) compactOne(ctx context.Context, ref string, opts CompactOptions, stats *CompactStats) {
	// An object under a kind's prefix that is not an artifact name. Reported
	// rather than passed over: this bucket is wsaw's, and something in it that
	// nothing can have written is worth an operator's attention even though it
	// is left exactly where it is (Tenet 5).
	if err := validateRef(ref); err != nil {
		stats.Problems++

		reportProblem(opts, ref, fmt.Errorf("%s: %w", truncateForMessage(ref), errNotAnArtifactName))

		return
	}

	// Read by key rather than through get: get resolves which spelling holds
	// the artifact, and this command already knows — it is looking at the raw
	// one, and the whole question is whether a packed one should exist too.
	data, err := s.bucket.getKey(ctx, ref)
	if err != nil {
		stats.Problems++

		reportProblem(opts, ref, err)

		return
	}

	// The key is a content address, so the object can be checked against it. An
	// object that is not what its key says is a corrupt artifact, and repacking
	// it would preserve the corruption and destroy the evidence that it
	// happened.
	if err := verifyArtifactDigest(ref, data); err != nil {
		stats.Problems++

		reportProblem(opts, ref, err)

		return
	}

	packed, worth := compressArtifact(data)
	if !worth {
		stats.NotWorthIt++

		return
	}

	if !opts.DryRun {
		if err := s.bucket.replaceWithCompressed(ctx, ref, packed); err != nil {
			stats.Problems++

			reportProblem(opts, ref, err)

			return
		}
	}

	stats.Compressed++
	stats.BytesBefore += int64(len(data))
	stats.BytesAfter += int64(len(packed))

	if opts.OnArtifact != nil {
		opts.OnArtifact(ref, int64(len(data)), int64(len(packed)))
	}
}

// replaceWithCompressed writes the compressed object, proves it holds the same
// artifact, and only then removes the raw one.
//
// The read-back is from the bucket rather than from the buffer just written:
// what has to be true is that the object now in the bucket decodes to the
// artifact, not that the bytes in memory would have. Against object storage
// that is one extra GET per artifact, and it is the GET that makes the deletion
// afterwards safe.
func (b *bucket) replaceWithCompressed(ctx context.Context, ref string, packed []byte) error {
	gz := ref + compressedSuffix

	if err := b.doN(ctx, "storing a compressed artifact", func(ctx context.Context) (int64, error) {
		return int64(len(packed)), b.write(ctx, gz, packed)
	}); err != nil {
		return artifactError("storing", gz, err)
	}

	stored, err := b.getKey(ctx, gz)
	if err != nil {
		return abandonCompressed(ctx, b, gz, fmt.Errorf("re-reading the compressed artifact: %w", err))
	}

	if _, err := decompressArtifact(ref, stored, b.artifactMaxBytes()); err != nil {
		return abandonCompressed(ctx, b, gz, err)
	}

	if err := b.do(ctx, "deleting an artifact", func(ctx context.Context) error {
		return b.b.Delete(ctx, ref)
	}); err != nil {
		return fmt.Errorf("removing the uncompressed artifact %s: %w", ref, err)
	}

	return nil
}

// abandonCompressed removes a compressed object that could not be verified, so
// a failed rewrite leaves the bucket as it found it.
func abandonCompressed(ctx context.Context, b *bucket, gz string, cause error) error {
	err := b.do(ctx, "deleting an artifact", func(ctx context.Context) error {
		return b.b.Delete(ctx, gz)
	})
	if gcerrors.Code(err) == gcerrors.NotFound {
		err = nil
	}

	if err != nil {
		return fmt.Errorf("%w (and the unverified object %s could not be removed: %w)", cause, gz, err)
	}

	return cause
}

// getKey reads one stored key whole, without asking which spelling of a
// reference holds the artifact. Compaction is the only caller: it is walking
// keys rather than resolving references.
func (b *bucket) getKey(ctx context.Context, key string) ([]byte, error) {
	if err := validateStoredKey(key); err != nil {
		return nil, err
	}

	var data []byte

	if err := b.doN(ctx, "reading an artifact", func(ctx context.Context) (int64, error) {
		read, err := b.b.ReadAll(ctx, key)
		if err != nil {
			return 0, err
		}

		data = read

		return int64(len(data)), nil
	}); err != nil {
		return nil, artifactError("reading", key, err)
	}

	return data, nil
}

func reportProblem(opts CompactOptions, ref string, err error) {
	if opts.OnProblem != nil {
		opts.OnProblem(ref, err)
	}
}
