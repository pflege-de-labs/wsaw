package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// newHangingCCM19Site serves the CCM19 fixture plus an image that never
// finishes, so the page's load event never fires and navigation runs out its
// budget. A third-party origin behind a slow proxy did exactly this to a real
// scan: 180 images never completed, the consent hook never ran, and the
// result still read "not-needed".
func newHangingCCM19Site(t *testing.T) *ccm19Site {
	t.Helper()

	release := make(chan struct{})

	s := &ccm19Site{}

	s.third = httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.third.Close)

	siteMux := http.NewServeMux()
	siteMux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0, 0, 1, 0})
	})

	siteMux.HandleFunc("/hang.png", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	siteMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := strings.Replace(ccm19FixtureHTML(), "<h1>fixture</h1>", `<h1>fixture</h1><img src="/hang.png">`, 1)
		_, _ = fmt.Fprint(w, page)
	})

	s.site = httptest.NewServer(siteMux)
	t.Cleanup(s.site.Close)

	// Registered after the server's Close, so it runs first: Close waits for
	// the hanging handler.
	t.Cleanup(func() { close(release) })

	return s
}

// TestConsentNeverAttemptedIsNotNotNeeded is the regression for a scan whose
// navigation timed out before the consent hook ran. The banner was on screen;
// "not-needed" would claim the site never asked for consent (Tenet 5).
func TestConsentNeverAttemptedIsNotNotNeeded(t *testing.T) {
	info := requireChrome(t)

	site := newHangingCCM19Site(t)

	s, closePool := newScannerFor(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target := site.target()
	target.ConsentModes = []model.ConsentMode{model.ConsentAccept}
	target.NavTimeout = 3 * time.Second
	target.HardTimeout = 10 * time.Second

	out, err := s.Scan(ctx, target, model.ConsentAccept)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if res.Termination != model.TermTimeout {
		t.Fatalf("termination = %q, want timeout: the fixture must stop navigation", res.Termination)
	}

	if res.Consent.Outcome != model.OutcomeFailed {
		t.Errorf("outcome = %q (%q), want failed: consent was never attempted",
			res.Consent.Outcome, res.Consent.Reason)
	}

	if res.Consent.Reason == "" {
		t.Error("a consent step that never ran must say why")
	}
}
