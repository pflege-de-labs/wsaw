package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// White-box tests of the index's door onto the bucket (Story 8.10, §6.1).
//
// Every behavioural test runs against both providers, for the same reason the
// artifact door's do: the whole value of the seam is that where the index
// lives stops mattering, and the one place that is not true — the order a
// listing arrives in — is the subject of its own test below.

// indexBody is a stand-in for an index object's contents. Its bytes never
// matter to the door, which is the point: the door validates the key and
// leaves the body to whoever writes it.
var indexBody = []byte(`{"layout":1}`)

// anIndexKey returns a well-formed entry key for a scan, so that a test that
// only needs "some key this store writes" does not have to build one.
func anIndexKey(t *testing.T, scanID string) string {
	t.Helper()

	key := exampleSeries().entry(exampleStart, sk(scanID), tm(model.TermIdle)).String()
	if err := validateIndexKey(key); err != nil {
		t.Fatalf("the test built an invalid key %q: %v", key, err)
	}

	return key
}

// TestPutIndexCreatesAKeyAndSaysSo is the tri-state of §6.1.
//
// bucket.write maps the lost conditional-create race to a silent nil, which is
// right for an artifact because the key already holds those exact bytes. An
// index key is not content-addressed — a byid key is named after a scan, not
// after its body — so the same swallow here would hide a scan ID reused for
// different content behind a successful write.
func TestPutIndexCreatesAKeyAndSaysSo(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)
			key := anIndexKey(t, "scan-first")

			outcome, err := b.putIndex(ctx, key, indexBody)
			if err != nil {
				t.Fatalf("first putIndex: %v", err)
			}

			if outcome != putCreated {
				t.Errorf("first putIndex reported %v, want putCreated", outcome)
			}

			// The same fact written twice. I1 says two writers of one fact
			// write the same bytes, so this is the ordinary case and it must
			// be reported as the no-op it is rather than as fresh work (AC10).
			outcome, err = b.putIndex(ctx, key, indexBody)
			if err != nil {
				t.Fatalf("second putIndex: %v", err)
			}

			if outcome != putExisted {
				t.Errorf("re-writing an identical object reported %v, want putExisted", outcome)
			}

			// Different bytes under the same key: still reported as existing,
			// and — this is the half that matters — the first write survives.
			// The door never rewrites (AC3).
			outcome, err = b.putIndex(ctx, key, []byte(`{"layout":2}`))
			if err != nil {
				t.Fatalf("conflicting putIndex: %v", err)
			}

			if outcome != putExisted {
				t.Errorf("a conflicting write reported %v, want putExisted", outcome)
			}

			got, err := b.getIndex(ctx, key)
			if err != nil {
				t.Fatalf("getIndex: %v", err)
			}

			if string(got) != string(indexBody) {
				t.Errorf("the object now holds %q, want the bytes the first write put there, %q", got, indexBody)
			}
		})
	}
}

// TestPutIndexWritesTheZeroByteMarkers covers the two productions whose whole
// content is their name — a tombstone and a ref marker — because an empty body
// is exactly the case a provider or a wrapper is most likely to treat as
// "nothing to write".
func TestPutIndexWritesTheZeroByteMarkers(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b := bucketFor(t, location)
			s := exampleSeries()

			marker, err := newRefMarker(exampleArtifact, resultRefOwner(s, sk(exampleScan)))
			if err != nil {
				t.Fatalf("newRefMarker: %v", err)
			}

			for _, key := range []string{
				s.tombstone(exampleStart, sk(exampleScan)).String(),
				marker.String(),
			} {
				assertZeroByteMarker(t, b, key)
			}
		})
	}
}

