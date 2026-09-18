package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.26: pressing "Scan now" on the watchboard used to navigate the
// reader to the target's history page, taking their filter, their collapsed
// groups and their place in the list with it. These tests cover where a press
// returns to, that nothing a request says can send it off this origin, and
// that the press still works with the script blocked.

// scanForm is the form a press submits, with whatever return page the test is
// about.
func scanForm(from string) url.Values {
	v := url.Values{}

	if from != "" {
		v.Set("from", from)
	}

	return v
}

// location is where a response redirects to, with the flash parameters kept.
func location(t *testing.T, resp *http.Response) *url.URL {
	t.Helper()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	return u
}

func TestRescanFromTheBoardReturnsToTheBoard(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()
	f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger)

	dest := location(t, f.postForm("/rescan/site/reject", scanForm("/")))

	if dest.Path != "/" {
		t.Errorf("a press on the board returned to %q, want the board", dest.Path)
	}

	// The message has to name the target: a board shows many, and "Scan
	// started" alone does not say which one started (AC3).
	msg := dest.Query().Get("ok")
	if !strings.Contains(msg, "site") || !strings.Contains(msg, "reject") {
		t.Errorf("the flash message %q does not name the target and consent mode", msg)
	}

	if strings.Contains(msg, "below") {
		t.Error("the flash message still points at a page below it, which the board has not")
	}

	<-trigger.started
	close(trigger.release)
}

func TestRescanWithoutAReturnPageGoesToTheTargetPage(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()
	f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger)

	dest := location(t, f.postForm("/rescan/site/reject", scanForm("")))

	if dest.Path != "/targets/site/reject" {
		t.Errorf("a press with no return page went to %q, want the target's history page", dest.Path)
	}

	<-trigger.started
	close(trigger.release)
}

// A return page arrives in a form field, so it is attacker-influenced. None of
// these may decide where the reader is sent (AC2, Tenet 9).
func TestRescanRefusesAReturnPageItDoesNotServe(t *testing.T) {
	t.Parallel()

	for _, from := range []string{
		"https://evil.example/",
		"//evil.example/",
		"/\r\nSet-Cookie: x=1",
		"/nowhere",
		"/targets/site",
		"/targets/site/bogus-mode",
		"/targets/site/reject/extra",
		"targets/site/reject",
	} {
		t.Run(from, func(t *testing.T) {
			t.Parallel()

			trigger := newBlockingTrigger()
			f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger)

			dest := location(t, f.postForm("/rescan/site/reject", scanForm(from)))

			if dest.Host != "" || dest.Scheme != "" {
				t.Fatalf("a return page of %q left this origin: %q", from, dest)
			}

			if dest.Path != "/targets/site/reject" {
				t.Errorf("a return page of %q redirected to %q, want the target's history page",
					from, dest.Path)
			}

			<-trigger.started
			close(trigger.release)
		})
	}
}

// A refusal has to come back to the same page the press came from, or the
// reader is moved off the board to be told that nothing happened.
func TestRescanRefusalReturnsToThePageItCameFrom(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()

	f := newFixtureLive(t,
		httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger, oneRunning)

	dest := location(t, f.postForm("/rescan/site/reject", scanForm("/")))

	if dest.Path != "/" {
		t.Errorf("a refused press returned to %q, want the board it came from", dest.Path)
	}

	if msg := dest.Query().Get("err"); msg == "" {
		t.Error("a refused press said nothing about why it was refused")
	} else if strings.Contains(msg, "below") {
		t.Errorf("the refusal %q points at a page below it, which the board has not", msg)
	}

	if got := trigger.count(); got != 0 {
		t.Errorf("a refused press started %d scans", got)
	}
}

// With the script blocked the form is the whole feature, so it must still
// start exactly one scan (AC1, AC8).
func TestRescanWithoutScriptStartsExactlyOneScan(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()
	f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger)

	resp := f.postForm("/rescan/site/reject", scanForm("/"), "Accept", "text/html")

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a plain form submission = %d, want 303", resp.StatusCode)
	}

	select {
	case <-trigger.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the press reported success without starting a scan")
	}

	if got := trigger.count(); got != 1 {
		t.Errorf("a single press started %d scans", got)
	}

	close(trigger.release)
}

