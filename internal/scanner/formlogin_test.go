package scanner_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

const fixturePassword = "open-sesame"

// passwordShop is shaped like a Shopify storefront behind its password page:
// every page redirects to /password without the session cookie, and a
// refused password answers 200 and sets a cookie anyway, so only where the
// browser ends up tells success from failure.
type passwordShop struct {
	shop       *httptest.Server
	thirdParty *httptest.Server

	mu sync.Mutex
	// thirdPartyCookies is every Cookie header the third party received.
	thirdPartyCookies []string
}

func newPasswordShop(t *testing.T) *passwordShop {
	t.Helper()

	p := &passwordShop{}

	third := http.NewServeMux()
	third.HandleFunc("/px.gif", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.thirdPartyCookies = append(p.thirdPartyCookies, r.Header.Get("Cookie"))
		p.mu.Unlock()

		w.Header().Set("Content-Type", "image/gif")
	})

	p.thirdParty = httptest.NewServer(third)
	t.Cleanup(p.thirdParty.Close)

	shop := http.NewServeMux()
	shop.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
	})
	shop.HandleFunc("/password.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
	})
	shop.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
	})
	shop.HandleFunc("/password", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.PostFormValue("form_type") == "storefront_password" &&
			r.PostFormValue("password") == fixturePassword {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "unlocked", Path: "/", HttpOnly: true})
			http.Redirect(w, r, "/", http.StatusFound)

			return
		}

		http.SetCookie(w, &http.Cookie{Name: "session", Value: "locked", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><link rel="stylesheet" href="/password.css"><form method="post"></form>`)
	})
	shop.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err != nil || c.Value != "unlocked" {
			http.Redirect(w, r, "/password", http.StatusFound)

			return
		}

		// The third party is reached as localhost, a different host from the
		// shop's 127.0.0.1, so a cookie scoped to the shop must not reach it.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><script src="/app.js"></script><img src="http://localhost:%s/px.gif">`,
			strings.TrimPrefix(p.thirdParty.URL, "http://127.0.0.1:"))
	})

	p.shop = httptest.NewServer(shop)
	t.Cleanup(p.shop.Close)

	return p
}

func (p *passwordShop) target(password string) config.Resolved {
	return config.Resolved{
		Name:         "shop",
		URL:          p.shop.URL + "/",
		ConsentModes: []model.ConsentMode{model.ConsentNone},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  30 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  500,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
		FormLogin: &capture.FormLogin{
			URL: p.shop.URL + "/password",
			Fields: map[string]secret.Value{
				"form_type": secret.Literal("storefront_password"),
				"password":  secret.Literal(password),
			},
		},
	}
}

func TestFormLoginScansThePageBehindThePassword(t *testing.T) {
	info := requireChrome(t)
	p := newPasswordShop(t)

	s, closePool := newScannerForArgs(t, info)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, p.target(fixturePassword), model.ConsentNone)
	if err != nil {
		t.Fatal(err)
	}

	res := out.Result
	if !res.OK() {
		t.Fatalf("scan not OK: termination %s, error %q", res.Termination, res.Error)
	}

	var sawApp, sawPixel bool

	for _, r := range res.Requests {
		switch {
		case strings.HasSuffix(r.URL, "/app.js"):
			sawApp = true
		case strings.HasSuffix(r.URL, "/px.gif"):
			sawPixel = true
		case strings.Contains(r.URL, "/password"):
			// The login exchange is not the observation.
			t.Errorf("login page traffic recorded: %s", r.URL)
		}
	}

	if !sawApp || !sawPixel {
		t.Errorf("protected page's assets missing: app.js %v, px.gif %v", sawApp, sawPixel)
	}

	if res.Environment.FormLoginURL != p.shop.URL+"/password" {
		t.Errorf("formLoginUrl = %q", res.Environment.FormLoginURL)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range p.thirdPartyCookies {
		if strings.Contains(c, "session=") {
			t.Errorf("the login session reached the third party: %q", c)
		}
	}
}

// TestFormLoginRejectedFailsTheScan: a refused password lands on the login
// page, and a clean-looking result of the login page is the worst outcome
// this feature could have (Tenet 5).
func TestFormLoginRejectedFailsTheScan(t *testing.T) {
	info := requireChrome(t)
	p := newPasswordShop(t)

	s, closePool := newScannerForArgs(t, info)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, _ := s.Scan(ctx, p.target("wrong"), model.ConsentNone)

	if out.Result == nil {
		t.Fatal("a refused login produced no result; the failure must be recorded")
	}

	if out.Result.OK() {
		t.Fatal("a scan that never got past the login page reported itself as OK")
	}

	if !strings.Contains(out.Result.Error, "login was not accepted") {
		t.Errorf("error = %q, want it to say the login was not accepted", out.Result.Error)
	}

	if strings.Contains(out.Result.Error, "wrong") {
		t.Errorf("error leaks a form field value: %q", out.Result.Error)
	}
}
