package browser_test

import (
	"regexp"
	"testing"

	"github.com/pflege-de-labs/wsaw/deploy/browser"
)

// TestBuildFilesDoNotPinChromium: the distributions keep only their newest
// Chromium, so a pinned version stops resolving with the next security
// release, and the image could then not be built at all.
func TestBuildFilesDoNotPinChromium(t *testing.T) {
	t.Parallel()

	pinned := regexp.MustCompile(`CHROMIUM_VERSION|chromium(-headless-shell|-common)?[=<>~]|snapshot\.debian\.org`)

	for _, b := range []browser.Base{browser.BaseAlpine, browser.BaseDebian} {
		data, err := browser.Context.ReadFile(b.File())
		if err != nil {
			t.Fatal(err)
		}

		if m := pinned.Find(data); m != nil {
			t.Errorf("%s pins Chromium: %q", b.File(), m)
		}
	}
}

func TestBaseValid(t *testing.T) {
	t.Parallel()

	for b, want := range map[browser.Base]bool{
		browser.BaseAlpine: true, browser.BaseDebian: true, "ubuntu": false, "": false,
	} {
		if got := b.Valid(); got != want {
			t.Errorf("Base(%q).Valid() = %v, want %v", b, got, want)
		}
	}
}
