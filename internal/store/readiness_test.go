package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/store"
)

// What a store answers about itself when something is asking whether wsaw can
// still do its job (Story 8.6, AC4 and AC5).

// TestPingCoversTheArtifactBucket: readiness asks one question of the store,
// and since the result document itself lives in the bucket, a reachable
// database with an unreachable bucket is a wsaw that cannot record a scan.
// That has to read as not ready rather than as healthy (Tenet 8).
func TestPingCoversTheArtifactBucket(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer func() { _ = s.Close() }()

	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("a freshly opened store does not answer a ping: %v", err)
	}

	if err := os.RemoveAll(opts.ArtifactDir); err != nil {
		t.Fatal(err)
	}

	err = s.Ping(t.Context())
	if err == nil {
		t.Fatal("a store whose artifact bucket has gone answers a ping as healthy")
	}

	if !strings.Contains(err.Error(), opts.ArtifactDir) {
		t.Errorf("the failure %q does not name the bucket", err)
	}
}

// TestProbeArtifactBucketAcceptsAWritableBucket: the ordinary start, where the
// probe is one write and one delete and then wsaw carries on.
func TestProbeArtifactBucketAcceptsAWritableBucket(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.ProbeArtifactBucket(t.Context()); err != nil {
		t.Fatalf("a writable bucket failed its probe: %v", err)
	}

	// Twice, because every start runs it: a probe that could only succeed
	// against an untouched bucket would fail every restart.
	if err := s.ProbeArtifactBucket(t.Context()); err != nil {
		t.Fatalf("the second probe of a writable bucket failed: %v", err)
	}
}

// TestProbeArtifactBucketRefusesABucketItCannotWriteTo: a bucket that lists
// happily and refuses writes — a read-only policy, a credential with read
// scope, a disk with nothing left on it — looks healthy to every check short
// of a write. So the write happens at startup, and names the bucket, instead
// of being discovered by the first scan of the night.
func TestProbeArtifactBucketRefusesABucketItCannotWriteTo(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, which is not refused write access by permissions")
	}

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer func() { _ = s.Close() }()

	// Made read-only before anything has been written to it: a probe that had
	// already run would have left the prefix it writes under behind, and a
	// directory inside a read-only directory is still writable.
	if err := os.Chmod(opts.ArtifactDir, 0o500); err != nil {
		t.Fatal(err)
	}

	// Put back, so the test's own directory can be cleaned up.
	t.Cleanup(func() { _ = os.Chmod(opts.ArtifactDir, 0o700) })

	err = s.ProbeArtifactBucket(t.Context())
	if err == nil {
		t.Fatal("a bucket that refuses writes passed its probe")
	}

	if !strings.Contains(err.Error(), opts.ArtifactDir) {
		t.Errorf("the failure %q does not name the bucket", err)
	}
}
