//go:build objectstore && cloudblob

package objectstore

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"
	"gocloud.dev/gcerrors"
)

// Where a real object store does not behave like a directory.
//
// These are the tests Story 8.9, AC2 is really asking for. The cycle test next
// door proves wsaw works against MinIO; these pin down *why it might not have*,
// one provider behaviour at a time, so that a claim made in a comment about S3
// somewhere in internal/store has something checking it. Each of them names the
// promise in the design that rests on the behaviour.
//
// They talk to the bucket directly rather than through the store, because what
// is under test here is the provider.

// openBucket opens the object store, or a subtree of it.
func openBucket(t *testing.T, prefix string) *blob.Bucket {
	t.Helper()

	b, err := blob.OpenBucket(t.Context(), minioBucketURL(prefix))
	if err != nil {
		t.Fatalf("opening the object store: %v", err)
	}

	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("closing the object store: %v", err)
		}
	})

	return b
}

// openDirectory opens a bucket that is a directory, for the comparisons.
//
// The options are the ones openFileBucket uses in the store — no sidecar
// metadata, and the directory created — because a comparison against a
// differently-configured local bucket would be comparing the wrong two things.
func openDirectory(t *testing.T) *blob.Bucket {
	t.Helper()

	dir := filepath.ToSlash(t.TempDir())

	b, err := blob.OpenBucket(t.Context(), "file://"+dir+"?create_dir=true&metadata=skip")
	if err != nil {
		t.Fatalf("opening a directory as a bucket: %v", err)
	}

	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("closing the directory: %v", err)
		}
	})

	return b
}

// keysUnder is every key a listing of prefix returns, in the order the provider
// returned them.
func keysUnder(t *testing.T, b *blob.Bucket, prefix string) []string {
	t.Helper()

	var keys []string

	iter := b.List(&blob.ListOptions{Prefix: prefix})

	for {
		obj, err := iter.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return keys
		}

		if err != nil {
			t.Fatalf("listing %q: %v", prefix, err)
		}

		keys = append(keys, obj.Key)
	}
}

// write puts one object with the content type wsaw writes.
func write(t *testing.T, b *blob.Bucket, key string, body []byte) {
	t.Helper()

	if err := b.WriteAll(t.Context(), key, body, &blob.WriterOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		t.Fatalf("writing %s: %v", key, err)
	}
}

// TestListingOrderIsTheByteOrderOfTheKeys is the behaviour the whole
// bucket-index layout rests on (Story 8.10, AC4): a fold reads a prefix listing
// and trusts the order it arrives in, rather than sorting what the bucket holds.
//
// It also pins down the one difference between a real object store and a
// directory that this project has written down and never checked against a real
// one. fileblob lists by walking directories, so it orders "screenshot/" before
// "screenshot-after-consent/" — the directory names compare as "screenshot" and
// "screenshot-after-consent". S3 has no directories and orders the whole key, so
// "screenshot-after-consent/…" comes first, because "-" (0x2d) sorts below "/"
// (0x2f).
//
// That is exactly the case indexRefPrefix documents and excludes by listing one
// kind at a time. Here it is, on the provider.
func TestListingOrderIsTheByteOrderOfTheKeys(t *testing.T) {
	needMinIO(t)

	// Four keys rather than two, and three of them in one directory: fileblob
	// walks the tree and then swaps adjacent pairs that came out inverted, so a
	// key that has to move more than one position is the case where the two
	// orders genuinely part company. This is that case, and it is the shape the
	// reference pins have — an object whose kind is a prefix of a sibling kind.
	keys := []string{
		"screenshot/aaaa",
		"screenshot/bbbb",
		"screenshot/cccc",
		"screenshot-after-consent/dddd",
	}

	remote := openBucket(t, fmt.Sprintf("order-%d/", time.Now().UnixNano()))
	local := openDirectory(t)

	for _, key := range keys {
		write(t, remote, key, []byte("x"))
		write(t, local, key, []byte("x"))
	}

	sorted := slices.Clone(keys)
	slices.Sort(sorted)

	got := keysUnder(t, remote, "")
	if !slices.Equal(got, sorted) {
		t.Errorf("the object store listed\n  %v\nwant the byte order\n  %v", got, sorted)
	}

	// The comparison, and the reason the index never lists this prefix whole.
	// If this ever stops differing, the exception in indexRefPrefix can go —
	// and until then, a listing that assumed one order would silently read the
	// other on the provider an operator is paying for.
	onDisk := keysUnder(t, local, "")

	if slices.Equal(onDisk, got) {
		t.Log("note: the directory bucket now agrees with the object store on this listing, " +
			"so the ordering exception documented at indexRefPrefix may no longer be needed")
	} else {
		t.Logf("the two providers order this listing differently, as indexRefPrefix says:\n"+
			"  object store %v\n  directory    %v", got, onDisk)
	}

	// Each prefix the index actually lists is order-stable on both, which is
	// the property the exception buys.
	for _, prefix := range []string{"screenshot/", "screenshot-after-consent/"} {
		if remoteKeys, localKeys := keysUnder(t, remote, prefix), keysUnder(t, local, prefix); !slices.Equal(remoteKeys, localKeys) {
			t.Errorf("listing %q gave %v on the object store and %v on disk", prefix, remoteKeys, localKeys)
		}
	}
}

