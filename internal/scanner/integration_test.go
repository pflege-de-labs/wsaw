package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/browser"
	"github.com/martint17r/wsaw/internal/config"
	"github.com/martint17r/wsaw/internal/consent"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/normalize"
	"github.com/martint17r/wsaw/internal/scanner"
)

// These tests drive a real browser against local fixture servers. They never
// touch a live third-party website: CI must not depend on someone else's site
// staying unchanged (Tenet 13, NFR §5).
//
// They skip when no usable Chrome is installed, and say so, so the fast suite
// stays runnable without a browser.

func requireChrome(t *testing.T) browser.Info {
	t.Helper()

	if os.Getenv("WSAW_SKIP_BROWSER_TESTS") != "" {
		t.Skip("skipping browser tests: WSAW_SKIP_BROWSER_TESTS is set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := browser.Discover(ctx, os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("skipping browser test: no usable Chrome found (%v)", err)
	}

	return info
}

// fixtureSite serves a page that loads a first-party script, a third-party
// pixel before any consent interaction, and a third-party script that only
// loads after the banner is dismissed. That shape is what the product exists
// to measure.
type fixtureSite struct {
	site       *httptest.Server
	thirdParty *httptest.Server

	// scriptBody is served as the third-party script, so a test can change it
	// between scans to exercise digest comparison.
	scriptBody string
}

// Fixture hostnames. Both servers listen on loopback, so without distinct
// names they would be the same party — correctly, since 127.0.0.1 is one
// host. Chrome's resolver rules map these names onto the test listeners, so
// first- and third-party attribution is exercised for real.
const (
	siteHost  = "site.test"
	thirdHost = "third-party.test"
)

// siteURL and thirdURL are the URLs as the browser sees them.
func (f *fixtureSite) siteURL() string  { return "http://" + siteHost + "/" }
func (f *fixtureSite) thirdURL() string { return "http://" + thirdHost }

// resolverRules maps the fixture hostnames to the actual listeners.
func (f *fixtureSite) resolverRules() string {
	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		siteHost, hostPort(f.site.URL), thirdHost, hostPort(f.thirdParty.URL))
}

func hostPort(rawURL string) string {
	return strings.TrimPrefix(strings.TrimPrefix(rawURL, "http://"), "https://")
}

func newFixtureSite(t *testing.T, withBanner bool) *fixtureSite {
	t.Helper()

	f := &fixtureSite{scriptBody: "console.log('v1');"}

	thirdMux := http.NewServeMux()

	thirdMux.HandleFunc("/px.gif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		// A one-pixel GIF.
		_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
	})

	thirdMux.HandleFunc("/tracker.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(f.scriptBody))
	})

	f.thirdParty = httptest.NewServer(thirdMux)
	t.Cleanup(f.thirdParty.Close)

	siteMux := http.NewServeMux()

	// Chrome requests a favicon on its own, and whether that request lands
	// before network idle varies between runs. Serving it makes the fixture
	// deterministic; the variance is Chrome's, not wsaw's.
	siteMux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0x00, 0x00, 0x01, 0x00})
	})

	siteMux.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__appLoaded = true;"))
	})

	siteMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Set-Cookie", "sid=abc123; Path=/; SameSite=Lax")

		_, _ = fmt.Fprint(w, f.html(withBanner))
	})

	f.site = httptest.NewServer(siteMux)
	t.Cleanup(f.site.Close)

	return f
}

func (f *fixtureSite) html(withBanner bool) string {
	banner := ""

	if withBanner {
		// A minimal but realistic banner: a container with consent wording and
		// two labelled buttons, which is what the heuristic rule matches.
		banner = `
<div id="cookie-banner" style="position:fixed;bottom:0;left:0;width:600px;height:120px;background:#eee">
  <p>We use cookies. Please choose whether to allow cookies and tracking.</p>
  <button id="accept-all" onclick="window.__wsawConsent('accept')">Accept all</button>
  <button id="reject-all" onclick="window.__wsawConsent('reject')">Reject all</button>
</div>`
	}

	return `<!DOCTYPE html>
<html><head><title>fixture</title>
<script src="/app.js"></script>
</head>
<body>
<h1>fixture site</h1>
<img src="` + f.thirdURL() + `/px.gif" alt="">
` + banner + `
<script>
window.__wsawConsent = function (choice) {
  var b = document.getElementById('cookie-banner');
  if (b) b.remove();
  document.cookie = 'consent=' + choice + '; path=/';
  // Post-consent third-party script, loaded only after a decision.
  var s = document.createElement('script');
  s.src = '` + f.thirdURL() + `/tracker.js?choice=' + choice;
  document.head.appendChild(s);
};
</script>
</body></html>`
}

