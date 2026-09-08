package store_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// These are the tests of what retention must refuse to do (Story 8.5, AC1,
// AC3 and AC4).
//
// retention_test.go covers what a prune and a sweep remove. This file covers
// the cases where removing would be wrong and the reason is not visible from
// the bucket: a result whose references cannot be derived, a scan that has
// deduplicated onto an old object, a bucket that has gone away, an index that
// knows nothing, and objects wsaw never wrote. Every one of them ends in
// deleted evidence if it regresses, and evidence does not come back.

// documentRefOf reads where a stored result's document went, from the row
// itself.
//
// The tests below need it to reach behind the store and delete the object, or
// to assert that a document survived, and the row is the only thing that knows
// the key. It goes through the raw database because no exported method answers
// it — nothing in wsaw needs to.
func documentRefOf(t *testing.T, opts store.Options, scanID string) string {
	t.Helper()

	db := rawDB(t, opts)
	query := "select artifact_ref from results where scan_id = ?"

	if opts.Driver == store.DriverPostgres {
		query = "select artifact_ref from results where scan_id = $1"
	}

	var ref string

	if err := db.QueryRowContext(t.Context(), query, scanID).Scan(&ref); err != nil {
		t.Fatalf("reading the document reference of scan %s: %v", scanID, err)
	}

	return ref
}

