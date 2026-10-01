package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.36: a scan can be approved as the baseline from its own page, where
// the evidence the decision rests on is, and once it is the baseline its page
// no longer compares it with itself.

const (
	approveFormMarker = `name="scanId"`
	refusedMarker     = "cannot be approved as the baseline"
)

func resultPage(t *testing.T, f *fixture, scanID string) string {
	t.Helper()

	return body(t, f.get("/results/site/reject/"+scanID, "Accept", "text/html"))
}

func approveFrom(from, scanID string) url.Values {
	v := url.Values{"scanId": {scanID}, "actor": {"reviewer"}}
	if from != "" {
		v.Set("from", from)
	}

	return v
}

func failed(r *model.Result) {
	r.Termination = model.TermError
	r.Error = "boom"
}

func skipped(r *model.Result) { r.Termination = model.TermSkipped }

func truncated(r *model.Result) { r.Termination = model.TermTimeout }

// extraHost adds a third party the earlier scans did not contact, so a
// comparison against them has something to report.
func extraHost(r *model.Result) {
	r.Requests = append(r.Requests, model.Request{
		URL: "https://newcomer.test/tag.js", NormalizedURL: "https://newcomer.test/tag.js", Method: "GET",
		ResourceType: "script", Host: "newcomer.test", Domain: "newcomer.test",
		Party: model.ThirdParty, Phase: model.PhasePost, Status: 200,
	})
}

// AC1: an approvable scan offers the history row's form, marked as sent from
// the scan page.
func TestTheScanPageOffersApproval(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ids := seedSeries(t, f, model.ConsentReject, 2)

	html := resultPage(t, f, ids[1])

	for _, want := range []string{
		`action="/approve/site/reject"`,
		`name="scanId" value="` + ids[1] + `"`,
		`name="from" value="result"`,
		`name="actor"`,
		`name="note"`,
		"Approve as baseline",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the scan page's approve form lacks %s", want)
		}
	}

	if strings.Contains(html, `id="approve-truncated"`) {
		t.Error("a scan that went idle carries the stopped-early warning")
	}
}

// AC2: a scan the store would refuse says why, and offers no button that can
// only fail.
func TestTheScanPageExplainsWhyAFailedScanCannotBeApproved(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*model.Result){"error": failed, "skipped": skipped} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, httpapi.Options{WebUI: true}, nil)
			f.seed("scan-bad", model.ConsentReject, time.Now(), mutate)

			html := resultPage(t, f, "scan-bad")

			if !strings.Contains(html, refusedMarker) {
				t.Error("a failed scan does not say why it cannot be the baseline")
			}

			if strings.Contains(html, approveFormMarker) {
				t.Error("a failed scan offers an approve form the store would refuse")
			}
		})
	}
}

// AC2: a scan that stopped early can be approved, and says what that means.
func TestTheScanPageWarnsBeforeApprovingATruncatedScan(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-cut", model.ConsentReject, time.Now(), truncated)

	html := resultPage(t, f, "scan-cut")

	if !strings.Contains(html, approveFormMarker) {
		t.Error("a truncated scan, which the store accepts, offers no approve form")
	}

	if !strings.Contains(html, `id="approve-truncated"`) || !strings.Contains(html, "may be incomplete") {
		t.Error("a truncated scan's approve form does not warn that its asset list may be incomplete")
	}
}

// AC3: the baseline's own page says that it is the baseline, with who
// approved it and why, and offers no second approval.
func TestTheBaselinesOwnPageSaysItIsTheBaseline(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ids := seedSeries(t, f, model.ConsentReject, 2)

	if _, err := f.store.SetBaseline("site", model.ConsentReject, ids[0], "tester", "reviewed in the weekly"); err != nil {
		t.Fatal(err)
	}

	html := resultPage(t, f, ids[0])

	for _, want := range []string{"This scan is the current baseline", "by tester", "reviewed in the weekly"} {
		if !strings.Contains(html, want) {
			t.Errorf("the baseline's page does not say %q", want)
		}
	}

	if strings.Contains(html, approveFormMarker) {
		t.Error("the baseline's page offers to approve it again")
	}
}

// AC1, AC2 and AC3 on a read-only deployment: the baseline is still named,
// and nothing is offered or explained.
func TestAReadOnlyScanPageOffersNoApproval(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, ReadOnly: true}, nil)
	ids := seedSeries(t, f, model.ConsentReject, 2)
	f.seed("scan-bad", model.ConsentReject, time.Now(), failed)
	approve(t, f, model.ConsentReject, ids[0])

	for _, id := range []string{ids[1], "scan-bad"} {
		html := resultPage(t, f, id)

		if strings.Contains(html, approveFormMarker) {
			t.Errorf("%s: a read-only deployment offers an approve form", id)
		}

		if strings.Contains(html, refusedMarker) {
			t.Errorf("%s: a read-only deployment explains an action it never offers", id)
		}
	}

	if !strings.Contains(resultPage(t, f, ids[0]), "This scan is the current baseline") {
		t.Error("a read-only deployment does not say which scan is the baseline")
	}
}

