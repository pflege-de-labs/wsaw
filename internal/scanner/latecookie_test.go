package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A tag that reports on the way out: when its page is closed it sends a
// keepalive beacon, and the beacon's response sets a cookie.
//
// That response arrives after the tab that sent it is gone, so no scan
// records the request — and on a browser whose tabs share one cookie jar, it
// lands in whichever scan is running by then. A pflege.de reject scan
// reported Bing's MUID and MR as new critical cookies this way, written by
// the unload beacon of the accept scan that had run just before it on the
// same browser, with no request to bing.com anywhere in its own result.
// Clearing the jar when a scan starts cannot catch it: the cookie is written
// after the clear.
const (
	leaverHost    = "leaver.test"
	bystanderHost = "bystander.test"
	lateCookie    = "late_beacon"
	ownCookie     = "bystander_own"
)

type lateBeaconSites struct {
	leaver    *httptest.Server
	bystander *httptest.Server

	// bystanderLoaded is closed when the second scan requests its document,
	// which is after that scan has prepared its browser state. The beacon is
	// answered only then, so its cookie is written into a scan that is
	// already running, every time, without depending on how fast the first
	// tab closes.
	bystanderLoaded chan struct{}
	loadedOnce      sync.Once

	beaconAnswered atomic.Bool
}

func newLateBeaconSites(t *testing.T) *lateBeaconSites {
	t.Helper()

	s := &lateBeaconSites{bystanderLoaded: make(chan struct{})}

	leaverMux := http.NewServeMux()

	leaverMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html>
<html><head><title>leaver</title></head>
<body>
<h1>leaver</h1>
<script>
addEventListener('pagehide', function () {
  navigator.sendBeacon('/beacon', 'bye');
});
</script>
</body></html>`)
	})

	leaverMux.HandleFunc("/beacon", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-s.bystanderLoaded:
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			// Answered regardless, so a scan that never reaches the
			// bystander cannot hang the test; the assertion on
			// beaconAnswered says whether the fixture did its job.
		}

		http.SetCookie(w, &http.Cookie{
			Name: lateCookie, Value: "1", Path: "/",
			MaxAge: 3600, SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusNoContent)

		s.beaconAnswered.Store(true)
	})

	s.leaver = httptest.NewServer(leaverMux)
	t.Cleanup(s.leaver.Close)

	bystanderMux := http.NewServeMux()

	bystanderMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)

			return
		}

		s.loadedOnce.Do(func() { close(s.bystanderLoaded) })

		// The bystander's own cookie, so an empty cookie list — capture that
		// read nothing — cannot pass for isolation.
		http.SetCookie(w, &http.Cookie{
			Name: ownCookie, Value: "1", Path: "/",
			MaxAge: 3600, SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html>
<html><head><title>bystander</title></head>
<body><h1>bystander</h1></body></html>`)
	})

	s.bystander = httptest.NewServer(bystanderMux)
	t.Cleanup(s.bystander.Close)

	return s
}

func (s *lateBeaconSites) resolverRules() string {
	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		leaverHost, hostPort(s.leaver.URL), bystanderHost, hostPort(s.bystander.URL))
}

func lateBeaconTarget(t *testing.T, name, host string) config.Resolved {
	t.Helper()

	return config.Resolved{
		Name:         name,
		URL:          "http://" + host + "/",
		ConsentModes: []model.ConsentMode{model.ConsentNone},
		IdleQuiet:    2 * time.Second,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}

// TestLateBeaconCookieStaysWithItsScan is Story 1.5 AC1 for state that
// arrives after its scan has ended: a cookie set by the response to a
// previous scan's unload beacon must not appear in the next scan on the same
// browser.
func TestLateBeaconCookieStaysWithItsScan(t *testing.T) {
	info := requireChrome(t)

	sites := newLateBeaconSites(t)

	// One browser, no recycling, so both scans share a Chrome process — the
	// configuration the leak needs.
	s, closePool := newScannerFor(t, info, sites.resolverRules())

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := s.Scan(ctx, lateBeaconTarget(t, "leaver", leaverHost), model.ConsentNone); err != nil {
		t.Fatalf("leaver scan: %v", err)
	}

	second, err := s.Scan(ctx, lateBeaconTarget(t, "bystander", bystanderHost), model.ConsentNone)
	if err != nil {
		t.Fatalf("bystander scan: %v", err)
	}

	if !second.Result.Environment.BrowserReused {
		t.Fatal("the second scan did not reuse the first scan's browser; this test cannot see the leak")
	}

	if !sites.beaconAnswered.Load() {
		t.Fatal("the leaver's unload beacon never reached the fixture; the test is not exercising a late cookie")
	}

	sawOwn := false

	for _, c := range second.Result.Cookies {
		switch c.Name {
		case ownCookie:
			sawOwn = true
		case lateCookie:
			t.Errorf("the bystander scan reported cookie %q for %s, set by the previous scan's unload beacon",
				c.Name, c.Domain)
		}
	}

	if !sawOwn {
		t.Errorf("the bystander scan did not record its own cookie %q, so its cookie list proves nothing: %+v (warnings %v)",
			ownCookie, second.Result.Cookies, second.Result.Warnings)
	}
}