func (f *fixtureSite) target(t *testing.T, modes ...model.ConsentMode) config.Resolved {
	t.Helper()

	if len(modes) == 0 {
		modes = []model.ConsentMode{model.ConsentNone}
	}

	return config.Resolved{
		Name:         "fixture",
		URL:          f.siteURL(),
		ConsentModes: modes,
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  30 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  500,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}

func newScanner(t *testing.T, info browser.Info, site *fixtureSite) (*scanner.Scanner, func()) {
	t.Helper()

	launch := browser.Options{Info: info, LaunchTimeout: 40 * time.Second}
	if site != nil {
		launch.ExtraArgs = []string{"host-resolver-rules=" + site.resolverRules()}
	}

	pool := browser.NewPool(browser.PoolOptions{Size: 1, Launch: launch})

	normalizer, err := normalize.New(normalize.Rules{DropQueryParams: []string{"cb"}})
	if err != nil {
		t.Fatal(err)
	}

	rules, err := consent.LoadBuiltinRules()
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

func TestCaptureRecordsFirstAndThirdPartyRequests(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, false)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(t), model.ConsentNone)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if !res.OK() {
		t.Fatalf("scan not OK: termination=%s error=%s warnings=%v",
			res.Termination, res.Error, res.Warnings)
	}

	if res.Termination != model.TermIdle {
		t.Errorf("termination = %q, want idle", res.Termination)
	}

	// The document, the first-party script, and the third-party pixel.
	var sawDocument, sawAppScript, sawPixel bool

	for _, req := range res.Requests {
		switch {
		case req.ResourceType == "document" && req.Party == model.FirstParty:
			sawDocument = true
		case strings.HasSuffix(req.URL, "/app.js"):
			sawAppScript = true

			if req.Party != model.FirstParty {
				t.Errorf("app.js party = %q, want first", req.Party)
			}
		case strings.Contains(req.URL, "/px.gif"):
			sawPixel = true

			if req.Party != model.ThirdParty {
				t.Errorf("pixel party = %q, want third", req.Party)
			}

			if req.Phase != model.PhasePre {
				t.Errorf("pixel phase = %q, want pre-interaction", req.Phase)
			}
		}
	}

	if !sawDocument {
		t.Error("the main document was not recorded")
	}

	if !sawAppScript {
		t.Error("the first-party script was not recorded")
	}

	if !sawPixel {
		t.Error("the third-party pixel was not recorded")
	}

	if len(res.ThirdPartyDomains("")) == 0 {
		t.Error("no third-party domains were attributed")
	}
}

func TestCaptureHashesScriptBodies(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, false)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(t), model.ConsentNone)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, req := range out.Result.Requests {
		if !strings.HasSuffix(req.URL, "/app.js") {
			continue
		}

		// Either a digest or an explanation, never silence (Tenet 5).
		if req.BodySHA256 == "" && req.BodyUnavailable == "" {
			t.Error("script has neither a digest nor an explanation for its absence")
		}

		if req.BodySHA256 != "" && len(req.BodySHA256) != 64 {
			t.Errorf("digest = %q, want a 64-character hex string", req.BodySHA256)
		}

		return
	}

	t.Error("the first-party script was not recorded at all")
}

func TestCaptureRecordsCookies(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, false)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(t), model.ConsentNone)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, c := range out.Result.Cookies {
		if c.Name != "sid" {
			continue
		}

		// The value must never be stored, only fingerprinted.
		if c.ValueSHA256 == "" {
			t.Error("cookie has no value digest")
		}

		if c.ValueLength != len("abc123") {
			t.Errorf("value length = %d, want %d", c.ValueLength, len("abc123"))
		}

		return
	}

	t.Errorf("the first-party cookie was not recorded: %+v", out.Result.Cookies)
}

