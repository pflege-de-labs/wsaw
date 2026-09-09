package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// open returns a store of whichever kind the suite is being run against.
//
// Every test below uses it, so one set of test bodies exercises SQLite,
// PostgreSQL, MySQL (Story 4.7, AC5) and — since Story 8.10 — the store that
// keeps its index in the artifact bucket instead of in rows (AC15). The tests
// are the specification of what a store does; a store that needed its own
// tests would be a store that had changed the behaviour.
//
// It returns the interface rather than a concrete store, which is what makes
// the last of those possible: the bucket-index store is not a *store.SQL and
// has no dialect, and every test body here asks only what the seam promises.
//
//	go test ./internal/store
//	WSAW_TEST_STORE_DRIVER=blob                                 go test ./internal/store
//	WSAW_TEST_STORE_DRIVER=postgres WSAW_TEST_POSTGRES_DSN=... go test ./internal/store
//	WSAW_TEST_STORE_DRIVER=mysql    WSAW_TEST_MYSQL_DSN=...    go test ./internal/store
func open(t *testing.T) store.Store {
	t.Helper()

	s, err := store.Open(t.Context(), storeOptions(t))
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

// openAt opens a store on options the caller already holds, closing it when the
// test ends.
//
// It is open(t) for the tests that also have to reach the artifact directory
// the store was given, because what they assert is what is beside the index on
// disk rather than what the store says about it.
func openAt(t *testing.T, opts store.Options) store.Store {
	t.Helper()

	s, err := store.Open(t.Context(), opts)
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

// storeOptions names an empty store on whichever dialect the suite is being
// run against.
//
// It is separate from open() for the tests that have to reach the database
// underneath a store as well as the store itself — the migration tests, which
// write rows in a layout no current wsaw produces (Story 8.4) — and for those
// that open the same store twice.
func storeOptions(t *testing.T) store.Options {
	t.Helper()

	opts := store.Options{
		ArtifactDir: filepath.Join(t.TempDir(), "artifacts"),
		// Discarded so that a suite which upgrades a good many stores does not
		// bury its own failures in migration progress. The one test that
		// asserts on that progress installs a logger of its own.
		Logger: slog.New(slog.DiscardHandler),
	}

	switch driver := os.Getenv("WSAW_TEST_STORE_DRIVER"); driver {
	case "", store.DriverSQLite:
		opts.Path = filepath.Join(t.TempDir(), "wsaw.db")

	case store.DriverBlob:
		// Nothing else to name: ArtifactDir above is the bucket, and for this
		// store the bucket is the whole store. There is no scratch database to
		// create and no container to skip on, which is why this one runs in
		// the default suite rather than behind an environment variable naming
		// a server.
		opts.Driver = driver

	case store.DriverPostgres, store.DriverMySQL:
		opts.Driver = driver
		opts.DSN = secret.Literal(scratchDatabase(t, driver))

	default:
		t.Fatalf("WSAW_TEST_STORE_DRIVER=%q is not a driver this suite knows", driver)
	}

	return opts
}

// skipUnlessSQL skips a test that is about an index kept in rows, when the
// suite is being run against the index kept in the bucket.
//
// The tests that call it are about a schema: the row layout that came before
// the bucket, the migration that moved documents out of a column, the backfill
// that derives references an upgrade could not. A store with no schema has
// nothing for them to hold it to — and refusing to open one for them is not a
// gap in its coverage, because what a store promises is asserted through the
// interface by the tests that use open(t), which every store runs (AC15).
//
// It reads the environment rather than the driver of an already-open store,
// because the point is not to open one.
func skipUnlessSQL(t *testing.T) {
	t.Helper()

	if os.Getenv("WSAW_TEST_STORE_DRIVER") == store.DriverBlob {
		t.Skip("this test is about a store that keeps its index in rows; the blob store keeps no schema")
	}
}

// assertTheHistorySurvivedALostBucket checks what each kind of store can still
// promise once its artifact bucket has gone away.
//
// For a store with a database the promise is the strong one the test around it
// is named for: a prune that could not reclaim must not remove the rows that
// say what there was to reclaim, so the history is read back and has to be
// intact.
//
// The bucket-index store cannot promise that and must not pretend to. Its
// index is objects in the same bucket, so an unmounted volume takes the history
// with it — there is no second place for it to have survived in. What it can
// promise is the half that matters: it says the store is unreachable rather
// than reporting an empty history as though the target had never been scanned,
// which is the inference Tenet 5 forbids and the one this test exists to
// prevent. The prune's own refusal, asserted above this call, is the same
// promise from the other end.
func assertTheHistorySurvivedALostBucket(t *testing.T, s store.Store) {
	t.Helper()

	if os.Getenv("WSAW_TEST_STORE_DRIVER") == store.DriverBlob {
		if err := s.Ping(t.Context()); err == nil {
			t.Error("a store whose bucket has gone away reported itself reachable")
		}

		return
	}

	rows, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Errorf("the store holds %d results after a prune that could not reach the bucket, want 1", len(rows))
	}
}

// foreignToThisStore is how many of the objects a sweep test plants are foreign
// to the store under test.
//
// Two of them are a bucket-index layout, which Story 8.10, AC16 explicitly
// permits a SQL deployment to meet in a bucket whose index once lived beside
// the evidence: to a SQL store all four are objects it did not write.
//
// The bucket-index store sees one of those two and not the other, and the
// count is one lower for that reason rather than because it judges the missing
// one safe. Its sweep walks the evidence plus the two index prefixes it needs
// work lists for — the pins under ref/ and the scan keys under byid/ — and
// never lists audit/ at all (the index tree is skipped whole; see the blob
// sweep's kinds()). So the planted audit entry is out of the sweep's reach,
// while the planted pin — a directory whose last segment is no owner that
// grammar spells — is inside it and is counted as a stray. Both stores leave
// all four exactly where they are, which is what the loop after this call
// asserts and what the criterion is actually about.
//
// A sweep that later grew a pass over audit/ would have to judge that key, and
// this comment is where to start.
func foreignToThisStore(planted int) int {
	if os.Getenv("WSAW_TEST_STORE_DRIVER") == store.DriverBlob {
		return planted - 1
	}

	return planted
}

// sqliteOptions is the on-disk store the schema tests need: they reopen it,
// read its file directly, or assert on what is on disk, none of which the
// dialect-agnostic open() above offers.
//
// The artifact directory is part of it because every store needs one — a scan's
// result document is written there rather than into a row (Story 8.2).
func sqliteOptions(dir string) store.Options {
	return store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
		Logger:      slog.New(slog.DiscardHandler),
	}
}