// A scripted press has no page to land on: it never left the one it was made
// from, so the answer is a body rather than a redirect (AC4).
func TestScriptedRescanIsAnsweredWithoutARedirect(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()
	f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger)

	resp := f.postForm("/rescan/site/reject", scanForm("/"), "Accept", "application/json")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a scripted press = %d, want 200", resp.StatusCode)
	}

	if dest := resp.Header.Get("Location"); dest != "" {
		t.Errorf("a scripted press was redirected to %q; the page it came from is still open", dest)
	}

	var reply struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &reply); err != nil {
		t.Fatal(err)
	}

	if !reply.OK {
		t.Error("a started scan was not reported as started")
	}

	if !strings.Contains(reply.Message, "site") || !strings.Contains(reply.Message, "reject") {
		t.Errorf("the reply %q does not name the target and consent mode", reply.Message)
	}

	<-trigger.started
	close(trigger.release)
}

// The script has to tell "this did not happen" from "this happened" without
// reading prose, so a refusal carries a status of its own (AC8).
func TestScriptedRescanReportsARefusalAsAFailure(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()

	f := newFixtureLive(t,
		httpapi.Options{WebUI: true, AllowAdHocScan: true}, trigger, oneRunning)

	resp := f.postForm("/rescan/site/reject", scanForm("/"), "Accept", "application/json")

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a duplicate scripted press = %d, want 409", resp.StatusCode)
	}

	var reply struct {
		Error string `json:"error"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &reply); err != nil {
		t.Fatal(err)
	}

	if reply.Error == "" {
		t.Error("a refused scripted press said nothing about why")
	}

	if got := trigger.count(); got != 0 {
		t.Errorf("a refused press started %d scans", got)
	}
}

func TestScriptedRescanIsRefusedInReadOnlyMode(t *testing.T) {
	t.Parallel()

	trigger := newBlockingTrigger()
	f := newFixture(t,
		httpapi.Options{WebUI: true, ReadOnly: true, AllowAdHocScan: true}, trigger)

	resp := f.postForm("/rescan/site/reject", scanForm("/"), "Accept", "application/json")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a scripted press in read-only mode = %d, want 403", resp.StatusCode)
	}

	if got := trigger.count(); got != 0 {
		t.Errorf("read-only mode started %d scans", got)
	}
}

// The board's own form, and the page furniture the script writes into.
func TestWatchboardScanFormSaysWhichPageItIsOn(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, &fakeTrigger{})
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `class="inline scan-form"`) {
		t.Error("the board's rescan form is not marked for the script to find")
	}

	if !strings.Contains(html, `<input type="hidden" name="from" value="/"`) {
		t.Error("the board's rescan form does not say which page it was submitted from")
	}

	// The slots are in the document before there is anything to put in them,
	// because an element that appears out of nowhere is not announced (AC5).
	for _, want := range []string{`id="flash-ok"`, `id="flash-error"`, `aria-live="polite"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the page has no %s for a scripted press to report into", want)
		}
	}

	if !strings.Contains(html, `<script src="/static/scan.js" defer></script>`) {
		t.Error("the board does not load the script that keeps a press on the page")
	}

	// Story 5.23 AC11: the CSP forbids unsafe-inline, so a handler or a style
	// written into the markup is dropped silently rather than failing loudly.
	for _, forbidden := range []string{` style="`, ` onclick=`, ` onsubmit=`} {
		if strings.Contains(html, forbidden) {
			t.Errorf("the board carries an inline %q, which the CSP drops silently", forbidden)
		}
	}
}

func TestScanScriptIsServedAndMakesNoOffsiteRequest(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, &fakeTrigger{})

	resp := f.get("/static/scan.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/scan.js = %d, want 200", resp.StatusCode)
	}

	js := body(t, resp)

	if !strings.Contains(js, "scan-form") {
		t.Error("/static/scan.js does not look like the script that intercepts a press")
	}

	if strings.Contains(js, "http://") || strings.Contains(js, "https://") {
		t.Error("/static/scan.js reaches for something off this origin")
	}
}

func TestTheBoardOffersNoScanButtonWhenScanningIsNotAllowed(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string]httpapi.Options{
		"read-only":        {WebUI: true, ReadOnly: true, AllowAdHocScan: true},
		"ad-hoc scans off": {WebUI: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, opts, &fakeTrigger{})
			f.seed("scan-1", model.ConsentReject, time.Now(), nil)

			html := body(t, f.get("/", "Accept", "text/html"))

			if strings.Contains(html, "Scan now") {
				t.Errorf("the board offers a scan button with %s", name)
			}
		})
	}
}
