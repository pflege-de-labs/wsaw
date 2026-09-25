package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The daemon's vacuum schedule (Story 4.13, AC13). Like the sweep's tests,
// these run the real loop against a clock the test moves. Sweeping is turned
// off in each unless a test says otherwise, so every armed delay reads as when
// the vacuum is due.

// vacuumStore is a maintenanceStore that can also be vacuumed, and keeps its
// vacuum receipt apart from its sweep receipt.
type vacuumStore struct {
	*maintenanceStore

	unsupported bool
	lastVac     *store.MaintenanceRun
	vacuumReads int
	vacuums     []store.VacuumOptions
	vacTriggers []string
	stats       store.VacuumStats
	vacuumErr   error
}

func (s *vacuumStore) SupportsVacuum() bool { return !s.unsupported }

func (s *vacuumStore) Vacuum(
	_ context.Context, trigger string, opts store.VacuumOptions,
) (store.VacuumStats, error) {
	s.enter("vacuum")
	defer s.leave()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.vacuums = append(s.vacuums, opts)
	s.vacTriggers = append(s.vacTriggers, trigger)
	s.lastVac = &store.MaintenanceRun{Kind: store.MaintenanceKindVacuum, FinishedAt: s.clock.Now()}

	return s.stats, s.vacuumErr
}

func (s *vacuumStore) LastMaintenanceRun(ctx context.Context, kind string) (store.MaintenanceRun, bool, error) {
	if kind != store.MaintenanceKindVacuum {
		return s.maintenanceStore.LastMaintenanceRun(ctx, kind)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.vacuumReads++

	if s.lastVac == nil {
		return store.MaintenanceRun{}, false, nil
	}

	return *s.lastVac, true, nil
}

func (s *vacuumStore) vacuumCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.vacuums)
}

func newVacuumStore(clock *fakeClock) *vacuumStore {
	return &vacuumStore{
		maintenanceStore: &maintenanceStore{clock: clock},
		stats:            store.VacuumStats{Outcome: store.VacuumVacuumed, FileBytesBefore: 1000, FileBytesAfter: 100},
	}
}

// vacuumApp is maintenanceApp for the vacuum: sweeping off, so the vacuum
// arms the only timer, unless mutate turns it back on.
func vacuumApp(t *testing.T, st *vacuumStore, mutate func(*config.Config)) *App {
	t.Helper()

	a, _ := maintenanceApp(t, st.maintenanceStore, func(c *config.Config) {
		off := false
		c.Store.Sweep = &off

		if mutate != nil {
			mutate(c)
		}
	})
	a.Store = st

	return a
}

// runVacuumLoop is runLoop with a channel for vacuum reloads.
func runVacuumLoop(t *testing.T, a *App, clock *fakeClock) chan VacuumSchedule {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	vacuums := make(chan VacuumSchedule, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)

		a.maintenanceLoop(ctx, nil, vacuums, clock.clock())
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return vacuums
}

// TestTheDefaultScheduleVacuumsWeekly: a configuration that says nothing about
// vacuuming vacuums, every seven days, at the default threshold (AC3).
func TestTheDefaultScheduleVacuumsWeekly(t *testing.T) {
	t.Parallel()

	got := VacuumScheduleFor(config.New())
	want := VacuumSchedule{Enabled: true, Interval: 7 * 24 * time.Hour, MinFreeRatio: store.DefaultVacuumMinFreeRatio}

	if got != want {
		t.Errorf("VacuumScheduleFor(config.New()) = %+v, want %+v", got, want)
	}
}

// TestTheFirstVacuumStartsPromptlyWhenNoneWasEverRecorded is AC7's first-run
// half — the store the Story 8.4 migration left — and AC4's "same Vacuum, at
// the configured threshold".
func TestTheFirstVacuumStartsPromptlyWhenNoneWasEverRecorded(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := newVacuumStore(clock)
	a := vacuumApp(t, st, func(c *config.Config) { c.Store.VacuumMinFreeRatio = 0.4 })

	runVacuumLoop(t, a, clock)

	if d := clock.waitArmed(t); d != firstSweepDelay {
		t.Fatalf("the first vacuum of a store that has never vacuumed is due after %s, want %s", d, firstSweepDelay)
	}

	clock.set(start.Add(firstSweepDelay))

	if d := clock.waitArmed(t); d != 7*24*time.Hour {
		t.Errorf("after the first vacuum the next is due in %s, want 168h", d)
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.vacuums) != 1 || st.vacTriggers[0] != store.TriggerSchedule || st.vacuums[0].MinFreeRatio != 0.4 ||
		st.vacuums[0].Force {
		t.Errorf("vacuums = %+v with triggers %v, want one scheduled, unforced vacuum at 0.4", st.vacuums, st.vacTriggers)
	}
}

