package daemon

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// spread used to take the hash modulo the window. Sum32 read as nanoseconds
// stops at about 4.3s, so every wider window was silently truncated to that
// and the shipped five minute jitter spread targets over four seconds. These
// tests pin the two properties that regression violated: the result stays
// inside the window, and a wide window is actually used.
func TestSpreadStaysInsideTheWindow(t *testing.T) {
	t.Parallel()

	windows := []time.Duration{
		time.Nanosecond,
		time.Second,
		30 * time.Second,
		5 * time.Minute,
		24 * time.Hour,
		time.Duration(math.MaxInt64),
	}

	hashes := []uint32{0, 1, math.MaxUint32 / 2, math.MaxUint32 - 1, math.MaxUint32}

	for _, window := range windows {
		for _, hash := range hashes {
			got := spread(hash, window)

			if got < 0 || got >= window {
				t.Errorf("spread(%d, %v) = %v, want within [0, %v)", hash, window, got, window)
			}
		}
	}
}

func TestSpreadUsesTheWholeWindow(t *testing.T) {
	t.Parallel()

	const window = 5 * time.Minute

	// Ten buckets across the window; a capped implementation puts every
	// target in the first one.
	var buckets [10]int

	for i := range 500 {
		h := fnv.New32a()
		_, _ = fmt.Fprintf(h, "target-%d/reject", i)

		buckets[int64(spread(h.Sum32(), window))*10/int64(window)]++
	}

	for i, count := range buckets {
		if count == 0 {
			t.Errorf("no target landed in tenth %d of the window; the window is not being used", i)
		}
	}
}

// A window wider than the old ceiling must produce delays beyond it.
func TestSpreadIsNotCappedAtTheHashWidth(t *testing.T) {
	t.Parallel()

	const (
		window  = 5 * time.Minute
		oldCeil = 4300 * time.Millisecond
	)

	var widest time.Duration

	for i := range 500 {
		h := fnv.New32a()
		_, _ = fmt.Fprintf(h, "target-%d/reject", i)

		if d := spread(h.Sum32(), window); d > widest {
			widest = d
		}
	}

	if widest <= oldCeil {
		t.Errorf("widest delay was %v, still within the %v the modulo used to cap at", widest, oldCeil)
	}
}

// A schedule kept in monotonic time measures awake time rather than elapsed
// time: the monotonic clock stops while the host is suspended, so after a
// laptop sleeps for an hour every job waits that hour out again while the API
// reports a nextRun that has long passed. Go compares two timestamps that both
// carry a monotonic reading by that reading alone, so the only way to keep the
// schedule on the wall clock is for it to hold no monotonic reading at all.
//
// These tests pin exactly that. The divergence between the two clocks cannot
// be manufactured inside a test process — only the host suspending produces it
// — so the invariant is what gets checked instead of the symptom.
func hasMonotonic(t time.Time) bool { return t != t.Round(0) }

func monotonicNow(t *testing.T) time.Time {
	t.Helper()

	now := time.Now()
	if !hasMonotonic(now) {
		t.Skip("time.Now carries no monotonic reading on this platform")
	}

	return now
}

func schedulingTarget() config.Resolved {
	return config.Resolved{
		Name:         "example",
		URL:          "https://example.test/",
		ConsentModes: []model.ConsentMode{model.ConsentNone},
		Interval:     time.Hour,
		MinInterval:  30 * time.Minute,
		Jitter:       10 * time.Minute,
	}
}

func TestScheduleTimesCarryNoMonotonicReading(t *testing.T) {
	t.Parallel()

	now := monotonicNow(t)

	jobs, err := buildJobs([]config.Resolved{schedulingTarget()}, now, false,
		func(string, model.ConsentMode) (time.Time, bool) {
			return now.Add(-2 * time.Hour), true
		})
	if err != nil {
		t.Fatalf("buildJobs: %v", err)
	}

	j := jobs[0]

	if hasMonotonic(j.next) {
		t.Errorf("next after buildJobs carries a monotonic reading: %v", j.next)
	}

	if hasMonotonic(j.lastRun) {
		t.Errorf("lastRun seeded from the store carries a monotonic reading: %v", j.lastRun)
	}

	j.advance(now)

	if hasMonotonic(j.next) {
		t.Errorf("next after advance carries a monotonic reading: %v", j.next)
	}

	j.scheduleRetry(now, time.Minute, "network unreachable")

	if hasMonotonic(j.next) {
		t.Errorf("next after scheduleRetry carries a monotonic reading: %v", j.next)
	}
}

type stubScanner struct{}

func (stubScanner) Scan(context.Context, config.Resolved, model.ConsentMode) (scanner.Outcome, error) {
	return scanner.Outcome{}, nil
}

func TestMarkStartedRecordsWallClockTime(t *testing.T) {
	t.Parallel()

	now := monotonicNow(t)

	d, err := New(stubScanner{}, nil, []config.Resolved{schedulingTarget()}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	j := d.jobs[0]
	d.markStarted(j, now)

	if hasMonotonic(j.lastRun) {
		t.Errorf("lastRun carries a monotonic reading: %v", j.lastRun)
	}

	if hasMonotonic(j.next) {
		t.Errorf("next carries a monotonic reading: %v", j.next)
	}
}
