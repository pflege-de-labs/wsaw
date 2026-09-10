//go:build e2e

// Package e2e drives wsaw as a deployed system rather than a single process:
// its own container, a server database in another, and the Klaro fixture in
// a third (Epic 7). Everything the fast suite proves, it proves in one
// process; what only exists between processes — a browser reaching another
// container, a database connection that can drop, a consent banner served
// over a real origin — is this package's business instead.
//
// These tests need test/e2e/compose.yaml already running with a database
// picked; they do not start it themselves. `make e2e-postgres-test` /
// `make e2e-mysql-test` bring the stack up, run this package against it,
// dump every service's logs on failure, and always tear the stack down
// (Story 7.4, AC8) — the make target is "the test" as much as this file is.
// Run directly with `go test -tags e2e`, the environment variables below
// are missing and every test skips with that explanation rather than
// failing to dial a stack that was never brought up.
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// TestPostgresStack is Story 7.4: everything below runs against whatever
// database `runStack` is told to use, so Story 7.5's MySQL entry point is
// one more call to the same body rather than a second copy of it that would
// drift (Story 7.5, AC1).
func TestPostgresStack(t *testing.T) {
	runStack(t, "postgres", "pgx")
}

// TestMySQLStack is Story 7.5: the same body as TestPostgresStack, plus the
// dialect differences MySQL specifically forces (AC2-AC5) — those have no
// Postgres equivalent, so they are not part of the shared body.
func TestMySQLStack(t *testing.T) {
	e := runStack(t, "mysql", "mysql")

	t.Run("mysql: charset and collation are pinned to utf8mb4", func(t *testing.T) { testMySQLCharset(t, e) })
	t.Run("mysql: strict sql_mode refuses an over-long value rather than truncating it",
		func(t *testing.T) { testMySQLStrictMode(t, e) })
	t.Run("mysql: a timestamp survives the round trip at the precision ordering needs",
		func(t *testing.T) { testMySQLTimestampPrecision(t, e) })
	t.Run("mysql: approving a second baseline exercises the upsert form",
		func(t *testing.T) { testMySQLUpsert(t, e) })
}

// stackEnv is what every subtest needs to reach the running stack: the API,
// direct SQL against the database, the fixture's variant switch, and enough
// of the Compose invocation that started it to stop and restart one service.
type stackEnv struct {
	dialect       string // "postgres" or "mysql"
	sqlDriverName string
	apiBase       string
	token         string
	siteBase      string
	compose       string
	db            *sql.DB
	dsn           string
}

func runStack(t *testing.T, dialect, sqlDriverName string) *stackEnv { //nolint:thelper // this is the test body, not an assertion helper: failures belong to its own lines
	apiBase := os.Getenv("WSAW_E2E_API_BASE")
	if apiBase == "" {
		t.Skipf("WSAW_E2E_API_BASE is not set; run `make e2e-%s-test`, which brings the stack up and sets it, rather than `go test` directly", dialect)
	}

	e := &stackEnv{
		dialect:       dialect,
		sqlDriverName: sqlDriverName,
		apiBase:       apiBase,
		token:         requireEnv(t, "WSAW_E2E_API_TOKEN"),
		siteBase:      requireEnv(t, "WSAW_E2E_SITE_BASE"),
		compose:       requireEnv(t, "WSAW_E2E_COMPOSE_CMD"),
		dsn:           requireEnv(t, "WSAW_E2E_DSN"),
	}

	db, err := sql.Open(sqlDriverName, e.dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", dialect, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("connecting directly to the %s the stack is using: %v", dialect, err)
	}

	e.db = db

	// AC5: the schema is wsaw's own, created against an empty database by
	// its own migrations — not a migration tool, and not created out of
	// band by this test.
	t.Run("the schema was created by wsaw's own migrations", func(t *testing.T) { testSchemaCreated(t, e) })

	// AC2 and AC3, one consent mode at a time. Each uses its own mode so
	// their result counts do not interfere with each other or with the
	// later subtests that scan "fixture" again.
	t.Run("none: the banner is present and untouched", func(t *testing.T) { testConsentNone(t, e) })
	t.Run("reject: the banner is dismissed and the blocked third party stays absent",
		func(t *testing.T) { testConsentReject(t, e) })
	t.Run("accept: it appears", func(t *testing.T) { testConsentAccept(t, e) })

	// AC4.
	t.Run("two scans of an unchanged fixture produce an empty diff", func(t *testing.T) { testEmptyDiff(t, e) })
	t.Run("a changed fixture produces exactly the expected changes", func(t *testing.T) { testChangedDiff(t, e) })

	// AC7.
	t.Run("baseline approval and its audit entry are atomic", func(t *testing.T) { testBaselineAtomic(t, e) })

	// AC6.
	t.Run("the database can restart across a scan, and the daemon survives it",
		func(t *testing.T) { testDatabaseRestart(t, e) })

	// AC5, second half.
	t.Run("restarting wsaw against the same database is a no-op", func(t *testing.T) { testDaemonRestart(t, e) })

	return e
}

