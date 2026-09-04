// Command browse opens the running end-to-end fixture in a visible Chrome, so
// a person can see the page and the consent banner that wsaw scans.
//
// It exists as a Go command rather than a line of shell for two reasons.
// Chrome's location is decided by internal/browser, and a second copy of that
// per-platform list in a Makefile would drift from the one wsaw actually
// uses. And two details of launching Chrome are easy to get wrong in a way
// that fails confusingly rather than loudly:
//
//   - A dedicated --user-data-dir is not optional. Chrome hands the URL to an
//     already-running instance when the profile is shared, and that instance
//     silently ignores the resolver rules — so the fixture's hostnames would
//     not resolve and the page would fail for a reason nothing on screen
//     explains.
//   - The resolver rules have to match the ones a scan uses, or a person is
//     looking at a different site from the one wsaw reports on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/martint17r/wsaw/internal/browser"
)

func main() {
	log.SetFlags(0)

	url := flag.String("url", "", "the fixture URL to open in Chrome")
	checkURL := flag.String("check-url", "",
		"the same fixture reached over loopback, used to confirm it is up before Chrome opens")
	rules := flag.String("resolver-rules", "",
		"Chrome --host-resolver-rules, mapping the fixture's hostnames onto loopback")
	profile := flag.String("profile", "", "directory for the throwaway Chrome profile")
	chromePath := flag.String("chrome-path", "", "path to Chrome or Chromium; discovered when empty")
	keepProfile := flag.Bool("keep-profile", false,
		"reuse the profile from the last run, keeping any consent decision made in it")

	flag.Parse()

	if err := run(*url, *checkURL, *rules, *profile, *chromePath, *keepProfile); err != nil {
		log.Fatalf("browse: %v", err)
	}
}

func run(url, checkURL, rules, profile, chromePath string, keepProfile bool) error {
	if url == "" || profile == "" {
		return errors.New("-url and -profile are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Checked before Chrome opens, so a fixture that is not running says so
	// here instead of as a connection error inside a browser window.
	//
	// Over loopback rather than over the URL Chrome will use: the resolver
	// rules exist only inside Chrome, so this process cannot resolve the
	// fixture's hostnames at all and checking them here would fail even when
	// the fixture is perfectly healthy.
	if checkURL != "" {
		if err := reachable(ctx, checkURL); err != nil {
			return err
		}
	}

	chrome, err := browser.Discover(ctx, chromePath)
	if err != nil {
		return err
	}

	if !keepProfile {
		// A fresh profile every time, so the consent banner appears every
		// time. A leftover Klaro cookie would hide the very thing this
		// command exists to show — the same reason wsaw scans cold by
		// default (Tenet 2).
		if err := os.RemoveAll(profile); err != nil {
			return fmt.Errorf("clearing the previous profile at %s: %w", profile, err)
		}
	}

	if err := os.MkdirAll(profile, 0o700); err != nil {
		return fmt.Errorf("creating the profile directory %s: %w", profile, err)
	}

	args := []string{
		// Dedicated profile: see the package comment. Without this, an
		// already-running Chrome takes the URL and drops every flag below.
		"--user-data-dir=" + filepath.Clean(profile),
		"--no-first-run",
		"--no-default-browser-check",
		// Quieter, not silent: these cut most of Chrome's own background
		// traffic, but it still talks to some of its services and says so on
		// stderr. What matters is that none of it reaches the fixture, so
		// what is on screen is the fixture and nothing else.
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
	}

	if rules != "" {
		args = append(args, "--host-resolver-rules="+rules)
	}

	args = append(args, url)

	log.Printf("opening %s", url)
	log.Printf("  chrome:  %s", chrome)
	log.Printf("  profile: %s (fresh: %t)", profile, !keepProfile)

	if rules != "" {
		log.Printf("  resolver: %s", rules)
	}

	log.Print("close the window, or press Ctrl-C here, to finish")

	// #nosec G204 -- the binary comes from wsaw's own discovery and every
	// argument is built above; nothing here is taken from a scanned page.
	cmd := exec.CommandContext(ctx, chrome.Path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Chrome in its own process group, killed as a group.
	//
	// Chrome is a tree: one parent and a renderer, a GPU process and a zygote
	// under it. Killing only the parent leaves the rest running, which is the
	// leak wsaw refuses to tolerate for its own browsers (Tenet 1) and no more
	// acceptable here. WaitDelay turns a Chrome that ignores the signal into
	// one that is killed anyway.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Run(); err != nil {
		// A window closed by the person using it, or a Ctrl-C, is the normal
		// way this ends rather than a failure to report.
		if ctx.Err() != nil || isExitStatus(err) {
			return nil
		}

		return fmt.Errorf("running %s: %w", chrome.Path, err)
	}

	return nil
}

// reachable reports whether the fixture is up, with a message that says what
// to do about it if not.
func reachable(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("the URL %q is not usable: %w", url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the fixture is not answering at %s; start it with `make e2e-fixture-up`: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the fixture answered %s with %d; check `make e2e-fixture-logs`", url, resp.StatusCode)
	}

	return nil
}

// isExitStatus reports whether the error is merely Chrome having exited
// non-zero, which it does routinely on being closed.
func isExitStatus(err error) bool {
	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}
