package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/confidence"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// WriteMarkdown renders a review-ready report.
//
// The ordering is deliberate: whether the scan is trustworthy comes first,
// then the consent outcome, then pre-consent traffic, then the diff. A
// reviewer must not read a host table before learning that the scan was
// truncated or that the banner was never dismissed.
func WriteMarkdown(w io.Writer, res *model.Result, rep *diff.Report, basis Basis) error {
	if _, err := io.WriteString(w, build(res, rep, basis).markdown()); err != nil {
		return fmt.Errorf("writing Markdown report: %w", err)
	}

	return nil
}

// build lays out the report every human-readable format renders.
func build(res *model.Result, rep *diff.Report, basis Basis) *document {
	d := &document{}

	d.heading(1, fmt.Sprintf("%s — %s", res.Target, res.ConsentMode))

	header := [][]span{{bold("URL:"), text(" " + res.URL)}}

	if res.FinalURL != "" && res.FinalURL != res.URL {
		header = append(header, []span{bold("Final URL:"), text(" " + res.FinalURL)})
	}

	header = append(header,
		[]span{bold("Scanned:"), text(" " + res.StartedAt.UTC().Format(time.RFC3339))},
		[]span{bold("Duration:"), text(" " + res.Duration.Round(time.Millisecond).String())},
		[]span{bold("Scan ID:"), text(" "), span{Text: res.ScanID, Code: true, Exact: true}},
	)

	d.lines(header...)

	buildTrust(d, res)
	buildConsent(d, res)
	buildPreConsent(d, res)
	buildCounts(d, res)
	// The data tables need the width, and they are consecutive, so the PDF
	// turns landscape once and stays there (Story 5.39, AC15).
	d.landscape()
	buildHosts(d, res)
	buildCookies(d, res)
	buildStorage(d, res)
	buildDiff(d, rep, basis)
	buildEnvironment(d, res)

	return d
}

// colParty heads the party column of every table that has one.
const colParty = "Party"

// buildTrust states up front whether the numbers below can be believed.
func buildTrust(d *document, res *model.Result) {
	switch {
	case !res.OK():
		lines := [][]span{{bold("This scan did not complete."), text(" Termination: "), code(string(res.Termination)), text(".")}}

		if res.Error != "" {
			lines = append(lines, []span{text("Error: " + res.Error)})
		}

		lines = append(lines, []span{text(`The asset list below is incomplete and must not be read as "nothing was loaded".`)})

		d.quote(lines...)

	case res.Truncated():
		d.quote(
			[]span{bold("This scan stopped early"), text(" ("), code(string(res.Termination)), text("). The asset list may be incomplete,")},
			[]span{text("and any reduction against the baseline may be an artefact rather than a real change.")},
		)
	}

	// A failed scan has said all there is to say above. A completed one can
	// still be one a reader should not take at face value, and the report
	// says so before the findings, not in the confidence table below them
	// (Story 5.39, AC2).
	if res.OK() {
		if caveat := confidence.ConsentCaveat(res); caveat != "" {
			d.quote([]span{bold("Consent state not confirmed:"), text(" " + caveat + ".")})
		}

		if c := res.Confidence; c != nil && c.Band == model.ConfidenceLow {
			d.quote([]span{
				bold("Low confidence"), textf(" (%d of 100). ", c.Score),
				text("The findings below rest on a partial observation; the Confidence section says why."),
			})
		}
	}

	if len(res.Warnings) > 0 {
		d.para(bold("Warnings"))

		items := make([]item, 0, len(res.Warnings))
		for _, warn := range res.Warnings {
			items = append(items, item{Spans: []span{text(warn)}})
		}

		d.list(items...)
	}

	buildConfidence(d, res.Confidence)
}

