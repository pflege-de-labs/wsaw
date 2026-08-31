package consent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martint17r/wsaw/internal/consent"
	"github.com/martint17r/wsaw/internal/model"
)

// The rule pack ships in the binary, so a broken rule file would be a broken
// release. These tests are the guard for that.

func TestBuiltinRulesLoad(t *testing.T) {
	t.Parallel()

	set, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatalf("LoadBuiltinRules: %v", err)
	}

	if set.Len() == 0 {
		t.Fatal("builtin rule pack is empty")
	}

	seen := make(map[string]bool)

	for _, r := range set.Rules() {
		if seen[r.Name] {
			t.Errorf("duplicate rule name %q", r.Name)
		}

		seen[r.Name] = true

		if len(r.Accept) == 0 && len(r.Reject) == 0 {
			t.Errorf("rule %q has no steps", r.Name)
		}

		// A rule with no verification can never report "applied", so every
		// shipped rule must define one (Story 2.5).
		if r.Verify == "" {
			t.Errorf("rule %q has no verify expression and could never be verified", r.Name)
		}
	}
}

// TestBuiltinRulesCoverExpectedVendors pins the vendor list the stories
// promise, so removing one is a deliberate act rather than an accident.
func TestBuiltinRulesCoverExpectedVendors(t *testing.T) {
	t.Parallel()

	set, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	vendors := make(map[string]bool)
	for _, r := range set.Rules() {
		if r.Vendor != "" {
			vendors[strings.ToLower(r.Vendor)] = true
		}
	}

	for _, want := range []string{"usercentrics", "onetrust", "cookiebot", "didomi", "sourcepoint", "consentmanager", "complianz", "borlabs cookie"} {
		if !vendors[want] {
			t.Errorf("builtin rules do not cover %q", want)
		}
	}
}

// TestHeuristicRulesAreMarked matters because a heuristic result is weaker
// evidence and must be visibly flagged in every result it produces.
func TestHeuristicRulesAreMarked(t *testing.T) {
	t.Parallel()

	set, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	var heuristics, vendorRules int

	for _, r := range set.Rules() {
		if r.Heuristic {
			heuristics++

			if r.Vendor != "" {
				t.Errorf("rule %q is both heuristic and vendor-specific", r.Name)
			}
		} else {
			vendorRules++
		}
	}

	if heuristics == 0 {
		t.Error("no heuristic fallback rule is shipped")
	}

	if vendorRules == 0 {
		t.Error("no vendor rules are shipped")
	}
}

// TestRuleOrderPrefersVendorsOverHeuristics encodes Tenet 11: guessing is the
// last resort, never the first attempt.
func TestRuleOrderPrefersVendorsOverHeuristics(t *testing.T) {
	t.Parallel()

	set, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	rules := set.Rules()

	firstHeuristic := -1

	for i, r := range rules {
		if r.Heuristic && firstHeuristic == -1 {
			firstHeuristic = i
		}

		if !r.Heuristic && firstHeuristic != -1 {
			t.Errorf("vendor rule %q is ordered after heuristic rule %q", r.Name, rules[firstHeuristic].Name)
		}
	}
}

