package scanner_test

// Story 2.9: a site that wrote its own consent banner. No vendor, no TCF, no
// stable selector — the class names come from the build — the choice lives in
// localStorage rather than a cookie, and the site's analytics call is
// reverse-proxied onto its own domain so it is first-party by every rule wsaw
// applies.
//
// The fixture reproduces the shape found on https://mein-pflegegrad-rechner.de/
// on 2026-09-18, where a reject-mode scan recorded `failed` with no CMP
// identity, no cookies, zero third-party domains and every request marked
// pre-interaction. It is a stored fixture rather than the live site (Tenet 13):
// the site will be rebuilt, and the shape is what must keep working.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// bespokeBannerFixtureHTML renders its banner from application code after
// load, the way a hydrating front end does, names its elements with
// build-generated classes, and records the answer in localStorage. The
// analytics beacon fires before anyone is asked anything and again after the
// choice, which is the behaviour that makes "0 third-party domains" a
// misleading summary.
const bespokeBannerFixtureHTML = `<!DOCTYPE html>
<html lang="de"><head><title>bespoke banner fixture</title></head>
<body class="__className_fca8ee">
<h1>Pflegegrad berechnen</h1>
<p>Ein Rechner, der keinen CMP einsetzt.</p>

<footer class="__ftr_9ac1">
  <p>Impressum · <a href="/datenschutz">Datenschutz</a> · Diese Seite nutzt Cookies laut Datenschutzerkl&auml;rung.</p>
</footer>

<script>
(function () {
  // Analytics starts on its own, before any consent interaction, exactly as a
  // self-hosted collector initialised on an idle callback does.
  fetch('/hog/e?ev=pageview', { method: 'GET' });

  function render() {
    var box = document.createElement('div');
    box.className = '__cls_b7f21a __cls_9931cc';
    box.setAttribute('style',
      'position:fixed;bottom:10px;left:10px;right:10px;max-width:1200px;background:#fff;padding:20px;height:180px');
    box.innerHTML =
      '<p class="__cls_1f0d">Wir verwenden Cookies</p>' +
      '<p>Diese Website nutzt Cookies, um Inhalte zu personalisieren und Zugriffe zu analysieren.</p>' +
      '<button class="__cls_44ab" id="deny">Ablehnen</button>' +
      '<button class="__cls_44ac" id="allow">Akzeptieren</button>';
    document.body.appendChild(box);

    function answer(accepted) {
      window.localStorage.setItem('cookie-accepted', accepted ? 'true' : 'false');
      box.remove();
      fetch('/hog/e?ev=consent', { method: 'GET' });
    }

    document.getElementById('deny').addEventListener('click', function () { answer(false); });
    document.getElementById('allow').addEventListener('click', function () { answer(true); });
  }

  // Mounted after load, not in the served HTML: the case a single check taken
  // the instant the page goes idle reports as "no CMP".
  setTimeout(render, 600);
})();
</script>
</body></html>`

// noBannerFixtureHTML asks for no consent at all. It is the other half of the
// three-way distinction: "no banner" must stay a clean, cheap outcome.
const noBannerFixtureHTML = `<!DOCTYPE html>
<html lang="de"><head><title>no banner fixture</title></head>
<body><h1>Nothing to consent to</h1><p>Static page, no tracking, no banner.</p></body></html>`

