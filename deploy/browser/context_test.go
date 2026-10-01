package browser_test

import (
	"regexp"
	"testing"

	"github.com/pflege-de-labs/wsaw/deploy/browser"
)

// TestChromiumVersionIsTheBuildFiles guards the image names derived from it
// (Story 6.12, AC3): a build file that lost or reshaped the argument would
// otherwise leave the binary naming an image nobody can build.
func TestChromiumVersionIsTheBuildFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		base  browser.Base
		shape string
	}{
		{browser.BaseAlpine, `^[0-9]+(\.[0-9]+)*-r[0-9]+$`},
		{browser.BaseDebian, `^[0-9]+(\.[0-9]+)*-[0-9]+$`},
	}

	for _, tt := range tests {
		v := browser.ChromiumVersion(tt.base)
		if !regexp.MustCompile(tt.shape).MatchString(v) {
			t.Errorf("ChromiumVersion(%s) = %q, want a package version matching %s", tt.base, v, tt.shape)
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
