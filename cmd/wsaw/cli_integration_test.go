package main

// These tests drive the CLI end to end against local fixture servers, with a
// real Chrome doing the rendering. They never touch a live site (Rule 7), and
// they skip — saying why — when no usable Chrome is present, exactly like
// internal/scanner's integration tests (Rule 1: a browser-dependent test is
// skipped, never faked).

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/share"
)

// requireChrome skips the test unless a real Chrome is discoverable. Kept as
// its own copy rather than imported from internal/scanner's test file: Go
// does not let one package's _test.go helpers be reused from another, and
// duplicating four lines beats inventing a shared test-only package for it.
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

// Fixture hostnames. Chrome's host-resolver-rules maps these onto the actual
// loopback listeners, so first- and third-party attribution happens for real
// rather than being faked.
const (
	cliSiteHost  = "wsaw-cli-site.test"
	cliThirdHost = "wsaw-cli-third.test"
	cliExtraHost = "wsaw-cli-extra.test"
)

// cliFixture serves a page with one third-party pixel that is always
// present, and a second, from a different third-party host, that only
// appears once addExtra is set. That shape is what lets one fixture serve
// both a clean baseline scan and a second scan that introduces a genuinely
// new third-party host, which is what a severity-tripping diff needs.
type cliFixture struct {
	site  *httptest.Server
	third *httptest.Server

	addExtra atomic.Bool
}

func (f *cliFixture) siteURL() string  { return "http://" + cliSiteHost + "/" }
func (f *cliFixture) thirdURL() string { return "http://" + cliThirdHost }
func (f *cliFixture) extraURL() string { return "http://" + cliExtraHost }

func (f *cliFixture) resolverRules() string {
	return fmt.Sprintf("MAP %s %s, MAP %s %s, MAP %s %s",
		cliSiteHost, cliHostPort(f.site.URL),
		cliThirdHost, cliHostPort(f.third.URL),
		cliExtraHost, cliHostPort(f.third.URL))
}

func cliHostPort(rawURL string) string {
	return strings.TrimPrefix(strings.TrimPrefix(rawURL, "http://"), "https://")
}

func cliGIFHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/gif")
	// A one-pixel GIF.
	_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
}

func newCLIFixture(t *testing.T, withBanner bool) *cliFixture {
	t.Helper()

	f := &cliFixture{}

	thirdMux := http.NewServeMux()
	thirdMux.HandleFunc("/px.gif", cliGIFHandler)
	thirdMux.HandleFunc("/extra.gif", cliGIFHandler)
	f.third = httptest.NewServer(thirdMux)
	t.Cleanup(f.third.Close)

	siteMux := http.NewServeMux()
	siteMux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0x00, 0x00, 0x01, 0x00})
	})
	siteMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, f.html(withBanner))
	})
	f.site = httptest.NewServer(siteMux)
	t.Cleanup(f.site.Close)

	return f
}

func (f *cliFixture) html(withBanner bool) string {
	extra := ""
	if f.addExtra.Load() {
		extra = `<img src="` + f.extraURL() + `/extra.gif" alt="">`
	}

	banner := ""
	if withBanner {
		banner = `
<div id="cookie-banner" style="position:fixed;bottom:0;left:0;width:600px;height:120px;background:#eee">
  <p>We use cookies. Please choose whether to allow cookies and tracking.</p>
  <button id="accept-all" onclick="window.__wsawConsent('accept')">Accept all</button>
  <button id="reject-all" onclick="window.__wsawConsent('reject')">Reject all</button>
</div>`
	}

	return `<!DOCTYPE html>
<html><head><title>cli fixture</title></head>
<body>
<h1>cli fixture</h1>
<img src="` + f.thirdURL() + `/px.gif" alt="">
` + extra + `
` + banner + `
<script>
window.__wsawConsent = function (choice) {
  var b = document.getElementById('cookie-banner');
  if (b) b.remove();
  document.cookie = 'consent=' + choice + '; path=/';
};
</script>
</body></html>`
}

