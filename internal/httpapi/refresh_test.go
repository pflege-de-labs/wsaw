package httpapi_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// Story 5.16: the dashboard refreshes on an interval the viewer sets from the
// page. The tests below care most about the two things that make that safe —
// the page saying how old it is, and a reload not repeating an action.

// AC1 and AC4: an interval configured for the deployment applies to a viewer
// who has expressed no preference, so a wall display is right the first time
// it is opened.
func TestTheConfiguredIntervalRefreshesAnIdlePage(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `content="30;url=/"`) {
		t.Error("the configured interval did not reach the page")
	}

	if !strings.Contains(html, "refreshing every 30s") {
		t.Error("the page does not say what it will do")
	}
}

// With no default and nothing running, the page holds still. A page that
// reloads for no reason is just load on the daemon.
func TestAnIdlePageWithNoDefaultDoesNotRefresh(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, "http-equiv=\"refresh\"") {
		t.Error("a page with nothing to watch refreshes anyway")
	}

	if !strings.Contains(html, "auto-refresh off") {
		t.Error("the page does not say that it will not refresh")
	}
}

// AC5: the page states when it was rendered. This is the criterion that makes
// automatic refreshing safe — a page that reloads silently invites the reader
// to trust whatever is on it.
func TestThePageSaysWhenItWasRendered(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, RefreshDefault: time.Minute}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "as of ") {
		t.Error("the page does not say when it was rendered")
	}

	if !strings.Contains(html, "data-rendered-at=") {
		t.Error("the render time is not machine-readable, so the age cannot be counted up (AC6)")
	}

	// AC6's other half: the enhancement that keeps the age honest has to be
	// loaded, and from this origin — the CSP allows no other.
	if !strings.Contains(html, `src="/static/refresh.js"`) {
		t.Error("the freshness script is not loaded")
	}
}

// AC2: the control is a form, not a script. With JavaScript off it still has
// to work.
func TestTheIntervalControlIsAPlainForm(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `<form method="post" action="/refresh"`) {
		t.Error("the interval control is not a form")
	}

	if !strings.Contains(html, `<select id="refresh-interval" name="interval">`) {
		t.Error("the control offers no intervals")
	}

	// The floor is visible as the shortest option rather than hidden behind a
	// rejection message (AC8).
	if strings.Contains(html, `<option value="1">`) {
		t.Error("the control offers an interval below the floor")
	}

	// And the no-JS refresh really is inside <noscript>, so the two
	// mechanisms cannot both fire.
	f2 := newFixture(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second}, nil)

	html = body(t, f2.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `<noscript><meta http-equiv="refresh"`) {
		t.Error("the meta refresh is not inside <noscript>; with script on, the page would refresh twice")
	}
}

// AC3: a choice is remembered per browser, in a cookie rather than in wsaw's
// configuration. One viewer's wall display must not change what anyone else
// sees.
func TestTheChoiceIsRememberedInACookie(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	resp := f.postForm("/refresh", url.Values{"interval": {"60"}, "return": {"/"}})

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setting the interval = %d, want 303", resp.StatusCode)
	}

	var cookie *http.Cookie

	for _, c := range resp.Cookies() {
		if c.Name == "wsaw_refresh" {
			cookie = c
		}
	}

	if cookie == nil {
		t.Fatal("no preference cookie was set")
	}

	if cookie.Value != "60" {
		t.Errorf("cookie value = %q, want 60", cookie.Value)
	}

	// And the page honours it on the next request.
	html := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_refresh=60"))

	if !strings.Contains(html, "refreshing every 1m") {
		t.Error("the remembered interval was not applied")
	}
}

// AC7: an explicit "off" beats the reload a running scan would otherwise
// trigger. The reader asked for the page to hold still, and the running scan
// is visible on it.
func TestTurningRefreshOffBeatsARunningScan(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{WebUI: true}, nil, oneRunning)

	// Without a preference, a running scan makes the page reload (Story 5.12).
	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "http-equiv=\"refresh\"") {
		t.Fatal("a page with a scan in flight no longer refreshes")
	}

	// With refreshing turned off, it does not.
	html = body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_refresh=0"))

	if strings.Contains(html, "http-equiv=\"refresh\"") {
		t.Error("a page refreshed despite the viewer turning it off")
	}

	if !strings.Contains(html, "auto-refresh off") {
		t.Error("the page does not say that refreshing is off")
	}
}

