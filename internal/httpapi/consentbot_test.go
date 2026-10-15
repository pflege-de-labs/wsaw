package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

func blockedAsBot(r *model.Result) {
	r.Consent = model.Consent{
		Outcome:         model.OutcomeFailed,
		Reason:          "CCM19 disabled itself for this browser",
		Kind:            model.CMPKindVendor,
		CMP:             "CCM19",
		LoaderRequestID: "7.2",
		BotDetected:     true,
	}
}

const botMarker = "blocked as bot"

// Story 2.11, AC5: a CMP that never ran for wsaw is marked on the tile, the
// scan page and the shared page, so nobody has to open the request list to
// learn that the missing banner was never shown to the scanner.
func TestABotBlockedCMPIsMarkedWhereverConsentIsRead(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now().Add(-time.Hour), blockedAsBot)

	for _, path := range []string{"/", "/results/site/reject/scan-1"} {
		if html := body(t, f.get(path, "Accept", "text/html")); !strings.Contains(html, botMarker) {
			t.Errorf("%s does not mark the scan as blocked as bot", path)
		}
	}

	signer := shareSigner(t)
	shared := newFixture(t, httpapi.Options{
		WebUI:        true,
		Token:        secret.Literal("s3cret"),
		Share:        signer,
		ShareBaseURL: "https://wsaw.example.com",
	}, nil)
	shared.seed("scan-1", model.ConsentReject, time.Now(), blockedAsBot)

	if html := body(t, shared.get(linkFor(t, signer, "site", "reject", "scan-1"))); !strings.Contains(html, botMarker) {
		t.Error("the shared page does not mark the scan as blocked as bot")
	}
}

func TestAnOrdinaryFailureIsNotMarkedAsBot(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now().Add(-time.Hour), func(r *model.Result) {
		r.Consent = model.Consent{Outcome: model.OutcomeFailed, Reason: "rules matched but none completed"}
	})

	for _, path := range []string{"/", "/results/site/reject/scan-1"} {
		if html := body(t, f.get(path, "Accept", "text/html")); strings.Contains(html, botMarker) {
			t.Errorf("%s marks an ordinary consent failure as blocked as bot", path)
		}
	}
}
