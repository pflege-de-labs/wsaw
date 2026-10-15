package consent_test

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// ccm19BotStub is the digest of the body CCM19 serves a client it takes for a
// bot: 'use strict';console.info("CCM19 disabled for bots"); and a newline.
const ccm19BotStub = "fb9d57d628d04d66afc5bc2e97347a904f46d3f0629daa79757ff17b8ab652ee"

const ccm19Loader = "https://cloud.ccm19.de/app.js"

func TestLoaderFieldsAreValidatedAtLoad(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, rule, want string
	}{
		{
			name: "relative loader",
			rule: `
    loaders: ["/app.js"]`,
			want: "absolute http(s) URL",
		},
		{
			name: "stubs without loaders",
			rule: `
    stubs:
      - sha256: ` + ccm19BotStub + `
        means: bot`,
			want: "must name its loaders",
		},
		{
			name: "short digest",
			rule: `
    loaders: ["https://cmp.example/app.js"]
    stubs:
      - sha256: fb9d57d6
        means: bot`,
			want: "64 lowercase hex",
		},
		{
			name: "uppercase digest",
			rule: `
    loaders: ["https://cmp.example/app.js"]
    stubs:
      - sha256: ` + strings.ToUpper(ccm19BotStub) + `
        means: bot`,
			want: "64 lowercase hex",
		},
		{
			name: "unknown meaning",
			rule: `
    loaders: ["https://cmp.example/app.js"]
    stubs:
      - sha256: ` + ccm19BotStub + `
        means: maintenance`,
			want: "only defined meaning",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeRules(t, `
version: 1
rules:
  - name: cmp
    accept:
      - click: "#ok"`+tc.rule+"\n")

			_, err := consent.LoadRuleFiles(path)
			if err == nil {
				t.Fatal("the rule was accepted")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

func TestCheckLoaders(t *testing.T) {
	t.Parallel()

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatalf("LoadBuiltinRules: %v", err)
	}

	noCMP := model.Consent{
		Outcome:        model.OutcomeNotNeeded,
		Reason:         "no consent management platform detected",
		Kind:           model.CMPKindNone,
		StaleHostRules: []string{"kept"},
	}

	loader := func(mod func(*model.Request)) []model.Request {
		req := model.Request{
			RequestID:   "7.2",
			URL:         ccm19Loader + "?apiKey=secret&domain=d",
			Status:      200,
			DecodedSize: 54,
			BodySHA256:  ccm19BotStub,
		}
		if mod != nil {
			mod(&req)
		}

		return []model.Request{{RequestID: "1.1", URL: "https://site.example/"}, req}
	}

	for _, tc := range []struct {
		name     string
		in       model.Consent
		requests []model.Request
		want     model.ConsentOutcome
		bot      bool
		reason   []string
	}{
		{
			name:     "known bot stub",
			in:       noCMP,
			requests: loader(nil),
			want:     model.OutcomeFailed,
			bot:      true,
			reason:   []string{"CCM19 disabled itself", ccm19Loader, `"CCM19 disabled for bots"`, "proxy"},
		},
		{
			name:     "unknown body",
			in:       noCMP,
			requests: loader(func(r *model.Request) { r.BodySHA256 = strings.Repeat("0", 64); r.DecodedSize = 12 }),
			want:     model.OutcomeFailed,
			reason:   []string{"never started", "status 200, 12 bytes"},
		},
		{
			name:     "no digest recorded",
			in:       noCMP,
			requests: loader(func(r *model.Request) { r.BodySHA256 = "" }),
			want:     model.OutcomeFailed,
			reason:   []string{"never started"},
		},
		{
			name:     "loader failed",
			in:       noCMP,
			requests: loader(func(r *model.Request) { r.Failed, r.FailureReason, r.Status = true, "net::ERR_BLOCKED_BY_CLIENT", 0 }),
			want:     model.OutcomeFailed,
			reason:   []string{"failed: net::ERR_BLOCKED_BY_CLIENT"},
		},
		{
			name:     "no loader on the page",
			in:       noCMP,
			requests: []model.Request{{RequestID: "1.1", URL: "https://site.example/"}},
			want:     model.OutcomeNotNeeded,
		},
		{
			name:     "a driven CMP is never rewritten",
			in:       model.Consent{Outcome: model.OutcomeApplied, Kind: model.CMPKindVendor, CMP: "CCM19"},
			requests: loader(nil),
			want:     model.OutcomeApplied,
		},
		{
			name:     "a bespoke banner keeps its own failure",
			in:       model.Consent{Outcome: model.OutcomeFailed, Kind: model.CMPKindBespoke, Reason: "bespoke"},
			requests: loader(nil),
			want:     model.OutcomeFailed,
			reason:   []string{"bespoke"},
		},
		{
			// Consent mode none and a scan that never ran are not-needed with
			// no detection behind them; a loaded CMP is expected there.
			name:     "not-needed without a detection",
			in:       model.Consent{Outcome: model.OutcomeNotNeeded, Reason: "consent mode is none"},
			requests: loader(nil),
			want:     model.OutcomeNotNeeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := rules.CheckLoaders(tc.in, "www.site.example", "site.example", tc.requests)

			if got.Outcome != tc.want {
				t.Fatalf("outcome = %s, want %s (reason %q)", got.Outcome, tc.want, got.Reason)
			}

			if got.BotDetected != tc.bot {
				t.Errorf("botDetected = %v, want %v", got.BotDetected, tc.bot)
			}

			for _, s := range tc.reason {
				if !strings.Contains(got.Reason, s) {
					t.Errorf("reason %q does not contain %q", got.Reason, s)
				}
			}

			if tc.in.Kind == model.CMPKindNone && tc.want != model.OutcomeNotNeeded {
				checkLoaderFinding(t, got)
			}
		})
	}
}

// checkLoaderFinding asserts what every loader finding records, bot or not.
func checkLoaderFinding(t *testing.T, got model.Consent) {
	t.Helper()

	if got.Kind != model.CMPKindVendor || got.CMP != "CCM19" || got.Detection != consent.DetectionLoader {
		t.Errorf("kind/cmp/detection = %s/%s/%s, want vendor/CCM19/%s",
			got.Kind, got.CMP, got.Detection, consent.DetectionLoader)
	}

	if got.LoaderRequestID != "7.2" {
		t.Errorf("loaderRequestId = %q, want the loader's request", got.LoaderRequestID)
	}

	if strings.Contains(got.Reason, "secret") {
		t.Errorf("reason quotes the page's URL, API key included: %q", got.Reason)
	}

	if len(got.StaleHostRules) != 1 {
		t.Errorf("the handler's stale-rule finding was dropped: %v", got.StaleHostRules)
	}
}

// TestCheckLoadersRespectsHostScope keeps a site's own rule from claiming a
// loader on another site.
func TestCheckLoadersRespectsHostScope(t *testing.T) {
	t.Parallel()

	rules, err := consent.LoadRuleFiles(writeRules(t, `
version: 1
rules:
  - name: own
    hosts: ["shop.example"]
    accept:
      - click: "#ok"
    loaders: ["https://cmp.example/load.js"]
`))
	if err != nil {
		t.Fatalf("LoadRuleFiles: %v", err)
	}

	in := model.Consent{Outcome: model.OutcomeNotNeeded, Kind: model.CMPKindNone}
	reqs := []model.Request{{RequestID: "2.1", URL: "https://cmp.example/load.js"}}

	if got := rules.CheckLoaders(in, "other.example", "other.example", reqs); got.Outcome != model.OutcomeNotNeeded {
		t.Errorf("a rule scoped to shop.example matched on other.example: %s", got.Reason)
	}

	if got := rules.CheckLoaders(in, "shop.example", "shop.example", reqs); got.Outcome != model.OutcomeFailed {
		t.Errorf("the rule did not match on its own host: %s", got.Outcome)
	}
}
