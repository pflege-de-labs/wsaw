package httpapi

// The grouping behind the scan page's Changes filter (changes.go), tested
// directly since the view and its kinds are unexported. The rendered page's
// own assertions are in changes_test.go.

import (
	"net/url"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
)

// Every change type wsaw can produce lands in exactly one family, and the
// case list is exhaustive on purpose: a new change type added to the differ
// without a line here fails this test rather than quietly becoming "other"
// in a UI nobody re-read (AC2).
func TestEveryChangeTypeHasAFamily(t *testing.T) {
	t.Parallel()

	cases := map[diff.ChangeType]changeKind{
		diff.HostAdded:      kindHosts,
		diff.HostRemoved:    kindHosts,
		diff.DeniedHost:     kindHosts,
		diff.AssetAdded:     kindRequests,
		diff.AssetRemoved:   kindRequests,
		diff.ScriptChanged:  kindRequests,
		diff.StatusChanged:  kindRequests,
		diff.CookieAdded:    kindOther,
		diff.CookieRemoved:  kindOther,
		diff.StorageAdded:   kindOther,
		diff.StorageRemoved: kindOther,
		diff.ConsentChanged: kindOther,
		diff.ScanDegraded:   kindOther,
	}

	all := []diff.ChangeType{
		diff.HostAdded, diff.HostRemoved, diff.AssetAdded, diff.AssetRemoved,
		diff.ScriptChanged, diff.CookieAdded, diff.CookieRemoved, diff.StorageAdded,
		diff.StorageRemoved, diff.StatusChanged, diff.ConsentChanged, diff.DeniedHost,
		diff.ScanDegraded,
	}

	if len(all) != len(cases) {
		t.Fatalf("the differ has %d change types and this test classifies %d", len(all), len(cases))
	}

	for _, typ := range all {
		if got := kindOf(typ); got != cases[typ] {
			t.Errorf("kindOf(%s) = %q, want %q", typ, got, cases[typ])
		}
	}
}

// An unknown type — one added to the differ and not to the families above —
// is visible as "other" rather than dropped from the page (Tenet 5, AC2).
func TestAnUnknownChangeTypeStaysVisible(t *testing.T) {
	t.Parallel()

	if got := kindOf(diff.ChangeType("something-new")); got != kindOther {
		t.Errorf("kindOf(unknown) = %q, want %q", got, kindOther)
	}
}

func changeReport(changes ...diff.Change) *diff.Report {
	return &diff.Report{Comparable: true, Changes: changes}
}

func change(typ diff.ChangeType, subject string, sev diff.Severity) diff.Change {
	return diff.Change{Type: typ, Subject: subject, Severity: sev, Detail: subject}
}

// The summary counts every change by family, in one pass over the report
// (AC1).
func TestTheSummaryCountsEachFamily(t *testing.T) {
	t.Parallel()

	v := newChangesView(changeReport(
		change(diff.HostAdded, "tracker.test", diff.SeverityHigh),
		change(diff.DeniedHost, "denied.test", diff.SeverityCritical),
		change(diff.AssetAdded, "https://tracker.test/a.js", diff.SeverityMedium),
		change(diff.ScriptChanged, "https://example.com/app.js", diff.SeverityLow),
		change(diff.StatusChanged, "https://example.com/x", diff.SeverityInfo),
		change(diff.CookieAdded, "example.com/_ga", diff.SeverityLow),
	), url.Values{})

	if v.Total != 6 {
		t.Errorf("Total = %d, want 6", v.Total)
	}

	want := map[changeKind]int{kindHosts: 2, kindRequests: 3, kindOther: 1}

	for _, f := range v.Facets {
		if f.Count != want[f.Kind] {
			t.Errorf("%s = %d, want %d", f.Kind, f.Count, want[f.Kind])
		}
	}

	if v.Worst != diff.SeverityCritical {
		t.Errorf("Worst = %q, want critical", v.Worst)
	}
}

// The families are always offered in the same order, so the line does not
// reshuffle itself between two scans of the same site (AC1).
func TestTheFacetsKeepTheirOrder(t *testing.T) {
	t.Parallel()

	v := newChangesView(changeReport(change(diff.CookieAdded, "example.com/_ga", diff.SeverityLow)), url.Values{})

	want := []changeKind{kindHosts, kindRequests, kindOther}

	if len(v.Facets) != len(want) {
		t.Fatalf("got %d facets, want %d", len(v.Facets), len(want))
	}

	for i, f := range v.Facets {
		if f.Kind != want[i] {
			t.Errorf("facet %d is %q, want %q", i, f.Kind, want[i])
		}
	}
}

