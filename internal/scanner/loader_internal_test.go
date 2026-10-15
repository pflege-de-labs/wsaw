package scanner

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 2.11, AC6: consent.onFailure decides what a CMP that never started
// does to the scan, exactly as it does for a failure the handler saw itself.
func TestCheckLoadersFollowsTheFailurePolicy(t *testing.T) {
	t.Parallel()

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatalf("LoadBuiltinRules: %v", err)
	}

	for _, tc := range []struct {
		policy  consent.FailurePolicy
		warning bool
	}{
		{consent.FailFlag, false},
		{consent.FailScan, true},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			t.Parallel()

			s := &Scanner{deps: Deps{Rules: rules}, opts: Options{ConsentOnFailure: tc.policy}}
			res := &model.Result{
				Consent: model.Consent{Outcome: model.OutcomeNotNeeded, Kind: model.CMPKindNone},
				Requests: []model.Request{{
					RequestID: "7.2", URL: "https://cloud.ccm19.de/app.js?apiKey=k", Status: 200,
					BodySHA256: "fb9d57d628d04d66afc5bc2e97347a904f46d3f0629daa79757ff17b8ab652ee",
				}},
			}

			s.checkLoaders(config.Resolved{URL: "https://www.site.example/"}, res, slog.New(slog.DiscardHandler))

			if res.Consent.Outcome != model.OutcomeFailed || !res.Consent.BotDetected {
				t.Fatalf("consent = %s, bot %v; want failed and bot", res.Consent.Outcome, res.Consent.BotDetected)
			}

			var warned bool

			for _, w := range res.Warnings {
				warned = warned || strings.HasPrefix(w, "consent interaction: "+consent.ErrConsentFailed.Error())
			}

			if warned != tc.warning {
				t.Errorf("policy %s: consent failure warning = %v, want %v (%v)", tc.policy, warned, tc.warning, res.Warnings)
			}
		})
	}
}