// buildConfidence lists every signal behind the score, the ones that cost
// nothing and the ones that could not be assessed included, as the scan page
// does (Story 5.35).
func buildConfidence(d *document, c *model.Confidence) {
	d.heading(2, "Confidence")

	switch {
	case c == nil:
		d.para(text("Confidence not computed: this scan was stored before wsaw scored scans."))

		return

	case c.Band == model.ConfidenceNone:
		d.para(bold("no observation"), text(" — this scan produced no asset list to trust."))

	default:
		d.para(bold(string(c.Band)), textf(" · %d of 100", c.Score))
	}

	rows := make([][][]span, 0, len(c.Reasons))

	for _, r := range c.Reasons {
		against := text("—")

		switch {
		case r.NotAssessed != "":
			against = text("not assessed: " + r.NotAssessed)
		case r.Reference != "":
			against = untrusted(r.Reference)
		}

		points := text("0")
		if r.Points != 0 {
			points = textf("−%d", r.Points)
		}

		rows = append(rows, [][]span{{text(string(r.Signal))}, {untrusted(r.Observed)}, {against}, {points}})
	}

	d.table([]column{narrow("Signal"), wide("Observed"), wide("Compared against"), figure("Points")}, rows)
}

func buildConsent(d *document, res *model.Result) {
	c := res.Consent

	d.heading(2, "Consent")

	outcome := []span{bold("Outcome:"), text(" "), code(string(c.Outcome))}
	if c.Reason != "" {
		outcome = append(outcome, text(" — "+c.Reason))
	}

	items := []item{{Spans: outcome}}

	switch {
	case c.Kind == model.CMPKindBespoke:
		// A banner nobody recognized is not an absent banner, and the report
		// must not let the two read the same (Story 2.9, AC1). It also is not
		// a vendor: whatever the matching rule called itself, no product was
		// identified here, and the line says so before it says anything else.
		cmp := []span{bold("CMP:"), text(" no vendor identified — the page has a consent banner of its own")}
		if c.Detection != "" {
			cmp = append(cmp, text(", matched by "), code(c.Detection))
		}

		items = append(items, item{Spans: cmp})

	case c.CMP != "":
		cmp := []span{bold("CMP:"), text(" " + c.CMP)}
		if c.CMPVersion != "" {
			cmp = append(cmp, text(" (version "+c.CMPVersion+")"))
		}

		items = append(items, item{Spans: append(cmp, text(", detected via "), code(c.Detection))})

	default:
		items = append(items, item{Spans: []span{bold("CMP:"), text(" none detected — no consent banner was found on the page")}})
	}

	if c.BotDetected {
		// A CMP that never ran shows no banner, and a reader must not take
		// the missing banner for a site without one (Story 2.11, AC5).
		items = append(items, item{Spans: []span{
			bold("Blocked as bot:"),
			text(" the CMP took this browser for a bot and never ran (loader request "), code(c.LoaderRequestID), text(")"),
		}})
	}

	if c.BannerHeading != "" {
		items = append(items, item{Spans: []span{bold("Banner:"), text(" "), untrusted(c.BannerHeading)}})
	}

	if len(c.StorageKeys) != 0 {
		items = append(items, item{Spans: append([]span{bold("Recorded in Web Storage:"), text(" ")}, codes(c.StorageKeys)...)})
	}

	if c.Mechanism != "" {
		mech := []span{bold("Mechanism:"), text(" "), code(c.Mechanism)}

		if c.Heuristic {
			// The reader must be able to weigh the evidence, and a label
			// guess is much weaker than a documented API call.
			mech = append(mech, text(" — "), bold("heuristic label matching; treat this result with caution"))
		}

		items = append(items, item{Spans: mech})
	}

	if c.TCString != "" {
		items = append(items, item{Spans: []span{bold("TC string:"), text(" "), code(c.TCString)}})
	}

	if c.GPPString != "" {
		items = append(items, item{Spans: []span{bold("GPP string:"), text(" "), code(c.GPPString)}})
	}

	for _, name := range c.StaleHostRules {
		items = append(items, item{Spans: []span{
			bold("Stale rule:"), text(" "), code(name), text(" is written for this host but no longer matches it"),
		}})
	}

	if diag := c.Diagnostic; diag != nil {
		// What was found, what it said, and what it offered: the three things
		// a rule author needs in order to write the rule that was missing
		// (Story 2.9, AC4).
		found := item{Spans: []span{bold("Banner found but not handled:")}}

		if diag.Element != "" {
			found.Sub = append(found.Sub, []span{text("element: "), code(diag.Element)})
		}

		if diag.Text != "" {
			found.Sub = append(found.Sub, []span{text("text: "), untrusted(diag.Text)})
		}

		if len(diag.Controls) != 0 {
			found.Sub = append(found.Sub, append([]span{text("controls: ")}, codes(diag.Controls)...))
		}

		items = append(items, found)
	}

	d.list(items...)
}

