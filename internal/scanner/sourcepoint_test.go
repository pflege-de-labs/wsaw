package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Sourcepoint draws its message in an iframe, so its buttons are never in the
// top document. The fixture reproduces the shape served on brigitte.de on
// 2026-10-08: the site defines _sp_queue before anything loads, Sourcepoint
// arrives later (there, through contentpass) and adds
// iframe#sp_message_iframe_<id>, and each button carries its choice as
// sp_choice_type_<n>. The message tells the page through postMessage, and the
// page removes the frame. Hosts are mapped to a local server.

const (
	spSiteHost     = "www.sp-site.test"
	spMessageHost  = "message.sp-site.test" // same site: rendered in-process
	spCrossCDNHost = "sp-cdn.test"          // another site
	spThirdHost    = "sp-ads.test"
)

func spTopHTML(messageHost string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="de"><head><title>sourcepoint fixture</title>
<script>window._sp_queue = [];</script></head>
<body>
<h1>Magazin</h1>
<script>
(function () {
  window.addEventListener('message', function (e) {
    if (!e.data || !e.data.spChoice) return;
    var c = document.getElementById('sp_message_container_1');
    if (c) c.remove();
    if (e.data.spChoice === 11) {
      var s = document.createElement('script');
      s.src = 'http://%s/tag.js';
      document.head.appendChild(s);
    }
  });

  // Sourcepoint is loaded lazily, after the page itself.
  setTimeout(function () {
    window._sp_ = { version: 'fixture' };
    var c = document.createElement('div');
    c.id = 'sp_message_container_1';
    c.setAttribute('style', 'position:fixed;inset:0;background:rgba(0,0,0,.5)');
    var f = document.createElement('iframe');
    f.id = 'sp_message_iframe_1';
    f.src = 'http://%s/message';
    f.setAttribute('style', 'width:800px;height:600px;border:0;background:#fff');
    c.appendChild(f);
    document.body.appendChild(c);
  }, 800);
})();
</script>
</body></html>`, spThirdHost, messageHost)
}

func spMessageHTML(withReject bool) string {
	reject := ""
	if withReject {
		reject = `<button title="Ablehnen" class="message-component message-button sp_choice_type_13">Ablehnen</button>`
	}

	return `<!DOCTYPE html><html><body>
<p>Brigitte mit Werbung und Cookies nutzen ... oder contentpass nutzen</p>
<button title="Zustimmen" class="message-component message-button sp_choice_type_11">Zustimmen</button>
<button title="Werbefrei" class="message-component message-button button-contentpass sp_choice_type_9">Werbefrei f&uuml;r 3,99 &euro; / Monat</button>
` + reject + `
<script>
document.querySelectorAll('button').forEach(function (b) {
  b.addEventListener('click', function () {
    var m = b.className.match(/sp_choice_type_(\d+)/);
    parent.postMessage({ spChoice: Number(m[1]) }, '*');
  });
});
</script>
</body></html>`
}

func newSourcepointServer(t *testing.T, messageHost string, withReject bool) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]

		switch {
		case r.URL.Path == "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})
		case host == spThirdHost:
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte("window.__tag = true;"))
		case r.URL.Path == "/message":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, spMessageHTML(withReject))
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, spTopHTML(messageHost))
		}
	}))

	t.Cleanup(srv.Close)

	return srv
}

func TestSourcepointMessageInAFrame(t *testing.T) {
	info := requireChrome(t)

	for _, tc := range []struct {
		name        string
		messageHost string
		withReject  bool
		mode        model.ConsentMode
		wantOutcome model.ConsentOutcome
		wantReason  string
		wantTag     bool
	}{
		{"accept", spMessageHost, true, model.ConsentAccept, model.OutcomeApplied, "", true},
		{"reject", spMessageHost, true, model.ConsentReject, model.OutcomeApplied, "", false},
		// The paid alternative is not a reject. A message without one must
		// fail on the missing control, never settle for another button.
		{"pay or OK has no reject", spMessageHost, false, model.ConsentReject, model.OutcomeFailed, "sp_choice_type_13", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSourcepointServer(t, tc.messageHost, tc.withReject)
			out := scanSourcepointFixture(t, info, srv, tc.mode)

			c := out.Consent

			if c.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q (%s), want %q", c.Outcome, c.Reason, tc.wantOutcome)
			}

			if !strings.Contains(c.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", c.Reason, tc.wantReason)
			}

			if c.Outcome == model.OutcomeApplied && c.Detection != "rule:sourcepoint" {
				t.Errorf("detection = %q, want the sourcepoint rule", c.Detection)
			}

			if loaded := requestedTag(out); loaded != tc.wantTag {
				t.Errorf("accept-only tag loaded = %v, want %v", loaded, tc.wantTag)
			}
		})
	}
}

// TestSourcepointMessageInAnotherProcess pins what happens when the browser
// renders the message frame in a process of its own, as a browser with
// strict site isolation does for a frame from another site. The scan must
// say it could not reach the frame, not report an unexplained failure or a
// choice it never made. The shipped headless browser keeps such frames
// in-process, so the test asks for site-per-process explicitly.
func TestSourcepointMessageInAnotherProcess(t *testing.T) {
	info := requireChrome(t)

	srv := newSourcepointServer(t, spCrossCDNHost, true)
	out := scanSourcepointFixture(t, info, srv, model.ConsentAccept, "site-per-process")

	c := out.Consent

	if c.Outcome != model.OutcomeFailed {
		t.Fatalf("outcome = %q (%s), want failed", c.Outcome, c.Reason)
	}

	if !strings.Contains(c.Reason, "another process") {
		t.Errorf("reason = %q, want it to say the frame is in another process", c.Reason)
	}

	if requestedTag(out) {
		t.Error("the accept-only tag loaded although no choice was made")
	}
}

func scanSourcepointFixture(t *testing.T, info browser.Info, srv *httptest.Server, mode model.ConsentMode, extraArgs ...string) *model.Result {
	t.Helper()

	addr := strings.TrimPrefix(srv.URL, "http://")
	resolver := fmt.Sprintf("MAP %s %s, MAP %s %s, MAP %s %s, MAP %s %s",
		spSiteHost, addr, spMessageHost, addr, spCrossCDNHost, addr, spThirdHost, addr)

	s, closePool := newScannerForArgs(t, info, append([]string{"host-resolver-rules=" + resolver}, extraArgs...)...)
	t.Cleanup(closePool)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	target := config.Resolved{
		Name:         "sourcepoint-fixture",
		URL:          "http://" + spSiteHost + "/",
		ConsentModes: []model.ConsentMode{mode},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
	}

	out, err := s.Scan(ctx, target, mode)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	return out.Result
}

func requestedTag(res *model.Result) bool {
	for _, req := range res.Requests {
		if strings.Contains(req.URL, spThirdHost+"/tag.js") {
			return true
		}
	}

	return false
}
