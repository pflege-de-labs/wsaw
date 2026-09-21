package httpapi_test

// The watchboard leads with what the last scan changed, not with what it
// loaded (Story 5.28). These assert on the rendered board; the label logic
// itself is covered directly in watchboard_delta_internal_test.go.

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// withBaseline seeds a one-request baseline and a two-request latest scan,
// which is the smallest pair that deviates in every figure the tile prints.
func withBaseline(t *testing.T, mode model.ConsentMode) *fixture {
	t.Helper()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	baseline := f.seed("scan-baseline", mode, time.Now().Add(-time.Hour), func(r *model.Result) {
		r.Requests = r.Requests[:1]
	})

	if _, err := f.store.SetBaseline("site", mode, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	f.seed("scan-latest", mode, time.Now(), nil)

	return f
}

// boardText is the rendered board with its entities resolved: html/template
// escapes a delta's leading "+" as &#43;, which a browser draws as a plus and
// a strings.Contains does not match.
func boardText(t *testing.T, f *fixture, headers ...string) string {
	t.Helper()

	return html.UnescapeString(body(t, f.get("/", append([]string{"Accept", "text/html"}, headers...)...)))
}

// The headline is the deviation, signed, with the absolutes gone from the
// visible line.
func TestTheTileLeadsWithTheDeviationFromTheBaseline(t *testing.T) {
	t.Parallel()

	page := boardText(t, withBaseline(t, model.ConsentReject))

	// One request and one third-party host were added against the baseline,
	// and that host was contacted before consent.
	if !strings.Contains(page, "+1 req · +1 3p · +1 pre") {
		t.Errorf("the tile does not lead with the deviation\n%s", page)
	}

	if !strings.Contains(page, `<span class="watch-scan-base">vs baseline</span>`) {
		t.Errorf("the tile does not say what it compared against\n%s", page)
	}
}

// "No change against the baseline I approved" and "no change against a scan
// nobody has looked at" are different claims, so the tile names its base
// rather than rendering the two identically (Tenet 5).
func TestTheTileNamesThePreviousScanWhenThereIsNoBaseline(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	f.seed("scan-earlier", model.ConsentReject, time.Now().Add(-time.Hour), func(r *model.Result) {
		r.Requests = r.Requests[:1]
	})
	f.seed("scan-latest", model.ConsentReject, time.Now(), nil)

	page := boardText(t, f)

	if !strings.Contains(page, `<span class="watch-scan-base">vs last scan</span>`) {
		t.Errorf("a fallback comparison is rendered as though it were a baseline\n%s", page)
	}

	// The same counts moved, but the marking still requires a real baseline
	// (Story 5.23, AC7 — unchanged by this story).
	if strings.Contains(page, "is-deviant") {
		t.Error("the previous-scan fallback marked the headline as deviating from a baseline")
	}
}

// A scan with nothing to compare against says so and prints its absolutes:
// with no base, those counts are the only figures that are true.
func TestAFirstScanPrintsItsAbsolutes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	f.seed("scan-only", model.ConsentReject, time.Now(), nil)

	page := boardText(t, f)

	if !strings.Contains(page, "2 req · 1 3p · 1 pre") {
		t.Errorf("a first scan does not print its absolute figures\n%s", page)
	}

	if !strings.Contains(page, `<span class="watch-scan-base">first scan</span>`) {
		t.Errorf("a first scan does not say why it has no deviation\n%s", page)
	}

	if strings.Contains(page, "no change") {
		t.Error("a scan with nothing to compare against rendered as unchanged")
	}
}

