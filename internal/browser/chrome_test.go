package browser_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/browser"
)

// stubVersions is every --version output these tests need a stub browser for.
// A stub is prepared for each before any test runs; see TestMain.
var stubVersions = []string{
	"Google Chrome 131.0.6778.85",
	"Chromium 130.0.6723.116 snap",
	"Chromium 124.0.6367.207 Arch Linux",
	"Google Chrome 140.0.7000.1 dev",
	"Chromium 90.0.4430.212",
	"not a version at all",
}

// stubs maps a --version output to the executable that prints it.
var stubs map[string]string

// TestMain writes every stub browser up front, before the first test — and
// therefore before anything in this process forks.
//
// Writing an executable and running it later is not safe to do while sibling
// tests are running: fork copies the writing test's still-open descriptor into
// the child, and for as long as that child has the file open for writing the
// kernel refuses to exec it. The result was a test that merely *reads* a stub
// failing with "text file busy" because an unrelated parallel test happened to
// be creating its own. Preparing them all first closes that window rather than
// retrying around it.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// The stubs are shell scripts, and wsaw does not support Windows;
		// fakeChrome skips there instead.
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "wsaw-browser-stubs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "preparing the stub browsers: %v\n", err)
		os.Exit(1)
	}

	stubs = make(map[string]string, len(stubVersions))

	for i, versionOutput := range stubVersions {
		path := filepath.Join(dir, "fake-chrome-"+strconv.Itoa(i))

		script := "#!/bin/sh\necho '" + versionOutput + "'\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
			fmt.Fprintf(os.Stderr, "writing the stub browser for %q: %v\n", versionOutput, err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}

		stubs[versionOutput] = path
	}

	code := m.Run()

	// Not deferred: os.Exit does not run deferred functions.
	_ = os.RemoveAll(dir)

	os.Exit(code)
}

// fakeChrome returns an executable stub that reports the given --version
// output, so version handling is testable without a real browser.
func fakeChrome(t *testing.T, versionOutput string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("wsaw does not support Windows")
	}

	path, ok := stubs[versionOutput]
	if !ok {
		t.Fatalf("no stub browser was prepared for %q; add it to stubVersions", versionOutput)
	}

	return path
}

func TestDiscoverExplicitPath(t *testing.T) {
	t.Parallel()

	path := fakeChrome(t, "Google Chrome 131.0.6778.85")

	info, err := browser.Discover(context.Background(), path)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if info.Path != path {
		t.Errorf("Path = %q, want %q", info.Path, path)
	}

	if info.Major != 131 {
		t.Errorf("Major = %d, want 131", info.Major)
	}

	if info.Version != "Google Chrome 131.0.6778.85" {
		t.Errorf("Version = %q", info.Version)
	}
}

func TestDiscoverParsesChromiumAndSnapOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		output string
		major  int
	}{
		{"Google Chrome 131.0.6778.85", 131},
		{"Chromium 130.0.6723.116 snap", 130},
		{"Chromium 124.0.6367.207 Arch Linux", 124},
		{"Google Chrome 140.0.7000.1 dev", 140},
	}

	for _, tc := range tests {
		info, err := browser.Discover(context.Background(), fakeChrome(t, tc.output))
		if err != nil {
			t.Errorf("Discover(%q): %v", tc.output, err)

			continue
		}

		if info.Major != tc.major {
			t.Errorf("Discover(%q).Major = %d, want %d", tc.output, info.Major, tc.major)
		}
	}
}

// TestDiscoverRejectsOldChrome covers the startup gate: an unsupported
// browser must fail immediately with an actionable message rather than
// producing results whose semantics differ from the schema.
func TestDiscoverRejectsOldChrome(t *testing.T) {
	t.Parallel()

	_, err := browser.Discover(context.Background(), fakeChrome(t, "Chromium 90.0.4430.212"))
	if !errors.Is(err, browser.ErrChromeTooOld) {
		t.Fatalf("err = %v, want ErrChromeTooOld", err)
	}

	// The message must name the requirement, not just say "too old".
	if got := err.Error(); !contains(got, "requires major") {
		t.Errorf("error message is not actionable: %s", got)
	}
}

func TestDiscoverExplicitPathMissing(t *testing.T) {
	t.Parallel()

	_, err := browser.Discover(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("Discover accepted a nonexistent path")
	}

	// An explicit path is never silently replaced by a different binary.
	if errors.Is(err, browser.ErrChromeNotFound) {
		t.Error("explicit path fell back to discovery instead of failing")
	}
}

func TestDiscoverRejectsDirectory(t *testing.T) {
	t.Parallel()

	if _, err := browser.Discover(context.Background(), t.TempDir()); err == nil {
		t.Fatal("Discover accepted a directory as a browser binary")
	}
}

func TestDiscoverUnparseableVersion(t *testing.T) {
	t.Parallel()

	_, err := browser.Discover(context.Background(), fakeChrome(t, "not a version at all"))
	if err == nil {
		t.Fatal("Discover accepted a binary with unparseable version output")
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()

	info := browser.Info{Path: "/usr/bin/chromium", Version: "Chromium 131.0.0.0", Major: 131}
	if got, want := info.String(), "Chromium 131.0.0.0 (/usr/bin/chromium)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	bare := browser.Info{Path: "/usr/bin/chromium"}
	if got := bare.String(); got != "/usr/bin/chromium" {
		t.Errorf("String() = %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}

		return false
	})()
}
