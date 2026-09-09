package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

// exampleConfig reads the annotated reference config from the repository
// root, two levels up from this package.
func exampleConfig() ([]byte, error) {
	return os.ReadFile(filepath.Join("..", "..", "wsaw.example.yaml"))
}

const oneTarget = `
targets:
  - name: example
    url: https://example.com/
`

// TestBodyIdentityRoundTrips checks the rule survives parsing and reaches the
// normalizer, which is where it does its work.
func TestBodyIdentityRoundTrips(t *testing.T) {
	t.Parallel()

	cfg := parse(t, oneTarget+`
normalize:
  bodyIdentity:
    - urlPattern: 'googletagmanager\.com/gtm\.js'
      extract: '"version":"(\d+)"'
      label: GTM container version
`)

	if len(cfg.Normalize.BodyIdentities) != 1 {
		t.Fatalf("got %d body identity rules, want 1", len(cfg.Normalize.BodyIdentities))
	}

	rules, err := cfg.NormalizeRules()
	if err != nil {
		t.Fatalf("NormalizeRules: %v", err)
	}

	if len(rules.BodyIdentities) != 1 {
		t.Fatalf("the rule did not reach the normalizer: %+v", rules.BodyIdentities)
	}

	if got := rules.BodyIdentities[0].Label; got != "GTM container version" {
		t.Errorf("label = %q, want the configured label", got)
	}
}

// TestBodyIdentityWithoutExactlyOneGroupIsRejected catches at load time a rule
// that could never produce a value. At scan time the mistake is silent: the
// script simply keeps being compared by digest, which is the noise the rule
// was added to remove.
func TestBodyIdentityWithoutExactlyOneGroupIsRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, oneTarget+`
normalize:
  bodyIdentity:
    - urlPattern: 'gtm\.js'
      extract: '"version":"\d+"'
`)

	if !strings.Contains(err.Error(), "capturing group") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

func TestBodyIdentityRejectsBadPatternsAndEmptyFields(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"invalid urlPattern": `
normalize:
  bodyIdentity:
    - urlPattern: '(['
      extract: '(\d+)'
`,
		"invalid extract": `
normalize:
  bodyIdentity:
    - urlPattern: 'gtm\.js'
      extract: '(['
`,
		"empty urlPattern": `
normalize:
  bodyIdentity:
    - urlPattern: ''
      extract: '(\d+)'
`,
		"empty extract": `
normalize:
  bodyIdentity:
    - urlPattern: 'gtm\.js'
      extract: ''
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mustReject(t, oneTarget+body)
		})
	}
}

func TestDegradedFailureRatioRoundTrips(t *testing.T) {
	t.Parallel()

	cfg := parse(t, oneTarget+`
detection:
  degradedFailureRatio: 0.02
`)

	if got := cfg.Detection.DegradedFailureRatio; got != 0.02 {
		t.Errorf("degradedFailureRatio = %v, want 0.02", got)
	}
}

func TestNegativeDegradedFailureRatioIsRejected(t *testing.T) {
	t.Parallel()

	mustReject(t, oneTarget+`
detection:
  degradedFailureRatio: -0.1
`)
}

// TestContainerLimitsRoundTrip covers the knobs that stop the browser
// starving in ways a diff would report as the site changing.
func TestContainerLimitsRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := parse(t, oneTarget+`
browser:
  container:
    shmSize: 2g
    fileDescriptors: 16384
`)

	if got := cfg.Browser.Container.SHMSize; got != "2g" {
		t.Errorf("shmSize = %q, want 2g", got)
	}

	if got := cfg.Browser.Container.FileDescriptors; got != 16384 {
		t.Errorf("fileDescriptors = %d, want 16384", got)
	}
}

func TestNegativeFileDescriptorsIsRejected(t *testing.T) {
	t.Parallel()

	mustReject(t, oneTarget+`
browser:
  container:
    fileDescriptors: -1
`)
}

// TestShippedExampleConfigIsValid is the guard that matters for a documented
// reference: an example nobody can load is worse than no example.
func TestShippedExampleConfigIsValid(t *testing.T) {
	t.Parallel()

	body, err := exampleConfig()
	if err != nil {
		t.Skipf("wsaw.example.yaml is not readable from here: %v", err)
	}

	cfg, err := config.Parse(body)
	if err != nil {
		t.Fatalf("the shipped example does not validate: %v", err)
	}

	rules, err := cfg.NormalizeRules()
	if err != nil {
		t.Fatalf("the example's normalize rules could not be built: %v", err)
	}

	// Building the rules does not compile the patterns; the normalizer does,
	// and a pattern that does not compile would fail every scan.
	if _, err := normalize.New(rules); err != nil {
		t.Fatalf("the example's normalize patterns do not compile: %v", err)
	}
}