// cliFixtureConfig writes a configuration file whose one target points at the
// fixture, resolved through Chrome's host-resolver-rules, with a local
// (never containerised) browser. Forcing "local" matters in this
// environment: podman is installed and responding, so leaving Runtime unset
// would send the scan through the container path, which needs an image this
// sandbox has no way to pull.
func cliFixtureConfig(t *testing.T, targetURL, resolverRules string) string {
	t.Helper()

	body := fmt.Sprintf(`targets:
  - name: fixture
    url: %s
    retryAttempts: 1
    idleQuiet: 400ms
    hardTimeout: 20s
    navTimeout: 15s
    maxRequests: 200
browser:
  runtime: local
  extraArgs: ["host-resolver-rules=%s"]
`, targetURL, resolverRules)

	return writeConfig(t, body)
}

// TestCmdScanExitCodesReflectFindingsAndOperationalFailure is the exit-code
// contract cmdScan exists to keep: 0 for a clean scan, 1 once a diff finding
// reaches the --fail-on threshold, and — the invariant that matters most —
// an operational failure (here, a target the browser could not reach at all)
// always outranks a findings exit, never silently reporting 0 or 1.
func TestCmdScanExitCodesReflectFindingsAndOperationalFailure(t *testing.T) {
	requireChrome(t)

	fx := newCLIFixture(t, false)
	path := cliFixtureConfig(t, fx.siteURL(), fx.resolverRules())

	ctx := context.Background()

	var code int

	_, stderr := capture(t, func() { code = cmdScan(ctx, []string{"--config", path}) })

	if code != exitOK {
		t.Fatalf("baseline scan exit = %d (stderr=%q), want %d", code, stderr, exitOK)
	}

	// The second scan's document adds a pixel from a third-party host that
	// was not present in the baseline: a new third-party host in reject mode
	// is the product's headline finding, and defaults to critical severity —
	// above the default --fail-on threshold of high.
	fx.addExtra.Store(true)

	_, stderr = capture(t, func() { code = cmdScan(ctx, []string{"--config", path}) })

	if code != exitFindings {
		t.Fatalf("scan with a new third-party host exit = %d (stderr=%q), want %d", code, stderr, exitFindings)
	}

	t.Run("operational failure outranks findings", func(t *testing.T) {
		unreachable := cliFixtureConfig(t, "http://127.0.0.1:1/", "")

		var code int

		_, stderr := capture(t, func() { code = cmdScan(ctx, []string{"--config", unreachable}) })

		if code != exitOperational {
			t.Fatalf("scan of an unreachable target exit = %d, want %d (operational)", code, exitOperational)
		}

		if !strings.Contains(stderr, "did not produce a trustworthy result") {
			t.Errorf("stderr = %q, want it to name the operational failure", stderr)
		}
	})
}

// TestCmdScanExitsOperationalWhenNoBrowserIsUsable needs no real Chrome: an
// explicit path that does not exist fails discovery deterministically
// wherever the test runs.
func TestCmdScanExitsOperationalWhenNoBrowserIsUsable(t *testing.T) {
	cfg := fmt.Sprintf(`targets:
  - name: fixture
    url: https://example.test/
browser:
  runtime: local
  path: %s
`, filepath.Join(t.TempDir(), "no-such-chrome"))

	path := writeConfig(t, cfg)

	var code int

	_, stderr := capture(t, func() { code = cmdScan(context.Background(), []string{"--config", path}) })

	if code != exitOperational {
		t.Fatalf("exit = %d (stderr=%q), want %d", code, stderr, exitOperational)
	}
}

