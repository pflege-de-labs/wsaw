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