// scratchCounter names each test's database. Tests run in parallel and each
// one assumes an empty store, so they cannot share a schema.
var scratchCounter atomic.Int64

// scratchDatabase creates an empty database on the configured server and
// returns a DSN pointing at it, dropping it when the test ends.
func scratchDatabase(t *testing.T, driver string) string {
	t.Helper()

	envVar := map[string]string{
		store.DriverPostgres: "WSAW_TEST_POSTGRES_DSN",
		store.DriverMySQL:    "WSAW_TEST_MYSQL_DSN",
	}[driver]

	admin := os.Getenv(envVar)
	if admin == "" {
		t.Skipf("%s is not set; start a %s and point it there to run the store suite against it", envVar, driver)
	}

	name := fmt.Sprintf("wsaw_test_%d_%d", os.Getpid(), scratchCounter.Add(1))

	db, err := sql.Open(sqlDriverFor(driver), admin)
	if err != nil {
		t.Fatalf("connecting to the %s server: %v", driver, err)
	}

	defer func() { _ = db.Close() }()

	// The name is generated above, not taken from input, so interpolating it
	// is safe — and neither database accepts a placeholder here anyway.
	if _, err := db.ExecContext(t.Context(), "create database "+name); err != nil { //nolint:gosec // see above
		t.Fatalf("creating scratch database %s: %v", name, err)
	}

	t.Cleanup(func() {
		cleanup, err := sql.Open(sqlDriverFor(driver), admin)
		if err != nil {
			return
		}

		defer func() { _ = cleanup.Close() }()

		// A fresh context: the test's own is already cancelled by the time
		// cleanup runs, and the database still has to be dropped.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		//nolint:gosec // the name is generated, not user input
		if _, err := cleanup.ExecContext(ctx, "drop database "+name); err != nil {
			t.Logf("dropping scratch database %s: %v", name, err)
		}
	})

	return withDatabase(t, driver, admin, name)
}

func sqlDriverFor(driver string) string {
	if driver == store.DriverPostgres {
		return "pgx"
	}

	return "mysql"
}

// withDatabase rewrites a DSN to point at another database on the same
// server. The two drivers spell a DSN differently, so each is handled by its
// own parser rather than by string surgery.
func withDatabase(t *testing.T, driver, dsn, name string) string {
	t.Helper()

	if driver == store.DriverPostgres {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parsing %s: %v", "WSAW_TEST_POSTGRES_DSN", err)
		}

		u.Path = "/" + name

		return u.String()
	}

	cfg, err := gomysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing WSAW_TEST_MYSQL_DSN: %v", err)
	}

	cfg.DBName = name

	return cfg.FormatDSN()
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
			{
				URL: "https://tracker.test/px", NormalizedURL: "https://tracker.test/px",
				Domain: "tracker.test", Party: model.ThirdParty, Phase: model.PhasePre,
			},
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

// HasResult answers the interface's "may I link to this?" without reading a
// document that runs to megabytes.
func TestHasResult(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		mode   model.ConsentMode
		scanID string
		want   bool
	}{
		{"stored", model.ConsentReject, "scan-1", true},
		{"absent", model.ConsentReject, "scan-2", false},
		{"another mode", model.ConsentAccept, "scan-1", false},
	} {
		got, err := s.HasResult("site", tc.mode, tc.scanID)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		if got != tc.want {
			t.Errorf("%s: HasResult = %v, want %v", tc.name, got, tc.want)
		}
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

// TestHasBaselineAnswersTheSameQuestionAsGetBaseline: the targets page asks
// whether a series has a baseline once per series on every render, and asks it
// with HasBaseline so that a yes/no does not cost a whole approved result
// (Story 8.10). A cheaper question that could disagree with the expensive one
// would be worth far less than the reads it saves, so both are asked here.
func TestHasBaselineAnswersTheSameQuestionAsGetBaseline(t *testing.T) {
	t.Parallel()

	s := open(t)

	assertBaseline := func(what string, mode model.ConsentMode, want bool) {
		t.Helper()

		has, err := s.HasBaseline("site", mode)
		if err != nil {
			t.Fatalf("HasBaseline %s: %v", what, err)
		}

		if has != want {
			t.Errorf("HasBaseline %s = %v, want %v", what, has, want)
		}

		_, err = s.GetBaseline("site", mode)
		if got := err == nil; got != want {
			t.Errorf("GetBaseline %s found a baseline = %v, want %v (err %v)", what, got, want, err)
		}
	}

	assertBaseline("before any approval", model.ConsentReject, false)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "martin", ""); err != nil {
		t.Fatalf("SetBaseline: %v", err)
	}

	assertBaseline("after approval", model.ConsentReject, true)

	// The same target under a different consent mode is a different series,
	// and one approval must not make the other look approved.
	assertBaseline("for the other consent mode", model.ConsentAccept, false)

	if err := s.DeleteBaseline("site", model.ConsentReject, "martin"); err != nil {
		t.Fatalf("DeleteBaseline: %v", err)
	}

	assertBaseline("after the approval was withdrawn", model.ConsentReject, false)
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

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
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

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxPerSeries: 3})
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

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{})
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