// assertZeroByteMarker writes one marker and reads it back both ways, because
// an empty object has to be listable and statable as well as present: the
// grace checks in compaction and prune read a marker's modification time and
// never its body.
func assertZeroByteMarker(t *testing.T, b *bucket, key string) {
	t.Helper()

	ctx := t.Context()

	if _, err := b.putIndex(ctx, key, nil); err != nil {
		t.Fatalf("putIndex(%q, nil): %v", key, err)
	}

	body, err := b.getIndex(ctx, key)
	if err != nil {
		t.Fatalf("getIndex(%q): %v", key, err)
	}

	if len(body) != 0 {
		t.Errorf("getIndex(%q) returned %d bytes, want none", key, len(body))
	}

	obj, err := b.statIndex(ctx, key)
	if err != nil {
		t.Fatalf("statIndex(%q): %v", key, err)
	}

	if obj.size != 0 || obj.key != key {
		t.Errorf("statIndex(%q) = %+v, want a zero-byte object under that key", key, obj)
	}

	if obj.modTime.IsZero() {
		t.Errorf("statIndex(%q) reported no modification time; the grace checks need one", key)
	}
}

// TestIndexObjectsThatAreNotThereAreNotFound keeps the distinction every
// caller in this package already relies on: an object that is absent is not a
// bucket that is broken. Whether "absent" then means "not yet" or "never" is
// the reader's judgement, and this layer does not guess (AC6).
func TestIndexObjectsThatAreNotThereAreNotFound(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)
			key := anIndexKey(t, "scan-absent")

			if _, err := b.getIndex(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("getIndex of an absent key = %v, want ErrNotFound", err)
			}

			if _, err := b.statIndex(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("statIndex of an absent key = %v, want ErrNotFound", err)
			}

			if err := b.deleteIndex(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("deleteIndex of an absent key = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestDeleteIndexRemovesOneKey covers the operation AC3 keeps out of every
// ordinary write: only compaction and prune delete, and both need "somebody
// else already collected it" to read as success rather than as a failure to
// explain.
func TestDeleteIndexRemovesOneKey(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)
			key := anIndexKey(t, "scan-doomed")
			survivor := anIndexKey(t, "scan-kept")

			for _, k := range []string{key, survivor} {
				if _, err := b.putIndex(ctx, k, indexBody); err != nil {
					t.Fatalf("putIndex(%q): %v", k, err)
				}
			}

			if err := b.deleteIndex(ctx, key); err != nil {
				t.Fatalf("deleteIndex: %v", err)
			}

			if _, err := b.getIndex(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("the deleted key reads back as %v, want ErrNotFound", err)
			}

			if _, err := b.getIndex(ctx, survivor); err != nil {
				t.Errorf("the neighbouring key went too: %v", err)
			}

			// Deleting it again is the concurrent-collector case, and it is
			// reported as absence rather than as a fault.
			if err := b.deleteIndex(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("the second deleteIndex = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestListIndexPageWalksOnePrefixAndStops is the read shape §6.3 depends on: a
// fold that wants the newest few entries reads one page and stops, and that is
// the difference between a page render costing one request and costing a
// listing of the whole history (AC11).
func TestListIndexPageWalksOnePrefixAndStops(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)
			s := exampleSeries()
			want := fillOneSeries(t, b, s, 25)

			// A key in another series must not appear in this listing; the
			// prefix is the whole of the query plan (AC4).
			other := seriesFor("https://elsewhere.example/", model.ConsentAccept)
			if _, err := b.putIndex(ctx, other.entry(exampleStart, sk("scan-other"), tm(model.TermIdle)).String(),
				indexBody); err != nil {
				t.Fatalf("putIndex for another series: %v", err)
			}

			// One page, bounded by what the caller wants.
			page, err := b.listIndexPage(ctx, s.dirPrefix(), indexCursor{}, 10)
			if err != nil {
				t.Fatalf("listIndexPage: %v", err)
			}

			if len(page.objects) != 10 {
				t.Errorf("the first page holds %d keys, want 10", len(page.objects))
			}

			if !page.next.more() {
				t.Error("the first page reports the listing complete with 15 keys still to come")
			}

			// And the whole prefix, page by page, in ten-key pages so the
			// cursor is exercised rather than assumed.
			if got := listAllIndexKeys(t, b, s.dirPrefix(), 10); !slices.Equal(got, want) {
				t.Errorf("the listing returned %d keys in the wrong order:\n got %v\nwant %v", len(got), got, want)
			}
		})
	}
}

