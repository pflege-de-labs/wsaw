package daemon_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// The scheduler is deliberately free of any browser dependency, so all of its
// behaviour — pacing, per-origin limits, reload, shutdown — is testable
// without Chrome (Tenet 13).

type fakeScanner struct {
	mu    sync.Mutex
	calls []call

	// block, when non-nil, holds every scan until it is closed.
	block chan struct{}

	// onScan runs inside the scan, for tests that need to observe timing.
	onScan func()
}

type call struct {
	target string
	mode   model.ConsentMode
	at     time.Time
}

func (f *fakeScanner) Scan(ctx context.Context, target config.Resolved, mode model.ConsentMode) (scanner.Outcome, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{target: target.Name, mode: mode, at: time.Now()})
	block := f.block
	onScan := f.onScan
	f.mu.Unlock()

	if onScan != nil {
		onScan()
	}

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return scanner.Outcome{}, ctx.Err()
		}
	}

	return scanner.Outcome{
		Result: &model.Result{
			Target:      target.Name,
			ConsentMode: mode,
			Termination: model.TermIdle,
		},
	}, nil
}

func (f *fakeScanner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

func (f *fakeScanner) countFor(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.calls {
		if c.target == target {
			n++
		}
	}

	return n
}

func target(name, url string, modes ...model.ConsentMode) config.Resolved {
	if len(modes) == 0 {
		modes = []model.ConsentMode{model.ConsentReject}
	}

	return config.Resolved{
		Name:         name,
		URL:          url,
		ConsentModes: modes,
		Interval:     50 * time.Millisecond,
	}
}

// waitFor polls until cond holds or the deadline passes, so tests never rely
// on a fixed sleep to paper over a race.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(2 * time.Millisecond)
	}

	return cond()
}

func run(t *testing.T, d *daemon.Daemon) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() { errCh <- d.Run(ctx) }()

	t.Cleanup(func() {
		cancel()

		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down")
		}
	})

	return cancel, errCh
}

func TestEachConsentModeBecomesItsOwnJob(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil,
		[]config.Resolved{target("site", "https://example.com/", model.ConsentNone, model.ConsentReject, model.ConsentAccept)},
		daemon.Options{Concurrency: 4, PerOriginConcurrency: 4, CatchUp: true, Tick: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	if got := len(d.Jobs()); got != 3 {
		t.Fatalf("got %d jobs, want 3 — one per consent mode", got)
	}

	run(t, d)

	if !waitFor(t, 2*time.Second, func() bool { return fake.count() >= 3 }) {
		t.Fatalf("only %d scans ran", fake.count())
	}

	modes := make(map[model.ConsentMode]bool)

	fake.mu.Lock()
	for _, c := range fake.calls {
		modes[c.mode] = true
	}
	fake.mu.Unlock()

	if len(modes) != 3 {
		t.Errorf("scanned modes = %v, want all three", modes)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		running int
		peak    int
	)

	fake := &fakeScanner{block: make(chan struct{})}
	fake.onScan = func() {
		mu.Lock()
		running++

		if running > peak {
			peak = running
		}

		mu.Unlock()
	}

	targets := make([]config.Resolved, 0, 8)
	for i := range 8 {
		targets = append(targets, target(string(rune('a'+i)), "https://host"+string(rune('a'+i))+".example.com/"))
	}

	d, err := daemon.New(fake, nil, targets, daemon.Options{
		Concurrency: 2, PerOriginConcurrency: 4, CatchUp: true, Tick: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return running >= 2
	})

	// Give the scheduler a chance to over-dispatch if it were going to.
	time.Sleep(100 * time.Millisecond)

	close(fake.block)

	mu.Lock()
	defer mu.Unlock()

	if peak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", peak)
	}
}

