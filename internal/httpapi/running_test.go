package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/httpapi"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/scanner"
)

// Story 5.12: a scan that is running exists in no stored record, so the
// interface has to be told about it separately. These tests use a stub
// registry rather than a real scan, since the question is what the interface
// does with the information.

func oneRunning() []scanner.Running {
	return []scanner.Running{{
		ScanID:      "scan-live",
		Target:      "site",
		URL:         "https://example.com/",
		ConsentMode: model.ConsentReject,
		StartedAt:   time.Now().Add(-30 * time.Second),
		Source:      scanner.SourceSchedule,
	}}
}

func TestSeriesPageShowsARunningScanAsPending(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{WebUI: true}, nil, oneRunning)

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, "scan-live") {
		t.Error("the running scan's ID is not on the target detail page")
	}

	if !strings.Contains(html, "pending") {
		t.Error(`the running scan's outcome is not stated as "pending" (Story 5.12, AC1)`)
	}

	// A page with a scan in flight must come back on its own, or the operator
	// is watching a snapshot that will never change.
	if !strings.Contains(html, `http-equiv="refresh"`) {
		t.Error("a page showing a running scan does not refresh itself")
	}
}

// A running scan of one series must not appear under another: the pending row
// makes a claim about a specific target and consent mode.
func TestRunningScansAreScopedToTheirSeries(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{WebUI: true}, nil, oneRunning)

	html := body(t, f.get("/targets/site/accept", "Accept", "text/html"))

	if strings.Contains(html, "scan-live") {
		t.Error("a scan running in reject mode is shown on the accept series")
	}

	if strings.Contains(html, `http-equiv="refresh"`) {
		t.Error("a page with nothing running refreshes anyway")
	}
}

func TestDashboardShowsRunningScansAcrossTargets(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{WebUI: true}, nil, oneRunning)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "Running scans") {
		t.Error("the dashboard does not list the scans in flight")
	}

	if !strings.Contains(html, "scan-live") {
		t.Error("the running scan is missing from the dashboard")
	}
}

// An idle daemon and a deployment that cannot scan at all are different
// claims, and reporting the second as the first is the quiet failure Tenet 5
// exists to prevent.
func TestRunningEndpointDistinguishesIdleFromUntracked(t *testing.T) {
	t.Parallel()

	type payload struct {
		Tracked bool              `json:"tracked"`
		Count   int               `json:"count"`
		Running []scanner.Running `json:"running"`
	}

	untracked := newFixture(t, httpapi.Options{}, nil)

	var got payload
	if err := json.Unmarshal([]byte(body(t, untracked.get("/api/v1/running"))), &got); err != nil {
		t.Fatal(err)
	}

	if got.Tracked {
		t.Error("an instance that does not scan reports activity as tracked")
	}

	idle := newFixtureLive(t, httpapi.Options{}, nil, func() []scanner.Running { return nil })

	got = payload{}
	if err := json.Unmarshal([]byte(body(t, idle.get("/api/v1/running"))), &got); err != nil {
		t.Fatal(err)
	}

	if !got.Tracked {
		t.Error("an idle scanner reports activity as untracked")
	}

	if got.Count != 0 || len(got.Running) != 0 {
		t.Errorf("idle scanner reports %d running scans", got.Count)
	}

	live := newFixtureLive(t, httpapi.Options{}, nil, oneRunning)

	got = payload{}
	if err := json.Unmarshal([]byte(body(t, live.get("/api/v1/running"))), &got); err != nil {
		t.Fatal(err)
	}

	if got.Count != 1 || len(got.Running) != 1 || got.Running[0].ScanID != "scan-live" {
		t.Errorf("running endpoint = %+v, want the one in-flight scan", got)
	}
}

