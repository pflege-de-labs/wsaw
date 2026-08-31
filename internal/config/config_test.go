package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/config"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/secret"
)

func parse(t *testing.T, body string) *config.Config {
	t.Helper()

	cfg, err := config.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	return cfg
}

func parseErr(t *testing.T, body string) error {
	t.Helper()

	_, err := config.Parse([]byte(body))
	if err == nil {
		t.Fatal("Parse accepted an invalid configuration")
	}

	return err
}

func TestMinimalConfig(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: example
    url: https://example.com/
`)

	if len(cfg.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(cfg.Targets))
	}

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	r := resolved[0]

	// The default consent mode is reject: the state that produces the
	// compliance finding wsaw exists for.
	if len(r.ConsentModes) != 1 || r.ConsentModes[0] != model.ConsentReject {
		t.Errorf("default consent modes = %v, want [reject]", r.ConsentModes)
	}

	if r.Robots != config.RobotsIgnore {
		t.Errorf("default robots policy = %q, want ignore", r.Robots)
	}

	if r.HardTimeout == 0 || r.IdleQuiet == 0 {
		t.Error("capture budget defaults were not applied")
	}
}

// TestUnknownFieldIsRejected: a misspelled key that is silently ignored looks
// exactly like a setting that does not work.
func TestUnknownFieldIsRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: example
    url: https://example.com/
    conentModes: [reject]
`)

	if !strings.Contains(err.Error(), "conentModes") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestValidationReportsLineNumbers(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: good
    url: https://example.com/
  - name: bad
    url: "ftp://example.com/"
`)

	if !strings.Contains(err.Error(), "line 5") {
		t.Errorf("error does not point at the offending target's line: %v", err)
	}
}

func TestAllProblemsAreReportedTogether(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: one
  - name: two
    url: "not a url with spaces"
    consentModes: [maybe]
`)

	// Fixing one error per restart is a bad operator experience, so every
	// problem is collected in one pass.
	if !strings.Contains(err.Error(), "problems") {
		t.Errorf("errors were not collected: %v", err)
	}
}

func TestDuplicateTargetNameIsRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: dup
    url: https://a.example.com/
  - name: dup
    url: https://b.example.com/
`)

	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate names not reported: %v", err)
	}
}

// TestCredentialsInURLAreRejected: they would be stored in every result and
// logged on every scan.
func TestCredentialsInURLAreRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: private
    url: https://user:pass@example.com/
`)

	if !strings.Contains(err.Error(), "basicAuth") {
		t.Errorf("error does not point at the alternative: %v", err)
	}
}

func TestNonHTTPSchemeIsRejected(t *testing.T) {
	t.Parallel()

	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "chrome://settings"} {
		_, err := config.Parse([]byte("targets:\n  - name: t\n    url: \"" + u + "\"\n"))
		if err == nil {
			t.Errorf("Parse accepted %q as a target URL", u)
		}
	}
}

func TestIntervalAndCronTogetherIsRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: t
    url: https://example.com/
    interval: 1h
    cron: "0 * * * *"
`)

	if !strings.Contains(err.Error(), "interval and cron") {
		t.Errorf("ambiguous schedule not reported: %v", err)
	}
}

func TestInvalidCronIsRejected(t *testing.T) {
	t.Parallel()

	parseErr(t, `
targets:
  - name: t
    url: https://example.com/
    cron: "not a cron"
`)
}

func TestIdleQuietMustBeShorterThanHardTimeout(t *testing.T) {
	t.Parallel()

	// Otherwise every scan ends on the hard timeout and never reports a clean
	// idle termination.
	err := parseErr(t, `
targets:
  - name: t
    url: https://example.com/
    idleQuiet: 60s
    hardTimeout: 30s
`)

	if !strings.Contains(err.Error(), "idleQuiet") {
		t.Errorf("error does not name the field: %v", err)
	}
}

func TestInvalidDurationNamesTheLine(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
targets:
  - name: t
    url: https://example.com/
    hardTimeout: "45 seconds"
`)

	if !strings.Contains(err.Error(), "line") {
		t.Errorf("duration error has no line reference: %v", err)
	}
}

