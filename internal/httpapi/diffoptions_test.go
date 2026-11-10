package httpapi_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A scan's page ranks its changes with the target's own severity rules, as the
// notification for the same scan does. Otherwise the two disagree about the
// same change, and the notification links to a page that contradicts it.
func TestTheDiffUsesTheTargetsSeverityRules(t *testing.T) {
	t.Parallel()

	var asked atomic.Value

	f := newFixtureWith(t, httpapi.Options{}, nil, nil, func(d *httpapi.Deps) {
		d.DiffOptions = func(target string) diff.Options {
			asked.Store(target)

			return diff.Options{Severity: diff.SeverityRules{ThirdPartyHostRejectMode: diff.SeverityLow}}
		}
	})

	base := time.Now().Add(-time.Hour)
	f.seed("scan-1", model.ConsentReject, base, func(r *model.Result) { r.Requests = r.Requests[:1] })
	f.seed("scan-2", model.ConsentReject, base.Add(time.Minute), nil)

	resp := f.get("/api/v1/diff/site/reject/scan-2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the diff = %d", resp.StatusCode)
	}

	var rep diff.Report
	if err := json.Unmarshal([]byte(body(t, resp)), &rep); err != nil {
		t.Fatal(err)
	}

	if got, _ := asked.Load().(string); got != "site" {
		t.Errorf("settings asked for target %q, want site", got)
	}

	for _, c := range rep.Changes {
		if c.Type == diff.HostAdded && c.Subject == "tracker.test" {
			if c.Severity != diff.SeverityLow {
				t.Errorf("host-added tracker.test ranks %s, want the target's own low", c.Severity)
			}

			return
		}
	}

	t.Fatalf("no host-added change for tracker.test in %+v", rep.Changes)
}
