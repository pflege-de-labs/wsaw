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

// Borlabs Cookie fixtures, used to prove the shipped rule drives both major
// versions.
//
// The v3 shape was taken from Borlabs Cookie 3.4.5 as deployed on
// wirtschaftsspiegel-thueringen.com on 2026-10-08: window.BorlabsCookie exists
// from the moment the script loads, the dialog is mounted into an empty
// #BorlabsCookieBox only after the visitor scrolls or presses a key, the
// floating widget in #BorlabsCookieWidget mounts it on click, and the choice
// is written to the borlabs-cookie cookie only once it is made. A scan of that
// site saw nothing but the widget and recorded the rule as failed.

const (
	borlabsSiteHost  = "borlabs-site.test"
	borlabsThirdHost = "borlabs-third.test"
)

type borlabsSite struct {
	site  *httptest.Server
	third *httptest.Server
}

func newBorlabsSite(t *testing.T, html string) *borlabsSite {
	t.Helper()

	s := &borlabsSite{}

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
		_, _ = fmt.Fprint(w, html)
	})

	s.site = httptest.NewServer(siteMux)
	t.Cleanup(s.site.Close)

	return s
}

func (s *borlabsSite) resolverRules() string {
	strip := func(u string) string { return strings.TrimPrefix(u, "http://") }

	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		borlabsSiteHost, strip(s.site.URL), borlabsThirdHost, strip(s.third.URL))
}

func (s *borlabsSite) target(mode model.ConsentMode) config.Resolved {
	return config.Resolved{
		Name:         "borlabs-fixture",
		URL:          "http://" + borlabsSiteHost + "/",
		ConsentModes: []model.ConsentMode{mode},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}
}

// borlabsV3FixtureHTML mounts its dialog only on scroll, a key press or a
// click on the widget, never on load.
func borlabsV3FixtureHTML() string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="de"><head><title>borlabs v3 fixture</title></head>
<body>
<h1>Wirtschaftsnachrichten</h1>
<div id="BorlabsCookieBox"></div>
<div id="BorlabsCookieWidget"><div class="brlbs-cmpnt-widget brlbs-bottom-0" role="button" tabindex="0"
  aria-label="Cookie-Einstellungen" style="position:fixed;bottom:10px;left:10px;width:40px;height:40px"></div></div>

