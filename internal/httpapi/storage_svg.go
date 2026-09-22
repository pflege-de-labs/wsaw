package httpapi

import (
	"fmt"
	"html"
	"html/template"
	"strings"
)

// This file draws the storage dashboard's three diagrams as inline SVG
// computed here, in Go — no chart library, no CDN, no JavaScript (Story
// 5.32, AC8): the binary stays self-contained (Tenet 14), and the page's
// read paths work with script off, the rule Story 5.16 already states for
// auto-refresh.
//
// Colours come from the interface's own CSS custom properties
// (var(--accent), and so on) rather than a literal hex value baked in here.
// A presentation attribute such as fill="var(--accent)" is a CSS value, not
// an inline style, so it is unaffected by the style-src CSP that forbids
// unsafe-inline (server.go) and it repaints correctly for the light and dark
// palettes wsaw.css already defines.
//
// AC9 is the rule every chart here follows: no diagram is the only carrier
// of a fact. Every bar states its value as text, every chart carries a
// <title> and an aria-label naming what it shows and its largest value, and
// colour never distinguishes anything a label does not — the same rule the
// watchboard's cycle bar already follows (Story 5.25).

// svgEscape is html.EscapeString under a name that says why it is called
// here: every text value embedded in the SVG markup below is untrusted in
// the same way any other rendered value is (server.go's own package
// comment) — a target name is operator-configured, but a `&` or a `<` in
// one must not become a second SVG element.
func svgEscape(s string) string { return html.EscapeString(s) }

const (
	chartFontSize = 11
	chartRowH     = 22
	chartBarH     = 14
	chartLabelW   = 160
	chartWidth    = 640
	chartPlotW    = chartWidth - chartLabelW - 80
)

// hBarSegment is one coloured piece of a stacked horizontal bar.
type hBarSegment struct {
	Bytes int64
	Fill  string
}

// hBarRow is one row of the storage-by-series diagram: a label, the
// segments that make up its bar, and the text shown at the bar's end.
type hBarRow struct {
	Label    string
	Detail   string
	Segments []hBarSegment
}

func (r hBarRow) total() int64 {
	var n int64
	for _, s := range r.Segments {
		n += s.Bytes
	}

	return n
}

// seriesBarSVG draws AC4's diagram: one horizontal bar per series, split
// into a document segment and an artifact segment, in the same order the
// table beside it lists them — so eye and figure never disagree.
func seriesBarSVG(rows []hBarRow) template.HTML {
	if len(rows) == 0 {
		return ""
	}

	var maxTotal int64

	for _, r := range rows {
		if t := r.total(); t > maxTotal {
			maxTotal = t
		}
	}

	height := len(rows)*chartRowH + 16

	var b strings.Builder

	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" width="100%%" height="%d" role="img" `+
		`aria-label="Storage by series. Largest: %s (%s)." xmlns="http://www.w3.org/2000/svg">`,
		chartWidth, height, height, svgEscape(rows[0].Label), formatBytes(maxTotal))
	fmt.Fprintf(&b, `<title>Storage by series, largest first</title>`)

	for i, r := range rows {
		y := i*chartRowH + 8

		fmt.Fprintf(&b, `<text x="0" y="%d" font-size="%d" fill="var(--fg)" dominant-baseline="middle">%s</text>`,
			y+chartBarH/2, chartFontSize, svgEscape(truncateLabel(r.Label, 22)))

		x := chartLabelW

		for _, seg := range r.Segments {
			w := 0.0
			if maxTotal > 0 {
				w = float64(seg.Bytes) / float64(maxTotal) * float64(chartPlotW)
			}

			if w > 0 {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%.1f" height="%d" fill="%s"><title>%s</title></rect>`,
					x, y, w, chartBarH, seg.Fill, svgEscape(r.Detail))
				x += int(w)
			}
		}

		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="%d" fill="var(--fg)" dominant-baseline="middle">%s</text>`,
			chartLabelW+chartPlotW+6, y+chartBarH/2, chartFontSize, formatBytes(r.total()))
	}

	b.WriteString(`</svg>`)

	return template.HTML(b.String()) //nolint:gosec // built from escaped fragments only
}

// vBarChart draws one vertical bar chart: a bar per labelled value, its
// value as text below it, and the whole chart carrying a <title> and an
// aria-label naming what it shows and its largest value (AC5, AC6, AC9). A
// value of zero still draws a zero-height bar rather than being
// indistinguishable from a gap — AC6 needs a freed-nothing prune run to be a
// visible bar, not an absence.
func vBarChart(title string, labels []string, values []int64, format func(int64) string) template.HTML {
	if len(labels) == 0 {
		return ""
	}

	const (
		width     = chartWidth
		plotH     = 120
		barGap    = 4
		labelH    = 28
		valueH    = 14
		topMargin = 8
	)

	barW := float64(width) / float64(len(labels))
	if barW > 40 {
		barW = 40
	}

	var maxV int64

	maxIdx := 0

	for i, v := range values {
		if v > maxV {
			maxV = v
			maxIdx = i
		}
	}

	height := topMargin + valueH + plotH + labelH

	var b strings.Builder

	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" width="100%%" height="%d" role="img" `+
		`aria-label="%s. Largest: %s (%s)." xmlns="http://www.w3.org/2000/svg">`,
		width, height, height, svgEscape(title), svgEscape(labels[maxIdx]), format(maxV))
	fmt.Fprintf(&b, `<title>%s</title>`, svgEscape(title))

	totalW := barW * float64(len(labels))
	startX := (float64(width) - totalW) / 2

	for i, v := range values {
		h := 0.0
		if maxV > 0 {
			h = float64(v) / float64(maxV) * float64(plotH)
		}

		x := startX + float64(i)*barW
		y := float64(topMargin + valueH + plotH)

		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="var(--accent)"><title>%s: %s</title></rect>`,
			x+barGap/2, y-h, barW-barGap, h, svgEscape(labels[i]), format(v))

		fmt.Fprintf(&b, `<text x="%.1f" y="%d" font-size="%d" fill="var(--fg)" text-anchor="middle">%s</text>`,
			x+barW/2, topMargin+valueH-2, chartFontSize, format(v))

		fmt.Fprintf(&b, `<text x="%.1f" y="%d" font-size="%d" fill="var(--muted)" text-anchor="middle">%s</text>`,
			x+barW/2, height-6, chartFontSize, svgEscape(labels[i]))
	}

	b.WriteString(`</svg>`)

	return template.HTML(b.String()) //nolint:gosec // built from escaped fragments only
}

// truncateLabel keeps a series label from overrunning its column, ending it
// with an ellipsis when it does not fit. The full name is still readable in
// the table beside the chart, so nothing here is the only place it appears.
func truncateLabel(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n-1]) + "…"
}
