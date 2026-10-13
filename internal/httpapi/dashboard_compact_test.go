package httpapi_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.38: every consent mode keeps its own tile, whatever its last scan
// found, so one mode's finding never sits behind another mode's tile.

// AC1: identical "none" and "reject" scans are still two tiles, each with its
// own link.
func TestDashboardNeverMergesAgreeingModes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	// seed() produces the same request set every time, so these agree on
	// termination, outcome and every count.
	f.seed("scan-none", model.ConsentNone, time.Now(), nil)
	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{
		`href="/results/site/none/scan-none"`,
		`href="/results/site/reject/scan-reject"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("a mode lost its own tile; looked for %s", want)
		}
	}

	if strings.Contains(html, "none / reject") {
		t.Error(`"none" and "reject" were merged into one tile`)
	}
}

// AC1: two never-scanned series are two tiles too.
func TestDashboardNeverMergesNeverScannedModes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{`data-modes="none"`, `data-modes="reject"`} {
		if !strings.Contains(html, want) {
			t.Errorf("a never-scanned mode has no tile of its own; looked for %s", want)
		}
	}
}

// AC1: scans that agree on every figure the old fold compared, but not on
// severity, each show their own severity.
func TestDashboardShowsEachModesOwnSeverity(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	baseline := f.seed("scan-baseline", model.ConsentReject, time.Now().Add(-time.Hour), func(r *model.Result) {
		r.Requests = r.Requests[:1]
	})

	if _, err := f.store.SetBaseline("site", model.ConsentReject, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	// Same requests in both modes; only "reject" has a baseline the tracker
	// is new against, which makes its change critical.
	f.seed("scan-none", model.ConsentNone, time.Now(), nil)
	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if got := tileSeverity(t, html, model.ConsentReject); got != "critical" {
		t.Errorf("the reject tile shows severity %q, want its own critical", got)
	}

	if got := tileSeverity(t, html, model.ConsentNone); got == "critical" {
		t.Error("the none tile shows reject's critical severity")
	}
}

// tileSeverity reads the data-severity of the board tile for mode.
func tileSeverity(t *testing.T, html string, mode model.ConsentMode) string {
	t.Helper()

	re := regexp.MustCompile(`data-severity="([^"]*)"\s+data-outcome="[^"]*"\s+data-modes="` + string(mode) + `"`)

	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no %s tile on the board", mode)
	}

	return m[1]
}

// Story 5.8, AC1: the list must show whether the last scan found anything,
// by severity, without the reader opening it.
func TestDashboardFlagsCriticalFindingsAgainstBaseline(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	baseline := f.seed("scan-baseline", model.ConsentReject, time.Now().Add(-time.Hour), func(r *model.Result) {
		// A clean baseline: no third party at all survived rejection.
		r.Requests = r.Requests[:1]
	})

	if _, err := f.store.SetBaseline("site", model.ConsentReject, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	// The next scan finds a tracker that got through despite rejection —
	// wsaw's strongest signal, always rated critical (rules.go,
	// forHostAdded).
	f.seed("scan-latest", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `sev-critical`) || !strings.Contains(html, `critical`) {
		t.Error("a tracker surviving rejection versus baseline was not flagged critical on the target list")
	}
}

// A target with nothing notable to report must not be flagged as one just
// because a comparison was possible — the badge itself is always on the
// rail (design_handoff_target_tile, 4a), but it must read as quiet rather
// than as a finding.
func TestDashboardStaysQuietWhenNothingIsWrong(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	baseline := f.seed("scan-baseline", model.ConsentAccept, time.Now().Add(-time.Hour), nil)

	if _, err := f.store.SetBaseline("site", model.ConsentAccept, baseline.ScanID, "test", ""); err != nil {
		t.Fatal(err)
	}

	// Same requests as the baseline: no changes at all.
	f.seed("scan-latest", model.ConsentAccept, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, `sev-critical`) || strings.Contains(html, `sev-high`) {
		t.Error("an unchanged scan against its baseline was flagged as a finding")
	}
}

// The rail's severity badge is always present — info and low read as a
// quiet grey rather than disappearing outright, so a reader always sees the
// exact word instead of having to infer "no badge means clean"
// (design_handoff_target_tile, 4a).
func TestTheRailBadgeIsAlwaysShown(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `class="watch-sev sev-info"`) {
		t.Errorf("a target with nothing to compare does not carry an info badge\n%s", html)
	}
}

// The host line truncates long URLs (CSS), so the full URL has to survive
// somewhere a reader can still get at it: the title attribute
// (design_handoff_target_tile, 4a).
func TestTheRailHostCarriesTheFullURLAsATitle(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `class="watch-host" title="https://example.com/"`) {
		t.Errorf("the host line does not carry the full URL as a title\n%s", html)
	}
}