// TestPerOriginLimit stops a target list full of one site's URLs from hitting
// that site with the whole worker pool at once.
func TestPerOriginLimit(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		running int
		peak    int
	)

	fake := &fakeScanner{block: make(chan struct{})}
	fake.onScan = func() {
		mu.Lock()
		running++

		if running > peak {
			peak = running
		}

		mu.Unlock()
	}

	// Six targets, all on one origin.
	var targets []config.Resolved

	for i := range 6 {
		targets = append(targets, target("t"+string(rune('a'+i)), "https://example.com/page"+string(rune('a'+i))))
	}

	d, err := daemon.New(fake, nil, targets, daemon.Options{
		Concurrency: 6, PerOriginConcurrency: 1, CatchUp: true, Tick: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return running >= 1
	})

	time.Sleep(100 * time.Millisecond)

	close(fake.block)

	mu.Lock()
	defer mu.Unlock()

	if peak > 1 {
		t.Errorf("peak concurrency against one origin = %d, want at most 1", peak)
	}
}

// TestMinIntervalIsAFloor: a misconfigured schedule must not be able to
// hammer an origin.
func TestMinIntervalIsAFloor(t *testing.T) {
	t.Parallel()

	tgt := target("site", "https://example.com/")
	tgt.Interval = time.Millisecond // absurdly fast
	tgt.MinInterval = time.Hour     // but the floor says no

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil, []config.Resolved{tgt}, daemon.Options{
		Concurrency: 4, PerOriginConcurrency: 4, CatchUp: true, Tick: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	waitFor(t, time.Second, func() bool { return fake.count() >= 1 })

	time.Sleep(150 * time.Millisecond)

	if got := fake.count(); got != 1 {
		t.Errorf("ran %d scans despite a one-hour minimum interval, want 1", got)
	}
}

// TestNoCatchUpSpreadsStartup: a restart must not fire every target at once.
func TestNoCatchUpSpreadsStartup(t *testing.T) {
	t.Parallel()

	var targets []config.Resolved

	// The startup delay is deterministic -- an FNV hash of "name/mode" -- so
	// these names are not arbitrary: each one lands more than three seconds
	// into the ten second window. Names whose delay falls near the assertion
	// below make this test a race against the scheduler rather than a check
	// of the jitter, which is how it used to fail on a loaded runner.
	for _, name := range []string{"te", "th", "tl", "to"} {
		tgt := target(name, "https://host"+name+".example.com/")
		tgt.Jitter = 10 * time.Second
		targets = append(targets, tgt)
	}

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil, targets, daemon.Options{
		Concurrency: 4, PerOriginConcurrency: 4, CatchUp: false, Tick: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	time.Sleep(120 * time.Millisecond)

	// With a ten second jitter window and no target due for another three
	// seconds, nothing at all should have run yet.
	if got := fake.count(); got != 0 {
		t.Errorf("%d scans ran immediately after startup; the jitter window was not applied", got)
	}

	// The schedule must still be populated, not empty.
	if len(d.Jobs()) != 4 {
		t.Errorf("got %d jobs, want 4", len(d.Jobs()))
	}
}

func TestReloadReplacesTargets(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil, []config.Resolved{target("old", "https://old.example.com/")},
		daemon.Options{Concurrency: 4, PerOriginConcurrency: 4, CatchUp: true, Tick: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	waitFor(t, time.Second, func() bool { return fake.countFor("old") >= 1 })

	d.Reload([]config.Resolved{target("new", "https://new.example.com/")})

	if !waitFor(t, 2*time.Second, func() bool { return fake.countFor("new") >= 1 }) {
		t.Fatal("the reloaded target never ran")
	}

	jobs := d.Jobs()
	if len(jobs) != 1 || jobs[0].Target != "new" {
		t.Errorf("jobs after reload = %+v, want only the new target", jobs)
	}
}

// TestReloadPreservesLastRun stops a reload from being a way, accidental or
// otherwise, to bypass the minimum scan interval.
func TestReloadPreservesLastRun(t *testing.T) {
	t.Parallel()

	tgt := target("site", "https://example.com/")
	tgt.MinInterval = time.Hour

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil, []config.Resolved{tgt}, daemon.Options{
		Concurrency: 4, PerOriginConcurrency: 4, CatchUp: true, Tick: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	waitFor(t, time.Second, func() bool { return fake.count() >= 1 })

	// Reload the same target: it must not become eligible again.
	d.Reload([]config.Resolved{tgt})

	time.Sleep(150 * time.Millisecond)

	if got := fake.count(); got != 1 {
		t.Errorf("ran %d scans after reload, want 1: the minimum interval was bypassed", got)
	}
}

// TestInvalidReloadKeepsPreviousSchedule: a watcher must not stop watching
// because of a bad edit.
func TestInvalidReloadKeepsPreviousSchedule(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil, []config.Resolved{target("good", "https://example.com/")},
		daemon.Options{Concurrency: 4, PerOriginConcurrency: 4, CatchUp: true, Tick: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	waitFor(t, time.Second, func() bool { return fake.count() >= 1 })

	bad := target("broken", "https://example.com/")
	bad.Cron = "definitely not a cron"

	d.Reload([]config.Resolved{bad})

	time.Sleep(100 * time.Millisecond)

	jobs := d.Jobs()
	if len(jobs) != 1 || jobs[0].Target != "good" {
		t.Errorf("jobs = %+v, want the previous schedule preserved", jobs)
	}
}

func TestGracefulShutdownWaitsForInFlightScans(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	finished := make(chan struct{})

	fake := &fakeScanner{}
	fake.onScan = func() {
		<-release
		close(finished)
	}

	d, err := daemon.New(fake, nil, []config.Resolved{target("site", "https://example.com/")},
		daemon.Options{
			Concurrency: 1, PerOriginConcurrency: 1, CatchUp: true,
			Tick: 5 * time.Millisecond, ShutdownGrace: 5 * time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() { errCh <- d.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return fake.count() >= 1 })

	cancel()

	// The daemon must still be waiting: the scan has not finished.
	select {
	case <-errCh:
		t.Fatal("daemon returned before the in-flight scan finished")
	case <-time.After(80 * time.Millisecond):
	}

	close(release)
	<-finished

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not return after the scan finished")
	}
}

// TestShutdownCancelsScansThatOverrunTheGrace: shutdown must complete even
// when a scan will not.
func TestShutdownCancelsScansThatOverrunTheGrace(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{block: make(chan struct{})} // never released

	d, err := daemon.New(fake, nil, []config.Resolved{target("stuck", "https://example.com/")},
		daemon.Options{
			Concurrency: 1, PerOriginConcurrency: 1, CatchUp: true,
			Tick: 5 * time.Millisecond, ShutdownGrace: 50 * time.Millisecond,
		})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() { errCh <- d.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return fake.count() >= 1 })

	cancel()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung on a scan that never finishes")
	}
}

func TestSinkReceivesOutcomes(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		received int
	)

	sink := daemon.SinkFunc(func(_ context.Context, out scanner.Outcome) {
		mu.Lock()
		defer mu.Unlock()

		if out.Result != nil {
			received++
		}
	})

	fake := &fakeScanner{}

	d, err := daemon.New(fake, sink, []config.Resolved{target("site", "https://example.com/")},
		daemon.Options{Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return received >= 1
	}) {
		t.Fatal("the sink never received an outcome")
	}
}

func TestNewRejectsInvalidCron(t *testing.T) {
	t.Parallel()

	tgt := target("t", "https://example.com/")
	tgt.Cron = "nonsense"

	if _, err := daemon.New(&fakeScanner{}, nil, []config.Resolved{tgt}, daemon.Options{}); err == nil {
		t.Fatal("New accepted an invalid cron expression")
	}
}

func TestNewRequiresScanner(t *testing.T) {
	t.Parallel()

	if _, err := daemon.New(nil, nil, nil, daemon.Options{}); err == nil {
		t.Fatal("New accepted a nil scanner")
	}
}
