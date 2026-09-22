package capture

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

const beaconURL = "https://trc-events.taboola.com/1419468/log/3/unip?en=pre_d_eng_tb&tos=30"

func beaconRecorder(t *testing.T, stallAfter time.Duration) *recorder {
	t.Helper()

	cl, err := classify.New("https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}

	n, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	beacons, err := CompileBeacons([]Beacon{{Host: "trc-events.taboola.com"}})
	if err != nil {
		t.Fatal(err)
	}

	return newRecorder(time.Now(), cl, n, nil, 100, 0, stallAfter, DefaultMaxBodyBytes, false, nil, beacons)
}

// TestBeaconIsRecordedButNotWaitedFor is the whole of Story 1.10 at the
// recorder level: the heartbeat is in the result, and it is not in the
// in-flight count that decides when the scan ends.
func TestBeaconIsRecordedButNotWaitedFor(t *testing.T) {
	t.Parallel()

	r := beaconRecorder(t, DefaultStallAfter)

	r.requestWillBeSent(willBeSent("1", beaconURL, "GET", network.ResourceTypeXHR))

	if inflight, _ := r.snapshot(); inflight != 0 {
		t.Errorf("inflight = %d while a beacon is open, want 0", inflight)
	}

	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", EncodedDataLength: 42, Timestamp: mono(0)})

	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("recorded %d requests, want the beacon kept", len(reqs))
	}

	if !reqs[0].Beacon {
		t.Error("the recorded request is not marked as a beacon; a reader cannot tell what the scan declined to wait for")
	}

	if reqs[0].TransferSize != 42 {
		t.Errorf("TransferSize = %d, want the beacon accounted like any other request", reqs[0].TransferSize)
	}
}

// TestBeaconAccountingLeavesRealRequestsAlone guards the bookkeeping across a
// beacon's whole lifecycle. A decrement for a request that was never counted
// would silently cancel out a real one, and the scan would stop while the
// page was still loading.
func TestBeaconAccountingLeavesRealRequestsAlone(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		finish func(r *recorder)
	}{
		{
			name: "completed",
			finish: func(r *recorder) {
				r.loadingFinished(&network.EventLoadingFinished{RequestID: "b", Timestamp: mono(0)})
			},
		},
		{
			name: "failed",
			finish: func(r *recorder) {
				r.loadingFailed(&network.EventLoadingFailed{
					RequestID: "b", ErrorText: "net::ERR_ABORTED", Timestamp: mono(0),
				})
			},
		},
		{
			name: "redirected",
			finish: func(r *recorder) {
				ev := willBeSent("b", beaconURL+"&hop=2", "GET", network.ResourceTypeXHR)
				ev.RedirectResponse = &network.Response{URL: beaconURL, Status: 302}
				r.requestWillBeSent(ev)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := beaconRecorder(t, time.Hour)

			// One genuine request the scan must keep waiting for.
			r.requestWillBeSent(willBeSent("real", "https://example.com/app.js", "GET", network.ResourceTypeScript))
			r.requestWillBeSent(willBeSent("b", beaconURL, "GET", network.ResourceTypeXHR))

			tc.finish(r)

			if inflight, _ := r.snapshot(); inflight != 1 {
				t.Errorf("inflight = %d, want 1: the page's own request must still hold the scan open", inflight)
			}
		})
	}
}

// TestSettleEndsWhileABeaconKeepsFiring reproduces the bug this story fixes.
// Taboola's time-on-site beacon repeats every 10s against a 10s quiet window,
// so the window is restarted a fraction before it ever elapses and the scan
// runs to its hard timeout with nothing left to observe. The same shape,
// scaled down.
//
// The fixture's beacon is a goroutine, so the ratio between the two durations
// is what the test really rests on: the network is only "never quiet" for as
// long as that goroutine keeps being scheduled. At one beacon per 40ms against
// a 100ms window, two missed wake-ups were a real quiet window, and a loaded
// CI machine produced exactly that — settle returned at 1.9945s of a 2s budget
// because the fixture stalled at the end, and the case that asserts the bug
// reproduces failed. Twenty-five beacons per window means a slip has to last
// the whole window to be mistaken for silence.
func TestSettleEndsWhileABeaconKeepsFiring(t *testing.T) {
	t.Parallel()

	const (
		quiet    = 500 * time.Millisecond
		interval = 20 * time.Millisecond
		budget   = 3 * time.Second
	)

	for _, tc := range []struct {
		name      string
		rules     []Beacon
		wantIdle  bool
		wantAfter time.Duration
		// minHeld is how long settle must stay blocked for the beacon to
		// count as having held the scan open.
		minHeld time.Duration
	}{
		{
			name:      "with the rule configured",
			rules:     []Beacon{{Host: "trc-events.taboola.com"}},
			wantIdle:  true,
			wantAfter: budget / 2,
		},
		{
			name:     "without it, the way the bug behaved",
			rules:    nil,
			wantIdle: false,
			minHeld:  2 * budget / 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cl, err := classify.New("https://example.com/", nil)
			if err != nil {
				t.Fatal(err)
			}

			n, err := normalize.New(normalize.Rules{})
			if err != nil {
				t.Fatal(err)
			}

			beacons, err := CompileBeacons(tc.rules)
			if err != nil {
				t.Fatal(err)
			}

			rec := newRecorder(time.Now(), cl, n, nil, 10_000, 0, time.Hour,
				DefaultMaxBodyBytes, false, nil, beacons)

			s := &session{opts: Options{IdleQuiet: quiet}, rec: rec}

			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()

			// The page itself is done; only the heartbeat is left.
			go func() {
				for i := 0; ctx.Err() == nil; i++ {
					id := network.RequestID(fmt.Sprintf("beacon-%d", i))
					rec.requestWillBeSent(willBeSent(string(id), beaconURL, "GET", network.ResourceTypeXHR))
					rec.loadingFinished(&network.EventLoadingFinished{RequestID: id, Timestamp: mono(0)})

					select {
					case <-ctx.Done():
					case <-time.After(interval):
					}
				}
			}()

			start := time.Now()
			s.settle(ctx)
			elapsed := time.Since(start)

			// What the unconfigured case is about is that settle kept
			// waiting, not that the context won the last millisecond of the
			// race: a run that blocked for four quiet windows has reproduced
			// the bug whether or not the deadline had formally expired. The
			// regression it guards against returns in about one window.
			switch {
			case tc.wantIdle && ctx.Err() != nil:
				t.Errorf("settle ran out the whole budget (%v); the beacon still held the scan open", elapsed)
			case tc.wantIdle && elapsed > tc.wantAfter:
				t.Errorf("settle took %v, want it to end within %v of the page going quiet", elapsed, tc.wantAfter)
			case !tc.wantIdle && elapsed < tc.minHeld:
				t.Errorf("settle returned after %v with the beacon still firing and no rule to exclude it; want it held for at least %v",
					elapsed, tc.minHeld)
			}
		})
	}
}