// AC1: the history row and the scan page offer approval for exactly the same
// scans.
func TestTheScanPageAndTheHistoryAgreeOnWhatCanBeApproved(t *testing.T) {
	t.Parallel()

	for _, readOnly := range []bool{false, true} {
		f := newFixture(t, httpapi.Options{WebUI: true, ReadOnly: readOnly}, nil)
		now := time.Now()

		seeded := map[string]func(*model.Result){
			"scan-ok": nil, "scan-base": nil, "scan-error": failed,
			"scan-skipped": skipped, "scan-timeout": truncated,
		}

		i := 0
		for id, mutate := range seeded {
			f.seed(id, model.ConsentReject, now.Add(-time.Duration(i)*time.Minute), mutate)
			i++
		}

		approve(t, f, model.ConsentReject, "scan-base")

		history := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

		for id := range seeded {
			form := `name="scanId" value="` + id + `"`
			inHistory := strings.Contains(history, form)
			onPage := strings.Contains(resultPage(t, f, id), form)

			if inHistory != onPage {
				t.Errorf("read-only %v, %s: history offers approval %v, scan page %v", readOnly, id, inHistory, onPage)
			}
		}
	}
}

// AC4: an approval made from the scan page lands back on that scan, which now
// says it is the baseline; the approval is audited like any other.
func TestApprovingFromTheScanPageReturnsToIt(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ids := seedSeries(t, f, model.ConsentReject, 2)

	dest := location(t, f.postForm("/approve/site/reject", approveFrom("result", ids[1])))

	if want := "/results/site/reject/" + ids[1]; dest.Path != want {
		t.Errorf("an approval from the scan page returned to %q, want %q", dest.Path, want)
	}

	if dest.Query().Get("ok") == "" {
		t.Error("the approval carries no confirmation back to the scan page")
	}

	b, err := f.store.GetBaseline("site", model.ConsentReject)
	if err != nil || b.ScanID != ids[1] {
		t.Fatalf("baseline = %+v, %v; want %s", b, err, ids[1])
	}

	if !strings.Contains(resultPage(t, f, ids[1]), "This scan is the current baseline") {
		t.Error("after approval the scan page does not say it is the baseline")
	}

	audit := body(t, f.get("/api/v1/audit"))
	if !strings.Contains(audit, "baseline-approved") || !strings.Contains(audit, "reviewer") {
		t.Errorf("the approval was not audited: %s", audit)
	}
}

// AC4: the history page's form is unchanged and still returns to the history.
func TestApprovingFromTheHistoryStillReturnsToTheHistory(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ids := seedSeries(t, f, model.ConsentReject, 1)

	dest := location(t, f.postForm("/approve/site/reject", approveFrom("", ids[0])))

	if dest.Path != "/targets/site/reject" {
		t.Errorf("an approval from the history returned to %q, want the history page", dest.Path)
	}
}

// AC4: nothing in the request chooses the destination.
func TestTheApprovalRedirectCannotBePointedElsewhere(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ids := seedSeries(t, f, model.ConsentReject, 1)
	scanPage := "/results/site/reject/" + ids[0]

	for _, tc := range []struct {
		from, want string
	}{
		{"result", scanPage},
		{"https://evil.example/", "/targets/site/reject"},
		{"//evil.example", "/targets/site/reject"},
		{"/audit", "/targets/site/reject"},
	} {
		form := approveFrom(tc.from, ids[0])
		form.Set("return", "https://evil.example/")
		form.Set("next", "https://evil.example/")

		dest := location(t, f.postForm("/approve/site/reject", form, "Referer", "https://evil.example/"))

		if dest.Host != "" || dest.Path != tc.want {
			t.Errorf("from %q: redirected to %q, want %q", tc.from, dest.String(), tc.want)
		}
	}
}

