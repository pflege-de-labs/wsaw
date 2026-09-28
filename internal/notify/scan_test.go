package notify_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/notify"
)

// Story 2.10, AC1 and AC2: a verified necessary-only outcome is the best a
// banner with no reject control allows, so it is trustworthy in reject mode.
// No other consent outcome changes, and neither does any scan-level check.
func TestTrustworthinessByConsentOutcomeInRejectMode(t *testing.T) {
	t.Parallel()

	cases := map[model.ConsentOutcome]bool{
		model.OutcomeApplied:       true,
		model.OutcomeNotNeeded:     true,
		model.OutcomeNecessaryOnly: true,
		model.OutcomeFailed:        false,
		model.OutcomeUnverified:    false,
		model.OutcomeBannerVisible: false,
	}

	for outcome, want := range cases {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()

			ev := notify.NewScanEvent(teamsResult(func(r *model.Result) {
				r.Consent = model.Consent{Outcome: outcome, CMP: "Cookiebot"}
			}), teamsReport())

			if ev.Trustworthy != want {
				t.Errorf("trustworthy = %v, want %v (caveat %q)", ev.Trustworthy, want, ev.Caveat)
			}

			if ev.Trustworthy && ev.Caveat != "" {
				t.Errorf("a trustworthy scan carries a caveat: %q", ev.Caveat)
			}
		})
	}
}

// Story 2.10, AC1: the scan-level checks run first. A necessary-only scan that
// failed or was cut short is still not a result anyone should read.
func TestNecessaryOnlyDoesNotRescueABrokenScan(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		mutate func(*model.Result)
		want   string
	}{
		"failed": {
			mutate: func(r *model.Result) {
				r.Termination = model.TermError
				r.Error = "the browser crashed"
			},
			want: "failed",
		},
		"skipped": {
			mutate: func(r *model.Result) {
				r.Termination = model.TermSkipped
				r.Error = "disallowed by robots.txt"
			},
			want: "did not run",
		},
		"truncated": {
			mutate: func(r *model.Result) { r.Termination = model.TermRequestCap },
			want:   "cut short",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ev := notify.NewScanEvent(teamsResult(func(r *model.Result) {
				r.Consent = model.Consent{Outcome: model.OutcomeNecessaryOnly}
				tc.mutate(r)
			}), teamsReport())

			if ev.Trustworthy {
				t.Fatal("a broken scan was trusted because its consent outcome was necessary-only")
			}

			if !strings.Contains(ev.Caveat, tc.want) {
				t.Errorf("caveat %q does not name the scan-level problem %q", ev.Caveat, tc.want)
			}
		})
	}
}

// Story 2.10, AC2: only a reject can produce necessary-only. If one ever
// appears under another mode, something upstream is wrong, and the scan is
// not promoted.
func TestNecessaryOnlyOutsideRejectModeIsNotTrusted(t *testing.T) {
	t.Parallel()

	for _, mode := range []model.ConsentMode{model.ConsentNone, model.ConsentAccept} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			ev := notify.NewScanEvent(teamsResult(func(r *model.Result) {
				r.ConsentMode = mode
				r.Consent = model.Consent{Outcome: model.OutcomeNecessaryOnly}
			}), teamsReport())

			if ev.Trustworthy {
				t.Fatalf("necessary-only in %s mode was trusted", mode)
			}

			if !strings.Contains(ev.Caveat, string(mode)) {
				t.Errorf("caveat %q does not name the mode it was recorded under", ev.Caveat)
			}
		})
	}
}

func necessaryOnlyResult(r *model.Result) {
	r.Consent = model.Consent{
		Outcome: model.OutcomeNecessaryOnly,
		CMP:     "Cookiebot",
		Reason: `rule "cookiebot" has no reject control; wsaw limited consent to strictly ` +
			"necessary categories via its necessary-only fallback",
	}
}

// Story 2.10, AC4: an unchanged necessary-only scan is a clean scan, and a
// channel that heard about it on every run would soon be muted.
func TestTeamsStaysQuietForAnUnchangedNecessaryOnlyScan(t *testing.T) {
	t.Parallel()

	srv := newCapture(http.StatusAccepted).serve()
	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, nil)

	if ev := notify.NewScanEvent(teamsResult(necessaryOnlyResult), teamsReport()); tn.WantsScan(ev) {
		t.Error("an unchanged necessary-only scan would be posted")
	}
}

// Story 2.10, AC3 and AC4: once there is something to say, the card says it
// like any other trustworthy scan, and still states that the banner has no
// reject control.
func TestTeamsCardForANecessaryOnlyScanStatesTheOutcomeWithoutACaveat(t *testing.T) {
	t.Parallel()

	for name, legacy := range map[string]bool{"adaptive card": false, "legacy message card": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			hits := newCapture(http.StatusAccepted)
			srv := hits.serve()

			t.Cleanup(srv.Close)

			tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.Legacy = legacy })

			ev := notify.NewScanEvent(teamsResult(necessaryOnlyResult),
				teamsReport(change(diff.SeverityHigh, "tracker.test")))

			if !tn.WantsScan(ev) {
				t.Fatal("a necessary-only scan with a high finding was filtered out")
			}

			if err := tn.DeliverScan(t.Context(), ev); err != nil {
				t.Fatal(err)
			}

			text := hits.last()

			if strings.Contains(text, "not trustworthy") || strings.Contains(text, "⚠") {
				t.Errorf("the card still flags a verified necessary-only scan as untrustworthy:\n%s", text)
			}

			for _, want := range []string{"necessary-only", "no reject control"} {
				if !strings.Contains(text, want) {
					t.Errorf("the card no longer states %q:\n%s", want, text)
				}
			}
		})
	}
}
