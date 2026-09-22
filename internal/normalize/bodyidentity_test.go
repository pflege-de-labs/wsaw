package normalize_test

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

const gtmRule = `googletagmanager\.com/gtm\.js`

func gtmNormalizer(t *testing.T) *normalize.Normalizer {
	t.Helper()

	n, err := normalize.New(normalize.Rules{
		BodyIdentities: []normalize.BodyIdentity{{
			URLPattern: gtmRule,
			Extract:    `"version":"(\d+)"`,
			Label:      "GTM container version",
		}},
	})
	if err != nil {
		t.Fatalf("compiling rules: %v", err)
	}

	return n
}

// TestBodyIdentityIgnoresBytesThatMoveWithoutTheVersion is the whole point of
// the rule: a tag-manager container folds experiment flags into every
// response, so two fetches of the same published container differ by a
// handful of tokens. The version it declares does not move, and that is what
// a comparison should key on.
func TestBodyIdentityIgnoresBytesThatMoveWithoutTheVersion(t *testing.T) {
	t.Parallel()

	n := gtmNormalizer(t)

	const url = "https://www.googletagmanager.com/gtm.js?id=GTM-PKKTX6B"

	first := `{"resource":{"version":"231","macros":[],"exp":{"1":610}}}`
	second := `{"resource":{"version":"231","macros":[],"exp":{"1":615,"2":true}}}`
	published := `{"resource":{"version":"232","macros":[],"exp":{"1":615}}}`

	label, a := n.BodyIdentity(url, first)
	if label != "GTM container version" {
		t.Errorf("label = %q, want the configured label", label)
	}

	_, b := n.BodyIdentity(url, second)
	if a != b {
		t.Errorf("identity moved with the experiment flags: %q then %q", a, b)
	}

	_, c := n.BodyIdentity(url, published)
	if c == a {
		t.Errorf("a container publish did not change the identity: still %q", c)
	}

	if a != "231" || c != "232" {
		t.Errorf("extracted %q and %q, want 231 and 232", a, c)
	}
}

// TestBodyIdentityOnlyAppliesToMatchingURLs keeps the rule from quietly
// governing every script on the page.
func TestBodyIdentityOnlyAppliesToMatchingURLs(t *testing.T) {
	t.Parallel()

	n := gtmNormalizer(t)

	label, value := n.BodyIdentity("https://cdn.test/app.js", `"version":"231"`)
	if label != "" || value != "" {
		t.Errorf("rule applied to an unrelated URL: %q = %q", label, value)
	}
}

// TestBodyIdentityYieldsNothingWhenTheBodyDoesNotCarryIt matters because the
// caller has to be able to tell "no identity" from "identity changed": a
// missing value makes the script not comparable, not changed.
func TestBodyIdentityYieldsNothingWhenTheBodyDoesNotCarryIt(t *testing.T) {
	t.Parallel()

	n := gtmNormalizer(t)

	label, value := n.BodyIdentity(
		"https://www.googletagmanager.com/gtm.js?id=GTM-X", "// an empty container")
	if label != "" || value != "" {
		t.Errorf("got %q = %q from a body without the identity, want nothing", label, value)
	}
}

// TestBodyIdentityRequiresExactlyOneCapturingGroup rejects at load time a
// rule that could never produce a value. Left to scan time the mistake is
// invisible: the script simply keeps being compared by digest.
func TestBodyIdentityRequiresExactlyOneCapturingGroup(t *testing.T) {
	t.Parallel()

	for name, extract := range map[string]string{
		"no groups":  `"version":"\d+"`,
		"two groups": `"(version)":"(\d+)"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := normalize.New(normalize.Rules{
				BodyIdentities: []normalize.BodyIdentity{{URLPattern: gtmRule, Extract: extract}},
			})
			if err == nil {
				t.Fatalf("extract %q was accepted", extract)
			}

			if !strings.Contains(err.Error(), "capturing group") {
				t.Errorf("error does not explain the problem: %v", err)
			}
		})
	}
}

func TestBodyIdentityRejectsInvalidPatterns(t *testing.T) {
	t.Parallel()

	if _, err := normalize.New(normalize.Rules{
		BodyIdentities: []normalize.BodyIdentity{{URLPattern: "([", Extract: `(\d+)`}},
	}); err == nil {
		t.Error("an invalid urlPattern was accepted")
	}

	if _, err := normalize.New(normalize.Rules{
		BodyIdentities: []normalize.BodyIdentity{{URLPattern: gtmRule, Extract: "(["}},
	}); err == nil {
		t.Error("an invalid extract was accepted")
	}
}

func TestHasBodyIdentitiesReportsWhetherAnyRuleIsConfigured(t *testing.T) {
	t.Parallel()

	bare, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatalf("compiling empty rules: %v", err)
	}

	if bare.HasBodyIdentities() {
		t.Error("an unconfigured normalizer claims to have identity rules")
	}

	if !gtmNormalizer(t).HasBodyIdentities() {
		t.Error("a configured normalizer does not report its identity rules")
	}
}

// TestCustomDataPathCollapsesToOneKey is the consentmanager case: the
// settings descriptor is base64-encoded into the filename, and the same
// version already travels in the sv= parameter of a sibling call. Left alone,
// one settings publish reports as two added assets and two removed ones.
func TestCustomDataPathCollapsesToOneKey(t *testing.T) {
	t.Parallel()

	n, err := normalize.New(normalize.Rules{
		PathReplacements: []normalize.Replacement{{
			Pattern: `/delivery/customdata/[A-Za-z0-9+/=_-]{16,}\.js$`,
			With:    "/delivery/customdata/{cmpsettings}.js",
		}},
	})
	if err != nil {
		t.Fatalf("compiling rules: %v", err)
	}

	const host = "https://cdn.consentmanager.net"

	v94 := n.Key(host + "/delivery/customdata/bV8xLndfNDc3MC5yX0dEUFIubF9lbi54dF85NA.js")
	v97 := n.Key(host + "/delivery/customdata/bV8xLndfNDc3MC5yX0dEUFIubF9lbi54dF85Nw.js")

	if v94 != v97 {
		t.Errorf("two settings versions produced different keys:\n  %s\n  %s", v94, v97)
	}

	if !strings.Contains(v94, "{cmpsettings}") {
		t.Errorf("the collapse is not visible in the key: %s", v94)
	}

	// Siblings on the same host must be left alone.
	if got := n.Key(host + "/delivery/js/cmp_en.min.js"); strings.Contains(got, "{cmpsettings}") {
		t.Errorf("the rule collapsed an unrelated script: %s", got)
	}
}
