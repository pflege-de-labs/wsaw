// Package consent brings a page into a requested consent state.
//
// CMP markup changes constantly, so rules are data rather than code (Tenet
// 10): a new banner is handled by editing YAML, never by releasing a new
// binary. The engine prefers a documented API over clicking, and clicking
// over label guessing, and always records which mechanism it used (Tenet 11).
package consent

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

//go:embed rules/*.yaml
var builtinRules embed.FS

// Action is one step in a rule.
type Action struct {
	// Click clicks the first element matching this selector via a synthetic
	// el.click(): fast, and indistinguishable from a real click to most CMPs.
	Click string `yaml:"click,omitempty"`
	// TrustedClick clicks the first element matching this selector through a
	// genuine, CDP-dispatched pointer event rather than a synthetic one. Use
	// it only where Click is known not to work: at least one CMP (CCM19)
	// checks Event.isTrusted and silently ignores a synthetic click, so the
	// interaction appears to succeed while the choice is never recorded.
	TrustedClick string `yaml:"trustedClick,omitempty"`
	// WaitFor waits for a selector to appear before continuing.
	WaitFor string `yaml:"waitFor,omitempty"`
	// WaitMillis pauses, for CMPs that animate their dialog.
	WaitMillis int `yaml:"waitMillis,omitempty"`
	// Eval runs a JavaScript expression in the page. Reserved for vendor
	// APIs; it is not an escape hatch for arbitrary scripting.
	Eval string `yaml:"eval,omitempty"`
	// Optional marks a step that may legitimately find nothing, such as an
	// intermediate "manage settings" button that some variants skip.
	Optional bool `yaml:"optional,omitempty"`
}

// Rule handles one CMP, or one site's bespoke banner.
type Rule struct {
	// Name identifies the rule in results and logs.
	Name string `yaml:"name"`
	// Vendor is the CMP product this rule targets, if any.
	Vendor string `yaml:"vendor,omitempty"`

	// Hosts restricts the rule to matching hosts. Patterns are matched
	// against the registrable domain and the full host. Empty means any host.
	Hosts []string `yaml:"hosts,omitempty"`

	// Detect is a JavaScript expression that must evaluate truthy for the
	// rule to apply. This is how a CMP is recognized without relying on the
	// host, which matters because most CMPs are used by many sites.
	Detect string `yaml:"detect,omitempty"`

	// Accept and Reject are the step sequences for each consent mode.
	Accept []Action `yaml:"accept,omitempty"`
	Reject []Action `yaml:"reject,omitempty"`

	// Necessary is the fallback step sequence tried in `reject` mode when this
	// rule defines no Reject steps, or when every Reject step is optional and
	// none of them matched anything — the shape of a banner that offers no
	// reject/decline control at all (Story 2.8). It should reach the closest
	// state such a banner allows: every optional category deselected, saved.
	// It is never tried ahead of a working Reject sequence, and it is never
	// used for any mode but `reject`.
	Necessary []Action `yaml:"necessary,omitempty"`

	// Verify is a JavaScript expression that must evaluate truthy after the
	// steps ran. Without it, the interaction can only ever be "unverified".
	Verify string `yaml:"verify,omitempty"`

	// Dismissed is a JavaScript expression that must evaluate truthy once the
	// banner itself is no longer displayed. It answers a different question
	// than Verify: a vendor API can record a choice without the banner's own
	// dismiss handler ever running, so "the CMP recorded my choice" and "the
	// banner is gone" need separate evidence (Story 2.7, AC1). Empty means
	// the shared heuristic container check decides.
	Dismissed string `yaml:"dismissed,omitempty"`

	// Priority orders rules; higher wins. Site-specific rules should
	// outrank generic vendor rules.
	Priority int `yaml:"priority,omitempty"`

	// Heuristic marks a rule as label guessing rather than a known CMP, so
	// results can flag it as less trustworthy.
	Heuristic bool `yaml:"heuristic,omitempty"`

	hostPatterns []*regexp.Regexp
}

