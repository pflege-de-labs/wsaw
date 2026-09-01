package notify

import (
	"context"
	"time"

	"github.com/martint17r/wsaw/internal/diff"
	"github.com/martint17r/wsaw/internal/model"
)

// ScanEvent is one scan and everything that changed in it.
//
// It exists because a webhook consumer and a chat channel want opposite
// shapes. A consumer wants one event per change, so each can be routed and
// deduplicated on its own. A channel wants one message per scan: forty changes
// posted as forty messages is unreadable and gets rate-limited (Story 5.14,
// AC4). Rather than make one notifier fold events back together, the
// dispatcher publishes both shapes and each notifier takes the one it needs.
type ScanEvent struct {
	Target      string            `json:"target"`
	URL         string            `json:"url"`
	ConsentMode model.ConsentMode `json:"consentMode"`
	Labels      map[string]string `json:"labels,omitempty"`

	ScanID   string        `json:"scanId"`
	At       time.Time     `json:"at"`
	Duration time.Duration `json:"durationNs"`

	Termination model.TerminationReason `json:"termination"`
	// Trustworthy is false when the scan failed, was skipped, or was cut
	// short. A truncated scan with few findings must never be presented as a
	// clean one (Tenet 5, Story 5.14 AC7).
	Trustworthy bool `json:"trustworthy"`
	// Caveat explains an untrustworthy scan in one sentence.
	Caveat string `json:"caveat,omitempty"`

	ConsentOutcome model.ConsentOutcome `json:"consentOutcome"`
	ConsentReason  string               `json:"consentReason,omitempty"`
	CMP            string               `json:"cmp,omitempty"`

	Requests          int `json:"requests"`
	ThirdPartyDomains int `json:"thirdPartyDomains"`
	PreConsentDomains int `json:"preConsentDomains"`

	Changes []diff.Change `json:"changes"`
	// Highest is the most severe change present, empty when there are none.
	Highest diff.Severity `json:"highestSeverity,omitempty"`
	// Suppressed counts changes an allow list hid, so a quiet report is
	// distinguishable from an over-configured one.
	Suppressed int `json:"suppressed"`

	// Comparable is false when this scan could not be compared to anything,
	// which is why a first scan reports no changes.
	Comparable bool   `json:"comparable"`
	Reason     string `json:"reason,omitempty"`
}

// NewScanEvent folds a result and its diff into one event.
func NewScanEvent(res *model.Result, rep *diff.Report) ScanEvent {
	ev := ScanEvent{
		Target:            res.Target,
		URL:               res.URL,
		ConsentMode:       res.ConsentMode,
		Labels:            res.Labels,
		ScanID:            res.ScanID,
		At:                res.StartedAt,
		Duration:          res.Duration,
		Termination:       res.Termination,
		ConsentOutcome:    res.Consent.Outcome,
		ConsentReason:     res.Consent.Reason,
		CMP:               res.Consent.CMP,
		Requests:          len(res.Requests),
		ThirdPartyDomains: len(res.ThirdPartyDomains("")),
		PreConsentDomains: len(res.ThirdPartyDomains(model.PhasePre)),
	}

	ev.Trustworthy, ev.Caveat = trustworthiness(res)

	if rep != nil {
		ev.Changes = rep.Changes
		ev.Suppressed = rep.Suppressed
		ev.Comparable = rep.Comparable
		ev.Reason = rep.Reason

		for _, c := range rep.Changes {
			if c.Severity.AtLeast(ev.Highest) {
				ev.Highest = c.Severity
			}
		}
	}

	return ev
}

// trustworthiness decides whether a scan's findings can be read at face value.
func trustworthiness(res *model.Result) (bool, string) {
	switch {
	case res.Termination == model.TermSkipped:
		return false, "this scan did not run: " + res.Error

	case !res.OK():
		return false, "this scan failed: " + res.Error

	case res.Truncated():
		return false, "this scan was cut short (" + string(res.Termination) +
			"), so assets the page would have loaded later are missing"

	case res.Consent.Outcome == model.OutcomeFailed:
		return false, "the consent interaction failed, so the consent state during this scan is not the one requested"

	case res.Consent.Outcome == model.OutcomeUnverified:
		return false, "the consent interaction could not be verified, so the consent state during this scan is unconfirmed"

	default:
		return true, ""
	}
}

// ScanNotifier delivers one message per scan. It is separate from Notifier
// rather than an extension of it because the two disagree about what an event
// is, and a notifier should implement only the shape it actually wants.
type ScanNotifier interface {
	Name() string
	WantsScan(ev ScanEvent) bool
	DeliverScan(ctx context.Context, ev ScanEvent) error
}

// wantsScanEvent is the filtering rule shared by scan-level notifiers.
//
// A chat channel is not a log: posting "nothing changed" after every scan
// trains people to ignore the channel, which is the failure this product can
// least afford. So a scan is worth a message when it found something at or
// above the threshold — or when its own result cannot be trusted, because
// silence about a broken scan reads as a clean one (AC7).
func wantsScanEvent(ev ScanEvent, minSeverity diff.Severity, targets []string, labels map[string]string) bool {
	if len(targets) > 0 && !contains(targets, ev.Target) {
		return false
	}

	for k, v := range labels {
		if ev.Labels[k] != v {
			return false
		}
	}

	if !ev.Trustworthy {
		return true
	}

	if len(ev.Changes) == 0 {
		return false
	}

	if minSeverity == "" {
		return true
	}

	return ev.Highest.AtLeast(minSeverity)
}
