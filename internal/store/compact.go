package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CompactOptions configures a pass over the artifact directory.
type CompactOptions struct {
	// DryRun reports what a real run would do and writes nothing.
	DryRun bool

	// OnArtifact is called for each artifact the run compressed, or would
	// have compressed in a dry run.
	OnArtifact func(ref string, original, stored int64)

	// OnProblem is called for each file the run refused to touch, with the
	// reason. A compaction that silently skipped half a directory is
	// indistinguishable from one that had nothing to do (Tenet 5).
	OnProblem func(ref string, err error)
}

// CompactStats reports what one pass did.
type CompactStats struct {
	// Scanned counts the raw artifacts considered — files that are already
	// compressed are not among them.
	Scanned int
	// Compressed counts the artifacts rewritten, or that a dry run would
	// have rewritten.
	Compressed int
	// AlreadyCompressed and NotWorthIt count what was left alone on purpose:
	// stored compressed already, or not meaningfully smaller gzipped.
	AlreadyCompressed int
	NotWorthIt        int
	// Problems counts files that were not touched because something about
	// them was wrong — unreadable, or not the artifact their name claims.
	Problems int

	// BytesBefore and BytesAfter cover the artifacts this run compressed,
	// which is what makes the saving a measured number rather than a claim.
	BytesBefore int64
	BytesAfter  int64
}

// Saved reports the bytes this run freed.
func (s CompactStats) Saved() int64 { return s.BytesBefore - s.BytesAfter }

// errNotAnArtifactName reports a file whose name is not a content address.
var errNotAnArtifactName = errors.New("not an artifact name")

// CompactArtifacts compresses the artifacts already on disk (Story 4.9).
//
// It is the deliberate counterpart to a write path that rewrites nothing: an
// upgrade leaves every stored artifact exactly where it was, and an operator
// who wants the saving applied to their history asks for it, at a time they
// choose.
//
// The invariant that makes it safe to run against live evidence, and against
// a running daemon, is the order: the compressed form is written and verified
// against the digest in the artifact's own name before the raw form is
// removed, so every artifact is readable in one form or the other at every
// instant (Story 4.9, AC3 and AC4).
func (s *Store) CompactArtifacts(ctx context.Context, opts CompactOptions) (CompactStats, error) {
	var stats CompactStats

	if s.artifactDir == "" {
		return stats, errors.New("artifact storage is not configured")
	}

	err := filepath.WalkDir(s.artifactDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		name := d.Name()

		// An in-flight write from a scan happening right now. It is not an
		// artifact until it has been renamed, and it is not this command's
		// business either way.
		if strings.HasPrefix(name, ".wsaw-artifact-") {
			return nil
		}

		if strings.HasSuffix(name, compressedSuffix) {
			stats.AlreadyCompressed++

			return nil
		}

		stats.Scanned++

		s.compactOne(path, opts, &stats)

		return nil
	})
	if err != nil {
		// A cancelled walk still reports the work it completed: the
		// artifacts it compressed are compressed, and saying so is the
		// difference between a resumable operation and an unknown state.
		return stats, fmt.Errorf("compacting artifacts: %w", err)
	}

	return stats, nil
}

// compactOne compresses a single artifact, or records why it did not.
//
// A problem with one artifact is never fatal: a directory with one corrupt
// file in it is exactly the directory an operator most wants this command to
// get through (Story 4.9, AC6).
func (s *Store) compactOne(path string, opts CompactOptions, stats *CompactStats) {
	ref, err := s.artifactRef(path)
	if err != nil {
		stats.Problems++

		reportProblem(opts, filepath.Base(path), err)

		return
	}

	data, err := os.ReadFile(path) //nolint:gosec // path comes from walking the artifact directory
	if err != nil {
		stats.Problems++

		reportProblem(opts, ref, err)

		return
	}

	// The name is a content address, so the file can be checked against it.
	// A file that is not what its name says is a corrupt artifact, and
	// repacking it would preserve the corruption and destroy the evidence
	// that it happened.
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
		if err := s.replaceWithCompressed(ref, path, packed); err != nil {
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

// replaceWithCompressed writes the compressed form, proves it holds the same
// artifact, and only then removes the raw one.
func (s *Store) replaceWithCompressed(ref, path string, packed []byte) error {
	gz := path + compressedSuffix

	if err := writeArtifactFile(gz, packed); err != nil {
		return err
	}

	// Read back from disk rather than trusting the buffer that was just
	// written: what has to be true is that the file now on disk decodes to
	// the artifact, not that the bytes in memory would have.
	stored, err := os.ReadFile(gz) //nolint:gosec // gz is the file this function just wrote
	if err != nil {
		return abandonCompressed(gz, fmt.Errorf("re-reading the compressed artifact: %w", err))
	}

	if _, err := decompressArtifact(ref, stored, s.maxArtifactBytes); err != nil {
		return abandonCompressed(gz, err)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing the uncompressed artifact: %w", err)
	}

	return nil
}

// abandonCompressed removes a compressed file that could not be verified, so
// a failed rewrite leaves the directory as it found it.
func abandonCompressed(gz string, cause error) error {
	if err := os.Remove(gz); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w (and the unverified file %s could not be removed: %w)", cause, gz, err)
	}

	return cause
}

// artifactRef turns a path inside the artifact directory back into the
// reference it was stored under, and refuses anything that is not one.
func (s *Store) artifactRef(path string) (string, error) {
	rel, err := filepath.Rel(s.artifactDir, path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, errNotAnArtifactName)
	}

	ref := filepath.ToSlash(rel)

	kind, digest, ok := strings.Cut(ref, "/")
	if !ok || kind == "" || strings.Contains(digest, "/") {
		return "", fmt.Errorf("%s: %w", ref, errNotAnArtifactName)
	}

	if len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("%s: %w", ref, errNotAnArtifactName)
	}

	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("%s: %w", ref, errNotAnArtifactName)
	}

	return ref, nil
}

func reportProblem(opts CompactOptions, ref string, err error) {
	if opts.OnProblem != nil {
		opts.OnProblem(ref, err)
	}
}
