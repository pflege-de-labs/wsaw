package httpapi

// The tile's deviation headline (watchboard_delta.go), tested directly since
// modeRow is unexported. The rendered page's own assertions are in
// watchboard_delta_test.go.

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// deltaRow builds a row whose last scan carries the three counts, compared
// against a base carrying three others.
func deltaRow(last [3]int, base *ComparisonView, severity diff.Severity) modeRow {
	return modeRow{Series: SeriesView{
		Mode: model.ConsentReject,
		LastScan: &store.Summary{
			ScanID:            "scan-latest",
			Requests:          last[0],
			ThirdPartyDomains: last[1],
			PreConsentDomains: last[2],
		},
		ComparedTo: base,
		Severity:   severity,
	}}
}

func comparedTo(base ComparisonBase, counts [3]int, compared bool) *ComparisonView {
	return &ComparisonView{
		Base:              base,
		ScanID:            "scan-base",
		Comparable:        compared,
		Requests:          counts[0],
		ThirdPartyDomains: counts[1],
		PreConsentDomains: counts[2],
	}
}

func TestDeltaLabel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		row      modeRow
		want     string
		wantBase string
	}{
		{
			name:     "an increase in every figure",
			row:      deltaRow([3]int{25, 6, 3}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityHigh),
			want:     "+7 req · +1 3p · +2 pre",
			wantBase: "vs baseline",
		},
		{
			name:     "a decrease",
			row:      deltaRow([3]int{12, 2, 0}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityLow),
			want:     "−6 req · −3 3p · −1 pre",
			wantBase: "vs baseline",
		},
		{
			// The unmoved figure is left out: the line carries movement, and
			// "+0 3p" is attention spent on nothing.
			name:     "an unmoved figure is omitted",
			row:      deltaRow([3]int{20, 5, 0}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityMedium),
			want:     "+2 req · −1 pre",
			wantBase: "vs baseline",
		},
		{
			name:     "nothing moved and nothing changed",
			row:      deltaRow([3]int{18, 5, 1}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityInfo),
			want:     "no change",
			wantBase: "vs baseline",
		},
		{
			// A tracker swapped for another tracker moves no count at all.
			// The quiet line must not cover a finding.
			name:     "equal counts with a change found",
			row:      deltaRow([3]int{18, 5, 1}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityCritical),
			want:     "changed, same counts",
			wantBase: "vs baseline",
		},
		{
			name:     "the previous-scan fallback says so",
			row:      deltaRow([3]int{20, 5, 1}, comparedTo(BasePrevious, [3]int{18, 5, 1}, true), diff.SeverityMedium),
			want:     "+2 req",
			wantBase: "vs last scan",
		},
		{
			// No base at all: the absolutes are the only true figures, and
			// the qualifier says why they are absolutes.
			name:     "a first scan prints its absolutes",
			row:      deltaRow([3]int{18, 5, 1}, nil, ""),
			want:     "18 req · 5 3p · 1 pre",
			wantBase: "first scan",
		},
		{
			// An errored scan on either side: never "no change".
			name:     "an uncomparable pair prints its absolutes",
			row:      deltaRow([3]int{18, 5, 1}, comparedTo(BaseApproved, [3]int{0, 0, 0}, false), ""),
			want:     "18 req · 5 3p · 1 pre",
			wantBase: "not comparable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.row.DeltaLabel(); got != tc.want {
				t.Errorf("DeltaLabel() = %q, want %q", got, tc.want)
			}

			if got := tc.row.StateLabel(); got != tc.wantBase {
				t.Errorf("StateLabel() = %q, want %q", got, tc.wantBase)
			}
		})
	}
}

// The quiet state is exactly one of the eight above: a real comparison, no
// count moved, and no change found.
func TestNoChangeIsOnlyTheQuietState(t *testing.T) {
	t.Parallel()

	quiet := deltaRow([3]int{18, 5, 1}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityInfo)
	if !quiet.NoChange() {
		t.Error("an unchanged scan against its baseline does not read as quiet")
	}

	for _, row := range []modeRow{
		deltaRow([3]int{18, 5, 1}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityCritical),
		deltaRow([3]int{20, 5, 1}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityHigh),
		deltaRow([3]int{18, 5, 1}, nil, ""),
		deltaRow([3]int{18, 5, 1}, comparedTo(BaseApproved, [3]int{0, 0, 0}, false), ""),
		{},
	} {
		if row.NoChange() {
			t.Errorf("%q read as quiet", row.DeltaLabel())
		}
	}
}

// A series with no scan at all keeps the board's existing line, which is a
// different statement from "nothing changed" (Tenet 5).
func TestARowWithNoScanHasNoHeadline(t *testing.T) {
	t.Parallel()

	row := modeRow{}

	if row.HasScan() {
		t.Error("a series with no stored scan claims to have one")
	}

	if got := row.AbsoluteLabel(); got != "" {
		t.Errorf("AbsoluteLabel() = %q, want empty", got)
	}

	if got := row.StateLabel(); got != "" {
		t.Errorf("StateLabel() = %q, want empty", got)
	}

	if got := row.ScanAria("site"); got != "" {
		t.Errorf("ScanAria() = %q, want empty", got)
	}
}

// The absolutes the headline no longer prints are still one hover or one
// screen reader away, for both scans, with the base named.
func TestScanAriaCarriesBothScansFigures(t *testing.T) {
	t.Parallel()

	row := deltaRow([3]int{25, 6, 3}, comparedTo(BaseApproved, [3]int{18, 5, 1}, true), diff.SeverityHigh)

	aria := row.ScanAria("site")

	for _, want := range []string{
		"Open the last scan of site in reject mode, scan-latest",
		"+7 req · +1 3p · +2 pre vs baseline",
		"this scan 25 requests, 6 third-party domains, 3 pre-consent",
		"baseline scan-base 18 requests, 5 third-party domains, 1 pre-consent",
	} {
		if !strings.Contains(aria, want) {
			t.Errorf("ScanAria() = %q, missing %q", aria, want)
		}
	}

	fallback := deltaRow([3]int{25, 6, 3}, comparedTo(BasePrevious, [3]int{18, 5, 1}, true), diff.SeverityHigh)
	if !strings.Contains(fallback.ScanAria("site"), "previous scan scan-base 18 requests") {
		t.Errorf("the fallback base is not named in the accessible name: %q", fallback.ScanAria("site"))
	}
}