func writeRules(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadRuleFiles(t *testing.T) {
	t.Parallel()

	path := writeRules(t, `
version: 1
rules:
  - name: site-specific
    hosts: ["*.example.com"]
    priority: 500
    detect: "true"
    reject:
      - click: "#no"
    verify: "true"
`)

	set, err := consent.LoadRuleFiles(path)
	if err != nil {
		t.Fatalf("LoadRuleFiles: %v", err)
	}

	if set.Len() != 1 {
		t.Fatalf("loaded %d rules, want 1", set.Len())
	}

	if got := set.Rules()[0].Steps(model.ConsentReject); len(got) != 1 {
		t.Errorf("reject steps = %d, want 1", len(got))
	}

	if got := set.Rules()[0].Steps(model.ConsentAccept); len(got) != 0 {
		t.Errorf("accept steps = %d, want 0", len(got))
	}
}

// TestUnknownFieldIsRejected protects operators from silent typos: a
// misspelled key that is ignored would look like a working rule that never
// fires.
func TestUnknownFieldIsRejected(t *testing.T) {
	t.Parallel()

	path := writeRules(t, `
version: 1
rules:
  - name: typo
    rejectt:
      - click: "#no"
`)

	if _, err := consent.LoadRuleFiles(path); err == nil {
		t.Fatal("a misspelled field was accepted silently")
	}
}

func TestUnsupportedVersionIsRejected(t *testing.T) {
	t.Parallel()

	path := writeRules(t, `
version: 99
rules:
  - name: future
    reject:
      - click: "#no"
`)

	_, err := consent.LoadRuleFiles(path)
	if err == nil {
		t.Fatal("an unsupported rule file version was accepted")
	}

	if !strings.Contains(err.Error(), "version 1") {
		t.Errorf("error does not say what this build understands: %v", err)
	}
}

func TestRuleWithoutStepsIsRejected(t *testing.T) {
	t.Parallel()

	path := writeRules(t, `
version: 1
rules:
  - name: pointless
    detect: "true"
`)

	if _, err := consent.LoadRuleFiles(path); err == nil {
		t.Fatal("a rule with neither accept nor reject steps was accepted")
	}
}

func TestHostPatternMatching(t *testing.T) {
	t.Parallel()

	path := writeRules(t, `
version: 1
rules:
  - name: wildcard
    hosts: ["*.example.com", "other.test"]
    reject:
      - click: "#no"
  - name: anyhost
    reject:
      - click: "#no"
`)

	set, err := consent.LoadRuleFiles(path)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		host, domain string
		wantNames    []string
	}{
		{"www.example.com", "example.com", []string{"wildcard", "anyhost"}},
		{"other.test", "other.test", []string{"wildcard", "anyhost"}},
		{"unrelated.test", "unrelated.test", []string{"anyhost"}},
		// A lookalike must not match the wildcard.
		{"evil-example.com", "evil-example.com", []string{"anyhost"}},
	}

	for _, tc := range tests {
		got := set.Candidates(tc.host, tc.domain)

		var names []string
		for _, r := range got {
			names = append(names, r.Name)
		}

		if len(names) != len(tc.wantNames) {
			t.Errorf("Candidates(%q) = %v, want %v", tc.host, names, tc.wantNames)

			continue
		}

		for i := range names {
			if names[i] != tc.wantNames[i] {
				t.Errorf("Candidates(%q) = %v, want %v", tc.host, names, tc.wantNames)

				break
			}
		}
	}
}

func TestPriorityOrdersRules(t *testing.T) {
	t.Parallel()

	path := writeRules(t, `
version: 1
rules:
  - name: low
    priority: 1
    reject: [{click: "#a"}]
  - name: high
    priority: 100
    reject: [{click: "#b"}]
  - name: middle
    priority: 50
    reject: [{click: "#c"}]
`)

	set, err := consent.LoadRuleFiles(path)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"high", "middle", "low"}
	for i, r := range set.Rules() {
		if r.Name != want[i] {
			t.Errorf("rule[%d] = %q, want %q", i, r.Name, want[i])
		}
	}
}

// TestMergeLetsUserRulesOverride is the operator's escape hatch: a shipped
// rule that breaks must be fixable without waiting for a release.
func TestMergeLetsUserRulesOverride(t *testing.T) {
	t.Parallel()

	builtin, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	path := writeRules(t, `
version: 1
rules:
  - name: my-onetrust-fix
    vendor: OneTrust
    priority: 1000
    detect: "true"
    reject: [{click: "#my-button"}]
    verify: "true"
`)

	user, err := consent.LoadRuleFiles(path)
	if err != nil {
		t.Fatal(err)
	}

	merged := consent.Merge(builtin, user)

	if merged.Len() != builtin.Len()+1 {
		t.Errorf("merged length = %d, want %d", merged.Len(), builtin.Len()+1)
	}

	if got := merged.Rules()[0].Name; got != "my-onetrust-fix" {
		t.Errorf("highest priority rule = %q, want the user override", got)
	}
}

func TestMissingRuleFile(t *testing.T) {
	t.Parallel()

	if _, err := consent.LoadRuleFiles(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing rule file was accepted")
	}
}
