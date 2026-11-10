package config

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
)

func TestDiffOptionsAreTheTargetsOwn(t *testing.T) {
	t.Parallel()

	c := New()
	c.Detection.Severity.ThirdPartyHostRejectMode = diff.SeverityLow
	c.Detection.DegradedFailureRatio = 0.2
	c.Targets = []Target{{Name: "site", URL: "https://site.test/"}}
	c.Targets[0].Severity.ThirdPartyHostRejectMode = diff.SeverityMedium

	for name, want := range map[string]diff.Severity{
		"site":       diff.SeverityMedium, // its own rule
		"typed.test": diff.SeverityLow,    // an ad-hoc scan inherits the default
	} {
		o := c.DiffOptions(name)

		if got := o.Severity.ThirdPartyHostRejectMode; got != want {
			t.Errorf("%s: thirdPartyHostRejectMode = %q, want %q", name, got, want)
		}

		if o.DegradedFailureRatio != 0.2 {
			t.Errorf("%s: DegradedFailureRatio = %v, want 0.2", name, o.DegradedFailureRatio)
		}
	}
}

// detection.baseline is ignored, so setting it is reported; leaving it unset,
// as the defaults now do, is not.
func TestDetectionBaselineIsDeprecated(t *testing.T) {
	t.Parallel()

	c := New()
	if got := c.DeprecatedKeys(); len(got) != 0 {
		t.Errorf("defaults report deprecated settings: %v", got)
	}

	c.Detection.Baseline = BaselinePrevious
	if _, ok := c.DeprecatedKeys()["detection.baseline"]; !ok {
		t.Errorf("detection.baseline set but not reported: %v", c.DeprecatedKeys())
	}
}