// TestAuditLogIsOrderedByWhenTheActionHappened pins the log to its timestamps
// rather than to its insertion order.
//
// The two only agree while every entry is recorded at the moment it happens,
// and RecordAudit exists precisely for the entries that are not: an action
// taken outside the store, written back afterwards. An entry that arrives
// late belongs where its At puts it — an audit log that showed a decision
// from last week above one from today because it was written down second
// would misdescribe the sequence of events it exists to record.
//
// The same-instant pair is the tie-break: two entries that share a timestamp
// must come back in one fixed order, because a listing that shuffled them
// between two reads would be indistinguishable from the log being edited.
func TestAuditLogIsOrderedByWhenTheActionHappened(t *testing.T) {
	t.Parallel()

	s := open(t)

	now := time.Now().UTC()
	// Written in an order that disagrees with the timestamps, so ordering by
	// the row identity and ordering by the recorded time cannot both pass.
	// The last two share an instant, which is what the tie-break answers.
	written := []struct {
		subject string
		at      time.Time
	}{
		{"today", now},
		{"last-week", now.Add(-7 * 24 * time.Hour)},
		{"yesterday", now.Add(-24 * time.Hour)},
		{"same-instant-first", now.Add(-time.Hour)},
		{"same-instant-second", now.Add(-time.Hour)},
	}

	for _, w := range written {
		if err := s.RecordAudit(store.AuditEntry{
			At:      w.at,
			Actor:   "martin",
			Action:  "allow-list-added",
			Subject: w.subject,
		}); err != nil {
			t.Fatalf("RecordAudit(%s): %v", w.subject, err)
		}
	}

	entries, err := s.Audit(10)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Subject)
	}

	want := []string{"today", "same-instant-second", "same-instant-first", "yesterday", "last-week"}
	if !slices.Equal(got, want) {
		t.Errorf("audit order = %v, want %v", got, want)
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

// TestStatArtifactTellsPrunedFromPresent is Story 5.17, AC3: the interface has
// to know whether evidence is still there without fetching it, which against a
// bucket is the difference between a request and a megabyte.
func TestStatArtifactTellsPrunedFromPresent(t *testing.T) {
	t.Parallel()

	s := open(t)

	ref, err := s.PutArtifact("screenshot", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}

	info, err := s.StatArtifact(t.Context(), ref)
	if err != nil {
		t.Fatalf("StatArtifact: %v", err)
	}

	if info.Size != int64(len("png-bytes")) {
		t.Errorf("StatArtifact = %d bytes, want %d", info.Size, len("png-bytes"))
	}

	// The digest an entity tag is built from is the reference's own second
	// half, so it is reported without a second look at the object.
	if _, want, _ := strings.Cut(ref, "/"); info.Digest != want {
		t.Errorf("StatArtifact reports digest %q, want %q", info.Digest, want)
	}

	if info.ModTime.IsZero() {
		t.Error("StatArtifact reports no modification time, so no Last-Modified can be written")
	}

	absent := "screenshot/" + strings.Repeat("00", 32)

	if _, err := s.StatArtifact(t.Context(), absent); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("stat of a pruned artifact = %v, want ErrNotFound", err)
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
		"screenshot/deadbeef",
	} {
		_, err := s.GetArtifact(ref)
		if err == nil {
			t.Errorf("GetArtifact(%q) succeeded; it must not escape the artifact directory", ref)

			continue
		}

		// A reference this store never wrote names nothing this store has, so
		// it reads as absent. References arrive from URLs, and answering a
		// crafted one with a server error would turn hostile input into an
		// alert (Story 8.1, AC5 and AC9).
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetArtifact(%q) = %v, want ErrNotFound", ref, err)
		}
	}
}

func TestArtifactPermissionsAreRestrictive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	s, err := store.OpenSQL(t.Context(), store.Options{Path: path, ArtifactDir: filepath.Join(dir, "artifacts")})
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

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

