package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These are white-box tests of the document migration: the batching, the
// progress it reports, and what it does with a row the bucket will not take.
// All three need to see inside the store — to choose a batch size, to read the
// log it writes, and to make one bucket write fail — which the black-box suite
// in documents_test.go cannot.

// documentStore opens a SQLite store, puts the document column back, and fills
// it with rows in the layout that came before the bucket.
//
// Putting the column back rather than building the old schema by hand keeps
// this test about the mover: the migration mechanism itself is covered against
// every dialect in documents_test.go.
func documentStore(t *testing.T, log *slog.Logger, documents map[string]string) *Store {
	t.Helper()

	dir := t.TempDir()

	s, err := Open(t.Context(), Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
		Logger:      log,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if _, err := s.db.ExecContext(t.Context(),
		`alter table results add column document text not null default ''`); err != nil {
		t.Fatalf("putting the document column back: %v", err)
	}

	at := time.Now().Add(-time.Hour)

	for scanID, document := range documents {
		if _, err := s.db.ExecContext(t.Context(),
			`insert into results (target, consent_mode, scan_id, started_at, termination, document)
			 values ('site', 'reject', ?, ?, 'idle', ?)`,
			scanID, at.UnixNano(), document); err != nil {
			t.Fatalf("writing a row in the old layout: %v", err)
		}
	}

	return s
}

// oldRows names five rows holding five distinct documents.
func oldRows() map[string]string {
	rows := map[string]string{}

	for _, id := range []string{"scan-a", "scan-b", "scan-c", "scan-d", "scan-e"} {
		rows[id] = `{"scanId":"` + id + `","target":"site","consentMode":"reject"}`
	}

	return rows
}

// TestTheMigrationWalksEveryBatch is AC3. Paging is where a migration skips a
// row or moves one twice, and the boundary is only crossed when there are more
// rows than one batch holds — which is why the batch size is a parameter here
// rather than the 256 a real run uses.
func TestTheMigrationWalksEveryBatch(t *testing.T) {
	t.Parallel()

	var log bytes.Buffer

	rows := oldRows()
	s := documentStore(t, slog.New(slog.NewTextHandler(&log, nil)), rows)

	if err := s.moveDocuments(t.Context(), 2); err != nil {
		t.Fatalf("moving documents in batches of two: %v", err)
	}

	m := s.DocumentMigration()
	if m.Moved != len(rows) || m.Failed != 0 {
		t.Fatalf("the migration reports %+v, want all %d rows moved", m, len(rows))
	}

	// Every row moved exactly once: none still holds a document, and each
	// names an artifact.
	var left int

	if err := s.db.QueryRowContext(t.Context(),
		`select count(*) from results where document <> '' or artifact_ref = ''`).Scan(&left); err != nil {
		t.Fatal(err)
	}

	if left != 0 {
		t.Errorf("%d rows were skipped by the batched walk", left)
	}

	// A second run over the same store finds nothing to do, which is what
	// makes the migration safe to rerun (AC2).
	if err := s.moveDocuments(t.Context(), 2); err != nil {
		t.Fatalf("running the migration again: %v", err)
	}

	// Nil, not a report of zeroes: DocumentMigration() promises nil where
	// nothing had to move, and a caller that trusts it must not be told a
	// migration happened.
	if again := s.DocumentMigration(); again != nil {
		t.Errorf("a second run reports %+v, want nil because nothing was left to move", again)
	}

	// And the operator watching an upgrade sees it advance: a line per batch,
	// with a rising count (AC3).
	progress := strings.Count(log.String(), "moving stored result documents\"")
	if progress < 3 {
		t.Errorf("the migration logged %d progress lines for three batches:\n%s", progress, log.String())
	}

	if !strings.Contains(log.String(), "moved=5") {
		t.Errorf("the migration never logged how much it had moved:\n%s", log.String())
	}
}

// TestARowTheBucketWillNotTakeIsReportedAndLeftInPlace is AC4 for the failure
// the story names first: a bucket write that keeps failing.
//
// The run must finish the rows it can, name the one it could not, and then
// refuse to complete the migration — because completing it would drop the
// column that still holds the only copy of that row's document.
func TestARowTheBucketWillNotTakeIsReportedAndLeftInPlace(t *testing.T) {
	t.Parallel()

	rows := oldRows()
	s := documentStore(t, slog.New(slog.DiscardHandler), rows)

	// The writability probe is the first write of the run, so the second is
	// the first row's.
	var writes int

	s.bucket.setRetry(func(ctx context.Context, op string, fn func(context.Context) error) error {
		if op == "storing an artifact" {
			writes++

			if writes == 3 {
				return errors.New("the bucket refused this object")
			}
		}

		return s.retry(ctx, op, fn)
	})

	err := s.moveDocuments(t.Context(), len(rows))
	if err == nil {
		t.Fatal("a migration with a row it could not move reported success")
	}

	m := s.DocumentMigration()
	if m.Failed != 1 || m.Moved != len(rows)-1 {
		t.Fatalf("the migration reports %+v, want one row failed and the rest moved", m)
	}

	if len(m.FailedScans) != 1 {
		t.Fatalf("the report names %v, want the one scan that failed", m.FailedScans)
	}

	// The error is what an operator acts on, so it has to carry the count and
	// the identifier.
	failed := m.FailedScans[0]

	if !strings.Contains(err.Error(), failed) || !strings.Contains(err.Error(), "1 of 5") {
		t.Errorf("the failure names neither the scan nor how many there were: %v", err)
	}

	// The row that failed still holds its document, so nothing is lost by
	// running again.
	var document string

	if err := s.db.QueryRowContext(t.Context(),
		`select document from results where scan_id = ?`, failed).Scan(&document); err != nil {
		t.Fatal(err)
	}

	if document != rows[failed] {
		t.Errorf("the row that failed holds %q, want its document untouched", document)
	}
}

// TestABucketThatDiesMidMigrationStopsTheWalk is the other half of AC4, and
// the reason a failure count is not a substitute for a decision.
//
// probeWritable establishes that the bucket was alive; a bucket that then
// refuses row after row has stopped being reachable, and grinding on would
// spend the whole retry policy per row — a day's work, a log line per scan, and
// a failure count the size of the store — before reporting what was already
// clear after five rows.
func TestABucketThatDiesMidMigrationStopsTheWalk(t *testing.T) {
	t.Parallel()

	rows := map[string]string{}

	// Comfortably more than the threshold, so a walk that did not stop would
	// visit rows the assertion below can count.
	for i := range maxConsecutiveBucketFailures * 4 {
		id := "scan-" + strconv.Itoa(i)
		rows[id] = `{"scanId":"` + id + `","target":"site","consentMode":"reject"}`
	}

	s := documentStore(t, slog.New(slog.DiscardHandler), rows)

	// Alive for the probe, dead from the first row onwards.
	var writes int

	s.bucket.setRetry(func(ctx context.Context, op string, fn func(context.Context) error) error {
		if op == "storing an artifact" {
			writes++

			if writes > 1 {
				return errors.New("the bucket has stopped answering")
			}
		}

		return s.retry(ctx, op, fn)
	})

	err := s.moveDocuments(t.Context(), len(rows))
	if err == nil {
		t.Fatal("a migration against a dead bucket reported success")
	}

	m := s.DocumentMigration()
	if m.Failed != maxConsecutiveBucketFailures {
		t.Errorf("the migration tried %d rows against a dead bucket, want it to stop after %d",
			m.Failed, maxConsecutiveBucketFailures)
	}

	if m.Moved != 0 {
		t.Errorf("the migration reports %d rows moved against a bucket that took none", m.Moved)
	}

	// An operator has to be able to act on it: which bucket, and which scans
	// were seen before it gave up.
	for _, want := range []string{"unreachable", s.bucket.String(), m.FailedScans[0]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}

	// Nothing was dropped: every document is still where it was.
	var left int

	if err := s.db.QueryRowContext(t.Context(),
		`select count(*) from results where document <> ''`).Scan(&left); err != nil {
		t.Fatal(err)
	}

	if left != len(rows) {
		t.Errorf("%d rows still hold a document, want all %d", left, len(rows))
	}
}

// TestABadRowDoesNotCountTowardsADeadBucket is the other side of that
// decision. A document that will not decode says nothing about whether the
// bucket is alive, so a scattering of them must not stop a migration that is
// otherwise working.
func TestABadRowDoesNotCountTowardsADeadBucket(t *testing.T) {
	t.Parallel()

	rows := map[string]string{}

	for i := range maxConsecutiveBucketFailures * 3 {
		rows["scan-"+strconv.Itoa(i)] = "this was never JSON"
	}

	s := documentStore(t, slog.New(slog.DiscardHandler), rows)

	if err := s.moveDocuments(t.Context(), len(rows)); err != nil {
		t.Fatalf("a migration of undecodable documents failed: %v", err)
	}

	m := s.DocumentMigration()
	if m.Moved != len(rows) || m.Unsummarised != len(rows) || m.Failed != 0 {
		t.Errorf("the migration reports %+v, want all %d rows moved and unsummarised", m, len(rows))
	}
}

// TestTheMigrationBoundsItsOwnQueries is Story 8.1, AC6 for the two calls that
// used to escape it.
//
// planDocuments and probeWritable are reached from a context that deliberately
// has no deadline — the migration may legitimately run for minutes — so each
// has to impose its own. Without that, a database or an endpoint that accepts
// the connection and never answers hangs the start of wsaw with no error and
// nothing in the log after "moving stored result documents".
func TestTheMigrationBoundsItsOwnQueries(t *testing.T) {
	t.Parallel()

	s := documentStore(t, slog.New(slog.DiscardHandler), oldRows())

	// A recording dialect, so the context planDocuments hands the database can
	// be inspected rather than inferred.
	recorder := &deadlineRecorder{dialect: s.d}
	s.d = recorder

	var probeDeadlines int

	s.bucket.setRetry(func(ctx context.Context, op string, fn func(context.Context) error) error {
		if _, ok := ctx.Deadline(); ok {
			probeDeadlines++
		} else {
			t.Errorf("the bucket operation %q ran with no deadline", op)
		}

		return s.retry(ctx, op, fn)
	})

	if err := s.moveDocuments(context.WithoutCancel(t.Context()), 2); err != nil {
		t.Fatalf("moving documents: %v", err)
	}

	if recorder.withoutDeadline > 0 {
		t.Errorf("%d catalogue queries ran with no deadline", recorder.withoutDeadline)
	}

	if recorder.withDeadline == 0 {
		t.Error("no catalogue query was made, so nothing was proved")
	}

	if probeDeadlines == 0 {
		t.Error("the writability probe made no bucket call, so nothing was proved")
	}
}

// deadlineRecorder counts how many catalogue queries carried a deadline. It
// wraps a real dialect rather than replacing one, so everything the migration
// does still happens.
type deadlineRecorder struct {
	dialect

	withDeadline    int
	withoutDeadline int
}

func (d *deadlineRecorder) hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	if _, ok := ctx.Deadline(); ok {
		d.withDeadline++
	} else {
		d.withoutDeadline++
	}

	return d.dialect.hasColumn(ctx, db, table, column)
}

