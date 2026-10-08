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

// wetter.com's "pay or OK" wall in front of a TCF CMP with no UI of its own,
// as served on 2026-10-08: the TCF API answers getTCData synchronously with
// eventStatus "cmpuishown" and no TC string until "Akzeptieren und weiter" is
// clicked, which stores the choice and reloads the page. The wall offers no
// reject. The site's hosts are mapped to a local server.

const (
	wetterSiteHost  = "www.wetter.com"
	wetterThirdHost = "wetter-ads.test"
)

func wetterFixtureHTML() string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="de"><head><title>wetter fixture</title>
<script>
(function () {
  var stored = window.localStorage.getItem('uc_tcf');
  window.__tcfapi = function (command, version, callback) {
    if (command === 'getTCData') {
      callback({ cmpId: 329, eventStatus: stored ? 'tcloaded' : 'cmpuishown', tcString: stored || '' }, true);
      return;
    }
    if (command === 'ping') { callback({ cmpLoaded: true, cmpStatus: 'loaded' }, true); return; }
    if (command === 'addEventListener') {
      callback({ cmpId: 329, eventStatus: stored ? 'tcloaded' : 'cmpuishown', tcString: stored || '', listenerId: 1 }, true);
      return;
    }
    callback(null, false);
  };
  if (stored) {
    var s = document.createElement('script');
    s.src = 'http://%s/tag.js';
    document.head.appendChild(s);
  }
})();
</script></head>
<body>
<h1>Wetter</h1>
<div id="cmp-style-reset"><div id="cmp-wetter" style="position:fixed;top:40px;left:280px;width:720px;height:700px;background:#fff">
  <p>Mit Werbung weiterlesen ... oder mit contentpass</p>
  <button id="cmp-btn-accept" class="cmp-btn-main">Akzeptieren und weiter</button>
  <button id="cmp-btn-signup" class="cmp-btn-main">Werbefrei f&uuml;r 3,99&euro; / Monat</button>
  <a id="cmp-lnk-settings" href="#">Privatsph&auml;re Einstellungen</a>
</div></div>
<script>
(function () {
  var wall = document.getElementById('cmp-style-reset');
  if (window.localStorage.getItem('uc_tcf')) {
    wall.style.display = 'none';
    return;
  }
  document.getElementById('cmp-btn-accept').addEventListener('click', function () {
    window.localStorage.setItem('uc_tcf', 'CQrw4jAQrw4jAAfHSSDECzF8AP_gAEPgAAYgLBAB');
    window.location.reload();
  });
})();
</script>
</body></html>`, wetterThirdHost)
}

func TestWetterComContentpassWall(t *testing.T) {
	info := requireChrome(t)

	for _, tc := range []struct {
		mode        model.ConsentMode
		wantOutcome model.ConsentOutcome
		wantTag     bool
	}{
		{model.ConsentAccept, model.OutcomeApplied, true},
		// The wall offers no reject and no necessary-only state. Recording
		// anything but a failure would claim a choice the site never offered.
		{model.ConsentReject, model.OutcomeFailed, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/favicon.ico":
					w.Header().Set("Content-Type", "image/x-icon")
					_, _ = w.Write([]byte{0, 0, 1, 0})
				case strings.HasPrefix(r.Host, wetterThirdHost):
					w.Header().Set("Content-Type", "text/javascript")
					_, _ = w.Write([]byte("window.__tag = true;"))
				default:
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, _ = fmt.Fprint(w, wetterFixtureHTML())
				}
			}))
			t.Cleanup(srv.Close)

			addr := strings.TrimPrefix(srv.URL, "http://")

			s, closePool := newScannerFor(t, info, fmt.Sprintf("MAP %s %s, MAP %s %s", wetterSiteHost, addr, wetterThirdHost, addr))
			defer closePool()

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			target := config.Resolved{
				Name:         "wetter-fixture",
				URL:          "http://" + wetterSiteHost + "/",
				ConsentModes: []model.ConsentMode{tc.mode},
				IdleQuiet:    1500 * time.Millisecond,
				HardTimeout:  40 * time.Second,
				NavTimeout:   20 * time.Second,
				MaxRequests:  200,
				MaxBytes:     1 << 20,
				Robots:       config.RobotsIgnore,
			}

			out, err := s.Scan(ctx, target, tc.mode)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			c := out.Result.Consent

			if c.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q (%s), want %q", c.Outcome, c.Reason, tc.wantOutcome)
			}

			if tc.mode == model.ConsentAccept && c.Detection != "rule:wetter-com-contentpass" {
				t.Errorf("detection = %q, want the wetter-com-contentpass rule", c.Detection)
			}

			var loaded bool

			for _, req := range out.Result.Requests {
				if strings.Contains(req.URL, wetterThirdHost+"/tag.js") {
					loaded = true
				}
			}

			if loaded != tc.wantTag {
				t.Errorf("accept-only tag loaded = %v, want %v", loaded, tc.wantTag)
			}
		})
	}
}
