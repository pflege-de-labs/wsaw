// Package confidence scores how far a scan's asset list can be trusted
// (Story 5.35).
//
// The score is derived and pure: it reads one result and the durations of
// earlier scans in the same series, and nothing else (Tenets 3 and 13). Every
// point it takes off is kept as a reason, because a number alone tells a
// reader that something is wrong and not what.
package confidence

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Penalties, in points off 100. They are code rather than configuration on
// purpose: a score whose meaning differs between deployments cannot be
// compared across them, and changing one should come with a test.
const (
	penaltyTimeout      = 30
	penaltyCapped       = 25
	penaltyFetchesMax   = 60
	fetchesPerRatio     = 400
	penaltyDurationFar  = 15
	penaltyDurationVery = 30
	penaltyConsent      = 40
	penaltyNecessary    = 20
	penaltyHeuristic    = 10
)

// Band thresholds.
const (
	maxScore     = 100
	highFloor    = 90
	mediumFloor  = 60
	degradedCeil = mediumFloor - 1
)

// Duration reference bounds.
const (
	// HistoryWanted is how many earlier scans the median is taken over, at
	// most. Enough to ride out one odd day, few enough that a site which
	// genuinely got slower is the new normal within two weeks of daily scans.
	HistoryWanted = 10
	// historyNeeded is the fewest earlier scans a median is formed from.
	// Below it one slow evening is half the reference.
	historyNeeded = 5
)

// Options carries the deployment settings the score must agree with.
type Options struct {
	// DegradedFailureRatio is the differ's threshold for calling a scan
	// degraded, already resolved to its default. A scan at or above it is
	// capped at band low, so the board cannot call a scan trustworthy that
	// the differ, for the same scan, reports as degraded.
	DegradedFailureRatio float64
}

// History is what the score knows about the series' earlier scans.
type History struct {
	// Durations of earlier scans of the same series that ended idle, newest
	// first. Truncated and failed scans are not in it, so a run of broken
	// scans cannot make the next broken one look normal.
	Durations []time.Duration
	// Unavailable says why Durations could not be read. Non-empty means the
	// duration signal is not assessed, rather than assessed against nothing.
	Unavailable string
}

// Score computes a result's confidence.
func Score(res *model.Result, hist History, opts Options) model.Confidence {
	if !res.OK() {
		return failed(res)
	}

	ref, durationReason := duration(res.Duration, hist)

	reasons := []model.ConfidenceReason{
		termination(res.Termination),
		fetches(res),
		durationReason,
		consent(res),
	}

	score := maxScore
	for _, r := range reasons {
		score -= r.Points
	}

	score = max(score, 0)

	degraded := res.IncompleteRatio() >= opts.DegradedFailureRatio
	if degraded {
		score = min(score, degradedCeil)
	}

	return model.Confidence{
		Score:             score,
		Band:              band(score),
		Reasons:           reasons,
		DurationReference: ref,
	}
}

// failed is the confidence of a scan that produced no asset list at all. It is
// not a low score: a low score is a poor observation, and this is none.
func failed(res *model.Result) model.Confidence {
	observed := string(res.Termination)
	if res.Error != "" {
		observed += ": " + res.Error
	}

	return model.Confidence{
		Score: 0,
		Band:  model.ConfidenceNone,
		Reasons: []model.ConfidenceReason{{
			Signal:   model.SignalTermination,
			Points:   maxScore,
			Observed: observed,
		}},
	}
}

func band(score int) model.ConfidenceBand {
	switch {
	case score >= highFloor:
		return model.ConfidenceHigh
	case score >= mediumFloor:
		return model.ConfidenceMedium
	default:
		return model.ConfidenceLow
	}
}

func termination(t model.TerminationReason) model.ConfidenceReason {
	r := model.ConfidenceReason{Signal: model.SignalTermination, Observed: string(t)}

	switch t {
	case model.TermTimeout:
		r.Points = penaltyTimeout
		r.Observed = "stopped at the time budget before the page went idle"
	case model.TermRequestCap:
		r.Points = penaltyCapped
		r.Observed = "stopped at the request cap before the page went idle"
	case model.TermByteCap:
		r.Points = penaltyCapped
		r.Observed = "stopped at the byte cap before the page went idle"
	default:
		r.Observed = "the page went idle"
	}

	return r
}

