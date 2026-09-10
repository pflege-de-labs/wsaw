package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A fixture implementing CCM19's window.CCM API, used to prove the shipped
// rule drives it correctly.
//
// CCM19 (papoo Software) documents no public JS API reference; the method
// names below — grantAllPrivileges, revokeAllPrivileges, the consent getter —
// were confirmed by reading the vendor's shipped app.js, and are reproduced
// here rather than assumed.

const (
	ccm19SiteHost  = "ccm19-site.test"
	ccm19ThirdHost = "ccm19-third.test"
)

type ccm19Site struct {
	site  *httptest.Server
	third *httptest.Server
}

func newCCM19Site(t *testing.T) *ccm19Site {
	t.Helper()

	s := &ccm19Site{}

	thirdMux := http.NewServeMux()
	thirdMux.HandleFunc("/tag.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__tag = true;"))
	})

	s.third = httptest.NewServer(thirdMux)
	t.Cleanup(s.third.Close)

	siteMux := http.NewServeMux()
	siteMux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0, 0, 1, 0})
	})

	siteMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, ccm19FixtureHTML())
	})

	s.site = httptest.NewServer(siteMux)
	t.Cleanup(s.site.Close)

	return s
}

func (s *ccm19Site) resolverRules() string {
	strip := func(u string) string { return strings.TrimPrefix(u, "http://") }

	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		ccm19SiteHost, strip(s.site.URL), ccm19ThirdHost, strip(s.third.URL))
}

func (s *ccm19Site) target() config.Resolved {
	return config.Resolved{
		Name:         "ccm19-fixture",
		URL:          "http://" + ccm19SiteHost + "/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}

// ccm19FixtureHTML reproduces the API surface confirmed by reading the real
// vendor script: a window.CCM object exposing grantAllPrivileges,
// revokeAllPrivileges and a consent getter, plus the button markup the rule
// falls back to when the API is absent — the accept-all button shares the
// "ccm--save-settings" class with the plain "save selection" button and is
// distinguished only by data-full-consent="true".
func ccm19FixtureHTML() string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>ccm19 fixture</title></head>
<body>
<h1>fixture</h1>

<div id="ccm-root">
  <div class="ccm-modal">
    <p>We use cookies. Please choose whether to allow tracking.</p>
    <button class="ccm--save-settings" data-full-consent="true">Accept all</button>
    <button class="ccm--decline-cookies">Decline</button>
  </div>
</div>

<script>
(function () {
  var consentGiven = false;
  var choice = null;

  function record(which) {
    choice = which;
    consentGiven = true;
    document.getElementById('ccm-root').remove();
    if (which === 'accept') {
      var s = document.createElement('script');
      s.src = 'http://%s/tag.js?c=accept';
      document.head.appendChild(s);
    }
  }

  window.CCM = Object.create(null, {
    consent: { get: function () { return consentGiven; }, enumerable: true },
    grantAllPrivileges: { value: function () { record('accept'); }, enumerable: true },
    revokeAllPrivileges: { value: function () { record('reject'); }, enumerable: true },
  });

  window.__wsawTestChoice = function () { return choice; };
})();
</script>
</body></html>`, ccm19ThirdHost)
}

// TestCCM19RejectUsesTheVendorAPI is the point of the rule: the choice is
// expressed through window.CCM.revokeAllPrivileges rather than a button
// click, and verified by reading window.CCM.consent back.
func TestCCM19RejectUsesTheVendorAPI(t *testing.T) {
	info := requireChrome(t)

	site := newCCM19Site(t)

	s, closePool := newScannerFor(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if !res.OK() {
		t.Fatalf("scan not OK: %s %s", res.Termination, res.Error)
	}

	c := res.Consent

	if c.Outcome != model.OutcomeApplied {
		t.Errorf("outcome = %q (%s), want applied", c.Outcome, c.Reason)
	}

	if c.CMP != "CCM19" {
		t.Errorf("CMP = %q, want CCM19", c.CMP)
	}

	if c.Detection != "rule:ccm19" {
		t.Errorf("detection = %q, want the ccm19 rule", c.Detection)
	}

	if c.Mechanism != "vendor-api" {
		t.Errorf("mechanism = %q, want vendor-api", c.Mechanism)
	}

	if c.Heuristic {
		t.Error("a documented vendor API was reported as a heuristic match")
	}

	for _, req := range res.Requests {
		if strings.Contains(req.URL, "/tag.js") {
			t.Errorf("the accept-only tag was loaded in reject mode: %s", req.URL)
		}
	}
}

// TestCCM19AcceptLoadsTheTag proves the accept path expresses a genuinely
// different choice, attributed to the post-consent phase.
func TestCCM19AcceptLoadsTheTag(t *testing.T) {
	info := requireChrome(t)

	site := newCCM19Site(t)

	s, closePool := newScannerFor(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	target := site.target()
	target.ConsentModes = []model.ConsentMode{model.ConsentAccept}

	out, err := s.Scan(ctx, target, model.ConsentAccept)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if res.Consent.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied", res.Consent.Outcome, res.Consent.Reason)
	}

	var sawTag bool

	for _, req := range res.Requests {
		if !strings.Contains(req.URL, "/tag.js") {
			continue
		}

		sawTag = true

		if req.Phase != model.PhasePost {
			t.Errorf("tag phase = %q, want post-interaction", req.Phase)
		}

		if req.Party != model.ThirdParty {
			t.Errorf("tag party = %q, want third", req.Party)
		}
	}

	if !sawTag {
		t.Error("accepting did not load the tag; grantAllPrivileges may not have been applied")
	}
}

// TestCCM19FallsBackToClickWhenAPIIsAbsent checks the click fallback: a page
// carrying CCM19's markup but no window.CCM at all must still be handled
// through the documented button classes.
func TestCCM19FallsBackToClickWhenAPIIsAbsent(t *testing.T) {
	info := requireChrome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><head><title>ccm19 no-api fixture</title></head>
<body>
<div id="ccm-root">
  <div class="ccm-modal">
    <button class="ccm--save-settings" data-full-consent="true" onclick="document.getElementById('ccm-root').remove()">Accept all</button>
    <button class="ccm--decline-cookies" onclick="document.getElementById('ccm-root').remove()">Decline</button>
  </div>
</div>
</body></html>`)
	}))
	defer srv.Close()

	s, closePool := newScannerFor(t, info, "")
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	target := config.Resolved{
		Name:         "ccm19-no-api",
		URL:          srv.URL + "/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		IdleQuiet:    time.Second,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  100,
		Robots:       config.RobotsIgnore,
	}

	out, err := s.Scan(ctx, target, model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if out.Result.Consent.CMP != "CCM19" {
		t.Errorf("CMP = %q, want CCM19", out.Result.Consent.CMP)
	}

	if out.Result.Consent.Outcome != model.OutcomeApplied {
		t.Errorf("outcome = %q (%s), want applied via the click fallback",
			out.Result.Consent.Outcome, out.Result.Consent.Reason)
	}
}
