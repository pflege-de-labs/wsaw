package diff

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// TextChecks is what makes a watched text complete (Story 4.15, AC8). It
// comes from the target's configuration and is applied when results are
// compared, so a change to it applies to history too (Tenet 4).
type TextChecks struct {
	// MustContain are strings that must each occur in the normalized text.
	MustContain []string
	// MinLength is the fewest characters the text may have.
	MinLength int
	// CheckAnchors reports in-page links whose target is missing.
	CheckAnchors bool
}

// NormalizeText is the form two texts are compared in: every line trimmed,
// runs of whitespace inside a line collapsed to one space, blank lines
// dropped (Story 4.15, AC6). It is derived from the stored text whenever it
// is needed and never stored in its place.
func NormalizeText(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]

	for _, l := range lines {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			out = append(out, l)
		}
	}

	return strings.Join(out, "\n")
}

// compareText reports a watched text that differs from the baseline's
// (Story 4.15, AC7, AC14). A text that could not be read on either side is
// not compared: that is textIncomplete's to report, and "changed" would be a
// guess.
func (d *differ) compareText() {
	before, after := d.baseline.Text, d.current.Text
	if before == nil || after == nil || before.Unavailable != "" || after.Unavailable != "" {
		return
	}

	textDiffers := NormalizeText(before.Text) != NormalizeText(after.Text)
	linksDiffer := countLinkChanges(before.Links, after.Links)

	if !textDiffers && linksDiffer == 0 {
		return
	}

	// The detail carries sizes, never the text: it reaches notifiers and
	// logs, and page text is untrusted (Tenet 9). The text is on the scan
	// page, escaped.
	detail := fmt.Sprintf("text %d → %d characters", before.Length, after.Length)
	if !textDiffers {
		detail = fmt.Sprintf("text unchanged, %d characters", after.Length)
	}

	if linksDiffer > 0 {
		detail += fmt.Sprintf("; %d link(s) changed", linksDiffer)
	}

	// The references are wsaw's own, so they are safe to carry, and they let
	// a reader fetch both texts (AC7).
	if before.Ref != "" && after.Ref != "" {
		detail += fmt.Sprintf("; stored as %s and %s", before.Ref, after.Ref)
	}

	d.add(Change{
		Type:     TextChanged,
		Severity: d.rules.Severity.TextChanged,
		Subject:  after.Selector,
		Before:   before.SHA256,
		After:    after.SHA256,
		Detail:   detail,
	})
}

// countLinkChanges counts the links that differ between two lists, compared
// by rendered text and resolved target, as a multiset: a link moved within
// the region is not a change, a link whose target changed is (AC14).
func countLinkChanges(before, after []model.TextLink) int {
	key := func(l model.TextLink) string { return NormalizeText(l.Text) + "\x00" + l.Target }

	seen := make(map[string]int, len(before))
	for _, l := range before {
		seen[key(l)]++
	}

	changed := 0

	for _, l := range after {
		if seen[key(l)] > 0 {
			seen[key(l)]--
		} else {
			changed++
		}
	}

	gone := 0
	for _, n := range seen {
		gone += n
	}

	return max(changed, gone)
}

// TextWithLinks is the text as a reader with the link targets would see it:
// the rendered text, then every link written as [text](target), one per
// line, in document order (Story 4.15, AC14). It is what the scan page's diff
// shows and what a check sends to a model (Story 4.16, AC3), so a retargeted
// link is a changed line and a model reads where each link really points.
func TextWithLinks(tc *model.TextCapture) string {
	if tc == nil || len(tc.Links) == 0 {
		if tc == nil {
			return ""
		}

		return tc.Text
	}

	var b strings.Builder

	b.WriteString(tc.Text)
	b.WriteString("\n\nLinks:\n")

	for _, l := range tc.Links {
		fmt.Fprintf(&b, "[%s](%s)\n", strings.Join(strings.Fields(l.Text), " "), l.Target)
	}

	return b.String()
}

// textFindings is what a watched text reports on every scan, whatever it is
// compared against: an incomplete text and a model's verdict on it.
func textFindings(current *model.Result, rules Options) []Change {
	var out []Change

	if c, ok := textIncomplete(current, rules); ok {
		out = append(out, c)
	}

	if c, ok := textCheckVerdict(current, rules); ok {
		out = append(out, c)
	}

	return out
}

// textCheckVerdict reports a model's failed or uncertain verdict (Story
// 4.16, AC6). The detail says it is model output and carries the verdict and
// the number of deciding findings, never the findings: they are a model's
// rewording of page text and reach notifiers and logs (AC4, Tenet 9). A
// check that did not run reports nothing here; it lowers the confidence
// instead (AC5).
func textCheckVerdict(current *model.Result, rules Options) (Change, bool) {
	if current.Text == nil || current.Text.Check == nil {
		return Change{}, false
	}

	chk := current.Text.Check

	c := Change{
		Target:      current.Target,
		ConsentMode: current.ConsentMode,
		Subject:     current.Text.Selector,
		Detail: fmt.Sprintf("model output (%s), not a compliance result: verdict %s, %d deciding finding(s)",
			chk.Model, chk.Verdict, chk.Deciding()),
	}

	switch chk.Verdict {
	case model.VerdictFailed:
		c.Type, c.Severity = TextCheckFailed, rules.Severity.TextCheckFailed
	case model.VerdictUncertain:
		c.Type, c.Severity = TextCheckUncertain, rules.Severity.TextCheckUncertain
	default:
		return Change{}, false
	}

	return c, true
}

