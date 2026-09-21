package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The watchboard's own severity ranking (Story 5.30): the board marks a
// third-party host appearing in a scan where nothing was consented to, and
// leaves request counts, assets and disappearing hosts at info.

// seedCompared stores an approved baseline and the scan after it in one
// series, which is the pair the tile's severity is computed from.
func seedCompared(t *testing.T, f *fixture, mode model.ConsentMode, base, last func(*model.Result)) {
	t.Helper()

	baseline := f.seed("scan-baseline", mode, time.Now().Add(-time.Hour), base)

	if _, err := f.store.SetBaseline("site", mode, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	f.seed("scan-latest", mode, time.Now(), last)
}

// onlyFirstParty drops the fixture's third-party request, so the scan after
// it introduces tracker.test as a host that was not contacted before.
func onlyFirstParty(r *model.Result) { r.Requests = r.Requests[:1] }

// A third party the site did not contact before is the finding the board
// exists for — but only where the visitor had agreed to nothing. In accept
// mode the visitor agreed, and a new vendor there is an expected consequence
// rather than something to redden a board over.
func TestAThirdPartyHostAddedIsCriticalWhereNothingWasConsented(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode  model.ConsentMode
		marks bool
	}{
		{model.ConsentNone, true},
		{model.ConsentReject, true},
		{model.ConsentAccept, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, httpapi.Options{WebUI: true}, nil)
			seedCompared(t, f, tc.mode, onlyFirstParty, nil)

			html := body(t, f.get("/", "Accept", "text/html"))

			if got := strings.Contains(html, "has-critical"); got != tc.marks {
				t.Errorf("a third-party host added in %s mode marked the tile = %v, want %v\n%s",
					tc.mode, got, tc.marks, html)
			}
		})
	}
}

// The consent phase does not override the mode on the tile: a tracker
// contacted before the banner was accepted still ranks info, and the
// pre-consent fact stays on the tile as its own marking, where a reader
// looks for it.
func TestAPreConsentHostInAcceptModeStaysMarkedPreWithoutMarkingTheTile(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	seedCompared(t, f, model.ConsentAccept, onlyFirstParty, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, "has-critical") {
		t.Errorf("a pre-consent host in accept mode marked the tile\n%s", html)
	}

	if !strings.Contains(html, `class="watch-scan sev-info is-pre"`) {
		t.Errorf("the pre-consent marking was lost with the severity\n%s", html)
	}
}

// Request counts and assets are the churn every live site produces. The tile
// states the request delta as a number; it must not also colour for it.
func TestAssetChurnLeavesTheTileAtInfo(t *testing.T) {
	t.Parallel()

	script := model.Request{
		URL: "https://tracker.test/t.js", NormalizedURL: "https://tracker.test/t.js", Method: "GET",
		ResourceType: "script", Host: "tracker.test", Domain: "tracker.test",
		Party: model.ThirdParty, Phase: model.PhasePre, Status: 200, BodySHA256: "aaa",
	}

	withScript := func(digest string) func(*model.Result) {
		return func(r *model.Result) {
			s := script
			s.BodySHA256 = digest
			r.Requests = append(r.Requests, s)
		}
	}

	for _, tc := range []struct {
		name       string
		base, last func(*model.Result)
	}{
		// A new third-party script: asset-added, which the diff engine ranks
		// high.
		{"asset added", nil, withScript("aaa")},
		{"asset removed", withScript("aaa"), nil},
		// The same URL serving different bytes: script-changed, high in the
		// engine.
		{"script changed", withScript("aaa"), withScript("bbb")},
		// A third party the site stopped contacting.
		{"host removed", nil, onlyFirstParty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, httpapi.Options{WebUI: true}, nil)
			// Reject mode, the strictest one: if anything re-ranks asset
			// churn upwards it happens here.
			seedCompared(t, f, model.ConsentReject, tc.base, tc.last)

			html := body(t, f.get("/", "Accept", "text/html"))

			if strings.Contains(html, "has-critical") {
				t.Errorf("asset churn marked the tile\n%s", html)
			}

			if !strings.Contains(html, `data-severity="info"`) {
				t.Errorf("asset churn did not rank info on the board\n%s", html)
			}

			// And the headline it colours: info is the quiet tone, so
			// "+1 req" reads as quietly as "no change" does.
			if !strings.Contains(html, `class="watch-scan sev-info`) {
				t.Errorf("asset churn coloured the headline as a finding\n%s", html)
			}
		})
	}
}

// The board's ranking is the board's. A machine reading severity off the API
// still gets the diff engine's own value for the same comparison (Tenet 16),
// which for a third-party script appearing is high and for a tracker
// contacted before consent is high as well.
func TestTheJSONAPIKeepsTheDiffEngineSeverity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		mode       model.ConsentMode
		base, last func(*model.Result)
	}{
		{"third-party script added", model.ConsentReject, nil, func(r *model.Result) {
			r.Requests = append(r.Requests, model.Request{
				URL: "https://tracker.test/t.js", NormalizedURL: "https://tracker.test/t.js", Method: "GET",
				ResourceType: "script", Host: "tracker.test", Domain: "tracker.test",
				Party: model.ThirdParty, Phase: model.PhasePre, Status: 200,
			})
		}},
		{"pre-consent host in accept mode", model.ConsentAccept, onlyFirstParty, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, httpapi.Options{WebUI: true}, nil)
			seedCompared(t, f, tc.mode, tc.base, tc.last)

			json := body(t, f.get("/api/v1/targets"))
			if !strings.Contains(json, `"severity": "high"`) {
				t.Errorf("the JSON API lost the diff engine's severity to the board's ranking\n%s", json)
			}

			html := body(t, f.get("/", "Accept", "text/html"))
			if !strings.Contains(html, `data-severity="info"`) {
				t.Errorf("the board did not rank the same scan info\n%s", html)
			}
		})
	}
}

// "Hosts only" narrows which changes are ranked; it does not change how one
// that survives it is ranked. An accept-mode host addition is the same info
// either way.
func TestHostsOnlyLeavesAnAcceptModeHostAdditionUnmarked(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	seedCompared(t, f, model.ConsentAccept, onlyFirstParty, nil)

	html := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_hostsonly=1"))

	if strings.Contains(html, "has-critical") {
		t.Errorf("hosts-only marked a tile for a host the visitor consented to\n%s", html)
	}
}
