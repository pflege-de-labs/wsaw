package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.35 renders what the document says; these fixtures say it directly
// rather than depending on the scoring rules, which have their own tests.
func withConfidence(score int, band model.ConfidenceBand, reasons ...model.ConfidenceReason) func(*model.Result) {
	return func(r *model.Result) {
		r.Confidence = &model.Confidence{Score: score, Band: band, Reasons: reasons}
	}
}

var (
	cutShort = model.ConfidenceReason{
		Signal: model.SignalTermination, Points: 30,
		Observed: "stopped at the time budget before the page went idle",
	}
	lostFetches = model.ConfidenceReason{
		Signal: model.SignalFetches, Points: 8,
		Observed: "2 of 25 network requests not observed, most often net::ERR_CONNECTION_RESET (2)",
	}
	noHistory = model.ConfidenceReason{
		Signal: model.SignalDuration, Observed: "1s", NotAssessed: "3 of 5 earlier clean scans in this series",
	}
	consentFine = model.ConfidenceReason{Signal: model.SignalConsent, Observed: "applied"}
)

// AC8: band and score in words on the tile, why in its title and accessible
// name, largest penalty first, and what could not be assessed after that.
func TestTheScanTileStatesItsConfidenceAndWhy(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now().Add(-time.Hour),
		withConfidence(62, model.ConfidenceMedium, lostFetches, cutShort, noHistory, consentFine))

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, ">confidence medium · 62<") {
		t.Fatalf("the tile does not print the band and score")
	}

	if !strings.Contains(html, `class="watch-conf conf-medium"`) {
		t.Error("the confidence line is not styled by its band")
	}

	title := findAttr(t, html, `class="watch-conf conf-medium"`, "title")
	aria := findAttr(t, html, `class="watch-conf conf-medium"`, "aria-label")

	if title != aria {
		t.Errorf("hover and screen reader are told different things:\n%q\n%q", title, aria)
	}

	termination := strings.Index(title, "−30 termination")
	fetches := strings.Index(title, "−8 fetches")
	duration := strings.Index(title, "duration not assessed (3 of 5")

	if termination < 0 || fetches < 0 || duration < 0 {
		t.Fatalf("the title does not give every reason: %q", title)
	}

	if termination >= fetches || fetches >= duration {
		t.Errorf("the reasons are not largest first, unassessed last: %q", title)
	}

	if strings.Contains(title, "consent") {
		t.Errorf("a signal that cost nothing is listed as a reason: %q", title)
	}
}

func TestHighConfidenceIsQuietOnTheTile(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), withConfidence(100, model.ConfidenceHigh, consentFine))

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `class="watch-conf conf-high"`) || !strings.Contains(html, ">confidence high · 100<") {
		t.Error("a high-confidence tile does not print its band and score")
	}

	if title := findAttr(t, html, `class="watch-conf conf-high"`, "title"); !strings.Contains(title, "every signal assessed") {
		t.Errorf("a clean scan's title = %q, want it to say every signal was assessed", title)
	}
}

// AC3 and AC10: a failed scan is no observation, and a document from before
// schema 2.1 is not computed. Neither is ever a number.
func TestAFailedOrUnscoredScanIsNotANumber(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	now := time.Now()

	f.seed("scan-failed", model.ConsentReject, now, func(r *model.Result) {
		r.Termination, r.Error = model.TermError, "navigation failed"
		withConfidence(0, model.ConfidenceNone, model.ConfidenceReason{
			Signal: model.SignalTermination, Points: 100, Observed: "error: navigation failed",
		})(r)
	})
	f.seed("scan-old", model.ConsentAccept, now, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, ">no observation<") {
		t.Error("a failed scan's tile does not read no observation")
	}

	if !strings.Contains(html, ">confidence not computed<") {
		t.Error("a pre-2.1 scan's tile does not read confidence not computed")
	}

	if strings.Contains(html, "confidence none") || strings.Contains(html, "· 0<") {
		t.Error("a failed scan is shown as a score")
	}
}

// AC9: the scan page lists every signal from the document, including the one
// that could not be assessed, and links the fetch penalty to the requests.
func TestTheScanPageExplainsTheScore(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(),
		withConfidence(62, model.ConfidenceMedium, cutShort, lostFetches, noHistory, consentFine))

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	section := html[strings.Index(html, `id="confidence"`):]
	section = section[:strings.Index(section, "</section>")]

	for _, want := range []string{
		"<strong>medium</strong> · 62 of 100",
		cutShort.Observed, "−30",
		"net::ERR_CONNECTION_RESET", "−8", `href="#requests"`,
		"not assessed: 3 of 5 earlier clean scans in this series",
		">consent<",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the confidence section does not contain %q", want)
		}
	}

	if !strings.Contains(html, `<section id="requests">`) {
		t.Error("the requests section the fetch row links to has no anchor")
	}
}

func TestTheScanPageSaysWhenNoScoreWasComputed(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "Confidence not computed") {
		t.Error("the scan page of an unscored scan does not say it was not computed")
	}
}

// AC10: the API carries the score on the summary and the reasons on the
// series, and says nothing at all for a scan that was never scored.
func TestTheTargetsAPICarriesConfidence(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(),
		withConfidence(62, model.ConfidenceMedium, cutShort, lostFetches, noHistory, consentFine))
	f.seed("scan-2", model.ConsentAccept, time.Now(), nil)

	var payload struct {
		Targets []struct {
			Series []struct {
				Mode     model.ConsentMode `json:"consentMode"`
				LastScan map[string]any    `json:"lastScan"`
				Conf     *model.Confidence `json:"confidence"`
			} `json:"series"`
		} `json:"targets"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/targets"))), &payload); err != nil || len(payload.Targets) != 1 {
		t.Fatalf("decoding /api/v1/targets: %v (%d targets)", err, len(payload.Targets))
	}

	targets := payload.Targets

	seen := 0

	for _, sv := range targets[0].Series {
		switch sv.Mode {
		case model.ConsentReject:
			seen++

			if sv.LastScan["confidenceScore"] != float64(62) || sv.LastScan["confidenceBand"] != "medium" {
				t.Errorf("reject lastScan = %v, want score 62 medium", sv.LastScan)
			}

			if sv.Conf == nil || len(sv.Conf.Reasons) != 4 {
				t.Errorf("reject confidence = %+v, want the document's four reasons", sv.Conf)
			}

		case model.ConsentAccept:
			seen++

			if _, ok := sv.LastScan["confidenceScore"]; ok || sv.Conf != nil {
				t.Errorf("an unscored scan reports a confidence: %v %+v", sv.LastScan, sv.Conf)
			}
		}
	}

	if seen != 2 {
		t.Fatalf("saw %d of the two seeded series", seen)
	}
}
