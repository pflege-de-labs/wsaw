package httpapi

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/diff"
)

// The target list as a watchboard.
//
// The list this replaces was a table per target. It answered "what is
// configured" well and "is anything wrong right now" badly: the two signals
// that decide whether somebody has to act — how old the last scan is, and
// whether a third party was contacted before the consent interaction — were
// two numeric columns among ten, at 0.92em, in the middle of eight tables.
//
// The watchboard keeps every value the list already rendered and changes only
// what size and position claim about it: one tile per series, the age large,
// a critical-severity change the only thing on the row allowed to draw the
// eye on its own. Grouping is by the "env" label, because that is the axis an
// operator reads the list along — our own properties and someone else's are
// not the same kind of finding.
//
// Nothing here talks to the store. Grouping, filtering and collapse are
// display decisions applied to the TargetViews the dashboard already built,
// so the JSON API keeps serving exactly what it served before (Story 5.8,
// AC4).

// watchLabel is the target label the board groups by.
const watchLabel = "env"

// unlabelledGroup is what a target with no watchLabel is grouped under. It is
// named rather than hidden or folded into another group: a target nobody gave
// an env is a configuration fact worth seeing, not a rendering edge case
// (Tenet 5).
const unlabelledGroup = "no env label"

// Cookies. Both are display preferences of the person looking rather than
// properties of the deployment, so they live in a cookie exactly as the
// refresh interval does (Story 5.16) — one viewer collapsing a group must not
// change what anybody else sees (Tenet 15).
const (
	// envsCookie lists the collapsed groups, comma-separated.
	envsCookie = "wsaw_envs"
	// filterCookie holds the target filter text.
	filterCookie = "wsaw_filter"
)

// maxFilterLen bounds what is read back out of the filter cookie. The value
// is echoed into the page (escaped, like everything else) and matched against
// every target on every render; neither needs to be unbounded.
const maxFilterLen = 120

// envGroup is one label value's worth of targets.
type envGroup struct {
	// Label is what the summary shows: the label value, or unlabelledGroup.
	Label string
	// Value is the raw label value, and the token the collapse cookie and
	// the enhancement script use. Empty for the unlabelled group.
	Value   string
	Targets []watchTarget

	// Collapsed is the viewer's remembered choice for this group. It is
	// rendered as the absence of <details open>, so the group collapses with
	// JavaScript off and the browser's own disclosure behaviour still works.
	Collapsed bool
}

// Count is what the summary says about the group's size, so the reader can
// judge a collapsed group without opening it.
func (g envGroup) Count() string {
	series := 0
	for _, t := range g.Targets {
		series += len(t.Rows)
	}

	return plural(len(g.Targets), "target") + " · " + plural(series, "series")
}

// watchTarget is one target as the board draws it.
type watchTarget struct {
	targetRow
}

// Host is the target's URL without its scheme. The scheme is the same on
// every row and costs the width the path needs.
func (t watchTarget) Host() string {
	u := t.URL
	for _, prefix := range []string{"https://", "http://"} {
		u = strings.TrimPrefix(u, prefix)
	}

	return u
}

// PreConsent is how many third-party hosts this row's last scan contacted
// before the consent interaction, or zero where there is no scan to report.
// It still marks the metrics line's "N pre" figure; TileClass no longer keys
// off it directly.
func (r modeRow) PreConsent() int {
	if r.Series.LastScan == nil {
		return 0
	}

	return r.Series.LastScan.PreConsentDomains
}

// HasCriticalChange reports whether the last scan's diff against its
// baseline contains at least one critical-severity change. Severity is
// already condensed to its worst outcome (SeriesView.Severity), and critical
// is the top of that ranking, so "worst severity is critical" and "at least
// one critical change" are the same fact — no separate count is needed.
func (r modeRow) HasCriticalChange() bool {
	return r.Series.Severity == diff.SeverityCritical
}

// DeviatesFromBaseline reports whether the displayed scan differs from the
// series' approved baseline.
//
// Rank() > 0 excludes both an empty Severity (the pair was not comparable at
// all) and diff.SeverityInfo (comparable, but MaxSeverity's own zero value
// for "no changes" — Report.MaxSeverity, diff.go) — neither is a deviation.
//
// And it is deliberately narrower than "there is any change at all": when no
// baseline is set, Severity still gets computed by falling back to comparing
// against the scan before this one (lastScanSeverity, api.go), which is a
// different claim than "deviates from the baseline" — one this method only
// makes when a baseline actually exists.
func (r modeRow) DeviatesFromBaseline() bool {
	return r.Series.HasBaseline && r.Series.Severity.Rank() > 0
}

// TileClass marks the states a tile renders differently: a critical change,
// a scan in flight, a stale series. The words are on the tile too — colour
// only reinforces them (Story 5.11, AC5's reasoning applied to the
// interface's own colour use).
func (r modeRow) TileClass() string {
	class := "watch-tile"

	switch {
	case len(r.Series.Running) > 0:
		class += " is-running"
	case r.HasCriticalChange():
		class += " has-critical"
	case r.Series.Stale:
		class += " is-stale"
	}

	return class
}

// shapeWatchboard applies the viewer's filter and grouping to a dashboard
// that has already collected its targets.
//
// Order matters: Total counts what is configured, Shown counts what survived
// the filter, and the page states both. A filtered list that says only
// "6 targets" is a watcher quietly claiming two targets do not exist.
func (d *dashboardData) shapeWatchboard(r *http.Request) {
	d.Total = len(d.Targets)
	d.Filter = watchFilter(r)
	d.Targets = filterTargets(d.Targets, d.Filter)
	d.Shown = len(d.Targets)
	d.PreConsentHosts = countPreConsent(d.Targets)
	d.Groups = groupByLabel(d.Targets, watchLabel, collapsedEnvs(r))
}

