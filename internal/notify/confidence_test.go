package notify_test

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/confidence"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/notify"
)

// Story 5.35, AC6: the score and the notification share their consent rules,
// so a scan a notification calls untrustworthy never reads as high confidence
// on the board. Asserted over every mode, outcome and termination, not over
// the cases the rules happen to name, so a rule added to one side and not the
// other fails here.
func TestAnUntrustworthyScanIsNeverHighConfidence(t *testing.T) {
	t.Parallel()

	modes := []model.ConsentMode{model.ConsentNone, model.ConsentReject, model.ConsentAccept}
	outcomes := []model.ConsentOutcome{
		model.OutcomeApplied, model.OutcomeNotNeeded, model.OutcomeNecessaryOnly,
		model.OutcomeFailed, model.OutcomeUnverified, model.OutcomeBannerVisible,
	}
	terminations := []model.TerminationReason{
		model.TermIdle, model.TermTimeout, model.TermRequestCap, model.TermByteCap,
		model.TermError, model.TermSkipped,
	}

	hist := confidence.History{}
	for range 10 {
		hist.Durations = append(hist.Durations, 2*time.Second)
	}

	for _, mode := range modes {
		for _, outcome := range outcomes {
			for _, term := range terminations {
				res := teamsResult(func(r *model.Result) {
					r.ConsentMode = mode
					r.Consent = model.Consent{Outcome: outcome}
					r.Termination = term
					r.Duration = 2 * time.Second

					if term.Failed() {
						r.Error = "failed"
					}
				})

				ev := notify.NewScanEvent(res, teamsReport())
				c := confidence.Score(res, hist, confidence.Options{DegradedFailureRatio: 0.05})

				if !ev.Trustworthy && c.Band == model.ConfidenceHigh {
					t.Errorf("%s/%s/%s: untrustworthy (%s) but confidence high %d",
						mode, outcome, term, ev.Caveat, c.Score)
				}
			}
		}
	}
}