func testSchemaCreated(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	if v := e.schemaVersion(t); v < 1 {
		t.Fatalf("schema version = %d, want at least 1", v)
	}
}

func testConsentNone(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	res, _ := e.scan(t, "fixture", model.ConsentNone)

	if res.Consent.Outcome != model.OutcomeNotNeeded {
		t.Errorf("consent outcome = %q, want %q — a mode that asks for no interaction must not report one",
			res.Consent.Outcome, model.OutcomeNotNeeded)
	}

	assertUnconditionalThirdPartyFiredPreConsent(t, res)
	assertRequestAbsent(t, res, "tracker", "/analytics.js")
	assertRequestAbsent(t, res, "tracker", "/collect")
}

func testConsentReject(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	res, _ := e.scan(t, "fixture", model.ConsentReject)

	assertKlaroApplied(t, res)
	assertUnconditionalThirdPartyFiredPreConsent(t, res)

	// Klaro's own state, not just wsaw's report of it: the consent-gated
	// script must never appear, in either phase, or Klaro did not actually
	// hold it back (AC2).
	assertRequestAbsent(t, res, "tracker", "/analytics.js")
	assertRequestAbsent(t, res, "tracker", "/collect")
}

func testConsentAccept(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	res, _ := e.scan(t, "fixture", model.ConsentAccept)

	assertKlaroApplied(t, res)
	assertUnconditionalThirdPartyFiredPreConsent(t, res)

	post := findRequest(res, "tracker", "/analytics.js")
	if post == nil {
		t.Fatal("the consent-gated script never fired — a scan that clicked nothing and a scan that clicked correctly would both pass this way")
	}

	if post.Phase != model.PhasePost || post.Party != model.ThirdParty {
		t.Errorf("analytics.js: phase=%q party=%q, want post-interaction third-party", post.Phase, post.Party)
	}

	if findRequest(res, "tracker", "/collect") == nil {
		t.Error("the analytics beacon (/collect) never fired even though its script did")
	}
}

// testEmptyDiff is AC4's first half: an unchanged fixture must diff as
// nothing at all.
func testEmptyDiff(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	e.scan(t, "fixture", model.ConsentNone) // establishes a baseline to compare the next one against

	_, d := e.scan(t, "fixture", model.ConsentNone)

	if !d.Comparable {
		t.Fatalf("not comparable: %s", d.Reason)
	}

	if len(d.Changes) != 0 {
		t.Errorf("changes = %+v, want none — a non-empty diff here is either a real change or a determinism bug (Story 7.1, AC4)", d.Changes)
	}
}

