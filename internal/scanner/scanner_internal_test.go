package scanner

// The helpers below decide whether a scan runs, how a failure is recorded, and
// what an operator is told about it. They are unexported, so the external
// scanner_test package cannot reach them, and driving them through Scan would
// need a browser to test a pure function. This file sits in the package for
// the same reason live_test.go does.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LOCALHOST", false}, // callers lower-case the host first; this must not guess
		{"::1", true},
		{"0.0.0.0", true},
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"[::1]", true},
		{"app.localhost", true},
		{"example.com", false},
		{"192.168.1.10", false},
		{"", false},
	}

	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestUnreachableFromContainerRefusesLoopbackTargets(t *testing.T) {
	t.Parallel()

	// A containerised browser has its own network namespace, so the host's
	// loopback is not the target's loopback. Scanning it anyway would produce
	// a result about the container's own localhost (Story 1.8, AC10).
	cases := []struct {
		name        string
		runtime     string
		url         string
		wantOK      bool
		wantInReson string
	}{
		{"local runtime reaches loopback", "local", "http://localhost:8080/", true, ""},
		{"unset runtime reaches loopback", "", "http://127.0.0.1:8080/", true, ""},
		{"container runtime reaches a routable host", "podman", "https://example.com/", true, ""},
		{"container runtime refuses loopback", "podman", "http://localhost:8080/", false, "podman"},
		{"container runtime refuses 127.0.0.1", "docker", "http://127.0.0.1:9/", false, "127.0.0.1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s := &Scanner{opts: Options{BrowserRuntime: c.runtime}}

			reason, ok := s.unreachableFromContainer(config.Resolved{Name: "t", URL: c.url})
			if ok != c.wantOK {
				t.Fatalf("reachable = %v, want %v (reason %q)", ok, c.wantOK, reason)
			}

			if c.wantOK {
				if reason != "" {
					t.Errorf("a reachable target came with reason %q, want none", reason)
				}

				return
			}

			// The reason is the whole point: it has to name the host and the
			// runtime, and say what to do instead.
			if !strings.Contains(reason, c.wantInReson) {
				t.Errorf("reason %q does not mention %q", reason, c.wantInReson)
			}

			if !strings.Contains(reason, `browser.runtime`) {
				t.Errorf("reason %q does not tell the operator how to fix it", reason)
			}
		})
	}
}

func TestScrubRedactsThroughTheSecretRegistry(t *testing.T) {
	t.Parallel()

	reg := &secret.Registry{}
	reg.Add(secret.Literal("hunter2-token"))

	s := &Scanner{deps: Deps{Secrets: reg}}

	got := s.scrub("dial failed: bearer hunter2-token rejected")
	if strings.Contains(got, "hunter2-token") {
		t.Errorf("scrub left the secret in %q", got)
	}

	// No registry configured must not mean a panic, and must not mean the
	// text is dropped either.
	bare := &Scanner{}
	if got := bare.scrub("plain text"); got != "plain text" {
		t.Errorf("scrub without a registry = %q, want it unchanged", got)
	}
}

func TestFailedResultRecordsAFailureRatherThanAnEmptyScan(t *testing.T) {
	t.Parallel()

	// Tenet 5: a failed observation must never look like a clean result. The
	// shape of this struct is the guarantee.
	reg := &secret.Registry{}
	reg.Add(secret.Literal("s3cret-password"))

	s := &Scanner{
		deps: Deps{Secrets: reg},
		opts: Options{WsawVersion: "v1.2.3", ChromeVersion: "Chromium 131"},
	}

	target := config.Resolved{
		Name:   "site",
		URL:    "https://example.com/",
		Labels: map[string]string{"team": "web"},
	}

	res := s.failedResult("scan-abc", target, model.ConsentReject, "proxy auth s3cret-password failed")

	if res.Termination != model.TermError {
		t.Errorf("termination = %q, want %q", res.Termination, model.TermError)
	}

	if res.Consent.Outcome != model.OutcomeFailed {
		t.Errorf("consent outcome = %q, want %q", res.Consent.Outcome, model.OutcomeFailed)
	}

	if res.Consent.Reason == "" {
		t.Error("a failed result carries no consent reason, so the failure is unexplained")
	}

	if strings.Contains(res.Error, "s3cret-password") {
		t.Errorf("the recorded error leaks a secret: %q", res.Error)
	}

	if res.Requests == nil {
		t.Error("requests is nil; a failed scan records an empty list, not a missing one")
	}

	if res.ScanID != "scan-abc" || res.Target != "site" || res.URL != target.URL {
		t.Errorf("result does not identify the scan: %+v", res)
	}

	if res.Environment.WsawVersion != "v1.2.3" || res.Environment.ChromeVersion != "Chromium 131" {
		t.Errorf("environment = %+v, want the configured versions", res.Environment)
	}

	if res.SchemaVersion != model.SchemaVersion {
		t.Errorf("schema version = %v, want %v", res.SchemaVersion, model.SchemaVersion)
	}
}

func TestErrorText(t *testing.T) {
	t.Parallel()

	if got := errorText(nil); got != "" {
		t.Errorf("errorText(nil) = %q, want empty", got)
	}

	if got := errorText(errors.New("boom")); got != "boom" {
		t.Errorf("errorText = %q, want %q", got, "boom")
	}
}