// codes renders values as code spans separated by commas.
func codes(values []string) []span {
	out := make([]span, 0, 2*len(values))

	for i, v := range values {
		if i > 0 {
			out = append(out, text(", "))
		}

		out = append(out, code(v))
	}

	return out
}

func buildPreConsent(d *document, res *model.Result) {
	domains := res.ThirdPartyDomains(model.PhasePre)

	d.heading(2, "Third parties contacted before any consent interaction")

	if len(domains) == 0 {
		d.para(text("None."))

		buildPreConsentFirstParty(d, res)

		return
	}

	if res.ConsentMode == model.ConsentReject {
		d.para(text("These hosts were contacted before the banner was rejected:"))
	} else {
		d.para(text("These hosts were contacted before any consent decision was expressed:"))
	}

	d.codeList(domains)

	buildPreConsentFirstParty(d, res)

	if res.ConsentMode == model.ConsentReject {
		if post := res.ThirdPartyDomains(model.PhasePost); len(post) > 0 {
			d.heading(3, "Third parties contacted after rejection")
			d.codeList(post)
		}
	}
}

// buildPreConsentFirstParty lists the site's own hosts contacted before the
// interaction. "No third parties" is a narrower claim than it looks: a
// tracker reverse-proxied onto the site's own domain — analytics on
// `hog.example.com`, a server-side tag container on `t.example.com` — is
// first-party by registrable domain and vanishes from a third-party
// summary. Naming the hosts does not classify them; it stops the report from
// implying that nothing was contacted (Story 2.9, AC7).
func buildPreConsentFirstParty(d *document, res *model.Result) {
	hosts := res.FirstPartyHosts(model.PhasePre)
	if len(hosts) == 0 {
		return
	}

	d.heading(3, "First-party hosts contacted before any consent interaction")
	d.para(text("These are the site's own hosts. A self-hosted or reverse-proxied analytics endpoint " +
		"appears here rather than above, so check them before reading the count above as \"nothing happened\"."))
	d.codeList(hosts)
}

func buildCounts(d *document, res *model.Result) {
	counts := res.CountsByResourceType()

	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}

	sort.Strings(types)

	rows := make([][][]span, 0, len(types)+1)
	for _, t := range types {
		rows = append(rows, [][]span{{text(t)}, {textf("%d", counts[t])}})
	}

	rows = append(rows, [][]span{{bold("total")}, {bold(fmt.Sprint(len(res.Requests)))}})

	d.heading(2, "Requests by resource type")
	d.table([]column{narrow("Type"), figure("Count")}, rows)
}

func buildHosts(d *document, res *model.Result) {
	summaries := res.HostSummaries()

	d.heading(2, "Hosts")

	if len(summaries) == 0 {
		if !res.OK() {
			// Not "nothing was loaded": the scan stopped before it could say.
			d.para(text("No network requests were recorded before the scan stopped, so this says nothing about what the page loads."))
		} else {
			d.para(text("No network requests were recorded."))
		}

		return
	}

	rows := make([][][]span, 0, len(summaries))
	for _, h := range summaries {
		rows = append(rows, [][]span{
			{code(h.Domain)}, {text(string(h.Party))},
			{textf("%d", h.Requests)}, {textf("%d", h.PreConsent)},
			{span{Text: fmt.Sprint(h.Bytes), PDFText: FormatBytes(h.Bytes)}}, {textf("%d", h.Failed)},
			{text(strings.Join(h.ResourceTypes, ", "))},
		})
	}

	d.table([]column{
		wide("Domain"), narrow(colParty), figure("Requests"), figure("Pre-consent"), figure("Bytes"), figure("Failed"),
		wide("Types"),
	}, rows)
}