// testChangedDiff is AC4's second half: a deliberately changed fixture must
// diff as exactly that change, at the severity the product's own rules
// assign it — not a hand-built expectation, but the real diff engine's real
// output.
func testChangedDiff(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	e.setVariant(t, "changed")
	t.Cleanup(func() { e.setVariant(t, "base") })

	_, d := e.scan(t, "fixture", model.ConsentNone)

	if !d.Comparable {
		t.Fatalf("not comparable: %s", d.Reason)
	}

	var sawNewHost, sawNewAsset, sawScriptChanged bool

	for _, c := range d.Changes {
		switch {
		case c.Type == diff.HostAdded && c.Domain == "extra-tracker":
			sawNewHost = true

			if c.Party != model.ThirdParty || c.Phase != model.PhasePre {
				t.Errorf("host-added extra-tracker: party=%q phase=%q, want third-party pre-interaction", c.Party, c.Phase)
			}

			if c.Severity.Rank() < diff.SeverityHigh.Rank() {
				t.Errorf("host-added extra-tracker: severity=%q, want at least %q — this is the headline finding", c.Severity, diff.SeverityHigh)
			}

		case c.Type == diff.AssetAdded && c.Domain == "extra-tracker":
			sawNewAsset = true

		case c.Type == diff.ScriptChanged && strings.HasSuffix(c.Subject, "/assets/app.js"):
			sawScriptChanged = true

			if c.Before == "" || c.After == "" || c.Before == c.After {
				t.Errorf("script-changed app.js: before=%q after=%q, want two different digests", c.Before, c.After)
			}

		default:
			t.Errorf("unexpected change, not one of the two the fixture's changed variant makes: %+v", c)
		}
	}

	if !sawNewHost {
		t.Error("no host-added change for the new third-party host")
	}

	if !sawNewAsset {
		t.Error("no asset-added change for the new host's asset")
	}

	if !sawScriptChanged {
		t.Error("no script-changed change for the rewritten first-party script")
	}
}

// testBaselineAtomic is AC7: a baseline approval and its audit entry, read
// back with SQL — not re-proving the transaction internal/store already
// unit-tests, but confirming the same guarantee holds over the real network
// connection a server database is.
func testBaselineAtomic(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	res, _ := e.scan(t, "fixture", model.ConsentAccept)

	e.approveBaseline(t, "fixture", model.ConsentAccept, res.ScanID)

	var baselineScanID string

	row := e.db.QueryRowContext(context.Background(),
		e.rebind("select scan_id from baselines where target = ? and consent_mode = ?"),
		"fixture", string(model.ConsentAccept))
	if err := row.Scan(&baselineScanID); err != nil {
		t.Fatalf("reading the baseline row: %v", err)
	}

	if baselineScanID != res.ScanID {
		t.Errorf("baseline scan_id = %q, want %q", baselineScanID, res.ScanID)
	}

	var auditDoc string

	row = e.db.QueryRowContext(context.Background(), "select document from audit order by id desc limit 1")
	if err := row.Scan(&auditDoc); err != nil {
		t.Fatalf("reading the latest audit row: %v", err)
	}

	var entry struct {
		Action      string `json:"action"`
		Target      string `json:"target"`
		ConsentMode string `json:"consentMode"`
		Subject     string `json:"subject"`
	}
	if err := json.Unmarshal([]byte(auditDoc), &entry); err != nil {
		t.Fatalf("decoding the audit entry: %v", err)
	}

	if entry.Action != "baseline-approved" || entry.Target != "fixture" ||
		entry.ConsentMode != string(model.ConsentAccept) || entry.Subject != res.ScanID {
		t.Errorf("audit entry %+v does not describe the baseline approval it should be paired with", entry)
	}
}

// testDatabaseRestart is AC6. The scan itself does not touch the database —
// only persisting its result does — so the database is stopped before
// triggering the scan and held down through it, which makes the outcome
// deterministic rather than a race against exactly when a "restart"
// finishes.
func testDatabaseRestart(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	before := e.countResults(t, "fixture", model.ConsentReject)

	e.composeRun(t, "stop", "db")
	e.waitReady(t, false, 30*time.Second)

	res, _ := e.scan(t, "fixture", model.ConsentReject) // still 200: the capture itself never touches the database

	e.composeRun(t, "start", "db")
	e.waitReady(t, true, 60*time.Second)

	if _, ok := e.resultDocument(t, "fixture", model.ConsentReject, res.ScanID); ok {
		t.Error("a result persisted despite the database being down for the whole scan — the retry budget should have been exhausted first")
	}

	if got := e.countResults(t, "fixture", model.ConsentReject); got != before {
		t.Errorf("result count = %d, want unchanged at %d — the failed attempt must not have left a partial or phantom row", got, before)
	}

	if logs := e.composeRun(t, "logs", "wsaw"); !strings.Contains(logs, "storing result failed") {
		t.Error("wsaw's own log does not record that persisting the scan failed — the failure has to be visible to an operator somewhere, or it is not recorded at all (Story 7.4, AC6)")
	}

	res2, _ := e.scan(t, "fixture", model.ConsentReject)

	if _, ok := e.resultDocument(t, "fixture", model.ConsentReject, res2.ScanID); !ok {
		t.Error("the next scan after the database came back was not persisted — the daemon did not reconnect")
	}
}