// fillOneSeries writes n entries into one series and returns their keys in the
// order a listing must return them.
//
// The instants descend as the loop counts up, so the write order disagrees
// with the read order and a store that returned what it was given would fail
// here rather than pass by coincidence.
func fillOneSeries(t *testing.T, b *bucket, s seriesID, n int) []string {
	t.Helper()

	want := make([]string, 0, n)

	for i := range n {
		at := exampleStart.Add(-time.Duration(i) * time.Second)
		key := s.entry(at, sk(fmt.Sprintf("scan-%02d", i)), tm(model.TermIdle)).String()
		want = append(want, key)

		if _, err := b.putIndex(t.Context(), key, indexBody); err != nil {
			t.Fatalf("putIndex(%q): %v", key, err)
		}
	}

	slices.Sort(want)

	return want
}

// TestAFinishedListingCannotBeContinued is the discipline gocloud enforces on
// its own page token and that this door must not lose in wrapping it.
//
// gocloud spells "start here" and "nothing left" as two different values
// precisely because a loop that passed the second one back would fetch the
// first page again, forever. A cursor that mapped an empty token onto "start"
// would hand that bug straight back to the caller.
func TestAFinishedListingCannotBeContinued(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")
	key := anIndexKey(t, "scan-only")

	if _, err := b.putIndex(ctx, key, indexBody); err != nil {
		t.Fatalf("putIndex: %v", err)
	}

	page, err := b.listIndexPage(ctx, exampleSeries().dirPrefix(), indexCursor{}, indexListPageSize)
	if err != nil {
		t.Fatalf("listIndexPage: %v", err)
	}

	if page.next.more() {
		t.Fatal("a listing that returned everything reports more pages")
	}

	if _, err := b.listIndexPage(ctx, exampleSeries().dirPrefix(), page.next, indexListPageSize); !errors.Is(
		err, errInvalidIndexKey,
	) {
		t.Errorf("continuing a finished listing = %v, want a refusal rather than the first page again", err)
	}
}

// TestTheIndexDoorRefusesEveryKeyThisStoreDoesNotWrite is the door itself.
//
// The artifact door's whitelist is right about exactly one shape and its value
// is that it stays that way, so the index gets a second whitelist rather than
// a relaxation of the first. Each operation is checked separately, because a
// door with one unguarded hinge is not a door — and the refusal has to happen
// before the bucket is touched, which is what "the key is not one this store
// writes" means for a key a crafted document supplied (Tenet 9).
func TestTheIndexDoorRefusesEveryKeyThisStoreDoesNotWrite(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")
	digest := strings.Repeat("a", digestLength)

	hostile := map[string]string{
		"an artifact key":               artifactKindResult + refSeparator + digest,
		"a screenshot key":              testKind + refSeparator + digest,
		"a bare digest":                 digest,
		"empty":                         "",
		"the bucket root":               refSeparator,
		"a traversal":                   "../../etc/passwd",
		"a traversal inside a key":      indexPrefix + "series/../../../etc/passwd",
		"an absolute path":              refSeparator + indexPrefix + "targets/x.reject",
		"another tool's object":         "vendor-export/" + digest,
		"a near miss on the root":       "_wsawx/index/v1/targets/" + exampleTK + ".reject",
		"the root with no leaf":         indexRoot,
		"a directory rather than a key": exampleSeries().dirPrefix(),
		"a nul byte":                    anIndexKey(t, "scan-x") + "\x00",
		"an over-long key":              indexRebuildPrefix + strings.Repeat("a", maxIndexKeyBytes),
	}

	for name, key := range hostile {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := b.putIndex(ctx, key, indexBody); !errors.Is(err, errInvalidIndexKey) {
				t.Errorf("putIndex(%q) = %v, want an invalid-key error", key, err)
			}

			if _, err := b.getIndex(ctx, key); !errors.Is(err, errInvalidIndexKey) {
				t.Errorf("getIndex(%q) = %v, want an invalid-key error", key, err)
			}

			if _, err := b.statIndex(ctx, key); !errors.Is(err, errInvalidIndexKey) {
				t.Errorf("statIndex(%q) = %v, want an invalid-key error", key, err)
			}

			if err := b.deleteIndex(ctx, key); !errors.Is(err, errInvalidIndexKey) {
				t.Errorf("deleteIndex(%q) = %v, want an invalid-key error", key, err)
			}
		})
	}
}

