package httpapi

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// confidenceNotComputed is what a tile says, in full, for a scan stored before
// schema 2.1. It is never a number: a missing score read as 100 would be a
// claim the scan never made (Story 5.35, AC10).
const confidenceNotComputed = "confidence not computed"

// noObservation is a failed or skipped scan, spelled out in the title and the
// accessible name behind its CXX mark (Story 5.35, AC3).
const noObservation = "no observation"

// ConfidenceLabel is the tile's compact confidence mark (Story 5.35, AC8): C
// and the score, as in C62; C? where no score was computed; and CXX for a
// failed or skipped scan. Neither of the last two is a number, because even
// C0 would read as a poor observation rather than as none. The band is in the
// title and the accessible name, and the mark itself carries what the colour
// does, so colour is never the only carrier.
func (r modeRow) ConfidenceLabel() string {
	last := r.Series.LastScan
	if last == nil {
		return ""
	}

	switch {
	case last.ConfidenceScore == nil:
		return "C?"
	case last.ConfidenceBand == model.ConfidenceNone:
		return "CXX"
	default:
		return "C" + strconv.Itoa(*last.ConfidenceScore)
	}
}

// confidenceSentence is the mark spelled out, for the title and the
// accessible name, where "C62" alone says nothing to a screen reader.
func (r modeRow) confidenceSentence() string {
	last := r.Series.LastScan
	if last == nil {
		return ""
	}

	switch {
	case last.ConfidenceScore == nil:
		return confidenceNotComputed
	case last.ConfidenceBand == model.ConfidenceNone:
		return noObservation
	default:
		return "confidence " + string(last.ConfidenceBand) + " · " + strconv.Itoa(*last.ConfidenceScore) + " of 100"
	}
}

// ConfidenceClass styles the line. High confidence is muted, like every other
// figure on the board that is only worth reading when something is wrong.
func (r modeRow) ConfidenceClass() string {
	last := r.Series.LastScan
	if last == nil || last.ConfidenceScore == nil {
		return "watch-conf muted"
	}

	return "watch-conf conf-" + string(last.ConfidenceBand)
}

// ConfidenceTitle answers "why this score" on hover and for a screen reader:
// every signal that cost points, largest first, then every signal that could
// not be assessed. Falls back to the label where the document could not be
// read, since the index holds the score and not its reasons.
func (r modeRow) ConfidenceTitle() string {
	label := r.confidenceSentence()

	c := r.Series.Confidence
	if c == nil {
		return label
	}

	return label + ": " + confidenceReasons(c)
}

// confidenceReasons writes a confidence's reasons as one sentence.
func confidenceReasons(c *model.Confidence) string {
	costly := make([]model.ConfidenceReason, 0, len(c.Reasons))

	var unassessed []string

	for _, reason := range c.Reasons {
		switch {
		case reason.Points > 0:
			costly = append(costly, reason)
		case reason.NotAssessed != "":
			unassessed = append(unassessed, string(reason.Signal)+" not assessed ("+reason.NotAssessed+")")
		}
	}

	// Stable, so equal penalties keep the document's own signal order and
	// the same scan always reads the same (Tenet 6).
	slices.SortStableFunc(costly, func(a, b model.ConfidenceReason) int {
		return cmp.Compare(b.Points, a.Points)
	})

	parts := make([]string, 0, len(costly)+len(unassessed))

	for _, reason := range costly {
		parts = append(parts, fmt.Sprintf("−%d %s: %s", reason.Points, reason.Signal, reason.Observed))
	}

	parts = append(parts, unassessed...)

	if len(parts) == 0 {
		return "every signal assessed, none fired"
	}

	return strings.Join(parts, "; ")
}