// testDaemonRestart is AC5's second half: restarting wsaw against the same,
// already-populated database must be a no-op — no re-run migration
// duplicates the schema version row, and nothing already stored is
// disturbed.
func testDaemonRestart(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	before := map[model.ConsentMode]int{
		model.ConsentNone:   e.countResults(t, "fixture", model.ConsentNone),
		model.ConsentReject: e.countResults(t, "fixture", model.ConsentReject),
		model.ConsentAccept: e.countResults(t, "fixture", model.ConsentAccept),
	}

	e.composeRun(t, "restart", "wsaw")
	e.waitReady(t, true, 60*time.Second)

	if v := e.schemaVersion(t); v < 1 {
		t.Errorf("schema version after restart = %d, want at least 1", v)
	}

	var versionRows int
	if err := e.db.QueryRowContext(context.Background(), "select count(*) from wsaw_schema_version").Scan(&versionRows); err != nil {
		t.Fatalf("counting schema version rows: %v", err)
	}

	if versionRows != 1 {
		t.Errorf("wsaw_schema_version has %d rows, want exactly 1 — a re-run migration must update it in place, not insert beside it", versionRows)
	}

	for mode, want := range before {
		if got := e.countResults(t, "fixture", mode); got != want {
			t.Errorf("%s result count after restart = %d, want unchanged at %d", mode, got, want)
		}
	}
}

// testMySQLCharset is AC3: MySQL's default collation is case-insensitive and
// accent-insensitive, which would silently merge two differently-cased
// target names into one series. This confirms the schema wsaw's own
// migrations created on this live server actually pins utf8mb4_bin rather
// than assuming the dialect code that builds the DDL is enough on its own.
func testMySQLCharset(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	var table, ddl string
	if err := e.db.QueryRowContext(context.Background(), "show create table results").Scan(&table, &ddl); err != nil {
		t.Fatalf("reading the results table's definition: %v", err)
	}

	if !strings.Contains(ddl, "utf8mb4") {
		t.Errorf("results table charset: %s does not mention utf8mb4", ddl)
	}

	if !strings.Contains(ddl, "utf8mb4_bin") {
		t.Errorf("results table collation: %s is not pinned to utf8mb4_bin — a case-insensitive default would make \"Site\" and \"site\" one series", ddl)
	}
}

// testMySQLStrictMode is AC4: without STRICT_ALL_TABLES, MySQL truncates a
// value that overflows its column and reports success — which would shorten
// a captured URL and change the finding rather than fail loudly. wsaw's own
// DSN always appends it (internal/store's mysqlDialect.dsn, unit-tested
// there); this proves the live server actually honours it, using a second
// connection built the same way and a value in a real column, rolled back
// regardless of outcome so it never touches what the other subtests count.
func testMySQLStrictMode(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	sep := "?"
	if strings.Contains(e.dsn, "?") {
		sep = "&"
	}

	strictDB, err := sql.Open(e.sqlDriverName, e.dsn+sep+"sql_mode='STRICT_ALL_TABLES'")
	if err != nil {
		t.Fatalf("opening a second connection: %v", err)
	}
	defer func() { _ = strictDB.Close() }()

	tx, err := strictDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }() // always: this insert must never be the reason a later subtest's count is wrong

	// consent_mode is varchar(32); this is longer.
	overLong := strings.Repeat("x", 64)

	_, err = tx.ExecContext(context.Background(),
		"insert into results (target, consent_mode, scan_id, started_at, termination, document) values (?, ?, ?, ?, ?, ?)",
		"e2e-strict-mode-probe", overLong, "probe", int64(1), "idle", "{}")

	if err == nil {
		t.Error("an over-long consent_mode was accepted — strict mode should have refused it rather than truncating it silently")
	}
}

