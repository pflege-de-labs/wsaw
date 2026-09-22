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

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// newScannerWithRules is newScannerFor with the rule set supplied by the
// caller, for tests that need a rule pack the builtin set does not ship
// (Story 2.7).
func newScannerWithRules(t *testing.T, info browser.Info, rules *consent.RuleSet) (*scanner.Scanner, func()) {
	t.Helper()

	launch := browser.Options{
		Info:          info,
		LaunchTimeout: 40 * time.Second,
		ProfileDir:    t.TempDir(),
	}

	pool := browser.NewPool(browser.PoolOptions{Size: 1, Launch: launch})

	normalizer, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	s, err := scanner.New(scanner.Deps{Pool: pool, Rules: rules}, scanner.Options{
		Normalizer:            normalizer,
		AllowHeuristicConsent: true,
		WsawVersion:           "test",
		ChromeVersion:         info.Version,
	})
	if err != nil {
		t.Fatal(err)
	}

	return s, func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing pool: %v", err)
		}
	}
}

func scanTarget(name, url string) config.Resolved {
	return config.Resolved{
		Name:         name,
		URL:          url,
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		IdleQuiet:    time.Second,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  100,
		Robots:       config.RobotsIgnore,
	}
}

// The hartmann.info Cookiebot regression fixture moved to
// consent_necessary_only_test.go: that site's banner (and hartmanndirect.com's,
// verified live) has no decline control at all, which Story 2.8 covers, not
// escalation.

// pointerOnlyBannerRulePack defines a fixture rule whose reject step records
// a choice independently of the banner (mirroring a vendor API), then tries a
// plain click on a decline control that only reacts to pointerdown — modelling
// the class of banner Element.click() cannot dismiss (Story 2.7, AC3).
const pointerOnlyBannerRulePack = `
version: 1
rules:
  - name: pointer-only-banner
    priority: 1000
    detect: |
      !!document.getElementById('pointer-banner')
    reject:
      - eval: |
          window.__wsawTestConsent = 'reject'; true
      - click: "#pointer-decline"
        optional: true
    verify: |
      window.__wsawTestConsent === 'reject'
`

const pointerOnlyBannerFixtureHTML = `<!DOCTYPE html>
<html><head><title>pointer-only banner fixture</title></head>
<body>
<h1>fixture</h1>
<div id="pointer-banner" style="position:fixed;bottom:0;width:400px;height:150px;background:#eee">
  <p>We use cookies for consent purposes.</p>
  <button id="pointer-decline">Decline</button>
</div>
<script>
document.getElementById('pointer-decline').addEventListener('pointerdown', function () {
  document.getElementById('pointer-banner').style.display = 'none';
});
</script>
</body></html>`

// TestEscalationClosesABannerThatOnlyReactsToPointerdown proves the point of
// the fallback click: the rule's own click step calls Element.click(), which
// never fires a "pointerdown" listener, so the first pass leaves the banner
// open even though the choice was already recorded. Escalation's fuller
// pointer/mouse event sequence is what actually closes it.
func TestEscalationClosesABannerThatOnlyReactsToPointerdown(t *testing.T) {
	info := requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pointerOnlyBannerFixtureHTML))
	}))
	defer srv.Close()

	rulesPath := filepath.Join(t.TempDir(), "pointer-only.yaml")
	if err := os.WriteFile(rulesPath, []byte(pointerOnlyBannerRulePack), 0o600); err != nil {
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

	out, err := s.Scan(ctx, scanTarget("pointer-only-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Detection != "rule:pointer-only-banner" {
		t.Fatalf("detection = %q, want the pointer-only-banner rule", c.Detection)
	}

	// The choice was recorded on the first pass; only the banner needed the
	// fallback click, so the final outcome is still applied.
	if c.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied: the fallback click should have closed the banner",
			c.Outcome, c.Reason)
	}

	// The mechanism must say a fallback click was needed, distinguishing this
	// from a rule whose own mechanism closed the banner unaided (Story 2.7, AC4).
	if c.Mechanism != "vendor-api+click" {
		t.Errorf("mechanism = %q, want vendor-api+click", c.Mechanism)
	}

	if !strings.Contains(c.Reason, "fallback click") {
		t.Errorf("reason = %q, want it to mention the fallback click", c.Reason)
	}
}

// delayedFadeBannerRulePack defines a fixture rule whose reject step records
// the choice immediately (mirroring a vendor API), on a banner that only
// starts hiding itself well after the choice is recorded — modelling a CMP
// that fades its dialog out on its own schedule rather than as a direct,
// synchronous side effect of the click/eval step (the race behind the
// scan-12496f5f4066e1889b918152 report: banner-visible recorded against a
// screenshot that already shows no banner).
const delayedFadeBannerRulePack = `
version: 1
rules:
  - name: delayed-fade-banner
    priority: 1000
    detect: |
      !!document.getElementById('fade-banner')
    reject:
      - eval: |
          window.__wsawTestConsent = 'reject';
          setTimeout(function () {
            document.getElementById('fade-banner').style.display = 'none';
          }, 600);
          true
    verify: |
      window.__wsawTestConsent === 'reject'
`

const delayedFadeBannerFixtureHTML = `<!DOCTYPE html>
<html><head><title>delayed-fade banner fixture</title></head>
<body>
<h1>fixture</h1>
<div id="fade-banner" style="position:fixed;bottom:0;width:400px;height:150px;background:#eee">
  <p>We use cookies for consent purposes.</p>
  <button id="fade-decline">Decline</button>
</div>
</body></html>`

// TestBannerGoneToleratesADelayedDismissAnimation is the regression test for
// the race fixed alongside this test: bannerGone took a single DOM snapshot
// the instant the CMP recorded a choice, with no tolerance for a dialog that
// disappears a moment later on its own. That single check landed while the
// banner was still on screen, so the outcome was locked in as
// banner-visible even though the banner was already gone well before the
// after-consent screenshot was taken.
func TestBannerGoneToleratesADelayedDismissAnimation(t *testing.T) {
	info := requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(delayedFadeBannerFixtureHTML))
	}))
	defer srv.Close()

	rulesPath := filepath.Join(t.TempDir(), "delayed-fade.yaml")
	if err := os.WriteFile(rulesPath, []byte(delayedFadeBannerRulePack), 0o600); err != nil {
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

	out, err := s.Scan(ctx, scanTarget("delayed-fade-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Detection != "rule:delayed-fade-banner" {
		t.Fatalf("detection = %q, want the delayed-fade-banner rule", c.Detection)
	}

	// The choice was recorded straight away; the banner also closes on its
	// own within the tolerated window, so the final outcome must be applied,
	// not banner-visible.
	if c.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied: the banner closes on its own shortly after the choice is recorded",
			c.Outcome, c.Reason)
	}
}