// TestUpgradingAStoreThatAlreadyHasRows upgrades a store in the shape wsaw
// shipped before the document moved to the bucket, all the way to the current
// schema (Stories 8.2, 8.3 and 8.4). An upgrade must add the new columns to a
// table that already has rows, move each row's document into the bucket, and
// leave the rows themselves in place: they hold months of evidence, and
// discarding them would destroy the history the product exists to keep.
//
// The version-1 schema is written out here on purpose. It is frozen — a
// migration that has shipped is never edited — so a test that pins it cannot go
// stale, and reusing the current one would test nothing.
func TestUpgradingAStoreThatAlreadyHasRows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	db := sqlDB(t, path)

	for _, stmt := range []string{
		`create table results (
			target       text    not null,
			consent_mode text    not null,
			scan_id      text    not null,
			started_at   integer not null,
			termination  text    not null,
			document     text    not null,
			primary key (target, consent_mode, scan_id)
		) strict`,
		`create index results_series on results (target, consent_mode, started_at desc, scan_id desc)`,
		`create table baselines (
			target       text    not null,
			consent_mode text    not null,
			scan_id      text    not null,
			approved_at  integer not null,
			document     text    not null,
			primary key (target, consent_mode)
		) strict`,
		`create table audit (
			id       integer primary key autoincrement,
			at       integer not null,
			document text    not null
		) strict`,
		`insert into results values ('site', 'reject', 'scan-old', 1700000000000000000, 'idle', '{"scanId":"scan-old"}')`,
		`pragma user_version = 1`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("building a version-1 store: %v", err)
		}
	}

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
	if err != nil {
		t.Fatalf("upgrading a version-1 store: %v", err)
	}

	defer func() { _ = s.Close() }()

	// The old row is still there, and its document has moved into the bucket
	// byte for byte rather than being rewritten (Story 8.4, AC1).
	if hasColumn(t, dir, "document") {
		t.Error("the upgrade left the document column in place")
	}

	ref, size, digest := resultRow(t, dir, "scan-old")

	stored, err := os.ReadFile(documentPath(t, dir, ref))
	if err != nil {
		t.Fatalf("the moved document is not in the bucket: %v", err)
	}

	if string(stored) != `{"scanId":"scan-old"}` {
		t.Errorf("the existing document was rewritten: %s", stored)
	}

	if int64(len(stored)) != size {
		t.Errorf("the row records %d bytes for a document of %d", size, len(stored))
	}

	if sum := sha256.Sum256(stored); hex.EncodeToString(sum[:]) != digest {
		t.Errorf("the row records digest %s for a document that hashes to %x", digest, sum)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The document names no target and no consent mode, so nothing about the
	// row's identity may have come from it.
	if len(got) != 1 || got[0].ScanID != "scan-old" || got[0].Target != "site" {
		t.Errorf("the upgraded row is listed as %+v, want the row wsaw already had", got)
	}

	// And its start time is the row's, not a zero recomputed from a document
	// that does not carry one: a change of storage layout must not reorder
	// history.
	if want := time.Unix(0, 1700000000000000000).UTC(); !got[0].StartedAt.Equal(want) {
		t.Errorf("the upgraded row starts at %s, want the %s the row recorded", got[0].StartedAt, want)
	}

	// And the upgraded store writes new results the new way.
	if err := s.PutResult(result("scan-new", time.Now(), model.ConsentReject)); err != nil {
		t.Fatalf("an upgraded store cannot store a result: %v", err)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-new"); err != nil {
		t.Errorf("reading back from an upgraded store: %v", err)
	}
}

// TestMigrationIsIdempotent: opening an existing store must not try to apply
// what is already there.
func TestMigrationIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for i := range 3 {
		s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	if _, err := store.OpenSQL(t.Context(), sqliteOptions(dir)); err == nil {
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

// TestUnsummarisedRowIsReportedNotFatal is Story 4.6, AC8 and Story 8.2, AC5,
// and Tenet 5: one row that cannot be summarised must neither hide the rest of
// the history nor vanish silently.
//
// The row this produces is the one an older wsaw wrote, whose document has not
// been moved into the bucket yet: it has no reference and no derived summary,
// which is exactly what the migration of Story 8.4 exists to fix.
func TestUnsummarisedRowIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	s, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
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

	// Put the middle row back into the shape a pre-bucket wsaw left it in.
	db := sqlDB(t, filepath.Join(dir, "wsaw.db"))
	if _, err := db.ExecContext(t.Context(), `
		update results
		set artifact_ref = '', document_digest = '', document_size = 0, requests = 0,
		    third_party_domains = 0, pre_consent_domains = 0
		where scan_id = 'scan-1'`); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenSQL(t.Context(), sqliteOptions(dir))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = reopened.Close() }()

	got, err := reopened.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("one unsummarised row made the whole history unreadable: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d summaries, want 3: the unsummarised row must still be listed", len(got))
	}

	var reported bool

	for _, sm := range got {
		if sm.ScanID != "scan-1" {
			continue
		}

		reported = true

		if sm.Error == "" {
			t.Errorf("the row without a summary is listed as though it had one: %+v", sm)
		}
	}

	if !reported {
		t.Error("the unsummarised row vanished from the listing instead of being reported")
	}

	// Its document is not readable from the bucket, and that reads as absent
	// rather than as a broken store.
	if _, err := reopened.GetResult("site", model.ConsentReject, "scan-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the unsummarised result = %v, want ErrNotFound", err)
	}

	// The readable ones are still readable.
	if _, err := reopened.GetResult("site", model.ConsentReject, "scan-2"); err != nil {
		t.Errorf("a healthy result became unreadable: %v", err)
	}
}

// --- Story 8.2: the document is an artifact, the row is an index -----------

// documentPath is where the bucket keeps a result document, given the directory
// a sqliteOptions store was built in. Reaching into it is the point: these tests
// assert what is on disk, not what the store says is on disk.
func documentPath(t *testing.T, dir, ref string) string {
	t.Helper()

	return artifactFile(t, filepath.Join(dir, "artifacts"), ref)
}

// artifactFile is where the bucket keeps one artifact, given the artifact
// directory itself.
//
// It is the same layout for every kind of store — the bucket is the bucket, and
// only the index differs (Story 8.1, AC2) — which is what lets the tests below
// assert on a stored document without knowing which store wrote it.
func artifactFile(t *testing.T, artifactDir, ref string) string {
	t.Helper()

	kind, digest, found := strings.Cut(ref, "/")
	if !found || kind == "" || digest == "" {
		t.Fatalf("artifact reference %q is not one this store wrote", ref)
	}

	return filepath.Join(artifactDir, kind, digest)
}

// documentRefFor finds the stored document of one scan by reading the artifact
// directory, so a test can assert on it without knowing whether the store keeps
// its index in rows or in objects.
//
// It returns what resultRow's three columns hold — where the document is, how
// big it is, and what it hashes to — computed from the object itself rather than
// from whatever the index recorded, which is what makes it usable in the tests
// that go on to prove the index and the object disagree.
func documentRefFor(t *testing.T, artifactDir, scanID string) (ref string, size int64, digest string) {
	t.Helper()

	dir := filepath.Join(artifactDir, "result")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the stored documents: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		stored, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the stored document %s: %v", path, err)
		}

		var decoded struct {
			ScanID string `json:"scanId"`
		}

		if err := json.Unmarshal(stored, &decoded); err != nil || decoded.ScanID != scanID {
			continue
		}

		sum := sha256.Sum256(stored)

		return "result/" + entry.Name(), int64(len(stored)), hex.EncodeToString(sum[:])
	}

	t.Fatalf("no stored document in %s names scan %s", dir, scanID)

	return "", 0, ""
}

