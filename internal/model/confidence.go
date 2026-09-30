package model

import "time"

// Confidence states how far a scan's asset list can be trusted, and why
// (Story 5.35).
//
// It is interpretation, not capture: every figure in it is derived from the
// rest of the document and from the durations of earlier scans in the same
// series. It is written into the document anyway, the way NormalizedURL is,
// so that a stored scan explains its own score. DurationReference is the one
// input the document could not otherwise reproduce, since the scans it was
// taken from may since have been pruned.
type Confidence struct {
	// Score runs from 0 to 100. 100 means every signal was assessed and none
	// fired; it never means "nothing was checked".
	Score int            `json:"score"`
	Band  ConfidenceBand `json:"band"`
	// Reasons holds one entry per signal, in a fixed order, including the
	// signals that cost nothing and the ones that could not be assessed.
	Reasons []ConfidenceReason `json:"reasons"`
	// DurationReference is what the scan's duration was compared against.
	// Absent when there was not enough history to form one; the duration
	// reason then says why.
	DurationReference *DurationReference `json:"durationReference,omitempty"`
}

// ConfidenceBand is the score's reading in words, so a reader never has to
// remember where the thresholds are.
type ConfidenceBand string

// Confidence bands.
const (
	// ConfidenceHigh is a score of 90 or more.
	ConfidenceHigh ConfidenceBand = "high"
	// ConfidenceMedium is a score from 60 to 89.
	ConfidenceMedium ConfidenceBand = "medium"
	// ConfidenceLow is a score from 1 to 59, and every scan the differ calls
	// degraded, whatever its score.
	ConfidenceLow ConfidenceBand = "low"
	// ConfidenceNone is a scan that failed or was skipped. It observed
	// nothing, which is a different statement from observing poorly.
	ConfidenceNone ConfidenceBand = "none"
)

// ConfidenceSignal names one input to the score.
type ConfidenceSignal string

// Confidence signals, in the order a Confidence lists them.
const (
	SignalTermination ConfidenceSignal = "termination"
	SignalFetches     ConfidenceSignal = "fetches"
	SignalDuration    ConfidenceSignal = "duration"
	SignalConsent     ConfidenceSignal = "consent"
)

// ConfidenceReason is one signal's contribution to the score.
type ConfidenceReason struct {
	Signal ConfidenceSignal `json:"signal"`
	// Points is how much this signal took off the score. Zero for a signal
	// that was assessed and did not fire, and for one that was not assessed.
	Points int `json:"points"`
	// Observed is what the scan showed, in operator-readable terms.
	Observed string `json:"observed,omitempty"`
	// Reference is what Observed was compared against, where there is one.
	Reference string `json:"reference,omitempty"`
	// NotAssessed says why this signal could not be checked. A signal that
	// was not checked is listed rather than left out, so that a high score
	// cannot hide a check that never ran (Tenet 5).
	NotAssessed string `json:"notAssessed,omitempty"`
}

// DurationReference is the series median a scan's duration was compared
// against.
type DurationReference struct {
	Median time.Duration `json:"medianNs"`
	// Scans is how many earlier scans the median was taken over.
	Scans int `json:"scans"`
}