// AC11: the interval can come from the URL, so a wall display needs one
// address and no further setup — and the URL wins over a stored preference.
func TestTheURLCanSetTheIntervalAndWins(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/?refresh=15", "Accept", "text/html", "Cookie", "wsaw_refresh=300"))

	if !strings.Contains(html, "refreshing every 15s") {
		t.Error("the interval in the URL did not win over the stored one")
	}

	// The refresh keeps the parameter, or the next reload would drop back to
	// the cookie's interval and the display would slow down on its own.
	if !strings.Contains(html, "refresh=15") {
		t.Error("the refresh URL does not carry the interval forward")
	}
}

// AC8: an interval below the floor is clamped rather than refused, and the
// viewer can see what they actually got.
func TestAnIntervalBelowTheFloorIsClamped(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/?refresh=1", "Accept", "text/html"))

	if strings.Contains(html, `content="1;`) {
		t.Error("a one-second interval was accepted; every refresh re-reads every target's latest result")
	}

	if !strings.Contains(html, "refreshing every 5s") {
		t.Errorf("the clamped interval is not shown as what it is")
	}
}

// AC9: a reload must not re-show a message about something that happened
// once. The refresh URL therefore drops the flash parameters.
func TestARefreshDoesNotRepeatAFlashMessage(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, RefreshDefault: 30 * time.Second}, nil)

	html := body(t, f.get("/?ok=Baseline+approved.", "Accept", "text/html"))

	// The message is shown once...
	if !strings.Contains(html, "Baseline approved.") {
		t.Fatal("the flash message was not shown at all")
	}

	// ...and the refresh does not bring it back.
	if strings.Contains(html, "ok=Baseline") {
		t.Error("the refresh URL carries the flash message, so it would reappear every 30 seconds")
	}

	if !strings.Contains(html, `content="30;url=/"`) {
		t.Errorf("the refresh URL is not the page without its flash parameters")
	}
}

// A nonsense interval is refused with an explanation rather than silently
// ignored: a viewer who thinks they set something must not be left believing
// it took effect.
func TestANonsenseIntervalIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	resp := f.postForm("/refresh", url.Values{"interval": {"soonish"}, "return": {"/"}})

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a bad interval = %d, want a redirect carrying the reason", resp.StatusCode)
	}

	if dest := resp.Header.Get("Location"); !strings.Contains(dest, "err=") {
		t.Errorf("redirected to %q without explaining the refusal", dest)
	}
}

// The control must not send a viewer somewhere else: the return path comes
// from the request, so it is attacker-influenced like any other (Tenet 9).
func TestTheReturnPathCannotLeaveThisOrigin(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	for _, bad := range []string{"https://evil.example/", "//evil.example/", "javascript:alert(1)"} {
		resp := f.postForm("/refresh", url.Values{"interval": {"30"}, "return": {bad}})

		dest := resp.Header.Get("Location")
		if strings.Contains(dest, "evil.example") || strings.HasPrefix(dest, "javascript:") {
			t.Errorf("return=%q redirected off-origin to %q", bad, dest)
		}
	}
}

// The series page gets the same treatment, so the setting is not a per-page
// surprise.
func TestTheSeriesPageAlsoHonoursTheSetting(t *testing.T) {
	t.Parallel()

	f := newFixtureLive(t, httpapi.Options{WebUI: true}, nil,
		func() []scanner.Running { return nil })
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/targets/site/reject?refresh=30", "Accept", "text/html"))

	if !strings.Contains(html, "refreshing every 30s") {
		t.Error("the series page ignores the interval")
	}

	if !strings.Contains(html, "as of ") {
		t.Error("the series page does not say when it was rendered")
	}
}
