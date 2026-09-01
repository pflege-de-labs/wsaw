package store_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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

// --- Story 4.6: database/sql on SQLite ------------------------------------

// sqlDB opens the store's own file directly, for assertions about the schema
// and about rows the store's API deliberately will not produce.
func sqlDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// TestPragmasAreApplied is AC6: SQLite has to be told to behave well for a
// long-running process, and the settings are easy to lose in a DSN.
func TestPragmasAreApplied(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = s.Close() }()

	db := sqlDB(t, path)

	var journal string
	if err := db.QueryRowContext(t.Context(), "pragma journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}

	// WAL is what lets the web interface read while the daemon writes.
	if strings.ToLower(journal) != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
}

// TestSchemaVersionIsRecorded is the first half of AC5.
func TestSchemaVersionIsRecorded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db := sqlDB(t, path)

	var version int
	if err := db.QueryRowContext(t.Context(), "pragma user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}

	if version < 1 {
		t.Errorf("user_version = %d, want at least 1: a store with no version cannot be migrated safely", version)
	}
}

// TestMigrationIsIdempotent: opening an existing store must not try to apply
// what is already there.
func TestMigrationIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	for i := range 3 {
		s, err := store.Open(store.Options{Path: path})
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}

		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), time.Now(), model.ConsentReject)); err != nil {
			t.Fatalf("put %d: %v", i+1, err)
		}

		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i+1, err)
		}
	}

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = s.Close() }()

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Errorf("got %d results across three opens, want 3", len(got))
	}
}

// TestNewerSchemaIsRefused is the half of AC5 that protects data: a binary
// that does not understand the schema must not write to it.
func TestNewerSchemaIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Pretend a much newer wsaw has been here.
	db := sqlDB(t, path)
	if _, err := db.ExecContext(t.Context(), "pragma user_version = 9999"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Open(store.Options{Path: path}); err == nil {
		t.Fatal("a store written by a newer schema was opened anyway")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// TestBaselineApprovalAndAuditAreAtomic is AC7. An approval is what silences
// future findings, so an audit log that can lose one is not an audit log.
func TestBaselineApprovalAndAuditAreAtomic(t *testing.T) {
	t.Parallel()

	s := open(t)

	// A scan that cannot be approved, so the approval fails part-way.
	bad := result("bad-1", time.Now(), model.ConsentReject)
	bad.Termination = model.TermError
	bad.Error = "navigate failed"

	if err := s.PutResult(bad); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "bad-1", "martin", ""); err == nil {
		t.Fatal("SetBaseline accepted a failed scan")
	}

	// Neither half may have landed.
	if _, err := s.GetBaseline("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a baseline was stored despite the approval failing: %v", err)
	}

	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("an audit entry was written for an approval that did not happen: %+v", entries)
	}
}

// TestCorruptRowIsReportedNotFatal is AC8, and Tenet 5: one unreadable record
// must neither hide the rest of the history nor vanish silently.
func TestCorruptRowIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-time.Hour)

	for i := range 3 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the middle one behind the store's back.
	db := sqlDB(t, path)
	if _, err := db.ExecContext(t.Context(), `update results set document = '{not json' where scan_id = 'scan-1'`); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = reopened.Close() }()

	got, err := reopened.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("one corrupt row made the whole history unreadable: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d summaries, want 3: the corrupt row must still be listed", len(got))
	}

	var reported bool

	for _, sm := range got {
		if sm.ScanID != "scan-1" {
			continue
		}

		reported = true

		if sm.Termination != model.TermError || sm.Error == "" {
			t.Errorf("the corrupt row is not reported as a failure: %+v", sm)
		}
	}

	if !reported {
		t.Error("the corrupt row vanished from the listing instead of being reported")
	}

	// The readable ones are still readable.
	if _, err := reopened.GetResult("site", model.ConsentReject, "scan-2"); err != nil {
		t.Errorf("a healthy result became unreadable: %v", err)
	}
}

// TestResultIsStoredAsItsDocument is AC3: the JSON schema is the interface, so
// the stored document must be the result itself, not a reassembly of columns.
func TestResultIsStoredAsItsDocument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	s, err := store.Open(store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	res := result("scan-1", time.Now(), model.ConsentReject)
	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db := sqlDB(t, path)

	// Read through SQLite's own JSON functions, which only works if the
	// column really holds the result document.
	var schemaVersion, termination string

	err = db.QueryRowContext(t.Context(), `
		select json_extract(document, '$.schemaVersion'), termination
		from results where scan_id = 'scan-1'`).Scan(&schemaVersion, &termination)
	if err != nil {
		t.Fatalf("the stored row is not the result document: %v", err)
	}

	if schemaVersion != model.SchemaVersion {
		t.Errorf("schemaVersion in the document = %q, want %q", schemaVersion, model.SchemaVersion)
	}

	// And the indexed column agrees with the document it was extracted from.
	if termination != string(res.Termination) {
		t.Errorf("termination column = %q, document says %q", termination, res.Termination)
	}
}

// TestOpenFailsActionablyOnAnUnusablePath is AC11.
func TestOpenFailsActionablyOnAnUnusablePath(t *testing.T) {
	t.Parallel()

	// A path whose parent is a file, so the directory cannot be created.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := store.Open(store.Options{Path: filepath.Join(blocker, "nested", "wsaw.db")})
	if err == nil {
		t.Fatal("Open succeeded against an unusable path")
	}
}