// TestAListingPagesThroughEverything is Story 8.10, AC11 against a provider
// that really does page.
//
// A directory and a memory bucket produce their pages from a slice they already
// hold, so the page token is theirs to invent and no page is ever served from a
// different view. S3 hands back a continuation token from the service, caps a
// page at a thousand keys whatever was asked for, and is where a paging loop
// that dropped or repeated a page would first show it.
func TestAListingPagesThroughEverything(t *testing.T) {
	needMinIO(t)

	b := openBucket(t, fmt.Sprintf("paging-%d/", time.Now().UnixNano()))

	// More than artifactListPageSize, which is 256, so the store's own loop
	// makes several round trips.
	const objects = 300

	want := make([]string, 0, objects)

	for i := range objects {
		key := fmt.Sprintf("body/%04d", i)
		want = append(want, key)

		write(t, b, key, []byte("x"))
	}

	slices.Sort(want)

	// Paged the way bucket.list pages: by token, at the store's page size.
	var (
		got   []string
		token = blob.FirstPageToken
		pages int
	)

	for len(token) > 0 {
		page, next, err := b.ListPage(t.Context(), token, 256, &blob.ListOptions{Prefix: "body/"})
		if err != nil {
			t.Fatalf("listing page %d: %v", pages+1, err)
		}

		pages++

		for _, obj := range page {
			got = append(got, obj.Key)
		}

		token = next
	}

	if pages < 2 {
		t.Errorf("%d objects came back in %d page(s); this test is not paging anything", objects, pages)
	}

	if !slices.Equal(got, want) {
		t.Errorf("paging returned %d keys, want %d, and %v",
			len(got), len(want), map[bool]string{true: "in the wrong order", false: "with the wrong contents"}[len(got) == len(want)])
	}
}

// TestWhatTheProviderSaysAboutAnObject covers the attributes the store reads
// back and the two it depends on.
//
// Size decides a Content-Length before a byte is served (Story 8.7, AC4), and
// ModTime is the only clock compaction and the sweep's grace period are decided
// against (Story 8.10, AC11). Both come from the provider, and neither is a
// filesystem's.
func TestWhatTheProviderSaysAboutAnObject(t *testing.T) {
	needMinIO(t)

	b := openBucket(t, fmt.Sprintf("attrs-%d/", time.Now().UnixNano()))

	const body = "a stored body of a known length"

	before := time.Now()

	write(t, b, "body/known", []byte(body))

	attrs, err := b.Attributes(t.Context(), "body/known")
	if err != nil {
		t.Fatalf("reading attributes: %v", err)
	}

	if attrs.Size != int64(len(body)) {
		t.Errorf("the object is %d bytes by the provider, %d by the writer", attrs.Size, len(body))
	}

	// Not in the future, which is the condition usableModTime refuses to time a
	// deletion against, and not absent, which would disable every grace period
	// there is.
	switch {
	case attrs.ModTime.IsZero():
		t.Error("the provider reports no modification time; every grace period is decided against one")

	case attrs.ModTime.After(time.Now().Add(time.Minute)):
		t.Errorf("the provider dates the object in the future: %s", attrs.ModTime)
	}

	// The granularity is the provider's, and S3's is whole seconds where a
	// filesystem's is nanoseconds. Logged rather than asserted, because what
	// the store needs is that an object written before another is not dated
	// after it — not any particular resolution. A grace period is hours.
	t.Logf("the object store dates objects to %s (a directory dates them to the nanosecond)",
		attrs.ModTime.Sub(attrs.ModTime.Truncate(time.Second)))

	if truncated := attrs.ModTime.Truncate(time.Second).Before(before.Truncate(time.Second)); truncated {
		t.Errorf("the object is dated %s, before the write at %s", attrs.ModTime, before)
	}

	// The content type is the one wsaw writes deliberately, so a provider
	// serving the object directly cannot be talked into calling a captured
	// script a document (Story 5.17, AC5).
	if attrs.ContentType != "application/octet-stream" {
		t.Errorf("the provider reports content type %q, want the one that was written", attrs.ContentType)
	}
}

