package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 5.20: the baseline is what every change on every other scan is
// measured against, so a reader must be able to see which scan it is and open
// it, rather than matching hex digits by eye between a panel and a table.

// seedSeries stores n scans, newest last, and returns their IDs in the order
// they were stored.
func seedSeries(t *testing.T, f *fixture, mode model.ConsentMode, n int) []string {
	t.Helper()

	base := time.Now().Add(-time.Duration(n) * time.Hour)
	ids := make([]string, 0, n)

	for i := range n {
		id := "scan-" + string(rune('a'+i%26)) + strings.Repeat("0", 2) + itoa(i)
		f.seed(id, mode, base.Add(time.Duration(i)*time.Hour), nil)
		ids = append(ids, id)
	}

	return ids
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var out []byte

	for ; n > 0; n /= 10 {
		out = append([]byte{byte('0' + n%10)}, out...)
	}

	return string(out)
}

func approve(t *testing.T, f *fixture, mode model.ConsentMode, scanID string) *store.Baseline {
	t.Helper()

	b, err := f.store.SetBaseline("site", mode, scanID, "tester", "")
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// AC1, AC2 and AC3: the panel links to the approved scan, the row that is the
// baseline says so in words, and it keeps the link every other row has.
func TestTheBaselineIsMarkedInTheHistoryAndLinkedFromThePanel(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	ids := seedSeries(t, f, model.ConsentReject, 3)
	approve(t, f, model.ConsentReject, ids[0])

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	link := "/results/site/reject/" + ids[0]

	if !strings.Contains(html, `<a href="`+link+`"><code>`+ids[0]) {
		t.Error("the panel does not link the approved scan to its result page")
	}

	if !strings.Contains(html, "badge-baseline") || !strings.Contains(html, ">baseline</span>") {
		t.Error("no row is marked as the baseline")
	}

	// The marked row keeps its own link: marking must not cost it the
	// affordance every other row has.
	if strings.Count(html, link) < 2 {
		t.Error("the marked row no longer links to its own result")
	}
}

// AC4: approving the baseline again would record an approval for a change
// that did not happen.
func TestTheBaselineRowDoesNotOfferApproval(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	ids := seedSeries(t, f, model.ConsentReject, 2)
	approve(t, f, model.ConsentReject, ids[1])

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, "current baseline") {
		t.Error("the baseline row does not say that it is the baseline")
	}

	// One row is the baseline, the other is not, so exactly one approve form
	// remains.
	if got := strings.Count(html, `name="scanId"`); got != 1 {
		t.Errorf("approve forms = %d, want 1: the baseline row must not offer approval", got)
	}

	if strings.Contains(html, `value="`+ids[1]+`"`) {
		t.Error("the baseline row still carries an approve form for itself")
	}
}

// AC5: the history is capped, so an approved baseline can be real, readable
// and simply older than the window. An unmarked table must not read as "none
// of these is the baseline".
func TestABaselineOlderThanTheHistorySaysSo(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	// One more than the page's limit, so the oldest falls outside it.
	ids := seedSeries(t, f, model.ConsentReject, 101)
	approve(t, f, model.ConsentReject, ids[0])

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if strings.Contains(html, "badge-baseline") {
		t.Fatal("a row was marked as the baseline, but the baseline is outside the listed window")
	}

	if !strings.Contains(html, "older than the scans listed below") {
		t.Error("the page does not say that the baseline is not among the rows below")
	}

	// It is absent from the table, not gone: it still opens.
	if !strings.Contains(html, `<a href="/results/site/reject/`+ids[0]+`"><code>`) {
		t.Error("the panel does not link a baseline that is merely older than the listed scans")
	}
}

// AC6: a baseline outlives the result it was approved from, because it holds
// its own copy. The link must not point into a 404.
func TestABaselineWhoseResultWasPrunedIsNotLinked(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	ids := seedSeries(t, f, model.ConsentReject, 2)
	approve(t, f, model.ConsentReject, ids[0])

	// Retention keeps the newest scan only; the baseline's own result goes.
	if _, err := f.store.Prune(time.Now(), store.Retention{MaxPerSeries: 1}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.GetBaseline("site", model.ConsentReject); err != nil {
		t.Fatalf("the baseline did not survive pruning: %v", err)
	}

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if strings.Contains(html, `href="/results/site/reject/`+ids[0]+`"`) {
		t.Error("the page links a result that pruning removed")
	}

	if !strings.Contains(html, "pruned by retention") {
		t.Error("the page does not say why the baseline cannot be opened")
	}

	if !strings.Contains(html, "holds its own copy") {
		t.Error("the page does not say that the baseline itself survives")
	}
}

// AC7: a series with no approved baseline keeps saying so. This story does
// not touch that panel.
func TestASeriesWithoutABaselineIsUnchanged(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	seedSeries(t, f, model.ConsentReject, 2)

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, "No approved baseline") {
		t.Error("the page no longer says that there is no approved baseline")
	}

	if strings.Contains(html, "badge-baseline") {
		t.Error("a row was marked as the baseline when none is approved")
	}
}

// AC8: identifying the baseline is a read. A read-only deployment sees the
// marking and the link; only the approve control is withheld.
func TestAReadOnlyDeploymentStillSeesTheBaseline(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, ReadOnly: true}, nil)

	ids := seedSeries(t, f, model.ConsentReject, 2)
	approve(t, f, model.ConsentReject, ids[0])

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, "badge-baseline") {
		t.Error("the baseline row is not marked on a read-only deployment")
	}

	if !strings.Contains(html, `<a href="/results/site/reject/`+ids[0]+`"><code>`) {
		t.Error("the panel does not link the baseline on a read-only deployment")
	}

	if strings.Contains(html, `name="scanId"`) {
		t.Error("a read-only deployment offers an approve form")
	}
}
