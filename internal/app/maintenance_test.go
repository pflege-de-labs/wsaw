package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The daemon's maintenance schedule (Story 4.12, AC9). Every test here runs
// the real loop against a clock the test moves: nothing waits on a real timer,
// and the jitter is fixed, so every due time below is exact.

// testJitter is the fixed jitter every test clock uses: the middle of the
// window, so a test that got the window wrong is off by a visible amount.
func testJitter(window time.Duration) time.Duration { return window / 2 }

// firstSweepDelay is when a store that has never swept is first swept under
// the test clock.
const firstSweepDelay = sweepStartupDelay + sweepStartupJitter/2

// fakeClock is a clock the test moves. Every timer the loop arms is announced
// on armed, which is how a test knows the loop has finished whatever it was
// doing and is waiting again — the synchronisation a sleep would only guess at.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer

	armed chan time.Duration
}

type fakeTimer struct {
	at   time.Time
	c    chan time.Time
	done bool
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start, armed: make(chan time.Duration, 64)}
}

func (c *fakeClock) clock() maintenanceClock {
	return maintenanceClock{now: c.Now, timer: c.timer, jitter: testJitter}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	t := &fakeTimer{at: c.now.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.mu.Unlock()

	c.armed <- d

	return t.c, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		t.done = true
	}
}

// set moves the clock to at and fires every timer that is then due.
func (c *fakeClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = at

	for _, t := range c.timers {
		if !t.done && !t.at.After(at) {
			t.done = true
			t.c <- at
		}
	}
}

// waitArmed returns the delay of the next timer the loop arms.
func (c *fakeClock) waitArmed(t *testing.T) time.Duration {
	t.Helper()

	select {
	case d := <-c.armed:
		return d
	case <-t.Context().Done():
		t.Fatal("the maintenance loop never armed a timer")

		return 0
	}
}

// maintenanceStore is a store that records what the maintenance loop asks of
// it. Only Prune, Sweep and LastMaintenanceRun are real; anything else the
// loop has no business calling panics through the nil embedded interface.
type maintenanceStore struct {
	store.Store

	mu sync.Mutex

	clock *fakeClock

	// last is the receipt LastMaintenanceRun reports, and every sweep
	// replaces it, as the real store's own receipt would.
	last  *store.MaintenanceRun
	reads int

	sweeps   []store.SweepOptions
	triggers []string
	prunes   int

	// busy is whichever operation is running; overlapped records that one
	// started while the other had not finished.
	busy       string
	overlapped bool

	// during runs inside every sweep, while it is still in progress.
	during   func()
	sweepErr error
}

func (s *maintenanceStore) enter(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.busy != "" {
		s.overlapped = true
	}

	s.busy = op
}

func (s *maintenanceStore) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.busy = ""
}

func (s *maintenanceStore) Prune(context.Context, string, time.Time, store.Retention) (store.PruneStats, error) {
	s.enter("prune")
	defer s.leave()

	s.mu.Lock()
	s.prunes++
	s.mu.Unlock()

	return store.PruneStats{}, nil
}

func (s *maintenanceStore) Sweep(
	_ context.Context, trigger string, now time.Time, opts store.SweepOptions,
) (store.SweepStats, error) {
	s.enter("sweep")
	defer s.leave()

	if s.during != nil {
		s.during()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweeps = append(s.sweeps, opts)
	s.triggers = append(s.triggers, trigger)
	s.last = &store.MaintenanceRun{Kind: store.MaintenanceKindSweep, StartedAt: now, FinishedAt: s.clock.Now()}

	return store.SweepStats{ArtifactsScanned: 3, ArtifactsDeleted: 1, BytesFreed: 10}, s.sweepErr
}

func (s *maintenanceStore) LastMaintenanceRun(context.Context, string) (store.MaintenanceRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reads++

	if s.last == nil {
		return store.MaintenanceRun{}, false, nil
	}

	return *s.last, true, nil
}

func (s *maintenanceStore) sweepCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.sweeps)
}