// TestRemoteListenRequiresToken: exposing results, which can contain personal
// data, to the network without authentication must not be possible by accident.
func TestRemoteListenRequiresToken(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
api:
  enabled: true
  listen: "0.0.0.0:8712"
targets:
  - name: t
    url: https://example.com/
`)

	if !strings.Contains(err.Error(), "authentication") {
		t.Errorf("error does not explain the requirement: %v", err)
	}
}

func TestLoopbackListenNeedsNoToken(t *testing.T) {
	t.Parallel()

	for _, listen := range []string{"127.0.0.1:8712", "localhost:8712", "[::1]:8712", ":8712"} {
		_, err := config.Parse([]byte("api:\n  enabled: true\n  listen: \"" + listen + "\"\n"))
		if err != nil {
			t.Errorf("loopback listen %q was rejected: %v", listen, err)
		}
	}
}

func TestDefaultsAreInherited(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
defaults:
  consentModes: [none, reject, accept]
  hardTimeout: 90s
  labels:
    team: platform
  robots: respect
targets:
  - name: inherits
    url: https://a.example.com/
  - name: overrides
    url: https://b.example.com/
    hardTimeout: 20s
    consentModes: [reject]
    labels:
      env: prod
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	inherits, overrides := resolved[0], resolved[1]

	if len(inherits.ConsentModes) != 3 {
		t.Errorf("inherited consent modes = %v", inherits.ConsentModes)
	}

	if inherits.HardTimeout != 90*time.Second {
		t.Errorf("inherited hardTimeout = %v, want 90s", inherits.HardTimeout)
	}

	if inherits.Robots != config.RobotsRespect {
		t.Errorf("inherited robots = %q, want respect", inherits.Robots)
	}

	if overrides.HardTimeout != 20*time.Second {
		t.Errorf("overridden hardTimeout = %v, want 20s", overrides.HardTimeout)
	}

	if len(overrides.ConsentModes) != 1 {
		t.Errorf("overridden consent modes = %v, want [reject]", overrides.ConsentModes)
	}

	// Labels merge rather than replace, so a global team label survives.
	if overrides.Labels["team"] != "platform" || overrides.Labels["env"] != "prod" {
		t.Errorf("labels did not merge: %v", overrides.Labels)
	}
}

// TestTargetCronClearsInheritedInterval: the two schedule kinds must never
// silently combine.
func TestTargetCronClearsInheritedInterval(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
defaults:
  interval: 1h
targets:
  - name: cronned
    url: https://example.com/
    cron: "0 3 * * *"
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if resolved[0].Cron != "0 3 * * *" {
		t.Errorf("cron = %q", resolved[0].Cron)
	}

	if resolved[0].Interval != 0 {
		t.Errorf("interval = %v, want zero when a cron is set", resolved[0].Interval)
	}
}

func TestDisabledTargetsAreNotResolved(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: on
    url: https://a.example.com/
  - name: off
    url: https://b.example.com/
    disabled: true
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(resolved) != 1 || resolved[0].Name != "on" {
		t.Errorf("resolved = %+v, want only the enabled target", resolved)
	}

	// The disabled target stays in the config so its history and baseline
	// survive.
	if len(cfg.Targets) != 2 {
		t.Error("disabled target was dropped from the config")
	}
}

func TestSecretReferencesResolveAtLoad(t *testing.T) {
	t.Setenv("WSAW_TEST_PASS", "s3cret-value")

	cfg, err := config.Parse([]byte(`
targets:
  - name: t
    url: https://example.com/
    basicAuthUser: admin
    basicAuthPassword: "${env:WSAW_TEST_PASS}"
`))
	if err != nil {
		t.Fatal(err)
	}

	var reg secret.Registry

	resolved, err := cfg.ResolveTargets(&reg)
	if err != nil {
		t.Fatal(err)
	}

	if got := resolved[0].BasicAuthPassword.Reveal(); got != "s3cret-value" {
		t.Errorf("password = %q", got)
	}

	// Registered secrets can be scrubbed out of error text later.
	if scrubbed := reg.Scrub("failed with s3cret-value"); strings.Contains(scrubbed, "s3cret-value") {
		t.Error("resolved secret was not registered for scrubbing")
	}
}

// TestMissingSecretFailsAtLoadNotMidScan is why resolution happens here: a
// missing environment variable must stop startup, not break the third scan.
func TestMissingSecretFailsAtLoadNotMidScan(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`
targets:
  - name: t
    url: https://example.com/
    basicAuthPassword: "${env:WSAW_DEFINITELY_UNSET}"
`))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.ResolveTargets(nil); err == nil {
		t.Fatal("ResolveTargets accepted an unresolvable secret")
	}
}

func TestNormalizeRulesIncludeShippedNoiseListByDefault(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
normalize:
  dropQueryParams: [custom_param]
`)

	rules, err := cfg.NormalizeRules()
	if err != nil {
		t.Fatal(err)
	}

	var hasDefault, hasCustom bool

	for _, p := range rules.DropQueryParams {
		if p == "utm_source" {
			hasDefault = true
		}

		if p == "custom_param" {
			hasCustom = true
		}
	}

	if !hasDefault {
		t.Error("shipped noise parameters were not included; the first scan of a real site would be all churn")
	}

	if !hasCustom {
		t.Error("configured parameter was dropped")
	}
}

