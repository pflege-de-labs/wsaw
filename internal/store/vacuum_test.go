package store_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The vacuum of a SQLite store (Story 4.13, AC13). Every test here runs
// against a SQLite file in a temp directory; the free-space check is
// injected, so none needs a nearly full disk.

// fillerBytes is how much a test leaves on the freelist: enough pages that
// the free ratio is far above the default threshold.
const fillerBytes = 4 << 20

// openVacuumable is openSQL for a test that needs the store to be SQLite.
func openVacuumable(t *testing.T) *store.SQL {
	t.Helper()

	if d := os.Getenv("WSAW_TEST_STORE_DRIVER"); d != "" && d != store.DriverSQLite {
		t.Skipf("vacuum is SQLite's; this run is against %s", d)
	}

	return openSQL(t)
}

// putResults stores n results and returns them as the store reads them back,
// so a test can compare what a vacuum leaves against what was there.
func putResults(t *testing.T, s *store.SQL, n int) []*model.Result {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Second)

	var out []*model.Result

	for i := range n {
		res := result("scan-"+string(rune('a'+i)), now.Add(time.Duration(i)*time.Minute), model.ConsentReject)
		if err := s.PutResult(res); err != nil {
			t.Fatalf("PutResult: %v", err)
		}

		got, err := s.GetResult(res.Target, res.ConsentMode, res.ScanID)
		if err != nil {
			t.Fatalf("GetResult: %v", err)
		}

		out = append(out, got)
	}

	return out
}

func assertResultsUnchanged(t *testing.T, s *store.SQL, want []*model.Result) {
	t.Helper()

	for _, w := range want {
		got, err := s.GetResult(w.Target, w.ConsentMode, w.ScanID)
		if err != nil {
			t.Errorf("GetResult(%s) after the vacuum: %v", w.ScanID, err)

			continue
		}

		if !reflect.DeepEqual(got, w) {
			t.Errorf("result %s reads back differently after the vacuum", w.ScanID)
		}
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return fi.Size()
}

func lastVacuum(t *testing.T, s *store.SQL) (store.MaintenanceRun, store.VacuumStats, bool) {
	t.Helper()

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindVacuum)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		return run, store.VacuumStats{}, false
	}

	stats, err := run.VacuumStats()
	if err != nil {
		t.Fatalf("VacuumStats: %v", err)
	}

	return run, stats, true
}

// TestAVacuumShrinksTheFileAndKeepsEveryResult is AC1: the file comes back to
// within a page or two of its live data, the WAL is truncated, every result
// reads back identically, and the receipt holds the exact stats returned.
func TestAVacuumShrinksTheFileAndKeepsEveryResult(t *testing.T) {
	t.Parallel()

	s := openVacuumable(t)
	want := putResults(t, s, 3)

	store.FillAndFree(t, s, fillerBytes)

	stats, err := s.Vacuum(t.Context(), store.TriggerCLI, store.VacuumOptions{})
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}

	if stats.Outcome != store.VacuumVacuumed {
		t.Fatalf("Outcome = %q, want %q", stats.Outcome, store.VacuumVacuumed)
	}

	if live := stats.PagesBefore - stats.FreePagesBefore; stats.PagesAfter > live+2 {
		t.Errorf("after the vacuum the file has %d pages, want at most %d live pages plus two",
			stats.PagesAfter, live)
	}

	if stats.FileBytesAfter != fileSize(t, stats.Path) || stats.FileBytesAfter >= stats.FileBytesBefore {
		t.Errorf("file went from %d to %d bytes (%d on disk), want it smaller",
			stats.FileBytesBefore, stats.FileBytesAfter, fileSize(t, stats.Path))
	}

	if !stats.CheckpointIncomplete && stats.WALBytesAfter != 0 {
		t.Errorf("the WAL is %d bytes after a complete checkpoint, want it truncated", stats.WALBytesAfter)
	}

	if stats.ReclaimedBytes() < fillerBytes/2 {
		t.Errorf("reclaimed %d bytes, want most of the %d left on the freelist", stats.ReclaimedBytes(), fillerBytes)
	}

	assertResultsUnchanged(t, s, want)

	run, recorded, found := lastVacuum(t, s)
	if !found {
		t.Fatal("a vacuum left no receipt")
	}

	if run.Trigger != store.TriggerCLI || run.Error != "" {
		t.Errorf("receipt = trigger %q, error %q; want a clean cli run", run.Trigger, run.Error)
	}

	if !reflect.DeepEqual(recorded, stats) {
		t.Errorf("the receipt reads back\n%+v\nwant what Vacuum returned\n%+v", recorded, stats)
	}
}