// maintenanceApp is an App with nothing but what the maintenance loop uses.
// Retention is off unless a test turns it on, so the only timer the loop arms
// is the sweep's and each armed delay reads directly as when it is due.
func maintenanceApp(t *testing.T, st *maintenanceStore, mutate func(*config.Config)) (*App, *bytes.Buffer) {
	t.Helper()

	cfg := config.New()
	cfg.Store.MaxPerSeries = 0

	if mutate != nil {
		mutate(cfg)
	}

	var logged bytes.Buffer

	return &App{
		Config:  cfg,
		Logger:  slog.New(slog.NewTextHandler(&syncWriter{w: &logged}, nil)),
		Metrics: metrics.New("test"),
		Store:   st,
	}, &logged
}

// syncWriter serialises writes to a buffer the loop's goroutine logs into.
type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.w.Write(p)
}

// runLoop starts the maintenance loop and returns the channel reloads go
// through and a function that stops the loop and waits for it to return.
func runLoop(t *testing.T, a *App, clock *fakeClock) (chan SweepSchedule, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	sweeps := make(chan SweepSchedule, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)

		a.maintenanceLoop(ctx, sweeps, clock.clock())
	}()

	stop := func() {
		cancel()
		<-done
	}

	t.Cleanup(stop)

	return sweeps, stop
}

func gauge(t *testing.T, r *metrics.Registry, name string) string {
	t.Helper()

	var out strings.Builder

	if err := r.WritePrometheus(&out); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}

	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			return line
		}
	}

	return ""
}

// TestTheDefaultScheduleSweepsDaily is the first bullet of AC9 from the
// loop's side: a configuration that says nothing about sweeping produces a
// schedule that sweeps, every 24 hours.
func TestTheDefaultScheduleSweepsDaily(t *testing.T) {
	t.Parallel()

	got := SweepScheduleFor(config.New())

	if want := (SweepSchedule{Enabled: true, Interval: 24 * time.Hour}); got != want {
		t.Errorf("SweepScheduleFor(config.New()) = %+v, want %+v", got, want)
	}
}

// TestTheFirstSweepStartsPromptlyWhenNoneWasEverRecorded is AC3's first-run
// half: a store with no receipt is swept shortly after startup, after the
// bounded, jittered delay — and then one interval after that sweep, not one
// interval after startup.
func TestTheFirstSweepStartsPromptlyWhenNoneWasEverRecorded(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := &maintenanceStore{clock: clock}
	a, _ := maintenanceApp(t, st, nil)

	runLoop(t, a, clock)

	if d := clock.waitArmed(t); d != firstSweepDelay {
		t.Fatalf("the first sweep of a store that has never swept is due after %s, want %s", d, firstSweepDelay)
	}

	clock.set(start.Add(firstSweepDelay))

	if d := clock.waitArmed(t); d != 24*time.Hour {
		t.Errorf("after the first sweep the next is due in %s, want 24h", d)
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.sweeps) != 1 {
		t.Fatalf("%d sweeps ran, want 1", len(st.sweeps))
	}

	// The same Sweep the command runs, said to be the schedule's, and never
	// with the override only an operator may give (AC4).
	if st.triggers[0] != store.TriggerSchedule || st.sweeps[0].AllowEmptyIndex {
		t.Errorf("the scheduled sweep ran with trigger %q and %+v", st.triggers[0], st.sweeps[0])
	}
}