// TestCmdDebugScansAFixtureAndPrintsMarkdown covers cmdDebug's success path,
// which Phase 1 left untouched (its tests only reach the flag-validation
// errors).
func TestCmdDebugScansAFixtureAndPrintsMarkdown(t *testing.T) {
	requireChrome(t)

	fx := newCLIFixture(t, false)

	var (
		stdout, stderr string
		runErr         error
	)

	stdout, stderr = capture(t, func() {
		runErr = cmdDebug(context.Background(), []string{
			"--browser-runtime", "local",
			"--consent-mode", "none",
			"--timeout", "20s",
			fx.siteURL(),
		})
	})

	if runErr != nil {
		t.Fatalf("cmdDebug: %v (stderr=%q)", runErr, stderr)
	}

	if !strings.Contains(stdout, "cli fixture") && !strings.Contains(stdout, fx.siteURL()) {
		t.Errorf("stdout does not look like a report of the fixture scan: %q", stdout)
	}

	if !strings.Contains(stderr, "scanning "+fx.siteURL()) {
		t.Errorf("stderr = %q, want the scanning announcement", stderr)
	}
}

// TestCmdShareEndToEndMintsAVerifiableLink needs no browser at all: sharing
// only reads the store and signs a token (app.New is built with
// RequireBrowser left false), so a store seeded directly is enough to
// exercise the whole command.
func TestCmdShareEndToEndMintsAVerifiableLink(t *testing.T) {
	path := writeConfig(t, oneTargetConfig+fmt.Sprintf(`
api:
  share:
    enabled: true
    key: %q
`, shareKey))

	seedStore(t, storePath(path), "scan-1")

	var runErr error

	stdout, stderr := capture(t, func() {
		runErr = cmdShare(context.Background(), []string{
			"--config", path,
			"--target", "site",
			"--mode", "reject",
			"--scan", "latest",
		})
	})

	if runErr != nil {
		t.Fatalf("cmdShare: %v (stderr=%q)", runErr, stderr)
	}

	link := strings.TrimSpace(stdout)

	idx := strings.Index(link, "?t=")
	if idx == -1 {
		t.Fatalf("link has no token: %q", link)
	}

	token := link[idx+len("?t="):]

	verifier, err := share.New(share.Options{Key: shareKey, Validity: time.Hour, MaxValidity: 24 * time.Hour})
	if err != nil {
		t.Fatalf("building a verifier: %v", err)
	}

	claims, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("the link cmdShare printed does not verify: %v", err)
	}

	if !claims.Allows("site", "reject", "scan-1") {
		t.Errorf("claims = %+v, want them to allow site/reject/scan-1", claims)
	}

	if !strings.Contains(stderr, "cannot be revoked") {
		t.Errorf("stderr = %q, want the no-revocation warning", stderr)
	}
}

