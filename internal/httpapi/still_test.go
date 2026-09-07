package httpapi_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.18: the scan detail page holds still. Story 5.16's interval applies
// to the interface, but a scan detail page is one finished scan — an
// immutable record — so refreshing it re-renders identical content and costs
// the reader their scroll position, their filter, and the screenshot they
// were looking at.

const resultPath = "/results/site/reject/scan-1"

func seedOneScan(t *testing.T, opts httpapi.Options) *fixture {
	t.Helper()

	f := newFixture(t, opts, nil)
	f.seed("scan-1", model.ConsentReject, time.Now().Add(-time.Hour), nil)

	return f
}

// AC1: whatever interval is in force for the interface, this page does not
// reload itself. Both mechanisms have to be off — the <noscript> meta refresh
// and the seconds the enhancement script reads.
func TestTheScanDetailPageDoesNotRefreshOnTheConfiguredInterval(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second})

	html := body(t, f.get(resultPath, "Accept", "text/html"))

	if strings.Contains(html, `http-equiv="refresh"`) {
		t.Error("the scan detail page emits a meta refresh; a finished scan cannot change")
	}

	if !strings.Contains(html, `data-refresh-seconds="0"`) {
		t.Error("the enhancement script is still given an interval, so it would reload the page")
	}

	// The dashboard, with the same deployment default, still refreshes (AC6).
	if !strings.Contains(body(t, f.get("/", "Accept", "text/html")), "refreshing every 30s") {
		t.Error("narrowing the scan detail page also turned the dashboard's refresh off")
	}
}

// A viewer's own choice, remembered in the cookie, does not reach this page
// either (AC1).
func TestTheScanDetailPageDoesNotRefreshOnTheViewersInterval(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true})

	html := body(t, f.get(resultPath, "Accept", "text/html", "Cookie", "wsaw_refresh=30"))

	if strings.Contains(html, `http-equiv="refresh"`) {
		t.Error("a viewer's remembered interval refreshed the scan detail page")
	}
}

// AC4: `?refresh=30` exists so a wall display needs no setup. A wall display
// of one finished scan still has nothing to refresh — and the parameter keeps
// working on the pages where something can change.
func TestAnIntervalInTheURLDoesNotRefreshTheScanDetailPage(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true})

	html := body(t, f.get(resultPath+"?refresh=30", "Accept", "text/html"))

	if strings.Contains(html, `http-equiv="refresh"`) {
		t.Error("an interval in the URL re-enabled refreshing on the scan detail page")
	}

	if strings.Contains(html, "refreshing every") {
		t.Error("the page claims it will refresh")
	}

	// AC6: the same parameter on a target's history still applies.
	series := body(t, f.get("/targets/site/reject?refresh=30", "Accept", "text/html"))

	if !strings.Contains(series, "refreshing every 30s") {
		t.Error("the URL parameter stopped working on a target's history")
	}
}

// AC2: it says why, rather than quietly behaving differently from the page
// the reader came from.
func TestTheScanDetailPageSaysWhyItHoldsStill(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second})

	html := body(t, f.get(resultPath, "Accept", "text/html"))

	if !strings.Contains(html, "this scan is finished and cannot change") {
		t.Error("the page does not say why it holds still; a reader would think refreshing is broken")
	}
}

// AC3: the render time stays — it is the honest anchor for a tab left open an
// hour — but there is no "next refresh" claim, and no going-stale warning,
// because a finished scan does not go stale.
func TestTheScanDetailPageStillSaysWhenItWasRendered(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second})

	html := body(t, f.get(resultPath, "Accept", "text/html"))

	if !strings.Contains(html, "as of ") || !strings.Contains(html, `data-rendered-at="`) {
		t.Error("the scan detail page no longer says when it was rendered")
	}

	if strings.Contains(html, "refreshing every") || strings.Contains(html, "auto-refresh off") {
		t.Error("the page reports a refresh setting, which says nothing true about this page")
	}
}

// AC5: the control stays usable — a reader may set the interface's interval
// from anywhere — but must not suggest it affects the page they are on. It
// also has to show what the interface is currently set to.
func TestTheIntervalControlStaysUsableOnTheScanDetailPage(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true})

	html := body(t, f.get(resultPath, "Accept", "text/html", "Cookie", "wsaw_refresh=60"))

	if !strings.Contains(html, `<form method="post" action="/refresh"`) {
		t.Error("the interval control is missing; the interface's interval cannot be set from here")
	}

	if !strings.Contains(html, `<option value="60" selected>1m</option>`) {
		t.Error("the control does not show the interface's current interval")
	}

	if !strings.Contains(html, "Refresh other pages") {
		t.Error("the control reads as though it applied to this page")
	}
}

// Setting the interval from the scan detail page comes back to the same
// scan — and the page still holds still afterwards.
func TestSettingTheIntervalFromTheScanDetailPageReturnsToIt(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true})

	resp := f.postForm("/refresh", url.Values{"interval": {"30"}, "return": {resultPath}})

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setting the interval = %d, want 303", resp.StatusCode)
	}

	if got := resp.Header.Get("Location"); got != resultPath {
		t.Errorf("Location = %q, want %q", got, resultPath)
	}

	if strings.Contains(body(t, f.get(resultPath, "Accept", "text/html", "Cookie", "wsaw_refresh=30")),
		`http-equiv="refresh"`) {
		t.Error("the interval just set now refreshes the scan detail page")
	}
}

// AC7: not refreshing is about not doing it unasked. A manual reload still
// serves the same record.
func TestAManualReloadOfTheScanDetailPageShowsTheSameRecord(t *testing.T) {
	t.Parallel()

	f := seedOneScan(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second})

	first := body(t, f.get(resultPath, "Accept", "text/html"))
	second := body(t, f.get(resultPath, "Accept", "text/html"))

	for _, html := range []string{first, second} {
		if !strings.Contains(html, "scan-1") || !strings.Contains(html, "tracker.test") {
			t.Fatal("the scan detail page does not show the record it was asked for")
		}
	}
}
