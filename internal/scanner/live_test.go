package scanner

import (
	"context"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The registry is tested directly rather than through Scan, because Scan needs
// a browser and the property under test — a scan is visible exactly while it
// runs — is a property of this type.

func TestLiveReportsWhatIsRunning(t *testing.T) {
	t.Parallel()

	live := NewLive()

	if got := live.Count(); got != 0 {
		t.Fatalf("a fresh registry reports %d running, want 0", got)
	}

	release := live.begin(Running{
		ScanID: "scan-1", Target: "site", ConsentMode: model.ConsentReject,
		StartedAt: time.Now(), Source: SourceSchedule,
	})

	running := live.Running()
	if len(running) != 1 {
		t.Fatalf("running = %d entries, want 1", len(running))
	}

	if running[0].ScanID != "scan-1" || running[0].Target != "site" {
		t.Errorf("running[0] = %+v, want the scan that was begun", running[0])
	}

	release()

	if got := live.Count(); got != 0 {
		t.Errorf("after release, %d scans still look running", got)
	}
}

// Oldest first, so a refreshing page does not reshuffle under the reader and
// the longest-running scan — the one worth worrying about — is at the top.
func TestRunningIsOrderedOldestFirst(t *testing.T) {
	t.Parallel()

	live := NewLive()
	base := time.Now()

	defer live.begin(Running{ScanID: "newer", StartedAt: base.Add(time.Minute)})()
	defer live.begin(Running{ScanID: "older", StartedAt: base})()

	running := live.Running()
	if len(running) != 2 {
		t.Fatalf("running = %d entries, want 2", len(running))
	}

	if running[0].ScanID != "older" || running[1].ScanID != "newer" {
		t.Errorf("order = %s, %s; want older, newer", running[0].ScanID, running[1].ScanID)
	}
}

// Ties are broken by scan ID: two scans of different targets can start in the
// same instant, and an unstable order would make the list jump between
// refreshes.
func TestRunningOrderIsStableForIdenticalStartTimes(t *testing.T) {
	t.Parallel()

	live := NewLive()
	at := time.Now()

	defer live.begin(Running{ScanID: "b", StartedAt: at})()
	defer live.begin(Running{ScanID: "a", StartedAt: at})()

	for range 20 {
		running := live.Running()
		if running[0].ScanID != "a" {
			t.Fatalf("order is not stable: got %s first", running[0].ScanID)
		}
	}
}

// A nil registry must behave like an empty one, so a component that was built
// without one does not panic on a read.
func TestNilLiveIsUsable(t *testing.T) {
	t.Parallel()

	var live *Live

	if got := live.Count(); got != 0 {
		t.Errorf("nil registry Count = %d, want 0", got)
	}

	if got := live.Running(); got != nil {
		t.Errorf("nil registry Running = %v, want nil", got)
	}

	live.begin(Running{ScanID: "x"})()
}

func TestSourceDefaultsToSchedule(t *testing.T) {
	t.Parallel()

	if got := sourceOf(context.Background()); got != SourceSchedule {
		t.Errorf("unlabelled context source = %q, want %q", got, SourceSchedule)
	}

	ctx := WithSource(context.Background(), SourceAPI)
	if got := sourceOf(ctx); got != SourceAPI {
		t.Errorf("labelled context source = %q, want %q", got, SourceAPI)
	}

	// An empty label must not erase a meaningful default.
	if got := sourceOf(WithSource(context.Background(), "")); got != SourceSchedule {
		t.Errorf("empty label source = %q, want %q", got, SourceSchedule)
	}
}
