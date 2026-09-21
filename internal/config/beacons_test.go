package config_test

// Story 1.10: which requests a scan refuses to wait for is configuration, and
// it adds up — the shipped list, the global list, and a target's own rules.

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/capture"
)

func TestBeaconRulesAreShippedByDefault(t *testing.T) {
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

	beacons, err := capture.CompileBeacons(resolved[0].Beacons)
	if err != nil {
		t.Fatalf("CompileBeacons: %v", err)
	}

	if !beacons.Matches("https://trc-events.taboola.com/1419468/log/3/unip?tos=20", "trc-events.taboola.com") {
		t.Error("the shipped list did not apply; a first scan of an ordinary site would run to its hard timeout")
	}
}

func TestBeaconRulesAccumulate(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
capture:
  beacons:
    - host: telemetry.example.net
defaults:
  beacons:
    - urlPattern: '/everywhere-ping'
targets:
  - name: pings-itself
    url: https://example.com/
    beacons:
      - host: example.com
        urlPattern: '/api/session/ping'
  - name: plain
    url: https://plain.example.com/
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]capture.Beacons{}

	for _, r := range resolved {
		b, err := capture.CompileBeacons(r.Beacons)
		if err != nil {
			t.Fatalf("CompileBeacons for %s: %v", r.Name, err)
		}

		byName[r.Name] = b
	}

	self := byName["pings-itself"]

	for _, want := range []struct{ url, host string }{
		{"https://trc-events.taboola.com/log", "trc-events.taboola.com"},
		{"https://telemetry.example.net/collect", "telemetry.example.net"},
		{"https://example.com/everywhere-ping", "example.com"},
		{"https://example.com/api/session/ping", "example.com"},
	} {
		if !self.Matches(want.url, want.host) {
			t.Errorf("Matches(%q) = false, want every layer of rules to apply", want.url)
		}
	}

	// A target's own rule stays its own.
	if byName["plain"].Matches("https://example.com/api/session/ping", "example.com") {
		t.Error("a per-target rule leaked into another target")
	}

	// And the page itself is still waited for.
	if self.Matches("https://example.com/", "example.com") {
		t.Error("a host rule narrowed by path must not match the page itself")
	}
}

func TestDefaultBeaconsCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
capture:
  useDefaultBeacons: false
targets:
  - name: plain
    url: https://example.com/
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(resolved[0].Beacons) != 0 {
		t.Errorf("got %d rules, want none once the shipped list is declined", len(resolved[0].Beacons))
	}
}

// TestUnusableBeaconRulesAreRefusedAtLoad keeps the mistake at the restart
// rather than in a scan: a rule matching everything would end every scan the
// moment the page paused, and a pattern that does not compile would fail
// where it costs an observation.
func TestUnusableBeaconRulesAreRefusedAtLoad(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, want string
	}{
		{
			name: "global rule with neither host nor pattern",
			body: `
capture:
  beacons:
    - {}
targets:
  - name: plain
    url: https://example.com/
`,
			want: "capture.beacons[0]",
		},
		{
			name: "target rule with an invalid pattern",
			body: `
targets:
  - name: plain
    url: https://example.com/
    beacons:
      - urlPattern: 'collect('
`,
			want: "urlPattern",
		},
		{
			name: "defaults rule with neither host nor pattern",
			body: `
defaults:
  beacons:
    - {}
targets:
  - name: plain
    url: https://example.com/
`,
			want: "defaults.beacons[0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := parseErr(t, tc.body)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
