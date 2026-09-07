package httpapi_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.15: the dashboard already describes each series' last scan. These
// tests are about being able to reach the scan it describes.

func TestDashboardLinksToTheLastScan(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	// Two scans, so "the last one" is a real choice rather than the only row.
	base := time.Now().Add(-3 * time.Hour)
	f.seed("scan-older", model.ConsentReject, base, nil)
	f.seed("scan-newest", model.ConsentReject, base.Add(time.Hour), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	want := `href="/results/site/reject/scan-newest"`
	if !strings.Contains(html, want) {
		t.Errorf("the dashboard does not link the last scan; looked for %s", want)
	}

	// AC2: the specific scan, never "latest". A row that says "3h ago" and
	// opens whatever is newest has misrepresented what the reader clicked.
	if strings.Contains(html, `href="/results/site/reject/latest"`) {
		t.Error(`the dashboard links to "latest" instead of to the scan the row describes`)
	}

	// And not to the older one, which is not what the row is about.
	if strings.Contains(html, `href="/results/site/reject/scan-older"`) {
		t.Error("the dashboard links a scan that is not the series' last")
	}
}

// AC5: the accessible name says where the link goes, so it is usable without
// the surrounding table for context.
func TestTheLastScanLinkSaysWhereItGoes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	label := findAttr(t, html, `href="/results/site/reject/scan-1"`, "aria-label")

	for _, want := range []string{"site", "reject", "scan-1"} {
		if !strings.Contains(label, want) {
			t.Errorf("the link's accessible name %q does not mention %q", label, want)
		}
	}

	if strings.Contains(strings.ToLower(label), "click here") {
		t.Errorf("the link's accessible name is not descriptive: %q", label)
	}

	// AC5: the icon is decoration. The cell's own text carries the link, so
	// the arrow must be hidden from assistive technology rather than read out.
	if !strings.Contains(html, `class="open-icon" aria-hidden="true"`) {
		t.Error("the link icon is not marked as decorative")
	}
}

// AC3: a series that has never been scanned has nothing to link to, and the
// explicit empty state must stay explicit (Tenet 5).
func TestNeverScannedSeriesHasNoLink(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "never scanned") {
		t.Error("the never-scanned state disappeared from the dashboard")
	}

	if strings.Contains(html, `href="/results/site/`) {
		t.Error("a series with no scans produced a result link")
	}
}

// AC4: a failed or skipped scan is still linked. That is the row a reader
// most needs to open, and linking only the healthy ones hides the failures.
func TestFailedAndSkippedScansAreStillLinked(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		mode   model.ConsentMode
		scanID string
		mutate func(*model.Result)
	}{
		"failed": {
			mode:   model.ConsentReject,
			scanID: "scan-failed",
			mutate: func(r *model.Result) {
				r.Termination = model.TermError
				r.Error = "navigate failed"
			},
		},
		"skipped": {
			mode:   model.ConsentAccept,
			scanID: "scan-skipped",
			mutate: func(r *model.Result) {
				r.Termination = model.TermSkipped
				r.Error = "disallowed by robots.txt"
			},
		},
	}

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	for _, tc := range cases {
		f.seed(tc.scanID, tc.mode, time.Now(), tc.mutate)
	}

	html := body(t, f.get("/", "Accept", "text/html"))

	for name, tc := range cases {
		want := fmt.Sprintf(`href="/results/site/%s/%s"`, tc.mode, tc.scanID)
		if !strings.Contains(html, want) {
			t.Errorf("a %s scan is not linked; looked for %s", name, want)
		}
	}
}

// AC6: a series with a scan in flight still links to the last finished scan,
// and the running scan itself links nowhere — there is no result yet.
func TestARunningScanLinksNowhereButTheFinishedOneStillDoes(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{WebUI: true}, nil, oneRunning)
	f.seed("scan-finished", model.ConsentReject, time.Now().Add(-time.Hour), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `href="/results/site/reject/scan-finished"`) {
		t.Error("a series with a scan in flight lost the link to its last finished scan")
	}

	// The in-flight scan has no result, so it must not be offered as one.
	if strings.Contains(html, `href="/results/site/reject/scan-live"`) {
		t.Error("a running scan was linked as though it had a result")
	}

	if !strings.Contains(html, "no result to open yet") {
		t.Error("the running scan does not say why it cannot be opened")
	}
}

// AC7: a plain anchor, so it works with JavaScript off and can be opened in a
// new tab, bookmarked and copied like any other link.
func TestTheLastScanLinkIsAPlainAnchor(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	anchor := findElement(t, html, `href="/results/site/reject/scan-1"`)

	for _, forbidden := range []string{"onclick", "javascript:", "data-", "href=\"#\""} {
		if strings.Contains(anchor, forbidden) {
			t.Errorf("the link depends on script (%q): %s", forbidden, anchor)
		}
	}

	// And it really is reachable: the href is a working route, not a
	// plausible-looking string.
	if resp := f.get("/results/site/reject/scan-1", "Accept", "text/html"); resp.StatusCode != 200 {
		t.Errorf("the linked result page returned %d", resp.StatusCode)
	}
}

// The running scans a series reports must not leak into another series' link
// either, which is the same scoping mistake in a different place.
func TestLinksAreScopedToTheirSeries(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, `href="/results/site/accept/scan-reject"`) ||
		strings.Contains(html, `href="/results/site/none/scan-reject"`) {
		t.Error("a scan is linked under a consent mode it does not belong to")
	}
}

// findElement returns the whole HTML element containing needle, so a test can
// assert about the element rather than about the page.
func findElement(t *testing.T, html, needle string) string {
	t.Helper()

	i := strings.Index(html, needle)
	if i < 0 {
		t.Fatalf("%s is not in the rendered page", needle)
	}

	start := strings.LastIndex(html[:i], "<")
	if start < 0 {
		t.Fatalf("no element start before %s", needle)
	}

	end := strings.Index(html[i:], ">")
	if end < 0 {
		t.Fatalf("no element end after %s", needle)
	}

	return html[start : i+end+1]
}

// findAttr reads one attribute off the element containing needle.
func findAttr(t *testing.T, html, needle, attr string) string {
	t.Helper()

	el := findElement(t, html, needle)

	i := strings.Index(el, attr+`="`)
	if i < 0 {
		t.Fatalf("the element has no %s attribute: %s", attr, el)
	}

	rest := el[i+len(attr)+2:]

	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated %s attribute: %s", attr, el)
	}

	return rest[:j]
}
