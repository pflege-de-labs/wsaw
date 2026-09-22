package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 5.27: the switch that lets somebody type a URL into the web interface,
// and what a typed URL is resolved into once it is through.

const adHocBase = `
targets:
  - name: site
    url: https://example.com/
api:
  enabled: true
`

func TestAdHocURLsAccepted(t *testing.T) {
	t.Parallel()

	cfg := parse(t, adHocBase+`
  adHocUrls:
    enabled: true
    consentModes: [reject, accept]
    maxPerHour: 5
`)

	if !cfg.API.AdHocURLs.Enabled {
		t.Fatal("adHocUrls.enabled did not survive parsing")
	}

	if got := cfg.API.AdHocURLs.Limit(); got != 5 {
		t.Errorf("Limit = %d, want 5", got)
	}

	modes := cfg.AdHocModes()
	if len(modes) != 2 || modes[0] != model.ConsentReject || modes[1] != model.ConsentAccept {
		t.Errorf("AdHocModes = %v, want reject and accept", modes)
	}
}

// Unset, the feature is off and takes the modes every other target gets.
func TestAdHocURLsDefaults(t *testing.T) {
	t.Parallel()

	cfg := parse(t, adHocBase)

	if cfg.API.AdHocURLs.Enabled {
		t.Error("typed URLs are enabled without anybody asking for them")
	}

	if got := cfg.API.AdHocURLs.Limit(); got != config.DefaultAdHocMaxPerHour {
		t.Errorf("Limit = %d, want the default %d", got, config.DefaultAdHocMaxPerHour)
	}

	modes := cfg.AdHocModes()
	if len(modes) != 1 || modes[0] != model.ConsentReject {
		t.Errorf("AdHocModes = %v, want the default reject", modes)
	}

	cfg = parse(t, `
defaults:
  consentModes: [none]
targets:
  - name: site
    url: https://example.com/
api:
  enabled: true
`)

	if modes := cfg.AdHocModes(); len(modes) != 1 || modes[0] != model.ConsentNone {
		t.Errorf("AdHocModes = %v, want the configured default of none", modes)
	}
}

// Each of these is a setting written to mean something that wsaw would
// otherwise ignore in silence.
func TestAdHocURLsRefusesAConfigurationThatCannotWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body, want string }{
		{
			"without the api", `
targets:
  - name: site
    url: https://example.com/
api:
  adHocUrls:
    enabled: true
`, "needs api.enabled",
		},
		{
			"in read-only mode", adHocBase + `
  readOnly: true
  adHocUrls:
    enabled: true
`, "readOnly",
		},
		{
			"without the web interface", adHocBase + `
  webui: false
  adHocUrls:
    enabled: true
`, "needs api.webui",
		},
		{
			"an unknown consent mode", adHocBase + `
  adHocUrls:
    enabled: true
    consentModes: [maybe]
`, "not a valid consent mode",
		},
		{
			"a negative budget", adHocBase + `
  adHocUrls:
    enabled: true
    maxPerHour: -1
`, "must not be negative",
		},
		{
			"settings without the switch", adHocBase + `
  adHocUrls:
    maxPerHour: 5
`, "set enabled: true or remove them",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := parseErr(t, tc.body)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A typed URL gets the deployment's defaults, because it is scanned by the
// same wsaw as everything else.
func TestResolveAdHocTargetAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
defaults:
  maxRequests: 42
  minInterval: 10m
targets:
  - name: site
    url: https://example.com/
`)

	r := cfg.ResolveAdHocTarget("example.org", "https://example.org/", model.ConsentAccept)

	if r.Name != "example.org" || r.URL != "https://example.org/" {
		t.Errorf("resolved %q at %q, want the name and URL it was given", r.Name, r.URL)
	}

	if len(r.ConsentModes) != 1 || r.ConsentModes[0] != model.ConsentAccept {
		t.Errorf("consent modes = %v, want only the mode asked for", r.ConsentModes)
	}

	if r.MaxRequests != 42 {
		t.Errorf("maxRequests = %d, want the configured default", r.MaxRequests)
	}

	if r.MinInterval != 10*time.Minute {
		t.Errorf("minInterval = %s, want the configured default", r.MinInterval)
	}
}

// What a typed URL does not get: the credentials the operator configured for
// their own sites. Sending those to an address somebody typed would hand them
// to whoever typed it.
func TestResolveAdHocTargetLeavesCredentialsBehind(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
defaults:
  basicAuthUser: operator
  basicAuthPassword: hunter2
  extraHeaders:
    X-Internal-Token: letmein
targets:
  - name: site
    url: https://example.com/
`)

	r := cfg.ResolveAdHocTarget("example.org", "https://example.org/", model.ConsentReject)

	if r.BasicAuthUser.IsSet() || r.BasicAuthPassword.IsSet() {
		t.Error("a typed URL was resolved with the deployment's basic auth credentials")
	}

	if len(r.ExtraHeaders) != 0 {
		t.Errorf("a typed URL was resolved with extra headers: %v", r.ExtraHeaders)
	}

	// The configured target still gets them, which is the point of the
	// setting.
	targets, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatalf("ResolveTargets: %v", err)
	}

	if !targets[0].BasicAuthUser.IsSet() || len(targets[0].ExtraHeaders) != 1 {
		t.Error("the configured target lost the defaults it is supposed to have")
	}
}

// A credential reference is not read on the typed-URL path at all, so an
// unresolvable one neither refuses the scan nor reports the reference — the
// name of an environment variable or the path of a key file is itself worth
// keeping out of a message that reaches whoever typed the address.
func TestResolveAdHocTargetReadsNoCredentialReference(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
defaults:
  basicAuthPassword: ${env:WSAW_DEFINITELY_NOT_SET}
targets:
  - name: site
    url: https://example.com/
`)

	r := cfg.ResolveAdHocTarget("example.org", "https://example.org/", model.ConsentReject)

	if r.BasicAuthPassword.IsSet() {
		t.Error("a typed URL was resolved with a credential")
	}

	// The configured target still fails at load, which is where a missing
	// environment variable belongs.
	if _, err := cfg.ResolveTargets(nil); err == nil {
		t.Error("ResolveTargets accepted an unresolvable credential reference")
	}
}