func buildCookies(d *document, res *model.Result) {
	if len(res.Cookies) == 0 {
		return
	}

	cookies, counts := merged(res.Cookies)
	collides := collisions(cookies, func(c model.Cookie) string {
		return strings.Join([]string{c.Name, c.Domain, string(c.Party), c.SameSite,
			fmt.Sprint(c.Session, c.Secure, c.HTTPOnly)}, "\x00")
	})

	rows := make([][][]span, 0, len(cookies))
	for i, c := range cookies {
		name := append([]span{code(c.Name)}, repeats(counts[i])...)
		if collides[i] {
			name = append(name, textf(" (path %s, value %s)", c.Path, digestPrefix(c.ValueSHA256)))
		}

		rows = append(rows, [][]span{
			name, {code(c.Domain)}, {text(string(c.Party))},
			{textf("%t", c.Session)}, {textf("%t", c.Secure)}, {textf("%t", c.HTTPOnly)}, {text(c.SameSite)},
		})
	}

	d.heading(2, "Cookies")
	d.table([]column{
		wide("Name"), wide("Domain"), narrow(colParty),
		narrow("Session"), narrow("Secure"), narrow("HttpOnly"), narrow("SameSite"),
	}, rows)
	d.para(text("Cookie values are not stored; only a digest and length are kept."))
}

func buildStorage(d *document, res *model.Result) {
	if len(res.Storage) == 0 {
		return
	}

	entries, counts := merged(res.Storage)
	collides := collisions(entries, func(e model.StorageEntry) string {
		return strings.Join([]string{e.Origin, string(e.Area), e.Key, string(e.Party), fmt.Sprint(e.ValueLength)}, "\x00")
	})

	rows := make([][][]span, 0, len(entries))
	for i, e := range entries {
		key := append([]span{{Text: e.Key, Code: true, Untrusted: true}}, repeats(counts[i])...)
		if collides[i] {
			key = append(key, textf(" (value %s)", digestPrefix(e.ValueSHA256)))
		}

		rows = append(rows, [][]span{
			{code(e.Origin)}, {text(string(e.Area))}, key, {text(string(e.Party))}, {textf("%d", e.ValueLength)},
		})
	}

	d.heading(2, "Web storage")
	d.table([]column{wide("Origin"), narrow("Area"), wide("Key"), narrow(colParty), narrow("Value length")}, rows)
	d.para(text("Storage values are not stored; only a digest and length are kept."))
}

// Basis says what a report's diff was computed against, so the report names
// it the way the scan page does (Story 5.36, AC5).
type Basis string

// Comparison bases.
const (
	// BasisUnknown is a caller that does not know; the report names the
	// compared scan only.
	BasisUnknown Basis = ""
	// BasisBaseline is the series' approved baseline.
	BasisBaseline Basis = "baseline"
	// BasisPrevious is the previous scan of a series with no baseline.
	BasisPrevious Basis = "previous"
	// BasisOwnBaseline is a baseline, compared with the scan before it.
	BasisOwnBaseline Basis = "own-baseline"
)

// sentence names the compared scan, or returns nil when the basis is unknown.
func (b Basis) sentence(scanID string) []span {
	switch b {
	case BasisBaseline:
		return []span{text("Compared against the approved baseline, scan "), code(scanID), text(".")}
	case BasisPrevious:
		return []span{text("Compared against the previous scan, "), code(scanID), text(". This series has no approved baseline.")}
	case BasisOwnBaseline:
		return []span{text("This scan is the baseline. Changes are shown against the previous scan, "), code(scanID), text(".")}
	default:
		return nil
	}
}

