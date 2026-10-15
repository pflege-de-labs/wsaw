package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

// The browser image refresh is configured under browser.container, and this
// file is the specification of what it accepts (Story 6.13, AC1, AC3, AC9).

// TestRefreshIsOffByDefault is AC1: a file that says nothing about refreshing
// leaves the image exactly where the operator put it.
func TestRefreshIsOffByDefault(t *testing.T) {
	t.Parallel()

	for _, ct := range []config.ContainerBrowser{
		parse(t, "").Browser.Container,
		config.New().Browser.Container,
	} {
		if got := ct.RefreshMode(); got != config.RefreshOff {
			t.Errorf("RefreshMode() = %q, want off", got)
		}

		if got := ct.RefreshEvery(); got != config.DefaultRefreshInterval {
			t.Errorf("RefreshEvery() = %s, want %s", got, config.DefaultRefreshInterval)
		}

		if ct.KeepReferenced() || ct.KeepCount() != 2 {
			t.Errorf("the default keep rule is referenced=%v count=%d, want the newest 2",
				ct.KeepReferenced(), ct.KeepCount())
		}
	}
}

func TestRefreshSettingsAreRead(t *testing.T) {
	t.Parallel()

	ct := parse(t, "browser:\n  container:\n    refresh: build\n    refreshInterval: 24h\n    refreshKeep: 3\n").
		Browser.Container

	if ct.RefreshMode() != config.RefreshBuild || ct.RefreshEvery() != 24*time.Hour || ct.KeepCount() != 3 {
		t.Errorf("read mode=%s every=%s keep=%d", ct.RefreshMode(), ct.RefreshEvery(), ct.KeepCount())
	}

	ref := parse(t, "browser:\n  container:\n    refreshKeepReferenced: true\n").Browser.Container
	if !ref.KeepReferenced() {
		t.Error("refreshKeepReferenced: true was not read")
	}

	off := parse(t, "browser:\n  container:\n    refreshKeepReferenced: false\n").Browser.Container
	if off.KeepReferenced() || off.KeepCount() != config.DefaultRefreshKeep {
		t.Error("refreshKeepReferenced: false on its own does not mean the default rule")
	}
}

// TestRefreshSettingsThatCannotWorkAreRefused covers AC1's validation, AC3's
// refusal of a digest, and AC9's mutually exclusive keep rules, each named by
// the line to change.
func TestRefreshSettingsThatCannotWorkAreRefused(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body     string
		contains []string
	}{
		"unknown mode": {
			body:     "browser:\n  container:\n    refresh: weekly\n",
			contains: []string{"line 3", "browser.container.refresh", `"weekly"`},
		},
		"interval under an hour": {
			body:     "browser:\n  container:\n    refreshInterval: 30m\n",
			contains: []string{"line 3", "browser.container.refreshInterval", "at least 1h0m0s"},
		},
		"both keep rules": {
			body: "browser:\n  container:\n    refreshKeep: 3\n    refreshKeepReferenced: true\n",
			contains: []string{
				"line 3", "browser.container.refreshKeep", "refreshKeepReferenced (line 4)", "keep one of the two",
			},
		},
		"both keep rules, whatever their values": {
			body:     "browser:\n  container:\n    refreshKeep: 2\n    refreshKeepReferenced: false\n",
			contains: []string{"browser.container.refreshKeep", "keep one of the two"},
		},
		"keep zero": {
			body:     "browser:\n  container:\n    refreshKeep: 0\n",
			contains: []string{"line 3", "browser.container.refreshKeep", "at least 1"},
		},
		"pull of a digest": {
			body:     "browser:\n  container:\n    image: ghcr.io/x/browser@sha256:abc\n    refresh: pull\n",
			contains: []string{"line 3", "browser.container.image", "pinned by digest"},
		},
		"pull of the image wsaw builds": {
			body:     "browser:\n  container:\n    refresh: pull\n",
			contains: []string{"line 3", "browser.container.refresh", "refresh: build"},
		},
		"misspelled key": {
			body:     "browser:\n  container:\n    refreshKeepReferencd: true\n",
			contains: []string{"refreshKeepReferencd"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			msg := parseErr(t, tc.body).Error()

			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

func TestPullOfARegistryTagIsAccepted(t *testing.T) {
	t.Parallel()

	ct := parse(t, "browser:\n  container:\n    image: ghcr.io/x/browser:stable\n    refresh: pull\n").Browser.Container
	if ct.RefreshMode() != config.RefreshPull {
		t.Errorf("RefreshMode() = %q, want pull", ct.RefreshMode())
	}
}
