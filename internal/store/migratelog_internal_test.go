package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
)

// These read the log an open writes about the schema. An operator looking at a
// start-up has to be able to tell, without opening the database, which schema
// version the store is at and whether this start changed it — a store that
// was quietly migrated by a routine restart is otherwise indistinguishable
// from one that was already current.

// logRecord is one decoded JSON log line.
type logRecord map[string]any

// openLogged opens the SQLite store at dir, logging into a fresh buffer, and
// returns the records that open wrote.
func openLogged(t *testing.T, dir string) []logRecord {
	t.Helper()

	var buf bytes.Buffer

	s, err := OpenSQL(t.Context(), Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
		Logger:      slog.New(slog.NewJSONHandler(&buf, nil)),
	})
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var records []logRecord

	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		var r logRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("decoding log line %q: %v", sc.Text(), err)
		}

		records = append(records, r)
	}

	return records
}

// findLog returns the first record with the message, or nil.
func findLog(records []logRecord, msg string) logRecord {
	for _, r := range records {
		if r["msg"] == msg {
			return r
		}
	}

	return nil
}

// countLog counts the records with the message.
func countLog(records []logRecord, msg string) int {
	n := 0

	for _, r := range records {
		if r["msg"] == msg {
			n++
		}
	}

	return n
}

// TestOpeningANewStoreLogsTheMigration covers the first start: every version
// is applied, each one is logged, and the summary names where the schema
// started and where it ended.
func TestOpeningANewStoreLogsTheMigration(t *testing.T) {
	t.Parallel()

	want := float64(len(sqliteDialect{}.migrations()))
	records := openLogged(t, t.TempDir())

	start := findLog(records, msgSchemaMigrating)
	if start == nil {
		t.Fatalf("no %q record in %v", msgSchemaMigrating, records)
	}

	if start["from_version"] != 0.0 || start["to_version"] != want || start["driver"] != DriverSQLite {
		t.Errorf("start record = %v, want from_version 0, to_version %v, driver %s", start, want, DriverSQLite)
	}

	if got := countLog(records, msgSchemaVersionApplied); float64(got) != want {
		t.Errorf("%d %q records, want one per version (%v)", got, msgSchemaVersionApplied, want)
	}

	done := findLog(records, msgSchemaMigrated)
	if done == nil {
		t.Fatalf("no %q record in %v", msgSchemaMigrated, records)
	}

	if done["from_version"] != 0.0 || done["schema_version"] != want || done["migrations_applied"] != want {
		t.Errorf("done record = %v, want from_version 0, schema_version %v, migrations_applied %v", done, want, want)
	}

	if findLog(records, msgSchemaCurrent) != nil {
		t.Errorf("a store that was just migrated was also logged as already current: %v", records)
	}
}

// TestReopeningACurrentStoreLogsThatNothingRan covers every later start: the
// version is reported and no migration is claimed.
func TestReopeningACurrentStoreLogsThatNothingRan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	openLogged(t, dir)

	records := openLogged(t, dir)

	current := findLog(records, msgSchemaCurrent)
	if current == nil {
		t.Fatalf("no %q record in %v", msgSchemaCurrent, records)
	}

	want := float64(len(sqliteDialect{}.migrations()))
	if current["schema_version"] != want || current["migrations_applied"] != 0.0 || current["driver"] != DriverSQLite {
		t.Errorf("current record = %v, want schema_version %v, migrations_applied 0, driver %s",
			current, want, DriverSQLite)
	}

	for _, msg := range []string{msgSchemaMigrating, msgSchemaVersionApplied, msgSchemaMigrated} {
		if findLog(records, msg) != nil {
			t.Errorf("reopening a current store logged %q: %v", msg, records)
		}
	}
}

// TestSchemaVersionIsWhatTheStoreOpenedAt covers the version the start-up
// "store opened" line reports: both the migrating open and the inspecting one
// must know it, since both are a start an operator reads.
func TestSchemaVersionIsWhatTheStoreOpenedAt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	opts := Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
		Logger:      slog.New(slog.DiscardHandler),
	}
	want := len(sqliteDialect{}.migrations())

	opened, err := OpenSQL(t.Context(), opts)
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}

	if got := opened.SchemaVersion(); got != want {
		t.Errorf("OpenSQL: SchemaVersion() = %d, want %d", got, want)
	}

	if err := opened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	inspected, err := OpenForInspection(t.Context(), opts)
	if err != nil {
		t.Fatalf("OpenForInspection: %v", err)
	}

	t.Cleanup(func() {
		if err := inspected.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if got := inspected.SchemaVersion(); got != want {
		t.Errorf("OpenForInspection: SchemaVersion() = %d, want %d", got, want)
	}
}