// TestConsentRejectSplitsPhases is the end-to-end version of the product's
// central claim: traffic before the banner decision is distinguishable from
// traffic after it.
func TestConsentRejectSplitsPhases(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, true)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(t, model.ConsentReject), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if !res.OK() {
		t.Fatalf("scan not OK: %s %s", res.Termination, res.Error)
	}

	// The banner was handled by the heuristic rule, which must be recorded as
	// heuristic so a reviewer can weigh it.
	if res.Consent.Outcome != model.OutcomeApplied && res.Consent.Outcome != model.OutcomeUnverified {
		t.Errorf("consent outcome = %q (%s), want applied or unverified",
			res.Consent.Outcome, res.Consent.Reason)
	}

	if res.Consent.Mechanism == "" {
		t.Error("no consent mechanism was recorded")
	}

	pre := res.ThirdPartyDomains(model.PhasePre)
	if len(pre) == 0 {
		t.Error("the pre-consent pixel was not attributed to the pre-interaction phase")
	}

	// The tracker script is injected by the click handler, so it must appear
	// in the post-interaction phase.
	var sawPostTracker bool

	for _, req := range res.Requests {
		if strings.Contains(req.URL, "/tracker.js") {
			sawPostTracker = true

			if req.Phase != model.PhasePost {
				t.Errorf("tracker.js phase = %q, want post-interaction", req.Phase)
			}

			if !strings.Contains(req.URL, "choice=reject") {
				t.Errorf("tracker URL = %q, want the reject choice", req.URL)
			}
		}
	}

	if !sawPostTracker {
		t.Error("the post-consent tracker script was not recorded; the banner may not have been clicked")
	}
}

// TestScanIsDeterministic is the property the whole product rests on: an
// unchanged site must produce an empty diff (Tenet 6).
func TestScanIsDeterministic(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, false)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := site.target(t)

	first, err := s.Scan(ctx, target, model.ConsentNone)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}

	second, err := s.Scan(ctx, target, model.ConsentNone)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}

	keys := func(res *model.Result) map[string]bool {
		out := make(map[string]bool)

		for _, req := range res.Requests {
			out[req.NormalizedURL] = true
		}

		return out
	}

	a, b := keys(first.Result), keys(second.Result)

	for k := range a {
		if !b[k] {
			t.Errorf("asset %q present in the first scan but not the second", k)
		}
	}

	for k := range b {
		if !a[k] {
			t.Errorf("asset %q present in the second scan but not the first", k)
		}
	}
}

// TestPerScanIsolation is Tenet 2: consent state must not leak between scans,
// or every comparison the product makes becomes meaningless.
func TestPerScanIsolation(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, true)
	s, closePool := newScanner(t, info, site)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	target := site.target(t, model.ConsentReject)

	// Scan twice in reject mode. If cookie state leaked, the second scan
	// would find no banner and report not-needed.
	for i := range 2 {
		out, err := s.Scan(ctx, target, model.ConsentReject)
		if err != nil {
			t.Fatalf("scan %d: %v", i+1, err)
		}

		if out.Result.Consent.Outcome == model.OutcomeNotNeeded {
			t.Errorf("scan %d found no banner; consent state leaked between scans", i+1)
		}
	}
}

func TestFailedNavigationIsRecordedNotLost(t *testing.T) {
	info := requireChrome(t)

	s, closePool := newScanner(t, info, nil)

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A port nothing listens on.
	target := config.Resolved{
		Name:         "unreachable",
		URL:          "http://127.0.0.1:1/",
		ConsentModes: []model.ConsentMode{model.ConsentNone},
		IdleQuiet:    300 * time.Millisecond,
		HardTimeout:  20 * time.Second,
		NavTimeout:   10 * time.Second,
		MaxRequests:  50,
		Robots:       config.RobotsIgnore,
	}

	out, _ := s.Scan(ctx, target, model.ConsentNone)

	if out.Result == nil {
		t.Fatal("a failed scan produced no result at all; the failure must be recorded")
	}

	if out.Result.OK() {
		t.Error("a scan of an unreachable host reported itself as OK")
	}

	if out.Result.Error == "" && out.Result.Termination == model.TermIdle {
		t.Error("the failure is not visible in the result")
	}
}

// TestNoProcessLeak covers the resource discipline a long-running daemon
// depends on (NFR §2).
func TestNoProcessLeak(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, false)

	pool := browser.NewPool(browser.PoolOptions{
		Size: 2,
		Launch: browser.Options{
			Info:          info,
			LaunchTimeout: 40 * time.Second,
			ExtraArgs:     []string{"host-resolver-rules=" + site.resolverRules()},
		},
	})

	normalizer, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	s, err := scanner.New(scanner.Deps{Pool: pool}, scanner.Options{Normalizer: normalizer})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for range 3 {
		if _, err := s.Scan(ctx, site.target(t), model.ConsentNone); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}

	if err := pool.Close(); err != nil {
		t.Fatalf("closing pool: %v", err)
	}

	if live := pool.Live(); live != 0 {
		t.Errorf("pool reports %d live browsers after Close, want 0", live)
	}

	// Profile directories must be gone too.
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Skip("cannot inspect the temp directory")
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wsaw-profile-") {
			t.Errorf("leaked browser profile directory %s", e.Name())
		}
	}
}
