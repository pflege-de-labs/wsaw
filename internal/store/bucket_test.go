package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gocloud.dev/blob"

	// The "mem" scheme, registered here and nowhere else on purpose. A bucket
	// that discards everything on exit is exactly what these tests want and
	// exactly what a deployment must never be able to configure by mistake: an
	// operator who copied "mem://" out of a gocloud example into
	// store.artifactDir would get a store that opens cleanly, reports every
	// scan as stored, and loses the evidence at shutdown — a failed observation
	// wearing a clean result (AGENTS §3.4, Tenet 5). Keeping the import in a
	// test file means the shipped binary refuses the scheme by name, with the
	// list of the ones it does support.
	_ "gocloud.dev/blob/memblob"
)

// White-box tests of the bucket seam (Story 8.1). They need no database and
// no network — the file bucket writes to a temp directory and the memory
// bucket writes to nothing at all — so they run in the fast suite.

const testKind = "screenshot-before-consent"

// bucketFor opens a bucket at location and closes it when the test ends.
func bucketFor(t *testing.T, location string) *bucket {
	t.Helper()

	b, err := openBucket(t.Context(), location)
	if err != nil {
		t.Fatalf("openBucket(%q): %v", location, err)
	}

	t.Cleanup(func() {
		if err := b.close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	return b
}

// locations are the two providers every behavioural test below runs against.
// The bucket's whole point is that where evidence lives stops mattering, so a
// test that only covered the local one would be testing the wrong thing.
func locations(t *testing.T) map[string]string {
	t.Helper()

	return map[string]string{
		"directory": filepath.Join(t.TempDir(), "artifacts"),
		"file URL":  "file://" + filepath.Join(t.TempDir(), "artifacts"),
		"memory":    "mem://",
	}
}

func TestBucketRoundTrip(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b := bucketFor(t, location)
			ref := assertStoredAndReadable(t, b, []byte("one pixel of evidence"))
			assertDeleted(t, b, ref)
		})
	}
}

// assertStoredAndReadable writes data and checks every read path agrees about
// it, returning the reference.
func assertStoredAndReadable(t *testing.T, b *bucket, data []byte) string {
	t.Helper()

	ctx := t.Context()

	ref, err := b.put(ctx, testKind, data)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	sum := sha256.Sum256(data)
	if want := testKind + "/" + hex.EncodeToString(sum[:]); ref != want {
		t.Errorf("put returned %q, want the content address %q", ref, want)
	}

	got, err := b.get(ctx, ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if string(got) != string(data) {
		t.Errorf("get returned %q, want %q", got, data)
	}

	size, err := b.stat(ctx, ref)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if size != int64(len(data)) {
		t.Errorf("stat reported %d bytes, want %d", size, len(data))
	}

	present, err := b.exists(ctx, ref)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}

	if !present {
		t.Error("exists reported false for an artifact that was just written")
	}

	return ref
}

// assertDeleted removes an artifact and checks it is gone.
func assertDeleted(t *testing.T, b *bucket, ref string) {
	t.Helper()

	ctx := t.Context()

	if err := b.remove(ctx, ref); err != nil {
		t.Fatalf("remove: %v", err)
	}

	present, err := b.exists(ctx, ref)
	if err != nil {
		t.Fatalf("exists after remove: %v", err)
	}

	if present {
		t.Error("exists reported true for a deleted artifact")
	}
}

// TestBucketContentAddressingCostsOneObject is AC3. Storing the same
// screenshot twice must produce one object and one reference, and the second
// write must not rewrite the first.
func TestBucketContentAddressingCostsOneObject(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)
			data := []byte("the same screenshot, twice")

			first, err := b.put(ctx, testKind, data)
			if err != nil {
				t.Fatalf("first put: %v", err)
			}

			second, err := b.put(ctx, testKind, data)
			if err != nil {
				t.Fatalf("second put: %v", err)
			}

			if first != second {
				t.Errorf("the same bytes produced two references, %q and %q", first, second)
			}

			var keys []string

			if err := b.list(ctx, testKind, func(ref string, _ int64) error {
				keys = append(keys, ref)

				return nil
			}); err != nil {
				t.Fatalf("list: %v", err)
			}

			if len(keys) != 1 {
				t.Errorf("two writes of one screenshot left %d objects (%v), want 1", len(keys), keys)
			}
		})
	}
}

