package report_test

// Story 2.9: what the Markdown report says about a site that wrote its own
// banner. The result document carries the facts; this is the layer where a
// reviewer actually reads them, and the two must not disagree (Tenet 16).

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

func markdown(t *testing.T, res *model.Result) string {
	t.Helper()

	var b strings.Builder

	if err := report.WriteMarkdown(&b, res, nil); err != nil {
		t.Fatal(err)
	}

	return b.String()
}

func TestBespokeBannerIsNotReportedAsNoCMP(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Consent = model.Consent{
		Outcome:       model.OutcomeApplied,
		Reason:        `rule "generic-label-match" applied and verified`,
		CMP:           "unknown (heuristic match)",
		Detection:     "rule:generic-label-match",
		Mechanism:     "heuristic",
		Heuristic:     true,
		Kind:          model.CMPKindBespoke,
		BannerHeading: "Wir verwenden Cookies",
		StorageKeys:   []string{"local:cookie-accepted"},
	}

	out := markdown(t, res)

	if !strings.Contains(out, "no vendor identified") {
		t.Error("the report does not say that no vendor was identified")
	}

	if strings.Contains(out, "none detected") {
		t.Error("a page with its own banner is reported as having no CMP")
	}

	if !strings.Contains(out, "Wir verwenden Cookies") {
		t.Error("the report does not name the banner it handled")
	}

	if !strings.Contains(out, "local:cookie-accepted") {
		t.Error("the report does not say where the choice was recorded")
	}
}

func TestPageWithNoBannerStillReportsNoCMP(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Consent = model.Consent{
		Outcome: model.OutcomeNotNeeded,
		Reason:  "no consent management platform detected",
		Kind:    model.CMPKindNone,
	}

	if out := markdown(t, res); !strings.Contains(out, "none detected") {
		t.Error("a page with no banner no longer reports that plainly")
	}
}

func TestUnhandledBannerReportsWhatItFound(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Consent = model.Consent{
		Outcome: model.OutcomeFailed,
		Reason:  "a consent banner is present but no rule matched it",
		Kind:    model.CMPKindBespoke,
		Diagnostic: &model.ConsentDiagnostic{
			Element:  "div#cookie-banner",
			Text:     "Diese Website nutzt Cookies",
			Controls: []string{"Ablehnen", "Akzeptieren"},
		},
		StaleHostRules: []string{"example-banner"},
	}

	out := markdown(t, res)

	for _, want := range []string{"div#cookie-banner", "Ablehnen", "Akzeptieren", "example-banner"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q, which is what a rule author needs", want)
		}
	}
}

func TestPreConsentFirstPartyHostsAreNamed(t *testing.T) {
	t.Parallel()

	// The fixture's own document request is first-party and pre-consent, so
	// the section has something to say; a proxied collector would appear here
	// the same way.
	res := fixture()
	res.Requests = append(res.Requests, model.Request{
		URL: "https://hog.example.com/e", NormalizedURL: "https://hog.example.com/e",
		Method: "GET", ResourceType: "xhr", Host: "hog.example.com", Domain: "example.com",
		Party: model.FirstParty, Phase: model.PhasePre, Status: 200,
	})

	out := markdown(t, res)

	if !strings.Contains(out, "First-party hosts contacted before any consent interaction") {
		t.Fatal("the report has no first-party pre-consent section")
	}

	if !strings.Contains(out, "hog.example.com") {
		t.Error("the report does not name the first-party host contacted before the interaction")
	}
}

func TestStorageTableKeepsValuesOut(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Storage = []model.StorageEntry{{
		Origin: "https://example.com", Area: model.StorageLocal, Key: "cookie-accepted",
		Party: model.FirstParty, ValueSHA256: "abc123", ValueLength: 5,
	}}

	out := markdown(t, res)

	if !strings.Contains(out, "## Web storage") || !strings.Contains(out, "cookie-accepted") {
		t.Fatal("the report does not list web storage")
	}

	if !strings.Contains(out, "only a digest and length are kept") {
		t.Error("the report does not say that storage values are not stored")
	}
}