func TestNormalizeRulesCanOptOutOfShippedList(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
normalize:
  useDefaultDropParams: false
  dropQueryParams: [only_this]
`)

	rules, err := cfg.NormalizeRules()
	if err != nil {
		t.Fatal(err)
	}

	if len(rules.DropQueryParams) != 1 || rules.DropQueryParams[0] != "only_this" {
		t.Errorf("DropQueryParams = %v", rules.DropQueryParams)
	}
}

func TestInvalidRegexpInNormalizeIsRejected(t *testing.T) {
	t.Parallel()

	parseErr(t, `
normalize:
  pathReplacements:
    - pattern: "([unclosed"
      with: "x"
`)
}

func TestInvalidSeverityIsRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
detection:
  severity:
    thirdPartyHostAdded: catastrophic
`)

	if !strings.Contains(err.Error(), "catastrophic") {
		t.Errorf("error does not name the bad value: %v", err)
	}
}

func TestNotifierValidation(t *testing.T) {
	t.Parallel()

	parseErr(t, `
notify:
  - name: hook
    url: "not-a-url"
`)

	parseErr(t, `
notify:
  - name: hook
    url: https://example.com/hook
    minSeverity: enormous
`)

	// A secret reference is not a URL yet and must not be rejected as one.
	parse(t, `
notify:
  - name: hook
    url: "${env:WSAW_HOOK_URL}"
`)
}

func TestConcurrencyDefaultsAreBounded(t *testing.T) {
	t.Parallel()

	cfg := parse(t, "targets: []")

	if n := cfg.Concurrency(); n < 1 || n > 8 {
		t.Errorf("Concurrency() = %d, want between 1 and 8", n)
	}

	if cfg.PoolSize() != cfg.Concurrency() {
		t.Errorf("PoolSize() = %d, want it to follow concurrency", cfg.PoolSize())
	}

	explicit := parse(t, "scheduler:\n  concurrency: 32\n")
	if explicit.Concurrency() != 32 {
		t.Errorf("explicit concurrency was not honoured: %d", explicit.Concurrency())
	}
}

func TestEmptyConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := parse(t, "")

	if len(cfg.Targets) != 0 {
		t.Error("empty config produced targets")
	}
}

func TestRobotsPolicyValidation(t *testing.T) {
	t.Parallel()

	parse(t, `
targets:
  - name: t
    url: https://example.com/
    robots: respect
`)

	parseErr(t, `
targets:
  - name: t
    url: https://example.com/
    robots: maybe
`)
}
