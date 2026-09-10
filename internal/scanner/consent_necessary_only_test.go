package scanner_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// noRejectBannerRulePack defines a fixture rule with no `reject` sequence at
// all — only a `necessary` fallback — modelling a CMP whose banner offers
// "Accept all" and a settings panel, but no reject/decline control of any
// kind (Story 2.8).
const noRejectBannerRulePack = `
version: 1
rules:
  - name: no-reject-banner
    priority: 1000
    detect: |
      !!document.getElementById('no-reject-banner')
    necessary:
      - click: "#open-settings"
      - click: "#save-settings"
    verify: |
      window.__wsawTestConsent === 'necessary'
`

// noRejectBannerFixtureHTML is a banner with "Accept all" and a settings
// panel whose categories default to unchecked (only "Necessary" is checked
// and locked). Saving the panel without touching anything records exactly
// the strictly-necessary state — there is no button anywhere on this banner
// that means "reject" or "decline".
const noRejectBannerFixtureHTML = `<!DOCTYPE html>
<html><head><title>no-reject banner fixture</title></head>
<body>
<h1>fixture</h1>
<div id="no-reject-banner" style="position:fixed;bottom:0;width:400px;height:260px;background:#eee">
  <p>We use cookies. Please choose.</p>
  <button id="accept-all">Accept all</button>
  <button id="open-settings">Manage settings</button>
  <div id="settings-panel" style="display:none">
    <label><input type="checkbox" checked disabled> Necessary</label>
    <label><input type="checkbox" id="cat-preferences"> Preferences</label>
    <label><input type="checkbox" id="cat-statistics"> Statistics</label>
    <label><input type="checkbox" id="cat-marketing"> Marketing</label>
    <button id="save-settings">Save settings</button>
  </div>
</div>
<script>
document.getElementById('accept-all').addEventListener('click', function () {
  window.__wsawTestConsent = 'all';
  document.getElementById('no-reject-banner').style.display = 'none';
});
document.getElementById('open-settings').addEventListener('click', function () {
  document.getElementById('settings-panel').style.display = 'block';
});
document.getElementById('save-settings').addEventListener('click', function () {
  window.__wsawTestConsent = 'necessary';
  document.getElementById('no-reject-banner').style.display = 'none';
});
</script>
</body></html>`

// TestNoRejectControlFallsBackToNecessaryOnly is the regression test for
// Story 2.8: a banner with no reject/decline control at all must not be
// reported as "failed" for lack of a matching selector, and must never be
// reported as "applied" either — there was nothing to reject. wsaw falls back
// to the rule's necessary-only sequence and records a distinct outcome.
func TestNoRejectControlFallsBackToNecessaryOnly(t *testing.T) {
	info := requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(noRejectBannerFixtureHTML))
	}))
	defer srv.Close()

	rulesPath := filepath.Join(t.TempDir(), "no-reject.yaml")
	if err := os.WriteFile(rulesPath, []byte(noRejectBannerRulePack), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, err := consent.LoadRuleFiles(rulesPath)
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("no-reject-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Detection != "rule:no-reject-banner" {
		t.Fatalf("detection = %q, want the no-reject-banner rule", c.Detection)
	}

	if c.Outcome != model.OutcomeNecessaryOnly {
		t.Fatalf("outcome = %q (%s), want necessary-only: this banner has no reject control at all",
			c.Outcome, c.Reason)
	}

	if c.Reason == "" {
		t.Error("no reason recorded for the necessary-only outcome")
	}

	if !strings.Contains(c.Reason, "no reject control") {
		t.Errorf("reason = %q, want it to explain that the banner has no reject control", c.Reason)
	}
}