func bespokeFixtureServer(t *testing.T, html string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

		case strings.HasPrefix(r.URL.Path, "/hog/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":1}`))

		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(html))
		}
	}))

	t.Cleanup(srv.Close)

	return srv
}

// newScannerWithoutHeuristics is newScannerWithRules with label guessing
// turned off, which is how an operator asks to be told about an unknown
// banner rather than have wsaw guess at it.
func newScannerWithoutHeuristics(t *testing.T, info browser.Info, rules *consent.RuleSet) (*scanner.Scanner, func()) {
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
		AllowHeuristicConsent: false,
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

// TestBespokeBannerIsHandledAndRecordedAsItsOwnKind is the regression test for
// the site that surfaced Story 2.9: the banner is mounted after load, the
// choice goes to localStorage, and no vendor fingerprint exists anywhere.
func TestBespokeBannerIsHandledAndRecordedAsItsOwnKind(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, bespokeBannerFixtureHTML)

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("bespoke-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result
	c := res.Consent

	// AC3: a banner rendered after load is a banner, not an absent one.
	if c.Outcome == model.OutcomeNotNeeded {
		t.Fatalf("outcome = not-needed (%s): a banner mounted after load was reported as no CMP at all", c.Reason)
	}

	if c.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied", c.Outcome, c.Reason)
	}

	// AC1: the page has a consent UI and no vendor behind it.
	if c.Kind != model.CMPKindBespoke {
		t.Errorf("cmp kind = %q, want %q", c.Kind, model.CMPKindBespoke)
	}

	if !c.Heuristic {
		t.Error("a label-matched banner was not flagged as heuristic")
	}

	// AC2: which banner this was, answerable from the document alone.
	if !strings.Contains(c.BannerHeading, "Wir verwenden Cookies") {
		t.Errorf("banner heading = %q, want the banner's own first line", c.BannerHeading)
	}

	// AC5: the choice was recorded in Web Storage, and that is visible.
	if !slices.Contains(c.StorageKeys, "local:cookie-accepted") {
		t.Errorf("consent storage keys = %v, want local:cookie-accepted", c.StorageKeys)
	}

	var stored *model.StorageEntry

	for i := range res.Storage {
		if res.Storage[i].Key == "cookie-accepted" {
			stored = &res.Storage[i]
		}
	}

	if stored == nil {
		t.Fatalf("the scan recorded no web storage; entries = %v", res.Storage)
	}

	if stored.Area != model.StorageLocal || stored.Party != model.FirstParty {
		t.Errorf("stored entry = %+v, want first-party localStorage", *stored)
	}

	if stored.ValueSHA256 == "" || stored.ValueLength == 0 {
		t.Errorf("stored entry = %+v, want a value digest and length but no value", *stored)
	}

	// AC7: the analytics call that fired before anyone was asked is
	// first-party by host, and must still be visible.
	pre := res.FirstPartyHosts(model.PhasePre)
	if len(pre) == 0 {
		t.Error("no first-party hosts recorded for the pre-consent phase, though the page beaconed on load")
	}

	var md strings.Builder
	if err := report.WriteMarkdown(&md, res, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(md.String(), "First-party hosts contacted before any consent interaction") {
		t.Error("the report does not name the first-party hosts contacted before the interaction")
	}

	if !strings.Contains(md.String(), "no vendor identified") {
		t.Error("the report does not say the banner had no vendor behind it")
	}
}

// TestUnhandledBespokeBannerIsNeverReportedAsNoCMP covers the case an operator
// asked for by turning label guessing off: wsaw must say it found a banner it
// could not drive, and leave behind what the next rule author needs.
func TestUnhandledBespokeBannerIsNeverReportedAsNoCMP(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, bespokeBannerFixtureHTML)

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithoutHeuristics(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("bespoke-unhandled", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Outcome == model.OutcomeNotNeeded {
		t.Fatalf("outcome = not-needed (%s): the page does ask for consent", c.Reason)
	}

	if c.Kind != model.CMPKindBespoke {
		t.Errorf("cmp kind = %q, want %q", c.Kind, model.CMPKindBespoke)
	}

	// AC4: what was found, what it said, what it offered.
	if c.Diagnostic == nil {
		t.Fatal("no diagnostic recorded for a banner that was found but not handled")
	}

	if c.Diagnostic.Element == "" {
		t.Error("the diagnostic does not name the element that matched")
	}

	if !slices.Contains(c.Diagnostic.Controls, "Ablehnen") ||
		!slices.Contains(c.Diagnostic.Controls, "Akzeptieren") {
		t.Errorf("diagnostic controls = %v, want the banner's own labels", c.Diagnostic.Controls)
	}
}

// TestPageWithoutABannerStaysNotNeeded is the other side of AC1: the wait must
// not invent a banner, and a page that never asks for consent keeps its clean,
// cheap outcome.
func TestPageWithoutABannerStaysNotNeeded(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, noBannerFixtureHTML)

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("no-banner-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Outcome != model.OutcomeNotNeeded {
		t.Fatalf("outcome = %q (%s), want not-needed on a page with no banner", c.Outcome, c.Reason)
	}

	if c.Kind != model.CMPKindNone {
		t.Errorf("cmp kind = %q, want %q", c.Kind, model.CMPKindNone)
	}

	if c.Diagnostic != nil {
		t.Errorf("a diagnostic was recorded for a page with no banner: %+v", *c.Diagnostic)
	}
}

// staleHostRulePack is scoped to the fixture's host and looks for markup that
// is not there any more — the state a site rule reaches after a redesign.
const staleHostRulePack = `
version: 1
rules:
  - name: fixture-site-banner
    priority: 50
    hosts:
      - "127.0.0.1"
    detect: |
      !!document.getElementById('banner-that-was-here-last-year')
    reject:
      - click: "#banner-that-was-here-last-year button"
    verify: |
      true
`

// TestHostRuleThatStoppedMatchingIsReported covers AC6: a rule written for
// this host that no longer matches it is how a rule stops working unnoticed.
func TestHostRuleThatStoppedMatchingIsReported(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, bespokeBannerFixtureHTML)

	rulesPath := filepath.Join(t.TempDir(), "stale.yaml")
	if err := os.WriteFile(rulesPath, []byte(staleHostRulePack), 0o600); err != nil {
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

	out, err := s.Scan(ctx, scanTarget("stale-rule-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if !slices.Contains(c.StaleHostRules, "fixture-site-banner") {
		t.Errorf("stale host rules = %v, want the rule written for this host", c.StaleHostRules)
	}

	var warned bool

	for _, w := range out.Result.Warnings {
		if strings.Contains(w, "fixture-site-banner") {
			warned = true
		}
	}

	if !warned {
		t.Errorf("warnings = %v, want one naming the rule that no longer matches", out.Result.Warnings)
	}
}

// storageOnlyRulePack has steps and no verify expression at all. Before Story
// 2.9 that could only ever be "unverified"; a site that records its choice in
// Web Storage leaves evidence that is just as good as a consent cookie.
const storageOnlyRulePack = `
version: 1
rules:
  - name: storage-evidence-banner
    priority: 50
    detect: |
      !!document.getElementById('deny')
    reject:
      - click: "#deny"
`

// TestStorageWriteVerifiesARuleThatDefinesNoVerification covers AC5 and AC8:
// the choice was recorded somewhere, and the banner is gone. Neither fact
// alone is enough, and together they are exactly what a consent cookie plus a
// dismissed dialog provide.
func TestStorageWriteVerifiesARuleThatDefinesNoVerification(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, bespokeBannerFixtureHTML)

	rulesPath := filepath.Join(t.TempDir(), "storage-only.yaml")
	if err := os.WriteFile(rulesPath, []byte(storageOnlyRulePack), 0o600); err != nil {
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

	out, err := s.Scan(ctx, scanTarget("storage-evidence-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied: the choice was recorded in storage and the banner is gone",
			c.Outcome, c.Reason)
	}

	if !strings.Contains(c.Reason, "Web Storage") {
		t.Errorf("reason = %q, want it to name the evidence it relied on", c.Reason)
	}

	if !slices.Contains(c.StorageKeys, "local:cookie-accepted") {
		t.Errorf("consent storage keys = %v, want local:cookie-accepted", c.StorageKeys)
	}
}

// tcfBannerFixtureHTML is a minimal TCF v2.2 CMP with a visible banner of its
// own: the vendor half of AC1's three-way distinction. The banner is here on
// purpose — a page can carry both, and the API is what decides the kind.
const tcfBannerFixtureHTML = `<!DOCTYPE html>
<html lang="en"><head><title>tcf fixture</title></head>
<body>
<h1>A page with a real CMP</h1>
<div id="cmp-banner" style="position:fixed;bottom:0;width:600px;height:150px;background:#eee">
  <p>We use cookies. Manage your consent below.</p>
  <button id="cmp-reject">Reject all</button>
  <button id="cmp-accept">Accept all</button>
</div>
<script>
(function () {
  var state = { cmpStatus: 'loaded', eventStatus: 'cmpuishown', tcString: '', cmpId: 28, cmpVersion: 2,
    gdprApplies: true, purpose: { consents: {} } };

  window.OneTrust = {
    RejectAll: function () { record('CPrejectSTRING'); },
    AllowAll: function () { record('CPacceptSTRING'); }
  };

  function record(tc) {
    state.tcString = tc;
    state.eventStatus = 'useractioncomplete';
    var banner = document.getElementById('cmp-banner');
    if (banner) banner.remove();
  }

  window.__tcfapi = function (command, version, callback) {
    if (command === 'getTCData') { callback(state, true); return; }
    callback(null, false);
  };

  document.getElementById('cmp-reject').addEventListener('click', function () { record('CPrejectSTRING'); });
  document.getElementById('cmp-accept').addEventListener('click', function () { record('CPacceptSTRING'); });
})();
</script>
</body></html>`

// TestTCFCMPIsRecordedAsAVendor is the third case of AC1: a real CMP product
// stays a vendor, banner or no banner, and the new classification must not
// reclassify what already worked.
func TestTCFCMPIsRecordedAsAVendor(t *testing.T) {
	info := requireChrome(t)

	srv := bespokeFixtureServer(t, tcfBannerFixtureHTML)

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, scanTarget("tcf-fixture", srv.URL+"/"), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Kind != model.CMPKindVendor {
		t.Errorf("cmp kind = %q, want %q for a page driven through the TCF API", c.Kind, model.CMPKindVendor)
	}

	if c.Outcome != model.OutcomeApplied {
		t.Errorf("outcome = %q (%s), want applied", c.Outcome, c.Reason)
	}

	if c.TCString == "" {
		t.Error("no TC string recorded, though the CMP returned one")
	}
}
