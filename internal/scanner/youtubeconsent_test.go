package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A target that redirects to a YouTube channel lands on YouTube's consent
// interstitial. The fixture reproduces the page served by
// consent.youtube.com/m on 2026-10-08 for beautytv.de, which a scan reported
// as "no consent management platform detected": two pairs of forms posting
// to /save (the page repeats them for its narrow layout), told apart only by
// their hidden inputs, and a save that redirects to the continue URL. The
// YouTube hosts are mapped to a local server; nothing leaves the machine.
// youtube.com is on Chrome's HSTS preload list, so the server speaks TLS and
// this test's browser accepts its self-signed certificate.

const (
	ytTargetHost  = "youtube-target.test"
	ytWWWHost     = "www.youtube.com"
	ytConsentHost = "consent.youtube.com"
	ytThirdHost   = "youtube-ads.test"
)

func ytConsentPageHTML(continueURL string) string {
	form := func(inputs, label string) string {
		return fmt.Sprintf(`<form action="https://%s/save" method="POST">
  <input type="hidden" name="gl" value="DE"><input type="hidden" name="pc" value="yt">
  <input type="hidden" name="continue" value="%s">%s
  <button class="VfPpkd-LgbsSe" jsname="x" aria-label="%s">%s</button>
</form>`, ytConsentHost, continueURL, inputs, label, label)
	}

	reject := form(`<input type="hidden" name="set_eom" value="true">`, "Alle ablehnen")
	accept := form(`<input type="hidden" name="set_ytc" value="true"><input type="hidden" name="set_apyt" value="true">`+
		`<input type="hidden" name="set_eom" value="false">`, "Alle akzeptieren")

	return `<!DOCTYPE html><html lang="de"><head><title>Bevor Sie zu YouTube weitergehen</title></head><body>
<h1>Bevor Sie zu YouTube weitergehen</h1>
<p>Wir verwenden Cookies und Daten einschlie&szlig;lich IP-Adressen, um Google-Dienste zu erbringen.</p>
<div class="narrow" style="display:none">` + reject + accept + `</div>
<div class="wide">` + reject + accept + `</div>
</body></html>`
}

func newYouTubeConsentServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]

		switch {
		case r.URL.Path == "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

		case host == ytTargetHost:
			http.Redirect(w, r, "https://"+ytWWWHost+"/@channel", http.StatusFound)

		case host == ytWWWHost:
			socs, err := r.Cookie("SOCS")
			if err != nil {
				cont := "https://" + ytWWWHost + "/@channel"
				http.Redirect(w, r, "https://"+ytConsentHost+"/m?continue="+url.QueryEscape(cont), http.StatusFound)

				return
			}

			tag := ""
			if socs.Value == "accepted" {
				tag = fmt.Sprintf(`<script src="https://%s/tag.js"></script>`, ytThirdHost)
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>channel</title>%s</head><body><h1>channel</h1></body></html>`, tag)

		case host == ytConsentHost && r.URL.Path == "/m":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, ytConsentPageHTML(r.URL.Query().Get("continue")))

		case host == ytConsentHost && r.URL.Path == "/save" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			value := "rejected"
			if r.PostForm.Get("set_ytc") == "true" {
				value = "accepted"
			}

			http.SetCookie(w, &http.Cookie{Name: "SOCS", Value: value, Domain: "youtube.com", Path: "/"})
			http.Redirect(w, r, r.PostForm.Get("continue"), http.StatusSeeOther)

		case host == ytThirdHost:
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte("window.__tag = true;"))

		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(srv.Close)

	return srv
}

func TestYouTubeConsentPage(t *testing.T) {
	info := requireChrome(t)

	for _, tc := range []struct {
		mode    model.ConsentMode
		wantTag bool
	}{
		{model.ConsentReject, false},
		{model.ConsentAccept, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			srv := newYouTubeConsentServer(t)
			addr := strings.TrimPrefix(srv.URL, "https://")
			resolver := fmt.Sprintf("MAP %s %s, MAP %s %s, MAP %s %s, MAP %s %s",
				ytTargetHost, addr, ytWWWHost, addr, ytConsentHost, addr, ytThirdHost, addr)

			s, closePool := newScannerForArgs(t, info, "host-resolver-rules="+resolver, "ignore-certificate-errors")
			defer closePool()

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			target := config.Resolved{
				Name:         "youtube-consent-fixture",
				URL:          "https://" + ytTargetHost + "/",
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

			res := out.Result
			c := res.Consent

			if c.Outcome != model.OutcomeApplied {
				t.Fatalf("outcome = %q (%s), want applied", c.Outcome, c.Reason)
			}

			if c.Detection != "rule:youtube-consent" || c.CMP != "YouTube" {
				t.Errorf("detection = %q, cmp = %q, want the youtube-consent rule", c.Detection, c.CMP)
			}

			var loaded bool

			for _, req := range res.Requests {
				if strings.Contains(req.URL, ytThirdHost+"/tag.js") {
					loaded = true
				}
			}

			if loaded != tc.wantTag {
				t.Errorf("accept-only tag loaded = %v, want %v", loaded, tc.wantTag)
			}
		})
	}
}
