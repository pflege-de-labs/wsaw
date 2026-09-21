package httpapi_test

// The scan page's Changes section as it renders: its summary line, its
// family chips, and what the server does with the filter they carry
// (Story 5.29). The grouping itself is tested in changes_internal_test.go.

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// seedChanged stores a baseline scan and a second scan that differs from it
// in all three families at once: a new third-party host, a new asset under a
// host that was already there, and a new cookie. It returns the path of the
// second scan's page.
func seedChanged(t *testing.T, f *fixture) string {
	t.Helper()

	base := time.Now().Add(-time.Hour)

	f.seed("scan-base", model.ConsentReject, base, nil)

	f.seed("scan-new", model.ConsentReject, base.Add(time.Minute), func(res *model.Result) {
		res.Requests = append(res.Requests,
			// A host the baseline never contacted.
			model.Request{
				URL: "https://newtracker.test/beacon", NormalizedURL: "https://newtracker.test/beacon",
				Method: "GET", ResourceType: "script", Host: "newtracker.test", Domain: "newtracker.test",
				Party: model.ThirdParty, Phase: model.PhasePre, Status: 200,
			},
			// A new asset under a host the baseline already knew.
			model.Request{
				URL: "https://tracker.test/extra.js", NormalizedURL: "https://tracker.test/extra.js",
				Method: "GET", ResourceType: "script", Host: "tracker.test", Domain: "tracker.test",
				Party: model.ThirdParty, Phase: model.PhasePost, Status: 200,
			},
		)

		res.Cookies = []model.Cookie{{
			Name: "_ga", Domain: "example.com", Path: "/", Party: model.FirstParty,
		}}
	})

	return "/results/site/reject/scan-new"
}

// AC1: the section leads with one line that says how many changes there are
// and how they split, before any row is read.
func TestTheChangesSectionLeadsWithASummaryLine(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	html := body(t, f.get(path, "Accept", "text/html"))

	if !strings.Contains(html, `class="change-summary"`) {
		t.Fatal("the Changes section has no summary line")
	}

	for _, want := range []string{"host</a>", "requests</a>", "other</a>", "worst "} {
		if !strings.Contains(html, want) {
			t.Errorf("the summary line does not state %q", want)
		}
	}

	summary := section(t, html, `class="change-summary"`, "</p>")

	for _, want := range []string{"1 host", "2 requests", "1 other", "4 changes"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary line reads %q, which does not state %q", summary, want)
		}
	}
}

// AC1: every row carries the family it belongs to, which is what the filter
// acts on — server-side now, client-side once the script runs.
func TestChangeRowsCarryTheirFamily(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	html := body(t, f.get(path, "Accept", "text/html"))

	for _, kind := range []string{"hosts", "requests", "other"} {
		if !strings.Contains(html, `<tr data-kind="`+kind+`"`) {
			t.Errorf("no change row is marked as %q", kind)
		}
	}
}

// AC3: the filter is applied by the server, so it works with script off —
// and it hides rows rather than dropping them, so the script can put them
// back without a round trip.
func TestFilteringToAFamilyHidesTheOtherRowsServerSide(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	html := body(t, f.get(path+"?changes=hosts", "Accept", "text/html"))

	if !strings.Contains(html, `<tr data-kind="hosts">`) {
		t.Error("the host rows are not visible under the hosts filter")
	}

	for _, kind := range []string{"requests", "other"} {
		if !strings.Contains(html, `<tr data-kind="`+kind+`" hidden>`) {
			t.Errorf("%s rows are not hidden under the hosts filter", kind)
		}
	}

	if strings.Contains(html, `<tr data-kind="requests">`) {
		t.Error("a request row rendered visible under the hosts filter")
	}
}

// AC1: the worst severity on the line is the scan's, not the visible rows' —
// a filter must not be able to make a finding disappear (Tenet 5).
func TestTheSummaryKeepsTheWorstSeverityUnderAFilter(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	unfiltered := section(t, body(t, f.get(path, "Accept", "text/html")), `class="change-summary"`, "</p>")
	filtered := section(t, body(t, f.get(path+"?changes=other", "Accept", "text/html")),
		`class="change-summary"`, "</p>")

	worst := func(s string) string {
		i := strings.Index(s, "worst ")
		if i < 0 {
			t.Fatalf("the summary line %q states no worst severity", s)
		}

		return s[i : strings.Index(s[i:], "<")+i]
	}

	if worst(unfiltered) != worst(filtered) {
		t.Errorf("filtering changed the worst severity from %q to %q", worst(unfiltered), worst(filtered))
	}

	if !strings.Contains(filtered, "4 changes") {
		t.Errorf("the filtered summary %q no longer states the scan's own total", filtered)
	}
}