// TestTheScheduleSurvivesARestart is AC3's restart half: a process that
// starts twenty hours after the last recorded sweep sweeps four hours later,
// not a day after it started — and one that starts after the interval has
// already passed sweeps after the startup delay, not at once.
func TestTheScheduleSurvivesARestart(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		sinceLast time.Duration
		want      time.Duration
	}{
		"due later today": {sinceLast: 20 * time.Hour, want: 4 * time.Hour},
		"already overdue": {sinceLast: 30 * time.Hour, want: firstSweepDelay},
		"due inside the startup delay": {
			// Due in five minutes, but a restart never sweeps sooner than the
			// floor: a crash-looping daemon must not list its bucket on every
			// restart.
			sinceLast: 24*time.Hour - 5*time.Minute, want: firstSweepDelay,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The first process sweeps and records a receipt, then stops.
			first := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
			clock := newFakeClock(first)
			st := &maintenanceStore{clock: clock}
			a, _ := maintenanceApp(t, st, nil)

			_, stop := runLoop(t, a, clock)
			clock.waitArmed(t)
			clock.set(first.Add(firstSweepDelay))
			clock.waitArmed(t)
			stop()

			swept := first.Add(firstSweepDelay)

			// The second process starts later, with only the receipt to go by.
			restarted := swept.Add(tc.sinceLast)
			clock2 := newFakeClock(restarted)
			st.clock = clock2
			a2, _ := maintenanceApp(t, st, nil)

			runLoop(t, a2, clock2)

			if d := clock2.waitArmed(t); d != tc.want {
				t.Errorf("a process started %s after the last sweep sweeps after %s, want %s", tc.sinceLast, d, tc.want)
			}
		})
	}
}

// TestSweepingOffRunsNoSweepAtAll is AC9's "explicit off disables the loop
// entirely": over two days of hourly prunes, sweep: false lists nothing and
// records nothing — it does not even read the receipt log.
func TestSweepingOffRunsNoSweepAtAll(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := &maintenanceStore{clock: clock}
	a, _ := maintenanceApp(t, st, func(c *config.Config) {
		off := false
		c.Store.Sweep = &off
		c.Store.MaxPerSeries = 10 // prunes arm the only timer there is
	})

	runLoop(t, a, clock)

	now := start

	for range 48 {
		d := clock.waitArmed(t)
		now = now.Add(d)
		clock.set(now)
	}

	clock.waitArmed(t)

	st.mu.Lock()
	defer st.mu.Unlock()

	if len(st.sweeps) != 0 || st.reads != 0 {
		t.Errorf("with sweeping off the loop swept %d times and read the receipt log %d times, want neither",
			len(st.sweeps), st.reads)
	}

	if st.prunes != 48 {
		t.Errorf("%d prunes ran in 48 hours, want 48: turning the sweep off must not stop retention", st.prunes)
	}
}