// fetches scores the requests wsaw could not observe. It counts exactly what
// the differ's degraded check counts, so the two cannot disagree about what a
// lost request is: a blocked request and an HTTP error are observations.
func fetches(res *model.Result) model.ConfidenceReason {
	total := 0

	for i := range res.Requests {
		if !res.Requests[i].NonNetwork {
			total++
		}
	}

	lost := res.IncompleteObservations()
	r := model.ConfidenceReason{Signal: model.SignalFetches}

	if total == 0 || lost == 0 {
		r.Observed = fmt.Sprintf("all %d network requests observed", total)

		return r
	}

	ratio := res.IncompleteRatio()
	r.Points = min(int(math.Round(ratio*fetchesPerRatio)), penaltyFetchesMax)
	r.Observed = fmt.Sprintf("%d of %d network requests not observed", lost, total)

	if reason, n := res.TopIncompleteReason(); reason != "" {
		r.Observed += fmt.Sprintf(", most often %s (%d)", reason, n)
	}

	return r
}

// duration compares a scan's length with the series median. Short and long are
// both penalised: a scan that ended fast usually saw less page than usual, and
// a slow one is the one nearest its budget.
func duration(d time.Duration, hist History) (*model.DurationReference, model.ConfidenceReason) {
	r := model.ConfidenceReason{Signal: model.SignalDuration, Observed: roundDuration(d)}

	if hist.Unavailable != "" {
		r.NotAssessed = hist.Unavailable

		return nil, r
	}

	earlier := hist.Durations
	if len(earlier) > HistoryWanted {
		earlier = earlier[:HistoryWanted]
	}

	if len(earlier) < historyNeeded {
		r.NotAssessed = fmt.Sprintf("%d of %d earlier clean scans in this series", len(earlier), historyNeeded)

		return nil, r
	}

	median := medianOf(earlier)
	ref := &model.DurationReference{Median: median, Scans: len(earlier)}
	r.Reference = fmt.Sprintf("median %s over %d earlier scans", roundDuration(median), len(earlier))

	switch {
	case d*4 < median || d > median*4:
		r.Points = penaltyDurationVery
	case d*2 < median || d > median*2:
		r.Points = penaltyDurationFar
	}

	return ref, r
}

func medianOf(ds []time.Duration) time.Duration {
	sorted := slices.Clone(ds)
	slices.Sort(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}

	return (sorted[mid-1] + sorted[mid]) / 2
}

func roundDuration(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

func consent(res *model.Result) model.ConfidenceReason {
	r := model.ConfidenceReason{Signal: model.SignalConsent, Observed: string(res.Consent.Outcome)}

	if caveat := ConsentCaveat(res); caveat != "" {
		r.Observed = caveat

		if res.Consent.Outcome == model.OutcomeNecessaryOnly {
			r.Points = penaltyNecessary
		} else {
			r.Points = penaltyConsent
		}
	}

	if res.Consent.Heuristic {
		r.Points += penaltyHeuristic
		r.Observed += "; the interaction used heuristic label matching"
	}

	return r
}

// ConsentCaveat returns why a completed scan's consent state cannot be taken
// at face value, or "" when it can.
//
// It is the one statement of these rules. Notifications call a scan
// untrustworthy for exactly these reasons, and the score takes points off for
// exactly the same ones, so a scan a notification warns about can never read
// as high confidence on the board.
func ConsentCaveat(res *model.Result) string {
	switch {
	case res.Consent.Outcome == model.OutcomeFailed:
		return "the consent interaction failed, so the consent state during this scan is not the one requested"

	case res.Consent.Outcome == model.OutcomeUnverified:
		return "the consent interaction could not be verified, so the consent state during this scan is unconfirmed"

	case res.Consent.Outcome == model.OutcomeBannerVisible:
		return "the CMP recorded the requested choice, but the banner was still displayed to a visitor"

	// A verified necessary-only outcome is trusted in reject mode: it is the
	// closest state to a rejection that a banner with no reject control
	// allows, and it was observed, not assumed. The missing control is a fact
	// about the site, which the outcome itself carries into every view
	// (Story 2.10). Only a reject can produce it, so under any other mode it
	// means something upstream went wrong.
	case res.Consent.Outcome == model.OutcomeNecessaryOnly && res.ConsentMode != model.ConsentReject:
		return "the consent outcome necessary-only was recorded in " + string(res.ConsentMode) +
			" mode, which only a reject can produce"

	default:
		return ""
	}
}