// A filter hides rows, it does not discard them: every change is still in
// the view, marked, which is what lets the script switch families without a
// round trip and what keeps the page honest with script off (AC3, AC5).
func TestAFilterMarksRowsRatherThanDroppingThem(t *testing.T) {
	t.Parallel()

	v := newChangesView(changeReport(
		change(diff.HostAdded, "tracker.test", diff.SeverityHigh),
		change(diff.AssetAdded, "https://tracker.test/a.js", diff.SeverityMedium),
		change(diff.CookieAdded, "example.com/_ga", diff.SeverityLow),
	), url.Values{changesParam: {"hosts"}})

	if len(v.Rows) != 3 {
		t.Fatalf("got %d rows, want all 3 rendered", len(v.Rows))
	}

	if v.Shown != 1 {
		t.Errorf("Shown = %d, want 1", v.Shown)
	}

	for _, row := range v.Rows {
		wantHidden := row.Kind != kindHosts
		if row.Hidden != wantHidden {
			t.Errorf("row %s (%s): Hidden = %v, want %v", row.Subject, row.Kind, row.Hidden, wantHidden)
		}
	}

	if !v.Filtered() || v.Kind != kindHosts {
		t.Errorf("Kind = %q, Filtered = %v, want hosts and true", v.Kind, v.Filtered())
	}
}

// The worst severity is the scan's, not the visible rows': filtering to a
// quiet family must not be able to make a critical finding disappear from
// the summary (Tenet 5, AC1).
func TestFilteringDoesNotChangeTheWorstSeverity(t *testing.T) {
	t.Parallel()

	rep := changeReport(
		change(diff.HostAdded, "tracker.test", diff.SeverityCritical),
		change(diff.CookieAdded, "example.com/_ga", diff.SeverityLow),
	)

	v := newChangesView(rep, url.Values{changesParam: {"other"}})

	if v.Worst != diff.SeverityCritical {
		t.Errorf("Worst = %q while filtered to other, want critical", v.Worst)
	}

	if v.Total != 2 {
		t.Errorf("Total = %d while filtered, want the report's own 2", v.Total)
	}
}

// A value nobody named is ignored, and the section renders in full. The
// alternative — matching nothing — would render a scan with findings as a
// scan with none (Tenet 5, AC4).
func TestAnUnrecognisedFilterValueRendersEverything(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "host", "HOSTS", "requests; drop", "1"} {
		v := newChangesView(changeReport(
			change(diff.HostAdded, "tracker.test", diff.SeverityHigh),
			change(diff.CookieAdded, "example.com/_ga", diff.SeverityLow),
		), url.Values{changesParam: {raw}})

		if v.Filtered() {
			t.Errorf("%q was treated as a filter (%q)", raw, v.Kind)
		}

		if v.Shown != 2 {
			t.Errorf("%q left %d of 2 rows visible", raw, v.Shown)
		}
	}
}

// The Requests table further down the page has filters of its own, and
// picking a change family must not throw them away (AC4).
func TestFacetLinksCarryThePagesOtherFilters(t *testing.T) {
	t.Parallel()

	q := url.Values{"host": {"tracker.test"}, "party": {"third"}, changesParam: {"hosts"}}

	v := newChangesView(changeReport(change(diff.AssetAdded, "https://tracker.test/a.js", diff.SeverityMedium)), q)

	for _, f := range v.Facets {
		u, err := url.Parse(f.Href)
		if err != nil {
			t.Fatalf("facet %s href %q: %v", f.Kind, f.Href, err)
		}

		got := u.Query()
		if got.Get("host") != "tracker.test" || got.Get("party") != "third" {
			t.Errorf("facet %s href %q dropped the request filters", f.Kind, f.Href)
		}

		if got.Get(changesParam) != string(f.Kind) {
			t.Errorf("facet %s href %q does not select its own family", f.Kind, f.Href)
		}

		if u.Fragment != changesAnchor {
			t.Errorf("facet %s href %q does not land on the section", f.Kind, f.Href)
		}
	}

	all, err := url.Parse(v.AllHref)
	if err != nil {
		t.Fatalf("AllHref %q: %v", v.AllHref, err)
	}

	if all.Query().Has(changesParam) {
		t.Errorf("AllHref %q still carries a family filter", v.AllHref)
	}

	if all.Query().Get("host") != "tracker.test" {
		t.Errorf("AllHref %q dropped the request filters", v.AllHref)
	}
}

// With nothing else in the query string the links stay a bare fragment,
// rather than a "?" and an empty query that would reload the page for
// nothing.
func TestClearingTheFilterWithNoOtherQueryIsJustTheAnchor(t *testing.T) {
	t.Parallel()

	v := newChangesView(changeReport(change(diff.HostAdded, "tracker.test", diff.SeverityHigh)),
		url.Values{changesParam: {"hosts"}})

	if v.AllHref != "#"+changesAnchor {
		t.Errorf("AllHref = %q, want %q", v.AllHref, "#"+changesAnchor)
	}
}
