package daemon_test

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A restart must not re-scan everything.
//
// The scheduler's idea of when a target last ran lives in memory, so after a
// restart it is empty and every target looks as though it had never been
// scanned: the minimum interval is skipped and the whole list fires inside
// the startup jitter window. For a daemon that restarts on every deploy that
// turns a 24-hour schedule into "on every deploy", against somebody else's
// site (Tenet 17).

// scheduledTarget is a target with a real-world schedule rather than the
// millisecond one the pacing tests use.
func scheduledTarget(name string, interval, minInterval time.Duration) config.Resolved {
	return config.Resolved{
		Name:         name,
		URL:          "https://example.com/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		Interval:     interval,
		MinInterval:  minInterval,
		// A tiny jitter window, so the startup delay is milliseconds: a
		// scheduler that wrongly thinks this target is due will scan inside
		// the test's window rather than 30 seconds later, where a timeout
		// would hide the bug.
		Jitter: 20 * time.Millisecond,
	}
}

func TestARestartDoesNotRescanATargetScannedRecently(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{}

	// The store says this target was scanned a minute ago. Its interval is a
	// day and its floor is an hour, so nothing should happen now.
	lastScan := time.Now().Add(-time.Minute)

	d, err := daemon.New(fake, nil,
		[]config.Resolved{scheduledTarget("site", 24*time.Hour, time.Hour)},
		daemon.Options{
			Concurrency: 4, PerOriginConcurrency: 4, Tick: 2 * time.Millisecond,
			LastScan: func(string, model.ConsentMode) (time.Time, bool) { return lastScan, true },
		})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	// Long enough that a scheduler which thought this target was due would
	// have run it.
	time.Sleep(150 * time.Millisecond)

	if got := fake.count(); got != 0 {
		t.Errorf("a target scanned a minute ago was scanned %d more times after a restart", got)
	}

	// And the schedule reflects the real history rather than starting over.
	jobs := d.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs", len(jobs))
	}

	if jobs[0].LastRun.IsZero() {
		t.Error("the job reports no last run, so the API and the dashboard would say never scanned")
	}

	if want := lastScan.Add(24 * time.Hour); jobs[0].NextRun.Before(want.Add(-time.Minute)) {
		t.Errorf("next run is %s, want about %s: the interval restarted instead of continuing",
			jobs[0].NextRun.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The other half: a target that really is overdue must still be scanned after
// a restart, or the fix would trade a scan storm for a watcher that stops
// watching (Tenet 8).
func TestARestartStillScansAnOverdueTarget(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{}

	lastScan := time.Now().Add(-48 * time.Hour)

	d, err := daemon.New(fake, nil,
		[]config.Resolved{scheduledTarget("site", 24*time.Hour, time.Hour)},
		daemon.Options{
			Concurrency: 4, PerOriginConcurrency: 4, Tick: 2 * time.Millisecond,
			LastScan: func(string, model.ConsentMode) (time.Time, bool) { return lastScan, true },
		})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return fake.count() >= 1 }) {
		t.Error("a target overdue by a day was not scanned after a restart")
	}
}

// A target with no history at all is new, and a new target is scanned soon.
func TestANewTargetIsStillScannedAfterARestart(t *testing.T) {
	t.Parallel()

	fake := &fakeScanner{}

	d, err := daemon.New(fake, nil,
		[]config.Resolved{scheduledTarget("site", 24*time.Hour, time.Hour)},
		daemon.Options{
			Concurrency: 4, PerOriginConcurrency: 4, Tick: 2 * time.Millisecond,
			LastScan: func(string, model.ConsentMode) (time.Time, bool) { return time.Time{}, false },
		})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return fake.count() >= 1 }) {
		t.Error("a target that has never been scanned was not scanned")
	}
}
