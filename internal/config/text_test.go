package config_test

import (
	"strings"
	"testing"
)

// Story 4.15, AC2, AC8 and AC13: a target's text block resolves into what
// capture reads and what a comparison checks; checkAnchors is on and maxBytes
// has a default unless set.
func TestTextResolvesIntoCaptureAndComparison(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: privacy
    url: https://www.example.com/datenschutz/
    text:
      selector: main .content
      mustContain: ["Ihre Rechte", "Automatisierte Entscheidungsfindung"]
      minLength: 20000
  - name: imprint
    url: https://www.example.com/impressum/
    text:
      selector: "#imprint"
      checkAnchors: false
      maxBytes: 4096
  - name: plain
    url: https://www.example.com/
`)

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if r := resolved[0].Text; r == nil || r.Selector != "main .content" || r.MaxBytes != 1<<20 {
		t.Errorf("privacy region = %+v", r)
	}

	if r := resolved[1].Text; r == nil || r.MaxBytes != 4096 {
		t.Errorf("imprint region = %+v", r)
	}

	if resolved[2].Text != nil {
		t.Errorf("a target without text reads %+v", resolved[2].Text)
	}

	if c := cfg.DiffOptions("privacy").Text; c == nil || !c.CheckAnchors || c.MinLength != 20000 || len(c.MustContain) != 2 {
		t.Errorf("privacy checks = %+v", c)
	}

	if c := cfg.DiffOptions("imprint").Text; c == nil || c.CheckAnchors {
		t.Errorf("imprint checks = %+v", c)
	}

	if c := cfg.DiffOptions("plain").Text; c != nil {
		t.Errorf("plain checks = %+v", c)
	}
}

// Story 4.15, AC2: a text block wsaw could not use is a load-time error that
// names the target and the field (Tenet 15).
func TestTextRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ block, field string }{
		"no selector":         {"selector: \"\"", "targets[0].text.selector"},
		"unbalanced bracket":  {`selector: "a[href"`, "targets[0].text.selector"},
		"unbalanced paren":    {`selector: "li:not(.x"`, "targets[0].text.selector"},
		"unclosed quote":      {`selector: "a[title='x]"`, "targets[0].text.selector"},
		"leading combinator":  {`selector: "> main"`, "targets[0].text.selector"},
		"trailing comma":      {`selector: "main,"`, "targets[0].text.selector"},
		"empty mustContain":   {"selector: main\n      mustContain: [\"  \"]", "targets[0].text.mustContain"},
		"negative minLength":  {"selector: main\n      minLength: -1", "targets[0].text.minLength"},
		"negative maxBytes":   {"selector: main\n      maxBytes: -1", "targets[0].text.maxBytes"},
		"min above max":       {"selector: main\n      minLength: 5000\n      maxBytes: 100", "targets[0].text.minLength"},
		"unknown key refused": {"selector: main\n      mustcontain: [x]", "mustcontain"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := parseErr(t, `
targets:
  - name: privacy
    url: https://www.example.com/datenschutz/
    text:
      `+tc.block+`
`)
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("err = %v, want one naming %s", err, tc.field)
			}
		})
	}
}

func TestTextSelectorsThatPass(t *testing.T) {
	t.Parallel()

	for _, sel := range []string{
		"main", "#content", "article.legal > div:nth-child(2)", `a[href^="#link"]`,
		`[role="main"]`, `div[data-x='a]b']`, `.a\[b`, "main, #fallback",
	} {
		cfg := `
targets:
  - name: t
    url: https://www.example.com/
    text:
      selector: '` + strings.ReplaceAll(sel, "'", "''") + `'
`
		parse(t, cfg)
	}
}

// Story 4.15, AC13 and the reason given for formLogin: a region belongs to one
// page, so it is refused in defaults.
func TestTextIsNotAllowedInDefaults(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
defaults:
  text:
    selector: main
targets:
  - name: t
    url: https://www.example.com/
`)
	if !strings.Contains(err.Error(), "defaults.text") {
		t.Errorf("err = %v", err)
	}
}
