package notify_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/notify"
)

// TestAChangeOfBrowserReachesEveryNotification is Story 6.13, AC8: a
// comparison across a change of browser says so in the webhook payload and in
// both Teams formats, and the changes themselves arrive untouched.
func TestAChangeOfBrowserReachesEveryNotification(t *testing.T) {
	t.Parallel()

	rep := teamsReport(change(diff.SeverityHigh, "tracker.example"))
	rep.BrowserChanged = &diff.BrowserChange{
		Before: diff.Browser{Version: "Chromium 152.0.7977.82"},
		After:  diff.Browser{Version: "Chromium 153.0.8000.1", Image: "localhost/wsaw-browser:build-20261015T000000Z"},
	}

	ev := notify.NewScanEvent(teamsResult(nil), rep)
	if ev.BrowserChanged == nil || ev.Highest != diff.SeverityHigh {
		t.Fatalf("the scan event lost the mark or the finding: %+v", ev)
	}

	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	for _, legacy := range []bool{false, true} {
		tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.Legacy = legacy })

		if err := tn.DeliverScan(t.Context(), ev); err != nil {
			t.Fatal(err)
		}

		for _, want := range []string{"The browser changed", "Chromium 152.0.7977.82", "Chromium 153.0.8000.1", "tracker.example"} {
			if !strings.Contains(hits.last(), want) {
				t.Errorf("legacy=%v: the card does not mention %q", legacy, want)
			}
		}
	}

	quiet := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityHigh, "tracker.example")))

	tn := newTeams(t, srv, nil)
	if err := tn.DeliverScan(t.Context(), quiet); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(hits.last(), "The browser changed") {
		t.Error("a comparison on one browser was marked")
	}
}
