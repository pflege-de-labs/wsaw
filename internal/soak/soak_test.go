//go:build soak

// Package soak holds the long-running stability test.
//
// It is behind a build tag and run on its own schedule, separate from the
// regular suite (Story 6.8), because it takes minutes and its failure mode —
// slow resource growth — is not something a pull request should wait on.
//
//	make soak
package soak

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// TestSoak scans continuously and asserts that nothing accumulates.
//
// The failure this guards against is the one that makes a watcher useless in
// production: it works for a week, then the host runs out of memory, file
// descriptors, or disk, and the watching stops (NFR §2).
func TestSoak(t *testing.T) {
	duration := 5 * time.Minute
	if v := os.Getenv("WSAW_SOAK_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("WSAW_SOAK_DURATION: %v", err)
		}

		duration = d
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration+2*time.Minute)
	defer cancel()

	info, err := browser.Discover(ctx, os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("no usable Chrome: %v", err)
	}

	site := newSite(t)

	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{Path: dir + "/soak.db", ArtifactDir: dir + "/artifacts"})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	// A private profile parent, so leftover-profile accounting is about this
	// pool rather than about whatever else the machine is running.
	profileDir := t.TempDir()

	pool := browser.NewPool(browser.PoolOptions{
		Size: 2,
		// Deliberately low, so recycling is exercised many times over.
		MaxScansPerBrowser: 5,
		Launch: browser.Options{
			Info:          info,
			LaunchTimeout: 60 * time.Second,
			ProfileDir:    profileDir,
			ExtraArgs:     []string{"host-resolver-rules=" + site.resolverRules()},
		},
	})

	normalizer, err := normalize.New(normalize.Rules{DropQueryParams: normalize.DefaultDropQueryParams})
	if err != nil {
		t.Fatal(err)
	}

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, err := scanner.New(
		scanner.Deps{Pool: pool, Store: st, Rules: rules},
		scanner.Options{Normalizer: normalizer, AllowHeuristicConsent: true, WsawVersion: "soak"},
	)
	if err != nil {
		t.Fatal(err)
	}

	target := site.target()

	// Baseline measurements after a warm-up, so one-off allocations from
	// starting the first browser do not read as a leak.
	for range 4 {
		if _, err := s.Scan(ctx, target, model.ConsentReject); err != nil {
			t.Fatalf("warm-up scan: %v", err)
		}
	}

	baseline := measure(t, profileDir)

	t.Logf("baseline: %s", baseline)

	deadline := time.Now().Add(duration)

	scans := 0

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}

		mode := model.ConsentReject
		if scans%2 == 0 {
			mode = model.ConsentNone
		}

		if _, err := s.Scan(ctx, target, mode); err != nil {
			t.Errorf("scan %d: %v", scans, err)
		}

		scans++

		if scans%10 == 0 {
			t.Logf("after %d scans: %s", scans, measure(t, profileDir))
		}
	}

	if scans < 10 {
		t.Fatalf("only %d scans completed; the soak test needs a working browser", scans)
	}

	if err := pool.Close(); err != nil {
		t.Errorf("closing pool: %v", err)
	}

	final := measure(t, profileDir)

	t.Logf("after %d scans: %s", scans, final)

	// Goroutines are the sharpest signal: a leak here means a scan is not
	// releasing something on every path.
	if grown := final.goroutines - baseline.goroutines; grown > 20 {
		t.Errorf("goroutines grew by %d over %d scans (from %d to %d)",
			grown, scans, baseline.goroutines, final.goroutines)
	}

	// Heap is noisier, so the bound is proportional rather than absolute.
	if final.heapMB > baseline.heapMB*3+64 {
		t.Errorf("heap grew from %d MiB to %d MiB over %d scans",
			baseline.heapMB, final.heapMB, scans)
	}

	if final.profileDirs != 0 {
		t.Errorf("%d browser profile directories remain after shutdown", final.profileDirs)
	}

	if live := pool.Live(); live != 0 {
		t.Errorf("%d browsers still live after Close", live)
	}
}

type snapshot struct {
	goroutines  int
	heapMB      uint64
	profileDirs int
}

func (s snapshot) String() string {
	return fmt.Sprintf("goroutines=%d heap=%dMiB profile_dirs=%d", s.goroutines, s.heapMB, s.profileDirs)
}

func measure(t *testing.T, profileDir string) snapshot {
	t.Helper()

	runtime.GC()

	var m runtime.MemStats

	runtime.ReadMemStats(&m)

	return snapshot{
		goroutines:  runtime.NumGoroutine(),
		heapMB:      m.HeapAlloc / (1 << 20),
		profileDirs: countProfileDirs(profileDir),
	}
}

// countProfileDirs counts leftover browser profiles, which is how a leaked
// temp directory shows up.
func countProfileDirs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	n := 0

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wsaw-profile-") {
			n++
		}
	}

	return n
}

// site is a fixture with a consent banner and a third-party host, so the soak
// exercises the full path rather than a trivial page.
type site struct {
	main  *httptest.Server
	third *httptest.Server
}

const (
	siteHost  = "soak-site.test"
	thirdHost = "soak-third.test"
)

func newSite(t *testing.T) *site {
	t.Helper()

	s := &site{}

	thirdMux := http.NewServeMux()
	thirdMux.HandleFunc("/t.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__t = Date.now();"))
	})
	thirdMux.HandleFunc("/px.gif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a"))
	})

	s.third = httptest.NewServer(thirdMux)
	t.Cleanup(s.third.Close)

	mainMux := http.NewServeMux()
	mainMux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0, 0, 1, 0})
	})
	mainMux.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__app = true;"))
	})
	mainMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Set-Cookie", "sid=soak; Path=/")

		_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>soak</title><script src="/app.js"></script></head>
<body><h1>soak</h1><img src="http://%s/px.gif" alt="">
<div id="banner" style="position:fixed;bottom:0;width:600px;height:120px">
<p>We use cookies and tracking.</p>
<button onclick="pick('accept')">Accept all</button>
<button onclick="pick('reject')">Reject all</button></div>
<script>function pick(c){document.getElementById('banner').remove();
var s=document.createElement('script');s.src='http://%s/t.js?c='+c;document.head.appendChild(s);}</script>
</body></html>`, thirdHost, thirdHost)
	})

	s.main = httptest.NewServer(mainMux)
	t.Cleanup(s.main.Close)

	return s
}

func (s *site) resolverRules() string {
	strip := func(u string) string { return strings.TrimPrefix(u, "http://") }

	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		siteHost, strip(s.main.URL), thirdHost, strip(s.third.URL))
}

func (s *site) target() config.Resolved {
	return config.Resolved{
		Name:         "soak",
		URL:          "http://" + siteHost + "/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		IdleQuiet:    750 * time.Millisecond,
		HardTimeout:  30 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}