// AC4: a value nobody named is ignored rather than matching nothing, so a
// hand-edited query string cannot render a scan with findings as a scan with
// none (Tenet 5).
func TestAnUnknownChangeFilterRendersEveryRow(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	html := body(t, f.get(path+"?changes=nonsense", "Accept", "text/html"))

	if strings.Contains(html, "hidden>") {
		t.Error("an unrecognised filter value hid rows")
	}

	for _, kind := range []string{"hosts", "requests", "other"} {
		if !strings.Contains(html, `<tr data-kind="`+kind+`">`) {
			t.Errorf("%s rows are missing under an unrecognised filter value", kind)
		}
	}
}

// AC4: the chips are links, and they carry the Requests table's own filters
// with them — choosing a family must not silently reset the rest of the page.
func TestChangeChipsAreLinksThatKeepTheOtherFilters(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	html := body(t, f.get(path+"?host=tracker.test", "Accept", "text/html"))
	summary := section(t, html, `class="change-summary"`, "</p>")

	if !strings.Contains(summary, "changes=hosts") {
		t.Errorf("the summary line %q offers no link that filters to hosts", summary)
	}

	if !strings.Contains(summary, "host=tracker.test") {
		t.Errorf("the summary line %q drops the request filter already in force", summary)
	}

	if !strings.Contains(summary, "#changes") {
		t.Errorf("the summary line %q does not land the reader on the section", summary)
	}
}

// AC5: the section's markup introduces no inline style or script attribute,
// which the page's CSP would drop silently rather than loudly (Story 5.23
// AC11), and the enhancement script is served like the page's others.
func TestTheChangesSectionStaysWithinTheContentSecurityPolicy(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	path := seedChanged(t, f)

	html := body(t, f.get(path, "Accept", "text/html"))

	summary := section(t, html, `class="change-summary"`, "</p>")
	if strings.Contains(summary, "style=") || strings.Contains(summary, "onclick=") {
		t.Errorf("the summary line %q carries an inline style or handler", summary)
	}

	if !strings.Contains(html, `<script src="/static/changes.js" defer></script>`) {
		t.Error("changes.js is not loaded by the page")
	}

	resp := f.get("/static/changes.js")
	if resp.StatusCode != 200 {
		t.Errorf("GET /static/changes.js = %d, want 200", resp.StatusCode)
	}
}

// AC2: a scan with nothing to compare against keeps the plain wording it
// already had; a summary line counting zero changes would be noise.
func TestAScanWithNoChangesHasNoSummaryLine(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-only", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/results/site/reject/scan-only", "Accept", "text/html"))

	if strings.Contains(html, `class="change-summary"`) {
		t.Error("a scan with no comparison rendered a change summary line")
	}
}

// AC1: a family with nothing in it is stated as a zero rather than offered
// as a link that would hide every row on the page.
func TestAnEmptyFamilyRendersAsAZeroNotALink(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	base := time.Now().Add(-time.Hour)
	f.seed("scan-base", model.ConsentReject, base, nil)
	f.seed("scan-cookie", model.ConsentReject, base.Add(time.Minute), func(res *model.Result) {
		res.Cookies = []model.Cookie{{Name: "_ga", Domain: "example.com", Path: "/", Party: model.FirstParty}}
	})

	html := body(t, f.get("/results/site/reject/scan-cookie", "Accept", "text/html"))
	summary := section(t, html, `class="change-summary"`, "</p>")

	for _, kind := range []string{"hosts", "requests"} {
		if !strings.Contains(summary, `<span class="chip is-zero" data-kind="`+kind+`">0 `+kind+`</span>`) {
			t.Errorf("the summary line %q does not state an empty %s family as a zero", summary, kind)
		}
	}

	if strings.Contains(summary, "changes=hosts") {
		t.Errorf("the summary line %q offers a link to a family with nothing in it", summary)
	}
}

// section returns the page text from marker to the first end after it, so an
// assertion about the summary line cannot be satisfied by something
// elsewhere on a long page.
func section(t *testing.T, html, marker, end string) string {
	t.Helper()

	start := strings.Index(html, marker)
	if start < 0 {
		t.Fatalf("the page has no %q", marker)
	}

	rest := html[start:]

	stop := strings.Index(rest, end)
	if stop < 0 {
		t.Fatalf("the %q block is never closed", marker)
	}

	return rest[:stop]
}
