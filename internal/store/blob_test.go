package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// What only the store with no database can get wrong (Story 8.10).
//
// These run whatever WSAW_TEST_STORE_DRIVER says, because they build their own
// options: the shared suite above is the specification every store answers, and
// this file is about the parts of this one that have no counterpart to compare
// against: which layout wrote the index (AC14), the exact objects one write
// leaves in the bucket (AC3, AC10), the fold that resolves a baseline from an
// append-only log (AC8), and what retention does to a history whose index is
// the bucket it is pruning (AC12).

// blobOptions names an empty bucket-index store.
func blobOptions(dir string) store.Options {
	return store.Options{
		Driver:      store.DriverBlob,
		ArtifactDir: dir,
		Logger:      slog.New(slog.DiscardHandler),
	}
}

// openBlob opens a bucket-index store on dir and closes it when the test ends.
func openBlob(t *testing.T, dir string) store.Store {
	t.Helper()

	s, err := store.Open(t.Context(), blobOptions(dir))
	if err != nil {
		t.Fatalf("opening a bucket-index store: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s
}

// layoutObject is the file one layout marker lands in, spelled out rather than
// asked of the code under test.
//
// The key is a promise to every future wsaw that opens the same bucket, so a
// test that derived it from the constants would agree with a change that
// silently orphaned an existing index (§4.5).
func layoutObject(dir string, version int) string {
	return filepath.Join(dir, "_wsaw", "index", "layout", fmt.Sprintf("%08d.json", version))
}

// writeLayoutObject puts a layout marker in the bucket behind the store's back,
// which is how a bucket written by another wsaw is simulated.
func writeLayoutObject(t *testing.T, dir string, version int, body string) {
	t.Helper()

	path := layoutObject(dir, version)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the layout directory: %v", err)
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing a layout object: %v", err)
	}
}

// TestOpeningAFreshBucketLaysOutTheIndex is the other half of AC14: a bucket
// that has never held an index gets one recorded, so the next wsaw to open it
// can tell which layout it is looking at rather than having to guess from the
// keys it finds.
func TestOpeningAFreshBucketLaysOutTheIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	at := time.Date(2026, 9, 8, 10, 45, 12, 0, time.UTC)

	opts := blobOptions(dir)
	opts.Now = func() time.Time { return at }
	opts.Version = "0.9.2"

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("opening a bucket-index store: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(layoutObject(dir, 1))
	if err != nil {
		t.Fatalf("the store recorded no layout: %v", err)
	}

	var rec struct {
		Layout    int    `json:"layout"`
		CreatedAt string `json:"createdAt"`
		WrittenBy string `json:"writtenBy"`
	}

	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("the layout object does not decode: %v (%s)", err, body)
	}

	if rec.Layout != 1 {
		t.Errorf("layout = %d, want 1", rec.Layout)
	}

	// From the injected clock, not from the host's: every timestamp this store
	// writes has to be reachable by a test without waiting for one.
	if rec.CreatedAt != "2026-09-08T10:45:12Z" {
		t.Errorf("createdAt = %q, want the clock the store was given", rec.CreatedAt)
	}

	if !strings.Contains(rec.WrittenBy, "0.9.2") {
		t.Errorf("writtenBy = %q, which does not name the build that wrote it", rec.WrittenBy)
	}
}

// TestOpeningAnIndexTwiceRewritesNothing is invariant I1 at the one key this
// step writes: no index object is ever written twice with different bytes, so
// a restart cannot change what the bucket says about itself (AC3).
func TestOpeningAnIndexTwiceRewritesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first := blobOptions(dir)
	first.Now = func() time.Time { return time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC) }

	s, err := store.Open(t.Context(), first)
	if err != nil {
		t.Fatalf("opening a bucket-index store: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before, err := os.ReadFile(layoutObject(dir, 1))
	if err != nil {
		t.Fatal(err)
	}

	// A day later, and a different build. Neither may leave a mark: the object
	// exists, so the conditional create is a no-op rather than an overwrite.
	second := blobOptions(dir)
	second.Now = func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) }
	second.Version = "1.0.0"

	again, err := store.Open(t.Context(), second)
	if err != nil {
		t.Fatalf("reopening a bucket-index store: %v", err)
	}

	if err := again.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := os.ReadFile(layoutObject(dir, 1))
	if err != nil {
		t.Fatal(err)
	}

	if string(after) != string(before) {
		t.Errorf("reopening rewrote the layout object:\n before %s\n after  %s", before, after)
	}
}

// TestANewerIndexLayoutIsRefused is AC14. A binary that does not understand the
// layout must not write to it, for the same reason and with the same message
// shape TestNewerSchemaIsRefused holds a SQL store to.
func TestANewerIndexLayoutIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Laid out by this build first, so what is refused below is an index this
	// wsaw had been using and a newer one has since moved on.
	s, err := store.Open(t.Context(), blobOptions(dir))
	if err != nil {
		t.Fatalf("opening a bucket-index store: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	writeLayoutObject(t, dir, 2, `{"layout":2,"createdAt":"2027-01-01T00:00:00Z","writtenBy":"wsaw 2.0.0"}`)

	_, err = store.Open(t.Context(), blobOptions(dir))
	if err == nil {
		t.Fatal("an index written by a newer layout was opened anyway")
	}

	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("the refusal does not explain the problem: %v", err)
	}
}

// TestAnIndexThisBuildDidNotStartIsNotReadAsEmpty is the case a probe that
// stopped at the first absent version would get wrong, and it is the one that
// matters: a bucket first written by a newer wsaw holds that layout's marker
// and no earlier one, so giving up at the gap would report an empty bucket,
// write layout 1 beside a layout 2 index, and read it as this build's own.
//
// Two versions ahead as well as one, because a probe bounded at "one past what
// this build understands" gets the first right and the second wrong — and a
// deployment that skips a release is exactly how the second happens.
func TestAnIndexThisBuildDidNotStartIsNotReadAsEmpty(t *testing.T) {
	t.Parallel()

	for _, layout := range []int{2, 3} {
		t.Run(fmt.Sprintf("layout %d", layout), func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			// Only the newer marker: no 00000001.json, exactly as a bucket laid
			// out by a future wsaw would look.
			writeLayoutObject(t, dir, layout, fmt.Sprintf(
				`{"layout":%d,"createdAt":"2027-01-01T00:00:00Z","writtenBy":"wsaw 2.0.0"}`, layout,
			))

			_, err := store.Open(t.Context(), blobOptions(dir))
			if err == nil {
				t.Fatal("a bucket whose only layout marker is a newer one was opened as if it were empty")
			}

			if !strings.Contains(err.Error(), "newer") {
				t.Errorf("the refusal does not explain the problem: %v", err)
			}

			if _, err := os.Stat(layoutObject(dir, 1)); err == nil {
				t.Error("the refused open wrote its own layout marker beside the newer one")
			}
		})
	}
}