// forgetWhatEveryResultReferences puts every stored row back into the state a
// row written by a pre-bucket wsaw is in: no reference rows, and nothing
// recorded about what it names.
//
// It is deliberately done behind the store's back, with no placeholders so
// that the two statements are the same on all three dialects. What it produces
// is the state the reference backfill exists to resolve — and, when the row's
// document has also been removed from the bucket, the state it cannot resolve.
func forgetWhatEveryResultReferences(t *testing.T, opts store.Options) {
	t.Helper()

	db := rawDB(t, opts)

	for _, statement := range []string{
		"delete from result_artifacts",
		"update results set refs_indexed = 0",
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// rewindSchemaVersion puts the recorded schema version back, wherever the
// dialect under test keeps it, so that a migration runs again on the next
// open.
func rewindSchemaVersion(t *testing.T, db *sql.DB, opts store.Options, version int) {
	t.Helper()

	if opts.Driver == "" || opts.Driver == store.DriverSQLite {
		if _, err := db.ExecContext(t.Context(), "pragma user_version = "+strconv.Itoa(version)); err != nil {
			t.Fatalf("rewinding the schema version: %v", err)
		}

		return
	}

	query := "update wsaw_schema_version set version = ? where id = 1"
	if opts.Driver == store.DriverPostgres {
		query = "update wsaw_schema_version set version = $1 where id = 1"
	}

	if _, err := db.ExecContext(t.Context(), query, version); err != nil {
		t.Fatalf("rewinding the schema version: %v", err)
	}
}

// TestRetentionKeepsWhatAResultWithUnknownReferencesMightName is the Tenet 5
// guard, which is the one thing standing between a sweep and the evidence of a
// result whose document can no longer be read.
//
// The store is put into the state that produces it: a result with a screenshot
// and a stored body, its document removed from the bucket behind wsaw's back —
// a lifecycle rule on the bucket does exactly that — and its row marked as one
// whose references have never been worked out. The backfill then cannot derive
// what it names, and from that moment no screenshot and no stored body may be
// declared garbage merely because no row says otherwise. A stray result
// document still may be: every row's own document reference is recorded in
// SQL, so the exemption for that kind is sound.
func TestRetentionKeepsWhatAResultWithUnknownReferencesMightName(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	shot, body := withEvidence(t, s, "scan-1", time.Now().Add(-time.Hour),
		[]byte("a screenshot nothing can account for"), []byte("a body nothing can account for"))

	document := documentRefOf(t, opts, "scan-1")

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The document goes the way a bucket lifecycle rule takes it: the object
	// is deleted and the row still names it.
	if err := os.Remove(filepath.Join(opts.ArtifactDir, document)); err != nil {
		t.Fatalf("removing the document behind the store's back: %v", err)
	}

	forgetWhatEveryResultReferences(t, opts)

	// An artifact nothing has ever referenced, of the one kind that stays
	// collectable. Written directly, so it carries no claim from a running
	// scan either.
	orphanDocument := writeArtifact(t, opts.ArtifactDir, "result", []byte(`{"scanId":"nobody"}`))

	// Reopened, which is what runs the backfill and marks the row.
	s, err = store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	// Long past every grace period, so nothing but this guard is keeping the
	// screenshot and the body alive.
	later := time.Now().Add(365 * 24 * time.Hour)

	planned, err := s.PlanSweep(t.Context(), later, store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if planned.UnknownReferences != 1 {
		t.Errorf("a plan reports UnknownReferences = %d, want 1", planned.UnknownReferences)
	}

	if planned.ArtifactsProtected < 2 {
		t.Errorf("a plan protects %d artifacts, want at least the screenshot and the body",
			planned.ArtifactsProtected)
	}

	for _, ref := range planned.Artifacts {
		if ref == shot || ref == body {
			t.Errorf("a plan would collect %s, which a result with unknown references might name", ref)
		}
	}

	stats, err := s.Sweep(t.Context(), later, store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.UnknownReferences != 1 {
		t.Errorf("UnknownReferences = %d, want 1: one row's references cannot be derived",
			stats.UnknownReferences)
	}

	if stats.ArtifactsProtected < 2 {
		t.Errorf("ArtifactsProtected = %d, want at least the screenshot and the body",
			stats.ArtifactsProtected)
	}

	assertStored(t, s, shot, "the screenshot of a result whose references are unknown")
	assertStored(t, s, body, "the stored body of a result whose references are unknown")

	// The one kind the guard exempts, and the reason it can: the document
	// reference of every row is recorded, so a result document nothing names
	// really is unreferenced.
	assertGone(t, s, orphanDocument, "a result document no row names")

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d, want 1 (the stray document alone)", stats.ArtifactsDeleted)
	}

	// A prune sees the same guard and reports the same reason, so the dry run
	// an operator reads before changing retention says why nothing was
	// reclaimed.
	pruned, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxPerSeries: 1})
	if err != nil {
		t.Fatal(err)
	}

	if pruned.UnknownReferences != 1 {
		t.Errorf("a prune reports UnknownReferences = %d, want 1", pruned.UnknownReferences)
	}

	assertStored(t, s, shot, "the screenshot after a prune")
	assertStored(t, s, body, "the stored body after a prune")
}

// TestTheDocumentOfALiveResultSurvivesAnUnfinishedBackfill is the hole the
// exemption above would otherwise be.
//
// The reference backfill is best-effort by design: it reads a document per row
// and gives up rather than failing an Open. A row it has not reached has no
// reference row of its own, so a sweep asking "does any result name this
// document" would get no for the document of a perfectly live result — and
// delete the evidence. The schema records every row's own document reference
// in one statement, before any document is read, and this is that guarantee.
func TestTheDocumentOfALiveResultSurvivesAnUnfinishedBackfill(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(result("scan-1", time.Now().Add(-time.Hour), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	document := documentRefOf(t, opts, "scan-1")

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The state a store is in when the backfill has not run: the row is there,
	// its document is there, and nothing has recorded the connection.
	forgetWhatEveryResultReferences(t, opts)

	// Rewound so the migration that records every row's document reference
	// runs again on the next open, as it does for a store upgrading now.
	rewindSchemaVersion(t, rawDB(t, opts), opts, 4)

	s, err = store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	stats, err := s.Sweep(t.Context(), time.Now().Add(365*24*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep deleted %d artifacts of a live result, want 0", stats.ArtifactsDeleted)
	}

	assertStored(t, s, document, "the document of a live result")

	if _, err := s.GetResult("site", model.ConsentReject, "scan-1"); err != nil {
		t.Errorf("the live result is unreadable after a sweep: %v", err)
	}
}

// TestRetentionKeepsAnArtifactARunningScanHasJustTaken is AC3 for the case the
// bucket cannot see.
//
// Artifacts are content-addressed, so a scan that captures an unchanged asset
// stores nothing: the key is already there, and its write time is the first
// scan's. A prune that deleted the result which put it there would find the
// object unreferenced and old, and collect the evidence of the scan running
// now — which would go on to commit a row naming an object that is gone.
func TestRetentionKeepsAnArtifactARunningScanHasJustTaken(t *testing.T) {
	t.Parallel()

	s := open(t)
	shared := []byte("an asset that did not change")

	// Yesterday's scan, the only result naming that body.
	_, body := withEvidence(t, s, "scan-yesterday", time.Now().Add(-25*time.Hour),
		[]byte("yesterday's screenshot"), shared)

	// Tonight's scan reaches the same bytes. The bucket write is a no-op and
	// the row is minutes away; this is the whole window.
	again, err := s.PutArtifact("body", shared)
	if err != nil {
		t.Fatal(err)
	}

	if again != body {
		t.Fatalf("the same bytes stored as %s and %s; content addressing is broken", body, again)
	}

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 {
		t.Fatalf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	if stats.ArtifactsProtected < 1 {
		t.Errorf("ArtifactsProtected = %d, want the body a running scan had taken", stats.ArtifactsProtected)
	}

	assertStored(t, s, body, "the body a running scan had taken")

	// Tonight's scan lands naming those bytes, and the evidence it names is
	// readable — the property the whole mechanism exists for.
	tonight := result("scan-tonight", time.Now(), model.ConsentReject)
	tonight.Requests[0].BodyRef = body

	if err := s.PutResult(tonight); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetArtifact(body); err != nil {
		t.Errorf("the stored body of a scan that succeeded is gone: %v", err)
	}
}

// TestASweepKeepsAnArtifactARunningScanHasJustTaken is the same defect from
// the sweep's side, where the grace period is measured against the object's
// own age and that age is a lie.
//
// The object here is genuinely old and genuinely unreferenced — exactly the
// garbage a sweep exists to collect — and a scan running now has just
// deduplicated onto it. Only the claim tells the two apart.
func TestASweepKeepsAnArtifactARunningScanHasJustTaken(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	// A result, so the sweep has an index to judge the bucket with.
	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	shared := []byte("bytes an interrupted scan left behind")

	// Written directly and dated two days ago: an object from a scan that died
	// before committing its row, which no reference and no claim covers.
	orphan := writeArtifact(t, opts.ArtifactDir, "body", shared)
	twoDaysAgo := time.Now().Add(-48 * time.Hour)

	if err := os.Chtimes(filepath.Join(opts.ArtifactDir, orphan), twoDaysAgo, twoDaysAgo); err != nil {
		t.Fatalf("backdating the orphan: %v", err)
	}

	// Tonight's scan captures the same unchanged asset. Nothing is written,
	// and the object's age does not change.
	if _, err := s.PutArtifact("body", shared); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Sweep(t.Context(), time.Now(), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep collected %d artifacts a running scan had taken, want 0", stats.ArtifactsDeleted)
	}

	if stats.ArtifactsProtected < 1 {
		t.Errorf("ArtifactsProtected = %d, want the claimed object", stats.ArtifactsProtected)
	}

	assertStored(t, s, orphan, "the body of a scan that was running")

	// A day and a bit later the claim has aged out with the scan that took it,
	// and the same object is the garbage it looks like.
	stats, err = s.Sweep(t.Context(), time.Now().Add(25*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d once the claim aged out, want 1", stats.ArtifactsDeleted)
	}

	assertGone(t, s, orphan, "the orphan whose claim aged out")
}

// TestRetentionRefusesToRunWithoutTheArtifactBucket is AC4 read the other way
// round: a bucket that is not there must not be reported as a bucket that had
// nothing in it.
//
// Deleting is idempotent, so "this key is already gone" counts as collected,
// which is right for one object and disastrous for an unmounted volume where
// every key answers that way. A prune would report thousands of deletions,
// clear the reference rows that were its work list, and leak every object once
// the volume came back.
func TestRetentionRefusesToRunWithoutTheArtifactBucket(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	withEvidence(t, s, "scan-old", time.Now().Add(-30*24*time.Hour),
		[]byte("a screenshot"), []byte("a body"))

	// The unmounted volume, which is what this looks like from inside the
	// process.
	if err := os.RemoveAll(opts.ArtifactDir); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err == nil {
		t.Fatalf("a prune against a bucket that is gone reported %d results and %d artifacts deleted, want an error",
			stats.ResultsDeleted, stats.ArtifactsDeleted)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("ArtifactsDeleted = %d against a bucket that is gone, want 0", stats.ArtifactsDeleted)
	}

	// The history is still there: a prune that could not reclaim must not
	// remove the rows that say what there was to reclaim. The row is what is
	// asserted rather than the result, because reading a result now means
	// reading a document out of a bucket that is gone.
	rows, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Errorf("the store holds %d results after a prune that could not reach the bucket, want 1", len(rows))
	}

	if _, err := s.Sweep(t.Context(), time.Now(), store.SweepOptions{}); err == nil {
		t.Error("a sweep against a bucket that is gone reported success")
	}
}

// TestASweepRefusesAnIndexThatKnowsNothing is the other way an absent index
// becomes deleted evidence.
//
// A sweep decides by reference. A store whose index holds nothing references
// nothing, so every object in the bucket looks like garbage — which is what a
// database restored without its bucket, or a fresh store pointed at somebody
// else's, looks like. Rebuilding an index from the bucket is Story 8.11 and
// does not exist yet, so that deletion has no way back and is refused until an
// operator says the empty history is real.
func TestASweepRefusesAnIndexThatKnowsNothing(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	// The bucket of a wsaw that has been running for a year, and a database
	// that knows nothing about it.
	document := writeArtifact(t, opts.ArtifactDir, "result", []byte(`{"scanId":"scan-1"}`))
	shot := writeArtifact(t, opts.ArtifactDir, "screenshot-before-consent", []byte("a year of evidence"))

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	later := time.Now().Add(365 * 24 * time.Hour)

	if _, err := s.Sweep(t.Context(), later, store.SweepOptions{}); !errors.Is(err, store.ErrEmptyIndex) {
		t.Fatalf("a sweep against an empty index returned %v, want ErrEmptyIndex", err)
	}

	assertStored(t, s, document, "a result document a lost index cannot account for")
	assertStored(t, s, shot, "a screenshot a lost index cannot account for")

	// An operator who really does have a bucket of leftovers and no history
	// says so, and the sweep proceeds.
	stats, err := s.Sweep(t.Context(), later, store.SweepOptions{AllowEmptyIndex: true})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 2 {
		t.Errorf("ArtifactsDeleted = %d with the override, want 2", stats.ArtifactsDeleted)
	}

	assertGone(t, s, document, "the document the operator acknowledged")
	assertGone(t, s, shot, "the screenshot the operator acknowledged")
}

// TestASweepLeavesWhatWsawDidNotWrite is AC4's counter and AC3's boundary.
//
// A sweep walks the bucket, so anything else in the bucket is in its path.
// Deleting a key wsaw did not write would be a sweep reaching outside its own
// evidence; reporting it as a delete the bucket refused — which is what
// attempting it produces, since the bucket seam refuses a reference it did not
// write — would make the refusal counter permanently non-zero and hide a
// bucket that has really stopped accepting deletes.
func TestASweepLeavesWhatWsawDidNotWrite(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	// Two shapes of foreign object: one with no kind at all, and one under a
	// prefix that is not a kind wsaw writes but has the right shape otherwise.
	foreign := map[string][]byte{
		"a-leftover-staging-file": []byte("not ours"),
		"vendor-export/0000000000000000000000000000000000000000000000000000000000000000": []byte("also not ours"),
	}

	for key, data := range foreign {
		path := filepath.Join(opts.ArtifactDir, filepath.FromSlash(key))

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.Sweep(t.Context(), time.Now().Add(365*24*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ForeignObjects != len(foreign) {
		t.Errorf("ForeignObjects = %d, want %d", stats.ForeignObjects, len(foreign))
	}

	if stats.ArtifactsFailed != 0 {
		t.Errorf("ArtifactsFailed = %d: an object wsaw never wrote is not a delete the bucket refused",
			stats.ArtifactsFailed)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("ArtifactsDeleted = %d, want 0", stats.ArtifactsDeleted)
	}

	for key := range foreign {
		if _, err := os.Stat(filepath.Join(opts.ArtifactDir, filepath.FromSlash(key))); err != nil {
			t.Errorf("the sweep removed %s, which wsaw did not write: %v", key, err)
		}
	}
}