// RuleFile is the on-disk format of a rule pack.
type RuleFile struct {
	// Version allows the format to evolve without breaking existing packs.
	Version int    `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

// RuleSet is a compiled, ordered collection of rules.
type RuleSet struct {
	rules []Rule
}

// LoadBuiltinRules returns the rule pack embedded in the binary.
func LoadBuiltinRules() (*RuleSet, error) {
	entries, err := builtinRules.ReadDir("rules")
	if err != nil {
		return nil, fmt.Errorf("reading embedded rules: %w", err)
	}

	var all []Rule

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}

		b, err := builtinRules.ReadFile(filepath.Join("rules", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading embedded rule file %s: %w", e.Name(), err)
		}

		rules, err := parseRules(e.Name(), b)
		if err != nil {
			return nil, err
		}

		all = append(all, rules...)
	}

	return compile(all)
}

// LoadRuleFiles loads user rule packs, which are appended after the builtin
// pack so that an operator can override a shipped rule by giving it a higher
// priority.
func LoadRuleFiles(paths ...string) (*RuleSet, error) {
	var all []Rule

	for _, path := range paths {
		b, err := os.ReadFile(path) //nolint:gosec // operator-supplied rule pack path
		if err != nil {
			return nil, fmt.Errorf("reading rule file %s: %w", path, err)
		}

		rules, err := parseRules(path, b)
		if err != nil {
			return nil, err
		}

		all = append(all, rules...)
	}

	return compile(all)
}

func parseRules(source string, b []byte) ([]Rule, error) {
	var file RuleFile

	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)

	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parsing rule file %s: %w", source, err)
	}

	if file.Version != 1 {
		return nil, fmt.Errorf("rule file %s: unsupported version %d, this build understands version 1", source, file.Version)
	}

	for i := range file.Rules {
		if file.Rules[i].Name == "" {
			return nil, fmt.Errorf("rule file %s: rule %d has no name", source, i)
		}

		if len(file.Rules[i].Accept) == 0 && len(file.Rules[i].Reject) == 0 && len(file.Rules[i].Necessary) == 0 {
			return nil, fmt.Errorf("rule file %s: rule %q defines neither accept, reject, nor necessary steps",
				source, file.Rules[i].Name)
		}
	}

	return file.Rules, nil
}

// Merge combines rule sets, later sets taking precedence at equal priority.
func Merge(sets ...*RuleSet) *RuleSet {
	var all []Rule

	for _, s := range sets {
		if s != nil {
			all = append(all, s.rules...)
		}
	}

	merged, err := compile(all)
	if err != nil {
		// compile only fails on invalid patterns, which were already
		// validated when each set was built.
		return &RuleSet{rules: all}
	}

	return merged
}

func compile(rules []Rule) (*RuleSet, error) {
	for i := range rules {
		for _, pattern := range rules[i].Hosts {
			re, err := compileHostPattern(pattern)
			if err != nil {
				return nil, fmt.Errorf("rule %q: host pattern %q: %w", rules[i].Name, pattern, err)
			}

			rules[i].hostPatterns = append(rules[i].hostPatterns, re)
		}
	}

	// Stable ordering by descending priority: a site-specific rule must be
	// tried before a generic vendor rule.
	stableSortByPriority(rules)

	return &RuleSet{rules: rules}, nil
}

// compileHostPattern turns a glob-ish host pattern into a regular expression.
// Operators write "*.example.com", not regular expressions, because host
// patterns are the part of a rule pack that non-programmers edit.
func compileHostPattern(pattern string) (*regexp.Regexp, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}

	var b strings.Builder

	b.WriteString(`\A`)

	for _, part := range strings.Split(pattern, "*") {
		b.WriteString(regexp.QuoteMeta(part))
		b.WriteString(`§`)
	}

	expr := strings.TrimSuffix(b.String(), `§`)
	expr = strings.ReplaceAll(expr, `§`, `.*`)

	return regexp.Compile(expr + `\z`)
}

// Rules returns the compiled rules in evaluation order.
func (s *RuleSet) Rules() []Rule {
	if s == nil {
		return nil
	}

	return s.rules
}

// Len reports how many rules the set holds.
func (s *RuleSet) Len() int {
	if s == nil {
		return 0
	}

	return len(s.rules)
}

// Candidates returns the rules whose host patterns allow the given host, in
// evaluation order. Detection expressions are evaluated later, in the page.
func (s *RuleSet) Candidates(host, domain string) []Rule {
	if s == nil {
		return nil
	}

	host = strings.ToLower(host)
	domain = strings.ToLower(domain)

	out := make([]Rule, 0, len(s.rules))

	for _, r := range s.rules {
		if r.matchesHost(host, domain) {
			out = append(out, r)
		}
	}

	return out
}

func (r *Rule) matchesHost(host, domain string) bool {
	if len(r.hostPatterns) == 0 {
		return true
	}

	for _, re := range r.hostPatterns {
		if re.MatchString(host) || re.MatchString(domain) {
			return true
		}
	}

	return false
}

// Steps returns the action sequence for a consent mode.
func (r *Rule) Steps(mode model.ConsentMode) []Action {
	switch mode {
	case model.ConsentAccept:
		return r.Accept
	case model.ConsentReject:
		return r.Reject
	default:
		return nil
	}
}

func stableSortByPriority(rules []Rule) {
	// Insertion sort keeps equal priorities in load order, which is what
	// makes "later file wins" predictable for operators.
	for i := 1; i < len(rules); i++ {
		for j := i; j > 0 && rules[j].Priority > rules[j-1].Priority; j-- {
			rules[j], rules[j-1] = rules[j-1], rules[j]
		}
	}
}