// TestAVacuumBelowTheThresholdIsSkipped is AC4: a file with little free
// space is not rewritten, and says so in a receipt; --force rewrites it
// anyway.
func TestAVacuumBelowTheThresholdIsSkipped(t *testing.T) {
	t.Parallel()

	s := openVacuumable(t)
	putResults(t, s, 3)

	// Nothing has been deleted, so any freelist the schema itself leaves is
	// far below a threshold of one half.
	opts := store.VacuumOptions{MinFreeRatio: 0.5}

	stats, err := s.Vacuum(t.Context(), store.TriggerSchedule, opts)
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}

	if stats.Outcome != store.VacuumSkipped || stats.PagesAfter != 0 || stats.MinFreeRatio != 0.5 {
		t.Errorf("stats = %+v, want a skip against 0.5 that rewrote nothing", stats)
	}

	if stats.ReclaimedBytes() != 0 {
		t.Errorf("a skipped vacuum reports %d bytes reclaimed", stats.ReclaimedBytes())
	}

	if run, recorded, found := lastVacuum(t, s); !found || recorded.Outcome != store.VacuumSkipped ||
		run.Trigger != store.TriggerSchedule {
		t.Errorf("receipt = %+v %+v (found %v), want a scheduled skip", run, recorded, found)
	}

	opts.Force = true

	stats, err = s.Vacuum(t.Context(), store.TriggerCLI, opts)
	if err != nil {
		t.Fatalf("forced Vacuum: %v", err)
	}

	if stats.Outcome != store.VacuumVacuumed || !stats.Forced {
		t.Errorf("forced stats = %+v, want a vacuum", stats)
	}
}

// TestAPlannedVacuumChangesNothing is AC1's --dry-run: the plan says what a
// vacuum would reclaim, and neither the file nor the receipt log moves.
func TestAPlannedVacuumChangesNothing(t *testing.T) {
	t.Parallel()

	s := openVacuumable(t)
	putResults(t, s, 1)
	store.FillAndFree(t, s, fillerBytes)

	stats, err := s.PlanVacuum(t.Context(), store.VacuumOptions{})
	if err != nil {
		t.Fatalf("PlanVacuum: %v", err)
	}

	before := fileSize(t, stats.Path)

	if stats.Outcome != store.VacuumVacuumed || stats.ReclaimableBytes() < fillerBytes/2 {
		t.Errorf("plan = %+v, want a vacuum reclaiming most of %d bytes", stats, fillerBytes)
	}

	if stats.BytesNeeded <= 0 || stats.BytesAvailable < stats.BytesNeeded {
		t.Errorf("plan needs %d bytes and has %d, want the space check reported", stats.BytesNeeded, stats.BytesAvailable)
	}

	if after := fileSize(t, stats.Path); after != before || stats.FileBytesBefore != before {
		t.Errorf("file is %d bytes after the plan, %d before; the plan measured %d", after, before, stats.FileBytesBefore)
	}

	if _, _, found := lastVacuum(t, s); found {
		t.Error("a plan left a receipt")
	}
}

// TestAVacuumWithoutRoomIsRefused is AC5: a disk that cannot hold the copy is
// refused before anything is rewritten, with both figures, and recorded.
func TestAVacuumWithoutRoomIsRefused(t *testing.T) {
	t.Parallel()

	s := openVacuumable(t)
	want := putResults(t, s, 2)
	store.FillAndFree(t, s, fillerBytes)

	store.SetFreeSpace(s, func(string) (int64, error) { return 1024, nil })

	stats, err := s.Vacuum(t.Context(), store.TriggerSchedule, store.VacuumOptions{Force: true})
	if !errors.Is(err, store.ErrVacuumNoSpace) {
		t.Fatalf("Vacuum = %v, want ErrVacuumNoSpace", err)
	}

	if stats.Outcome != store.VacuumRefused || stats.BytesAvailable != 1024 || stats.BytesNeeded <= 1024 {
		t.Errorf("stats = %+v, want a refusal naming what was needed and the 1024 bytes available", stats)
	}

	if got := fileSize(t, stats.Path); got != stats.FileBytesBefore {
		t.Errorf("a refused vacuum changed the file from %d to %d bytes", stats.FileBytesBefore, got)
	}

	assertResultsUnchanged(t, s, want)

	if run, recorded, found := lastVacuum(t, s); !found || recorded.Outcome != store.VacuumRefused || run.Error == "" {
		t.Errorf("receipt = %+v %+v (found %v), want a refusal with its reason", run, recorded, found)
	}
}

// TestAnInterruptedVacuumLeavesTheFileIntact is AC10: a vacuum whose context
// is cancelled just before the rewrite leaves every result readable and an
// error receipt behind.
//
// The cancellation lands between the space check and the statement, which is
// as close to "during" as a test can place it deterministically; that the
// driver also interrupts a statement already running is its own contract.
func TestAnInterruptedVacuumLeavesTheFileIntact(t *testing.T) {
	t.Parallel()

	s := openVacuumable(t)
	want := putResults(t, s, 2)
	store.FillAndFree(t, s, fillerBytes)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store.SetFreeSpace(s, func(string) (int64, error) {
		cancel()

		return 1 << 40, nil
	})

	stats, err := s.Vacuum(ctx, store.TriggerSchedule, store.VacuumOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Vacuum = %v, want context.Canceled", err)
	}

	if stats.Outcome != store.VacuumError {
		t.Errorf("Outcome = %q, want %q", stats.Outcome, store.VacuumError)
	}

	assertResultsUnchanged(t, s, want)

	if run, recorded, found := lastVacuum(t, s); !found || recorded.Outcome != store.VacuumError || run.Error == "" {
		t.Errorf("receipt = %+v %+v (found %v), want an error receipt", run, recorded, found)
	}
}