// TestTheVacuumScheduleSurvivesARestart is AC7's restart half: a process
// started three days after the last recorded vacuum vacuums four days later.
func TestTheVacuumScheduleSurvivesARestart(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := newVacuumStore(clock)
	st.lastVac = &store.MaintenanceRun{FinishedAt: start.Add(-3 * 24 * time.Hour)}

	runVacuumLoop(t, vacuumApp(t, st, nil), clock)

	if d := clock.waitArmed(t); d != 4*24*time.Hour {
		t.Errorf("a process started 3 days after the last vacuum vacuums after %s, want 96h", d)
	}
}

// TestVacuumingOffRunsNoVacuumAtAll: vacuum: false neither vacuums nor reads
// the receipt log for one (AC3).
func TestVacuumingOffRunsNoVacuumAtAll(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := newVacuumStore(clock)
	a := vacuumApp(t, st, func(c *config.Config) {
		off := false
		c.Store.Vacuum = &off
		c.Store.MaxPerSeries = 10 // prunes arm the only timer there is
	})

	runVacuumLoop(t, a, clock)

	now := start

	for range 24 * 8 {
		now = now.Add(clock.waitArmed(t))
		clock.set(now)
	}

	clock.waitArmed(t)

	st.mu.Lock()
	reads := st.vacuumReads
	st.mu.Unlock()

	if n := st.vacuumCount(); n != 0 || reads != 0 {
		t.Errorf("with vacuuming off the loop vacuumed %d times and read its receipt %d times", n, reads)
	}
}

// TestAStoreThatIsNotSQLiteIsNeverVacuumed is AC2 from the daemon's side: a
// store that cannot be vacuumed gets no schedule, and the log says so once.
func TestAStoreThatIsNotSQLiteIsNeverVacuumed(t *testing.T) {
	t.Parallel()

	for name, s := range map[string]func(*vacuumStore) store.Store{
		"a server dialect": func(v *vacuumStore) store.Store {
			v.unsupported = true

			return v
		},
		"a store with no vacuum": func(v *vacuumStore) store.Store { return v.maintenanceStore },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			clock := newFakeClock(time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC))
			st := newVacuumStore(clock)
			a := vacuumApp(t, st, func(c *config.Config) { c.Store.MaxPerSeries = 10 })
			a.Store = s(st)

			var logged bytes.Buffer

			a.Logger = newTestLogger(&logged)

			m := a.newMaintenance(t.Context(), clock.clock())

			if !m.nextVacuum.IsZero() {
				t.Errorf("a vacuum was scheduled for %s", m.nextVacuum)
			}

			if !strings.Contains(logged.String(), "does not apply to this store") {
				t.Errorf("the log does not say why no vacuum is scheduled:\n%s", logged.String())
			}
		})
	}
}

// TestAVacuumNeverOverlapsAPruneOrASweep is AC6: all three fall due at once
// and run one after the other, prune, sweep, vacuum.
func TestAVacuumNeverOverlapsAPruneOrASweep(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := newVacuumStore(clock)
	due := start.Add(time.Hour)
	st.last = &store.MaintenanceRun{FinishedAt: due.Add(-24 * time.Hour)}
	st.lastVac = &store.MaintenanceRun{FinishedAt: due.Add(-7 * 24 * time.Hour)}

	a := vacuumApp(t, st, func(c *config.Config) {
		c.Store.Sweep = nil
		c.Store.MaxPerSeries = 10
	})

	runVacuumLoop(t, a, clock)

	if d := clock.waitArmed(t); d != time.Hour {
		t.Fatalf("first wake after %s, want 1h", d)
	}

	clock.set(due)
	clock.waitArmed(t)

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.overlapped {
		t.Error("a vacuum ran at the same time as a prune or a sweep")
	}

	if st.prunes != 1 || len(st.sweeps) != 1 || len(st.vacuums) != 1 {
		t.Errorf("%d prunes, %d sweeps and %d vacuums ran, want one of each", st.prunes, len(st.sweeps), len(st.vacuums))
	}
}