// TestTheIndexDoorRefusesEveryPrefixOutsideTheIndex is the listing half. A
// prefix cannot be parsed the way a key can, so the check is that it is inside
// the root and spelled in bytes the grammar can produce — which is what stops
// a listing from enumerating the evidence or another tool's objects.
func TestTheIndexDoorRefusesEveryPrefixOutsideTheIndex(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")

	refused := map[string]string{
		"the whole bucket":           "",
		"an artifact kind":           artifactKindResult + refSeparator,
		"a near miss on the root":    "_wsawx" + refSeparator,
		"a traversal":                indexPrefix + "..",
		"a traversal in the middle":  indexPrefix + "../series/",
		"a current directory":        indexPrefix + "./series/",
		"a doubled separator":        indexSeriesPrefix + refSeparator,
		"a leading separator":        refSeparator + indexPrefix,
		"an uppercase segment":       strings.ToUpper(indexSeriesPrefix),
		"a byte outside the grammar": indexSeriesPrefix + "a b",
		"a nul byte":                 indexSeriesPrefix + "\x00",
		"an over-long prefix":        indexSeriesPrefix + strings.Repeat("a", maxIndexKeyBytes),
	}

	for name, prefix := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := b.listIndexPage(ctx, prefix, indexCursor{}, indexListPageSize); !errors.Is(
				err, errInvalidIndexKey,
			) {
				t.Errorf("listIndexPage(%q) = %v, want an invalid-key error", prefix, err)
			}
		})
	}

	// A partial key inside the root is a legitimate narrowing — it is how the
	// sweep asks for the pins on one artifact — and must be accepted.
	pinned, err := refMarkerDirPrefix(exampleArtifact)
	if err != nil {
		t.Fatalf("refMarkerDirPrefix: %v", err)
	}

	for _, prefix := range []string{indexRoot, indexPrefix, indexSeriesPrefix, exampleSeries().dirPrefix(), pinned} {
		if _, err := b.listIndexPage(ctx, prefix, indexCursor{}, indexListPageSize); err != nil {
			t.Errorf("listIndexPage(%q): %v", prefix, err)
		}
	}

	// And a reference the grammar cannot pin is refused before it becomes a
	// prefix, because the references reaching it come out of stored documents.
	if _, err := refMarkerDirPrefix("../../etc/passwd"); !errors.Is(err, errInvalidRef) {
		t.Errorf("refMarkerDirPrefix of a traversal = %v, want an invalid-reference error", err)
	}
}

