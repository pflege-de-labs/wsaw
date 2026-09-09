package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// These are the tests of Story 8.5: what retention removes from the bucket, and
// — more importantly — what it must not remove.
//
// They run on whichever dialect the suite is configured for, through open(),
// because deciding what to delete is a query and a query is exactly where a
// dialect can differ (Story 4.7, AC5) — and against whichever bucket it is
// configured for, because deleting evidence is the one operation whose failure
// modes are the provider's rather than the query planner's (Story 8.9, AC1).

// withEvidence stores a result that names a screenshot and a stored body, and
// returns the references it names.
//
// The artifacts go in first, exactly as a scan writes them: the bucket is
// written before the row that references it (Story 8.2, AC4), and it is that
// order the sweep's grace period exists to survive.
func withEvidence(t *testing.T, s store.Store, id string, at time.Time, screenshot, body []byte) (screenshotRef, bodyRef string) {
	t.Helper()

	screenshotRef, err := s.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	bodyRef, err = s.PutArtifact("body", body)
	if err != nil {
		t.Fatalf("storing a body: %v", err)
	}

	res := result(id, at, model.ConsentReject)
	res.Screenshots = []model.Artifact{{
		Kind: "screenshot-before-consent", Ref: screenshotRef, Bytes: int64(len(screenshot)),
	}}
	res.Requests[0].BodyRef = bodyRef

	if err := s.PutResult(res); err != nil {
		t.Fatalf("storing result %s: %v", id, err)
	}

	return screenshotRef, bodyRef
}

// writeArtifact puts an object into the bucket the way the store's own bucket
// layer would: content-addressed, under its kind.
//
// It writes directly rather than through a store because its callers are
// planting what a store would not: an artifact for a store built in the layout
// that came before this one, where opening a store would apply the very upgrade
// the test is about to exercise, or an orphan no index names.
func writeArtifact(t *testing.T, bucket *store.TestBucket, kind string, data []byte) string {
	t.Helper()

	sum := sha256.Sum256(data)
	ref := kind + "/" + hex.EncodeToString(sum[:])

	bucket.Write(ref, data)

	return ref
}

func assertStored(t *testing.T, s store.Store, ref, what string) {
	t.Helper()

	if _, err := s.StatArtifact(t.Context(), ref); err != nil {
		t.Errorf("%s (%s) is gone: %v", what, ref, err)
	}
}

func assertGone(t *testing.T, s store.Store, ref, what string) {
	t.Helper()

	switch _, err := s.StatArtifact(t.Context(), ref); {
	case err == nil:
		t.Errorf("%s (%s) is still stored", what, ref)
	case !errors.Is(err, store.ErrNotFound):
		t.Errorf("checking %s (%s): %v", what, ref, err)
	}
}

// TestPruningDeletesTheArtifactsItStopsReferencing is AC1's first half: what a
// pruned result alone referenced goes with it.
func TestPruningDeletesTheArtifactsItStopsReferencing(t *testing.T) {
	t.Parallel()

	s := open(t)
	old := time.Now().Add(-30 * 24 * time.Hour)

	shot, body := withEvidence(t, s, "scan-old", old, []byte("a screenshot"), []byte("a body"))

	// Read back first, so the test knows the evidence was there to begin with
	// and is not asserting that a failed write was cleaned up.
	assertStored(t, s, shot, "the screenshot")
	assertStored(t, s, body, "the stored body")

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	// The document, the screenshot and the body: three artifacts, one result.
	if stats.ArtifactsDeleted != 3 {
		t.Errorf("ArtifactsDeleted = %d, want 3 (document, screenshot, body)", stats.ArtifactsDeleted)
	}

	if stats.BytesFreed <= 0 {
		t.Errorf("BytesFreed = %d, want the bytes of what was deleted", stats.BytesFreed)
	}

	assertGone(t, s, shot, "the screenshot")
	assertGone(t, s, body, "the stored body")

	if stats.ArtifactsFailed != 0 {
		t.Errorf("ArtifactsFailed = %d, want 0", stats.ArtifactsFailed)
	}
}

