package scanner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/robots"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// logLine is one structured record, decoded so assertions are about fields
// rather than about substrings of a formatted line.
type logLine struct {
	Level       string `json:"level"`
	Msg         string `json:"msg"`
	ScanID      string `json:"scan_id"`
	Target      string `json:"target"`
	ConsentMode string `json:"consent_mode"`
	URL         string `json:"url"`
}

func decodeLog(t *testing.T, raw string) []logLine {
	t.Helper()

	var out []logLine

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}

		var l logLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("log line is not valid JSON: %v (%s)", err, line)
		}

		out = append(out, l)
	}

	return out
}

func findLine(lines []logLine, msg string) (logLine, bool) {
	for _, l := range lines {
		if l.Msg == msg {
			return l, true
		}
	}

	return logLine{}, false
}

// skippingScanner builds a scanner whose target is disallowed by robots.txt.
// That path returns before the browser is ever acquired, so the logging
// contract is testable without Chrome (Tenet 13).
func skippingScanner(t *testing.T) (*scanner.Scanner, config.Resolved, *bytes.Buffer) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	normalizer, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	// The pool is required by the constructor but never acquired on this
	// path, so no browser is launched.
	pool := browser.NewPool(browser.PoolOptions{Size: 1})

	s, err := scanner.New(scanner.Deps{
		Pool:   pool,
		Robots: robots.NewChecker(robots.Options{Client: srv.Client()}),
		Logger: logger,
	}, scanner.Options{Normalizer: normalizer})
	if err != nil {
		t.Fatal(err)
	}

	target := config.Resolved{
		Name:         "logged-target",
		URL:          srv.URL + "/page",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		Robots:       config.RobotsRespect,
		IdleQuiet:    time.Second,
		HardTimeout:  10 * time.Second,
	}

	return s, target, &buf
}

// TestScanStartIsLogged is Story 5.13: an operator must be able to see
// activity as it begins, not only once it is over.
func TestScanStartIsLogged(t *testing.T) {
	t.Parallel()

	s, target, buf := skippingScanner(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, target, model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	lines := decodeLog(t, buf.String())

	started, ok := findLine(lines, "scan started")
	if !ok {
		t.Fatalf("no \"scan started\" line was emitted; got %+v", lines)
	}

	// The scan ID is what ties the line to the stored result and to the
	// closing line.
	if started.ScanID == "" {
		t.Error("the start line carries no scan_id")
	}

	if out.Result != nil && started.ScanID != out.Result.ScanID {
		t.Errorf("start line scan_id = %q, result scan_id = %q; they must match",
			started.ScanID, out.Result.ScanID)
	}

	if started.Target != "logged-target" {
		t.Errorf("target = %q, want the configured target name", started.Target)
	}

	if started.ConsentMode != string(model.ConsentReject) {
		t.Errorf("consent_mode = %q, want reject", started.ConsentMode)
	}

	if started.URL != target.URL {
		t.Errorf("url = %q, want %q", started.URL, target.URL)
	}

	// A lifecycle event an operator watches for belongs at info, not debug.
	if started.Level != "INFO" {
		t.Errorf("level = %q, want INFO", started.Level)
	}
}

// TestScanStartIsLoggedEvenWhenSkipped: a scan that never reaches the browser
// still began, and an operator matching activity in a log must see it.
func TestScanStartIsLoggedEvenWhenSkipped(t *testing.T) {
	t.Parallel()

	s, target, buf := skippingScanner(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.Scan(ctx, target, model.ConsentReject); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	lines := decodeLog(t, buf.String())

	started, ok := findLine(lines, "scan started")
	if !ok {
		t.Fatal("a skipped scan emitted no start line")
	}

	skipped, ok := findLine(lines, "scan skipped by robots policy")
	if !ok {
		t.Fatal("the skip itself was not logged")
	}

	if skipped.ScanID != started.ScanID {
		t.Errorf("skip line scan_id = %q, start line = %q; both must carry the same scan",
			skipped.ScanID, started.ScanID)
	}

	// Ordering matters: the start line must come first, or a reader cannot
	// use it to bracket the scan.
	startIdx, skipIdx := -1, -1

	for i, l := range lines {
		switch l.Msg {
		case "scan started":
			startIdx = i
		case "scan skipped by robots policy":
			skipIdx = i
		}
	}

	if startIdx > skipIdx {
		t.Error("the start line was emitted after the skip line")
	}
}

// TestScanStartAndFinishShareTheScanID exercises the full path, so the
// bracketing an operator relies on is verified against a real scan rather
// than only against the skip path.
func TestScanStartAndFinishShareTheScanID(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, false)

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	normalizer, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	pool := browser.NewPool(browser.PoolOptions{
		Size: 1,
		Launch: browser.Options{
			Info:          info,
			LaunchTimeout: 40 * time.Second,
			ProfileDir:    t.TempDir(),
			ExtraArgs:     []string{"host-resolver-rules=" + site.resolverRules()},
			// The browser's own lifecycle logging would drown the assertions.
			Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		},
	})

	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing pool: %v", err)
		}
	}()

	s, err := scanner.New(
		scanner.Deps{Pool: pool, Logger: logger},
		scanner.Options{Normalizer: normalizer},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := s.Scan(ctx, site.target(t), model.ConsentNone); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	lines := decodeLog(t, buf.String())

	started, ok := findLine(lines, "scan started")
	if !ok {
		t.Fatalf("no start line: %+v", lines)
	}

	finished, ok := findLine(lines, "scan finished")
	if !ok {
		t.Fatalf("no finish line: %+v", lines)
	}

	if started.ScanID != finished.ScanID {
		t.Errorf("start scan_id = %q, finish scan_id = %q; a reader cannot pair them",
			started.ScanID, finished.ScanID)
	}
}