// testMySQLTimestampPrecision is AC5: MySQL's default DATETIME drops
// fractional seconds, and "the scan before this one" is decided by that
// ordering. wsaw sidesteps DATETIME entirely — started_at is a bigint count
// of nanoseconds, the same as every other dialect — so this confirms that
// choice actually preserves sub-second precision on a live server rather
// than assuming the column type in the migration is enough on its own.
func testMySQLTimestampPrecision(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	res, _ := e.scan(t, "fixture", model.ConsentNone)

	var startedAt int64
	if err := e.db.QueryRowContext(context.Background(),
		"select started_at from results where target = 'fixture' and consent_mode = 'none' and scan_id = ?",
		res.ScanID).Scan(&startedAt); err != nil {
		t.Fatalf("reading started_at for %s: %v", res.ScanID, err)
	}

	if startedAt%1_000_000_000 == 0 {
		t.Errorf("started_at = %d, a whole number of seconds — sub-second precision did not survive the round trip", startedAt)
	}
}

// testMySQLUpsert is AC2's upsert form: approving a baseline for a target
// and consent mode that already has one must overwrite it, which is
// MySQL's "insert ... as new on duplicate key update" rather than the
// portable form the other two dialects use. The row-level mechanics of that
// SQL are internal/store's own dialect tests to prove (test-store-mysql,
// against a real MySQL container already); this confirms the same
// guarantee holds through the real API and a real baseline approval rather
// than a hand-built query.
func testMySQLUpsert(t *testing.T, e *stackEnv) { //nolint:thelper // extracted subtest body, not an assertion helper
	first, _ := e.scan(t, "fixture", model.ConsentAccept)
	e.approveBaseline(t, "fixture", model.ConsentAccept, first.ScanID)

	second, _ := e.scan(t, "fixture", model.ConsentAccept)
	e.approveBaseline(t, "fixture", model.ConsentAccept, second.ScanID)

	var got string

	row := e.db.QueryRowContext(context.Background(),
		e.rebind("select scan_id from baselines where target = ? and consent_mode = ?"),
		"fixture", string(model.ConsentAccept))
	if err := row.Scan(&got); err != nil {
		t.Fatalf("reading the baseline row: %v", err)
	}

	if got != second.ScanID {
		t.Errorf("baseline scan_id = %q after a second approval, want %q — the second approval should have overwritten the first, not left it or duplicated the row", got, second.ScanID)
	}

	var rows int
	if err := e.db.QueryRowContext(context.Background(),
		e.rebind("select count(*) from baselines where target = ? and consent_mode = ?"),
		"fixture", string(model.ConsentAccept)).Scan(&rows); err != nil {
		t.Fatalf("counting baseline rows: %v", err)
	}

	if rows != 1 {
		t.Errorf("baselines has %d rows for fixture/accept, want exactly 1", rows)
	}
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()

	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set; this test runs through `make e2e-*-test`, which sets it", name)
	}

	return v
}

// rebind converts a query written with ? placeholders into this dialect's
// form, the same job internal/store's own dialect seam does — this test
// reads the database independently of that package, so it needs its own,
// much smaller version of the same translation.
func (e *stackEnv) rebind(query string) string {
	if e.dialect != "postgres" {
		return query
	}

	var b strings.Builder

	n := 0

	for _, r := range query {
		if r == '?' {
			n++

			b.WriteString("$" + strconv.Itoa(n))

			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}

func (e *stackEnv) schemaVersion(t *testing.T) int {
	t.Helper()

	var v int
	if err := e.db.QueryRowContext(context.Background(),
		"select version from wsaw_schema_version where id = 1").Scan(&v); err != nil {
		t.Fatalf("reading the schema version: %v", err)
	}

	return v
}

func (e *stackEnv) countResults(t *testing.T, target string, mode model.ConsentMode) int {
	t.Helper()

	var n int
	if err := e.db.QueryRowContext(context.Background(),
		e.rebind("select count(*) from results where target = ? and consent_mode = ?"),
		target, string(mode)).Scan(&n); err != nil {
		t.Fatalf("counting results for %s/%s: %v", target, mode, err)
	}

	return n
}

func (e *stackEnv) resultDocument(t *testing.T, target string, mode model.ConsentMode, scanID string) (model.Result, bool) {
	t.Helper()

	var doc string

	err := e.db.QueryRowContext(context.Background(),
		e.rebind("select document from results where target = ? and consent_mode = ? and scan_id = ?"),
		target, string(mode), scanID).Scan(&doc)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.Result{}, false
	case err != nil:
		t.Fatalf("reading result %s/%s/%s: %v", target, mode, scanID, err)
	}

	var r model.Result
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		t.Fatalf("decoding stored result %s: %v", scanID, err)
	}

	return r, true
}

