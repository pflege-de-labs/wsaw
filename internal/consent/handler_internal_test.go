package consent

// These are the parts of the consent engine that decide what an operator is
// told — how a CMP identifier is rendered, how a reason string is assembled,
// what an empty rule set does when asked a question. They are unexported, and
// the browser-driven tests in internal/scanner reach them only along the paths
// a fixture happens to take, so they are exercised directly here.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

func TestNumStringRendersJSONNumbersWithoutGoNoise(t *testing.T) {
	t.Parallel()

	// A CMP ID arrives from the page as a JSON number, which lands in an
	// `any` as float64. Formatting that with %s produces
	// "%!s(float64=28)" in the operator-visible CMP name, which is what this
	// helper exists to prevent.
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"absent", nil, ""},
		{"already a string", "28", "28"},
		{"JSON number", float64(28), "28"},
		{"large JSON number", float64(1234567), "1234567"},
		{"fractional is truncated", float64(28.9), "28"},
		{"native int", 42, "42"},
		{"unexpected type falls back", true, "true"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got := numString(c.in)
			if got != c.want {
				t.Errorf("numString(%#v) = %q, want %q", c.in, got, c.want)
			}

			if strings.Contains(got, "%!") {
				t.Errorf("numString(%#v) leaked a Go format error: %q", c.in, got)
			}
		})
	}
}

func TestAppendReason(t *testing.T) {
	t.Parallel()

	if got := appendReason("", "second"); got != "second" {
		t.Errorf("appending to an empty reason = %q, want %q", got, "second")
	}

	if got := appendReason("first", "second"); got != "first; second" {
		t.Errorf("appendReason = %q, want %q", got, "first; second")
	}
}