<script>
(function () {
  window.BorlabsCookie = { Cookie: {}, Consents: {} };

  var box = document.getElementById('BorlabsCookieBox');
  var mounted = false;

  function record(consents, accepted) {
    document.cookie = 'borlabs-cookie=' + encodeURIComponent(JSON.stringify(
      { consents: consents, domainPath: location.host + '/', uid: 'anonymous', v3: true, version: 1 })) + '; path=/';
    box.innerHTML = '';
    if (accepted) {
      var s = document.createElement('script');
      s.src = 'http://%s/tag.js?c=accept';
      document.head.appendChild(s);
    }
  }

  function mount() {
    if (mounted) return;
    mounted = true;
    box.innerHTML =
      '<div class="brlbs-cmpnt-dialog brlbs-cmpnt-dialog-box" role="dialog" ' +
      'style="position:fixed;top:100px;left:100px;width:600px;height:300px;background:#fff">' +
      '<p>Datenschutzeinstellungen. Wir nutzen Cookies auf unserer Website.</p>' +
      '<button class="brlbs-cmpnt-btn brlbs-btn-accept-all">Alle akzeptieren</button>' +
      '<button class="brlbs-cmpnt-btn brlbs-btn-accept-only-essential">Nur essenzielle Cookies akzeptieren</button>' +
      '</div>';
    box.querySelector('.brlbs-btn-accept-all').addEventListener('click', function () {
      record({ essential: ['borlabs-cookie'], statistics: ['google-analytics'] }, true);
    });
    box.querySelector('.brlbs-btn-accept-only-essential').addEventListener('click', function () {
      record({ essential: ['borlabs-cookie'] }, false);
    });
  }

  window.addEventListener('scroll', mount);
  window.addEventListener('keydown', mount);
  document.querySelector('#BorlabsCookieWidget .brlbs-cmpnt-widget').addEventListener('click', mount);
})();
</script>
</body></html>`, borlabsThirdHost)
}

// borlabsV2FixtureHTML is the v2 shape the rule was first written for: the
// box is rendered on load, and its controls are anchors.
func borlabsV2FixtureHTML() string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="de"><head><title>borlabs v2 fixture</title></head>
<body>
<h1>fixture</h1>
<div id="BorlabsCookieBox"><div class="_brlbs-box" style="position:fixed;top:100px;left:100px;width:600px;height:300px;background:#fff">
  <p>Wir nutzen Cookies auf unserer Website.</p>
  <a href="#" class="_brlbs-btn-accept-all">Alle akzeptieren</a>
  <a href="#" class="_brlbs-refuse-cookie">Nur essenzielle Cookies akzeptieren</a>
</div></div>

<script>
(function () {
  window.BorlabsCookie = { Cookie: {} };
  var box = document.getElementById('BorlabsCookieBox');
  function record(accepted) {
    document.cookie = 'borlabs-cookie=' + encodeURIComponent(JSON.stringify({ consents: { essential: ['borlabs-cookie'] } })) + '; path=/';
    box.innerHTML = '';
    if (accepted) {
      var s = document.createElement('script');
      s.src = 'http://%s/tag.js?c=accept';
      document.head.appendChild(s);
    }
  }
  box.querySelector('._brlbs-btn-accept-all').addEventListener('click', function (e) { e.preventDefault(); record(true); });
  box.querySelector('._brlbs-refuse-cookie').addEventListener('click', function (e) { e.preventDefault(); record(false); });
})();
</script>
</body></html>`, borlabsThirdHost)
}

func TestBorlabsRule(t *testing.T) {
	info := requireChrome(t)

	for _, tc := range []struct {
		name    string
		html    string
		mode    model.ConsentMode
		wantTag bool
	}{
		{"v3 reject opens the deferred dialog", borlabsV3FixtureHTML(), model.ConsentReject, false},
		{"v3 accept opens the deferred dialog", borlabsV3FixtureHTML(), model.ConsentAccept, true},
		{"v2 reject", borlabsV2FixtureHTML(), model.ConsentReject, false},
		{"v2 accept", borlabsV2FixtureHTML(), model.ConsentAccept, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newBorlabsSite(t, tc.html)

			s, closePool := newScannerFor(t, info, site.resolverRules())
			defer closePool()

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			out, err := s.Scan(ctx, site.target(tc.mode), tc.mode)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			res := out.Result

			if !res.OK() {
				t.Fatalf("scan not OK: %s %s", res.Termination, res.Error)
			}

			c := res.Consent

			if c.Outcome != model.OutcomeApplied {
				t.Fatalf("outcome = %q (%s), want applied", c.Outcome, c.Reason)
			}

			if c.Detection != "rule:borlabs" {
				t.Errorf("detection = %q, want the borlabs rule", c.Detection)
			}

			if c.Heuristic {
				t.Error("a vendor rule was reported as a heuristic match")
			}

			checkAcceptOnlyTag(t, res, tc.wantTag)
		})
	}
}

// checkAcceptOnlyTag asserts whether the fixture's accept-only tag was
// loaded, and that it never counts as pre-consent traffic.
func checkAcceptOnlyTag(t *testing.T, res *model.Result, want bool) {
	t.Helper()

	var loaded bool

	for _, req := range res.Requests {
		if strings.Contains(req.URL, "/tag.js") {
			loaded = true

			if req.Phase != model.PhasePost {
				t.Errorf("the accept-only tag was recorded in phase %q", req.Phase)
			}
		}
	}

	if loaded != want {
		t.Errorf("accept-only tag loaded = %v, want %v", loaded, want)
	}
}
