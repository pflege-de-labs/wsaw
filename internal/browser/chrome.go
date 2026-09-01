// Package browser discovers, launches, and supervises Chrome.
//
// Chrome is treated as a hostile, disposable subprocess (Tenet 1): every
// interaction has a deadline, every profile directory is removed on every
// exit path, and a browser that stops answering is killed and replaced rather
// than waited on.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// MinChromeMajor is the oldest Chrome major version wsaw supports. Below this
// the CDP surface wsaw relies on differs enough that results would not mean
// what the schema says they mean.
const MinChromeMajor = 120

// ErrChromeNotFound is returned when no usable browser binary exists.
var ErrChromeNotFound = errors.New("no Chrome or Chromium binary found")

// ErrChromeTooOld is returned when the discovered browser predates
// MinChromeMajor.
var ErrChromeTooOld = errors.New("the browser version is too old")

// Info describes a discovered browser binary.
type Info struct {
	Path    string
	Version string
	Major   int
}

// String renders the browser identity for logs and results.
func (i Info) String() string {
	if i.Version == "" {
		return i.Path
	}

	return i.Version + " (" + i.Path + ")"
}

// candidateNames are the executable names to look for on PATH, most
// preferred first. Chromium is listed alongside Chrome because the container
// image ships Chromium for redistribution reasons.
var candidateNames = []string{
	"google-chrome-stable",
	"google-chrome",
	"chromium-browser",
	"chromium",
	"chrome",
}

// wellKnownPaths lists per-platform install locations checked after PATH.
func wellKnownPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()

		paths := []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/opt/homebrew/bin/chromium",
			"/usr/local/bin/chromium",
		}

		if home != "" {
			paths = append(paths,
				filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
				filepath.Join(home, "Applications/Chromium.app/Contents/MacOS/Chromium"),
			)
		}

		return paths

	case "linux":
		return []string{
			"/usr/bin/google-chrome-stable",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium-browser",
			"/usr/bin/chromium",
			"/snap/bin/chromium",
			"/usr/lib/chromium/chromium",
			"/usr/lib/chromium-browser/chromium-browser",
		}

	default:
		// wsaw supports Linux and macOS only; other platforms get PATH lookup
		// and a clear error if that fails.
		return nil
	}
}

// Discover locates a usable browser. An explicit path is used as given and
// never silently replaced by a different binary, so an operator who pins a
// build gets that build or an error.
//
// The version check happens here, at startup, rather than at first scan, so a
// misconfigured host fails immediately with an actionable message (NFR §3).
func Discover(ctx context.Context, explicitPath string) (Info, error) {
	if explicitPath != "" {
		info, err := probe(ctx, explicitPath)
		if err != nil {
			return Info{}, fmt.Errorf("configured Chrome path %s: %w", explicitPath, err)
		}

		return info, checkVersion(info)
	}

	var tried []string

	for _, name := range candidateNames {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}

		info, err := probe(ctx, path)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%v)", path, err))

			continue
		}

		return info, checkVersion(info)
	}

	for _, path := range wellKnownPaths() {
		if _, err := os.Stat(path); err != nil {
			continue
		}

		info, err := probe(ctx, path)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%v)", path, err))

			continue
		}

		return info, checkVersion(info)
	}

	msg := fmt.Sprintf("%v: looked for %s on PATH and in the standard %s locations",
		ErrChromeNotFound, strings.Join(candidateNames, ", "), runtime.GOOS)

	if len(tried) > 0 {
		msg += "; rejected: " + strings.Join(tried, ", ")
	}

	msg += ". Install Chrome or Chromium, or set --chrome-path"

	return Info{}, errors.New(msg)
}

// versionPattern extracts the version from output such as
// "Google Chrome 131.0.6778.85" or "Chromium 130.0.6723.116 snap".
var versionPattern = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

// probe runs the binary with --version to confirm it is executable and to
// read its version.
func probe(ctx context.Context, path string) (Info, error) {
	if fi, err := os.Stat(path); err != nil {
		return Info{}, fmt.Errorf("not usable: %w", err)
	} else if fi.IsDir() {
		return Info{}, errors.New("not usable: path is a directory")
	}

	// #nosec G204 -- path comes from operator configuration or a fixed
	// candidate list, never from a scanned page.
	cmd := exec.CommandContext(ctx, path, "--version")

	out, err := cmd.Output()
	if err != nil {
		return Info{}, fmt.Errorf("running --version: %w", err)
	}

	version := strings.TrimSpace(string(out))

	m := versionPattern.FindStringSubmatch(version)
	if m == nil {
		return Info{}, fmt.Errorf("could not parse version from %q", version)
	}

	major, err := strconv.Atoi(m[1])
	if err != nil {
		return Info{}, fmt.Errorf("could not parse major version from %q: %w", version, err)
	}

	return Info{Path: path, Version: version, Major: major}, nil
}

func checkVersion(info Info) error {
	if info.Major < MinChromeMajor {
		return fmt.Errorf("%w: %s at %s is major %d, wsaw requires major %d or newer",
			ErrChromeTooOld, info.Version, info.Path, info.Major, MinChromeMajor)
	}

	return nil
}
