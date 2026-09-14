package main

// "wsaw ui" (Story 5.21), tested at two levels: dialAddr and buildUILoginURL
// as pure functions, and a full round trip against a real httpapi.Server so
// the CLI's HTTP call and the server's mint/redeem handlers are proven to
// agree, not just to compile against the same types.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func TestDialAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:8712", "127.0.0.1:8712"},
		{"0.0.0.0:8712", "127.0.0.1:8712"},
		{":8712", "127.0.0.1:8712"},
		{"[::]:8712", "127.0.0.1:8712"},
		{"malformed", "malformed"},
	}

	for _, c := range cases {
		if got := dialAddr(c.listen); got != c.want {
			t.Errorf("dialAddr(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func TestBuildUILoginURLRefusesWhenAPIIsNotEnabled(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	_, _, err := buildUILoginURL(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("err = %v, want a complaint about api.enabled", err)
	}
}

func TestBuildUILoginURLRefusesWhenWebUIIsOff(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{API: config.API{Enabled: true, Listen: "127.0.0.1:8712", WebUI: boolPtr(false)}}

	_, _, err := buildUILoginURL(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "web interface is disabled") {
		t.Fatalf("err = %v, want a complaint about api.webui", err)
	}
}

func TestBuildUILoginURLWithNoTokenOpensThePlainDashboard(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{API: config.API{Enabled: true, Listen: "127.0.0.1:8712"}}

	got, expiresIn, err := buildUILoginURL(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildUILoginURL: %v", err)
	}

	if got != "http://127.0.0.1:8712/" {
		t.Errorf("url = %q, want the plain dashboard", got)
	}

	if expiresIn != 0 {
		t.Errorf("expiresIn = %v, want 0 when there is nothing to redeem", expiresIn)
	}
}

func TestBuildUILoginURLWithNoDaemonReachableFailsClearly(t *testing.T) {
	t.Parallel()

	// Port 0 with a token set forces the mint call, which then has nothing
	// to connect to.
	cfg := &config.Config{
		API: config.API{Enabled: true, Listen: "127.0.0.1:1", Token: "s3cret"},
	}

	_, _, err := buildUILoginURL(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "no wsaw daemon reachable") {
		t.Fatalf("err = %v, want a clear \"no daemon\" message", err)
	}
}

// newUIFixtureServer starts a real httpapi.Server — no Chrome, no daemon,
// just the HTTP surface "wsaw ui" actually talks to.
func newUIFixtureServer(t *testing.T, token string) *httptest.Server {
	t.Helper()

	dir := t.TempDir()

	st, err := store.Open(store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	opts := httpapi.Options{WebUI: true}
	if token != "" {
		opts.Token = secret.Literal(token)
	}

	srv, err := httpapi.New(opts, httpapi.Deps{Store: st})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts
}

func TestBuildUILoginURLMintsAndRedeemsAgainstARealServer(t *testing.T) {
	t.Parallel()

	ts := newUIFixtureServer(t, "s3cret")

	cfg := &config.Config{
		API: config.API{
			Enabled: true,
			Listen:  strings.TrimPrefix(ts.URL, "http://"),
			Token:   "s3cret",
		},
	}

	loginURL, expiresIn, err := buildUILoginURL(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildUILoginURL: %v", err)
	}

	if expiresIn <= 0 {
		t.Errorf("expiresIn = %v, want positive", expiresIn)
	}

	if !strings.Contains(loginURL, "/login/otp/") {
		t.Fatalf("loginURL = %q, want a one-time login link", loginURL)
	}

	// This is what opening loginURL in a browser does: one GET, no headers,
	// no prior cookie.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(loginURL) //nolint:noctx // loginURL is this test's own httptest server
	if err != nil {
		t.Fatalf("redeeming the link: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect", resp.StatusCode)
	}

	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Value != "s3cret" {
		t.Fatalf("cookies = %+v, want one session cookie carrying the standing token", cookies)
	}

	// The link is single-use: minting again is required, this same URL must
	// not work twice.
	second, err := client.Get(loginURL) //nolint:noctx // see above
	if err != nil {
		t.Fatalf("redeeming the link a second time: %v", err)
	}

	defer func() { _ = second.Body.Close() }()

	if len(second.Cookies()) != 0 {
		t.Error("a one-time link signed in a second time")
	}
}

func TestBuildUILoginURLWithNoTokenConfiguredOnTheServerSide(t *testing.T) {
	t.Parallel()

	// The server has no token; the CLI's config agrees. There is nothing to
	// mint, so this must not even attempt the HTTP round trip.
	ts := newUIFixtureServer(t, "")

	cfg := &config.Config{
		API: config.API{Enabled: true, Listen: strings.TrimPrefix(ts.URL, "http://")},
	}

	loginURL, expiresIn, err := buildUILoginURL(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildUILoginURL: %v", err)
	}

	if expiresIn != 0 {
		t.Errorf("expiresIn = %v, want 0", expiresIn)
	}

	want := "http://" + strings.TrimPrefix(ts.URL, "http://") + "/"
	if loginURL != want {
		t.Errorf("loginURL = %q, want %q", loginURL, want)
	}
}

func TestCmdUIPrintFlagPrintsTheLinkInsteadOfOpeningABrowser(t *testing.T) {
	ts := newUIFixtureServer(t, "s3cret")

	path := writeConfig(t, "api:\n  enabled: true\n  listen: \""+strings.TrimPrefix(ts.URL, "http://")+"\"\n  token: s3cret\n")

	stdout, _ := capture(t, func() {
		if err := cmdUI(context.Background(), []string{"--config", path, "--print"}); err != nil {
			t.Errorf("cmdUI: %v", err)
		}
	})

	if !strings.Contains(stdout, "/login/otp/") {
		t.Errorf("stdout = %q, want the one-time login URL", stdout)
	}
}

func TestCmdUIFailsWhenTheInterfaceIsNotEnabled(t *testing.T) {
	path := writeConfig(t, "")

	err := cmdUI(context.Background(), []string{"--config", path, "--print"})
	if err == nil {
		t.Fatal("cmdUI: want an error when api.enabled is false")
	}

	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("err = %v, want a complaint about api.enabled", err)
	}
}