// TestTheTwoDoorsCannotReachEachOthersObjects is §4.1's disjointness at the
// bucket rather than in the grammar: not only do the validators disagree about
// each other's keys, the two key spaces do not overlap in a live bucket, so
// neither door's listing can see the other's objects.
//
// This is what lets the index and the evidence share one bucket, and it is
// what Story 8.5's sweep depends on when it walks the bucket and leaves alone
// what wsaw's artifact grammar did not write.
func TestTheTwoDoorsCannotReachEachOthersObjects(t *testing.T) {
	t.Parallel()

	for name, location := range locations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			b := bucketFor(t, location)

			ref, err := b.put(ctx, testKind, []byte("a screenshot"))
			if err != nil {
				t.Fatalf("put: %v", err)
			}

			indexKey := anIndexKey(t, "scan-side-by-side")
			if _, err := b.putIndex(ctx, indexKey, indexBody); err != nil {
				t.Fatalf("putIndex: %v", err)
			}

			// The artifact door cannot name the index object.
			if _, err := b.get(ctx, indexKey); !errors.Is(err, errInvalidRef) {
				t.Errorf("the artifact door read the index key: %v", err)
			}

			// The index door cannot name the artifact.
			if _, err := b.getIndex(ctx, ref); !errors.Is(err, errInvalidIndexKey) {
				t.Errorf("the index door read the artifact: %v", err)
			}

			assertOnlyTheArtifactLooksLikeOne(t, b, ref, indexKey)

			// And a listing through the index door sees only the index.
			page, err := b.listIndexPage(ctx, indexRoot, indexCursor{}, indexListPageSize)
			if err != nil {
				t.Fatalf("listIndexPage: %v", err)
			}

			if len(page.objects) != 1 || page.objects[0].key != indexKey {
				t.Errorf("a listing of the index root returned %+v, want only %q", page.objects, indexKey)
			}
		})
	}
}