func (e *stackEnv) approveBaseline(t *testing.T, target string, mode model.ConsentMode, scanID string) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"scanId": scanID})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/api/v1/baseline/%s/%s", e.apiBase, target, mode), strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("approving baseline: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("approving baseline %s/%s/%s: %d: %s", target, mode, scanID, resp.StatusCode, respBody)
	}
}

// scan triggers a real scan through wsaw's own API — the same path an
// operator or a schedule would use — and returns the decoded result and
// diff. It never reads the database: that is every subtest's own job,
// through e.resultDocument, which is the whole point of an end-to-end test
// over a unit one (AC1).
func (e *stackEnv) scan(t *testing.T, target string, mode model.ConsentMode) (*model.Result, *diff.Report) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/api/v1/scan/%s/%s", e.apiBase, target, mode), nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("triggering a scan of %s/%s: %v", target, mode, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scanning %s/%s: %d: %s", target, mode, resp.StatusCode, body)
	}

	var out struct {
		Result *model.Result `json:"result"`
		Diff   *diff.Report  `json:"diff"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the scan response for %s/%s: %v\n%s", target, mode, err, body)
	}

	return out.Result, out.Diff
}

func (e *stackEnv) setVariant(t *testing.T, variant string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut,
		e.siteBase+"/__fixture/variant", strings.NewReader(variant))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("switching the fixture to the %s variant: %v", variant, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("switching the fixture to the %s variant: %d: %s", variant, resp.StatusCode, body)
	}
}

func (e *stackEnv) ready(t *testing.T) bool {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, e.apiBase+"/api/v1/ready", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Ready bool `json:"ready"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	return out.Ready
}

func (e *stackEnv) waitReady(t *testing.T, want bool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if e.ready(t) == want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("wsaw's readiness never reached %v within %s", want, timeout)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// composeRun drives the same Compose invocation that started the stack
// (WSAW_E2E_COMPOSE_CMD, built by the Makefile from the variables that
// brought the stack up), rather than this test guessing container names —
// which differ between podman-compose and Docker Compose (Story 7.3 found
// exactly this difference once already).
func (e *stackEnv) composeRun(t *testing.T, args ...string) string {
	t.Helper()

	line := e.compose + " " + strings.Join(args, " ")

	out, err := exec.CommandContext(context.Background(), "sh", "-c", line).CombinedOutput()
	if err != nil {
		t.Fatalf("running `%s`: %v\n%s", line, err, out)
	}

	return string(out)
}

func findRequest(res *model.Result, host, pathSuffix string) *model.Request {
	for i, r := range res.Requests {
		if r.Host == host && strings.HasSuffix(strings.SplitN(r.URL, "?", 2)[0], pathSuffix) {
			return &res.Requests[i]
		}
	}

	return nil
}

func assertRequestAbsent(t *testing.T, res *model.Result, host, pathSuffix string) {
	t.Helper()

	if r := findRequest(res, host, pathSuffix); r != nil {
		t.Errorf("%s%s fired even though it should have stayed blocked (phase=%q)", host, pathSuffix, r.Phase)
	}
}

// assertUnconditionalThirdPartyFiredPreConsent is AC3: the fixture's one
// unconditional third-party asset must be reported as contacted before any
// consent decision, in every consent mode.
func assertUnconditionalThirdPartyFiredPreConsent(t *testing.T, res *model.Result) {
	t.Helper()

	r := findRequest(res, "tracker", "/pixel.gif")
	if r == nil {
		t.Fatal("the unconditional third-party pixel never fired")
	}

	if r.Party != model.ThirdParty {
		t.Errorf("pixel.gif: party=%q, want third-party", r.Party)
	}

	if r.Phase != model.PhasePre {
		t.Errorf("pixel.gif: phase=%q, want pre-interaction — this is what \"contacted before any consent decision\" looks like", r.Phase)
	}
}

func assertKlaroApplied(t *testing.T, res *model.Result) {
	t.Helper()

	if res.Consent.Outcome != model.OutcomeApplied {
		t.Fatalf("consent outcome = %q, want %q: %s", res.Consent.Outcome, model.OutcomeApplied, res.Consent.Reason)
	}

	if res.Consent.CMP != "Klaro" {
		t.Errorf("detected CMP = %q, want Klaro", res.Consent.CMP)
	}
}