// textIncomplete checks the current text against what the target says it
// must be, on every scan and with no baseline (Story 4.15, AC8). It reports
// once per scan, with every reason, until the text passes; approving a
// baseline does not silence it.
func textIncomplete(current *model.Result, rules Options) (Change, bool) {
	if current == nil || current.Text == nil || rules.Text == nil {
		return Change{}, false
	}

	tc := current.Text

	// A scan that never loaded the page is reported as degraded; its text
	// was never there to be incomplete.
	if tc.Unavailable != "" && !current.OK() {
		return Change{}, false
	}

	reasons := textProblems(tc, rules.Text)
	if len(reasons) == 0 {
		return Change{}, false
	}

	return Change{
		Type:        TextIncomplete,
		Severity:    rules.Severity.TextIncomplete,
		Target:      current.Target,
		ConsentMode: current.ConsentMode,
		Subject:     tc.Selector,
		Detail:      strings.Join(reasons, "; "),
	}, true
}

// textProblems lists why a text is incomplete. Each reason names what is
// missing in operator terms; the configured strings are the operator's, the
// fragments are quoted because they come from the page.
func textProblems(tc *model.TextCapture, checks *TextChecks) []string {
	if tc.Unavailable != "" {
		return []string{"no text: " + tc.Unavailable}
	}

	var reasons []string

	norm := NormalizeText(tc.Text)

	var missing []string

	for _, s := range checks.MustContain {
		if !strings.Contains(norm, NormalizeText(s)) {
			missing = append(missing, fmt.Sprintf("%q", s))
		}
	}

	if len(missing) > 0 {
		reasons = append(reasons, "missing "+strings.Join(missing, ", "))
	}

	if checks.MinLength > 0 && tc.Length < checks.MinLength {
		reasons = append(reasons, fmt.Sprintf("%d characters, fewer than the minimum of %d", tc.Length, checks.MinLength))
	}

	if checks.CheckAnchors {
		var dangling []string

		for _, f := range tc.Fragments {
			if !f.Found && !slices.Contains(dangling, f.Href) {
				dangling = append(dangling, f.Href)
			}
		}

		if len(dangling) > 0 {
			reasons = append(reasons, "in-page links to nothing: "+quoteAll(dangling))
		}
	}

	return reasons
}

// quoteAll quotes page-controlled strings, so a fragment cannot pass itself
// off as part of the sentence around it.
func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}

	return strings.Join(q, ", ")
}

// LineOp says what a line of a text diff is.
type LineOp byte

// Line operations.
const (
	LineSame    LineOp = ' '
	LineRemoved LineOp = '-'
	LineAdded   LineOp = '+'
)

// TextLine is one line of a text diff.
type TextLine struct {
	Op   LineOp
	Text string
}

// maxDiffCells bounds the line diff's work (lines before × lines after in
// the part that differs). Beyond it the differing part is shown as removed
// and then added, which is still correct, only less precise.
const maxDiffCells = 4_000_000

// TextDiff is a line diff of two texts in their normalized form, for the
// scan page (Story 4.15, AC7). The common beginning and end are found first,
// which is all a cut text needs; the middle gets a longest-common-
// subsequence diff when it is small enough.
func TextDiff(before, after string) []TextLine {
	a := splitLines(NormalizeText(before))
	b := splitLines(NormalizeText(after))

	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}

	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}

	out := make([]TextLine, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		out = append(out, TextLine{LineSame, l})
	}

	out = append(out, middleDiff(a[pre:len(a)-suf], b[pre:len(b)-suf])...)

	for _, l := range a[len(a)-suf:] {
		out = append(out, TextLine{LineSame, l})
	}

	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}

	return strings.Split(s, "\n")
}

func middleDiff(a, b []string) []TextLine {
	var out []TextLine

	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			out = append(out, TextLine{LineRemoved, l})
		}

		for _, l := range b {
			out = append(out, TextLine{LineAdded, l})
		}

		return out
	}

	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}

	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, TextLine{LineSame, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, TextLine{LineRemoved, a[i]})
			i++
		default:
			out = append(out, TextLine{LineAdded, b[j]})
			j++
		}
	}

	for ; i < len(a); i++ {
		out = append(out, TextLine{LineRemoved, a[i]})
	}

	for ; j < len(b); j++ {
		out = append(out, TextLine{LineAdded, b[j]})
	}

	return out
}
