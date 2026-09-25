package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The daemon's vacuum schedule is configured in the store section beside the
// sweep's, and this file is the specification of what it accepts (Story 4.13,
// AC3, AC4, AC13).

// TestVacuumingIsOnWeeklyWhenTheSettingIsAbsent: a file that says nothing
// about vacuuming vacuums, once a week, at the default threshold.
func TestVacuumingIsOnWeeklyWhenTheSettingIsAbsent(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]*config.Config{
		"no store section":             parse(t, ""),
		"a store section without them": parse(t, "store:\n  maxPerSeries: 10\n"),
		"config.New()":                 config.New(),
	} {
		if !cfg.Store.VacuumEnabled() {
			t.Errorf("%s: vacuuming is off in a configuration that never mentioned it", name)
		}

		if got := cfg.Store.VacuumEvery(); got != 7*24*time.Hour {
			t.Errorf("%s: the default vacuum interval is %s, want 168h", name, got)
		}

		if got := cfg.Store.VacuumFreeRatio(); got != store.DefaultVacuumMinFreeRatio {
			t.Errorf("%s: the default free ratio is %g, want %g", name, got, store.DefaultVacuumMinFreeRatio)
		}
	}
}

// TestVacuumingIsConfiguredByWritingItDown: off only when written, and an
// interval and threshold as written.
func TestVacuumingIsConfiguredByWritingItDown(t *testing.T) {
	t.Parallel()

	if parse(t, "store:\n  vacuum: false\n").Store.VacuumEnabled() {
		t.Error("store.vacuum: false left vacuuming on")
	}

	on := parse(t, "store:\n  vacuum: true\n  vacuumInterval: 24h\n  vacuumMinFreeRatio: 0.5\n")
	if !on.Store.VacuumEnabled() || on.Store.VacuumEvery() != 24*time.Hour || on.Store.VacuumFreeRatio() != 0.5 {
		t.Errorf("read as enabled=%v every %s at %g", on.Store.VacuumEnabled(), on.Store.VacuumEvery(),
			on.Store.VacuumFreeRatio())
	}

	if got := parse(t, "store:\n  vacuumInterval: 1h\n  vacuumMinFreeRatio: 1\n").Store; got.VacuumEvery() != time.Hour ||
		got.VacuumFreeRatio() != 1 {
		t.Errorf("the boundaries were not accepted: every %s at %g", got.VacuumEvery(), got.VacuumFreeRatio())
	}
}

// TestAVacuumScheduleThatCannotWorkIsRefusedWithItsLine is AC3 and AC4's
// validation: sub-hour, zero and unparseable intervals and an out-of-range
// threshold are refused, naming the setting and the line.
func TestAVacuumScheduleThatCannotWorkIsRefusedWithItsLine(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body     string
		contains []string
	}{
		"shorter than an hour": {
			body:     "store:\n  vacuumInterval: 30m\n",
			contains: []string{"line 2", "store.vacuumInterval", "30m0s", "1h0m0s minimum"},
		},
		"zero, written down": {
			body:     "store:\n  vacuumInterval: 0s\n",
			contains: []string{"line 2", "store.vacuumInterval", "store.vacuum: false"},
		},
		"not a duration": {
			body:     "store:\n  vacuumInterval: weekly\n",
			contains: []string{"line 2", `"weekly" is not a valid duration`},
		},
		"a ratio of zero": {
			body:     "store:\n  vacuumMinFreeRatio: 0\n",
			contains: []string{"line 2", "store.vacuumMinFreeRatio", "outside (0, 1]"},
		},
		"a ratio above one": {
			body:     "store:\n  vacuumMinFreeRatio: 1.5\n",
			contains: []string{"line 2", "store.vacuumMinFreeRatio", "1.5"},
		},
		"a negative ratio": {
			body:     "store:\n  vacuumMinFreeRatio: -0.1\n",
			contains: []string{"line 2", "store.vacuumMinFreeRatio"},
		},
		"settings with vacuuming off": {
			body:     "store:\n  vacuum: false\n  vacuumInterval: 12h\n  vacuumMinFreeRatio: 0.3\n",
			contains: []string{"line 3", "store.vacuumInterval", "line 4", "store.vacuumMinFreeRatio", "(line 2)"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := parseErr(t, tc.body)

			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the rejection %q does not mention %q", err, want)
				}
			}
		})
	}
}
