package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// extraRulePack is a minimal user rule pack: enough for the loader to accept
// it and for the listing to show it alongside the built-in rules.
const extraRulePack = `version: 1

rules:
  - name: test-vendor
    vendor: TestVendor
    priority: 99
    detect: |
      !!window.__testVendor
    reject:
      - eval: |
          window.__testVendor.reject()
`

func TestOrDash(t *testing.T) {
	t.Parallel()

	if got := orDash(""); got != "—" {
		t.Errorf("orDash(%q) = %q, want a dash", "", got)
	}

	if got := orDash("klaro"); got != "klaro" {
		t.Errorf("orDash(%q) = %q, want it unchanged", "klaro", got)
	}
}

func TestCmdRulesRequiresASubcommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no subcommand", args: nil, want: "usage: wsaw rules list|test"},
		{name: "unknown subcommand", args: []string{"explain"}, want: `unknown subcommand "explain"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			capture(t, func() {
				err = cmdRules(t.Context(), tc.args)
			})

			if err == nil {
				t.Fatal("cmdRules accepted a missing or unknown subcommand")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCmdRulesListShowsTheBuiltinPack goes through cmdRules rather than
// calling cmdRulesList directly, so the subcommand dispatch is covered too.
func TestCmdRulesListShowsTheBuiltinPack(t *testing.T) {
	var err error

	stdout, _ := capture(t, func() {
		err = cmdRules(t.Context(), []string{"list"})
	})

	if err != nil {
		t.Fatalf("rules list: %v", err)
	}

	for _, want := range []string{"RULE", "VENDOR", "PRIORITY", "HEURISTIC", "MODES", "rule(s)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing does not contain %q; got:\n%s", want, stdout)
		}
	}

	// Rules are data, and the listing says so — that sentence is the reason
	// an operator edits a pack instead of filing a bug.
	if !strings.Contains(stdout, "Rules are data") {
		t.Errorf("listing does not explain that rules are data:\n%s", stdout)
	}
}

func TestCmdRulesListMergesAUserPack(t *testing.T) {
	pack := filepath.Join(t.TempDir(), "extra.yaml")
	if err := os.WriteFile(pack, []byte(extraRulePack), 0o600); err != nil {
		t.Fatal(err)
	}

	builtinOnly, _ := capture(t, func() {
		if err := cmdRulesList(nil); err != nil {
			t.Errorf("rules list: %v", err)
		}
	})

	merged, _ := capture(t, func() {
		if err := cmdRulesList([]string{"--rules", pack}); err != nil {
			t.Errorf("rules list with a pack: %v", err)
		}
	})

	if strings.Contains(builtinOnly, "test-vendor") {
		t.Fatal("the built-in listing already contains the test rule")
	}

	if !strings.Contains(merged, "test-vendor") || !strings.Contains(merged, "TestVendor") {
		t.Errorf("merged listing does not show the user rule:\n%s", merged)
	}
}

func TestCmdRulesListRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--nope"}},
		{name: "missing rule file", args: []string{"--rules", filepath.Join(t.TempDir(), "absent.yaml")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			capture(t, func() {
				err = cmdRulesList(tc.args)
			})

			if err == nil {
				t.Errorf("cmdRulesList(%v) succeeded, want an error", tc.args)
			}
		})
	}
}

// TestCmdRulesTestRefusesBadInvocations covers the validation that happens
// before a browser is needed: everything here fails without launching Chrome.
func TestCmdRulesTestRefusesBadInvocations(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no URL", args: nil, want: "usage: wsaw rules test"},
		{name: "two URLs", args: []string{"https://a.test/", "https://b.test/"}, want: "usage: wsaw rules test"},
		{name: "unknown flag", args: []string{"--nope"}, want: "flag provided but not defined"},
		{
			name: "invalid consent mode",
			args: []string{"--consent-mode", "maybe", "https://a.test/"},
			want: "--consent-mode must be reject or accept",
		},
		{
			// Testing a rule in "none" mode is meaningless: nothing is clicked,
			// so no rule can fire.
			name: "consent mode none",
			args: []string{"--consent-mode", "none", "https://a.test/"},
			want: "--consent-mode must be reject or accept",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			capture(t, func() {
				err = cmdRulesTest(t.Context(), tc.args)
			})

			if err == nil {
				t.Fatalf("cmdRulesTest(%v) succeeded, want an error", tc.args)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCmdDebugRefusesBadInvocations does the same for the debug command.
func TestCmdDebugRefusesBadInvocations(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no URL", args: nil, want: "usage: wsaw debug"},
		{name: "unknown flag", args: []string{"--nope"}, want: "flag provided but not defined"},
		{
			name: "invalid consent mode",
			args: []string{"--consent-mode", "maybe", "https://a.test/"},
			want: "is not valid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			capture(t, func() {
				err = cmdDebug(t.Context(), tc.args)
			})

			if err == nil {
				t.Fatalf("cmdDebug(%v) succeeded, want an error", tc.args)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