// resultRow reads the columns that replaced the document, for a stored scan.
func resultRow(t *testing.T, dir, scanID string) (ref string, size int64, digest string) {
	t.Helper()

	db := sqlDB(t, filepath.Join(dir, "wsaw.db"))

	err := db.QueryRowContext(t.Context(),
		`select artifact_ref, document_size, document_digest from results where scan_id = ?`,
		scanID).Scan(&ref, &size, &digest)
	if err != nil {
		t.Fatalf("reading the row for %s: %v", scanID, err)
	}

	return ref, size, digest
}

// hasColumn reports whether the results table still carries a column, which is
// how the tests of Story 8.4 assert that a migration dropped one.
func hasColumn(t *testing.T, dir, column string) bool {
	t.Helper()

	db := sqlDB(t, filepath.Join(dir, "wsaw.db"))

	var n int

	if err := db.QueryRowContext(t.Context(),
		`select count(*) from pragma_table_info('results') where name = ?`, column).Scan(&n); err != nil {
		t.Fatalf("reading the columns of the results table: %v", err)
	}

	return n > 0
}

// TestResultDocumentIsStoredInTheBucket is Story 8.2, AC1 and AC2: the index
// carries where the document is, how big it is and what it hashes to — and not
// the document, nor a copy of it kept "for now".
//
// It runs against every store, because "the document is in the bucket" is a
// promise the interface makes and not a property of a schema. That the SQL
// stores no longer keep a document column is asserted where the column was
// dropped, in TestUpgradingAStoreThatAlreadyHasRows.
func TestResultDocumentIsStoredInTheBucket(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	res := result("scan-1", time.Now(), model.ConsentReject)
	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	ref, size, digest := documentRefFor(t, opts.ArtifactDir, "scan-1")

	stored, err := os.ReadFile(artifactFile(t, opts.ArtifactDir, ref))
	if err != nil {
		t.Fatalf("the reference does not name a stored artifact: %v", err)
	}

	if int64(len(stored)) != size {
		t.Errorf("the artifact holds %d bytes, the index records %d", len(stored), size)
	}

	sum := sha256.Sum256(stored)
	if hex.EncodeToString(sum[:]) != digest {
		t.Errorf("the artifact hashes to %x, the index records %s", sum, digest)
	}

	// Content-addressed like every other artifact: the key is the digest.
	if want := "result/" + digest; ref != want {
		t.Errorf("artifact reference = %q, want %q", ref, want)
	}

	// And what is stored is the result document itself, not a reassembly of
	// columns: the JSON schema is the product's interface (Tenet 16).
	var decoded map[string]any

	if err := json.Unmarshal(stored, &decoded); err != nil {
		t.Fatalf("the stored artifact is not the result document: %v", err)
	}

	if decoded["schemaVersion"] != model.SchemaVersion || decoded["scanId"] != "scan-1" {
		t.Errorf("the stored artifact is not this scan's document: %v", decoded)
	}
}

// TestEveryResultRoundTripsThroughTheBucket is AC3: nothing outside the store
// learns that the document moved, so every result must come back exactly as it
// went in.
func TestEveryResultRoundTripsThroughTheBucket(t *testing.T) {
	t.Parallel()

	s := open(t)

	base := time.Now().Add(-time.Hour)

	failed := result("scan-failed", base, model.ConsentReject)
	failed.Termination = model.TermError
	failed.Error = "the browser crashed"

	big := result("scan-big", base.Add(time.Minute), model.ConsentReject)
	for i := range 500 {
		big.Requests = append(big.Requests, model.Request{
			RequestID: fmt.Sprintf("req-%d", i),
			URL:       fmt.Sprintf("https://third-party-%04d.example/asset/%d.js", i, i),
			Domain:    fmt.Sprintf("third-party-%04d.example", i),
			Party:     model.ThirdParty,
			Phase:     model.PhasePost,
		})
	}

	unicode := result("scan-unicode", base.Add(2*time.Minute), model.ConsentAccept)
	unicode.URL = "https://example.com/pfad/über-uns?emoji=🍪"

	labelled := result("scan-labelled", base.Add(3*time.Minute), model.ConsentNone)
	labelled.Labels = map[string]string{"team": "web", "tier": "1"}
	labelled.Warnings = []string{"a body could not be captured"}
	labelled.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: "screenshot-before-consent/" + strings.Repeat("ab", 32)}}

	for _, res := range []*model.Result{failed, big, unicode, labelled} {
		if err := s.PutResult(res); err != nil {
			t.Fatalf("PutResult %s: %v", res.ScanID, err)
		}
	}

	for _, res := range []*model.Result{failed, big, unicode, labelled} {
		got, err := s.GetResult(res.Target, res.ConsentMode, res.ScanID)
		if err != nil {
			t.Fatalf("GetResult %s: %v", res.ScanID, err)
		}

		want, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}

		back, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}

		if string(back) != string(want) {
			t.Errorf("%s did not round trip:\n stored %s\n   read %s", res.ScanID, want, back)
		}
	}
}