// AC4: a refusal from the scan page returns to the scan page with its reason.
func TestARefusedApprovalReturnsToTheScanPage(t *testing.T) {
	t.Parallel()

	t.Run("failed scan", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, httpapi.Options{WebUI: true}, nil)
		f.seed("scan-bad", model.ConsentReject, time.Now(), failed)

		dest := location(t, f.postForm("/approve/site/reject", approveFrom("result", "scan-bad")))

		if dest.Path != "/results/site/reject/scan-bad" {
			t.Errorf("a refused approval returned to %q, want the scan page", dest.Path)
		}

		if !strings.Contains(dest.Query().Get("err"), "cannot be a baseline") {
			t.Errorf("the refusal's reason %q does not say why", dest.Query().Get("err"))
		}
	})

	t.Run("read-only", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, httpapi.Options{WebUI: true, ReadOnly: true}, nil)
		ids := seedSeries(t, f, model.ConsentReject, 1)

		dest := location(t, f.postForm("/approve/site/reject", approveFrom("result", ids[0])))

		if dest.Path != "/results/site/reject/"+ids[0] {
			t.Errorf("a read-only refusal returned to %q, want the scan page", dest.Path)
		}

		if !strings.Contains(dest.Query().Get("err"), "read-only") {
			t.Errorf("the refusal's reason %q does not say the deployment is read-only", dest.Query().Get("err"))
		}

		if _, err := f.store.GetBaseline("site", model.ConsentReject); err == nil {
			t.Error("a read-only deployment approved a baseline")
		}
	})
}

func diffOf(t *testing.T, f *fixture, scanID string) diff.Report {
	t.Helper()

	var rep diff.Report

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/diff/site/reject/"+scanID))), &rep); err != nil {
		t.Fatal(err)
	}

	return rep
}

// AC5: the baseline's own page, and the diff endpoint, compare it with the
// scan before it and name that scan; every other scan is still compared with
// the baseline.
func TestTheBaselineIsComparedWithThePreviousScanNotWithItself(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	now := time.Now()

	f.seed("scan-1", model.ConsentReject, now.Add(-2*time.Hour), nil)
	f.seed("scan-2", model.ConsentReject, now.Add(-time.Hour), extraHost)
	f.seed("scan-3", model.ConsentReject, now, nil)
	approve(t, f, model.ConsentReject, "scan-2")

	html := resultPage(t, f, "scan-2")

	if !strings.Contains(html, `id="compared-against"`) ||
		!strings.Contains(html, `<a href="/results/site/reject/scan-1"><code>scan-1</code></a>`) {
		t.Error("the baseline's page does not name and link the scan it is compared with")
	}

	if !strings.Contains(html, "newcomer.test") {
		t.Error("the baseline's page does not show the changes against the previous scan")
	}

	if strings.Contains(html, "No changes against the baseline") {
		t.Error("the baseline's page compares it with itself")
	}

	if rep := diffOf(t, f, "scan-2"); rep.BaselineScanID != "scan-1" || !rep.Comparable || len(rep.Changes) == 0 {
		t.Errorf("diff of the baseline = against %q, comparable %v, %d changes; want against scan-1 with changes",
			rep.BaselineScanID, rep.Comparable, len(rep.Changes))
	}

	// Every other scan is still measured against the baseline, earlier and
	// later alike.
	for _, id := range []string{"scan-1", "scan-3"} {
		if rep := diffOf(t, f, id); rep.BaselineScanID != "scan-2" {
			t.Errorf("diff of %s is against %q, want the baseline scan-2", id, rep.BaselineScanID)
		}

		if strings.Contains(resultPage(t, f, id), `id="compared-against"`) {
			t.Errorf("%s, which is not the baseline, says it is compared with the previous scan", id)
		}
	}
}

// AC5: a baseline with nothing before it says so, not "no baseline".
func TestAFirstScanThatIsTheBaselineSaysNothingCameBeforeIt(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)
	approve(t, f, model.ConsentReject, "scan-1")

	html := resultPage(t, f, "scan-1")

	if !strings.Contains(html, diff.ReasonFirstBaseline) {
		t.Error("a first scan that is the baseline does not say nothing came before it")
	}

	if strings.Contains(html, "no baseline to compare against") {
		t.Error("the baseline's page says there is no baseline")
	}

	if rep := diffOf(t, f, "scan-1"); rep.Reason != diff.ReasonFirstBaseline || rep.Comparable {
		t.Errorf("diff reason = %q (comparable %v), want %q", rep.Reason, rep.Comparable, diff.ReasonFirstBaseline)
	}
}

// AC7: a shared view neither offers approval nor names the series' baseline.
func TestTheSharedViewOffersNoApprovalAndNamesNoBaseline(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)
	f.seed("scan-2", model.ConsentReject, time.Now().Add(time.Minute), nil)
	approve(t, f, model.ConsentReject, "scan-1")

	for _, id := range []string{"scan-1", "scan-2"} {
		resp := f.get(linkFor(t, signer, "site", "reject", id))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("shared %s: status %d", id, resp.StatusCode)
		}

		html := body(t, resp)

		for _, banned := range []string{approveFormMarker, "Approve as baseline", "current baseline", `id="baseline"`} {
			if strings.Contains(html, banned) {
				t.Errorf("shared %s renders %q", id, banned)
			}
		}
	}
}