// watchFilter reads the remembered filter text.
//
// It is applied on the server as well as in the script so that the preference
// means the same thing with JavaScript off: a viewer who filtered the board
// down and came back to it gets the board they left, not a full list that
// silently ignores the field it is showing them.
func watchFilter(r *http.Request) string {
	c, err := r.Cookie(filterCookie)
	if err != nil {
		return ""
	}

	// watch.js writes the cookie with encodeURIComponent, so decode it back;
	// PathUnescape (not QueryUnescape) because encodeURIComponent never
	// produces a literal "+" for a space, so it must not be read as one.
	value := c.Value
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}

	value = strings.TrimSpace(value)
	if len(value) > maxFilterLen {
		value = value[:maxFilterLen]
	}

	return value
}

// collapsedEnvs reads the remembered collapsed groups.
func collapsedEnvs(r *http.Request) map[string]bool {
	out := map[string]bool{}

	c, err := r.Cookie(envsCookie)
	if err != nil {
		return out
	}

	for _, token := range strings.Split(c.Value, ",") {
		if token = strings.TrimSpace(token); token != "" {
			out[token] = true
		}
	}

	return out
}

// filterTargets keeps the targets a filter string matches.
//
// It matches the name, the URL and the labels — the three things written on
// the row, so a reader can always see why something matched. Matching is
// case-insensitive and by substring: "kneipp", "curabox.de/pflege" and
// "env=prod" all work, and none of them is a syntax anybody has to learn.
func filterTargets(rows []targetRow, filter string) []targetRow {
	if filter == "" {
		return rows
	}

	needle := strings.ToLower(filter)
	out := make([]targetRow, 0, len(rows))

	for _, row := range rows {
		if strings.Contains(strings.ToLower(targetHaystack(row)), needle) {
			out = append(out, row)
		}
	}

	return out
}

// targetHaystack is everything about a target the filter may match.
func targetHaystack(row targetRow) string {
	var b strings.Builder

	b.WriteString(row.Name)
	b.WriteString(" ")
	b.WriteString(row.URL)

	for _, k := range sortedKeys(row.Labels) {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(row.Labels[k])
	}

	return b.String()
}

// countPreConsent totals the third-party hosts contacted before the consent
// interaction across every series shown. It is the board's headline number,
// so it counts what is on screen rather than what is configured.
func countPreConsent(rows []targetRow) int {
	n := 0

	for _, row := range rows {
		for _, mr := range row.Rows {
			n += mr.PreConsent()
		}
	}

	return n
}

// groupByLabel groups targets by one label's value.
//
// Groups are ordered by label value, with the unlabelled group last: it is
// the least likely to be what somebody opened the page for, and putting it
// first would push the deployment's real environments below the fold.
func groupByLabel(rows []targetRow, key string, collapsed map[string]bool) []envGroup {
	byValue := map[string][]watchTarget{}
	values := make([]string, 0, len(rows))

	for _, row := range rows {
		value := row.Labels[key]

		if _, seen := byValue[value]; !seen {
			values = append(values, value)
		}

		byValue[value] = append(byValue[value], watchTarget{targetRow: row})
	}

	sort.SliceStable(values, func(i, j int) bool {
		switch {
		case values[i] == "":
			return false
		case values[j] == "":
			return true
		default:
			return values[i] < values[j]
		}
	})

	out := make([]envGroup, 0, len(values))

	for _, value := range values {
		label := value
		if label == "" {
			label = unlabelledGroup
		}

		out = append(out, envGroup{
			Label:     label,
			Value:     value,
			Targets:   byValue[value],
			Collapsed: collapsed[value],
		})
	}

	return out
}

// handleUIFilter stores the viewer's filter text and sends them back to the
// board.
//
// A POST with CSRF and a redirect afterwards, exactly like the refresh
// interval it sits next to (Story 5.16, AC9): it is a deliberate change to a
// remembered preference, and the reload that follows must be a GET rather
// than a repeat of this submission.
func (s *Server) handleUIFilter(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}

	filter := strings.TrimSpace(r.FormValue("filter"))
	if len(filter) > maxFilterLen {
		filter = filter[:maxFilterLen]
	}

	http.SetCookie(w, &http.Cookie{ //nolint:gosec // a display preference, no secret; Secure follows TLS as elsewhere
		Name:     filterCookie,
		Value:    filter,
		Path:     "/",
		HttpOnly: false, // watch.js reads it to keep the field and the board in step
		Secure:   s.opts.TLSCert != "",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((365 * 24 * 60 * 60)),
	})

	// #nosec G710 -- safeLocal reduces the destination to a path on this
	// origin, the same treatment handleUIRefresh gets.
	http.Redirect(w, r, safeLocal(r.FormValue("return")), http.StatusSeeOther)
}

// sortedKeys orders a label map, so a row's rendering is stable across
// renders rather than following Go's map iteration.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// plural renders a count with its noun. "series" is already plural, so it is
// left alone rather than pluralised into something that is not a word.
func plural(n int, noun string) string {
	switch {
	case noun == "series":
		return itoa(n) + " series"
	case n == 1:
		return "1 " + noun
	default:
		return itoa(n) + " " + noun + "s"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte

	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}
