package store_test

import (
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// openSQL is open(t) for the tests below: LastMaintenanceRun and
// MaintenanceRuns are deliberately not on the store.Store seam (AC6), so a
// test that reads them needs the concrete type.
func openSQL(t *testing.T) *store.SQL {
	t.Helper()

	s, err := store.OpenSQL(t.Context(), storeOptions(t))
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s
}

// TestPruneRecordsAReceipt: a real prune leaves a receipt that reads back the
// exact stats it returned, minus the fields a plan already fills (AC2, AC9).
func TestPruneRecordsAReceipt(t *testing.T) {
	t.Parallel()

	s := openSQL(t)
	now := time.Now()

	if err := s.PutResult(result("scan-1", now.Add(-2*time.Hour), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(result("scan-2", now, model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	before := time.Now()

	stats, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{Keep: &store.Keep{Last: 1}})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		t.Fatal("LastMaintenanceRun found nothing after a real prune")
	}

	if run.Trigger != store.TriggerCLI {
		t.Errorf("Trigger = %q, want %q", run.Trigger, store.TriggerCLI)
	}

	if run.Error != "" {
		t.Errorf("Error = %q, want none for a prune that succeeded", run.Error)
	}

	if run.StartedAt.Before(before.Add(-time.Second)) || run.FinishedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("run spans %s..%s, want it inside the test's own window", run.StartedAt, run.FinishedAt)
	}

	got, err := run.PruneStats()
	if err != nil {
		t.Fatalf("PruneStats: %v", err)
	}

	// Results, Artifacts and Plans are what a plan already printed — a
	// receipt states what happened, not that list again (AC2).
	want := stats
	want.Results, want.Artifacts, want.Plans = nil, nil, nil

	if got.ResultsDeleted != want.ResultsDeleted || got.SeriesPruned != want.SeriesPruned ||
		got.ResultsKept != want.ResultsKept || got.ArtifactsDeleted != want.ArtifactsDeleted ||
		got.BytesFreed != want.BytesFreed {
		t.Errorf("recorded stats = %+v, want %+v", got, want)
	}

	if got.Results != nil || got.Artifacts != nil || got.Plans != nil {
		t.Errorf("recorded stats carry Results/Artifacts/Plans = %+v, want them cleared", got)
	}
}

// TestSweepRecordsAReceipt is TestPruneRecordsAReceipt for Sweep.
func TestSweepRecordsAReceipt(t *testing.T) {
	t.Parallel()

	s := openSQL(t)
	now := time.Now()

	if err := s.PutResult(result("scan-1", now, model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Sweep(t.Context(), store.TriggerSchedule, now, store.SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindSweep)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		t.Fatal("LastMaintenanceRun found nothing after a real sweep")
	}

	if run.Trigger != store.TriggerSchedule {
		t.Errorf("Trigger = %q, want %q", run.Trigger, store.TriggerSchedule)
	}

	got, err := run.SweepStats()
	if err != nil {
		t.Fatalf("SweepStats: %v", err)
	}

	want := stats
	want.Artifacts, want.RebuildMarkers = nil, nil

	if got.ArtifactsScanned != want.ArtifactsScanned || got.ArtifactsDeleted != want.ArtifactsDeleted ||
		got.BytesFreed != want.BytesFreed {
		t.Errorf("recorded stats = %+v, want %+v", got, want)
	}

	if got.Artifacts != nil || got.RebuildMarkers != nil {
		t.Errorf("recorded stats carry Artifacts/RebuildMarkers = %+v, want them cleared", got)
	}
}

// TestAPlanRecordsNothing: PlanPrune and PlanSweep changed nothing and
// already printed their own answer, so neither leaves a receipt (AC3).
func TestAPlanRecordsNothing(t *testing.T) {
	t.Parallel()

	s := openSQL(t)
	now := time.Now()

	if err := s.PutResult(result("scan-1", now.Add(-2*time.Hour), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PlanPrune(t.Context(), now, store.Retention{Keep: &store.Keep{Last: 0}}); err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}

	if _, err := s.PlanSweep(t.Context(), now, store.SweepOptions{}); err != nil {
		t.Fatalf("PlanSweep: %v", err)
	}

	if _, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune); err != nil {
		t.Fatalf("LastMaintenanceRun(prune): %v", err)
	} else if found {
		t.Error("PlanPrune left a receipt, want none")
	}

	if _, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindSweep); err != nil {
		t.Fatalf("LastMaintenanceRun(sweep): %v", err)
	} else if found {
		t.Error("PlanSweep left a receipt, want none")
	}
}

// TestLastMaintenanceRunReportsNoneWhenNothingRanYet: a store nothing has
// pruned or swept yet reports that as its own fact, not a zero-valued run
// that would read as one that ran and did nothing (Tenet 5).
func TestLastMaintenanceRunReportsNoneWhenNothingRanYet(t *testing.T) {
	t.Parallel()

	s := openSQL(t)

	_, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if found {
		t.Error("a store nothing has pruned reports a run, want none")
	}
}

// TestARunThatErrorsStillLeavesAReceipt: a run that fails part way is still
// recorded, with whatever it had already removed and the error text (AC4).
func TestARunThatErrorsStillLeavesAReceipt(t *testing.T) {
	t.Parallel()

	s := openSQL(t)

	runErr := errors.New("the bucket went away halfway through")
	stats := store.PruneStats{ResultsDeleted: 3, ArtifactsDeleted: 5, BytesFreed: 1024}

	if err := s.RecordPruneRun(t.Context(), store.TriggerCLI, time.Now(), stats, runErr); err != nil {
		t.Fatalf("RecordPruneRun: %v", err)
	}

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		t.Fatal("a run that errored left no receipt")
	}

	if run.Error != runErr.Error() {
		t.Errorf("Error = %q, want %q", run.Error, runErr.Error())
	}

	got, err := run.PruneStats()
	if err != nil {
		t.Fatalf("PruneStats: %v", err)
	}

	if got.ResultsDeleted != 3 || got.ArtifactsDeleted != 5 || got.BytesFreed != 1024 {
		t.Errorf("recorded stats = %+v, want the partial counts the caller saw", got)
	}
}

// TestMaintenanceRunsAreTrimmedToTheNewest200: a log growing without bound,
// inside the feature whose purpose is bounding growth, would be self-refuting
// (AC5). Each kind is trimmed independently.
func TestMaintenanceRunsAreTrimmedToTheNewest200(t *testing.T) {
	t.Parallel()

	s := openSQL(t)

	const total = 205

	for i := range total {
		if err := s.RecordPruneRun(t.Context(), store.TriggerCLI, time.Now(),
			store.PruneStats{ResultsDeleted: i}, nil); err != nil {
			t.Fatalf("RecordPruneRun #%d: %v", i, err)
		}
	}

	if err := s.RecordSweepRun(t.Context(), store.TriggerCLI, time.Now(), store.SweepStats{}, nil); err != nil {
		t.Fatalf("RecordSweepRun: %v", err)
	}

	pruneRuns, err := s.MaintenanceRuns(t.Context(), store.MaintenanceKindPrune, 0)
	if err != nil {
		t.Fatalf("MaintenanceRuns(prune): %v", err)
	}

	if len(pruneRuns) != 200 {
		t.Fatalf("len(pruneRuns) = %d, want 200", len(pruneRuns))
	}

	newest, err := pruneRuns[0].PruneStats()
	if err != nil {
		t.Fatalf("PruneStats: %v", err)
	}

	if newest.ResultsDeleted != total-1 {
		t.Errorf("newest recorded run has ResultsDeleted = %d, want %d", newest.ResultsDeleted, total-1)
	}

	oldest, err := pruneRuns[len(pruneRuns)-1].PruneStats()
	if err != nil {
		t.Fatalf("PruneStats: %v", err)
	}

	if oldest.ResultsDeleted != total-200 {
		t.Errorf("oldest surviving run has ResultsDeleted = %d, want %d", oldest.ResultsDeleted, total-200)
	}

	// The sweep kind's single run must survive trimming the prune log: each
	// kind is trimmed on its own.
	sweepRuns, err := s.MaintenanceRuns(t.Context(), store.MaintenanceKindSweep, 0)
	if err != nil {
		t.Fatalf("MaintenanceRuns(sweep): %v", err)
	}

	if len(sweepRuns) != 1 {
		t.Errorf("len(sweepRuns) = %d, want 1", len(sweepRuns))
	}
}

// TestAStoreWithTheOldTriggerColumnKeepsItsReceipts: version 6 reached main
// naming its column trigger, and SQLite and PostgreSQL stores opened by those
// builds carry it. The rename in version 8 must bring such a store up to the
// column every query names, and a receipt written before it must survive the
// rename and read back with the trigger it was recorded with.
//
// Rewound to 7, that is a store exactly as those builds left it; rewound to 6,
// version 7 is replayed on the way as well, which is the other state the old
// column can be met in.
func TestAStoreWithTheOldTriggerColumnKeepsItsReceipts(t *testing.T) {
	t.Parallel()

	if os.Getenv("WSAW_TEST_STORE_DRIVER") == store.DriverMySQL {
		t.Skip("MySQL's version 6 could never be applied with the old column, so no MySQL store has one")
	}

	for _, rewound := range []int{6, 7} {
		t.Run("from version "+strconv.Itoa(rewound), func(t *testing.T) {
			t.Parallel()

			opts := storeOptions(t)
			s := openWithTheOldTriggerColumn(t, opts, rewound)

			assertTheOldReceiptSurvived(t, s)

			if err := s.RecordPruneRun(t.Context(), store.TriggerCLI, time.Now(),
				store.PruneStats{ResultsDeleted: 8}, nil); err != nil {
				t.Fatalf("RecordPruneRun after the rename: %v", err)
			}

			runs, err := s.MaintenanceRuns(t.Context(), store.MaintenanceKindPrune, 0)
			if err != nil {
				t.Fatalf("MaintenanceRuns: %v", err)
			}

			if len(runs) != 2 || runs[0].Trigger != store.TriggerCLI || runs[1].Trigger != store.TriggerSchedule {
				t.Errorf("runs after the rename = %+v, want the new cli run and then the old schedule run", runs)
			}

			if got, want := recordedSchemaVersion(t, opts), freshVersion(t); got != want {
				t.Errorf("the migrated store records schema version %d, want %d as a fresh store does", got, want)
			}
		})
	}
}

// openWithTheOldTriggerColumn builds a store as a build of main between
// Story 4.11 and the rename left it — the column called trigger, one receipt
// written under that name — rewinds its version to rewound, and opens it, which
// is what migrates it.
func openWithTheOldTriggerColumn(t *testing.T, opts store.Options, rewound int) *store.SQL {
	t.Helper()

	fresh, err := store.OpenSQL(t.Context(), opts)
	if err != nil {
		t.Fatalf("creating the store to take back: %v", err)
	}

	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	db := rawDB(t, opts)

	// No placeholders, so the statements are the same text in both dialects.
	for _, stmt := range []string{
		`alter table maintenance_runs rename column triggered_by to trigger`,
		`insert into maintenance_runs (kind, trigger, started_at, finished_at, error, stats)
			values ('prune', 'schedule', 1000, 2000, '', '{"resultsDeleted":7}')`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("building a store with the old column: %s: %v", stmt, err)
		}
	}

	rewindSchemaVersion(t, db, opts, rewound)

	s, err := store.OpenSQL(t.Context(), opts)
	if err != nil {
		t.Fatalf("migrating a store with the old trigger column: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// assertTheOldReceiptSurvived checks that the receipt openWithTheOldTriggerColumn
// wrote before the rename reads back exactly as it was written.
func assertTheOldReceiptSurvived(t *testing.T, s *store.SQL) {
	t.Helper()

	old, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		t.Fatal("the receipt written before the rename is gone")
	}

	stats, err := old.PruneStats()
	if err != nil {
		t.Fatalf("PruneStats: %v", err)
	}

	if old.Trigger != store.TriggerSchedule || stats.ResultsDeleted != 7 ||
		!old.StartedAt.Equal(time.Unix(0, 1000)) || !old.FinishedAt.Equal(time.Unix(0, 2000)) {
		t.Errorf("the receipt written before the rename reads back as %+v (%+v), "+
			"want trigger %q, 7 results deleted, 1000..2000ns", old, stats, store.TriggerSchedule)
	}
}

// recordedSchemaVersion reads the schema version a store records, from
// wherever its dialect keeps it.
func recordedSchemaVersion(t *testing.T, opts store.Options) int {
	t.Helper()

	query := "select version from wsaw_schema_version where id = 1"
	if opts.Driver == "" || opts.Driver == store.DriverSQLite {
		query = "pragma user_version"
	}

	var version int

	if err := rawDB(t, opts).QueryRowContext(t.Context(), query).Scan(&version); err != nil {
		t.Fatalf("reading the schema version: %v", err)
	}

	return version
}

// freshVersion is the version a store created by this build records, read
// rather than written here so that the next migration does not make it stale.
func freshVersion(t *testing.T) int {
	t.Helper()

	opts := storeOptions(t)

	s, err := store.OpenSQL(t.Context(), opts)
	if err != nil {
		t.Fatalf("creating a store to read the current schema version from: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	return recordedSchemaVersion(t, opts)
}
