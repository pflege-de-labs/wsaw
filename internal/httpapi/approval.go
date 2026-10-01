package httpapi

import (
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Whether a scan may be approved as the baseline, from whichever page offers
// it (Story 5.36).
//
// Approving used to be possible only from a row of the history table. The
// scan's own page now offers it too, because that is where the evidence the
// decision rests on is. Two pages offering one action must not disagree about
// when it is available, so the rule lives here and both call it.

// approvalState is what a page may offer about making one scan the baseline.
type approvalState string

const (
	// approvalOffered: the scan can be approved, and this deployment may
	// write.
	approvalOffered approvalState = "offered"
	// approvalCurrent: the scan already is the baseline. Approving it again
	// would write an audit record for a change that did not happen
	// (Story 5.20, AC4).
	approvalCurrent approvalState = "current"
	// approvalRefused: the scan did not produce a trustworthy asset list, and
	// the store would refuse it. The page says so instead of offering a
	// button that can only fail (AC2).
	approvalRefused approvalState = "refused"
	// approvalHidden: the deployment is read-only. Nothing is offered and
	// nothing is explained, as on the history page before this story.
	approvalHidden approvalState = "hidden"
)

// Offered, Current and Refused let a template branch on the state without
// spelling its string values.
func (a approvalState) Offered() bool { return a == approvalOffered }

// Current reports that the scan is the series' baseline.
func (a approvalState) Current() bool { return a == approvalCurrent }

// Refused reports that the scan cannot be a baseline at all.
func (a approvalState) Refused() bool { return a == approvalRefused }

// approvalFor decides what a page may offer. Being the baseline is stated even
// on a read-only deployment, because identifying the baseline is a read
// (Story 5.20, AC8); a failed scan's explanation is not, because a read-only
// reader was never going to be offered the action it explains.
func approvalFor(ok, isBaseline, writable bool) approvalState {
	switch {
	case isBaseline:
		return approvalCurrent
	case !writable:
		return approvalHidden
	case !ok:
		return approvalRefused
	default:
		return approvalOffered
	}
}

// summaryOK applies Result.OK to a history row, so that the history page and
// the scan page judge a scan by one definition and not two that can drift.
func summaryOK(s store.Summary) bool {
	r := model.Result{Error: s.Error, Termination: s.Termination}

	return r.OK()
}

// approveFromResult is the value of the approve form's "from" field when the
// form sits on a scan's own page.
const approveFromResult = "result"

// approveReturn decides where an approval returns to.
//
// The form says which page it was sent from, never where to go: the
// destination is built here from the target, mode and scan ID the handler has
// already read, so no form field, query parameter or Referer can point the
// redirect anywhere else (AC4, Tenet 9). Anything but the scan page's marker
// returns to the target's history page, as every approval did before this
// story.
func approveReturn(from, target string, mode model.ConsentMode, scanID string) string {
	if from == approveFromResult && scanID != "" {
		return resultPath(target, mode, scanID)
	}

	return "/targets/" + target + "/" + string(mode)
}
