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

// A site that keeps its consent decision in localStorage rather than in a
// cookie, which is what CCM19 and a large class of CMPs do.
//
// The cookie jar was cleared between scans from the start, so the isolation
// test built on a cookie-storing fixture passed while a reused browser was
// carrying an accept decision from one scan into the next: the next scan saw
// no banner, and every tag the stored decision released was recorded as
// pre-consent traffic (Story 1.5, AC1).
const storageSiteHost = "storage-site.test"

type storageSite struct {
	site       *httptest.Server
	thirdParty *httptest.Server
}

func newStorageSite(t *testing.T) *storageSite {
	t.Helper()

	s := &storageSite{}

	thirdMux := http.NewServeMux()

	thirdMux.HandleFunc("/tag.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__tagLoaded = true;"))
	})

	s.thirdParty = httptest.NewServer(thirdMux)
	t.Cleanup(s.thirdParty.Close)

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
		_, _ = fmt.Fprint(w, s.html())
	})

	s.site = httptest.NewServer(siteMux)
	t.Cleanup(s.site.Close)

	return s
}

func (s *storageSite) resolverRules() string {
	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		storageSiteHost, hostPort(s.site.URL), thirdHost, hostPort(s.thirdParty.URL))
}

func (s *storageSite) target(t *testing.T, mode model.ConsentMode) config.Resolved {
	t.Helper()

	return config.Resolved{
		Name:         "storage-fixture",
		URL:          "http://" + storageSiteHost + "/",
		ConsentModes: []model.ConsentMode{mode},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}

// html renders the banner only when no decision is stored, and fires the
// third-party tag straight away when an accept decision is — the behaviour
// that turns leaked storage into a fabricated pre-consent finding.
func (s *storageSite) html() string {
	return `<!DOCTYPE html>
<html><head><title>storage fixture</title></head>
<body>
<h1>fixture</h1>
<script>
(function () {
  var stored = null;

  try { stored = window.localStorage.getItem('consent'); } catch (e) {}

  function loadTag(reason) {
    var s = document.createElement('script');
    s.src = 'http://` + thirdHost + `/tag.js?c=' + reason;
    document.head.appendChild(s);
  }

  if (stored === 'accept') {
    loadTag('stored');

    return;
  }

  if (stored === 'reject') {
    return;
  }

  var banner = document.createElement('div');
  banner.id = 'cookie-banner';
  banner.setAttribute('style', 'position:fixed;bottom:0;left:0;width:600px;height:120px;background:#eee');
  banner.innerHTML = '<p>We use cookies. Please choose whether to allow cookies and tracking.</p>' +
    '<button id="accept-all">Accept all</button>' +
    '<button id="reject-all">Reject all</button>';
  document.body.appendChild(banner);

  function decide(choice) {
    return function () {
      try { window.localStorage.setItem('consent', choice); } catch (e) {}

      banner.remove();

      if (choice === 'accept') loadTag('accept');
    };
  }

  document.getElementById('accept-all').addEventListener('click', decide('accept'));
  document.getElementById('reject-all').addEventListener('click', decide('reject'));
})();
</script>
</body></html>`
}

// TestPerScanStorageIsolation is Story 1.5 AC1 for the half of the state that
// is not a cookie: an accept-mode scan must not leave a decision behind that
// the next scan on the same browser inherits.
func TestPerScanStorageIsolation(t *testing.T) {
	info := requireChrome(t)

	site := newStorageSite(t)

	// One browser, no recycling: every scan here runs on the same Chrome
	// process, which is the configuration the leak needs (maxScansPerBrowser
	// above 1) and the one a throughput-tuned deployment runs.
	s, closePool := newScannerFor(t, info, site.resolverRules())

	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first, err := s.Scan(ctx, site.target(t, model.ConsentAccept), model.ConsentAccept)
	if err != nil {
		t.Fatalf("accept scan: %v", err)
	}

	if first.Result.Consent.Outcome == model.OutcomeNotNeeded {
		t.Fatalf("the accept scan found no banner on a fresh browser; the fixture is not exercising consent (%+v)",
			first.Result.Consent)
	}

	if !storedDecision(first.Result) {
		t.Fatalf("the accept scan did not record the stored decision; the fixture wrote no localStorage")
	}

	second, err := s.Scan(ctx, site.target(t, model.ConsentReject), model.ConsentReject)
	if err != nil {
		t.Fatalf("reject scan: %v", err)
	}

	if !second.Result.Environment.BrowserReused {
		t.Fatal("the second scan did not reuse the first scan's browser; this test cannot see the leak")
	}

	if second.Result.Consent.Outcome == model.OutcomeNotNeeded {
		t.Errorf("the second scan found no banner: the first scan's decision leaked in localStorage (%+v)",
			second.Result.Consent)
	}

	// The network side of the same leak, and the one that reaches a report: a
	// tag released by the inherited decision is recorded as firing before any
	// consent interaction.
	for _, req := range second.Result.Requests {
		if strings.Contains(req.URL, "/tag.js?c=stored") {
			t.Errorf("the second scan loaded the stored-consent tag (%s, phase %s): "+
				"pre-consent traffic manufactured by the leak", req.URL, req.Phase)
		}
	}
}

// storedDecision reports whether the fixture's localStorage decision was
// captured, so a fixture that silently stopped storing anything fails loudly
// rather than passing the isolation assertion for the wrong reason.
func storedDecision(res *model.Result) bool {
	for _, entry := range res.Storage {
		if entry.Area == model.StorageLocal && entry.Key == "consent" {
			return true
		}
	}

	return false
}