// TestASharedArtifactSurvivesThePruneOfOneResult is AC1's second half, and the
// reason deletion is decided by reference rather than by age: artifacts are
// content-addressed, so two scans that captured identical bytes are one object.
func TestASharedArtifactSurvivesThePruneOfOneResult(t *testing.T) {
	t.Parallel()

	s := open(t)

	same := []byte("the same screenshot both times")
	sameBody := []byte("the same body both times")

	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)

	oldShot, oldBody := withEvidence(t, s, "scan-old", old, same, sameBody)
	newShot, newBody := withEvidence(t, s, "scan-new", recent, same, sameBody)

	if oldShot != newShot || oldBody != newBody {
		t.Fatalf("identical bytes produced different references (%s/%s and %s/%s); "+
			"the sharing this test is about does not exist", oldShot, oldBody, newShot, newBody)
	}

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 {
		t.Fatalf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	// Only the old scan's own document is unreferenced now.
	if stats.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d, want 1: only the pruned result's document lost its last reference",
			stats.ArtifactsDeleted)
	}

	assertStored(t, s, oldShot, "the shared screenshot")
	assertStored(t, s, oldBody, "the shared body")

	// The surviving result must still read, evidence included: keeping the
	// bytes is only half of it if the row no longer points at them.
	res, err := s.GetResult("site", model.ConsentReject, "scan-new")
	if err != nil {
		t.Fatalf("the surviving result is unreadable after the prune: %v", err)
	}

	if len(res.Screenshots) != 1 || res.Screenshots[0].Ref != newShot {
		t.Errorf("the surviving result no longer names its screenshot")
	}
}

// TestPruningKeepsWhatABaselineNames guards the one deletion retention must
// never make. A baseline is never pruned because it defines "expected"; the
// evidence its own copy of the scan names has to survive with it.
func TestPruningKeepsWhatABaselineNames(t *testing.T) {
	t.Parallel()

	s := open(t)
	old := time.Now().Add(-30 * 24 * time.Hour)

	shot, body := withEvidence(t, s, "scan-old", old, []byte("approved screenshot"), []byte("approved body"))

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-old", "martin", ""); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 {
		t.Fatalf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("ArtifactsDeleted = %d, want 0: the baseline still names every one of them",
			stats.ArtifactsDeleted)
	}

	assertStored(t, s, shot, "the approved scan's screenshot")
	assertStored(t, s, body, "the approved scan's body")

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("baseline lost after pruning: %v", err)
	}

	if b.Result == nil || len(b.Result.Screenshots) != 1 {
		t.Error("the baseline's copy of the result no longer names its screenshot")
	}
}

// TestADryRunListsWhatWouldGoAndRemovesNothing is AC6. Deleting evidence is
// irreversible, so the plan has to be readable before it happens — and it has
// to describe the prune that would actually run.
func TestADryRunListsWhatWouldGoAndRemovesNothing(t *testing.T) {
	t.Parallel()

	s := open(t)
	old := time.Now().Add(-30 * 24 * time.Hour)

	shot, body := withEvidence(t, s, "scan-old", old, []byte("a screenshot"), []byte("a body"))

	retention := store.Retention{MaxAge: 24 * time.Hour}

	plan, err := s.PlanPrune(t.Context(), time.Now(), retention)
	if err != nil {
		t.Fatal(err)
	}

	if plan.ResultsDeleted != 1 || len(plan.Results) != 1 {
		t.Fatalf("plan reports %d results deleted and names %d, want 1 and 1",
			plan.ResultsDeleted, len(plan.Results))
	}

	if plan.Results[0].ScanID != "scan-old" {
		t.Errorf("plan names %q, want scan-old", plan.Results[0].ScanID)
	}

	if len(plan.Artifacts) != 3 || plan.ArtifactsDeleted != 3 {
		t.Errorf("plan names %d artifacts and counts %d, want 3 and 3 (document, screenshot, body)",
			len(plan.Artifacts), plan.ArtifactsDeleted)
	}

	if plan.BytesFreed <= 0 {
		t.Errorf("plan reports %d bytes, want the size of what it would delete", plan.BytesFreed)
	}

	// Nothing moved: the row, the evidence and the readability of the result.
	assertStored(t, s, shot, "the screenshot")
	assertStored(t, s, body, "the stored body")

	if _, err := s.GetResult("site", model.ConsentReject, "scan-old"); err != nil {
		t.Errorf("a dry run removed the result it was only describing: %v", err)
	}

	// And the prune it described does what it said.
	stats, err := s.Prune(t.Context(), time.Now(), retention)
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != plan.ResultsDeleted || stats.ArtifactsDeleted != plan.ArtifactsDeleted {
		t.Errorf("the prune removed %d results and %d artifacts, the plan said %d and %d",
			stats.ResultsDeleted, stats.ArtifactsDeleted, plan.ResultsDeleted, plan.ArtifactsDeleted)
	}
}