// TestALayoutObjectThatCannotBeReadIsNotTreatedAsAbsent is Tenet 5 at the one
// object that says what the rest of the index is. "I could not read it" and
// "it is not there" are different facts, and only the second may lead to
// writing: treating a damaged marker as absent would lay a fresh layout over an
// index whose shape is unknown.
func TestALayoutObjectThatCannotBeReadIsNotTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"a body that does not decode":  "not json at all",
		"a body claiming another year": `{"layout":7,"createdAt":"2026-09-08T10:45:12Z","writtenBy":"wsaw 0.9.2"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeLayoutObject(t, dir, 1, body)

			_, err := store.Open(t.Context(), blobOptions(dir))
			if err == nil {
				t.Fatal("a store with an unreadable layout marker was opened")
			}

			if !errors.Is(err, store.ErrCorrupt) {
				t.Errorf("err = %v, want it to report corruption", err)
			}
		})
	}
}

// TestTheBucketIndexStoreRequiresALocation: there is nothing to derive one
// from. A SQLite store can put its evidence beside its database file; here the
// bucket is the store, and a guess would start a history somewhere the operator
// never named (Tenet 15).
func TestTheBucketIndexStoreRequiresALocation(t *testing.T) {
	t.Parallel()

	_, err := store.Open(t.Context(), store.Options{Driver: store.DriverBlob})
	if err == nil {
		t.Fatal("a bucket-index store with nowhere to keep its index was opened")
	}

	for _, want := range []string{"artifactURL", "artifactDir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say what to set: %v", err)
		}
	}
}

// TestTheDriverNameSelectsTheBucketIndexStore is AC1 at the seam: blob is one
// of the drivers configuration may name, and naming it is what selects a store
// with no database. Nothing else changes — SQLite is still what an unset driver
// gets, which the whole of the rest of this suite asserts by running.
func TestTheDriverNameSelectsTheBucketIndexStore(t *testing.T) {
	t.Parallel()

	listed := false

	for _, name := range store.Drivers() {
		if name == store.DriverBlob {
			listed = true
		}
	}

	if !listed {
		t.Errorf("Drivers() = %v, which does not offer the bucket-index store", store.Drivers())
	}

	if got := openBlob(t, t.TempDir()).Driver(); got != store.DriverBlob {
		t.Errorf("driver = %q, want %q", got, store.DriverBlob)
	}
}

// TestTheBucketIndexStoreAnswersTheStartupChecks: the two questions startup
// asks of any store have to have an answer here too, because this is the store
// where an unusable bucket means there is nothing left at all (Story 8.6, AC4
// and AC5).
func TestTheBucketIndexStoreAnswersTheStartupChecks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	if err := s.Ping(t.Context()); err != nil {
		t.Errorf("a freshly opened store does not answer a ping: %v", err)
	}

	if err := s.ProbeArtifactBucket(t.Context()); err != nil {
		t.Errorf("the bucket probe fails against a writable bucket: %v", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	err := s.Ping(t.Context())
	if err == nil {
		t.Fatal("a store whose bucket has gone answers a ping as healthy")
	}

	if !errors.Is(err, store.ErrBucketUnreachable) {
		t.Errorf("err = %v, want it to name the bucket as the unreachable half", err)
	}
}

// TestAnArtifactRoundTripsWithoutAnIndex is the half of this store that needs
// no index at all: evidence is content-addressed in the same bucket under the
// same keys whichever store wrote it (Story 8.1, AC2), so these four answer
// before a single index object exists.
func TestAnArtifactRoundTripsWithoutAnIndex(t *testing.T) {
	t.Parallel()

	s := openBlob(t, t.TempDir())
	data := []byte("a screenshot of a consent banner")

	ref, err := s.PutArtifact("screenshot-before-consent", data)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	got, err := s.GetArtifact(ref)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}

	if string(got) != string(data) {
		t.Errorf("the artifact came back as %q", got)
	}

	info, err := s.StatArtifact(t.Context(), ref)
	if err != nil {
		t.Fatalf("StatArtifact: %v", err)
	}

	if info.Size != int64(len(data)) {
		t.Errorf("size = %d, want %d", info.Size, len(data))
	}

	reader, err := s.OpenArtifact(t.Context(), ref)
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}

	defer func() { _ = reader.Close() }()

	if reader.Size != int64(len(data)) {
		t.Errorf("the opened artifact reports %d bytes, want %d", reader.Size, len(data))
	}
}

// TestAMissingArtifactIsReportedAsAbsent: a reference this store never wrote,
// and one it could have written but did not, both read as not found rather than
// as a broken bucket — which is what the HTTP layer turns into a 404 for
// evidence that has been pruned (Story 5.17, AC3).
func TestAMissingArtifactIsReportedAsAbsent(t *testing.T) {
	t.Parallel()

	s := openBlob(t, t.TempDir())

	cases := map[string]string{
		"a reference to nothing":         "screenshot-before-consent/" + strings.Repeat("a", 64),
		"a reference of the wrong shape": "../../etc/passwd",
	}

	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := s.GetArtifact(ref); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

// --- results in the bucket index (Story 8.10, §6.2 to §6.4) ---------------

// The literal spellings this store promises. They are written out rather than
// derived from the package, because a key is a promise to every future wsaw
// that opens the same bucket: a test that computed them would agree with a
// change that silently orphaned an existing index (§4.5).
//
// The instant is the one the design works through: 2026-09-08T10:45:12.123456789Z
// inverts to 7434507724731319018 and reads as 20260908T104512Z.
const (
	siteSeries   = "fbae041b02c41ed0fd8a4efb039bc780-site"
	siteInverted = "7434507724731319018"
	siteStamp    = "20260908T104512Z"
)

func siteStart() time.Time { return time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC) }

// bucketContents reads every object in a bucket directory, keyed by the key it
// is stored under.
//
// It walks the directory rather than asking the store, because what these tests
// are about is exactly what the store put in the bucket and nothing else — a
// store reporting on its own writes could not fail them.
func bucketContents(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = string(body)

		return nil
	})
	if err != nil {
		t.Fatalf("reading the bucket: %v", err)
	}

	return out
}

// bucketKeys is bucketContents without the bodies, sorted.
func bucketKeys(t *testing.T, dir string) []string {
	t.Helper()

	keys := make([]string, 0, 16)
	for key := range bucketContents(t, dir) {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

// digestOf is the content address the bucket gives some bytes.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// resultWithEvidence is one scan that names a screenshot and a stored body, so
// that the pins a stored result writes are more than the document's own.
func resultWithEvidence(t *testing.T, s store.Store, id string, at time.Time) (*model.Result, string, string) {
	t.Helper()

	screenshot := []byte("a screenshot of " + id)
	body := []byte("a response body for " + id)

	screenshotRef, err := s.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	bodyRef, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatalf("storing a body: %v", err)
	}

	res := result(id, at, model.ConsentReject)
	res.Screenshots = []model.Artifact{{
		Kind: "screenshot-before-consent", Ref: screenshotRef, Bytes: int64(len(screenshot)),
	}}
	res.Requests[0].BodyRef = bodyRef

	return res, screenshotRef, bodyRef
}

// TestStoringAResultWritesOneKeyPerFactAndNothingElse is AC3 and AC4 written out
// as the objects they produce.
//
// Every key here is derived from the scan it records and none of them is a key
// anything rewrites: the document by its content, the pins by artifact and
// owner, the scan-ID object by the scan, the entry by when the scan started, and
// the series marker by the target and mode. Spelling the whole set out is what
// catches a key quietly gaining a field, losing one, or moving to another
// prefix — none of which any behavioural test would notice, and all of which
// would orphan every index already in a bucket.
func TestStoringAResultWritesOneKeyPerFactAndNothingElse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// The clock is pinned so that the take markers, which are named for the
	// instant wsaw stored the bytes rather than for anything in the scan, are
	// keys this test can spell out like the rest of them.
	s := blobAt(t, dir, func() time.Time { return siteStart() })

	res, screenshotRef, bodyRef := resultWithEvidence(t, s, "scan-1", siteStart())

	if err := s.PutResult(res); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	document, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	documentRef := "result/" + digestOf(document)
	owner := "r." + siteSeries + ".reject.scan-1"
	index := "_wsaw/index/v1/"

	want := []string{
		// The evidence, content-addressed at the bucket root.
		bodyRef,
		documentRef,
		screenshotRef,

		// The layout marker, written when the bucket was opened.
		"_wsaw/index/layout/00000001.json",

		// One object per scan, addressed by its identity.
		index + "byid/" + siteSeries + "/reject/scan-1",

		// One pin per artifact the result names, including the document.
		index + "ref/" + bodyRef + "/" + owner,
		index + "ref/" + documentRef + "/" + owner,
		index + "ref/" + screenshotRef + "/" + owner,

		// One take per artifact the scan captured, recording that wsaw held
		// those bytes before any result named them. The document has none: it
		// is written by the store rather than captured, and no other scan can
		// content-address to it (see Blob.recordTake).
		index + "ref/" + bodyRef + "/t." + siteInverted,
		index + "ref/" + screenshotRef + "/t." + siteInverted,

		// The scan's place in its target's history.
		index + "series/" + siteSeries + "/reject/r." + siteInverted + "." + siteStamp + ".scan-1.idle",

		// The series itself.
		index + "targets/" + siteSeries + ".reject",
	}

	slices.Sort(want)

	if got := bucketKeys(t, dir); !slices.Equal(got, want) {
		t.Errorf("the bucket holds\n  %s\nwant\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestStoringTheSameResultTwiceChangesNothingInTheBucket is AC10.
//
// A store operation that failed ambiguously — the write landed, the response did
// not arrive — is retried, and the retry must not produce a second version of
// the same fact. Here it cannot: every key is derived from what it records and
// every write is a conditional create, so the second call writes the same bytes
// to the same keys and the bucket is byte-for-byte what it was.
func TestStoringTheSameResultTwiceChangesNothingInTheBucket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	res, _, _ := resultWithEvidence(t, s, "scan-1", siteStart())

	if err := s.PutResult(res); err != nil {
		t.Fatalf("the first PutResult: %v", err)
	}

	before := bucketContents(t, dir)

	if err := s.PutResult(res); err != nil {
		t.Fatalf("storing the same result again: %v", err)
	}

	after := bucketContents(t, dir)

	if !maps.Equal(before, after) {
		t.Errorf("re-storing one result changed the bucket:\n before %v\n  after %v",
			slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}

	// And the history holds one scan, not two.
	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Errorf("the history holds %d results after storing one twice, want 1", len(got))
	}
}

// TestAScanIDIsNotGivenASecondHistory is the one place this store deliberately
// answers differently from the SQL stores, which upsert.
//
// A scan ID is a key here, and rewriting a key is the thing an append-only index
// does not do (AC3). Two documents under one scan ID would be two histories for
// one scan, and neither of them could be shown to a reviewer as what was
// observed — so the second is refused, loudly and naming both, and the first
// stays exactly as it was.
//
// Nothing in production can reach it: newScanID mints a fresh identifier for
// every scan, retries included, and a genuine retry of one scan writes an
// identical document to an identical content address.
func TestAScanIDIsNotGivenASecondHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	first := result("scan-1", siteStart(), model.ConsentReject)
	if err := s.PutResult(first); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	second := result("scan-1", siteStart().Add(time.Hour), model.ConsentReject)
	second.Error = "a different scan entirely"

	err := s.PutResult(second)
	if err == nil {
		t.Fatal("a second scan was recorded under an identifier that already names one")
	}

	for _, want := range []string{"scan-1", "result/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	// The refusal is not an absence, and the recorded scan is untouched.
	if errors.Is(err, store.ErrNotFound) {
		t.Errorf("the refusal reads as an empty history: %v", err)
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("the recorded scan became unreadable: %v", err)
	}

	if !got.StartedAt.Equal(first.StartedAt) || got.Error != "" {
		t.Errorf("the recorded scan was replaced: %+v", got)
	}

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != 1 {
		t.Errorf("the history holds %d results, want 1", len(summaries))
	}
}

// TestAScanIsReachableByIDBeforeItIsInAListing is why the object addressed by
// scan ID is written before the entry in the series listing.
//
// Every ordering of these writes can be interrupted, so the question is which
// half-written state a reader survives. This one — reachable by identity, not
// yet in the listing — is a history that is one scan short and heals. The
// reverse is a row in an interface that returns nothing when it is clicked,
// which is the one visibly wrong state, and no ordering that produces it is
// worth the symmetry.
//
// It is also AC6 from the reading side: a scan the listing does not show is not
// reported as one that was deleted, and nothing about it is an error.
func TestAScanIsReachableByIDBeforeItIsInAListing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	older := result("scan-older", siteStart(), model.ConsentReject)
	if err := s.PutResult(older); err != nil {
		t.Fatal(err)
	}

	newer := result("scan-newer", siteStart().Add(time.Minute), model.ConsentReject)
	if err := s.PutResult(newer); err != nil {
		t.Fatal(err)
	}

	// The state PutResult leaves when it is interrupted after the scan-ID
	// object and before the entry, produced here by removing the entry.
	entry := filepath.Join(dir, "_wsaw", "index", "v1", "series", siteSeries, "reject")

	entries, err := os.ReadDir(entry)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.Contains(e.Name(), "scan-newer") {
			if err := os.Remove(filepath.Join(entry, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-newer")
	if err != nil {
		t.Fatalf("a scan that is not in the listing is not reachable by its identity: %v", err)
	}

	if got.ScanID != "scan-newer" {
		t.Errorf("GetResult returned %s", got.ScanID)
	}

	if ok, err := s.HasResult("site", model.ConsentReject, "scan-newer"); err != nil || !ok {
		t.Errorf("HasResult = %v, %v, want true", ok, err)
	}

	// The listing is short of it and says nothing is wrong, because nothing is:
	// an entry that is not visible is a result that is not there yet.
	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("a missing entry made the whole listing fail: %v", err)
	}

	if len(summaries) != 1 || summaries[0].ScanID != "scan-older" {
		t.Errorf("the listing shows %+v, want only scan-older", summaries)
	}

	// And the newest scan the store can see is the one it can see, rather than
	// an error about the one it cannot.
	latest, err := s.LatestResult("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("LatestResult: %v", err)
	}

	if latest.ScanID != "scan-older" {
		t.Errorf("LatestResult = %s, want scan-older", latest.ScanID)
	}
}

// TestReadingOneVisibleSetTwiceGivesOneAnswer is AC7 as far as a bucket that
// does not lag can show it.
//
// Nothing here writes between the two reads, so the visible key set is the same
// one and every read path has to produce the same answer from it — the same
// listing, the same latest, the same previous. It is the floor under the
// eventual-consistency tests that a lagging bucket makes possible; a store that
// could not manage determinism over an unchanging bucket would have no chance
// over a changing one.
func TestReadingOneVisibleSetTwiceGivesOneAnswer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	base := siteStart()

	for i := range 5 {
		res := result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)
		if i == 3 {
			res.Termination = model.TermError
			res.Error = "the browser crashed"
		}

		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	read := func() string {
		t.Helper()

		summaries, err := s.ListResults("site", model.ConsentReject, 0)
		if err != nil {
			t.Fatal(err)
		}

		latest, err := s.LatestResult("site", model.ConsentReject)
		if err != nil {
			t.Fatal(err)
		}

		previous, err := s.PreviousResult("site", model.ConsentReject, "scan-4")
		if err != nil {
			t.Fatal(err)
		}

		series, err := s.Series()
		if err != nil {
			t.Fatal(err)
		}

		encoded, err := json.Marshal(map[string]any{
			"listing": summaries, "latest": latest.ScanID,
			"previous": previous.ScanID, "series": series,
		})
		if err != nil {
			t.Fatal(err)
		}

		return string(encoded)
	}

	if first, second := read(), read(); first != second {
		t.Errorf("two reads of one unchanged bucket disagreed:\n %s\n %s", first, second)
	}
}

// --- baselines and the audit log in the bucket index (Story 8.10, §6.5) ----

// blobAt opens a bucket-index store whose clock is pinned, so that a test can
// place two decisions on one instant on purpose and see what the store does
// about it. Nothing here waits for a real nanosecond to pass (AGENTS §5).
func blobAt(t *testing.T, dir string, now func() time.Time) store.Store {
	t.Helper()

	opts := blobOptions(dir)
	opts.Now = now

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("opening a bucket-index store: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s
}

// keysUnder is every key in the bucket below one prefix, sorted.
func keysUnder(t *testing.T, dir, prefix string) []string {
	t.Helper()

	var found []string

	for _, key := range bucketKeys(t, dir) {
		if strings.HasPrefix(key, prefix) {
			found = append(found, key)
		}
	}

	return found
}

// The prefixes these tests read the bucket through, spelled out for the reason
// §4.5 gives: a key is a promise to every future wsaw that opens the same
// bucket, and a test that derived them would agree with a change that orphaned
// an index already written.
const (
	baselinePrefix  = "_wsaw/index/v1/baseline/"
	auditPrefix     = "_wsaw/index/v1/audit/"
	refPrefix       = "_wsaw/index/v1/ref/"
	siteDecisionDir = baselinePrefix + siteSeries + "/reject/"
)

// TestApprovingABaselineWritesOneObjectThatCarriesBoth is AC9 as the objects it
// produces.
//
// The approval and its audit entry are one body under one key, so there is no
// interruption that records the approval without the entry that explains it.
// What the audit prefix holds is a second copy, filed where the log is read
// from, at a key derived from the decision's own — which is what lets audit
// compaction delete it later without ever putting a baseline at risk.
func TestApprovingABaselineWritesOneObjectThatCarriesBoth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, siteStart)

	res, screenshotRef, bodyRef := resultWithEvidence(t, s, "scan-1", siteStart())
	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	before := keysUnder(t, dir, refPrefix)

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "post-CMP-upgrade state"); err != nil {
		t.Fatalf("SetBaseline: %v", err)
	}

	decisions := keysUnder(t, dir, baselinePrefix)
	if len(decisions) != 1 {
		t.Fatalf("one approval wrote %d decision objects: %v", len(decisions), decisions)
	}

	// The key spells out the series, the inverted instant, the readable stamp,
	// the operation, and the digest of the body it holds.
	wantPrefix := siteDecisionDir + siteInverted + "." + siteStamp + ".approve."
	if !strings.HasPrefix(decisions[0], wantPrefix) {
		t.Fatalf("the decision key is %q, want it to begin %q", decisions[0], wantPrefix)
	}

	contents := bucketContents(t, dir)
	did := strings.TrimPrefix(decisions[0], wantPrefix)

	if got := digestOf([]byte(contents[decisions[0]]))[:len(did)]; got != did {
		t.Errorf("the decision key names digest %s and its body hashes to %s", did, got)
	}

	// One body, both facts, and the approved scan by value.
	var decision struct {
		Op       string           `json:"op"`
		Audit    store.AuditEntry `json:"audit"`
		Baseline *store.Baseline  `json:"baseline"`
		Refs     []string         `json:"refs"`
	}

	if err := json.Unmarshal([]byte(contents[decisions[0]]), &decision); err != nil {
		t.Fatalf("the decision object does not decode: %v", err)
	}

	switch {
	case decision.Op != "approve":
		t.Errorf("the decision records op %q", decision.Op)
	case decision.Baseline == nil || decision.Baseline.Result == nil:
		t.Error("the decision does not carry the scan it approved by value")
	case decision.Audit.Action != "baseline-approved" || decision.Audit.Subject != "scan-1":
		t.Errorf("the decision does not carry its own audit entry: %+v", decision.Audit)
	case !decision.Audit.At.Equal(decision.Baseline.ApprovedAt):
		t.Errorf("the approval says %s and its audit entry says %s",
			decision.Baseline.ApprovedAt, decision.Audit.At)
	}

	// The audit prefix holds the same entry, at the decision's own ordering
	// fields and its own digest.
	wantAudit := auditPrefix + siteInverted + "." + siteStamp + "." + did
	if got := keysUnder(t, dir, auditPrefix); !slices.Equal(got, []string{wantAudit}) {
		t.Errorf("the audit log holds %v, want [%s]", got, wantAudit)
	}

	// And the evidence the copy names is pinned, by the decision, so that
	// retention cannot delete it while the approval stands.
	pins := keysUnder(t, dir, refPrefix)
	for _, ref := range []string{screenshotRef, bodyRef} {
		want := refPrefix + ref + "/d." + did
		if !slices.Contains(pins, want) {
			t.Errorf("the approved scan's evidence %s is not pinned by the decision: %v", ref, pins)
		}
	}

	if len(pins) != len(before)+len(decision.Refs) {
		t.Errorf("an approval of a scan naming %d artifacts added %d pins",
			len(decision.Refs), len(pins)-len(before))
	}
}

// TestARevocationAndAReApprovalAreAppends is AC8: the current baseline is a
// fold over decisions nothing edits and nothing deletes.
//
// The clock is pinned, so all three decisions ask to be recorded at the same
// nanosecond. Without the monotonic bump they would be indistinguishable in the
// order that decides which one is in force, and the fold would be choosing
// between "approved" and "withdrawn" by digest.
func TestARevocationAndAReApprovalAreAppends(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, siteStart)

	for _, id := range []string{"scan-1", "scan-2"} {
		if err := s.PutResult(result(id, siteStart(), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "first"); err != nil {
		t.Fatalf("approving: %v", err)
	}

	if err := s.DeleteBaseline("site", model.ConsentReject, "eva"); err != nil {
		t.Fatalf("withdrawing: %v", err)
	}

	if _, err := s.GetBaseline("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a withdrawn baseline reads as %v, want ErrNotFound", err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-2", "eva", "second"); err != nil {
		t.Fatalf("re-approving: %v", err)
	}

	decisions := keysUnder(t, dir, baselinePrefix)
	if len(decisions) != 3 {
		t.Fatalf("three decisions left %d objects: %v", len(decisions), decisions)
	}

	// Newest first, because the ordering field is inverted, and strictly
	// ordered because each decision cleared the one before it by a nanosecond.
	ops := make([]string, 0, len(decisions))

	for _, key := range decisions {
		fields := strings.Split(strings.TrimPrefix(key, siteDecisionDir), ".")
		if len(fields) != 4 {
			t.Fatalf("the decision key %q does not split into four fields", key)
		}

		ops = append(ops, fields[2])
	}

	if want := []string{"approve", "revoke", "approve"}; !slices.Equal(ops, want) {
		t.Errorf("the decisions read %v newest first, want %v", ops, want)
	}

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("GetBaseline after the re-approval: %v", err)
	}

	if b.ScanID != "scan-2" || b.Note != "second" {
		t.Errorf("the baseline in force is %+v, want the re-approval of scan-2", b)
	}

	// All three decisions are in the log, winners and losers alike.
	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 3 {
		t.Errorf("the audit log holds %d of the three decisions: %+v", len(entries), entries)
	}
}

// TestADecisionThatCannotBeReadIsCorruptionAndNotAnEmptyAnswer is the rule that
// matters most in this file.
//
// Nothing in this store edits or deletes a decision object, so one that a
// listing showed and the bucket cannot produce as it was written means the
// bucket lost bytes or somebody changed them. Answering "there is no baseline"
// would silence exactly the findings nobody approved anything about, so the
// answer is corruption — and this is the one place in the store where a
// decision object may not be skipped, stepped over or read as absence.
//
// The body is damaged rather than removed because a local file bucket lists what
// it holds: an object deleted here leaves the listing too, and the *listed and
// gone* half of the same rule needs a bucket whose listings lag (§8.3 #16). The
// check that catches both is the same one — the key is the digest of the body.
func TestADecisionThatCannotBeReadIsCorruptionAndNotAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", ""); err != nil {
		t.Fatal(err)
	}

	decisions := keysUnder(t, dir, baselinePrefix)
	if len(decisions) != 1 {
		t.Fatalf("expected one decision, got %v", decisions)
	}

	path := filepath.Join(dir, filepath.FromSlash(decisions[0]))
	if err := os.WriteFile(path, []byte(`{"layout":1,"op":"approve"}`), 0o600); err != nil {
		t.Fatalf("damaging the decision object: %v", err)
	}

	_, err := s.GetBaseline("site", model.ConsentReject)

	switch {
	case errors.Is(err, store.ErrNotFound):
		t.Errorf("a decision object that cannot be read reads as no baseline at all: %v", err)
	case !errors.Is(err, store.ErrCorrupt):
		t.Errorf("GetBaseline over a damaged decision object = %v, want ErrCorrupt", err)
	}

	// The key is still listed, so the cheap question must not answer "no"
	// either: it is the same fold, and the two may never disagree.
	has, err := s.HasBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("HasBaseline: %v", err)
	}

	if !has {
		t.Error("HasBaseline said no while the decision that says yes is still listed")
	}
}

// TestAnAuditEntryIsPutBackForADecisionThatLostIt is the self-heal AC9 asks for
// where one write genuinely cannot carry everything.
//
// The decision object is authoritative and complete; the audit key is a pointer
// at it. A process that died between the two left a decision the log does not
// show, and the next decision of the same series repairs it from the listing it
// was going to make anyway.
func TestAnAuditEntryIsPutBackForADecisionThatLostIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", ""); err != nil {
		t.Fatal(err)
	}

	pointers := keysUnder(t, dir, auditPrefix)
	if len(pointers) != 1 {
		t.Fatalf("expected one audit entry, got %v", pointers)
	}

	// The state an interrupted write leaves: the decision, and no entry.
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(pointers[0]))); err != nil {
		t.Fatalf("removing the audit entry: %v", err)
	}

	if err := s.DeleteBaseline("site", model.ConsentReject, "eva"); err != nil {
		t.Fatalf("withdrawing: %v", err)
	}

	if got := keysUnder(t, dir, auditPrefix); len(got) != 2 {
		t.Fatalf("the withdrawal left %d audit entries, want the approval's back beside it: %v", len(got), got)
	}

	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	actions := make([]string, 0, len(entries))
	for _, e := range entries {
		actions = append(actions, e.Action)
	}

	if want := []string{"baseline-deleted", "baseline-approved"}; !slices.Equal(actions, want) {
		t.Errorf("the log reads %v, want %v", actions, want)
	}
}

// TestTwoIdenticalAuditEntriesInOneInstantStayTwoEntries is the nonce.
//
// The key of an audit entry is a digest of what it records, so two identical
// actions recorded at one instant would hash to one key and become one entry —
// and deleting an audit record because it resembled another one is not
// acceptable. Under a pinned clock this is not a rare race but the ordinary
// case.
func TestTwoIdenticalAuditEntriesInOneInstantStayTwoEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, siteStart)

	e := store.AuditEntry{At: siteStart(), Actor: "eva", Action: "allow-list-added", Subject: "tracker.test"}

	for range 2 {
		if err := s.RecordAudit(e); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}

	if got := keysUnder(t, dir, auditPrefix); len(got) != 2 {
		t.Fatalf("two identical entries left %d objects: %v", len(got), got)
	}

	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Errorf("the log reports %d of the two entries: %+v", len(entries), entries)
	}
}

// TestARecordedAuditEntryKeepsTheInstantItWasGiven states the one place where
// the key and the body deliberately disagree.
//
// The key's ordering field is where the store filed the entry — stepped past
// any nanosecond already taken, so that two entries recorded at one instant come
// back in the order they were recorded. The body keeps the instant the caller
// gave, because the log records when the action happened and not where the
// store put it.
func TestARecordedAuditEntryKeepsTheInstantItWasGiven(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, siteStart)

	for _, subject := range []string{"first", "second"} {
		if err := s.RecordAudit(store.AuditEntry{
			At: siteStart(), Actor: "eva", Action: "allow-list-added", Subject: subject,
		}); err != nil {
			t.Fatalf("RecordAudit(%s): %v", subject, err)
		}
	}

	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}

	if entries[0].Subject != "second" || entries[1].Subject != "first" {
		t.Errorf("the log reads %q then %q, want the later entry first",
			entries[0].Subject, entries[1].Subject)
	}

	for _, e := range entries {
		if !e.At.Equal(siteStart()) {
			t.Errorf("the entry for %q reports %s, want the instant it was given, %s",
				e.Subject, e.At, siteStart())
		}
	}

	// The keys, however, are one nanosecond apart: that is the position, and it
	// is the only thing a listing can order by.
	keys := keysUnder(t, dir, auditPrefix)
	if len(keys) != 2 {
		t.Fatalf("got %d audit objects, want 2: %v", len(keys), keys)
	}

	if strings.Contains(keys[0], siteInverted) == strings.Contains(keys[1], siteInverted) {
		t.Errorf("both entries were filed at one instant: %v", keys)
	}
}

// --- retention against the bucket index (Story 8.10, §7.3 and §7.4) --------

// writeIndexObject puts a key into the index tree by hand.
//
// It writes the file directly rather than going through the store, because what
// its callers are building is a state the store produces only by being
// interrupted — a rebuild that is halfway through, a key whose delete the bucket
// refused — and there is no interface for asking a store to be interrupted.
func writeIndexObject(t *testing.T, dir, key string, body []byte) {
	t.Helper()

	path := filepath.Join(dir, filepath.FromSlash(key))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the index directory for %s: %v", key, err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the index object %s: %v", key, err)
	}
}

// pinned is a clock that does not move, so that every key a test's writes
// produce is a key the test can spell out (AGENTS §5).
func pinned(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// oneEntryKey is the single loose entry of the site series, which the two tests
// below damage in two different ways.
func oneEntryKey(t *testing.T, dir string) string {
	t.Helper()

	entries := keysUnder(t, dir, "_wsaw/index/v1/series/"+siteSeries+"/reject/r.")
	if len(entries) != 1 {
		t.Fatalf("the series holds %d entries, want exactly one: %v", len(entries), entries)
	}

	return entries[0]
}

// TestAnEntryThatWillNotDecodeIsShownFromItsKeyAndNotDropped is Tenet 5 at the
// one object a listing cannot do without.
//
// An entry object that is present and unreadable is a fact about one scan, and
// the answer to it may be neither "the read fails" nor "that scan is not in the
// history". The key still says when the scan started, what it was called and
// how it terminated — that is why those are in the key — so the row is shown
// from the key, carrying the error where its summary would have been, exactly
// as a SQL store reports a row whose document has gone.
//
// It is the entry object and not a decision object, and the two take different
// paths on purpose: a decision that will not read is ErrCorrupt, because
// inferring "there is no baseline" from it would silence findings nobody
// approved, while an entry that will not read is one row among many that must
// not hide the rest.
func TestAnEntryThatWillNotDecodeIsShownFromItsKeyAndNotDropped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	writeIndexObject(t, dir, oneEntryKey(t, dir), []byte("not an index entry at all"))

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("a listing over one damaged entry failed altogether: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("the listing returned %d summaries, want the scan its key still names: %+v",
			len(summaries), summaries)
	}

	got := summaries[0]

	if got.ScanID != "scan-1" || got.Target != "site" || got.ConsentMode != model.ConsentReject {
		t.Errorf("the damaged entry came back as %+v, want the scan its key spells", got)
	}

	if !got.StartedAt.Equal(siteStart()) || got.Termination != model.TermIdle {
		t.Errorf("the damaged entry lost what its key still carries: %+v", got)
	}

	if got.Error == "" {
		t.Errorf("the damaged entry is reported as an ordinary scan: %+v", got)
	}
}

// TestAScanStoredOutOfOrderHidesNothingAndOverwritesNothing is AC5's other
// half, asserted rather than argued from the key grammar.
//
// A host whose clock steps backwards stores a scan at an instant earlier than
// one already in the history. Ordering here is a total function of the key —
// the inverted start time, then the scan ID — so the newcomer takes the
// position its own timestamp gives it and cannot land on top of anything: two
// distinct scans are two distinct keys whatever the clocks did. What the test
// adds to that argument is the part a reader cares about, which is that both
// are still listed, both still resolve by ID, and the newest is still the
// newest.
func TestAScanStoredOutOfOrderHidesNothingAndOverwritesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	if err := s.PutResult(result("scan-late", siteStart(), model.ConsentReject)); err != nil {
		t.Fatalf("storing the first scan: %v", err)
	}

	// An hour before the scan that is already there, which is a clock that has
	// stepped back rather than a scan that arrived out of order on purpose.
	if err := s.PutResult(result("scan-early", siteStart().Add(-time.Hour), model.ConsentReject)); err != nil {
		t.Fatalf("storing a scan dated before the one already in the history: %v", err)
	}

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"scan-late", "scan-early"}
	if got := listedScans(summaries); !slices.Equal(got, want) {
		t.Errorf("the history lists %v, want %v: newest first, and neither hidden", got, want)
	}

	for _, id := range want {
		if _, err := s.GetResult("site", model.ConsentReject, id); err != nil {
			t.Errorf("scan %s is not reachable by ID: %v", id, err)
		}
	}

	latest, err := s.LatestResult("site", model.ConsentReject)
	if err != nil || latest.ScanID != "scan-late" {
		t.Errorf("the newest scan reads as %q, %v, want scan-late", scanOf(latest), err)
	}

	previous, err := s.PreviousResult("site", model.ConsentReject, "scan-late")
	if err != nil || previous.ScanID != "scan-early" {
		t.Errorf("the scan before scan-late reads as %q, %v, want scan-early", scanOf(previous), err)
	}
}

// TestRetentionKeepsAResultWhoseEntryNamesNoDocument is the bucket-index answer
// to the SQL suite's TestRetentionKeepsWhatAResultWithUnknownReferencesMightName,
// which is one of the tests a store with no rows has to skip.
//
// The state is not the same — there is no reference table to be behind — but the
// judgement is: retention may not remove a result whose references it cannot
// account for, because deleting it would mean inferring "this named nothing"
// from a gap in wsaw's own index. Here the gap is an entry that does not say
// where its document is, and the answer is to leave the result in the history
// and say so in the count the command prints.
func TestRetentionKeepsAResultWhoseEntryNamesNoDocument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	res, screenshot, body := resultWithEvidence(t, s, "scan-1", siteStart())

	if err := s.PutResult(res); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	key := oneEntryKey(t, dir)

	// The entry as it was written, with the one field retention needs blanked:
	// everything else has to stay, or the entry would be damaged rather than
	// incomplete and a different rule would answer.
	var entry map[string]any

	if err := json.Unmarshal([]byte(bucketContents(t, dir)[key]), &entry); err != nil {
		t.Fatalf("reading the entry back: %v", err)
	}

	document, ok := entry["document"].(map[string]any)
	if !ok {
		t.Fatalf("the entry object holds no document section: %v", entry)
	}

	document["ref"] = ""

	rewritten, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}

	writeIndexObject(t, dir, key, rewritten)

	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("a result with unaccountable references failed the whole prune: %v", err)
	}

	if stats.UnknownReferences != 1 {
		t.Errorf("UnknownReferences = %d, want 1", stats.UnknownReferences)
	}

	if stats.ResultsDeleted != 0 || stats.ArtifactsDeleted != 0 {
		t.Errorf("the prune removed %d results and %d artifacts, want none of either",
			stats.ResultsDeleted, stats.ArtifactsDeleted)
	}

	// The history still shows it, and its evidence is still there.
	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != 1 || summaries[0].ScanID != "scan-1" {
		t.Errorf("the listing returned %+v, want the scan retention could not account for", summaries)
	}

	for _, ref := range []string{screenshot, body} {
		if _, err := s.StatArtifact(t.Context(), ref); err != nil {
			t.Errorf("the evidence of a result retention kept (%s) was collected anyway: %v", ref, err)
		}
	}
}

// TestPruningAResultAppendsATombstoneAndLeavesNothingElseBehind is AC12 written
// out as the objects it produces.
//
// A result leaves this history by an append and by deletions, never by a live
// index object being rewritten: what is left when a whole series has expired is
// the tombstone that says a scan was there and was removed, and the layout
// object that says which index this is. Everything else — the entry, the object
// addressed by scan ID, the pins, the takes, the series marker and the three
// artifacts — is a key no reader can still need.
//
// The tombstone is what stays. It is written before anything is deleted and it
// is not collected here, because a compaction checkpoint written earlier may
// still carry the entry it suppresses, and a prune whose effect depended on the
// listing that would have shown that checkpoint being fresh is exactly what §7.3
// refuses to build.
func TestPruningAResultAppendsATombstoneAndLeavesNothingElseBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	res, screenshot, body := resultWithEvidence(t, s, "scan-1", siteStart())

	if err := s.PutResult(res); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	// A year on, so that the scan is past the age limit and every take is long
	// past the grace that made it mean anything.
	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	if stats.ArtifactsDeleted != 3 {
		t.Errorf("ArtifactsDeleted = %d, want 3 (document, screenshot, body)", stats.ArtifactsDeleted)
	}

	if stats.IndexKeysFailed != 0 {
		t.Errorf("IndexKeysFailed = %d, want 0", stats.IndexKeysFailed)
	}

	want := []string{
		"_wsaw/index/layout/00000001.json",
		"_wsaw/index/v1/series/" + siteSeries + "/reject/d." + siteInverted + ".scan-1",
	}

	if got := bucketKeys(t, dir); !slices.Equal(got, want) {
		t.Errorf("after pruning the whole history the bucket holds\n  %s\nwant\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// And the evidence is gone from the bucket, not merely unreferenced.
	for _, ref := range []string{screenshot, body} {
		if _, err := s.StatArtifact(t.Context(), ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("artifact %s: %v, want it collected", ref, err)
		}
	}
}

// TestAWithdrawnBaselineStopsKeepingTheEvidenceItApproved is the other half of
// "retention deletes what it stops referencing".
//
// An approval pins the evidence of the scan it approved, which is what carries
// that evidence through the expiry of the history around it. A withdrawal pins
// nothing — so once the withdrawn approval's pins are old enough that this run
// can be sure the listing showing the withdrawal was not a stale one, the
// evidence is collected like any other. Without this every superseded and every
// revoked approval would keep its scan's screenshots for the life of the bucket.
//
// Both decisions stay exactly where they are. A compliance decision whose
// reversal leaves no trace is not an auditable decision, and nothing in this
// store deletes a decision object.
func TestAWithdrawnBaselineStopsKeepingTheEvidenceItApproved(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	res, screenshot, body := resultWithEvidence(t, s, "scan-1", siteStart())

	if err := s.PutResult(res); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "martin", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteBaseline("site", model.ConsentReject, "martin"); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 3 {
		t.Errorf("ArtifactsDeleted = %d, want 3: a withdrawn approval keeps nothing", stats.ArtifactsDeleted)
	}

	for _, ref := range []string{screenshot, body} {
		if _, err := s.StatArtifact(t.Context(), ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("artifact %s: %v, want it collected", ref, err)
		}
	}

	if decisions := keysUnder(t, dir, siteDecisionDir); len(decisions) != 2 {
		t.Errorf("the bucket holds %d decisions, want the approval and the withdrawal: %v",
			len(decisions), decisions)
	}
}

// TestABaselineThatStandsKeepsTheEvidenceThroughAPrune is the positive check
// §7.3 asks for, and the reason it is positive.
//
// The approval's own pins would keep the evidence, but a listing served from a
// stale view can come back without them, and inferring "nothing needs this" from
// a listing that came back short is how a prune deletes the evidence of an
// approved scan. So the decision in force is read and what it names is protected
// by name.
func TestABaselineThatStandsKeepsTheEvidenceThroughAPrune(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	res, screenshot, body := resultWithEvidence(t, s, "scan-1", siteStart())

	if err := s.PutResult(res); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "martin", ""); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 || stats.ArtifactsDeleted != 0 {
		t.Errorf("the prune removed %d results and %d artifacts, want 1 and 0",
			stats.ResultsDeleted, stats.ArtifactsDeleted)
	}

	for _, ref := range []string{screenshot, body} {
		if _, err := s.StatArtifact(t.Context(), ref); err != nil {
			t.Errorf("the approved scan's evidence (%s) is gone: %v", ref, err)
		}
	}

	if _, err := s.GetBaseline("site", model.ConsentReject); err != nil {
		t.Errorf("the baseline is unreadable after the prune: %v", err)
	}
}

// TestPruningOneSeriesKeepsWhatAnotherSeriesBaselineNames is the same
// protection across the boundary a prune walks: one series at a time.
//
// Artifacts are content-addressed, so one asset captured identically in two
// consent modes is one object with a pin from each. The prune of the second
// series meets the first series' decision pin, and it has no baseline of its
// own to weigh it against — so a rule that judged every decision pin against
// the series in front of it would read that pin as a superseded approval and
// collect the object, taking the standing baseline's evidence with it.
//
// A decision pin is only this run's to remove when the run has positively
// established that the decision is not the one in force *for its own series*.
// Everything else stays, which is the same fail-safe direction §7.3 argues the
// positive baseline check from.
func TestPruningOneSeriesKeepsWhatAnotherSeriesBaselineNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	// One asset both consent modes captured unchanged, which content addressing
	// makes one object. This is the everyday case, not a contrived one: a
	// cookie banner's own logo does not differ between accept and reject.
	shared := []byte("one screenshot both consent modes captured")

	ref, err := s.PutArtifact("screenshot-before-consent", shared)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	naming := func(id string, mode model.ConsentMode) *model.Result {
		res := result(id, siteStart(), mode)
		res.Screenshots = []model.Artifact{{
			Kind: "screenshot-before-consent", Ref: ref, Bytes: int64(len(shared)),
		}}

		return res
	}

	// The prune walks the series in order, so the approved one is reached first
	// and the unapproved one meets the pin it left behind.
	if err := s.PutResult(naming("scan-a", model.ConsentAccept)); err != nil {
		t.Fatalf("storing the approved scan: %v", err)
	}

	if _, err := s.SetBaseline("site", model.ConsentAccept, "scan-a", "martin", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(naming("scan-b", model.ConsentReject)); err != nil {
		t.Fatalf("storing the unapproved scan: %v", err)
	}

	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	// Only the unapproved scan's own document goes. The shared screenshot is
	// still named by the standing approval, and so is the approved document.
	if stats.ResultsDeleted != 2 || stats.ArtifactsDeleted != 1 {
		t.Errorf("the prune removed %d results and %d artifacts, want 2 and 1 (the unapproved document)",
			stats.ResultsDeleted, stats.ArtifactsDeleted)
	}

	if _, err := s.StatArtifact(t.Context(), ref); err != nil {
		t.Errorf("the screenshot the standing baseline names was collected: %v", err)
	}

	b, err := s.GetBaseline("site", model.ConsentAccept)
	if err != nil {
		t.Fatalf("the baseline of the other series is unreadable after the prune: %v", err)
	}

	for _, shot := range b.Result.Screenshots {
		if _, err := s.StatArtifact(t.Context(), shot.Ref); err != nil {
			t.Errorf("the approved copy names %s, which is gone: %v", shot.Ref, err)
		}
	}
}

// TestAPrunedResultThatStaysFetchableIsReportedAndThenCollected is
// PruneStats.IndexKeysFailed and the sweep pass that answers it.
//
// A prune deletes the object addressed by scan ID before it touches any
// artifact, so that a reader who loses the race gets a clean "not found" rather
// than the lost-evidence answer Story 5.17, AC3 exists to keep apart. When that
// delete is refused the result is out of every listing and still fetchable by
// its ID, which is a different thing from bytes that were not reclaimed and is
// counted as one — and the next sweep collects the key, because the tombstone
// beside it says a prune meant it to be gone.
func TestAPrunedResultThatStaysFetchableIsReportedAndThenCollected(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores the directory permissions this test denies with")
	}

	dir := t.TempDir()
	s := blobAt(t, dir, pinned(siteStart()))

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	// The one directory whose deletes are refused: everything else the prune
	// touches is elsewhere in the tree.
	byID := filepath.Join(dir, filepath.FromSlash("_wsaw/index/v1/byid/"+siteSeries+"/reject"))

	if err := os.Chmod(byID, 0o500); err != nil {
		t.Fatalf("making the scan-ID directory read-only: %v", err)
	}

	t.Cleanup(func() { _ = os.Chmod(byID, 0o700) })

	later := siteStart().Add(365 * 24 * time.Hour)

	stats, err := s.Prune(t.Context(), later, store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("a refused index delete failed the whole prune: %v", err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("ResultsDeleted = %d, want 1: retention still has to remove the result", stats.ResultsDeleted)
	}

	if stats.IndexKeysFailed != 1 {
		t.Errorf("IndexKeysFailed = %d, want 1", stats.IndexKeysFailed)
	}

	if stats.ArtifactsFailed != 0 {
		t.Errorf("ArtifactsFailed = %d: no artifact was left behind, only an index key", stats.ArtifactsFailed)
	}

	found, err := s.HasResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Error("the scan-ID object was reported as un-deletable and is gone anyway")
	}

	if err := os.Chmod(byID, 0o700); err != nil {
		t.Fatalf("making the scan-ID directory writable again: %v", err)
	}

	if _, err := s.Sweep(t.Context(), later, store.SweepOptions{AllowEmptyIndex: true}); err != nil {
		t.Fatal(err)
	}

	found, err = s.HasResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatal(err)
	}

	if found {
		t.Error("the sweep left the scan-ID object of a pruned result behind")
	}
}

// TestASweepLeavesAResultDocumentTheIndexHasLostSightOf is AC6's last clause and
// §7.4's third rule.
//
// A document visible without its index entry is what an interrupted write leaves
// and what a listing that has not caught up looks like. It decodes to the scan
// it records, so it is something Story 8.11 can restore rather than garbage —
// and "not silently lost" is unconditional, where a grace period would protect
// it for a day and then delete it. It is counted instead, so an operator can see
// that the bucket holds evidence the index does not.
func TestASweepLeavesAResultDocumentTheIndexHasLostSightOf(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	// A stored scan, so the index has something to judge the bucket with.
	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	orphan := writeArtifact(t, dir, "result", []byte(`{"scanId":"scan-2","target":"site"}`))

	// Long past any grace period, so nothing but the rule is keeping it.
	later := time.Now().Add(365 * 24 * time.Hour)

	stats, err := s.Sweep(t.Context(), later, store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep collected %d artifacts, want 0", stats.ArtifactsDeleted)
	}

	if stats.ResultsWithoutEntry != 1 {
		t.Errorf("ResultsWithoutEntry = %d, want 1", stats.ResultsWithoutEntry)
	}

	if _, err := s.StatArtifact(t.Context(), orphan); err != nil {
		t.Errorf("the sweep collected a document a rebuild could restore: %v", err)
	}

	// An operator who says the index really is gone gets the other answer, and
	// that is the only way to it.
	stats, err = s.Sweep(t.Context(), later, store.SweepOptions{AllowEmptyIndex: true})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d with the override, want the orphaned document", stats.ArtifactsDeleted)
	}
}

// TestASweepCollectsNothingWhileARebuildIsRunning is §7.4's fourth rule.
//
// A rebuild derives the pins from the documents, so while one is running "I have
// no pin for this artifact" and "I have not derived the pins yet" are the same
// observation. A sweep that read the first meaning would delete the evidence a
// rebuild interrupted at 40 % had not reached. Story 8.10 honours the marker;
// Story 8.11 writes it.
func TestASweepCollectsNothingWhileARebuildIsRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := openBlob(t, dir)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	// Genuinely unreferenced: exactly what a sweep exists to collect, and what
	// it must not collect now.
	orphan := writeArtifact(t, dir, "body", []byte("evidence a rebuild has not reached"))
	writeIndexObject(t, dir, "_wsaw/index/v1/rebuild/scan-2", nil)

	stats, err := s.Sweep(t.Context(), time.Now().Add(365*24*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep collected %d artifacts while a rebuild was running, want 0", stats.ArtifactsDeleted)
	}

	if stats.RebuildInProgress != 1 {
		t.Errorf("RebuildInProgress = %d, want the one rebuild marker", stats.RebuildInProgress)
	}

	// Its own number, and not the one that means a stored result's document is
	// missing or no longer decodes: `wsaw store sweep` prints that one as an
	// integrity problem, and a rebuild half way through is not one.
	if stats.UnknownReferences != 0 {
		t.Errorf("UnknownReferences = %d while a rebuild was running, want 0", stats.UnknownReferences)
	}

	if _, err := s.StatArtifact(t.Context(), orphan); err != nil {
		t.Errorf("the sweep collected evidence a rebuild had not reached: %v", err)
	}
}