// A scan matching its baseline is the quiet state, stated rather than left as
// an empty line, and muted so the board stays silent until something needs a
// reader.
func TestAnUnchangedScanReadsAsQuiet(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	baseline := f.seed("scan-baseline", model.ConsentAccept, time.Now().Add(-time.Hour), nil)

	if _, err := f.store.SetBaseline("site", model.ConsentAccept, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	f.seed("scan-latest", model.ConsentAccept, time.Now(), nil)

	page := boardText(t, f)

	if !strings.Contains(page, "no change") {
		t.Errorf("an unchanged scan does not say so\n%s", page)
	}

	if !strings.Contains(page, "is-quiet") {
		t.Errorf("the quiet state is not muted\n%s", page)
	}
}

// The absolutes are one hover or one screen reader away, for both scans.
func TestTheHeadlineCarriesBothScansFiguresInItsAccessibleName(t *testing.T) {
	t.Parallel()

	page := boardText(t, withBaseline(t, model.ConsentReject))

	for _, want := range []string{
		"this scan 2 requests, 1 third-party domains, 1 pre-consent",
		"baseline scan-baseline 1 requests, 0 third-party domains, 0 pre-consent",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the accessible name is missing %q\n%s", want, page)
		}
	}
}

// The "hosts only" toggle narrows severity, not counts: a figure that means
// two different things depending on a cookie is not a figure.
func TestHostsOnlyDoesNotNarrowTheDeviation(t *testing.T) {
	t.Parallel()

	full := boardText(t, withBaseline(t, model.ConsentReject))
	narrowed := boardText(t, withBaseline(t, model.ConsentReject), "Cookie", "wsaw_hostsonly=1")

	const want = "+1 req · +1 3p · +1 pre"

	if !strings.Contains(full, want) || !strings.Contains(narrowed, want) {
		t.Error("the deviation figures differ with the hosts-only toggle set")
	}
}

// The headline is server-rendered text in a plain anchor: nothing about it
// needs script, and no inline style attribute reaches a page whose CSP
// forbids one (Story 5.23, AC11).
func TestTheHeadlineNeedsNoScriptAndNoInlineStyle(t *testing.T) {
	t.Parallel()

	page := boardText(t, withBaseline(t, model.ConsentReject))

	if strings.Contains(page, "style=") {
		t.Error("the board rendered an inline style attribute")
	}

	if !strings.Contains(page, `href="/results/site/reject/scan-latest"`) {
		t.Errorf("the headline is not the link to the scan it describes\n%s", page)
	}
}

// The board's figures come from the comparison the severity pass already
// made — the same one the API now reports — rather than from a second read of
// the same two documents (Story 5.28, AC6).
func TestTheAPIReportsWhatTheLastScanWasComparedAgainst(t *testing.T) {
	t.Parallel()

	f := withBaseline(t, model.ConsentReject)

	resp := f.get("/api/v1/targets", "Accept", "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/targets = %d", resp.StatusCode)
	}

	var payload struct {
		Targets []struct {
			Series []struct {
				Mode       model.ConsentMode       `json:"consentMode"`
				LastScan   *struct{ Requests int } `json:"lastScan"`
				ComparedTo *struct {
					Base       string `json:"base"`
					ScanID     string `json:"scanId"`
					Comparable bool   `json:"comparable"`
					Requests   int    `json:"requests"`
				} `json:"comparedTo"`
			} `json:"series"`
		} `json:"targets"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &payload); err != nil {
		t.Fatal(err)
	}

	var found bool

	for _, target := range payload.Targets {
		for _, series := range target.Series {
			if series.Mode != model.ConsentReject {
				continue
			}

			found = true

			if series.ComparedTo == nil {
				t.Fatal("the API does not say what the last scan was compared against")
			}

			if series.ComparedTo.Base != "baseline" || series.ComparedTo.ScanID != "scan-baseline" {
				t.Errorf("comparedTo = %+v, want the approved baseline", series.ComparedTo)
			}

			if !series.ComparedTo.Comparable || series.ComparedTo.Requests != 1 {
				t.Errorf("comparedTo = %+v, want a comparable base of one request", series.ComparedTo)
			}

			// The absolutes stay absolute for a client that wants to
			// subtract them itself (Tenet 16).
			if series.LastScan == nil || series.LastScan.Requests != 2 {
				t.Errorf("lastScan = %+v, want the scan's own two requests", series.LastScan)
			}
		}
	}

	if !found {
		t.Fatal("the API reported no reject series")
	}
}
