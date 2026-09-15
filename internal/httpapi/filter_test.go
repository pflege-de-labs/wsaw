package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.22: instant, client-side filtering of the target list. The actual
// filtering runs in the browser (filter.js), which this package's tests do
// not execute — matching how refresh.js is covered — so these assert what
// the server must get right for that script to work: the row-level data it
// filters on, the option values it offers, and that a no-JavaScript reader
// gets the same target list as always (AC8).

// AC1: filter controls exist for severity, outcome, mode and labels, and
// offer the actual values the rest of the interface uses — never a set that
// could drift from what a row can actually carry.
func TestDashboardOffersEveryFilterDimension(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{
		`id="target-filters"`,
		`<input type="checkbox" name="severity" value="critical">`,
		`<input type="checkbox" name="severity" value="info">`,
		`<input type="checkbox" name="outcome" value="applied">`,
		`<input type="checkbox" name="outcome" value="necessary-only">`,
		`<input type="checkbox" name="mode" value="none">`,
		`<input type="checkbox" name="mode" value="reject">`,
		`<input type="checkbox" name="mode" value="accept">`,
		`id="filter-label"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("filter panel is missing %s", want)
		}
	}
}

// AC6: a single reset control exists and starts disabled, since a page that
// just loaded has no active filter to clear — it is never a dead click, but
// it also never starts live for nothing to do.
func TestDashboardResetButtonStartsDisabled(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `id="filter-reset"`) {
		t.Fatal("no reset-all control on the dashboard")
	}

	if !strings.Contains(html, `id="filter-reset" class="filter-reset" disabled`) {
		t.Error("reset-all did not start disabled with nothing filtered yet")
	}
}

// AC5: the page states how many targets it holds before any filtering
// happens, so filter.js has an honest baseline to update instead of
// inventing the count itself.
func TestDashboardStatesTheUnfilteredCount(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "showing 1 of 1 targets") {
		t.Error("the page does not state its own unfiltered target count")
	}
}

// AC1/AC4: each row on the target list carries the data a filter needs to
// judge it in isolation, without walking back up to its target section.
func TestTargetRowsCarryFilterData(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true}, nil, nil, func(d *httpapi.Deps) {
		d.Targets = func() []config.Resolved {
			return []config.Resolved{{
				Name:         "site",
				URL:          "https://example.com/",
				Labels:       map[string]string{"team": "infra", "env": "prod"},
				ConsentModes: []model.ConsentMode{model.ConsentReject, model.ConsentAccept},
			}}
		}
	})

	baseline := f.seed("scan-baseline", model.ConsentReject, time.Now().Add(-time.Hour), func(r *model.Result) {
		r.Requests = r.Requests[:1]
	})

	if _, err := f.store.SetBaseline("site", model.ConsentReject, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	// A tracker survives rejection versus baseline: rated critical, and the
	// row's outcome is "applied" (the fixture's seed default).
	f.seed("scan-latest", model.ConsentReject, time.Now(), nil)
	f.seed("scan-accept", model.ConsentAccept, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{
		`data-severity="critical"`,
		`data-outcome="applied"`,
		`data-modes="reject"`,
		`data-modes="accept"`,
		`data-labels="env=prod team=infra "`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("a target-list row is missing %s\n%s", want, html)
		}
	}
}

// AC1: a row whose series has never been scanned carries no severity or
// outcome, so it correctly matches no severity/outcome filter rather than
// an invented one.
func TestNeverScannedRowCarriesEmptyFilterData(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{`data-severity=""`, `data-outcome=""`, `data-modes="none reject"`} {
		if !strings.Contains(html, want) {
			t.Errorf("a never-scanned row's filter data was not empty as expected: missing %s\n%s", want, html)
		}
	}
}

// AC8: with no JavaScript, the panel must not be left sitting on the page,
// inert and confusing — it starts absent, and filter.js is the only thing
// that ever reveals it. The full, unfiltered target list still renders
// exactly as it always has (Story 5.7, AC6).
func TestFilterPanelStartsHiddenForNoJavaScript(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `id="target-filters" hidden`) {
		t.Error("the filter panel is not hidden by default")
	}

	if !strings.Contains(html, `href="/results/site/reject/scan-1"`) {
		t.Error("the underlying target list did not render without JavaScript")
	}
}

// AC2: filter.js is the enhancement, and it is only ever a static file the
// browser fetches — nothing about applying a filter is a server round-trip.
func TestFilterScriptIsServedAndLinked(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `<script src="/static/filter.js" defer></script>`) {
		t.Error("the dashboard does not load filter.js")
	}

	resp := f.get("/static/filter.js")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("GET /static/filter.js = %d, want 200", resp.StatusCode)
	}

	js := body(t, resp)
	if !strings.Contains(js, "rowMatches") {
		t.Error("/static/filter.js does not look like the filter script")
	}
}

// AC8: a target list with no targets configured must not offer a filter
// panel with nothing to filter.
func TestNoFilterPanelWithoutTargets(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true}, nil, nil, func(d *httpapi.Deps) {
		d.Targets = func() []config.Resolved { return nil }
	})

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, `id="target-filters"`) {
		t.Error("a filter panel was rendered with no targets to filter")
	}
}

// Each severity/outcome/mode checkbox carries a placeholder filter.js fills
// in with how many targets that option currently matches, shown in
// parentheses after the option. The server renders the empty placeholder
// (never a number, since it has no idea which filters, if any, are
// currently active in the reader's browser); the count itself is filter.js's
// job, exercised in the jsdom-based verification described in Story 5.22's
// commit rather than here.
func TestFilterOptionsCarryACountPlaceholder(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{
		`<span class="filter-n" data-dim="severity" data-value="critical"></span>`,
		`<span class="filter-n" data-dim="outcome" data-value="applied"></span>`,
		`<span class="filter-n" data-dim="mode" data-value="reject"></span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("a filter option is missing its count placeholder: %s", want)
		}
	}
}