// assertOnlyTheArtifactLooksLikeOne walks the whole bucket through the
// artifact door and checks that exactly one of the keys it finds is one wsaw's
// artifact grammar wrote.
//
// This is the shape Story 8.5's sweep reads the bucket in, and the assertion
// is the one the sweep's safety rests on: the index object is visible to the
// walk — it is in the same bucket — and is not garbage of the sweep's own
// making (Story 8.5, AC4).
func assertOnlyTheArtifactLooksLikeOne(t *testing.T, b *bucket, ref, indexKey string) {
	t.Helper()

	var walked []string

	if err := b.list(t.Context(), "", func(obj artifactObject) error {
		walked = append(walked, obj.ref)

		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	if !slices.Contains(walked, indexKey) {
		t.Fatalf("a walk of the bucket missed the index object entirely; it holds %v", walked)
	}

	for _, key := range walked {
		if isArtifactRef(key) != (key == ref) {
			t.Errorf("isArtifactRef(%q) = %v; only the artifact is one", key, isArtifactRef(key))
		}
	}
}

// TestIndexObjectsShareTheBucketsRetryPolicy is the reason this file borrows
// bucket.do rather than reimplementing what sits under it.
//
// A second copy of the retry loop would be a second answer to "is this failure
// transient", and the two would drift. The assertion is that every one of the
// five operations goes through the store's retrier, so a policy set once
// governs the index as well as the evidence.
func TestIndexObjectsShareTheBucketsRetryPolicy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	b := bucketFor(t, "mem://")
	key := anIndexKey(t, "scan-retried")

	var ops []string

	b.setRetry(func(ctx context.Context, op string, fn func(context.Context) error) error {
		ops = append(ops, op)

		return fn(ctx)
	})

	if _, err := b.putIndex(ctx, key, indexBody); err != nil {
		t.Fatalf("putIndex: %v", err)
	}

	if _, err := b.getIndex(ctx, key); err != nil {
		t.Fatalf("getIndex: %v", err)
	}

	if _, err := b.statIndex(ctx, key); err != nil {
		t.Fatalf("statIndex: %v", err)
	}

	if _, err := b.listIndexPage(ctx, indexRoot, indexCursor{}, indexListPageSize); err != nil {
		t.Fatalf("listIndexPage: %v", err)
	}

	if err := b.deleteIndex(ctx, key); err != nil {
		t.Fatalf("deleteIndex: %v", err)
	}

	want := []string{
		"writing an index object",
		"reading an index object",
		"reading index object attributes",
		"listing index objects",
		"deleting an index object",
	}

	if !slices.Equal(ops, want) {
		t.Errorf("the index door ran %v through the retrier, want %v", ops, want)
	}
}

// TestIndexObjectsOnLocalDiskAreReadableOnlyByWsaw keeps the decision the
// artifact door made: an index object carries a scan's summary, its target and
// its consent mode, which is as much a description of a person's browsing as
// the evidence it points at (Tenet 19). fileblob would otherwise publish it
// with whatever the umask allowed.
func TestIndexObjectsOnLocalDiskAreReadableOnlyByWsaw(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	b := bucketFor(t, dir)
	key := anIndexKey(t, "scan-private")

	if _, err := b.putIndex(t.Context(), key, indexBody); err != nil {
		t.Fatalf("putIndex: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := info.Mode().Perm(); got != artifactFileMode {
		t.Errorf("the index object is mode %v, want %v", got, artifactFileMode)
	}
}

// TestEveryPrefixTheIndexListsOrdersTheSameOnEveryProvider is the guard on the
// listing-order invariant of §4.2, and it is a test rather than a comment
// because the hazard is invisible in review.
//
// fileblob lists by walking the directory tree and does not sort the keys that
// walk produces; a cloud provider sorts the whole key. The two agree only where
// no entry's name is a proper prefix of a sibling's whose next byte sorts below
// "/". Every prefix this store lists is built to satisfy that, and this asserts
// it against the two providers actually available here rather than against the
// argument.
func TestEveryPrefixTheIndexListsOrdersTheSameOnEveryProvider(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	mem := bucketFor(t, "mem://")
	disk := bucketFor(t, filepath.Join(t.TempDir(), "artifacts"))

	keys, prefixes := orderingCorpus(t)

	for _, b := range []*bucket{mem, disk} {
		for _, key := range keys {
			if _, err := b.putIndex(ctx, key, indexBody); err != nil {
				t.Fatalf("putIndex(%q) into %s: %v", key, b, err)
			}
		}
	}

	for _, prefix := range prefixes {
		want := listAllIndexKeys(t, mem, prefix, indexListPageSize)

		sorted := slices.Clone(want)
		slices.Sort(sorted)

		if !slices.Equal(want, sorted) {
			t.Errorf("the memory bucket did not list %q in lexicographic order; the test's premise is wrong", prefix)

			continue
		}

		if got := listAllIndexKeys(t, disk, prefix, indexListPageSize); !slices.Equal(got, want) {
			t.Errorf("listing %q gave a different order on disk than in memory:\n disk %v\n  mem %v",
				prefix, got, want)
		}
	}
}

// orderingCorpus builds keys of every form across several targets, modes,
// instants, artifact kinds and owners, together with every prefix this store
// lists them under.
//
// The kinds include "screenshot" alongside "screenshot-after-consent" on
// purpose: they are the one pair in wsaw's whole key space where one name is a
// proper prefix of another, and they are why the ref markers are listed one
// kind at a time.
func orderingCorpus(t *testing.T) (keys, prefixes []string) {
	t.Helper()

	targets := []string{exampleTarget, "Site", "site", "https://a.example/", "https://a.example/deeper"}
	modes := []model.ConsentMode{model.ConsentNone, model.ConsentReject, model.ConsentAccept, "reject/ALL?"}
	kinds := []string{
		artifactKindBody, artifactKindResult, artifactKindScreenshot,
		"screenshot-before-consent", "screenshot-after-consent",
	}

	prefixes = []string{
		indexLayoutPrefix, indexTargetsPrefix, indexSeriesPrefix, indexByIDPrefix,
		indexBaselinePrefix, indexAuditPrefix, indexAuditCkptPrefix, indexRebuildPrefix,
	}

	keys = append(keys, layoutKey(indexLayoutVersion))

	for i, target := range targets {
		for j, mode := range modes {
			s := seriesFor(target, mode)
			prefixes = append(prefixes, s.dirPrefix(), s.byIDDirPrefix(), s.baselineDirPrefix())

			for k := range 3 {
				at := exampleStart.Add(time.Duration(i*100+j*10+k) * time.Microsecond)
				scan := sk(fmt.Sprintf("scan-%d%d%d", i, j, k))
				did := shortSum([]byte(at.String()), identityHexLen)

				keys = append(
					keys,
					s.markerKey(),
					s.entry(at, scan, tm(model.TermIdle)).String(),
					s.tombstone(at, scan).String(),
					s.checkpoint(at, did).String(),
					s.byIDKey(scan),
					s.decision(at, opApprove, did).String(),
					auditEntryAt(at, did).String(),
					auditCheckpointAt(at, did).String(),
					rebuildKey(scan),
				)

				for n, kind := range kinds {
					artifact := kind + refSeparator + shortSum([]byte(fmt.Sprint(i, j, k, n)), digestLength)

					marker, err := newRefMarker(artifact, resultRefOwner(s, scan))
					if err != nil {
						t.Fatalf("newRefMarker(%q): %v", artifact, err)
					}

					pins, err := refMarkerDirPrefix(artifact)
					if err != nil {
						t.Fatalf("refMarkerDirPrefix(%q): %v", artifact, err)
					}

					keys = append(keys, marker.String())
					prefixes = append(prefixes, pins, indexRefPrefix+kind+refSeparator)
				}
			}
		}
	}

	slices.Sort(keys)
	keys = slices.Compact(keys)
	slices.Sort(prefixes)
	prefixes = slices.Compact(prefixes)

	return keys, prefixes
}

// listAllIndexKeys pages a prefix to exhaustion and returns the keys in the
// order the provider gave them.
func listAllIndexKeys(t *testing.T, b *bucket, prefix string, limit int) []string {
	t.Helper()

	var got []string

	for cursor := (indexCursor{}); cursor.more(); {
		page, err := b.listIndexPage(t.Context(), prefix, cursor, limit)
		if err != nil {
			t.Fatalf("listIndexPage(%q) on %s: %v", prefix, b, err)
		}

		for _, obj := range page.objects {
			got = append(got, obj.key)
		}

		cursor = page.next
	}

	return got
}

// TestListingEveryRefMarkerAtOnceIsNotOrderStable records the one prefix the
// grammar deliberately does not make order-stable, as an executable fact
// rather than as a warning nobody reads.
//
// ref/ holds one directory per artifact kind, and validKind admits
// "screenshot" alongside "screenshot-before-consent": a name that is a proper
// prefix of a sibling's whose next byte is "-", which sorts below "/". fileblob
// walks into the shorter directory first while a cloud provider sorts the
// longer key ahead, so a whole-ref/ listing comes back in two different orders.
//
// This costs the design nothing, because the artifact key space it is joined
// against — "<kind>/<digest>" at the bucket root — has the identical property
// for the identical reason. §7.4's merge join streams both sides one kind at a
// time, and per kind both are stable, which the test above asserts. If a later
// change makes this listing stable, this test fails, and the right response is
// to delete it and relax the per-kind requirement — not to work around it.
func TestListingEveryRefMarkerAtOnceIsNotOrderStable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	disk := bucketFor(t, filepath.Join(t.TempDir(), "artifacts"))
	s := exampleSeries()

	for i, kind := range []string{artifactKindScreenshot, "screenshot-after-consent"} {
		for j := range 2 {
			artifact := kind + refSeparator + shortSum([]byte(fmt.Sprint(i, j)), digestLength)

			marker, err := newRefMarker(artifact, resultRefOwner(s, sk(exampleScan)))
			if err != nil {
				t.Fatalf("newRefMarker: %v", err)
			}

			if _, err := disk.putIndex(ctx, marker.String(), nil); err != nil {
				t.Fatalf("putIndex: %v", err)
			}
		}
	}

	got := listAllIndexKeys(t, disk, indexRefPrefix, indexListPageSize)

	sorted := slices.Clone(got)
	slices.Sort(sorted)

	if slices.Equal(got, sorted) {
		t.Errorf("a whole-ref/ listing is now lexicographic on disk: %v\n"+
			"If fileblob started sorting, delete this test and the per-kind requirement it documents.", got)
	}
}
