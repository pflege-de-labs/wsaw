package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The target list is compact because "none" and "reject" usually produce the
// same result, and it flags a target whose last scan found anything
// meaningful against its baseline. Neither is written into a story yet: both
// were added on explicit request in the session that introduced them
// (AGENTS.md §1's flag-it-if-not-in-a-story rule).

// A reader should only have to look at "none" and "reject" as one line when
// they genuinely agree.
func TestDashboardFoldsNoneAndRejectWhenTheyAgree(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	// seed() produces the same request set every time, so two default scans
	// agree on termination, outcome and every count.
	f.seed("scan-none", model.ConsentNone, time.Now(), nil)
	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)
	f.seed("scan-accept", model.ConsentAccept, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "none / reject") {
		t.Error(`identical "none" and "reject" scans were not folded into one row`)
	}

	// Folding must not swallow either link: the reader still needs a way to
	// open (or rescan) the mode the row represents.
	if !strings.Contains(html, `href="/results/site/none/scan-none"`) &&
		!strings.Contains(html, `href="/results/site/reject/scan-reject"`) {
		t.Error("the folded row lost its link to the underlying scan")
	}
}

// A disagreement between "did nothing" and "explicitly rejected" is itself a
// finding, so it must not be hidden behind the fold.
func TestDashboardKeepsNoneAndRejectSeparateWhenTheyDisagree(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	f.seed("scan-none", model.ConsentNone, time.Now(), nil)
	f.seed("scan-reject", model.ConsentReject, time.Now(), func(r *model.Result) {
		// Reject actually blocked the tracker that "none" let through.
		r.Requests = r.Requests[:1]
	})

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, "none / reject") {
		t.Error(`"none" and "reject" were folded despite disagreeing`)
	}

	for _, want := range []string{
		`href="/results/site/none/scan-none"`,
		`href="/results/site/reject/scan-reject"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("a disagreeing series lost its own row/link; looked for %s", want)
		}
	}
}

// Two never-scanned series (e.g. "accept" configured but never run) count as
// agreeing, so an all-empty target still gets the compact treatment.
func TestDashboardFoldsNeverScannedNoneAndReject(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "none / reject") {
		t.Error(`two never-scanned series were not folded`)
	}
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

// A target with nothing notable to report must not carry a badge just
// because a comparison was possible — the list should flag what needs a
// look, not restate "clean" on every row.
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