// TestSweepCollectsAnUnreferencedArtifactOnlyAfterTheGracePeriod is AC3.
//
// An artifact that nothing references is either garbage from an interrupted
// write or a scan in progress, and the only thing that tells them apart from
// outside the process is how long ago it was written.
func TestSweepCollectsAnUnreferencedArtifactOnlyAfterTheGracePeriod(t *testing.T) {
	t.Parallel()

	s := open(t)

	// Written and never referenced, which is exactly what a scan that was
	// interrupted between the bucket write and the row leaves behind.
	orphan, err := s.PutArtifact("screenshot-before-consent", []byte("nobody's screenshot"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()

	stats, err := s.Sweep(t.Context(), now, store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep collected %d artifacts written moments ago, want 0", stats.ArtifactsDeleted)
	}

	if stats.ArtifactsProtected != 1 {
		t.Errorf("ArtifactsProtected = %d, want 1", stats.ArtifactsProtected)
	}

	assertStored(t, s, orphan, "an artifact written moments ago")

	// A day and a bit later, the same object is garbage rather than a scan in
	// progress. The clock is a parameter rather than a sleep, so the grace
	// period is tested rather than waited out.
	later := now.Add(25 * time.Hour)

	stats, err = s.Sweep(t.Context(), later, store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d after the grace period, want 1", stats.ArtifactsDeleted)
	}

	if stats.BytesFreed != int64(len("nobody's screenshot")) {
		t.Errorf("BytesFreed = %d, want %d", stats.BytesFreed, len("nobody's screenshot"))
	}

	assertGone(t, s, orphan, "an unreferenced artifact past its grace period")
}

// TestASweepKeepsTheEvidenceOfAStoredResult is the other half of AC3: the
// sweep walks the whole bucket, so the thing it must be trusted not to do is
// collect evidence that is in use.
func TestASweepKeepsTheEvidenceOfAStoredResult(t *testing.T) {
	t.Parallel()

	s := open(t)

	shot, body := withEvidence(t, s, "scan-1", time.Now().Add(-time.Hour),
		[]byte("in use"), []byte("also in use"))

	// Long past any grace period, so nothing but the reference check is
	// keeping these objects alive.
	stats, err := s.Sweep(t.Context(), time.Now().Add(365*24*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep deleted %d artifacts of a stored result, want 0", stats.ArtifactsDeleted)
	}

	if stats.ArtifactsScanned != 3 {
		t.Errorf("ArtifactsScanned = %d, want 3 (document, screenshot, body)", stats.ArtifactsScanned)
	}

	assertStored(t, s, shot, "a referenced screenshot")
	assertStored(t, s, body, "a referenced body")

	if _, err := s.GetResult("site", model.ConsentReject, "scan-1"); err != nil {
		t.Errorf("the result is unreadable after a sweep: %v", err)
	}
}

// TestADeletionFailureDoesNotFailThePrune is AC4.
//
// The bucket is made to refuse one delete by taking write permission off the
// directory the key lives in, which is how a local filesystem says no. The
// prune must still succeed, count it, and leave the key for the next sweep to
// find — the reference rows are the work list, so clearing them before the
// object is gone would turn a refused delete into a permanent leak.
//
// The refusal is a local one, so this runs against a directory only. What a
// bucket that refuses a delete for its own reasons does to a prune is the same
// assertion made with a scheduled fault instead of a permission bit, in
// TestAPruneSurvivesABucketThatRefusesToDelete.
func TestADeletionFailureDoesNotFailThePrune(t *testing.T) {
	t.Parallel()

	skipUnlessLocalBucket(t, "the refused delete is a directory's permission bits")

	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores the directory permissions this test denies with")
	}

	opts := storeOptions(t)

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	old := time.Now().Add(-30 * 24 * time.Hour)
	shot, _ := withEvidence(t, s, "scan-old", old, []byte("undeletable"), []byte("a body"))

	kind := filepath.Join(opts.ArtifactDir, "screenshot-before-consent")

	if err := os.Chmod(kind, 0o500); err != nil {
		t.Fatalf("making the screenshot directory read-only: %v", err)
	}

	// Restored whatever the test does, so the temp directory can be removed.
	t.Cleanup(func() { _ = os.Chmod(kind, 0o700) })

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("a refused deletion failed the whole prune: %v", err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("ResultsDeleted = %d, want 1: retention still has to remove the row", stats.ResultsDeleted)
	}

	if stats.ArtifactsFailed != 1 {
		t.Errorf("ArtifactsFailed = %d, want 1", stats.ArtifactsFailed)
	}

	assertStored(t, s, shot, "the artifact the bucket would not delete")

	// With the bucket writable again, the next sweep finds the same key: it was
	// left on the work list rather than forgotten.
	if err := os.Chmod(kind, 0o700); err != nil {
		t.Fatalf("making the screenshot directory writable again: %v", err)
	}

	swept, err := s.Sweep(t.Context(), time.Now(), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if swept.ArtifactsDeleted != 1 {
		t.Errorf("the sweep collected %d artifacts, want the 1 the prune could not delete",
			swept.ArtifactsDeleted)
	}

	assertGone(t, s, shot, "the artifact the next sweep collected")
}

// TestPruningNothingRemovesNothing keeps the no-retention case honest now that
// a prune can delete evidence: a store with no retention configured must not
// touch the bucket at all.
func TestPruningNothingRemovesNothing(t *testing.T) {
	t.Parallel()

	s := open(t)

	shot, body := withEvidence(t, s, "scan-1", time.Now().Add(-365*24*time.Hour),
		[]byte("kept"), []byte("also kept"))

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 0 || stats.ArtifactsDeleted != 0 {
		t.Errorf("a prune with no retention removed %d results and %d artifacts, want none",
			stats.ResultsDeleted, stats.ArtifactsDeleted)
	}

	assertStored(t, s, shot, "the screenshot")
	assertStored(t, s, body, "the stored body")
}

// TestCountBasedPruningReclaimsEveryDocument is the count limit's side of AC1
// and AC5: retention by count is the one that runs constantly on a busy store,
// so it is the one whose bytes have to come back.
func TestCountBasedPruningReclaimsEveryDocument(t *testing.T) {
	t.Parallel()

	s := open(t)
	base := time.Now().Add(-time.Hour)

	for i := range 10 {
		res := result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)
		// Distinct bytes per scan, so each document is its own object and the
		// count is not quietly a count of one shared artifact.
		res.URL = fmt.Sprintf("https://example.com/%d", i)

		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxPerSeries: 3})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 7 {
		t.Fatalf("ResultsDeleted = %d, want 7", stats.ResultsDeleted)
	}

	if stats.ArtifactsDeleted != 7 {
		t.Errorf("ArtifactsDeleted = %d, want 7: one document per pruned result", stats.ArtifactsDeleted)
	}

	// The three that were kept still read, which is what proves the deletion
	// was decided by reference and not by age.
	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("kept %d results, want 3", len(got))
	}

	for _, sum := range got {
		if _, err := s.GetResult("site", model.ConsentReject, sum.ScanID); err != nil {
			t.Errorf("kept result %s is unreadable after the prune: %v", sum.ScanID, err)
		}
	}
}