// hartmannCookiebotFixtureHTML reproduces the banner found on
// https://hartmanndirect.com/de-de (and https://www.hartmann.info/): a
// Cookiebot "categories" skin whose only controls are "Auswahl erlauben"
// (id CybotCookiebotDialogBodyLevelButtonLevelOptinAllowallSelection) and
// "Alle zulassen" — verified against the live DOM, no decline/reject control
// of any kind exists. window.Cookiebot.submitCustomConsent records the choice
// exactly as the real vendor script does, but — like the real hartmann.info
// skin this first surfaced on — nothing here ever hides the dialog, so the
// fallback's own escalation is exercised too and still cannot close it.
const hartmannCookiebotFixtureHTML = `<!DOCTYPE html>
<html><head><title>hartmann-style fixture</title></head>
<body>
<h1>Welcome — for product information please select your country</h1>

<div id="CybotCookiebotDialog" style="position:fixed;bottom:0;width:640px;height:220px;background:#f4f4f4">
  <p>This website uses cookies. We use cookies to personalise content and ads, to provide
  social media features and to analyse our traffic.</p>
  <input type="checkbox" id="CybotCookiebotDialogBodyLevelButtonNecessary" checked disabled>
  <input type="checkbox" id="CybotCookiebotDialogBodyLevelButtonPreferences">
  <input type="checkbox" id="CybotCookiebotDialogBodyLevelButtonStatistics">
  <input type="checkbox" id="CybotCookiebotDialogBodyLevelButtonMarketing">
  <a id="CybotCookiebotDialogBodyLevelButtonLevelOptinAllowallSelection" href="javascript:void(0)">Auswahl erlauben</a>
  <a id="CybotCookiebotDialogBodyLevelButtonLevelOptinAllowAll" href="javascript:void(0)">Alle zulassen</a>
</div>

<script>
(function () {
  window.Cookiebot = {
    hasResponse: false,
    submitCustomConsent: function (preferences, statistics, marketing) {
      // The real vendor script records the choice here. It does not touch the
      // dialog's markup at all — nothing on this skin ever hides it, the same
      // way the real site's banner behaves.
      window.Cookiebot.hasResponse = true;
      window.__wsawTestConsent = { preferences: preferences, statistics: statistics, marketing: marketing };
    }
  };

  var selection = document.getElementById('CybotCookiebotDialogBodyLevelButtonLevelOptinAllowallSelection');
  selection.addEventListener('click', function () {
    window.Cookiebot.submitCustomConsent(
      document.getElementById('CybotCookiebotDialogBodyLevelButtonPreferences').checked,
      document.getElementById('CybotCookiebotDialogBodyLevelButtonStatistics').checked,
      document.getElementById('CybotCookiebotDialogBodyLevelButtonMarketing').checked
    );
  });

  var allowAll = document.getElementById('CybotCookiebotDialogBodyLevelButtonLevelOptinAllowAll');
  allowAll.addEventListener('click', function () {
    window.Cookiebot.submitCustomConsent(true, true, true);
  });
})();
</script>
</body></html>`

// TestCookiebotWithNoDeclineControlFallsBackToNecessaryOnly is the regression
// test for Story 2.8, using the actual shipped "cookiebot" rule (not a
// synthetic fixture rule) against a banner shaped exactly like the live DOM
// verified on https://hartmanndirect.com/de-de: only "Auswahl erlauben" and
// "Alle zulassen" are offered, no decline control exists anywhere, and the
// dialog never hides itself on any click.
//
// Before Story 2.8, the cookiebot rule called
// window.Cookiebot.submitCustomConsent(false, false, false) unconditionally —
// a function that exists on every Cookiebot-powered page regardless of which
// buttons the site chose to show. That recorded a full rejection no visitor
// of this banner could ever have produced themselves, which is worse than the
// honest "no reject control" this story exists to report (Tenet 5).
func TestCookiebotWithNoDeclineControlFallsBackToNecessaryOnly(t *testing.T) {
	info := requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(hartmannCookiebotFixtureHTML))
	}))
	defer srv.Close()

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("hartmann-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.CMP != "Cookiebot" {
		t.Fatalf("CMP = %q, want Cookiebot", c.CMP)
	}

	if c.Detection != "rule:cookiebot" {
		t.Fatalf("detection = %q, want the cookiebot rule", c.Detection)
	}

	// This banner offers no reject control at all, so the outcome must never
	// be "applied" — that would claim a rejection this banner cannot produce —
	// and it is not what the fallback is for either: it is "necessary-only".
	if c.Outcome != model.OutcomeNecessaryOnly {
		t.Fatalf("outcome = %q (%s), want necessary-only: this banner has no decline control at all",
			c.Outcome, c.Reason)
	}

	if !strings.Contains(c.Reason, "no reject control") {
		t.Errorf("reason = %q, want it to explain that the banner has no reject control", c.Reason)
	}

	// The fixture's dialog never hides itself, mirroring the real site: the
	// necessary-only fallback's own escalation is exercised and still cannot
	// close it, and that must be visible in the reason too.
	if !strings.Contains(c.Reason, "remained displayed") {
		t.Errorf("reason = %q, want it to say the banner remained displayed", c.Reason)
	}

	if c.Heuristic {
		t.Error("a documented vendor rule was reported as a heuristic match")
	}
}
