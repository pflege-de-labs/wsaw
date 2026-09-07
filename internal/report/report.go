// Package report renders results and diffs into the formats humans and other
// systems consume.
//
// Every format here is a derived view: the JSON result schema is the real
// interface, and nothing appears in a Markdown report or a CSV that is not
// recoverable from the stored result (Tenet 16).
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// WriteJSON writes a result as indented JSON.
func WriteJSON(w io.Writer, res *model.Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("writing JSON result: %w", err)
	}

	return nil
}

// WriteJSONL writes one result per line, for streaming large runs.
func WriteJSONL(w io.Writer, results ...*model.Result) error {
	enc := json.NewEncoder(w)

	for _, res := range results {
		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("writing JSONL result: %w", err)
		}
	}

	return nil
}

// WriteCSV writes the host table, which is the view that goes into a
// spreadsheet during a compliance review.
func WriteCSV(w io.Writer, res *model.Result) error {
	cw := csv.NewWriter(w)

	header := []string{
		"target", "consent_mode", "consent_outcome", "scanned_at",
		"domain", "party", "requests", "pre_consent_requests", "bytes", "resource_types", "failed", "hosts",
	}

	if err := cw.Write(header); err != nil {
		return fmt.Errorf("writing CSV header: %w", err)
	}

	scannedAt := res.StartedAt.UTC().Format(time.RFC3339)

	for _, h := range res.HostSummaries() {
		row := []string{
			res.Target,
			string(res.ConsentMode),
			string(res.Consent.Outcome),
			scannedAt,
			h.Domain,
			string(h.Party),
			strconv.Itoa(h.Requests),
			strconv.Itoa(h.PreConsent),
			strconv.FormatInt(h.Bytes, 10),
			strings.Join(h.ResourceTypes, " "),
			strconv.Itoa(h.Failed),
			strings.Join(h.Hosts, " "),
		}

		if err := cw.Write(row); err != nil {
			return fmt.Errorf("writing CSV row: %w", err)
		}
	}

	cw.Flush()

	if err := cw.Error(); err != nil {
		return fmt.Errorf("flushing CSV: %w", err)
	}

	return nil
}

// WriteMarkdown renders a review-ready report.
//
// The ordering is deliberate: whether the scan is trustworthy comes first,
// then the consent outcome, then pre-consent traffic, then the diff. A
// reviewer must not read a host table before learning that the scan was
// truncated or that the banner was never dismissed.
func WriteMarkdown(w io.Writer, res *model.Result, rep *diff.Report) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s — %s\n\n", res.Target, res.ConsentMode)
	fmt.Fprintf(&b, "**URL:** %s  \n", res.URL)

	if res.FinalURL != "" && res.FinalURL != res.URL {
		fmt.Fprintf(&b, "**Final URL:** %s  \n", res.FinalURL)
	}

	fmt.Fprintf(&b, "**Scanned:** %s  \n", res.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "**Duration:** %s  \n", res.Duration.Round(time.Millisecond))
	fmt.Fprintf(&b, "**Scan ID:** `%s`\n\n", res.ScanID)

	writeTrust(&b, res)
	writeConsent(&b, res)
	writePreConsent(&b, res)
	writeCounts(&b, res)
	writeHosts(&b, res)
	writeCookies(&b, res)
	writeDiff(&b, rep)
	writeEnvironment(&b, res)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing Markdown report: %w", err)
	}

	return nil
}

// writeTrust states up front whether the numbers below can be believed.
func writeTrust(b *strings.Builder, res *model.Result) {
	switch {
	case !res.OK():
		fmt.Fprintf(b, "> **This scan did not complete.** Termination: `%s`.\n", res.Termination)

		if res.Error != "" {
			fmt.Fprintf(b, "> Error: %s\n", res.Error)
		}

		b.WriteString("> The asset list below is incomplete and must not be read as \"nothing was loaded\".\n\n")

	case res.Truncated():
		fmt.Fprintf(b, "> **This scan stopped early** (`%s`). The asset list may be incomplete,\n", res.Termination)
		b.WriteString("> and any reduction against the baseline may be an artefact rather than a real change.\n\n")
	}

	if len(res.Warnings) > 0 {
		b.WriteString("**Warnings**\n\n")

		for _, warn := range res.Warnings {
			fmt.Fprintf(b, "- %s\n", warn)
		}

		b.WriteString("\n")
	}
}

func writeConsent(b *strings.Builder, res *model.Result) {
	c := res.Consent

	b.WriteString("## Consent\n\n")

	fmt.Fprintf(b, "- **Outcome:** `%s`", c.Outcome)

	if c.Reason != "" {
		fmt.Fprintf(b, " — %s", c.Reason)
	}

	b.WriteString("\n")

	if c.CMP != "" {
		fmt.Fprintf(b, "- **CMP:** %s", c.CMP)

		if c.CMPVersion != "" {
			fmt.Fprintf(b, " (version %s)", c.CMPVersion)
		}

		fmt.Fprintf(b, ", detected via `%s`\n", c.Detection)
	} else {
		b.WriteString("- **CMP:** none detected\n")
	}

	if c.Mechanism != "" {
		fmt.Fprintf(b, "- **Mechanism:** `%s`", c.Mechanism)

		if c.Heuristic {
			// The reader must be able to weigh the evidence, and a label
			// guess is much weaker than a documented API call.
			b.WriteString(" — **heuristic label matching; treat this result with caution**")
		}

		b.WriteString("\n")
	}

	if c.TCString != "" {
		fmt.Fprintf(b, "- **TC string:** `%s`\n", c.TCString)
	}

	if c.GPPString != "" {
		fmt.Fprintf(b, "- **GPP string:** `%s`\n", c.GPPString)
	}

	b.WriteString("\n")
}

