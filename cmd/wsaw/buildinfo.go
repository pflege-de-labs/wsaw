package main

import "runtime/debug"

// Placeholders the build variables hold when no linker flags set them.
const (
	unstampedVersion = "dev"
	unstampedValue   = "unknown"
)

// shortCommitLen matches the abbreviation Go uses in a pseudo-version, so the
// commit and the version name the same hash.
const shortCommitLen = 12

// buildInfo is what `wsaw version` prints and what results record as the
// writing build.
type buildInfo struct {
	version, commit, date string
}

// withVCSFallback fills whatever the linker left unset from the version
// control information the go command embeds on its own.
//
// Release builds stamp all three values through -X, and those always win. A
// plain `go build` or `go install` in a checkout passes no flags, but still
// records the module's pseudo-version (with a +dirty suffix for a modified
// tree) and the commit; without this, such a binary claims to be "dev" and
// nobody can tell which code it runs. `go run` and -buildvcs=false embed
// nothing, and those builds keep the placeholders.
func withVCSFallback(stamped buildInfo, bi *debug.BuildInfo) buildInfo {
	if bi == nil {
		return stamped
	}

	out := stamped

	// "(devel)" is how the go command says it did not know a version either.
	if out.version == unstampedVersion && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		out.version = bi.Main.Version
	}

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if out.commit == unstampedValue && s.Value != "" {
				out.commit = s.Value[:min(shortCommitLen, len(s.Value))]
			}
		case "vcs.time":
			if out.date == unstampedValue && s.Value != "" {
				out.date = s.Value
			}
		}
	}

	return out
}

// applyVCSFallback rewrites the build variables from the running binary's
// embedded build information. It runs once, before any command reads them.
func applyVCSFallback() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	info := withVCSFallback(buildInfo{version: version, commit: commit, date: date}, bi)
	version, commit, date = info.version, info.commit, info.date
}