// TestAMigratedStoreCanPrune is AC2's other requirement: a store upgraded from
// the layout that kept documents in rows must come out of the upgrade able to
// say what each result references, or retention would have to refuse to delete
// anything at all.
func TestAMigratedStoreCanPrune(t *testing.T) {
	t.Parallel()

	o := oldLayoutStore(t)

	old := time.Now().Add(-30 * 24 * time.Hour)
	res := result("scan-old", old, model.ConsentReject)

	// A screenshot the old row's document names, stored in the bucket the way
	// Story 4.6 stored one: content-addressed, beside the database.
	shot := writeArtifact(t, evidence(t, o.opts), "screenshot-before-consent", []byte("an old screenshot"))
	res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: shot}}

	o.insert(t, res)

	s, err := store.OpenSQL(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("migrating the store: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	assertStored(t, s, shot, "the migrated result's screenshot")

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.UnknownReferences != 0 {
		t.Errorf("UnknownReferences = %d after a migration, want 0: the upgrade has to record what each result names",
			stats.UnknownReferences)
	}

	if stats.ResultsDeleted != 1 {
		t.Fatalf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	if stats.ArtifactsDeleted != 2 {
		t.Errorf("ArtifactsDeleted = %d, want 2 (the migrated document and its screenshot)",
			stats.ArtifactsDeleted)
	}

	assertGone(t, s, shot, "the migrated result's screenshot")
}