// TestBucketReadsAnExistingArtifactDirectory is AC2, and it is the criterion
// that protects installations that already hold months of evidence: a
// directory written by the filesystem implementation must be readable, and
// re-writable, through the bucket with nothing moved.
func TestBucketReadsAnExistingArtifactDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	data := []byte("evidence captured by an older wsaw")
	sum := sha256.Sum256(data)
	ref := testKind + "/" + hex.EncodeToString(sum[:])
	path := filepath.Join(dir, filepath.FromSlash(ref))

	// Written exactly the way the directory implementation wrote it: the kind
	// as a subdirectory at 0700, the hex digest as the file name, 0600, and
	// no metadata beside it.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the old layout: %v", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing the old artifact: %v", err)
	}

	ctx := t.Context()
	b := bucketFor(t, dir)

	got, err := b.get(ctx, ref)
	if err != nil {
		t.Fatalf("reading an artifact written in the old layout: %v", err)
	}

	if string(got) != string(data) {
		t.Errorf("read back %q, want %q", got, data)
	}

	size, err := b.stat(ctx, ref)
	if err != nil {
		t.Fatalf("stat on an artifact written in the old layout: %v", err)
	}

	if size != int64(len(data)) {
		t.Errorf("stat reported %d bytes, want %d", size, len(data))
	}

	// Re-storing the same evidence is a no-op, and writing new evidence lands
	// in the same directory in the same shape.
	if again, err := b.put(ctx, testKind, data); err != nil || again != ref {
		t.Errorf("re-storing existing evidence returned (%q, %v), want (%q, nil)", again, err, ref)
	}

	fresh := []byte("evidence captured after the upgrade")
	if _, err := b.put(ctx, testKind, fresh); err != nil {
		t.Fatalf("storing new evidence beside the old: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, testKind))
	if err != nil {
		t.Fatalf("reading the artifact directory: %v", err)
	}

	freshSum := sha256.Sum256(fresh)
	want := map[string]bool{
		hex.EncodeToString(sum[:]):      true,
		hex.EncodeToString(freshSum[:]): true,
	}

	for _, entry := range entries {
		if !want[entry.Name()] {
			t.Errorf("the bucket left %q in the artifact directory; the layout is <kind>/<sha256hex> and nothing else", entry.Name())
		}
	}

	if len(entries) != len(want) {
		t.Errorf("the artifact directory holds %d files, want %d", len(entries), len(want))
	}
}

// TestBucketRefusesHostileReferences is AC5. References are built from
// page-controlled data, so the only safe answer is that anything which is not
// the shape this store writes names nothing at all (Tenet 9).
func TestBucketRefusesHostileReferences(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", digestLength)

	hostile := []struct {
		name string
		ref  string
	}{
		{"empty", ""},
		{"no separator", digest},
		{"parent directory", "../../etc/passwd"},
		{"encoded separator", "..%2f..%2fetc%2fpasswd"},
		{"traversal after a valid kind", "body/../../b"},
		{"traversal in the middle", "a/../../b"},
		{"traversal appended to a digest", "body/" + digest + "/../../../etc/passwd"},
		{"absolute path", "/abs"},
		{"absolute path with a digest", "/body/" + digest},
		{"windows separator", "body\\" + digest},
		{"bare dot dot", ".."},
		{"dot kind", "./" + digest},
		{"empty kind", "/" + digest},
		{"empty digest", "body/"},
		{"trailing separator", "body/" + digest + "/"},
		{"nul byte", "body/" + digest + "\x00"},
		{"newline", "body\n/" + digest},
		{"uppercase digest", "body/" + strings.ToUpper(digest)},
		{"short digest", "body/abc123"},
		{"long digest", "body/" + digest + "a"},
		{"non hex digest", "body/" + strings.Repeat("g", digestLength)},
		{"unicode kind", "b\u00f3dy/" + digest},
		// Written as escapes rather than as the characters themselves: a
		// bidirectional override in source is a hazard of its own, and an
		// invisible space is unreviewable.
		{"fullwidth separator", "body\uff0f" + digest},
		{"right to left override", "body/\u202e" + digest[1:]},
		{"cyrillic homoglyph in the digest", "body/\u0430" + digest[1:]},
		{"zero width space in the kind", "bo\u200bdy/" + digest},
		{"very long reference", strings.Repeat("a", 100000) + "/" + digest},
		{"leading hyphen kind", "-body/" + digest},
		{"trailing hyphen kind", "body-/" + digest},
	}

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)

			for _, tc := range hostile {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					if _, err := b.get(ctx, tc.ref); !errors.Is(err, errInvalidRef) {
						t.Errorf("get(%q) returned %v, want an invalid-reference error", tc.ref, err)
					}

					if _, err := b.stat(ctx, tc.ref); !errors.Is(err, errInvalidRef) {
						t.Errorf("stat(%q) returned %v, want an invalid-reference error", tc.ref, err)
					}

					if _, err := b.exists(ctx, tc.ref); !errors.Is(err, errInvalidRef) {
						t.Errorf("exists(%q) returned %v, want an invalid-reference error", tc.ref, err)
					}

					if _, _, err := b.newReader(ctx, tc.ref); !errors.Is(err, errInvalidRef) {
						t.Errorf("newReader(%q) returned %v, want an invalid-reference error", tc.ref, err)
					}

					if err := b.remove(ctx, tc.ref); !errors.Is(err, errInvalidRef) {
						t.Errorf("remove(%q) returned %v, want an invalid-reference error", tc.ref, err)
					}
				})
			}
		})
	}
}