// TestAReloadAppliesTheVacuumSchedule is AC11: a reload that turns
// vacuuming off stops the next one.
func TestAReloadAppliesTheVacuumSchedule(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := newVacuumStore(clock)
	a := vacuumApp(t, st, func(c *config.Config) { c.Store.MaxPerSeries = 10 })

	vacuums := runVacuumLoop(t, a, clock)

	clock.waitArmed(t)

	vacuums <- VacuumSchedule{Enabled: false, Interval: time.Hour, MinFreeRatio: 0.2}

	if d := clock.waitArmed(t); d != time.Hour {
		t.Errorf("after vacuuming was turned off the next wake is in %s, want the prune in 1h", d)
	}

	now := start

	for range 3 {
		now = now.Add(time.Hour)
		clock.set(now)
		clock.waitArmed(t)
	}

	if n := st.vacuumCount(); n != 0 {
		t.Errorf("%d vacuums ran after a reload turned vacuuming off", n)
	}
}

// TestTheVacuumMetricsMoveOnSuccessAndSkipOnly is AC9: vacuumed and skipped
// move the gauge, refused and error do not, and only the rewrite's bytes are
// counted. It is also AC10's shutdown: an interrupted vacuum is logged as
// one, not as a failure somebody has to go and look at.
func TestTheVacuumMetricsMoveOnSuccessAndSkipOnly(t *testing.T) {
	t.Parallel()

	st := newVacuumStore(newFakeClock(time.Now()))
	a := vacuumApp(t, st, nil)

	var logged bytes.Buffer

	a.Logger = newTestLogger(&logged)

	schedule := VacuumScheduleFor(a.Config)

	st.stats, st.vacuumErr = store.VacuumStats{Outcome: store.VacuumRefused}, fmt.Errorf("x: %w", store.ErrVacuumNoSpace)
	a.vacuumOnce(t.Context(), st, time.Unix(1_700_000_000, 0), schedule)

	st.stats, st.vacuumErr = store.VacuumStats{Outcome: store.VacuumError}, errors.New("disk I/O error")
	a.vacuumOnce(t.Context(), st, time.Unix(1_700_000_001, 0), schedule)

	if got := gauge(t, a.Metrics, "wsaw_last_successful_vacuum_timestamp_seconds"); !strings.HasSuffix(got, " 0") {
		t.Errorf("a refused or failed vacuum moved the gauge: %q", got)
	}

	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), "level=ERROR") {
		t.Errorf("a refusal should warn and a failure should be an error:\n%s", logged.String())
	}

	st.stats = store.VacuumStats{Outcome: store.VacuumVacuumed, FileBytesBefore: 1000, WALBytesBefore: 500, FileBytesAfter: 100}
	st.vacuumErr = nil
	a.vacuumOnce(t.Context(), st, time.Unix(1_800_000_000, 0), schedule)

	st.stats = store.VacuumStats{Outcome: store.VacuumSkipped}
	a.vacuumOnce(t.Context(), st, time.Unix(1_900_000_000, 0), schedule)

	if got := gauge(t, a.Metrics, "wsaw_last_successful_vacuum_timestamp_seconds"); got != "wsaw_last_successful_vacuum_timestamp_seconds 1900000000" {
		t.Errorf("after a skip the gauge reads %q, want the skip's time", got)
	}

	if got := gauge(t, a.Metrics, "wsaw_vacuum_bytes_reclaimed_total"); got != "wsaw_vacuum_bytes_reclaimed_total 1400" {
		t.Errorf("reclaimed = %q, want the 1400 bytes the one rewrite gave back", got)
	}

	logged.Reset()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	st.stats, st.vacuumErr = store.VacuumStats{Outcome: store.VacuumError}, context.Canceled
	a.vacuumOnce(ctx, st, time.Now(), schedule)

	if strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "interrupted by shutdown") {
		t.Errorf("a vacuum cancelled by shutdown was logged as:\n%s", logged.String())
	}
}

// newTestLogger logs as text into buf, safely from the loop's goroutine.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(&syncWriter{w: buf}, nil))
}
