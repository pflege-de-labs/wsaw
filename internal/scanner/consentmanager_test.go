package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/config"
	"github.com/martint17r/wsaw/internal/model"
)

// A fixture implementing Consentmanager's documented __cmp API, used to prove
// the shipped rule drives it correctly.
//
//	https://www.consentmanager.net/en/help/developer-reference/javascript-api/
//	https://www.consentmanager.net/en/help/developer-reference/cmp-events/
//
// The fixture reproduces the behaviour that makes this CMP easy to get wrong:
// __cmp exists as a stub immediately, but queues calls until the CMP has
// initialised. A rule that acts against the stub appears to succeed and
// changes nothing, so the fixture must model that rather than being ready at
// once.

const cmpInitDelayMs = 400

type cmpSite struct {
	site  *httptest.Server
	third *httptest.Server

	// neverIdle makes the page poll forever, so the network never goes quiet.
	neverIdle bool
}

const (
	cmpSiteHost  = "cmp-site.test"
	cmpThirdHost = "cmp-third.test"
)

func newCMPSite(t *testing.T) *cmpSite {
	t.Helper()

	return newCMPSiteWith(t, false)
}

// newCMPSiteWith optionally adds an endless poller, modelling the many real
// sites — analytics heartbeats, long-polling, players — that never reach
// network idle.
func newCMPSiteWith(t *testing.T, neverIdle bool) *cmpSite {
	t.Helper()

	s := &cmpSite{neverIdle: neverIdle}

	thirdMux := http.NewServeMux()
	thirdMux.HandleFunc("/px.gif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a"))
	})
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
	siteMux.HandleFunc("/poll", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	siteMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, cmpFixtureHTML(s.neverIdle))
	})

	s.site = httptest.NewServer(siteMux)
	t.Cleanup(s.site.Close)

	return s
}

func (s *cmpSite) resolverRules() string {
	strip := func(u string) string { return strings.TrimPrefix(u, "http://") }

	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		cmpSiteHost, strip(s.site.URL), cmpThirdHost, strip(s.third.URL))
}

