package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

// The daemon's sweep schedule is configured in the store section, and this
// file is the specification of what it accepts (Story 4.12, AC1, AC2, AC9).

// TestSweepingIsOnDailyWhenTheSettingIsAbsent is the half of AC1 that decides
// whether the feature does anything at all: a file that says nothing about
// sweeping sweeps, once a day. Garbage that nobody collects grows without
// bound, so silence must not read as "off".
func TestSweepingIsOnDailyWhenTheSettingIsAbsent(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"no store section":             "",
		"a store section without them": "store:\n  maxPerSeries: 10\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := parse(t, body)

			if !cfg.Store.SweepEnabled() {
				t.Error("sweeping is off in a configuration that never mentioned it")
			}

			if got := cfg.Store.SweepEvery(); got != 24*time.Hour {
				t.Errorf("the default sweep interval is %s, want 24h", got)
			}
		})
	}

	// And the zero Config a caller builds in Go says the same thing, because
	// it is the same absence.
	if !config.New().Store.SweepEnabled() || config.New().Store.SweepEvery() != config.DefaultSweepInterval {
		t.Error("config.New() does not default to sweeping daily")
	}
}

// TestSweepingIsTurnedOffOnlyByWritingItDown is the other half of AC1.
func TestSweepingIsTurnedOffOnlyByWritingItDown(t *testing.T) {
	t.Parallel()

	off := parse(t, "store:\n  sweep: false\n")
	if off.Store.SweepEnabled() {
		t.Error("store.sweep: false left sweeping on")
	}

	on := parse(t, "store:\n  sweep: true\n  sweepInterval: 6h\n")
	if !on.Store.SweepEnabled() || on.Store.SweepEvery() != 6*time.Hour {
		t.Errorf("store.sweep: true, sweepInterval: 6h reads as enabled=%v every %s",
			on.Store.SweepEnabled(), on.Store.SweepEvery())
	}
}

// TestASweepIntervalThatCannotWorkIsRefusedWithItsLine is AC2: sub-hour and
// unparseable intervals are configuration errors, reported the way every
// other invalid store value is — naming the setting and the line.
func TestASweepIntervalThatCannotWorkIsRefusedWithItsLine(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body     string
		contains []string
	}{
		"shorter than an hour": {
			body:     "store:\n  sweepInterval: 30m\n",
			contains: []string{"line 2", "store.sweepInterval", "30m0s", "1h0m0s minimum"},
		},
		"zero, written down": {
			// Zero is neither the default nor off: it is refused, and the
			// message says how to spell each of the two.
			body:     "store:\n  sweepInterval: 0s\n",
			contains: []string{"line 2", "store.sweepInterval", "store.sweep: false"},
		},
		"not a duration": {
			body:     "store:\n  sweepInterval: daily\n",
			contains: []string{"line 2", `"daily" is not a valid duration`},
		},
		"negative": {
			body:     "store:\n  sweepInterval: -24h\n",
			contains: []string{"line 2", "must not be negative"},
		},
		"an interval with sweeping off": {
			body:     "store:\n  sweep: false\n  sweepInterval: 12h\n",
			contains: []string{"line 3", "store.sweepInterval", "(line 2)"},
		},
	}

	for name, tc := range cases {
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

// TestAnHourlySweepIsTheFloorAndIsAccepted keeps the boundary where AC2 puts
// it: an hour is allowed, only less is not.
func TestAnHourlySweepIsTheFloorAndIsAccepted(t *testing.T) {
	t.Parallel()

	cfg := parse(t, "store:\n  sweepInterval: 1h\n")

	if got := cfg.Store.SweepEvery(); got != time.Hour {
		t.Errorf("SweepEvery = %s, want 1h", got)
	}
}
