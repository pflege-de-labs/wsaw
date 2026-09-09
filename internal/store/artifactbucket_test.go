package store_test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The second axis this suite runs on (Story 8.9, AC1).
//
// WSAW_TEST_STORE_DRIVER picks how the index is kept — rows in SQLite,
// PostgreSQL or MySQL, or objects in the bucket. This picks where the evidence
// itself goes, and the two are independent: every store keeps its documents,
// screenshots and stored bodies in the same bucket layout (Story 8.1, AC2), so
// the same suite has to hold against a directory on local disk, against a
// bucket that only exists in memory, and against a real object store.
//
//	go test ./internal/store                                        # a directory
//	WSAW_TEST_ARTIFACT_BUCKET=memory go test ./internal/store       # memory
//	WSAW_TEST_ARTIFACT_BUCKET=s3://bucket?… go test ./internal/store # MinIO, S3
//
// The argument is Story 4.7, AC5's, applied to the other half of the store: the
// tests are the specification of what a store does, so a provider difference
// has to fail them rather than be discovered by an operator. And the failure
// mode this guards against is specific and quiet — a test that reaches the
// evidence with os.ReadFile still passes against a directory long after the
// code it covers has stopped working anywhere else, because the assertion never
// travelled through a bucket at all. Every such reach now goes through
// store.OpenTestBucket, which opens the configured bucket the way the store
// does.
//
// A handful of tests are genuinely about the local filesystem — the permissions
// evidence is written with, a directory made read-only, a directory removed
// from under a running store. They say so with skipUnlessLocalBucket and skip
// where the bucket is not a directory, which is not a gap: what they asserted
// about a bucket is asserted for every bucket by the tests around them.
const (
	// envArtifactBucket names the bucket the whole suite writes to.
	envArtifactBucket = "WSAW_TEST_ARTIFACT_BUCKET"

	// bucketDirectory is the default: a fresh temporary directory per store,
	// which is what an unconfigured wsaw deployment uses too.
	bucketDirectory = "file"

	// bucketMemory is a bucket that exists only for the length of one test.
	bucketMemory = "memory"
)

// runPrefix keeps one `go test` process's objects apart from another's.
//
// It matters for exactly one bucket and it matters absolutely: a real object
// store is not created fresh per run, and `make test-store-minio` points two
// suites at one bucket in sequence. A per-process random prefix means the
// second run starts empty, as every test in it assumes, without the runs having
// to agree on anything.
var runPrefix = func() string {
	var b [6]byte

	if _, err := rand.Read(b[:]); err != nil {
		panic("test setup: no randomness for a bucket prefix: " + err.Error())
	}

	return "wsaw-test-" + hex.EncodeToString(b[:])
}()

// bucketCounter numbers the buckets within one process, so that two stores in
// one run cannot see each other's objects.
var bucketCounter atomic.Uint64

// artifactLocation is where one store's evidence goes, for whichever bucket the
// suite is being run against.
//
// Every call is a bucket of its own — a new temporary directory, a new memory
// bucket, a new prefix — because almost every test here assumes an empty store,
// and two stores in one test (a reopen, a migration's before and after) name
// their location once and pass it around.
func artifactLocation(t *testing.T) string {
	t.Helper()

	switch configured := artifactBucket(); configured {
	case bucketDirectory:
		return filepath.Join(t.TempDir(), "artifacts")

	case bucketMemory:
		// The lagging bucket with nothing scheduled, which is an in-memory
		// object store and is already in this suite (Story 8.10, AC15). A
		// second in-memory implementation beside it would be a second set of
		// listing, paging and error-code decisions to keep in step with the
		// providers, for no gain — and this one brings I3 along, so every test
		// in the suite is now watched for an index key deleted that this run
		// has not seen in a listing.
		//
		// I1 is the one rule that has to come off: three tests here plant a
		// tampered, truncated or undecodable object at a live content address
		// on purpose, and a fake cannot tell corruption arriving from outside
		// the store from a key derived from the wrong thing. See allowRewrites.
		//
		// memblob is deliberately not used here even though the package
		// registers it: blob.OpenBucket("mem://") builds a *new* empty bucket
		// on every call, so a store reopened on the same location would find
		// its evidence gone, and the tests that reopen a store are the ones
		// that would then be proving nothing.
		b := newLagBucket(t)
		b.allowRewrites()

		return b.url()

	default:
		return bucketURLFor(t, configured)
	}
}

// artifactBucket is what the environment asked for, normalised.
func artifactBucket() string {
	configured := strings.TrimSpace(os.Getenv(envArtifactBucket))
	if configured == "" {
		return bucketDirectory
	}

	return configured
}

// bucketURLFor gives one store its own corner of a bucket named by URL.
//
// The prefix is a query parameter gocloud.dev/blob understands for every
// provider, applied before the driver sees the URL, so nothing in wsaw learns
// that its bucket is a subtree of a larger one. An operator's own prefix is
// kept and extended rather than replaced, because a bucket shared with
// something else is exactly when one is set.
func bucketURLFor(t *testing.T, raw string) string {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a URL: %v", envArtifactBucket, raw, err)
	}

	q := u.Query()
	q.Set("prefix", fmt.Sprintf("%s%s/%d/", q.Get("prefix"), runPrefix, bucketCounter.Add(1)))
	u.RawQuery = q.Encode()

	return u.String()
}

// skipUnlessLocalBucket skips a test whose subject is the local filesystem
// rather than the bucket seam: what permissions evidence is written with, what
// happens when a directory is made read-only or taken away.
//
// why is the sentence the skip reports, so that a reader of the run sees which
// promise was not checked here rather than a bare "skipped".
func skipUnlessLocalBucket(t *testing.T, why string) {
	t.Helper()

	if artifactBucket() != bucketDirectory {
		t.Skipf("this test is about a bucket that is a directory on local disk: %s", why)
	}
}

// evidence opens the artifact bucket a store was given, for a test that has to
// assert on the objects themselves.
//
// Taking the options rather than a location keeps the call sites honest: the
// bucket a test reaches is the one the store under test was handed, and there
// is no path for a test to build.
func evidence(t *testing.T, opts store.Options) *store.TestBucket {
	t.Helper()

	return store.OpenTestBucket(t, opts.ArtifactDir)
}