func TestContainsFold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{"WebSocket closed", "websocket", true},
		{"websocket closed", "WEBSOCKET", true},
		{"tail match here", "here", true},
		{"exact", "exact", true},
		{"short", "a much longer needle", false},
		{"nothing to see", "websocket", false},
		{"anything", "", true},
	}

	for _, c := range cases {
		if got := containsFold(c.haystack, c.needle); got != c.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", c.haystack, c.needle, got, c.want)
		}
	}
}

func TestIsBrowserFailureSeparatesPageFaultsFromBrowserFaults(t *testing.T) {
	t.Parallel()

	// The distinction decides whether the browser goes back into the pool. A
	// page that timed out is a finding; a browser that lost its socket is not.
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"deadline is the page's fault", context.DeadlineExceeded, false},
		{"cancellation is the page's fault", context.Canceled, false},
		{"wrapped deadline is still the page's fault", fmt.Errorf("navigating: %w", context.DeadlineExceeded), false},
		{"websocket", errors.New("read tcp: websocket: close 1006"), true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:9222: connection refused"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"closed network connection", errors.New("use of closed network connection"), true},
		{"target closed", errors.New("Target closed"), true},
		{"browser marked dead", errors.New("browser is no longer usable"), true},
		{"page crash", errors.New("page crash detected"), true},
		{"chrome failed", errors.New("Chrome failed to start"), true},
		{"an ordinary page error", errors.New("net::ERR_NAME_NOT_RESOLVED"), false},
	}

	for _, c := range cases {
		if got := isBrowserFailure(c.err); got != c.want {
			t.Errorf("%s: isBrowserFailure = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHostAndDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url, host, domain string
	}{
		{"https://WWW.Example.co.uk/path", "www.example.co.uk", "example.co.uk"},
		{"http://sub.example.com:8080/", "sub.example.com", "example.com"},
		{"://not a url", "", ""},
	}

	for _, c := range cases {
		host, domain := hostAndDomain(c.url)
		if host != c.host || domain != c.domain {
			t.Errorf("hostAndDomain(%q) = (%q, %q), want (%q, %q)", c.url, host, domain, c.host, c.domain)
		}
	}
}

func TestAttemptTravelsInTheContext(t *testing.T) {
	t.Parallel()

	if got := attemptOf(context.Background()); got.attempt != 1 || got.attempts != 1 {
		t.Errorf("an unlabelled context reports %+v, want the first of one", got)
	}

	ctx := WithAttempt(context.Background(), 3, 5, "previous failure")

	got := attemptOf(ctx)
	if got.attempt != 3 || got.attempts != 5 || got.previous != "previous failure" {
		t.Errorf("attemptOf = %+v, want attempt 3 of 5 with the previous error", got)
	}

	// A caller counting from zero must not produce "attempt 0 of 3".
	if got := attemptOf(WithAttempt(context.Background(), 0, 3, "")); got.attempt != 1 {
		t.Errorf("attempt 0 was recorded as %d, want it clamped to 1", got.attempt)
	}
}

func TestNewScanIDIsUniqueAndPrefixed(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 64)

	for range 64 {
		id := newScanID()

		if !strings.HasPrefix(id, "scan-") {
			t.Fatalf("scan ID %q is not prefixed", id)
		}

		if seen[id] {
			t.Fatalf("scan ID %q was issued twice", id)
		}

		seen[id] = true
	}
}

func TestRunningCountTracksInFlightScans(t *testing.T) {
	t.Parallel()

	s := &Scanner{live: NewLive()}

	if got := s.RunningCount(); got != 0 {
		t.Fatalf("a fresh scanner reports %d running, want 0", got)
	}

	release := s.live.begin(Running{ScanID: "scan-1", Target: "site", StartedAt: time.Now()})

	if got := s.RunningCount(); got != 1 {
		t.Errorf("running count = %d, want 1", got)
	}

	release()

	if got := s.RunningCount(); got != 0 {
		t.Errorf("running count after release = %d, want 0", got)
	}
}

func TestRecordFailureAndBrowserRestartAreOptional(t *testing.T) {
	t.Parallel()

	// Every Metrics field is optional, so the zero value must not panic.
	bare := &Scanner{}
	bare.recordFailure("site", model.ConsentNone, "reason")
	bare.browserRestarted("site", "reason")

	var failed, restarted string

	s := &Scanner{deps: Deps{Metrics: Metrics{
		ScanFailed:     func(target string, _ model.ConsentMode, reason string) { failed = target + ":" + reason },
		BrowserRestart: func(target, reason string) { restarted = target + ":" + reason },
	}}}

	s.recordFailure("site", model.ConsentReject, "timeout")
	s.browserRestarted("site", "crashed")

	if failed != "site:timeout" {
		t.Errorf("ScanFailed saw %q", failed)
	}

	if restarted != "site:crashed" {
		t.Errorf("BrowserRestart saw %q", restarted)
	}
}

func TestNewRejectsMissingCollaborators(t *testing.T) {
	t.Parallel()

	if _, err := New(Deps{}, Options{}); err == nil {
		t.Error("New accepted a nil browser pool")
	}

	// A pool that has not launched anything, so no Chrome is involved.
	pool := browser.NewPool(browser.PoolOptions{Size: 1})
	t.Cleanup(func() { _ = pool.Close() })

	if _, err := New(Deps{Pool: pool}, Options{}); err == nil {
		t.Error("New accepted a nil normalizer")
	}
}