// TestAPruneAndASweepNeverOverlap is AC5. Both fall due at the same instant,
// and the sweep then runs for longer than its own interval: the prune waits
// for it, and the sweep that fell due while it ran is not started a second
// time — the next is one interval after this one finished.
func TestAPruneAndASweepNeverOverlap(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := &maintenanceStore{clock: clock}

	sweepTook := 30 * time.Hour
	finished := start.Add(time.Hour).Add(sweepTook)

	st.during = func() { clock.set(finished) }
	st.last = &store.MaintenanceRun{FinishedAt: start.Add(time.Hour - 24*time.Hour)}

	a, _ := maintenanceApp(t, st, func(c *config.Config) { c.Store.MaxPerSeries = 10 })

	runLoop(t, a, clock)

	// The prune is hourly and the sweep is due at the same hour.
	if d := clock.waitArmed(t); d != time.Hour {
		t.Fatalf("first wake after %s, want 1h", d)
	}

	clock.set(start.Add(time.Hour))

	// The prune ran first and the sweep then held the loop for thirty hours,
	// so the next prune is overdue: it is what runs next, at once.
	if d := clock.waitArmed(t); d > 0 {
		t.Errorf("after the long sweep the next wake is in %s, want the overdue prune now", d)
	}

	clock.set(finished)

	// And then the prune's own hour — not a second sweep, which is due a
	// whole interval after the long one finished.
	if d := clock.waitArmed(t); d != time.Hour {
		t.Errorf("after the overdue prune the next wake is in %s, want the next prune in 1h", d)
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.overlapped {
		t.Error("a prune and a sweep ran at the same time")
	}

	if st.prunes != 2 || len(st.sweeps) != 1 {
		t.Errorf("%d prunes and %d sweeps ran, want 2 and 1", st.prunes, len(st.sweeps))
	}
}

// TestAScheduledSweepNeverAllowsAnEmptyIndex is AC4 against a real store: the
// daemon's sweep of a store that holds nothing is refused, recorded as a
// receipt with its error, counted as a failure, logged as a warning that names
// the command an operator would use — and not retried: the loop's next sweep
// is a whole interval away.
func TestAScheduledSweepNeverAllowsAnEmptyIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	s, err := store.OpenSQL(t.Context(), store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	var logged bytes.Buffer

	a := &App{
		Config:  config.New(),
		Logger:  slog.New(slog.NewTextHandler(&syncWriter{w: &logged}, nil)),
		Metrics: metrics.New("test"),
		Store:   s,
	}

	a.sweepOnce(t.Context(), time.Now())

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindSweep)
	if err != nil || !found {
		t.Fatalf("LastMaintenanceRun = found %v, error %v; the refusal must leave a receipt", found, err)
	}

	if run.Trigger != store.TriggerSchedule || !strings.Contains(run.Error, "index holds nothing") {
		t.Errorf("receipt = trigger %q, error %q; want a scheduled run refused for its empty index", run.Trigger, run.Error)
	}

	if !strings.Contains(logged.String(), "level=WARN") ||
		!strings.Contains(logged.String(), "wsaw store sweep --allow-empty-index") {
		t.Errorf("the refusal was not logged as a warning naming the override:\n%s", logged.String())
	}

	if got := gauge(t, a.Metrics, "wsaw_sweep_runs_total"); got != `wsaw_sweep_runs_total{outcome="error"} 1` {
		t.Errorf("sweep counter = %q, want one error", got)
	}

	if got := gauge(t, a.Metrics, "wsaw_last_successful_sweep_timestamp_seconds"); !strings.HasSuffix(got, " 0") {
		t.Errorf("a refused sweep moved the last-successful-sweep gauge: %q", got)
	}
}

// TestARefusedSweepWaitsAWholeInterval is AC4's "never retried in a loop",
// from the loop's side.
func TestARefusedSweepWaitsAWholeInterval(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := &maintenanceStore{clock: clock, sweepErr: fmt.Errorf("sweep: %w", store.ErrEmptyIndex)}
	a, _ := maintenanceApp(t, st, nil)

	runLoop(t, a, clock)

	clock.waitArmed(t)
	clock.set(start.Add(firstSweepDelay))

	if d := clock.waitArmed(t); d != 24*time.Hour {
		t.Errorf("a refused sweep is tried again after %s, want the full interval", d)
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	for _, opts := range st.sweeps {
		if opts.AllowEmptyIndex {
			t.Error("a scheduled sweep set AllowEmptyIndex")
		}
	}
}

// TestTheSweepMetricsMoveOnSuccessAndNotOnFailure is AC6 and AC9's last
// bullet: the outcome counter counts both, the gauge moves only on success,
// and a sweep's deletions join the artifact counters a prune already feeds.
func TestTheSweepMetricsMoveOnSuccessAndNotOnFailure(t *testing.T) {
	t.Parallel()

	st := &maintenanceStore{clock: newFakeClock(time.Now())}
	a, logged := maintenanceApp(t, st, nil)

	st.sweepErr = errors.New("the bucket went away")
	a.sweepOnce(t.Context(), time.Unix(1_700_000_000, 0))

	if got := gauge(t, a.Metrics, "wsaw_last_successful_sweep_timestamp_seconds"); !strings.HasSuffix(got, " 0") {
		t.Errorf("a failed sweep moved the gauge: %q", got)
	}

	if !strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "artifacts_deleted=1") {
		t.Errorf("a failed sweep was not logged as an error with its stats:\n%s", logged.String())
	}

	st.sweepErr = nil
	a.sweepOnce(t.Context(), time.Unix(1_800_000_000, 0))

	if got := gauge(t, a.Metrics, "wsaw_last_successful_sweep_timestamp_seconds"); got != "wsaw_last_successful_sweep_timestamp_seconds 1800000000" {
		t.Errorf("after a successful sweep the gauge reads %q, want its time", got)
	}

	var out strings.Builder
	if err := a.Metrics.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`wsaw_sweep_runs_total{outcome="error"} 1`,
		`wsaw_sweep_runs_total{outcome="success"} 1`,
		"wsaw_artifacts_deleted_total 2",
		"wsaw_artifact_bytes_freed_total 20",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metrics do not contain %q:\n%s", want, out.String())
		}
	}
}

