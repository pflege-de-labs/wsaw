package config_test

// Story 2.9, AC3: how long to wait for a banner to appear is a deployment
// decision with a per-target override, because a slow front end is a property
// of one site rather than of the tool.

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/consent"
)

func TestConsentBannerWaitFallsBackToTheBuiltinDefault(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: plain
    url: https://example.com/
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if resolved[0].ConsentBannerWait != consent.DefaultBannerWait {
		t.Errorf("banner wait = %s, want the built-in default %s",
			resolved[0].ConsentBannerWait, consent.DefaultBannerWait)
	}
}

func TestConsentBannerWaitIsOverridablePerTarget(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
consent:
  bannerWait: 2s
defaults:
  consentBannerWait: 4s
targets:
  - name: inherits-defaults
    url: https://example.com/
  - name: slow-front-end
    url: https://slow.example.com/
    consentBannerWait: 12s
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]time.Duration{}
	for _, r := range resolved {
		byName[r.Name] = r.ConsentBannerWait
	}

	if byName["inherits-defaults"] != 4*time.Second {
		t.Errorf("inherited wait = %s, want the defaults block's 4s", byName["inherits-defaults"])
	}

	if byName["slow-front-end"] != 12*time.Second {
		t.Errorf("target wait = %s, want the target's own 12s", byName["slow-front-end"])
	}
}

func TestConsentBannerWaitUsesTheDeploymentWideSettingWhenNoDefaultIsSet(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
consent:
  bannerWait: 9s
targets:
  - name: plain
    url: https://example.com/
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if resolved[0].ConsentBannerWait != 9*time.Second {
		t.Errorf("banner wait = %s, want the consent block's 9s", resolved[0].ConsentBannerWait)
	}
}