// TestAnEmptyObjectIsAnObject is what the bucket index's reference pins are: a
// zero-byte object whose whole content is its key (Story 8.10, §7.4).
//
// A directory stores an empty file without noticing. An object store, a proxy
// in front of one, or an SDK that skips a body of length zero has more ways to
// get it wrong, and a pin that did not exist would make retention delete
// evidence a result still names.
func TestAnEmptyObjectIsAnObject(t *testing.T) {
	needMinIO(t)

	b := openBucket(t, fmt.Sprintf("empty-%d/", time.Now().UnixNano()))

	const key = "_wsaw/index/v1/ref/body/" +
		"0000000000000000000000000000000000000000000000000000000000000000/r.scan-1"

	write(t, b, key, nil)

	found, err := b.Exists(t.Context(), key)
	if err != nil || !found {
		t.Fatalf("a zero-byte pin is not there: %v, %v", found, err)
	}

	attrs, err := b.Attributes(t.Context(), key)
	if err != nil {
		t.Fatalf("reading the pin's attributes: %v", err)
	}

	if attrs.Size != 0 {
		t.Errorf("the pin is %d bytes, want 0", attrs.Size)
	}

	if keys := keysUnder(t, b, "_wsaw/"); !slices.Equal(keys, []string{key}) {
		t.Errorf("a listing of the pins returned %v, want just the one", keys)
	}
}

// TestTheProviderCodesTheStoreClassifiesBy is Story 8.1, AC8: every decision
// bucket.go makes about a failure is made from gcerrors.Code and never from a
// message, because the messages differ between providers and change with their
// SDKs. The codes are what has to be the same, and this is where that is
// checked against a real one.
func TestTheProviderCodesTheStoreClassifiesBy(t *testing.T) {
	needMinIO(t)

	b := openBucket(t, fmt.Sprintf("codes-%d/", time.Now().UnixNano()))
	local := openDirectory(t)

	for name, bucket := range map[string]*blob.Bucket{"object store": b, "directory": local} {
		t.Run(name, func(t *testing.T) {
			// A key that is not there. NotFound is what tells pruned evidence
			// from a bucket that is broken, and getting it wrong turns one into
			// the other.
			_, err := bucket.ReadAll(t.Context(), "result/missing")
			if code := gcerrors.Code(err); code != gcerrors.NotFound {
				t.Errorf("reading a missing key = %v (code %v), want NotFound", err, code)
			}

			if code := gcerrors.Code(bucket.Delete(t.Context(), "result/missing")); code != gcerrors.NotFound {
				t.Errorf("deleting a missing key = code %v, want NotFound", code)
			}

			// The conditional create the write path uses so that two scans
			// content-addressing to one key cannot rewrite each other
			// (Story 8.1, AC4). The loser has to be refused, not served.
			write(t, bucket, "result/taken", []byte("the first bytes"))

			err = bucket.WriteAll(t.Context(), "result/taken", []byte("the second bytes"),
				&blob.WriterOptions{ContentType: "application/octet-stream", IfNotExist: true})

			if code := gcerrors.Code(err); code != gcerrors.FailedPrecondition {
				t.Errorf("a conditional write over an existing key = %v (code %v), want FailedPrecondition", err, code)
			}

			body, err := bucket.ReadAll(t.Context(), "result/taken")
			if err != nil {
				t.Fatal(err)
			}

			if string(body) != "the first bytes" {
				t.Errorf("the refused write changed the object to %q", body)
			}
		})
	}
}