// TestBucketRefusesHostileKindsAndPrefixes covers the two other places a
// caller names a key: the kind a write chooses, and the prefix a sweep lists.
func TestBucketRefusesHostileKindsAndPrefixes(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")

	kinds := []string{"", "..", "../escape", "body/nested", "BODY", "body\x00", "b\u00f3dy", strings.Repeat("b", maxKindLength+1)}

	for _, kind := range kinds {
		if _, err := b.put(ctx, kind, []byte("x")); !errors.Is(err, errInvalidRef) {
			t.Errorf("put with kind %q returned %v, want an invalid-reference error", kind, err)
		}
	}

	prefixes := []string{"..", "../body", "body/..", "body/" + strings.Repeat("a", digestLength+1), "/body", "body/zz"}

	for _, prefix := range prefixes {
		err := b.list(ctx, prefix, func(string, int64) error { return nil })
		if !errors.Is(err, errInvalidRef) {
			t.Errorf("list under prefix %q returned %v, want an invalid-reference error", prefix, err)
		}
	}

	// A partially typed digest is a legitimate way to narrow a listing, so it
	// is accepted where a malformed one is not.
	if err := b.list(ctx, "body/abc", func(string, int64) error { return nil }); err != nil {
		t.Errorf("list under a partial digest: %v", err)
	}
}

// TestBucketMissingArtifactIsNotFound is AC9. Every caller that tells
// "pruned" from "broken" today keeps that distinction (Story 5.17, AC3).
func TestBucketMissingArtifactIsNotFound(t *testing.T) {
	t.Parallel()

	missing := testKind + "/" + strings.Repeat("0", digestLength)

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)

			if _, err := b.get(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Errorf("get on a missing artifact returned %v, want ErrNotFound", err)
			}

			if _, err := b.stat(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Errorf("stat on a missing artifact returned %v, want ErrNotFound", err)
			}

			if _, _, err := b.newReader(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Errorf("newReader on a missing artifact returned %v, want ErrNotFound", err)
			}

			if err := b.remove(ctx, missing); !errors.Is(err, ErrNotFound) {
				t.Errorf("remove of a missing artifact returned %v, want ErrNotFound", err)
			}

			present, err := b.exists(ctx, missing)
			if err != nil {
				t.Fatalf("exists on a missing artifact: %v", err)
			}

			if present {
				t.Error("exists reported true for an artifact that was never written")
			}
		})
	}
}

