package httpapi

import (
	"net/url"
	"slices"

	"github.com/pflege-de-labs/wsaw/internal/diff"
)

// The Changes section of a scan page, grouped so a reader can separate the
// two questions it answers before reading a single row (Story 5.29): did the
// site start or stop talking to somebody, and did what it already loads
// move? A host appearing is a new party to the page; an asset under a host
// already approved is the same party doing something different. Mixed into
// one list sorted by severity the two interleave, and a reader who wants one
// of them reads both.

// changeKind is the family a change belongs to. The value is what the query
// string, the row attribute and the chip label all say, so the vocabulary on
// the page, in the URL and in the DOM is one vocabulary.
type changeKind string

const (
	kindHosts    changeKind = "hosts"
	kindRequests changeKind = "requests"
	kindOther    changeKind = "other"
)

// changesParam is the query key that carries the filter, so a filtered
// section can be linked to and reloaded. The filter is applied server-side
// and the script only makes it instant.
const changesParam = "changes"

// changesAnchor is the id of the Changes section, and the fragment every
// facet link ends with.
const changesAnchor = "changes"

// requestChangeTypes are the changes about a request of the kind the page
// already lists in its own Requests table: an asset arriving or leaving, a
// script's body changing under a stable URL, a status moving. The hosts
// family reuses hostChangeTypes (api.go) rather than restating it, so this
// filter and the watchboard's "hosts only" toggle cannot come to disagree
// about what a host change is.
var requestChangeTypes = []diff.ChangeType{
	diff.AssetAdded, diff.AssetRemoved, diff.ScriptChanged, diff.StatusChanged,
}

// kindOf classifies one change. Everything that is neither a host nor a
// request — a cookie, a storage key, the consent outcome, a degraded scan —
// is "other" rather than being dropped: a change wsaw recorded must appear
// somewhere on the page it belongs to (Tenet 5), and an unclassified new
// change type lands in a visible bucket instead of vanishing.
func kindOf(t diff.ChangeType) changeKind {
	switch {
	case slices.Contains(hostChangeTypes, t):
		return kindHosts
	case slices.Contains(requestChangeTypes, t):
		return kindRequests
	default:
		return kindOther
	}
}

// changesView is the Changes section as the page renders it: one summary
// line, three facets, and every row — including the rows the active facet
// hides, which are rendered and marked rather than left out, so the script
// can switch facets without asking the daemon for a page it already has.
type changesView struct {
	Rows   []changeRow
	Facets []changeFacet

	// Kind is the facet in force, empty when the section is unfiltered.
	Kind changeKind
	// AllHref clears the filter, and is what the total in the summary links
	// to.
	AllHref string

	// Total counts every change in the report, Shown the ones the active
	// facet leaves visible. Unfiltered the two are equal.
	Total int
	Shown int

	// Worst is the highest severity present across all changes, not across
	// the visible ones: the summary states what the scan found, and a filter
	// must not be able to talk a critical finding off the line.
	Worst diff.Severity

	// Suppressed is carried from the report so the count sits on the same
	// line as the totals it qualifies.
	Suppressed int
}

// changeRow is one change plus what the page needs to group it.
type changeRow struct {
	diff.Change

	Kind changeKind
	// Hidden marks a row the active facet excludes. It renders with the
	// `hidden` attribute, which is honest with script off and instantly
	// reversible with script on.
	Hidden bool
}

// changeFacet is one chip in the summary line: a count, and the link that
// filters to it.
type changeFacet struct {
	Kind changeKind
	// Label is what the chip says, which is the kind in the grammatical
	// number its own count calls for — "1 host", not "1 hosts". The kind
	// itself stays the machine-readable value in the URL and the row
	// attribute.
	Label string
	Count int
	Href  string
	// On marks the facet in force, which the chip shows and aria-current
	// announces.
	On bool
}

// facetLabel is the kind as the chip prints it next to a count.
func facetLabel(kind changeKind, count int) string {
	if count != 1 {
		return string(kind)
	}

	switch kind {
	case kindHosts:
		return "host"
	case kindRequests:
		return "request"
	case kindOther:
		return string(kindOther)
	default:
		return string(kind)
	}
}

// Filtered reports whether a facet is in force, which is what decides
// whether the summary line offers a way back to all of them.
func (v changesView) Filtered() bool { return v.Kind != "" }

// newChangesView groups a report for rendering. An unrecognised filter value
// is ignored rather than matching nothing: a query string typed or truncated
// by hand must not be able to make a scan with findings render as a page with
// none (Tenet 5).
func newChangesView(rep *diff.Report, q url.Values) changesView {
	v := changesView{
		Kind:       parseChangeKind(q.Get(changesParam)),
		AllHref:    changesHref(q, ""),
		Suppressed: rep.Suppressed,
		Worst:      rep.MaxSeverity(),
		Total:      len(rep.Changes),
		Rows:       make([]changeRow, 0, len(rep.Changes)),
	}

	counts := map[changeKind]int{}

	for _, c := range rep.Changes {
		kind := kindOf(c.Type)
		counts[kind]++

		hidden := v.Kind != "" && kind != v.Kind
		if !hidden {
			v.Shown++
		}

		v.Rows = append(v.Rows, changeRow{Change: c, Kind: kind, Hidden: hidden})
	}

	for _, kind := range []changeKind{kindHosts, kindRequests, kindOther} {
		v.Facets = append(v.Facets, changeFacet{
			Kind:  kind,
			Label: facetLabel(kind, counts[kind]),
			Count: counts[kind],
			Href:  changesHref(q, kind),
			On:    v.Kind == kind,
		})
	}

	return v
}

// parseChangeKind reads the filter out of the query string.
func parseChangeKind(s string) changeKind {
	kind := changeKind(s)
	if kind == kindHosts || kind == kindRequests || kind == kindOther {
		return kind
	}

	return ""
}

// changesHref is the address of this page with the change filter set to kind,
// or cleared when kind is empty. Every other query parameter is carried over,
// because the Requests table further down the page has filters of its own and
// choosing a facet must not silently reset them. The fragment lands the
// reader on the section they clicked in rather than at the top of a long
// page.
func changesHref(q url.Values, kind changeKind) string {
	next := url.Values{}

	for k, values := range q {
		next[k] = slices.Clone(values)
	}

	if kind == "" {
		next.Del(changesParam)
	} else {
		next.Set(changesParam, string(kind))
	}

	if len(next) == 0 {
		return "#" + changesAnchor
	}

	return "?" + next.Encode() + "#" + changesAnchor
}