// TestDigestMismatchIsCorruptionNotEvidence is AC6. Once the payload is outside
// the database it is outside the database's guarantees, so a document that no
// longer matches what was written must be refused rather than returned.
func TestDigestMismatchIsCorruptionNotEvidence(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	ref, _, _ := documentRefFor(t, opts.ArtifactDir, "scan-1")
	path := artifactFile(t, opts.ArtifactDir, ref)

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The same number of bytes, so only the digest can catch it — a size check
	// alone would hand back a document that says something else.
	tampered := []byte(strings.Replace(string(stored),
		"https://tracker.test/px", "https://tracker.evil/px", 1))

	if len(tampered) != len(stored) {
		t.Fatalf("the tampered document is %d bytes, the original %d", len(tampered), len(stored))
	}

	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = s.GetResult("site", model.ConsentReject, "scan-1")

	if !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("reading a tampered document = %v, want ErrCorrupt", err)
	}

	if errors.Is(err, store.ErrNotFound) {
		t.Error("corruption was reported as absence: evidence that is present and wrong is not evidence that was pruned")
	}
}

// TestTruncatedDocumentIsCorruption covers the other half of the same check:
// an upload that stopped half way is still the wrong document.
func TestTruncatedDocumentIsCorruption(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	ref, _, _ := documentRefFor(t, opts.ArtifactDir, "scan-1")
	path := artifactFile(t, opts.ArtifactDir, ref)

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, stored[:len(stored)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-1"); !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("reading a truncated document = %v, want ErrCorrupt", err)
	}
}

// TestAMissingDocumentLeavesTheRestReadable is AC5: one lost document must not
// make a target's whole history unreadable.
func TestAMissingDocumentLeavesTheRestReadable(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	base := time.Now().Add(-time.Hour)

	for i := range 3 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	// Deleted behind the store's back, as a lifecycle rule or a stray operator
	// would delete it.
	ref, _, _ := documentRefFor(t, opts.ArtifactDir, "scan-1")
	if err := os.Remove(artifactFile(t, opts.ArtifactDir, ref)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("a missing document broke the listing: %v", err)
	}

	if len(got) != 3 {
		t.Errorf("got %d summaries, want 3: a missing document must not remove a result from the history", len(got))
	}

	// ErrEvidenceGone, and deliberately not ErrNotFound. The row is still in
	// the index, so "there is no such scan" would be untrue — and every caller
	// that skips a result which does not exist would skip lost evidence just as
	// quietly (Story 8.2, AC5).
	_, err = s.GetResult("site", model.ConsentReject, "scan-1")
	if !errors.Is(err, store.ErrEvidenceGone) {
		t.Errorf("reading the missing document = %v, want ErrEvidenceGone: absence is recorded, not inferred", err)
	}

	if errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the missing document also reports ErrNotFound (%v), "+
			"which is how a caller comes to treat deleted evidence as a scan that never happened", err)
	}

	for _, scanID := range []string{"scan-0", "scan-2"} {
		if _, err := s.GetResult("site", model.ConsentReject, scanID); err != nil {
			t.Errorf("%s became unreadable because another result's document is gone: %v", scanID, err)
		}
	}
}

// --- Story 8.3: listing without fetching a document ------------------------

// TestListingNeedsNoDocumentAtAll is AC2 from the outside: with every document
// removed from the bucket, a listing must still be complete and correct,
// because it never reads one. (The bucket-operation count itself is asserted in
// the white-box test, which can see the bucket.)
//
// It runs against the bucket-index store too, and it is the criterion that
// survives being carried there. "A listing performs zero bucket reads" cannot
// hold where the index is the bucket; what has to hold, and what this asserts,
// is that a listing never fetches the megabytes a scan's document runs to
// (Story 8.10, §6.4).
func TestListingNeedsNoDocumentAtAll(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	base := time.Now().Add(-time.Hour)

	for i := range 3 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), base.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.RemoveAll(filepath.Join(opts.ArtifactDir, "result")); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("listing read the documents it must not read: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d summaries, want 3", len(got))
	}

	for _, sm := range got {
		if sm.Requests != 1 || sm.ThirdPartyDomains != 1 || sm.PreConsentDomains != 1 {
			t.Errorf("%s: summary is not the one that was written: %+v", sm.ScanID, sm)
		}

		if sm.Error != "" {
			t.Errorf("%s: a stored summary was reported as underived: %q", sm.ScanID, sm.Error)
		}
	}

	// The two other read paths that must not need a document either.
	if ok, err := s.HasResult("site", model.ConsentReject, "scan-1"); err != nil || !ok {
		t.Errorf("HasResult = %v, %v: it is a row lookup", ok, err)
	}

	if series, err := s.Series(); err != nil || len(series) != 1 {
		t.Errorf("Series = %v, %v", series, err)
	}
}

