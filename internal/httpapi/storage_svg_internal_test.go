package httpapi

import (
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"unicode/utf8"
)

// svgSizeRe reads the width attribute and the viewBox width off a chart's
// root <svg> element.
var svgSizeRe = regexp.MustCompile(`^<svg viewBox="0 0 (\d+) \d+" width="([^"]+)"`)

// rectXRe reads the x of each bar, so a test can measure the slot one bar
// and its labels have.
var rectXRe = regexp.MustCompile(`<rect x="([\d.]+)"`)

// assertDrawnAtNaturalSize fails when a chart's SVG would be stretched to
// its container: with width="100%" and the stylesheet's height:auto, a
// 640-unit viewBox on a 1800px-wide page scales every 11px label to ~30px.
// The width attribute must match the viewBox, so the stylesheet's
// max-width only ever shrinks a chart, never enlarges it.
func assertDrawnAtNaturalSize(t *testing.T, svg string) {
	t.Helper()

	m := svgSizeRe.FindStringSubmatch(svg)
	if m == nil {
		t.Fatalf("chart does not start with a sized <svg>: %.80s", svg)
	}

	if m[1] != m[2] {
		t.Errorf("svg width=%q, viewBox width %s: the chart scales with its container, and its text with it", m[2], m[1])
	}
}

func TestSeriesBarSVGIsDrawnAtNaturalSize(t *testing.T) {
	t.Parallel()

	svg := seriesBarSVG([]hBarRow{{Label: "Kneipp.Home / accept", Segments: []hBarSegment{{Bytes: 1 << 20, Fill: "var(--accent)"}}}})

	assertDrawnAtNaturalSize(t, string(svg))
}

func TestVBarChartIsDrawnAtNaturalSize(t *testing.T) {
	t.Parallel()

	svg := vBarChart("t", []string{"Sep", "Oct"}, []int64{284_100_000, 63_500_000}, formatBytes)

	assertDrawnAtNaturalSize(t, string(svg))
}

// TestVBarChartSlotFitsItsLabels pins the overlap the storage dashboard
// showed: twelve bars in 40-unit slots, each topped by a value such as
// "284.1 MB" about 50 units wide at the chart's font size, ran every label
// into its neighbour. Every slot must be at least as wide as the longest
// label it carries.
func TestVBarChartSlotFitsItsLabels(t *testing.T) {
	t.Parallel()

	labels := make([]string, pruneRunsChartLimit)
	values := make([]int64, pruneRunsChartLimit)

	for i := range labels {
		labels[i] = fmt.Sprintf("Sep %d", 20+i)
		values[i] = 888_800_000 + int64(i)
	}

	svg := string(vBarChart("t", labels, values, formatBytes))

	xs := rectXRe.FindAllStringSubmatch(svg, 2)
	if len(xs) != 2 {
		t.Fatalf("want at least two bars, got %d", len(xs))
	}

	x0, err := strconv.ParseFloat(xs[0][1], 64)
	if err != nil {
		t.Fatal(err)
	}

	x1, err := strconv.ParseFloat(xs[1][1], 64)
	if err != nil {
		t.Fatal(err)
	}

	slot := x1 - x0

	// A conservative average glyph advance for the UI's sans-serif face:
	// digits and capitals run about 0.6em, and a label must fit with room
	// to spare rather than exactly.
	const glyphEm = 0.6

	for _, s := range []string{formatBytes(values[0]), labels[0]} {
		need := float64(utf8.RuneCountInString(s)) * glyphEm * chartFontSize
		if slot < need {
			t.Errorf("slot %.1f units, label %q needs about %.1f: neighbouring labels overlap", slot, s, need)
		}
	}
}
