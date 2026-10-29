// Package browser embeds the build context of the per-scan browser image, so
// that a released binary can build the image on the operator's machine and
// the project never has to distribute Chromium (Story 6.12).
//
// The embedded files are this directory's own, so there is one copy of each:
// the one CI builds and `wsaw browser build` runs.
package browser

import "embed"

// Context is the image's build context: both build files and everything they
// copy.
//
//go:embed Dockerfile Containerfile entrypoint.sh
var Context embed.FS

// Base names the distribution an image is built from, and with it the build
// file.
type Base string

const (
	// BaseAlpine builds from the Dockerfile, the image wsaw uses by default.
	BaseAlpine Base = "alpine"
	// BaseDebian builds from the Containerfile, from Debian testing, for when
	// Alpine lags Chrome stable.
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