// TestSummaryColumnsAreWhatSummarizeProduced is AC4: the columns are written by
// the same derivation the interface renders, so a listing and the document it
// points at cannot disagree.
func TestSummaryColumnsAreWhatSummarizeProduced(t *testing.T) {
	t.Parallel()

	s := open(t)

	at := time.Now().Add(-time.Hour)

	res := result("scan-1", at, model.ConsentReject)
	res.Duration = 1500 * time.Millisecond
	res.Termination = model.TermRequestCap
	res.Error = "hit the request cap"
	res.Consent = model.Consent{Outcome: model.OutcomeUnverified, CMP: "cookiebot"}
	res.Requests = append(res.Requests, []model.Request{
		{
			URL: "https://tracker.test/second.js", Domain: "tracker.test",
			Party: model.ThirdParty, Phase: model.PhasePost,
		},
		{
			URL: "https://analytics.test/a.js", Domain: "analytics.test",
			Party: model.ThirdParty, Phase: model.PhasePost,
		},
		{
			URL: "https://example.com/app.js", Domain: "example.com",
			Party: model.FirstParty, Phase: model.PhasePre,
		},
	}...)

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}

	sm := got[0]

	for _, tc := range []struct {
		field string
		got   any
		want  any
	}{
		{"ScanID", sm.ScanID, "scan-1"},
		{"Target", sm.Target, "site"},
		{"ConsentMode", sm.ConsentMode, model.ConsentReject},
		{"Duration", sm.Duration, res.Duration},
		{"Termination", sm.Termination, model.TermRequestCap},
		{"Error", sm.Error, "hit the request cap"},
		{"ConsentOutcome", sm.ConsentOutcome, model.OutcomeUnverified},
		{"ConsentCMP", sm.ConsentCMP, "cookiebot"},
		{"Requests", sm.Requests, len(res.Requests)},
		{"ThirdPartyDomains", sm.ThirdPartyDomains, 2},
		{"PreConsentDomains", sm.PreConsentDomains, 1},
	} {
		if tc.got != tc.want {
			t.Errorf("summary %s = %v, want %v", tc.field, tc.got, tc.want)
		}
	}

	// The instant survives even though the offset is not stored: the ordering
	// every listing depends on is the instant.
	if !sm.StartedAt.Equal(at) {
		t.Errorf("StartedAt = %s, want %s", sm.StartedAt, at)
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

	_, err := store.OpenSQL(t.Context(), store.Options{Path: filepath.Join(blocker, "nested", "wsaw.db")})
	if err == nil {
		t.Fatal("OpenSQL succeeded against an unusable path")
	}
}

// --- Story 4.7: the same behaviour on every dialect -----------------------
//
// These run against whichever driver the suite is configured for, because
// each of them is a place where a database's defaults would otherwise change
// what wsaw records — quietly, which is the worst way to lose evidence.

// TestTargetNamesAreCaseSensitive guards against a collation that folds case.
// MySQL's default collation is case- and accent-insensitive: under it,
// "Site" and "site" would be one series, which neither SQLite nor PostgreSQL
// would do. Evidence must not depend on which database it landed in.
func TestTargetNamesAreCaseSensitive(t *testing.T) {
	t.Parallel()

	s := open(t)

	at := time.Now()

	upper := result("scan-upper", at, model.ConsentReject)
	upper.Target = "Site"

	lower := result("scan-lower", at, model.ConsentReject)
	lower.Target = "site"

	for _, res := range []*model.Result{upper, lower} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	series, err := s.Series()
	if err != nil {
		t.Fatal(err)
	}

	if len(series) != 2 {
		t.Fatalf("got %d series for targets \"Site\" and \"site\", want 2: the store folded case", len(series))
	}

	got, err := s.ListResults("Site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].ScanID != "scan-upper" {
		t.Errorf("listing \"Site\" returned %+v, want only its own scan", got)
	}
}

// TestLargeDocumentSurvives is why MySQL needs longtext: TEXT holds 64 KiB
// and a result for a real page exceeds that. A store that truncated it would
// return a document that no longer parses, having reported success.
func TestLargeDocumentSurvives(t *testing.T) {
	t.Parallel()

	s := open(t)

	res := result("scan-big", time.Now(), model.ConsentReject)

	// Enough requests to push the encoded document well past 64 KiB.
	for i := range 2000 {
		res.Requests = append(res.Requests, model.Request{
			RequestID:    fmt.Sprintf("req-%d", i),
			URL:          fmt.Sprintf("https://third-party-%04d.example/asset/%d.js", i, i),
			Host:         fmt.Sprintf("third-party-%04d.example", i),
			Domain:       fmt.Sprintf("third-party-%04d.example", i),
			ResourceType: "script",
			Party:        model.ThirdParty,
			Phase:        model.PhasePre,
		})
	}

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-big")
	if err != nil {
		t.Fatalf("a large result could not be read back: %v", err)
	}

	if len(got.Requests) != len(res.Requests) {
		t.Errorf("read back %d requests, stored %d: the document was truncated",
			len(got.Requests), len(res.Requests))
	}
}

// TestUnicodeSurvivesTheRoundTrip is why the MySQL tables are utf8mb4: a
// captured URL contains whatever the page put in it, including four-byte
// characters, and a three-byte character set would refuse or mangle them.
func TestUnicodeSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)

	const tricky = "https://example.com/pfad/über-uns?emoji=🍪&kanji=日本語"

	res := result("scan-unicode", time.Now(), model.ConsentReject)
	res.URL = tricky
	res.Requests = []model.Request{{RequestID: "req-1", URL: tricky, Host: "example.com"}}

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-unicode")
	if err != nil {
		t.Fatal(err)
	}

	if got.URL != tricky {
		t.Errorf("URL read back as %q, want %q", got.URL, tricky)
	}

	if len(got.Requests) != 1 || got.Requests[0].URL != tricky {
		t.Errorf("request URL did not survive: %+v", got.Requests)
	}
}

