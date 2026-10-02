package httpapi

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 5.36: the one rule both pages use to decide what they offer for
// approving a scan as the baseline.

func TestApprovalFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                     string
		ok, isBaseline, writable bool
		want                     approvalState
	}{
		{"approvable", true, false, true, approvalOffered},
		{"failed scan", false, false, true, approvalRefused},
		{"the baseline", true, true, true, approvalCurrent},
		// Identifying the baseline is a read, so a read-only deployment
		// still says which scan it is (Story 5.20, AC8).
		{"the baseline, read-only", true, true, false, approvalCurrent},
		{"approvable, read-only", true, false, false, approvalHidden},
		// A read-only reader is offered nothing, so nothing is explained.
		{"failed scan, read-only", false, false, false, approvalHidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := approvalFor(tc.ok, tc.isBaseline, tc.writable); got != tc.want {
				t.Errorf("approvalFor(%v, %v, %v) = %q, want %q", tc.ok, tc.isBaseline, tc.writable, got, tc.want)
			}
		})
	}
}

// A history row and a scan page must judge one scan the same way, so a row is
// judged by Result.OK itself rather than a second definition of it.
func TestSummaryOKAgreesWithResultOK(t *testing.T) {
	t.Parallel()

	for _, term := range []model.TerminationReason{
		model.TermIdle, model.TermTimeout, model.TermRequestCap, model.TermByteCap,
		model.TermError, model.TermSkipped,
	} {
		for _, errText := range []string{"", "boom"} {
			res := model.Result{Termination: term, Error: errText}
			sum := store.Summary{Termination: term, Error: errText}

			if got, want := summaryOK(sum), res.OK(); got != want {
				t.Errorf("termination %q, error %q: summaryOK = %v, Result.OK = %v", term, errText, got, want)
			}
		}
	}
}

func TestApproveReturn(t *testing.T) {
	t.Parallel()

	const history = "/targets/site/reject"

	for _, tc := range []struct {
		name, from, scanID, want string
	}{
		{"from the scan page", approveFromResult, "scan-1", "/results/site/reject/scan-1"},
		{"from the history page", "", "scan-1", history},
		{"scan page without a scan", approveFromResult, "", history},
		// The field names a page, never a destination: anything else is
		// the history page, whatever it looks like.
		{"an off-site URL", "https://evil.example/", "scan-1", history},
		{"a scheme-relative URL", "//evil.example", "scan-1", history},
		{"a local path", "/audit", "scan-1", history},
		// The scan ID is escaped into one path segment, so it cannot climb
		// out of the result page.
		{"a scan ID with a slash", approveFromResult, "../../audit", "/results/site/reject/..%2F..%2Faudit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := approveReturn(tc.from, "site", model.ConsentReject, tc.scanID); got != tc.want {
				t.Errorf("approveReturn(%q, %q) = %q, want %q", tc.from, tc.scanID, got, tc.want)
			}
		})
	}
}