// TestASweepInterruptedByShutdownIsNotReportedAsAFailure: a cancelled sweep is
// counted, since it did not succeed, but it is a shutdown and is logged as
// one rather than as an error somebody has to go and look at (AC7).
func TestASweepInterruptedByShutdownIsNotReportedAsAFailure(t *testing.T) {
	t.Parallel()

	st := &maintenanceStore{clock: newFakeClock(time.Now()), sweepErr: context.Canceled}
	a, logged := maintenanceApp(t, st, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	a.sweepOnce(ctx, time.Now())

	if strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "interrupted by shutdown") {
		t.Errorf("a sweep cancelled by shutdown was logged as:\n%s", logged.String())
	}

	if got := gauge(t, a.Metrics, "wsaw_last_successful_sweep_timestamp_seconds"); !strings.HasSuffix(got, " 0") {
		t.Errorf("an interrupted sweep moved the gauge: %q", got)
	}
}

// TestAReloadAppliesTheSweepSchedule is AC8: a reload that turns sweeping off
// stops the next sweep, one that turns it back on reschedules it from the
// receipt, and one that changes nothing does not push the next sweep back.
func TestAReloadAppliesTheSweepSchedule(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	st := &maintenanceStore{clock: clock}
	st.last = &store.MaintenanceRun{FinishedAt: start.Add(-20 * time.Hour)}
	a, _ := maintenanceApp(t, st, func(c *config.Config) { c.Store.MaxPerSeries = 10 })

	sweeps, _ := runLoop(t, a, clock)

	if d := clock.waitArmed(t); d != time.Hour {
		t.Fatalf("first wake after %s, want the prune in 1h", d)
	}

	// Unchanged: the timer is re-armed for exactly the same moment.
	sweeps <- SweepScheduleFor(a.Config)

	if d := clock.waitArmed(t); d != time.Hour {
		t.Errorf("an unchanged schedule moved the next wake to %s", d)
	}

	// Off: across the hours the sweep would have been due, it is not run.
	sweeps <- SweepSchedule{Enabled: false, Interval: 24 * time.Hour}

	clock.waitArmed(t)

	now := start
	for range 6 {
		now = now.Add(time.Hour)
		clock.set(now)
		clock.waitArmed(t)
	}

	if n := st.sweepCount(); n != 0 {
		t.Fatalf("%d sweeps ran after a reload turned sweeping off", n)
	}

	// Back on, every six hours: the receipt is 26 hours old, so the sweep
	// is overdue and starts after the startup delay.
	sweeps <- SweepSchedule{Enabled: true, Interval: 6 * time.Hour}

	if d := clock.waitArmed(t); d != firstSweepDelay {
		t.Errorf("after sweeping was turned back on the next wake is in %s, want the sweep in %s", d, firstSweepDelay)
	}

	clock.set(now.Add(firstSweepDelay))
	clock.waitArmed(t)

	if n := st.sweepCount(); n != 1 {
		t.Errorf("%d sweeps ran after sweeping was turned back on, want 1", n)
	}
}