// TestTheMigrationUpdateMatchesItsArguments keeps the statement and its
// bindings in step, the same discipline the insert is held to. A column added
// to resultDerived without a value to go in it would otherwise surface as a
// placeholder-count error during somebody's upgrade.
func TestTheMigrationUpdateMatchesItsArguments(t *testing.T) {
	t.Parallel()

	statement := documentMigrationUpdate()

	// The derived columns, the emptied document column, and the three key
	// columns of the where clause.
	want := len(resultDerived) + 1 + len(resultKey)

	if got := strings.Count(statement, "?"); got != want {
		t.Errorf("the update has %d placeholders, want %d: %s", got, want, statement)
	}

	args := append(resultDerivedArgs(Summary{}, resultRef{}), documentMovedToBucket)
	args = append(args, resultRowKey{}.args()...)

	if len(args) != want {
		t.Errorf("the update binds %d values for %d placeholders", len(args), want)
	}

	for _, col := range resultDerived {
		if !strings.Contains(statement, col+" = ?") {
			t.Errorf("the update does not write the %s column: %s", col, statement)
		}
	}

	// The two columns the row recorded for itself are deliberately not
	// rewritten: a migration that recomputed them from a document that
	// disagreed would reorder a target's history.
	for _, col := range resultScanned {
		if strings.Contains(statement, col+" = ?") {
			t.Errorf("the update rewrites %s, which belongs to the row: %s", col, statement)
		}
	}
}