func writePreConsent(b *strings.Builder, res *model.Result) {
	domains := res.ThirdPartyDomains(model.PhasePre)

	b.WriteString("## Third parties contacted before any consent interaction\n\n")

	if len(domains) == 0 {
		b.WriteString("None.\n\n")

		return
	}

	if res.ConsentMode == model.ConsentReject {
		b.WriteString("These hosts were contacted before the banner was rejected:\n\n")
	} else {
		b.WriteString("These hosts were contacted before any consent decision was expressed:\n\n")
	}

	for _, d := range domains {
		fmt.Fprintf(b, "- `%s`\n", d)
	}

	b.WriteString("\n")

	if res.ConsentMode == model.ConsentReject {
		post := res.ThirdPartyDomains(model.PhasePost)
		if len(post) > 0 {
			b.WriteString("### Third parties contacted after rejection\n\n")

			for _, d := range post {
				fmt.Fprintf(b, "- `%s`\n", d)
			}

			b.WriteString("\n")
		}
	}
}

func writeCounts(b *strings.Builder, res *model.Result) {
	counts := res.CountsByResourceType()

	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}

	sort.Strings(types)

	b.WriteString("## Requests by resource type\n\n")
	b.WriteString("| Type | Count |\n|---|---:|\n")

	for _, t := range types {
		fmt.Fprintf(b, "| %s | %d |\n", t, counts[t])
	}

	fmt.Fprintf(b, "| **total** | **%d** |\n\n", len(res.Requests))
}

func writeHosts(b *strings.Builder, res *model.Result) {
	summaries := res.HostSummaries()

	b.WriteString("## Hosts\n\n")

	if len(summaries) == 0 {
		b.WriteString("No network requests were recorded.\n\n")

		return
	}

	b.WriteString("| Domain | Party | Requests | Pre-consent | Bytes | Failed | Types |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---|\n")

	for _, h := range summaries {
		fmt.Fprintf(b, "| `%s` | %s | %d | %d | %d | %d | %s |\n",
			h.Domain, h.Party, h.Requests, h.PreConsent, h.Bytes, h.Failed,
			strings.Join(h.ResourceTypes, ", "))
	}

	b.WriteString("\n")
}

func writeCookies(b *strings.Builder, res *model.Result) {
	if len(res.Cookies) == 0 {
		return
	}

	b.WriteString("## Cookies\n\n")
	b.WriteString("| Name | Domain | Party | Session | Secure | HttpOnly | SameSite |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")

	for _, c := range res.Cookies {
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %t | %t | %t | %s |\n",
			c.Name, c.Domain, c.Party, c.Session, c.Secure, c.HTTPOnly, c.SameSite)
	}

	b.WriteString("\nCookie values are not stored; only a digest and length are kept.\n\n")
}

func writeDiff(b *strings.Builder, rep *diff.Report) {
	if rep == nil {
		return
	}

	b.WriteString("## Changes\n\n")

	if !rep.Comparable {
		fmt.Fprintf(b, "No comparison was possible: %s\n\n", rep.Reason)

		return
	}

	if len(rep.Changes) == 0 {
		b.WriteString("No changes against the baseline.\n")

		if rep.Suppressed > 0 {
			fmt.Fprintf(b, "\n%d change(s) were suppressed by the allow list.\n", rep.Suppressed)
		}

		b.WriteString("\n")

		return
	}

	fmt.Fprintf(b, "Compared against scan `%s`.\n\n", rep.BaselineScanID)

	b.WriteString("| Severity | Change | Subject | Detail |\n|---|---|---|---|\n")

	for _, c := range rep.Changes {
		fmt.Fprintf(b, "| **%s** | %s | `%s` | %s |\n",
			c.Severity, c.Type, c.Subject, escapePipes(c.Detail))
	}

	if rep.Suppressed > 0 {
		fmt.Fprintf(b, "\n%d further change(s) were suppressed by the allow list.\n", rep.Suppressed)
	}

	b.WriteString("\n")
}

func writeEnvironment(b *strings.Builder, res *model.Result) {
	e := res.Environment

	b.WriteString("## Scan environment\n\n")
	fmt.Fprintf(b, "- wsaw %s, Chrome %s\n", orNone(e.WsawVersion), orNone(e.ChromeVersion))
	fmt.Fprintf(b, "- Viewport %dx%d at %gx, mobile=%t\n", e.ViewportWidth, e.ViewportHeight, e.DeviceScale, e.Mobile)

	if e.UserAgent != "" {
		fmt.Fprintf(b, "- User agent override: `%s`\n", e.UserAgent)
	} else {
		b.WriteString("- User agent: browser default\n")
	}

	if e.AcceptLanguage != "" {
		fmt.Fprintf(b, "- Accept-Language: `%s`\n", e.AcceptLanguage)
	}

	if e.Timezone != "" {
		fmt.Fprintf(b, "- Timezone: `%s`\n", e.Timezone)
	}

	if len(e.ExtraHeaders) > 0 {
		// Names only: values may be credentials.
		fmt.Fprintf(b, "- Extra headers: `%s`\n", strings.Join(e.ExtraHeaders, "`, `"))
	}

	if e.BasicAuth {
		b.WriteString("- Basic authentication was used\n")
	}

	if e.Proxy != "" {
		fmt.Fprintf(b, "- Proxy: `%s`\n", e.Proxy)
	}

	if e.WarmCache {
		b.WriteString("- **Warm cache**: counts are not comparable with cold-cache scans\n")
	}

	b.WriteString("\n")
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}

	return s
}

// escapePipes keeps a detail string from breaking the Markdown table it sits
// in. Detail text can contain a URL from the scanned page, so it is untrusted.
func escapePipes(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")

	return s
}
