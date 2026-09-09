package store

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// A test's door to the objects a store wrote, whichever provider holds them
// (Story 8.9, AC1).
//
// The tests around the store have always asserted on the objects themselves
// and not only on what the store says about them — that a document was written
// byte for byte, that a tampered one is refused, that a deleted one is reported
// rather than inferred. Until this file they did it with os.ReadFile and
// os.Remove on a path built from store.Options.ArtifactDir, which quietly made
// every one of them a test of the local filesystem: point the same suite at a
// memory bucket or at S3 and it does not fail, it fails to compile a promise it
// was never checking.
//
// So the reach-past-the-store is a bucket operation now. It is opened by the
// store's own openBucket, so a test reaches the objects exactly the way the
// code under test does — same URL handling, same fileblob options, same
// providers — and one suite runs unchanged against a directory, against memory,
// and against MinIO.
//
// It lives in an _test.go file, so nothing here is in a shipped binary, and it
// is in package store rather than in the external test package because openBucket
// is unexported and duplicating its options in a test would be a second
// definition of the layout to keep in step.

// TestBucket is the artifact bucket of one store, opened beside it.
//
// It holds the *testing.T it was opened with, so its methods can fail the test
// where a bucket operation fails: every one of them is a step the test needs to
// have worked, never the thing under test.
type TestBucket struct {
	t *testing.T
	b *bucket
}

// OpenTestBucket opens the bucket at location and closes it when the test ends.
//
// location is whatever store.Options.ArtifactDir holds: a directory, a file
// URL, or a bucket URL of any scheme this build can reach — including the
// schemes only a test registers.
func OpenTestBucket(t *testing.T, location string) *TestBucket {
	t.Helper()

	b, err := openBucket(t.Context(), location)
	if err != nil {
		t.Fatalf("opening the artifact bucket at %s: %v", location, err)
	}

	t.Cleanup(func() {
		if err := b.close(); err != nil {
			t.Errorf("closing the artifact bucket: %v", err)
		}
	})

	return &TestBucket{t: t, b: b}
}

// Read returns what the bucket holds under key, failing the test if it holds
// nothing.
func (tb *TestBucket) Read(key string) []byte {
	tb.t.Helper()

	body, err := tb.TryRead(key)
	if err != nil {
		tb.t.Fatalf("reading %s from the artifact bucket: %v", key, err)
	}

	return body
}

// TryRead returns what the bucket holds under key, or the provider's error —
// for the tests whose subject is the failure.
func (tb *TestBucket) TryRead(key string) ([]byte, error) {
	tb.t.Helper()

	body, err := tb.b.b.ReadAll(tb.t.Context(), key)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	return body, nil
}

// Write puts data at key, whatever is there already.
//
// It writes the raw key rather than content-addressing the bytes, which is the
// point: the tests that use it are the ones planting an object the store would
// never write — a tampered document, a truncated one, an orphan under a kind
// nothing references.
func (tb *TestBucket) Write(key string, data []byte) {
	tb.t.Helper()

	if err := tb.b.b.WriteAll(tb.t.Context(), key, data, &blob.WriterOptions{
		ContentType: artifactContentType,
	}); err != nil {
		tb.t.Fatalf("writing %s to the artifact bucket: %v", key, err)
	}
}

// Remove deletes one object, failing the test if it was not there.
func (tb *TestBucket) Remove(key string) {
	tb.t.Helper()

	if err := tb.b.b.Delete(tb.t.Context(), key); err != nil {
		tb.t.Fatalf("deleting %s from the artifact bucket: %v", key, err)
	}
}

// RemoveAll deletes everything under prefix and reports how many objects that
// was. An empty prefix empties the bucket.
func (tb *TestBucket) RemoveAll(prefix string) int {
	tb.t.Helper()

	keys := tb.Keys(prefix)

	for _, key := range keys {
		tb.Remove(key)
	}

	return len(keys)
}

// Has reports whether the bucket holds an object at key.
func (tb *TestBucket) Has(key string) bool {
	tb.t.Helper()

	found, err := tb.b.b.Exists(tb.t.Context(), key)
	if err != nil {
		tb.t.Fatalf("asking the artifact bucket for %s: %v", key, err)
	}

	return found
}

// Size is how many bytes the object at key holds, and -1 where there is none.
func (tb *TestBucket) Size(key string) int64 {
	tb.t.Helper()

	attrs, err := tb.b.b.Attributes(tb.t.Context(), key)

	switch {
	case gcerrors.Code(err) == gcerrors.NotFound:
		return -1

	case err != nil:
		tb.t.Fatalf("reading the attributes of %s: %v", key, err)
	}

	return attrs.Size
}

// Keys is every key under prefix, in the order the provider lists them.
//
// The order is not sorted here on purpose. A listing's order is a promise this
// index rests on (Story 8.10, AC4) and a difference between providers is
// exactly what Story 8.9 exists to surface, so a test that wants sorted keys
// sorts them and says so.
func (tb *TestBucket) Keys(prefix string) []string {
	tb.t.Helper()

	var keys []string

	iter := tb.b.b.List(&blob.ListOptions{Prefix: prefix})

	for {
		obj, err := iter.Next(tb.t.Context())
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			tb.t.Fatalf("listing %s in the artifact bucket: %v", prefix, err)
		}

		keys = append(keys, obj.Key)
	}

	return keys
}

// RebuildBatchSize is how many stored documents one batch of a rebuild reads.
//
// Exported for the test that interrupts a rebuild part way through, which has
// to write more documents than one batch holds in order to produce a half
// rebuilt index at all: a batch is read whole and merged afterwards, so a run
// that fails inside the first batch has written nothing and is not the state
// AC13 asks about. Taking the real number rather than lowering it is the same
// argument the compaction tests make about compactAfter.
const RebuildBatchSize = rebuildBatchSize

// RebuildMarkerTTL is how long a marker keeps a sweep off a bucket before it is
// treated as the leftover of a run that was killed.
const RebuildMarkerTTL = rebuildMarkerTTL
