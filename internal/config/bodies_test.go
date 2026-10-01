package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func resolvedBodies(t *testing.T, body string) map[string]config.BodyPolicy {
	t.Helper()

	resolved, err := parse(t, body).ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string]config.BodyPolicy, len(resolved))
	for _, r := range resolved {
		out[r.Name] = r.Bodies
	}

	return out
}

// Story 1.11, AC1–AC3: one shape at both levels, merged field by field, with
// storeBodies as shorthand and storage off unless something turns it on.
func TestBodyPolicyResolution(t *testing.T) {
	t.Parallel()

	got := resolvedBodies(t, `
defaults:
  bodies:
    store: all
    ratio: 0.05
    requestBodies: true
targets:
  - name: inherits
    url: https://a.example/
  - name: overrides-ratio
    url: https://b.example/
    bodies:
      ratio: 1
      ratioWindow: 24h
  - name: turns-it-off
    url: https://c.example/
    storeBodies: false
  - name: shorthand
    url: https://d.example/
    storeBodies: true
`)

	want := map[string]config.BodyPolicy{
		"inherits": {
			Store: model.BodyStoreAll, Ratio: 0.05, RatioWindow: config.DefaultBodyRatioWindow,
			RequestBodies: true, MaxBodyBytes: 8 << 20, MaxScanBytes: config.DefaultMaxScanBytes,
		},
		"overrides-ratio": {
			Store: model.BodyStoreAll, Ratio: 1, RatioWindow: 24 * time.Hour,
			RequestBodies: true, MaxBodyBytes: 8 << 20, MaxScanBytes: config.DefaultMaxScanBytes,
		},
		"turns-it-off": {
			Store: model.BodyStoreNone, Ratio: 0.05, RatioWindow: config.DefaultBodyRatioWindow,
			RequestBodies: true, MaxBodyBytes: 8 << 20, MaxScanBytes: config.DefaultMaxScanBytes,
		},
		"shorthand": {
			Store: model.BodyStoreHashed, Ratio: 1, RatioWindow: config.DefaultBodyRatioWindow,
			RequestBodies: true, MaxBodyBytes: 8 << 20, MaxScanBytes: config.DefaultMaxScanBytes,
		},
	}

	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %+v, want %+v", name, got[name], w)
		}
	}

	if got["turns-it-off"].Enabled() {
		t.Error("storeBodies: false on a target did not turn storage off")
	}
}

// AC2: an existing configuration stores exactly what it stored before.
func TestStoreBodiesKeepsItsMeaning(t *testing.T) {
	t.Parallel()

	got := resolvedBodies(t, `
defaults:
  storeBodies: true
targets:
  - name: site
    url: https://a.example/
`)["site"]

	if got.Store != model.BodyStoreHashed || got.Ratio != 1 || !got.Enabled() {
		t.Errorf("storeBodies: true resolved to %+v, want store hashed at ratio 1", got)
	}

	if got.RequestBodies {
		t.Error("storeBodies: true started storing request payloads, which it never did")
	}
}

// AC3: with nothing configured, nothing is stored.
func TestBodiesAreOffByDefault(t *testing.T) {
	t.Parallel()

	got := resolvedBodies(t, `
targets:
  - name: site
    url: https://a.example/
`)["site"]

	if got.Enabled() || got.Store != model.BodyStoreNone {
		t.Errorf("default body policy = %+v, want store none", got)
	}
}

func TestBodiesValidation(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ body, want string }{
		"both on one level": {`
targets:
  - name: site
    url: https://a.example/
    storeBodies: true
    bodies: {store: all}
`, "both storeBodies and bodies are set"},
		"both on defaults": {`
defaults:
  storeBodies: true
  bodies: {store: all}
targets:
  - name: site
    url: https://a.example/
`, "defaults"},
		"unknown mode": {`
targets:
  - name: site
    url: https://a.example/
    bodies: {store: everything}
`, "bodies.store"},
		"ratio above one": {`
targets:
  - name: site
    url: https://a.example/
    bodies: {store: all, ratio: 1.5}
`, "bodies.ratio"},
		"negative ratio": {`
targets:
  - name: site
    url: https://a.example/
    bodies: {store: all, ratio: -0.1}
`, "bodies.ratio"},
		"body cap above scan cap": {`
targets:
  - name: site
    url: https://a.example/
    bodies: {store: all, maxBodyBytes: 2000, maxScanBytes: 1000}
`, "bodies.maxBodyBytes"},
		"window shorter than interval": {`
targets:
  - name: site
    url: https://a.example/
    interval: 24h
    bodies: {store: all, ratio: 0.1, ratioWindow: 12h}
`, "bodies.ratioWindow"},
		"negative scan cap": {`
targets:
  - name: site
    url: https://a.example/
    bodies: {store: all, maxScanBytes: -1}
`, "bodies.maxScanBytes"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := parseErr(t, c.body)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// A window shorter than the interval is fine when the ratio is 0 or 1: there
// is no ratio to express, so there is nothing to refuse.
func TestAShortWindowIsFineWithoutARatio(t *testing.T) {
	t.Parallel()

	parse(t, `
targets:
  - name: site
    url: https://a.example/
    interval: 24h
    bodies: {store: all, ratio: 1, ratioWindow: 1h}
`)
}
