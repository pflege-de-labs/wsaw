package store_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// These are the tests of Story 8.4: the upgrade that moves every result
// document out of the database and into the artifact bucket.
//
// They run against every dialect (AC7), because a migration is where the three
// databases differ most — one of them cannot roll DDL back, one of them counts
// string length in characters, and all three spell "drop a column" slightly
// differently. A migration verified on SQLite alone would be a migration
// verified on the deployment least likely to be holding a year of evidence.

// oldLayout is a store whose results table still holds its documents in a
// column: the shape every wsaw before Story 8.2 wrote, and the input the
// document migration exists to consume.
type oldLayout struct {
	opts   store.Options
	driver string
	db     *sql.DB
}

// summaryColumnsAddedByMigration2 are the columns version 2 adds, in the order
// it adds them. They are named here so that oldLayoutStore can take them away
// again and leave a genuine version-1 table behind.
var summaryColumnsAddedByMigration2 = []string{
	"artifact_ref", "document_size", "document_digest",
	"duration_ns", "scan_error", "consent_outcome", "consent_cmp",
	"requests", "third_party_domains", "pre_consent_domains",
}

// oldLayoutStore builds a store in the layout that came before the bucket:
// schema version 1, the document in a column, and none of the columns version
// 2 adds.
//
// It gets there by taking the current schema back rather than by writing three
// dialects' worth of frozen DDL into the test: a store is created normally,
// every column version 2 added is dropped again, the document column is put
// back, and the recorded version is rewound to 1. What that produces is the
// state a real upgrade meets, built from the dialects' own DDL rather than from
// a copy of it that could drift.
//
// Rewinding to 1 rather than to 2 is the point of the exercise (AC7). Rows are
// inserted after this and before the store is reopened, so migration 2 — the
// ten-column ALTER, the one statement the three dialects genuinely spell
// differently — runs against a *populated* table on every dialect, which is
// what a customer's upgrade does and what a store created empty in CI never
// would.
func oldLayoutStore(t *testing.T) *oldLayout {
	t.Helper()

	o := &oldLayout{opts: storeOptions(t), driver: os.Getenv("WSAW_TEST_STORE_DRIVER")}
	if o.driver == "" {
		o.driver = store.DriverSQLite
	}

	s, err := store.Open(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("creating the store to take back: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("closing the store to take back: %v", err)
	}

	o.db = rawDB(t, o.opts)
	o.dropSummaryColumns(t)

	// longtext for MySQL, because a result document exceeds the 64 KiB a TEXT
	// holds; no default there either, since MySQL takes no literal default for
	// a long text column. The table is empty at this point, so nothing needs
	// one — and version 1 never declared one either.
	documentColumn := map[string]string{
		store.DriverSQLite:   "text not null default ''",
		store.DriverPostgres: "text not null default ''",
		store.DriverMySQL:    "longtext not null",
	}[o.driver]

	o.exec(t, "alter table results add column document "+documentColumn)
	o.setVersion(t, 1)

	return o
}

// dropSummaryColumns removes what migration 2 added, so that migration 2 has
// something to do when the store is opened again.
//
// SQLite drops one column per statement; the two servers take them all in one
// ALTER, which is also the shape their own migrations use.
func (o *oldLayout) dropSummaryColumns(t *testing.T) {
	t.Helper()

	if o.driver == store.DriverSQLite {
		for _, col := range summaryColumnsAddedByMigration2 {
			o.exec(t, "alter table results drop column "+col)
		}

		return
	}

	drops := make([]string, 0, len(summaryColumnsAddedByMigration2))
	for _, col := range summaryColumnsAddedByMigration2 {
		drops = append(drops, "drop column "+col)
	}

	o.exec(t, "alter table results "+strings.Join(drops, ", "))
}

// setVersion rewinds the recorded schema version, which is where each dialect
// keeps it.
func (o *oldLayout) setVersion(t *testing.T, version int) {
	t.Helper()

	if o.driver == store.DriverSQLite {
		o.exec(t, "pragma user_version = "+strconv.Itoa(version))

		return
	}

	o.exec(t, "update wsaw_schema_version set version = ? where id = 1", version)
}

// rawDB opens the store's database directly, whichever dialect it is on.
func rawDB(t *testing.T, opts store.Options) *sql.DB {
	t.Helper()

	if opts.Driver == "" || opts.Driver == store.DriverSQLite {
		return sqlDB(t, opts.Path)
	}

	db, err := sql.Open(sqlDriverFor(opts.Driver), opts.DSN.Reveal())
	if err != nil {
		t.Fatalf("connecting to the %s store: %v", opts.Driver, err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// rebind rewrites ? placeholders for the dialect under test, so the statements
// in this file are written once.
func (o *oldLayout) rebind(query string) string {
	if o.driver != store.DriverPostgres {
		return query
	}

	for n := 1; strings.Contains(query, "?"); n++ {
		query = strings.Replace(query, "?", "$"+strconv.Itoa(n), 1)
	}

	return query
}

func (o *oldLayout) exec(t *testing.T, query string, args ...any) {
	t.Helper()

	if _, err := o.db.ExecContext(t.Context(), o.rebind(query), args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// insert writes one row the way a pre-bucket wsaw wrote it: the document in
// the column, and the two columns the scan itself recorded beside it.
func (o *oldLayout) insert(t *testing.T, res *model.Result) []byte {
	t.Helper()

	document, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	// The version-1 column set, and nothing else: at this point the table has
	// no summary columns, because the store this simulates was written before
	// they existed.
	o.exec(t, `insert into results (target, consent_mode, scan_id, started_at, termination, document)
		values (?, ?, ?, ?, ?, ?)`,
		res.Target, string(res.ConsentMode), res.ScanID,
		res.StartedAt.UnixNano(), string(res.Termination), string(document))

	return document
}

// insertRaw writes a row whose document is whatever the caller says, including
// bytes that are not JSON at all.
func (o *oldLayout) insertRaw(t *testing.T, scanID, document string) {
	t.Helper()

	o.exec(t, `insert into results (target, consent_mode, scan_id, started_at, termination, document)
		values (?, ?, ?, ?, ?, ?)`,
		"site", string(model.ConsentReject), scanID,
		time.Now().UnixNano(), string(model.TermIdle), document)
}

// document reads back what a row still holds, or reports that the column has
// gone.
func (o *oldLayout) document(t *testing.T, scanID string) (string, bool) {
	t.Helper()

	var document string

	err := o.db.QueryRowContext(t.Context(),
		o.rebind(`select document from results where scan_id = ?`), scanID).Scan(&document)
	if err != nil {
		return "", false
	}

	return document, true
}

func (o *oldLayout) reference(t *testing.T, scanID string) string {
	t.Helper()

	var ref string

	if err := o.db.QueryRowContext(t.Context(),
		o.rebind(`select artifact_ref from results where scan_id = ?`), scanID).Scan(&ref); err != nil {
		t.Fatalf("reading the artifact reference of %s: %v", scanID, err)
	}

	return ref
}

// artifact is where the bucket keeps one document on disk. Every dialect's
// store writes its evidence to a local directory in these tests, so reaching
// into it works whichever database is under test.
func (o *oldLayout) artifact(t *testing.T, ref string) string {
	t.Helper()

	kind, digest, found := strings.Cut(ref, "/")
	if !found || kind != "result" {
		t.Fatalf("artifact reference %q is not a result document", ref)
	}

	return filepath.Join(o.opts.ArtifactDir, kind, digest)
}

// storedDocuments lists the result documents the bucket holds.
func (o *oldLayout) storedDocuments(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(o.opts.ArtifactDir, "result"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		t.Fatalf("listing the artifact bucket: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// refuseUpdatesTo makes the database reject any update of one row, which is
// how these tests interrupt a migration part-way through.
//
// A trigger rather than a mocked store: what is being tested is that the
// migration leaves a resumable state behind when a write it has already
// decided to make does not land, and only the database can refuse that
// convincingly. Each dialect spells it differently, which is the price of
// interrupting all three.
func (o *oldLayout) refuseUpdatesTo(t *testing.T, scanID string) {
	t.Helper()

	switch o.driver {
	case store.DriverSQLite:
		o.exec(t, `create trigger refuse_update before update on results for each row
			when old.scan_id = '`+scanID+`' begin select raise(abort, 'interrupted'); end`)

	case store.DriverPostgres:
		o.exec(t, `create or replace function refuse_update() returns trigger language plpgsql as $$
			begin raise exception 'interrupted'; end $$`)
		o.exec(t, `create trigger refuse_update before update on results for each row
			when (old.scan_id = '`+scanID+`') execute function refuse_update()`)

	case store.DriverMySQL:
		o.exec(t, `create trigger refuse_update before update on results for each row
			begin if old.scan_id = '`+scanID+`' then
				signal sqlstate '45000' set message_text = 'interrupted';
			end if; end`)
	}
}

func (o *oldLayout) allowUpdates(t *testing.T) {
	t.Helper()

	if o.driver == store.DriverPostgres {
		o.exec(t, `drop trigger refuse_update on results`)
		o.exec(t, `drop function refuse_update()`)

		return
	}

	o.exec(t, `drop trigger refuse_update`)
}

// migratedResults are the three shapes a store holds: a clean scan, a failed
// one, and one whose evidence carries characters outside ASCII.
func migratedResults(t *testing.T) []*model.Result {
	t.Helper()

	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	clean := result("scan-clean", base, model.ConsentReject)
	clean.Duration = 4 * time.Second
	clean.Consent.CMP = "cookiebot"

	failed := result("scan-failed", base.Add(time.Minute), model.ConsentReject)
	failed.Termination = model.TermError
	failed.Error = "navigate: context deadline exceeded"
	failed.Requests = nil

	// A document whose bytes outnumber its characters, so that a dry run
	// counting characters instead of bytes reports the wrong size.
	unicode := result("scan-unicode", base.Add(2*time.Minute), model.ConsentReject)
	unicode.URL = "https://exämple.com/straße?q=σελίδα"
	unicode.Requests = append(unicode.Requests, model.Request{
		URL: "https://tracker.test/π", NormalizedURL: "https://tracker.test/π",
		Domain: "tracker.test", Party: model.ThirdParty, Phase: model.PhasePost,
	})

	return []*model.Result{clean, failed, unicode}
}

// TestMigratingDocumentsFromRowsToTheBucket is AC1 and AC7: results written in
// the old layout are moved, byte for byte, and read back as themselves.
func TestMigratingDocumentsFromRowsToTheBucket(t *testing.T) {
	t.Parallel()

	o := oldLayoutStore(t)

	documents := map[string][]byte{}

	for _, res := range migratedResults(t) {
		documents[res.ScanID] = o.insert(t, res)
	}

	s, err := store.Open(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("migrating a store in the old layout: %v", err)
	}

	defer func() { _ = s.Close() }()

	// The column is gone, not merely emptied (AC1).
	if _, err := o.db.QueryContext(t.Context(), "select document from results"); err == nil {
		t.Error("the document column survived the migration")
	}

	m := s.DocumentMigration()
	if m == nil || m.Moved != len(documents) || m.Failed != 0 || m.Unsummarised != 0 {
		t.Fatalf("the migration reports %+v, want %d rows moved and nothing left behind", m, len(documents))
	}

	for scanID, want := range documents {
		ref := o.reference(t, scanID)

		stored, err := os.ReadFile(o.artifact(t, ref))
		if err != nil {
			t.Fatalf("%s: the document named by the row is not in the bucket: %v", scanID, err)
		}

		if string(stored) != string(want) {
			t.Errorf("%s: the stored document is not the one that was in the row:\n got %s\nwant %s",
				scanID, stored, want)
		}

		// Content-addressed like every other artifact, so a document stored
		// twice is one object.
		sum := sha256.Sum256(want)
		if got := "result/" + hex.EncodeToString(sum[:]); ref != got {
			t.Errorf("%s: reference = %q, want the content address %q", scanID, ref, got)
		}

		// And what comes back out of the store is the result that went in.
		got, err := s.GetResult("site", model.ConsentReject, scanID)
		if err != nil {
			t.Fatalf("%s: reading a migrated result: %v", scanID, err)
		}

		again, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}

		if string(again) != string(want) {
			t.Errorf("%s: the migrated result does not re-encode to its document:\n got %s\nwant %s",
				scanID, again, want)
		}
	}

	assertMigratedSummaries(t, s)
}

// assertMigratedSummaries checks the columns the migration derived, which is
// the other half of AC1: a migrated row must be what a freshly written one
// would be.
func assertMigratedSummaries(t *testing.T, s *store.Store) {
	t.Helper()

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("listing migrated results: %v", err)
	}

	if len(summaries) != 3 {
		t.Fatalf("got %d summaries, want 3", len(summaries))
	}

	byID := map[string]store.Summary{}
	for _, sm := range summaries {
		byID[sm.ScanID] = sm
	}

	clean := byID["scan-clean"]
	if clean.Duration != 4*time.Second || clean.Requests != 1 || clean.ThirdPartyDomains != 1 ||
		clean.PreConsentDomains != 1 || clean.ConsentCMP != "cookiebot" ||
		clean.ConsentOutcome != model.OutcomeApplied || clean.Error != "" {
		t.Errorf("the summary derived for a clean scan is wrong: %+v", clean)
	}

	failed := byID["scan-failed"]
	if failed.Termination != model.TermError || failed.Error != "navigate: context deadline exceeded" ||
		failed.Requests != 0 {
		t.Errorf("the summary derived for a failed scan is wrong: %+v", failed)
	}

	unicode := byID["scan-unicode"]
	if unicode.Requests != 2 || unicode.PreConsentDomains != 1 {
		t.Errorf("the summary derived for a scan with two requests is wrong: %+v", unicode)
	}
}

// TestADryRunReportsWhatWouldMoveAndMovesNothing is AC6. Deciding whether to
// start a one-way migration means knowing how many objects and how many bytes
// are about to arrive in a bucket, and finding out must not itself be the
// migration.
func TestADryRunReportsWhatWouldMoveAndMovesNothing(t *testing.T) {
	t.Parallel()

	o := oldLayoutStore(t)

	var want int64

	for _, res := range migratedResults(t) {
		want += int64(len(o.insert(t, res)))
	}

	plan, err := store.PlanDocumentMigration(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("planning the migration: %v", err)
	}

	if plan.Rows != 3 || plan.Bytes != want {
		t.Errorf("the plan reports %d rows and %d bytes, want 3 and %d", plan.Rows, plan.Bytes, want)
	}

	if plan.Moved != 0 || plan.MovedBytes != 0 {
		t.Errorf("a dry run reports having moved something: %+v", plan)
	}

	if plan.Bucket != o.opts.ArtifactDir {
		t.Errorf("the plan names %q as the destination, want %q", plan.Bucket, o.opts.ArtifactDir)
	}

	// Nothing moved: the rows still hold their documents and the bucket holds
	// no document at all.
	if document, ok := o.document(t, "scan-clean"); !ok || document == "" {
		t.Error("a dry run emptied a row")
	}

	if stored := o.storedDocuments(t); len(stored) != 0 {
		t.Errorf("a dry run wrote %d documents to the bucket: %v", len(stored), stored)
	}

	// And what it promised is what the migration then does.
	s, err := store.Open(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("migrating after the dry run: %v", err)
	}

	defer func() { _ = s.Close() }()

	done := s.DocumentMigration()
	if done.Moved != plan.Rows || done.MovedBytes != plan.Bytes {
		t.Errorf("the migration moved %d rows and %d bytes where the plan promised %d and %d",
			done.Moved, done.MovedBytes, plan.Rows, plan.Bytes)
	}

	// A store that has already migrated has nothing left to plan.
	after, err := store.PlanDocumentMigration(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("planning a migrated store: %v", err)
	}

	if after.Pending() {
		t.Errorf("a migrated store still reports %+v to move", after)
	}
}

// TestAnInterruptedMigrationResumes is AC2: a run that stops half way leaves a
// store the next run finishes, without moving a document twice and without
// skipping one.
//
// The interruption is a real abort rather than a simulated half-done state: a
// trigger refuses the row update for one scan, which is what the migration
// meets if the database goes away mid-run — some rows committed, one in flight,
// the rest untouched. It also leaves the interesting case behind on purpose,
// because the document of the row that failed is already in the bucket by then:
// content addressing is what makes writing it again on the next run free, and
// the object count at the end is what proves it.
func TestAnInterruptedMigrationResumes(t *testing.T) {
	t.Parallel()

	o := oldLayoutStore(t)

	documents := map[string][]byte{}

	for i := range 4 {
		res := result(fmt.Sprintf("scan-%d", i), time.Now().Add(time.Duration(i)*time.Minute), model.ConsentReject)
		res.Duration = time.Duration(i+1) * time.Second
		documents[res.ScanID] = o.insert(t, res)
	}

	o.refuseUpdatesTo(t, "scan-2")

	if _, err := store.Open(t.Context(), o.opts); err == nil {
		t.Fatal("a migration that could not record a moved document reported success")
	} else if !strings.Contains(err.Error(), "scan-2") {
		t.Errorf("the failure does not name the scan it stopped on: %v", err)
	}

	// The column is still there, and so is the document of the row that did
	// not complete: nothing was dropped that had not been recorded (AC4).
	if document, ok := o.document(t, "scan-2"); !ok || document != string(documents["scan-2"]) {
		t.Fatalf("the row that could not be recorded lost its document: %q, present=%v", document, ok)
	}

	// The rows before it did move, and stayed moved: an interrupted run is
	// resumed, not restarted.
	before := 0

	for _, scanID := range []string{"scan-0", "scan-1"} {
		if document, _ := o.document(t, scanID); document == "" {
			before++
		}
	}

	if before != 2 {
		t.Errorf("%d of the two rows before the interruption moved, want both", before)
	}

	o.allowUpdates(t)

	s, err := store.Open(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("resuming the migration: %v", err)
	}

	defer func() { _ = s.Close() }()

	if m := s.DocumentMigration(); m == nil || m.Moved != 2 || m.Failed != 0 {
		t.Errorf("the resumed run reports %+v, want the two remaining rows moved", m)
	}

	// Nothing was skipped and nothing was written twice: one object per
	// distinct document, including the one written by both runs.
	if stored := o.storedDocuments(t); len(stored) != len(documents) {
		t.Errorf("the bucket holds %d documents for %d results: %v", len(stored), len(documents), stored)
	}

	for scanID, want := range documents {
		got, err := s.GetResult("site", model.ConsentReject, scanID)
		if err != nil {
			t.Fatalf("%s: reading a resumed result: %v", scanID, err)
		}

		again, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}

		if string(again) != string(want) {
			t.Errorf("%s: the resumed result is not the document that was stored", scanID)
		}
	}
}

// TestADocumentThatWillNotDecodeIsMovedAndReported is the other half of AC4.
//
// The bytes are the evidence, so they move whether or not this build can read
// them; what cannot be produced is a summary, and the row says so rather than
// carrying zeroes that would read as a scan that observed nothing (Tenet 5).
func TestADocumentThatWillNotDecodeIsMovedAndReported(t *testing.T) {
	t.Parallel()

	o := oldLayoutStore(t)

	o.insert(t, result("scan-good", time.Now(), model.ConsentReject))
	o.insertRaw(t, "scan-broken", "{this was never JSON")

	s, err := store.Open(t.Context(), o.opts)
	if err != nil {
		t.Fatalf("one undecodable document stopped the whole migration: %v", err)
	}

	defer func() { _ = s.Close() }()

	m := s.DocumentMigration()
	if m == nil || m.Moved != 2 || m.Unsummarised != 1 || m.Failed != 0 {
		t.Fatalf("the migration reports %+v, want two rows moved and one of them unsummarised", m)
	}

	stored, err := os.ReadFile(o.artifact(t, o.reference(t, "scan-broken")))
	if err != nil {
		t.Fatalf("the undecodable document was not preserved: %v", err)
	}

	if string(stored) != "{this was never JSON" {
		t.Errorf("the undecodable document was rewritten: %s", stored)
	}

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, sm := range summaries {
		if sm.ScanID == "scan-broken" && sm.Error == "" {
			t.Errorf("the row with an undecodable document is listed as though it had a summary: %+v", sm)
		}
	}

	if len(summaries) != 2 {
		t.Errorf("got %d summaries, want both rows listed", len(summaries))
	}

	// Reading it reports corruption rather than handing back an empty result.
	if _, err := s.GetResult("site", model.ConsentReject, "scan-broken"); !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("reading the undecodable result = %v, want ErrCorrupt", err)
	}
}

// TestAMigrationThatLostItsVersionRecordFinishes is the MySQL half of AC2.
//
// MySQL commits DDL implicitly, so a connection lost between an ALTER and the
// row that records the version leaves a store whose schema is correct and whose
// version says otherwise. The next start has to finish the job rather than
// refuse to open a store nothing is wrong with — which, without help, would
// need an operator to hand-edit a version number.
//
// Both ALTERs can lose their record, and they fail differently on the rerun:
// replaying version 3 asks MySQL to drop a column that has gone (1091), and
// replaying version 2 asks it to add columns that are already there (1060).
// Only one of the two was tolerated until a reviewer noticed; both are covered
// here so neither can regress into a store that never opens again.
func TestAMigrationThatLostItsVersionRecordFinishes(t *testing.T) {
	t.Parallel()

	if os.Getenv("WSAW_TEST_STORE_DRIVER") != store.DriverMySQL {
		t.Skip("only MySQL commits DDL outside the transaction that records the version, so only MySQL can lose one")
	}

	for _, tc := range []struct {
		name    string
		rewound int
	}{
		{name: "the drop of the document column was not recorded", rewound: 2},
		{name: "the addition of the summary columns was not recorded", rewound: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := oldLayoutStore(t)
			o.insert(t, result("scan-1", time.Now(), model.ConsentReject))

			first, err := store.Open(t.Context(), o.opts)
			if err != nil {
				t.Fatalf("migrating: %v", err)
			}

			if err := first.Close(); err != nil {
				t.Fatal(err)
			}

			// The schema is complete; pretend the version record never landed.
			o.setVersion(t, tc.rewound)

			s, err := store.Open(t.Context(), o.opts)
			if err != nil {
				t.Fatalf("reopening a store whose version record was lost at %d: %v", tc.rewound, err)
			}

			defer func() { _ = s.Close() }()

			if _, err := s.GetResult("site", model.ConsentReject, "scan-1"); err != nil {
				t.Errorf("the recovered store cannot read its results: %v", err)
			}

			var version int

			if err := o.db.QueryRowContext(t.Context(),
				"select version from wsaw_schema_version where id = 1").Scan(&version); err != nil {
				t.Fatal(err)
			}

			if version != 3 {
				t.Errorf("the recovered store records schema version %d, want 3", version)
			}
		})
	}
}

// TestAnUnwritableBucketFailsBeforeAnyRowIsTouched is AC5. A migration that
// discovered the bucket was read-only half way through would leave a store
// nobody can reason about; discovering it first costs one write.
func TestAnUnwritableBucketFailsBeforeAnyRowIsTouched(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, which is not refused write access by permissions")
	}

	o := oldLayoutStore(t)

	documents := map[string][]byte{}
	for _, res := range migratedResults(t) {
		documents[res.ScanID] = o.insert(t, res)
	}

	if err := os.MkdirAll(o.opts.ArtifactDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(o.opts.ArtifactDir, 0o500); err != nil {
		t.Fatal(err)
	}

	// Restored so the test's own directory can be cleaned up.
	t.Cleanup(func() { _ = os.Chmod(o.opts.ArtifactDir, 0o700) })

	_, err := store.Open(t.Context(), o.opts)
	if err == nil {
		t.Fatal("a store with a read-only artifact bucket opened anyway")
	}

	if !strings.Contains(err.Error(), o.opts.ArtifactDir) {
		t.Errorf("the failure does not name the bucket: %v", err)
	}

	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("the failure does not say what is wrong with it: %v", err)
	}

	// Not one row was touched.
	for scanID, want := range documents {
		if document, ok := o.document(t, scanID); !ok || document != string(want) {
			t.Errorf("%s: the row was touched before the bucket was checked", scanID)
		}
	}
}