func (s *cmpSite) target() config.Resolved {
	return config.Resolved{
		Name:         "cmp-fixture",
		URL:          "http://" + cmpSiteHost + "/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}

// cmpFixtureHTML models the documented API surface: a stub that queues, an
// init after a delay, setConsent(0|1), consentStatus, close, and the
// addEventListener events wsaw's readiness check subscribes to.
func cmpFixtureHTML(neverIdle bool) string {
	poller := ""
	if neverIdle {
		// Keeps a request in flight indefinitely, so settle() can never
		// report the network quiet.
		poller = `<script>setInterval(function(){fetch('/poll?t=' + Date.now());}, 250);</script>`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><title>consentmanager fixture</title></head>
<body>
<h1>fixture</h1>
<img src="http://%s/px.gif" alt="">

<div id="cmpbox" style="position:fixed;bottom:0;width:600px;height:140px;background:#eee">
  <p>We use cookies. Please choose whether to allow tracking.</p>
  <button id="cmpwelcomebtnyes" class="cmpboxbtn cmpboxbtnyes">Accept all</button>
  <button id="cmpwelcomebtnno" class="cmpboxbtn cmpboxbtnno">Reject all</button>
</div>

<script>
(function () {
  var ready = false;
  var queue = [];
  var listeners = {};
  var consentExists = false;
  var consentData = '';
  var choice = null;

  function fire(name) {
    (listeners[name] || []).forEach(function (fn) {
      try { fn(name, null, null); } catch (e) {}
    });
  }

  function handle(command, parameter) {
    switch (command) {
      case 'consentStatus':
        return {consentExists: consentExists, consentData: consentData};

      case 'setConsent':
        choice = parameter === 1 ? 'accept' : 'reject';
        consentExists = true;
        consentData = 'CM-' + choice;
        // A real CMP only loads tagging once a choice permits it. The
        // fixture loads a third-party tag on accept, so a test can tell the
        // two choices apart by their network effect.
        if (parameter === 1) {
          var s = document.createElement('script');
          s.src = 'http://%s/tag.js?c=accept';
          document.head.appendChild(s);
        }
        fire('consent');
        fire(parameter === 1 ? 'consentapproved' : 'consentrejected');
        return true;

      case 'close':
        var box = document.getElementById('cmpbox');
        if (box) box.remove();
        fire('consentscreenoff');
        return true;

      case 'addEventListener':
        var name = parameter[0], fn = parameter[1];
        listeners[name] = listeners[name] || [];
        listeners[name].push(fn);
        return true;

      case 'getCMPData':
        return {consentExists: consentExists, consentstring: consentData, gdprApplies: true};

      case 'ping':
        return false;

      default:
        return null;
    }
  }

  // Before initialisation __cmp queues rather than acting, which is the
  // behaviour a rule must wait out.
  window.__cmp = function (command, parameter, callback) {
    if (!ready) {
      queue.push([command, parameter, callback]);
      return undefined;
    }
    var result = handle(command, parameter);
    if (typeof callback === 'function') callback(result, true);
    return result;
  };

  window.setTimeout(function () {
    ready = true;
    fire('init');
    fire('settings');
    queue.forEach(function (item) {
      var result = handle(item[0], item[1]);
      if (typeof item[2] === 'function') item[2](result, true);
    });
    queue = [];
  }, %d);

  window.__wsawTestChoice = function () { return choice; };
})();
</script>
%s
</body></html>`, cmpThirdHost, cmpThirdHost, cmpInitDelayMs, poller)
}

// TestConsentmanagerRejectUsesTheVendorAPI is the point of the rule: the
// choice is expressed through the documented API and verified by reading it
// back, rather than inferred from a banner disappearing.
func TestConsentmanagerRejectUsesTheVendorAPI(t *testing.T) {
	info := requireChrome(t)

	site := newCMPSite(t)

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
		t.Errorf("outcome = %q (%s), want applied — the API read-back should verify the choice",
			c.Outcome, c.Reason)
	}

	if c.CMP != "Consentmanager" {
		t.Errorf("CMP = %q, want Consentmanager", c.CMP)
	}

	if c.Detection != "rule:consentmanager" {
		t.Errorf("detection = %q, want the consentmanager rule", c.Detection)
	}

	// The vendor API is stronger evidence than a click, and the result must
	// say which was used.
	if c.Mechanism != "vendor-api" {
		t.Errorf("mechanism = %q, want vendor-api", c.Mechanism)
	}

	if c.Heuristic {
		t.Error("a documented vendor API was reported as a heuristic match")
	}

	// Rejecting must not load the tag the fixture only loads on accept.
	for _, req := range res.Requests {
		if strings.Contains(req.URL, "/tag.js") {
			t.Errorf("the accept-only tag was loaded in reject mode: %s", req.URL)
		}
	}
}

// TestConsentmanagerAcceptLoadsTheTag proves the accept path expresses a
// genuinely different choice, and that the resulting traffic is attributed to
// the post-consent phase.
func TestConsentmanagerAcceptLoadsTheTag(t *testing.T) {
	info := requireChrome(t)

	site := newCMPSite(t)

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
		t.Error("accepting did not load the tag; setConsent(1) may not have been applied")
	}
}

// TestConsentmanagerWaitsForInitialisation is the failure this rule exists to
// avoid: __cmp accepts calls immediately but queues them until the CMP is
// ready, so acting too early looks like success and changes nothing.
func TestConsentmanagerWaitsForInitialisation(t *testing.T) {
	info := requireChrome(t)

	site := newCMPSite(t)

	s, closePool := newScannerFor(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(), model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// The fixture only records a choice once initialised. An "applied"
	// outcome therefore proves the rule waited rather than firing at the stub
	// and reporting success.
	if out.Result.Consent.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s): the rule did not wait for the CMP to initialise",
			out.Result.Consent.Outcome, out.Result.Consent.Reason)
	}

	if out.Result.Consent.InteractedAt == nil {
		t.Error("no interaction time was recorded")
	}
}

// TestConsentmanagerBannerRemainsWhenApiIsUnreachable checks the honest
// outcome: with no API and no matching markup, the rule must not claim to
// have applied anything.
func TestConsentmanagerBannerRemainsWhenApiIsUnreachable(t *testing.T) {
	info := requireChrome(t)

	// A page carrying consentmanager's markup but no __cmp implementation at
	// all, and buttons that do nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><head><title>inert</title></head><body>
<div id="cmpbox" style="position:fixed;bottom:0;width:600px;height:140px;background:#eee">
<p>We use cookies and tracking.</p>
<button id="cmpwelcomebtnno" class="cmpboxbtn cmpboxbtnno">Reject all</button>
</div></body></html>`)
	}))
	defer srv.Close()

	s, closePool := newScannerFor(t, info, "")
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	target := config.Resolved{
		Name:         "inert-cmp",
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

	// The banner is still on the page, so the outcome must not be "applied".
	// Whether it reports failed or unverified depends on which rule matched;
	// what matters is that it does not claim success.
	if out.Result.Consent.Outcome == model.OutcomeApplied {
		t.Errorf("reported applied while the banner is still present: %s", out.Result.Consent.Reason)
	}

	if out.Result.Consent.Reason == "" {
		t.Error("no reason recorded for a non-applied outcome")
	}
}

// TestConsentRunsOnAPageThatNeverGoesIdle is the regression test for a bug
// found by scanning a real site.
//
// Capture settles before touching the banner, so the pre-consent request set
// is complete. On a page that never reaches network idle — analytics
// heartbeats, long-polling, video — that settle used to consume the entire
// scan budget. The consent hook then ran against an expired context, failed
// to inject its helpers, and the scan reported "no CMP detected" for a page
// that plainly had one: a false negative on the product's headline finding.
//
// Capture now reserves budget for the interaction, so the banner is handled
// even though the page never goes quiet.
func TestConsentRunsOnAPageThatNeverGoesIdle(t *testing.T) {
	info := requireChrome(t)

	site := newCMPSiteWith(t, true)

	s, closePool := newScannerFor(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	target := site.target()
	// A budget the poller would otherwise consume entirely.
	target.HardTimeout = 30 * time.Second
	target.IdleQuiet = 2 * time.Second

	out, err := s.Scan(ctx, target, model.ConsentReject)

	// Exhausting the budget is a recorded outcome, not an error to report.
	if err != nil {
		t.Fatalf("Scan returned an error for a page that merely never settled: %v", err)
	}

	res := out.Result

	// The scan is expected to end on its budget: the page never goes quiet.
	if res.Termination != model.TermTimeout {
		t.Logf("termination = %q (a timeout was expected but is not required)", res.Termination)
	}

	if res.Consent.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied: the interaction was starved of budget",
			res.Consent.Outcome, res.Consent.Reason)
	}

	if res.Consent.CMP != "Consentmanager" {
		t.Errorf("CMP = %q, want Consentmanager", res.Consent.CMP)
	}

	// A truncated scan must still report honestly that it was truncated.
	if !res.Truncated() && res.Termination == model.TermTimeout {
		t.Error("a timed-out scan did not report itself as truncated")
	}
}
