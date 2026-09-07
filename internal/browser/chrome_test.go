package browser_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/browser"
)

// fakeChrome writes an executable stub that reports the given --version
// output, so version handling is testable without a real browser.
func fakeChrome(t *testing.T, versionOutput string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("wsaw does not support Windows")
	}

	path := filepath.Join(t.TempDir(), "fake-chrome")

	script := "#!/bin/sh\necho '" + versionOutput + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
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