// syncBuffer is a mutex-guarded strings.Builder: supervise logs from several
// goroutines (the scheduler, the reload handler, the prune loop), so a plain
// strings.Builder would race.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// TestSuperviseReloadsOnSIGHUPAndShutsDownOnContextCancel is the one test
// Phase 3 exists for: supervise (113 statements) had no coverage at all
// before this, and it is where a leaked goroutine or a hung shutdown would
// live. It drives the real signal path — SIGHUP is delivered to this test
// process, exactly as an operator's `kill -HUP` would be — and then cancels
// the context supervise was given, standing in for the SIGTERM main() turns
// into cancellation.
func TestSuperviseReloadsOnSIGHUPAndShutsDownOnContextCancel(t *testing.T) {
	requireChrome(t)

	fx := newCLIFixture(t, false)
	path := cliFixtureConfig(t, fx.siteURL(), fx.resolverRules())

	cf := parseFlags(t, "--config", path)

	cfg, err := cf.load()
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, err := app.New(ctx, cfg, app.Options{Version: "test", RequireBrowser: true})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	logged := &syncBuffer{}
	a.Logger = slog.New(slog.NewTextHandler(logged, nil))

	done := make(chan error, 1)

	go func() { done <- supervise(ctx, a, *cf) }()

	// Adding a second target is a reload the running process can apply
	// without a restart (only the target list is reloadable).
	updatedBody := fmt.Sprintf(`targets:
  - name: fixture
    url: %s
    retryAttempts: 1
    idleQuiet: 400ms
    hardTimeout: 20s
    navTimeout: 15s
    maxRequests: 200
  - name: fixture2
    url: %s
    retryAttempts: 1
browser:
  runtime: local
  extraArgs: ["host-resolver-rules=%s"]
`, fx.siteURL(), fx.siteURL(), fx.resolverRules())

	body := updatedBody + "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewriting the configuration: %v", err)
	}

	// Until supervise reaches its own signal.Notify(hup, syscall.SIGHUP), this
	// process has no handler for SIGHUP at all, and the OS default action for
	// an unhandled SIGHUP is to terminate it — so sending one immediately
	// would kill the whole test binary rather than reach supervise. This
	// warm-up is generous next to how little supervise does before Notify
	// (build a dispatcher, start two goroutines): it is the one sleep in this
	// test that is not polling for a condition, because there is nothing
	// observable to poll for it against.
	time.Sleep(300 * time.Millisecond)

	// SIGHUP is delivered asynchronously, so this resends it while polling
	// for the reload to land — the same bounded-poll shape internal/daemon's
	// own tests use for asynchronous state, not a fixed sleep hoping
	// something happened by then.
	var reloaded bool

	for range 40 {
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		time.Sleep(50 * time.Millisecond)

		if strings.Contains(logged.String(), "configuration reloaded") {
			reloaded = true

			break
		}
	}

	if !reloaded {
		t.Fatalf("SIGHUP never reloaded the configuration; log so far: %s", logged.String())
	}

	if strings.Contains(logged.String(), "reload rejected") {
		t.Errorf("a valid reload was rejected: %s", logged.String())
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("supervise returned %v after its context was cancelled, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("supervise did not shut down after its context was cancelled")
	}
}

// TestSuperviseRejectsAReloadItCannotApply is the regression the running
// configuration's integrity depends on: a change that is not just the target
// list must be refused, leaving the running process exactly as it was
// (cmd/wsaw/run_build_test.go asserts the same for reload() in isolation;
// this drives it through the real signal path instead).
func TestSuperviseRejectsAReloadItCannotApply(t *testing.T) {
	requireChrome(t)

	fx := newCLIFixture(t, false)
	path := cliFixtureConfig(t, fx.siteURL(), fx.resolverRules())

	cf := parseFlags(t, "--config", path)

	cfg, err := cf.load()
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, err := app.New(ctx, cfg, app.Options{Version: "test", RequireBrowser: true})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	logged := &syncBuffer{}
	a.Logger = slog.New(slog.NewTextHandler(logged, nil))

	done := make(chan error, 1)

	go func() { done <- supervise(ctx, a, *cf) }()

	badBody := fmt.Sprintf(`targets:
  - name: fixture
    url: %s
scheduler:
  concurrency: 9
browser:
  runtime: local
  extraArgs: ["host-resolver-rules=%s"]
`, fx.siteURL(), fx.resolverRules())

	body := badBody + "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewriting the configuration: %v", err)
	}

	// See the identical warm-up in TestSuperviseReloadsOnSIGHUPAndShutsDownOnContextCancel:
	// without it, a SIGHUP sent before supervise's signal.Notify registers
	// kills this process instead of reaching supervise.
	time.Sleep(300 * time.Millisecond)

	var rejected bool

	for range 40 {
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		time.Sleep(50 * time.Millisecond)

		if strings.Contains(logged.String(), "reload rejected") {
			rejected = true

			break
		}
	}

	if !rejected {
		t.Fatalf("SIGHUP with an unreloadable change was not rejected; log so far: %s", logged.String())
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("supervise returned %v after its context was cancelled, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("supervise did not shut down after its context was cancelled")
	}
}