func TestIsFalsyResult(t *testing.T) {
	t.Parallel()

	// Only an explicit false is a failure: a vendor script that returns
	// nothing has not said it could not act.
	cases := map[string]bool{
		"false":  true,
		" false": true,
		"true":   false,
		"null":   false,
		"":       false,
		"0":      false,
	}

	for raw, want := range cases {
		if got := isFalsyResult([]byte(raw)); got != want {
			t.Errorf("isFalsyResult(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestJSStringEscapesForThePage(t *testing.T) {
	t.Parallel()

	// Selectors come from rule packs and are interpolated into an expression
	// evaluated in the page, so quoting is not cosmetic.
	if got := jsString(`a[href="x"]`); got != `"a[href=\"x\"]"` {
		t.Errorf("jsString = %s, want the quotes escaped", got)
	}
}

func TestWithDefaultsFillsTheBudgetsAndTheLogger(t *testing.T) {
	t.Parallel()

	got := (&Options{}).withDefaults()

	if got.StepTimeout != DefaultStepTimeout {
		t.Errorf("step timeout = %v, want %v", got.StepTimeout, DefaultStepTimeout)
	}

	if got.TotalTimeout != DefaultTotalTimeout {
		t.Errorf("total timeout = %v, want %v", got.TotalTimeout, DefaultTotalTimeout)
	}

	if got.OnFailure != FailFlag {
		t.Errorf("failure policy = %q, want %q", got.OnFailure, FailFlag)
	}

	if got.Logger == nil {
		t.Error("logger is nil; every path in the handler logs through it")
	}

	// Configured values survive.
	configured := (&Options{
		StepTimeout:  time.Second,
		TotalTimeout: 2 * time.Second,
		OnFailure:    FailScan,
		Logger:       slog.Default(),
	}).withDefaults()

	if configured.StepTimeout != time.Second || configured.TotalTimeout != 2*time.Second {
		t.Errorf("withDefaults overwrote configured budgets: %+v", configured)
	}

	if configured.OnFailure != FailScan {
		t.Errorf("withDefaults overwrote the failure policy: %q", configured.OnFailure)
	}
}

func TestMechanismForAndRuleCMPName(t *testing.T) {
	t.Parallel()

	evalRule := Rule{Name: "vendor-x", Vendor: "Vendor X"}
	clickRule := Rule{Name: "clicky"}

	if got := mechanismFor(evalRule, []Action{{Eval: "window.cmp.reject()"}}); got != MechanismVendorAPI {
		t.Errorf("an eval step reports mechanism %q, want %q", got, MechanismVendorAPI)
	}

	if got := mechanismFor(clickRule, []Action{{Click: "#reject"}}); got != MechanismSelector {
		t.Errorf("a click step reports mechanism %q, want %q", got, MechanismSelector)
	}

	if got := mechanismFor(Rule{Heuristic: true}, []Action{{Click: "#reject"}}); got != MechanismHeuristic {
		t.Errorf("a heuristic rule reports mechanism %q, want %q", got, MechanismHeuristic)
	}

	if got := ruleCMPName(evalRule); got != "Vendor X" {
		t.Errorf("ruleCMPName = %q, want the vendor", got)
	}

	if got := ruleCMPName(clickRule); got != "clicky" {
		t.Errorf("ruleCMPName without a vendor = %q, want the rule name", got)
	}
}

func TestNilRuleSetAnswersInsteadOfPanicking(t *testing.T) {
	t.Parallel()

	// A daemon started with no rule packs at all still runs scans; the
	// handler asks the set for candidates on every page.
	var s *RuleSet

	if got := s.Len(); got != 0 {
		t.Errorf("Len on a nil set = %d, want 0", got)
	}

	if got := s.Rules(); got != nil {
		t.Errorf("Rules on a nil set = %v, want nil", got)
	}

	if got := s.Candidates("example.com", "example.com"); got != nil {
		t.Errorf("Candidates on a nil set = %v, want nil", got)
	}
}

func TestStepsForAModeWithNoSequence(t *testing.T) {
	t.Parallel()

	r := Rule{Accept: []Action{{Click: "#ok"}}, Reject: []Action{{Click: "#no"}}}

	if got := r.Steps(model.ConsentAccept); len(got) != 1 || got[0].Click != "#ok" {
		t.Errorf("accept steps = %+v", got)
	}

	if got := r.Steps(model.ConsentReject); len(got) != 1 || got[0].Click != "#no" {
		t.Errorf("reject steps = %+v", got)
	}

	// "none" is not a sequence to run; the handler never interacts in it.
	if got := r.Steps(model.ConsentNone); got != nil {
		t.Errorf("steps for mode none = %+v, want nil", got)
	}
}

func TestLoadRuleFilesRejectsANamelessRule(t *testing.T) {
	t.Parallel()

	path := writeRuleFile(t, `
version: 1
rules:
  - accept:
      - click: "#ok"
`)

	_, err := LoadRuleFiles(path)
	if err == nil {
		t.Fatal("a rule with no name was accepted")
	}

	if !strings.Contains(err.Error(), "has no name") {
		t.Errorf("error = %v, want it to say the rule has no name", err)
	}
}

func TestLoadRuleFilesRejectsABadHostPattern(t *testing.T) {
	t.Parallel()

	// Host patterns are the part of a rule pack non-programmers edit. They are
	// globs, quoted before compilation, so regex metacharacters in them are
	// literal rather than an error — an empty entry is the one a pack can
	// actually get wrong, and it must fail at load with the rule named rather
	// than silently matching every host at scan time.
	path := writeRuleFile(t, `
version: 1
rules:
  - name: broken
    hosts: [""]
    accept:
      - click: "#ok"
`)

	_, err := LoadRuleFiles(path)
	if err == nil {
		t.Fatal("an empty host pattern was accepted")
	}

	if !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "empty pattern") {
		t.Errorf("error = %v, want it to name the rule and say the pattern is empty", err)
	}
}

func TestCompileHostPatternRejectsAnEmptyPattern(t *testing.T) {
	t.Parallel()

	if _, err := compileHostPattern("   "); err == nil {
		t.Error("a whitespace-only host pattern compiled; it would match nothing and say nothing")
	}

	// A glob's metacharacters are literal, not a regex the operator has to
	// escape, and matching is case-insensitive on the pattern side.
	re, err := compileHostPattern("*.EXAMPLE.co.uk")
	if err != nil {
		t.Fatalf("compiling a wildcard pattern: %v", err)
	}

	if !re.MatchString("www.example.co.uk") || re.MatchString("example.com") {
		t.Errorf("wildcard pattern %v does not match as a glob", re)
	}
}

func TestMergeKeepsRulesWhenRecompilationFails(t *testing.T) {
	t.Parallel()

	// compile only fails on a pattern that was already validated when the set
	// was built, so this is the defensive branch. Losing every rule because
	// of it would silently disable consent handling for the whole daemon.
	good := &RuleSet{rules: []Rule{{Name: "good", Accept: []Action{{Click: "#ok"}}}}}
	broken := &RuleSet{rules: []Rule{{Name: "broken", Hosts: []string{""}}}}

	merged := Merge(good, nil, broken)

	if merged.Len() != 2 {
		t.Errorf("merged set holds %d rules, want both of them kept", merged.Len())
	}
}

func TestBuiltinRulesAreCompiledOnce(t *testing.T) {
	t.Parallel()

	set, err := LoadBuiltinRules()
	if err != nil {
		t.Fatalf("loading the embedded rule pack: %v", err)
	}

	if set.Len() == 0 {
		t.Fatal("the embedded rule pack is empty")
	}

	// Every shipped rule must be usable for the mode it claims to handle,
	// which is the property the pack's own tests cannot assert per rule.
	for _, r := range set.Rules() {
		if len(r.Accept) == 0 && len(r.Reject) == 0 && len(r.Necessary) == 0 {
			t.Errorf("shipped rule %q defines no steps at all", r.Name)
		}
	}
}

func writeRuleFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rules.yaml")

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the rule file: %v", err)
	}

	return path
}
