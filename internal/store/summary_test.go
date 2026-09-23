package store

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// These are white-box tests of ListResults' AC2 join: DocumentBytes and
// ArtifactBytes have to come from the row and the reference table alone, at
// the cost of nothing against the bucket (Story 5.31, AC2).

// resultWithEvidence stores a result naming one screenshot and one stored
// body, and returns their references alongside the bytes ListResults should
// report for it.
func resultWithEvidence(t *testing.T, s *SQL, id string, at time.Time) (screenshotBytes, bodyBytes int64) {
	t.Helper()

	screenshot := []byte("a screenshot, " + id)
	body := []byte("a response body, " + id)

	screenshotRef, err := s.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	bodyRef, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatalf("storing a body: %v", err)
	}

	res := storedResult(id, at)
	res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: screenshotRef, Bytes: int64(len(screenshot))}}
	res.Requests[0].BodyRef = bodyRef
	res.Requests[0].BodyStoredSize = int64(len(body))

	if err := s.PutResult(res); err != nil {
		t.Fatalf("storing result %s: %v", id, err)
	}

	return int64(len(screenshot)), int64(len(body))
}

// TestListResultsReportsDocumentAndArtifactBytes: a row's size is
// document_size plus the joined sum of result_artifacts.bytes, and the
// listing that reports it touches the bucket for nothing (the same
// assertion TestListingPerformsNoBucketOperations makes, extended to cover
// this join).
func TestListResultsReportsDocumentAndArtifactBytes(t *testing.T) {
	t.Parallel()

	s, ops := openCounted(t)

	now := time.Now()

	shotBytes, bodyBytes := resultWithEvidence(t, s, "scan-a", now)

	ops.Store(0)

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	if n := ops.Load(); n != 0 {
		t.Errorf("ListResults performed %d bucket operations, want 0", n)
	}

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}

	sm := got[0]

	if sm.DocumentBytes <= 0 {
		t.Errorf("DocumentBytes = %d, want the stored document's own size", sm.DocumentBytes)
	}

	if want := shotBytes + bodyBytes; sm.ArtifactBytes != want {
		t.Errorf("ArtifactBytes = %d, want %d (screenshot + body)", sm.ArtifactBytes, want)
	}

	if !sm.ArtifactBytesRecorded {
		t.Error("ArtifactBytesRecorded = false, want true for a result stored with the bytes column present")
	}
}

// TestListResultsChargesEachScanForASharedArtifact: artifacts are
// content-addressed, so two results that captured byte-identical evidence
// share one bucket object but are each charged for it in their own row
// (AC4) — the sum is not deduplicated across scans, only within one.
func TestListResultsChargesEachScanForASharedArtifact(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	shared := []byte("byte-identical evidence")

	sharedRef, err := s.PutArtifact("screenshot-before-consent", shared)
	if err != nil {
		t.Fatalf("storing the shared screenshot: %v", err)
	}

	now := time.Now()

	for i, id := range []string{"scan-a", "scan-b"} {
		res := storedResult(id, now.Add(time.Duration(i)*time.Minute))
		res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: sharedRef, Bytes: int64(len(shared))}}

		if err := s.PutResult(res); err != nil {
			t.Fatalf("storing result %s: %v", id, err)
		}
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	for _, sm := range got {
		if sm.ArtifactBytes != int64(len(shared)) {
			t.Errorf("scan %s: ArtifactBytes = %d, want %d (its own charge for the shared artifact)",
				sm.ScanID, sm.ArtifactBytes, len(shared))
		}
	}
}

// TestListResultsFlagsArtifactBytesNotRecordedForAPreMigrationResult: a
// result_artifacts row written before the bytes column existed defaults to
// 0, and that must read as "size not recorded" rather than as a genuinely
// empty artifact (AC1, AC5) — which is exactly what a result naming no
// evidence at all does read as.
func TestListResultsFlagsArtifactBytesNotRecordedForAPreMigrationResult(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	now := time.Now()

	screenshot := []byte("a screenshot with no other evidence beside it")

	screenshotRef, err := s.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	legacyResult := storedResult("scan-legacy", now)
	legacyResult.Requests = nil // no body reference, so the screenshot is the only artifact row
	legacyResult.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: screenshotRef, Bytes: int64(len(screenshot))}}

	if err := s.PutResult(legacyResult); err != nil {
		t.Fatalf("storing result scan-legacy: %v", err)
	}

	// Simulates the row this migration's own comment describes: a reference
	// written before Story 5.31 shipped, its bytes column at the default a
	// bare ADD COLUMN gives every existing row. It is the only artifact row
	// this result names, so zeroing it is what a pre-migration write of this
	// exact result would have left behind.
	if _, err := s.db.ExecContext(t.Context(),
		s.q(`update result_artifacts set bytes = 0 where artifact_ref = ?`), screenshotRef); err != nil {
		t.Fatalf("simulating a pre-migration row: %v", err)
	}

	if err := s.PutResult(storedResult("scan-bare", now.Add(time.Minute))); err != nil {
		t.Fatalf("storing a result with no evidence: %v", err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	byID := make(map[string]Summary, len(got))
	for _, sm := range got {
		byID[sm.ScanID] = sm
	}

	legacy, ok := byID["scan-legacy"]
	if !ok {
		t.Fatal("scan-legacy is missing from the listing")
	}

	if legacy.ArtifactBytesRecorded {
		t.Error("a result whose only artifact's bytes were zeroed out reports ArtifactBytesRecorded = true, want false")
	}

	if legacy.ArtifactBytes != 0 {
		t.Errorf("ArtifactBytes = %d, want 0 (the zeroed row)", legacy.ArtifactBytes)
	}

	bare, ok := byID["scan-bare"]
	if !ok {
		t.Fatal("scan-bare is missing from the listing")
	}

	if !bare.ArtifactBytesRecorded {
		t.Error("a result that names no evidence at all reports ArtifactBytesRecorded = false, want true")
	}

	if bare.ArtifactBytes != 0 {
		t.Errorf("ArtifactBytes = %d, want 0 (no evidence named)", bare.ArtifactBytes)
	}
}
