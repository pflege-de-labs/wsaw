package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.22 shipped per-dimension checkbox filtering (severity, outcome,
// mode); the watchboard later replaced it with one substring filter over
// name, URL and labels (watchboard.go, filterTargets — see
// watchboard_test.go for that filter's own tests). What survives here is
// what still applies to the current design: the row-level data any
// client-side filtering would need, the Clear button as the one remaining
// stand-in for the old reset control, and filter.js still being served even
// though nothing on the current page has the ids it looks for.

// The Clear button is never a dead click for the same reason the old reset
// control started disabled: it only exists when there is something to clear.
func TestTheClearButtonOnlyAppearsWhenAFilterIsActive(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	unfiltered := body(t, f.get("/", "Accept", "text/html"))
	if strings.Contains(unfiltered, `class="watch-filter-clear"`) {
		t.Error("the Clear button is present with nothing filtered yet")
	}

	filtered := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_filter=site"))
	if !strings.Contains(filtered, `class="watch-filter-clear"`) {
		t.Error("the Clear button did not appear once a filter was in force")
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

	if !strings.Contains(html, "showing 1/1") {
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