// TestBucketStreamingReaderReportsSize is AC7: the HTTP path needs the length
// before it has read a byte, and must not have to buffer the object to get it.
func TestBucketStreamingReaderReportsSize(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)
			data := []byte(strings.Repeat("a multi-megabyte document, in miniature. ", 1000))

			ref, err := b.put(ctx, "result", data)
			if err != nil {
				t.Fatalf("put: %v", err)
			}

			r, size, err := b.newReader(ctx, ref)
			if err != nil {
				t.Fatalf("newReader: %v", err)
			}

			defer func() {
				if err := r.Close(); err != nil {
					t.Errorf("closing the reader: %v", err)
				}
			}()

			if size != int64(len(data)) {
				t.Errorf("newReader reported %d bytes, want %d", size, len(data))
			}

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("reading the stream: %v", err)
			}

			if string(got) != string(data) {
				t.Error("the stream did not return the bytes that were written")
			}
		})
	}
}

// TestBucketListPagesThroughLargeResults exercises the paging, which is the
// only reason list takes a callback: a sweep over every document of every
// scan must not depend on one response carrying them all.
func TestBucketListPagesThroughLargeResults(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")

	const count = artifactListPageSize*2 + 7

	want := make(map[string]bool, count)

	for i := range count {
		ref, err := b.put(ctx, "result", fmt.Appendf(nil, "document %d", i))
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}

		want[ref] = true
	}

	seen := make(map[string]bool, count)

	if err := b.list(ctx, "result", func(ref string, size int64) error {
		if seen[ref] {
			t.Errorf("list returned %q twice", ref)
		}

		if size == 0 {
			t.Errorf("list reported %q as empty", ref)
		}

		seen[ref] = true

		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(seen) != len(want) {
		t.Fatalf("list returned %d keys, want %d", len(seen), len(want))
	}

	for ref := range want {
		if !seen[ref] {
			t.Errorf("list did not return %q", ref)
		}
	}
}

// TestBucketListStopsOnCallbackError checks that a caller can abandon a
// listing without the walk swallowing its reason.
func TestBucketListStopsOnCallbackError(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")

	for i := range 3 {
		if _, err := b.put(ctx, "result", fmt.Appendf(nil, "document %d", i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	stop := errors.New("enough")
	calls := 0

	err := b.list(ctx, "result", func(string, int64) error {
		calls++

		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("list returned %v, want the callback's own error", err)
	}

	if calls != 1 {
		t.Errorf("the callback ran %d times after asking to stop, want 1", calls)
	}
}

// TestBucketWriteIsAtomicUnderCancellation is AC4 and AC6 together: a write
// whose context dies must leave no key behind, because a half-written key is
// a reader's idea of complete evidence.
func TestBucketWriteIsAtomicUnderCancellation(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	b := bucketFor(t, dir)
	data := []byte("evidence that must not be published half way")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := b.put(ctx, testKind, data); err == nil {
		t.Fatal("put with a cancelled context succeeded, want a failure")
	}

	sum := sha256.Sum256(data)
	ref := testKind + "/" + hex.EncodeToString(sum[:])

	present, err := b.exists(t.Context(), ref)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}

	if present {
		t.Error("a cancelled write published its key; a reader would treat it as evidence")
	}

	// Nor may it leave a partial file under another name for a sweep to trip
	// over later.
	entries, err := os.ReadDir(filepath.Join(dir, testKind))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the artifact directory: %v", err)
	}

	for _, entry := range entries {
		t.Errorf("a cancelled write left %q behind", entry.Name())
	}
}

// TestBucketArtifactPermissionsAreRestrictive holds the local bucket to what
// the directory implementation guaranteed: evidence can carry personal data,
// so it is readable by nobody else (Tenet 19).
func TestBucketArtifactPermissionsAreRestrictive(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	b := bucketFor(t, dir)

	ref, err := b.put(t.Context(), testKind, []byte("private evidence"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	paths := map[string]os.FileMode{
		dir:                          artifactDirMode,
		filepath.Join(dir, testKind): artifactDirMode,
		filepath.Join(dir, filepath.FromSlash(ref)): artifactFileMode,
	}

	for path, want := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}

		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
	}
}

// TestBucketRejectsUnusableLocations is Tenet 15 applied to storage: a
// location wsaw cannot use fails when it is opened, naming what it is and
// what this build can actually reach.
func TestBucketRejectsUnusableLocations(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()

		// Deliberately refused rather than defaulted to the working
		// directory: a bucket rooted there would let a retention sweep
		// enumerate files that are not artifacts.
		if _, err := openBucket(t.Context(), ""); err == nil {
			t.Fatal("openBucket accepted an empty location")
		}
	})

	t.Run("unknown scheme", func(t *testing.T) {
		t.Parallel()

		_, err := openBucket(t.Context(), "ftp://example.invalid/artifacts")
		if err == nil {
			t.Fatal("openBucket accepted an ftp:// location")
		}

		for _, want := range []string{"ftp", "a plain directory path", "mem"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error %q does not mention %q; an operator needs to know what this build supports", err, want)
			}
		}
	})

	t.Run("remote file host", func(t *testing.T) {
		t.Parallel()

		if _, err := openBucket(t.Context(), "file://elsewhere/artifacts"); err == nil {
			t.Fatal("openBucket accepted a file:// location on another host")
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing the fixture: %v", err)
		}

		if _, err := openBucket(t.Context(), path); err == nil {
			t.Fatal("openBucket accepted a path that is a file")
		}
	})
}

