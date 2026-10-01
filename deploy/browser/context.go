// Package browser embeds the build context of the per-scan browser image, so
// that a released binary can build the image on the operator's machine and
// the project never has to distribute Chromium (Story 6.12).
//
// The embedded files are this directory's own, so there is one copy of each:
// the one CI builds, the Chromium workflows bump, and `wsaw browser build`
// runs.
package browser

import (
	"bufio"
	"bytes"
	"embed"
	"strings"
)

// Context is the image's build context: both build files and everything they
// copy.
//
//go:embed Dockerfile Containerfile entrypoint.sh
var Context embed.FS

// Base names the distribution an image is built from, and with it the build
// file and the shape of its Chromium package version.
type Base string

const (
	// BaseAlpine builds from the Dockerfile, the image wsaw uses by default.
	BaseAlpine Base = "alpine"
	// BaseDebian builds from the Containerfile, from a dated Debian snapshot,
	// for when Alpine lags Chrome stable.
	BaseDebian Base = "debian"
)

// Valid reports whether b is a base this package can build.
func (b Base) Valid() bool {
	return b == BaseAlpine || b == BaseDebian
}

// File is the build file for b, relative to Context.
func (b Base) File() string {
	if b == BaseDebian {
		return "Containerfile"
	}

	return "Dockerfile"
}

// ChromiumVersion is the chromium-headless-shell package version the build
// file for b installs, read from its CHROMIUM_VERSION argument. It is empty
// only if the file has lost that argument, which the package's tests refuse.
func ChromiumVersion(b Base) string {
	data, err := Context.ReadFile(b.File())
	if err != nil {
		// The files are embedded at compile time; they cannot be missing.
		return ""
	}

	const prefix = "ARG CHROMIUM_VERSION="

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), prefix); ok {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
