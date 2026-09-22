package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

func TestKeepPolicyIsParsed(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
store:
  keep:
    last: 10
    within: 72h
    hourly: 24
    daily: 14
    weekly: 8
    monthly: 12
    yearly: 3
    timezone: Europe/Berlin
`)

	k := cfg.Store.Keep
	if k == nil {
		t.Fatal("store.keep was not parsed")
	}

	if k.Last != 10 || k.Hourly != 24 || k.Daily != 14 || k.Weekly != 8 || k.Monthly != 12 || k.Yearly != 3 {
		t.Errorf("keep = %+v, want the configured counts", k)
	}

	policy, err := k.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}

	if policy.Within != 72*time.Hour {
		t.Errorf("Within = %v, want 72h", policy.Within)
	}

	if policy.Location == nil || policy.Location.String() != "Europe/Berlin" {
		t.Errorf("Location = %v, want Europe/Berlin", policy.Location)
	}
}

// TestKeepPolicyRefusesTheBoundsItReplaces: a maxPerSeries left in the file
// would cut a policy asked to keep five years back to a few hundred scans,
// and it would do it silently.
func TestKeepPolicyRefusesTheBoundsItReplaces(t *testing.T) {
	t.Parallel()

	for _, legacy := range []string{"  maxAge: 720h\n", "  maxPerSeries: 200\n"} {
		err := parseErr(t, "store:\n"+legacy+"  keep:\n    daily: 7\n")

		if !strings.Contains(err.Error(), "store.keep") {
			t.Errorf("error does not name store.keep: %v", err)
		}

		if !strings.Contains(err.Error(), strings.TrimSpace(strings.SplitN(legacy, ":", 2)[0])) {
			t.Errorf("error does not name the bound it replaces: %v", err)
		}
	}
}

// TestShippedDefaultDoesNotCountAsAConfiguredBound: maxPerSeries has a
// default, so its presence in the struct says nothing about the file.
func TestShippedDefaultDoesNotCountAsAConfiguredBound(t *testing.T) {
	t.Parallel()

	cfg := parse(t, "store:\n  keep:\n    daily: 7\n")

	if cfg.Store.Keep == nil {
		t.Fatal("store.keep was not parsed")
	}
}

func TestKeepPolicyMustKeepSomething(t *testing.T) {
	t.Parallel()

	err := parseErr(t, "store:\n  keep:\n    daily: 0\n")
	if !strings.Contains(err.Error(), "keeps nothing") {
		t.Errorf("error does not say the policy keeps nothing: %v", err)
	}
}

func TestKeepPolicyRejectsNegativeCountsAndUnknownZones(t *testing.T) {
	t.Parallel()

	err := parseErr(t, "store:\n  keep:\n    daily: -1\n")
	if !strings.Contains(err.Error(), "store.keep.daily") {
		t.Errorf("error does not name the negative field: %v", err)
	}

	err = parseErr(t, "store:\n  keep:\n    daily: 7\n    timezone: Mars/Olympus\n")
	if !strings.Contains(err.Error(), "store.keep.timezone") {
		t.Errorf("error does not name the timezone: %v", err)
	}
}

// TestKeepPolicyDefaultsToTheLocalZone keeps the common case configuration-free:
// an operator who says "daily" means their own day.
func TestKeepPolicyDefaultsToTheLocalZone(t *testing.T) {
	t.Parallel()

	k := config.Keep{Daily: 7}

	policy, err := k.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}

	if policy.Location != time.Local {
		t.Errorf("Location = %v, want the local zone", policy.Location)
	}
}

// TestKeepPolicyIsNotReloadable: the prune loop reads the policy when it
// starts, so a SIGHUP that edits it has to be refused rather than half
// applied — the same rule every other store setting follows.
func TestKeepPolicyIsNotReloadable(t *testing.T) {
	t.Parallel()

	running := parse(t, "store:\n  keep:\n    daily: 7\n")
	next := parse(t, "store:\n  keep:\n    daily: 14\n")

	changed := config.NonReloadableChanges(running, next)

	found := false

	for _, c := range changed {
		if c == "store.keep" {
			found = true
		}
	}

	if !found {
		t.Errorf("NonReloadableChanges = %v, want it to name store.keep", changed)
	}
}
