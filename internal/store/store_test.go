package store_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()

	dir := t.TempDir()

	s, err := store.Open(store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s
}

func result(id string, at time.Time, mode model.ConsentMode) *model.Result {
	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        id,
		Target:        "site",
		URL:           "https://example.com/",
		ConsentMode:   mode,
		StartedAt:     at,
		FinishedAt:    at.Add(time.Second),
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied},
		Requests: []model.Request{
			{URL: "https://tracker.test/px", NormalizedURL: "https://tracker.test/px",
				Domain: "tracker.test", Party: model.ThirdParty, Phase: model.PhasePre},
		},
	}
}

func TestPutAndGetResult(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Now()

	if err := s.PutResult(result("scan-1", now, model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}

	if got.ScanID != "scan-1" || len(got.Requests) != 1 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestGetResultNotFound(t *testing.T) {
	t.Parallel()

	s := open(t)

	_, err := s.GetResult("site", model.ConsentReject, "absent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestSeriesAreIsolatedByConsentMode: the store must not let a query for one
// mode return another mode's results, since comparing across modes is invalid.
func TestSeriesAreIsolatedByConsentMode(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Now()

	if err := s.PutResult(result("reject-1", now, model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(result("accept-1", now, model.ConsentAccept)); err != nil {
		t.Fatal(err)
	}

	rejects, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(rejects) != 1 || rejects[0].ScanID != "reject-1" {
		t.Errorf("reject series = %+v, want only reject-1", rejects)
	}

	series, err := s.Series()
	if err != nil {
		t.Fatal(err)
	}

	if len(series) != 2 {
		t.Errorf("got %d series, want 2", len(series))
	}
}

func TestListResultsIsNewestFirst(t *testing.T) {
	t.Parallel()

	s := open(t)
	base := time.Now().Add(-time.Hour)

	for i := range 5 {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), at, model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListResults("site", model.ConsentReject, 3)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d summaries, want 3", len(got))
	}

	want := []string{"scan-4", "scan-3", "scan-2"}
	for i := range want {
		if got[i].ScanID != want[i] {
			t.Errorf("summary[%d] = %q, want %q", i, got[i].ScanID, want[i])
		}
	}
}

func TestSummaryCarriesPreConsentCount(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 1)
	if err != nil {
		t.Fatal(err)
	}

	if got[0].PreConsentDomains != 1 {
		t.Errorf("PreConsentDomains = %d, want 1", got[0].PreConsentDomains)
	}

	if got[0].ThirdPartyDomains != 1 {
		t.Errorf("ThirdPartyDomains = %d, want 1", got[0].ThirdPartyDomains)
	}
}

func TestLatestAndPreviousResult(t *testing.T) {
	t.Parallel()

	s := open(t)
	base := time.Now().Add(-time.Hour)

	for i := range 3 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := s.LatestResult("site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	if latest.ScanID != "scan-2" {
		t.Errorf("LatestResult = %q, want scan-2", latest.ScanID)
	}

	prev, err := s.PreviousResult("site", model.ConsentReject, "scan-2")
	if err != nil {
		t.Fatal(err)
	}

	if prev.ScanID != "scan-1" {
		t.Errorf("PreviousResult = %q, want scan-1", prev.ScanID)
	}

	if _, err := s.PreviousResult("site", model.ConsentReject, "scan-0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviousResult before the oldest = %v, want ErrNotFound", err)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	b, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "martin", "reviewed")
	if err != nil {
		t.Fatalf("SetBaseline: %v", err)
	}

	if b.Result == nil || b.Result.ScanID != "scan-1" {
		t.Fatal("baseline does not carry its result")
	}

	got, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	if got.ApprovedBy != "martin" || got.Note != "reviewed" {
		t.Errorf("approval metadata lost: %+v", got)
	}
}

// TestBaselineRejectsFailedScan: approving a broken scan would pin a broken
// observation as the definition of correct.
func TestBaselineRejectsFailedScan(t *testing.T) {
	t.Parallel()

	s := open(t)

	bad := result("bad-1", time.Now(), model.ConsentReject)
	bad.Termination = model.TermError
	bad.Error = "navigate failed"

	if err := s.PutResult(bad); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "bad-1", "martin", ""); err == nil {
		t.Fatal("SetBaseline accepted a failed scan")
	}
}

// TestBaselineSurvivesRetentionPruning is why the baseline stores its own
// copy: expiring history must not silently invalidate what "expected" means.
func TestBaselineSurvivesRetentionPruning(t *testing.T) {
	t.Parallel()

	s := open(t)
	old := time.Now().Add(-30 * 24 * time.Hour)

	if err := s.PutResult(result("scan-old", old, model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-old", "martin", ""); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("ResultsDeleted = %d, want 1", stats.ResultsDeleted)
	}

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("baseline lost after pruning: %v", err)
	}

	if b.Result == nil || len(b.Result.Requests) != 1 {
		t.Error("baseline result is no longer usable after pruning")
	}
}

func TestPruneByCount(t *testing.T) {
	t.Parallel()

	s := open(t)
	base := time.Now().Add(-time.Hour)

	for i := range 10 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.Prune(time.Now(), store.Retention{MaxPerSeries: 3})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 7 {
		t.Errorf("ResultsDeleted = %d, want 7", stats.ResultsDeleted)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("kept %d results, want 3", len(got))
	}

	// The newest must be the ones kept.
	if got[0].ScanID != "scan-9" {
		t.Errorf("newest kept = %q, want scan-9", got[0].ScanID)
	}
}

func TestPruneWithNoRetentionKeepsEverything(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(result("scan-1", time.Now().Add(-365*24*time.Hour), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Prune(time.Now(), store.Retention{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 0 {
		t.Errorf("ResultsDeleted = %d with no retention configured, want 0", stats.ResultsDeleted)
	}
}

func TestAuditLogRecordsApprovals(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "martin", "looks fine"); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(entries))
	}

	e := entries[0]
	if e.Action != "baseline-approved" || e.Actor != "martin" || e.Subject != "scan-1" {
		t.Errorf("audit entry = %+v", e)
	}
}

func TestArtifactRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)

	ref, err := s.PutArtifact("screenshot", []byte("png-bytes"))
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	got, err := s.GetArtifact(ref)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}

	if string(got) != "png-bytes" {
		t.Errorf("artifact round trip = %q", got)
	}

	// Content addressing means storing the same bytes twice is one file.
	ref2, err := s.PutArtifact("screenshot", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}

	if ref != ref2 {
		t.Errorf("same content produced different references: %q and %q", ref, ref2)
	}
}

