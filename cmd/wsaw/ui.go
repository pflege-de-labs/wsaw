package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// cmdUI opens the operator's browser straight into the web interface,
// already signed in (Story 5.21).
//
// It reads the same configuration the daemon runs with, so it needs no
// separate credential of its own: an operator who can read the config file
// can already read the token in it (Story 3.6). What this command spends
// that token on is not putting it in front of a browser. It asks the running
// daemon for a one-time login token — good for one redemption, expiring in
// seconds — and opens the browser at that link instead, so the standing
// token never sits in a URL a browser keeps in history or offers back to a
// site's Referer header.
func cmdUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)

	configPath := fs.String("config", "",
		"path to the configuration file (default: the first of the standard locations that exists)")
	printOnly := fs.Bool("print", false, "print the sign-in URL instead of opening a browser")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadUIConfig(*configPath)
	if err != nil {
		return err
	}

	loginURL, expiresIn, err := buildUILoginURL(ctx, cfg)
	if err != nil {
		return err
	}

	if *printOnly {
		fmt.Println(loginURL)

		return nil
	}

	if err := openBrowser(ctx, loginURL); err != nil {
		fmt.Fprintf(os.Stderr, "ui: could not open a browser (%v); open this link yourself", err)

		if expiresIn > 0 {
			fmt.Fprintf(os.Stderr, " within %s", expiresIn)
		}

		fmt.Fprintln(os.Stderr, ":")
		fmt.Println(loginURL)

		return nil
	}

	fmt.Println("Opened wsaw's web interface in your browser.")

	return nil
}

// loadUIConfig reads the daemon's own configuration, without requiring a
// target list the way a scan does: "ui" only needs to know where the
// interface listens and how it is protected.
func loadUIConfig(path string) (*config.Config, error) {
	if path != "" {
		return config.Load(path)
	}

	return loadFromDefaultPaths()
}

// mintTimeout bounds the local call that asks a running daemon for a
// one-time login token. It is a loopback request to a process that is
// already up, so a slow answer means something is wrong, not busy.
const mintTimeout = 5 * time.Second

// buildUILoginURL resolves where the interface lives and, when it is
// protected, exchanges the standing token for a one-time login link. The
// returned duration is how long that link stays redeemable; it is zero when
// no token is configured, because there is then nothing to redeem.
func buildUILoginURL(ctx context.Context, cfg *config.Config) (string, time.Duration, error) {
	if !cfg.API.Enabled {
		return "", 0, errors.New("ui: the HTTP interface is not enabled (api.enabled is false in the configuration)")
	}

	webUI := true
	if cfg.API.WebUI != nil {
		webUI = *cfg.API.WebUI
	}

	if !webUI {
		return "", 0, errors.New("ui: the web interface is disabled (api.webui is false in the configuration)")
	}

	token, err := secret.Resolve(cfg.API.Token)
	if err != nil {
		return "", 0, fmt.Errorf("ui: api.token: %w", err)
	}

	scheme := "http"
	if cfg.API.TLSCert != "" {
		scheme = "https"
	}

	base := scheme + "://" + dialAddr(cfg.API.Listen)

	if !token.IsSet() {
		return base + "/", 0, nil
	}

	tok, expiresIn, err := mintLoginToken(ctx, base, token.Reveal())
	if err != nil {
		return "", 0, err
	}

	return base + "/login/otp/" + url.PathEscape(tok), expiresIn, nil
}

// dialAddr resolves a listen address to one the CLI, and the browser it
// opens, can actually reach. wsaw binds "0.0.0.0:PORT" or ":PORT" to accept
// remote clients, but a local command and the browser it launches reach the
// daemon through loopback regardless of who else is allowed to.
func dialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port)
}

// mintLoginToken asks a running daemon for a one-time login token,
// authenticating with the standing token exactly as any other API client
// does (Story 5.11, AC2). A network error here means no daemon answered at
// that address; a 404 means one did, with its web interface turned off.
func mintLoginToken(ctx context.Context, baseURL, apiToken string) (string, time.Duration, error) {
	mintCtx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(mintCtx, http.MethodPost, baseURL+"/api/v1/ui/login-token", nil)
	if err != nil {
		return "", 0, fmt.Errorf("ui: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf(
			"ui: no wsaw daemon reachable at %s (is it running with api.enabled: true?): %w", baseURL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", 0, fmt.Errorf("ui: the daemon at %s has the web interface disabled", baseURL)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return "", 0, fmt.Errorf("ui: minting a sign-in link failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("ui: decoding the sign-in link response: %w", err)
	}

	return out.Token, time.Duration(out.ExpiresIn) * time.Second, nil
}

// openBrowser launches the platform's default "open a URL" mechanism. wsaw
// only ships for linux and darwin (Tenet 14), so those are the only two
// mechanisms this needs to know.
func openBrowser(ctx context.Context, rawURL string) error {
	var opener string

	switch runtime.GOOS {
	case "darwin":
		opener = "open"

	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no graphical display is available")
		}

		opener = "xdg-open"

	default:
		return fmt.Errorf("opening a browser is not supported on %s", runtime.GOOS)
	}

	if _, err := exec.LookPath(opener); err != nil {
		return fmt.Errorf("%s not found", opener)
	}

	//nolint:gosec // opener is one of two fixed binary names chosen above,
	// never operator or network input; rawURL is a link this process built
	// itself, from a loopback address and a random token, not from anything
	// scanned.
	return exec.CommandContext(ctx, opener, rawURL).Start()
}