func buildDiff(d *document, rep *diff.Report, basis Basis) {
	d.heading(2, "Changes")

	// An empty section reads as "nothing changed", so it says why it is
	// empty (Story 5.39, AC2).
	if rep == nil {
		d.para(text("No comparison is part of this report."))

		return
	}

	if !rep.Comparable {
		d.para(text("No comparison was possible: " + rep.Reason))

		return
	}

	named := basis.sentence(rep.BaselineScanID)

	if len(rep.Changes) == 0 {
		if named != nil {
			d.para(named...)
			d.para(text("No changes."))
		} else {
			d.para(text("No changes against the baseline."))
		}

		if rep.Suppressed > 0 {
			d.para(textf("%d change(s) were suppressed by the allow list.", rep.Suppressed))
		}

		return
	}

	switch {
	case named != nil:
		d.para(named...)
	case rep.BaselineScanID != "":
		// Blank on a shared view, which must not name a scan outside the
		// one shared (Story 5.19, AC3).
		d.para(text("Compared against scan "), code(rep.BaselineScanID), text("."))
	}

	rows := make([][][]span, 0, len(rep.Changes))
	for _, c := range rep.Changes {
		rows = append(rows, [][]span{
			{bold(string(c.Severity))}, {text(string(c.Type))}, {code(c.Subject)}, {untrusted(c.Detail)},
		})
	}

	// The PDF stacks detail over subject in one cell, so neither is squeezed
	// into a column one word wide (Story 5.39, AC11).
	detail := wide("Detail")
	detail.Lead = true

	d.table([]column{narrow("Severity"), narrow("Change"), wide("Subject"), detail}, rows)

	if rep.Suppressed > 0 {
		d.para(textf("%d further change(s) were suppressed by the allow list.", rep.Suppressed))
	}
}

func buildEnvironment(d *document, res *model.Result) {
	e := res.Environment

	items := []item{
		{Spans: []span{textf("wsaw %s, Chrome %s", orNone(e.WsawVersion), orNone(e.ChromeVersion))}},
		{Spans: []span{textf("Viewport %dx%d at %gx, mobile=%t", e.ViewportWidth, e.ViewportHeight, e.DeviceScale, e.Mobile)}},
	}

	if e.UserAgent != "" {
		items = append(items, item{Spans: []span{text("User agent override: "), code(e.UserAgent)}})
	} else {
		items = append(items, item{Spans: []span{text("User agent: browser default")}})
	}

	if e.AcceptLanguage != "" {
		items = append(items, item{Spans: []span{text("Accept-Language: "), code(e.AcceptLanguage)}})
	}

	if e.Timezone != "" {
		items = append(items, item{Spans: []span{text("Timezone: "), code(e.Timezone)}})
	}

	if len(e.ExtraHeaders) > 0 {
		// Names only: values may be credentials.
		items = append(items, item{Spans: append([]span{text("Extra headers: ")}, codes(e.ExtraHeaders)...)})
	}

	if e.BasicAuth {
		items = append(items, item{Spans: []span{text("Basic authentication was used")}})
	}

	if e.FormLoginURL != "" {
		items = append(items, item{Spans: []span{text("Logged in through the form at "), code(e.FormLoginURL)}})
	}

	if e.Proxy != "" {
		items = append(items, item{Spans: []span{text("Proxy: "), code(e.Proxy)}})
	}

	d.heading(2, "Scan environment")
	d.list(items...)
}

// merged lists items identical in every stored field once, in first-seen
// order, with how often each occurred. The result keeps every entry; this is
// only how the report shows them (Story 5.39, AC14).
func merged[T any](items []T) ([]T, []int) {
	out := make([]T, 0, len(items))
	counts := make([]int, 0, len(items))
	at := make(map[string]int, len(items))

	for i, it := range items {
		b, err := json.Marshal(it)

		key := string(b)
		if err != nil {
			// Not expected for result types; an item that cannot be keyed
			// is shown on its own rather than merged with anything.
			key = fmt.Sprintf("\x00%d", i)
		}

		if j, ok := at[key]; ok {
			counts[j]++

			continue
		}

		at[key] = len(out)
		out = append(out, it)
		counts = append(counts, 1)
	}

	return out, counts
}

// collisions marks the items that share what the table shows with another
// item, so their rows can name what tells them apart.
func collisions[T any](items []T, shown func(T) string) []bool {
	seen := make(map[string]int, len(items))
	for _, it := range items {
		seen[shown(it)]++
	}

	out := make([]bool, len(items))
	for i, it := range items {
		out[i] = seen[shown(it)] > 1
	}

	return out
}

func repeats(n int) []span {
	if n < 2 {
		return nil
	}

	return []span{textf(" ×%d", n)}
}

func digestPrefix(sha string) string {
	if len(sha) < 8 {
		return orNone(sha)
	}

	return sha[:8]
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}

	return s
}