// The target list is what an external monitor polls, so the in-flight scans
// belong there too rather than only in the HTML.
func TestTargetViewCarriesRunningScans(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{}, nil, oneRunning)

	var payload struct {
		Targets []httpapi.TargetView `json:"targets"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/targets"))), &payload); err != nil {
		t.Fatal(err)
	}

	found := false

	for _, series := range payload.Targets[0].Series {
		switch series.Mode {
		case model.ConsentReject:
			if len(series.Running) != 1 {
				t.Errorf("reject series reports %d running scans, want 1", len(series.Running))
			}

			found = true
		default:
			if len(series.Running) != 0 {
				t.Errorf("series %s reports a running scan that belongs to another mode", series.Mode)
			}
		}
	}

	if !found {
		t.Error("the reject series is missing from the target view")
	}
}

// blockingTrigger holds a scan open until the test releases it, which is what
// makes the concurrency behaviour observable.
type blockingTrigger struct {
	started chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func newBlockingTrigger() *blockingTrigger {
	return &blockingTrigger{
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
}

func (b *blockingTrigger) Trigger(ctx context.Context, target string, mode model.ConsentMode) (scanner.Outcome, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	b.started <- struct{}{}

	select {
	case <-b.release:
	case <-ctx.Done():
		return scanner.Outcome{}, ctx.Err()
	}

	return scanner.Outcome{Result: &model.Result{
		ScanID: "adhoc-1", Target: target, ConsentMode: mode, Termination: model.TermIdle,
	}}, nil
}

func (b *blockingTrigger) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.calls
}

// Two scans of one target and consent mode would produce two results for the
// same moment and double the load on somebody else's site (Tenet 17).
func TestConcurrentScanOfTheSameSeriesIsRefused(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()

	f := newFixtureLive(t,
		httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger, oneRunning)

	resp := f.postForm("/api/v1/scan/site/reject", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("scan of an already-running series = %d, want 409", resp.StatusCode)
	}

	if got := trigger.count(); got != 0 {
		t.Errorf("the refused request still started %d scans", got)
	}

	// A series with nothing running is unaffected.
	accepted := make(chan *http.Response, 1)

	go func() { accepted <- f.postForm("/api/v1/scan/site/accept", nil) }()

	select {
	case <-trigger.started:
	case <-time.After(5 * time.Second):
		t.Fatal("a scan of an idle series was not started")
	}

	close(trigger.release)

	// Waited for, so the request finishes before the test's context is
	// cancelled from under it.
	select {
	case resp := <-accepted:
		if resp.StatusCode != http.StatusOK {
			t.Errorf("scan of an idle series = %d, want 200", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scan of the idle series never completed")
	}
}

// The web interface hands the page back immediately: a scan takes the better
// part of a minute, and the pending row is the point (Story 5.12).
func TestUIRescanDoesNotHoldTheBrowserOpen(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()

	f := newFixtureLive(t,
		httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger,
		func() []scanner.Running { return nil })

	done := make(chan *http.Response, 1)

	go func() {
		done <- f.postForm("/rescan/site/reject", url.Values{})
	}()

	var resp *http.Response

	select {
	case resp = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the rescan request did not return while the scan was still running")
	}

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("rescan = %d, want 303", resp.StatusCode)
	}

	if dest := resp.Header.Get("Location"); !strings.HasPrefix(dest, "/targets/site/reject") {
		t.Errorf("rescan redirected to %q, want the target detail page", dest)
	}

	// The scan really was started, not merely reported as started.
	select {
	case <-trigger.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the rescan reported success without starting a scan")
	}

	close(trigger.release)
}

// Pressing "Scan now" twice must not start two scans.
func TestUIRescanRefusesWhileOneIsRunning(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()

	f := newFixtureLive(t,
		httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger, oneRunning)

	resp := f.postForm("/rescan/site/reject", url.Values{})

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rescan = %d, want 303", resp.StatusCode)
	}

	if dest := resp.Header.Get("Location"); !strings.Contains(dest, "err=") {
		t.Errorf("a duplicate rescan redirected to %q without explaining the refusal", dest)
	}

	if got := trigger.count(); got != 0 {
		t.Errorf("a duplicate rescan started %d scans", got)
	}
}

// TestReadinessFailsWhenTheStoreIsUnreachable is Story 4.7's consequence for
// readiness: with a server database the store is a network dependency, and a
// wsaw that cannot record what it observes is not ready however healthy its
// browser is (Tenet 8).
func TestReadinessFailsWhenTheStoreIsUnreachable(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	// Ready while the store is open.
	if resp := f.get("/api/v1/ready"); resp.StatusCode != http.StatusOK {
		t.Fatalf("ready = %d with a working store, want 200", resp.StatusCode)
	}

	// Closing the store is the cheapest stand-in for a database that has gone
	// away: every operation on it now fails, which is what matters here.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	resp := f.get("/api/v1/ready")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("ready = %d with an unreachable store, want 503", resp.StatusCode)
	}

	var payload struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &payload); err != nil {
		t.Fatal(err)
	}

	if payload.Ready {
		t.Error("readiness reported true with an unreachable store")
	}

	if !strings.Contains(payload.Reason, "not reachable") {
		t.Errorf("the reason does not say the store is unreachable: %q", payload.Reason)
	}
}
