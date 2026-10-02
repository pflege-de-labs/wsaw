package notify

import (
	"fmt"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// ConfidenceGate is a notifier's minimum confidence (Story 5.37). The zero
// value has no threshold and changes nothing.
type ConfidenceGate struct {
	// Enabled is false when the notifier has no minConfidence.
	Enabled bool
	// Min is the lowest score whose findings are reported at face value.
	Min int
	// Drop holds a below-threshold scan's findings back instead of
	// delivering them under a caveat.
	Drop bool
}

// ScanConfidence is a scan's stored confidence as a notification carries it.
// It is copied from the result document, never recomputed, so a notification
// and the scan page always state the same score (AC2).
type ScanConfidence struct {
	Score int                  `json:"score"`
	Band  model.ConfidenceBand `json:"band"`
	// Reason is the largest penalty in words, empty when nothing cost points.
	Reason string `json:"reason,omitempty"`
}

func scanConfidence(res *model.Result) *ScanConfidence {
	if res.Confidence == nil {
		return nil
	}

	out := &ScanConfidence{Score: res.Confidence.Score, Band: res.Confidence.Band}

	top := 0

	for _, r := range res.Confidence.Reasons {
		if r.Points > top {
			top = r.Points
			out.Reason = r.Observed
		}
	}

	return out
}

// below is the one threshold rule both notifier kinds apply (AC6).
//
// A scan that observed nothing is never below: it is already reported as
// untrustworthy, and a threshold that hid it would hide a watcher that has
// stopped seeing anything (AC5). A scan without a stored score is below any
// threshold, because "not computed" must never read as 100 (AC2).
func (g ConfidenceGate) below(c *ScanConfidence, observedNothing bool) bool {
	if !g.Enabled || observedNothing {
		return false
	}

	if c == nil {
		return true
	}

	if c.Band == model.ConfidenceNone {
		return false
	}

	return c.Score < g.Min
}

// drops reports whether a delivery is held back entirely.
func (g ConfidenceGate) drops(c *ScanConfidence, observedNothing bool) bool {
	return g.Drop && g.below(c, observedNothing)
}

func (g ConfidenceGate) caveat(c *ScanConfidence) string {
	if c == nil {
		return fmt.Sprintf("confidence was not computed for this scan, and this channel requires %d of 100", g.Min)
	}

	s := fmt.Sprintf("confidence %d of 100, below this channel's %d", c.Score, g.Min)
	if c.Reason != "" {
		s += ": " + c.Reason
	}

	return s
}

// markScan labels a below-threshold scan as low confidence. Only a scan the
// existing rules trust is relabelled: the threshold adds a reason to distrust
// a scan, and a scan that is already untrustworthy keeps the caveat that says
// what actually went wrong with it.
func (g ConfidenceGate) markScan(ev ScanEvent) ScanEvent {
	if !ev.Trustworthy || !g.below(ev.Confidence, ev.observedNothing) {
		return ev
	}

	ev.Trustworthy = false
	ev.Caveat = g.caveat(ev.Confidence)
	ev.BelowConfidence = true

	return ev
}

// markChange labels one change event from a below-threshold scan.
func (g ConfidenceGate) markChange(ev Event) Event {
	ev.BelowConfidence = g.below(ev.Confidence, ev.observedNothing)

	return ev
}

// confidenceGated is a notifier with a minimum confidence. The dispatcher,
// not the notifier, holds a dropped delivery back, because the dispatcher
// owns the log line and the counter that keep a drop from being silent (AC4).
type confidenceGated interface {
	confidenceGate() ConfidenceGate
}