// TestArtifactPathTraversalIsRefused: references derive from stored results,
// which derive from page-controlled data, so they are untrusted input.
func TestArtifactPathTraversalIsRefused(t *testing.T) {
	t.Parallel()

	s := open(t)

	for _, ref := range []string{
		"../../etc/passwd",
		"screenshot/../../../../etc/passwd",
		"/etc/passwd",
	} {
		if _, err := s.GetArtifact(ref); err == nil {
			t.Errorf("GetArtifact(%q) succeeded; it must not escape the artifact directory", ref)
		}
	}
}

func TestArtifactPermissionsAreRestrictive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	s, err := store.Open(store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = s.Close() }()

	ref, err := s.PutArtifact("screenshot", []byte("secret evidence"))
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "artifacts", filepath.FromSlash(ref)))
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("artifact permissions = %o, want 600: results can contain personal data", perm)
	}
}

func TestDatabasePermissionsAreRestrictive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = s.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("database permissions = %o, want 600", perm)
	}
}

func TestReopenPreservesData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}

	defer func() { _ = reopened.Close() }()

	if _, err := reopened.GetResult("site", model.ConsentReject, "scan-1"); err != nil {
		t.Errorf("data lost across reopen: %v", err)
	}
}

func TestPutResultRejectsIncompleteRecords(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(&model.Result{ScanID: "x"}); err == nil {
		t.Error("stored a result with no target")
	}

	if err := s.PutResult(&model.Result{Target: "site"}); err == nil {
		t.Error("stored a result with no scan ID")
	}
}