// TestCloudSchemesFollowTheBuildTag records the dependency decision as a
// test: the default build reaches the local disk and nothing else, and the
// cloudblob build reaches the three providers (Story 8.8, AC3 and AC4).
func TestCloudSchemesFollowTheBuildTag(t *testing.T) {
	t.Parallel()

	// The schemes are checked against the registry rather than by opening
	// them. Opening an s3:// URL would set an AWS credential chain going, and
	// a credential chain reaches for the instance metadata service — a
	// network call, which a test never makes (AGENTS.md §3).
	linked := cloudBuildHint() == ""

	for _, scheme := range []string{"s3", "gs", "azblob"} {
		if got := blob.DefaultURLMux().ValidBucketScheme(scheme); got != linked {
			t.Errorf("the %q scheme is registered = %v, want %v for this build", scheme, got, linked)
		}
	}

	if linked {
		return
	}

	// Without the drivers, the refusal has to tell an operator how to get
	// them rather than leaving them to read the source.
	_, err := openBucket(t.Context(), "s3://bucket/prefix")
	if err == nil {
		t.Fatal("openBucket accepted an s3:// location in a build without the cloud drivers")
	}

	for _, want := range []string{"s3", "cloudblob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not mention %q", err, want)
		}
	}
}

// TestWorthRetryingReadsProviderCodes is AC8's first half: what is worth
// another attempt is decided from the provider's error code, not its wording.
func TestWorthRetryingReadsProviderCodes(t *testing.T) {
	t.Parallel()

	b := bucketFor(t, "mem://")

	_, missing := b.b.Attributes(t.Context(), testKind+"/"+strings.Repeat("0", digestLength))
	if missing == nil {
		t.Fatal("reading a missing key succeeded")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"a missing key", missing, false},
		{"a deadline the service reported", fmt.Errorf("upload: %w", context.DeadlineExceeded), true},
		{"a refused connection", errors.New("dial tcp: connection refused"), true},
		{"a reset connection", errors.New("read: connection reset by peer"), true},
		{"an access denied", errors.New("AccessDenied: not your bucket"), false},
		{"a malformed request", errors.New("InvalidArgument: key too long"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := worthRetrying(tc.err); got != tc.want {
				t.Errorf("worthRetrying(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBucketUsesTheStoresRetryPolicy is AC8's second half, and the reason
// transientBucketError exists: a retryable bucket failure has to be
// recognised by the classifier Store.retry already consults, so that one
// policy — maxAttempts and retryBackoff — governs both the database and the
// bucket.
func TestBucketUsesTheStoresRetryPolicy(t *testing.T) {
	t.Parallel()

	transient := fmt.Errorf("the bucket is busy: %w", context.DeadlineExceeded)
	permanent := errors.New("AccessDenied: not your bucket")

	cases := []struct {
		name     string
		failures int
		err      error
		attempts int
		wantErr  bool
	}{
		{"a transient failure is tried again", 2, transient, 3, false},
		{"a permanent failure is not", 2, permanent, 1, true},
		{"retries are bounded", 99, transient, 3, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := bucketFor(t, "mem://")

			var attempts int

			// The same loop Store.retry runs, down to the dialect's own
			// classifier. If the bucket's errors did not fit that classifier
			// this test would fail, which is the point of it.
			b.setRetry(func(ctx context.Context, _ string, fn func(context.Context) error) error {
				const maxAttempts = 3

				var last error

				for range maxAttempts {
					attempts++

					err := fn(ctx)
					if err == nil {
						return nil
					}

					if !(sqliteDialect{}).isTransient(err) {
						return err
					}

					last = err
				}

				return last
			})

			remaining := tc.failures

			err := b.do(t.Context(), "a bucket operation", func(context.Context) error {
				if remaining > 0 {
					remaining--

					return tc.err
				}

				return nil
			})

			if (err != nil) != tc.wantErr {
				t.Errorf("do returned %v, wantErr %v", err, tc.wantErr)
			}

			if attempts != tc.attempts {
				t.Errorf("the operation ran %d times, want %d", attempts, tc.attempts)
			}
		})
	}
}

// TestBucketDoesNotRetryACancelledCaller keeps a shutdown fast: a caller that
// gave up is not a flaky bucket.
func TestBucketDoesNotRetryACancelledCaller(t *testing.T) {
	t.Parallel()

	b := bucketFor(t, "mem://")

	attempts := 0

	b.setRetry(func(ctx context.Context, _ string, fn func(context.Context) error) error {
		var last error

		for range 3 {
			attempts++

			if err := fn(ctx); err != nil {
				if !(sqliteDialect{}).isTransient(err) {
					return err
				}

				last = err

				continue
			}

			return nil
		}

		return last
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := b.do(ctx, "a bucket operation", func(context.Context) error {
		return fmt.Errorf("aborted: %w", context.Canceled)
	})
	if err == nil {
		t.Fatal("do succeeded with a cancelled context")
	}

	if attempts != 1 {
		t.Errorf("a cancelled caller was retried %d times, want 1 attempt", attempts)
	}
}

// TestTransientBucketErrorIsTransparent guards the wrapper: it must not hide
// the provider's error from errors.Is or from gcerrors.
func TestTransientBucketErrorIsTransparent(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("upload failed: %w", context.DeadlineExceeded)
	wrapped := &transientBucketError{err: inner}

	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("the wrapper hides the error it wraps")
	}

	if wrapped.Error() != inner.Error() {
		t.Errorf("the wrapper reports %q, want %q", wrapped.Error(), inner.Error())
	}

	if !isTransientMessage(wrapped) {
		t.Error("the store's own classifier does not recognise the wrapper as transient")
	}
}

// TestValidKindAcceptsTheKindsThisStoreWrites keeps the whitelist honest
// against the kinds the rest of wsaw actually uses.
func TestValidKindAcceptsTheKindsThisStoreWrites(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"body", "result", "screenshot-before-consent", "screenshot-after-consent"} {
		if !validKind(kind) {
			t.Errorf("validKind(%q) = false; that kind is written today", kind)
		}
	}
}