// TestOrderingSurvivesSubMillisecondStarts is why start times are stored as
// an integer count of nanoseconds rather than as a timestamp column. MySQL's
// DATETIME drops fractional seconds by default, and "the scan before this
// one" is decided by this ordering.
func TestOrderingSurvivesSubMillisecondStarts(t *testing.T) {
	t.Parallel()

	s := open(t)

	base := time.Now()

	for i := range 3 {
		// One microsecond apart: below the resolution of a default DATETIME,
		// above nothing at all.
		if err := s.PutResult(result(
			fmt.Sprintf("scan-%d", i),
			base.Add(time.Duration(i)*time.Microsecond),
			model.ConsentReject,
		)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}

	for i, want := range []string{"scan-2", "scan-1", "scan-0"} {
		if got[i].ScanID != want {
			t.Fatalf("order = %s at position %d, want %s: sub-millisecond start times were flattened",
				got[i].ScanID, i, want)
		}
	}

	prev, err := s.PreviousResult("site", model.ConsentReject, "scan-2")
	if err != nil {
		t.Fatal(err)
	}

	if prev.ScanID != "scan-1" {
		t.Errorf("the scan before scan-2 is %s, want scan-1", prev.ScanID)
	}
}

// TestScansThatShareAnInstantAreStillOrderedDeterministically is the tie-break
// every store owes the rest of wsaw.
//
// Two scans of one target can start in the same nanosecond — a coarse timer, a
// clock that was stepped, or two hosts that disagree — and the history still has
// to have one order, the same one on every read and the same one for every kind
// of store. It is the scan ID, descending, which is what makes AC5's promise
// hold: a wrong clock may put an entry in an unexpected position and can never
// make one scan hide or overwrite another.
func TestScansThatShareAnInstantAreStillOrderedDeterministically(t *testing.T) {
	t.Parallel()

	s := open(t)

	at := time.Now().Add(-time.Hour)

	// Stored out of order, so that nothing about the answer can come from the
	// order they were written in.
	for _, id := range []string{"scan-b", "scan-a", "scan-c"} {
		if err := s.PutResult(result(id, at, model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, sm := range got {
		order = append(order, sm.ScanID)
	}

	want := []string{"scan-c", "scan-b", "scan-a"}
	if !slices.Equal(order, want) {
		t.Errorf("the history reads %v, want %v", order, want)
	}

	latest, err := s.LatestResult("site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	if latest.ScanID != "scan-c" {
		t.Errorf("LatestResult = %s, want scan-c", latest.ScanID)
	}

	// And the same rule read the other way round: the scan before one of a tied
	// run is the next one down, not the whole run skipped and not itself.
	prev, err := s.PreviousResult("site", model.ConsentReject, "scan-c")
	if err != nil {
		t.Fatal(err)
	}

	if prev.ScanID != "scan-b" {
		t.Errorf("the scan before scan-c is %s, want scan-b", prev.ScanID)
	}

	if _, err := s.PreviousResult("site", model.ConsentReject, "scan-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviousResult before the lowest of a tied run = %v, want ErrNotFound", err)
	}
}

// TestDriverIsReported keeps the suite honest: a run that believes it is
// testing PostgreSQL while quietly using SQLite proves nothing.
func TestDriverIsReported(t *testing.T) {
	t.Parallel()

	s := open(t)

	want := os.Getenv("WSAW_TEST_STORE_DRIVER")
	if want == "" {
		want = store.DriverSQLite
	}

	if got := s.Driver(); got != want {
		t.Errorf("store driver = %q, want %q", got, want)
	}
}

// --- Story 3.8: a failed scan is not a comparison baseline ----------------

// TestPreviousResultSkipsFailedScans is AC6. Without it, a scan that
// succeeded after a retry would be compared against the failure it replaced,
// and the diff would report the whole site as new — a finding manufactured
// out of wsaw's own recovery.
func TestPreviousResultSkipsFailedScans(t *testing.T) {
	t.Parallel()

	s := open(t)

	base := time.Now().Add(-time.Hour)

	good := result("scan-good", base, model.ConsentReject)

	failed := result("scan-failed", base.Add(time.Minute), model.ConsentReject)
	failed.Termination = model.TermError
	failed.Error = "the browser crashed"

	skipped := result("scan-skipped", base.Add(2*time.Minute), model.ConsentReject)
	skipped.Termination = model.TermSkipped
	skipped.Error = "disallowed by robots.txt"

	retried := result("scan-retried", base.Add(3*time.Minute), model.ConsentReject)

	for _, res := range []*model.Result{good, failed, skipped, retried} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	prev, err := s.PreviousResult("site", model.ConsentReject, "scan-retried")
	if err != nil {
		t.Fatal(err)
	}

	if prev.ScanID != "scan-good" {
		t.Errorf("the scan before scan-retried is %s, want scan-good: a failed or skipped scan is not an observation of the site",
			prev.ScanID)
	}

	// The failures are still there. A retry must not be a way to make a bad
	// scan disappear (Tenet 5).
	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != 4 {
		t.Errorf("history holds %d results, want all 4 including the failures", len(summaries))
	}
}

// A truncated scan is still an observation — it saw real assets, just not all
// of them — so it remains a usable baseline. Only failures and skips do not.
func TestPreviousResultKeepsTruncatedScans(t *testing.T) {
	t.Parallel()

	s := open(t)

	base := time.Now().Add(-time.Hour)

	truncated := result("scan-truncated", base, model.ConsentReject)
	truncated.Termination = model.TermTimeout

	current := result("scan-current", base.Add(time.Minute), model.ConsentReject)

	for _, res := range []*model.Result{truncated, current} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	prev, err := s.PreviousResult("site", model.ConsentReject, "scan-current")
	if err != nil {
		t.Fatal(err)
	}

	if prev.ScanID != "scan-truncated" {
		t.Errorf("the scan before scan-current is %s, want scan-truncated", prev.ScanID)
	}
}

// With nothing but failures behind it, there is no baseline — and that has to
// read as "nothing to compare against" rather than as an empty site.
func TestPreviousResultReportsNothingWhenOnlyFailuresPrecede(t *testing.T) {
	t.Parallel()

	s := open(t)

	base := time.Now().Add(-time.Hour)

	failed := result("scan-failed", base, model.ConsentReject)
	failed.Termination = model.TermError
	failed.Error = "the browser crashed"

	current := result("scan-current", base.Add(time.Minute), model.ConsentReject)

	for _, res := range []*model.Result{failed, current} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.PreviousResult("site", model.ConsentReject, "scan-current"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviousResult = %v, want ErrNotFound", err)
	}
}
