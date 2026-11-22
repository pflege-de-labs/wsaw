package config

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/diff"
)

// DefaultTextMaxBytes caps a watched text. The longest legal page tried when
// the story was written, a privacy statement, was about 125 KB of text; a
// megabyte leaves room for eight of those and still keeps a runaway region
// out of every result document.
const DefaultTextMaxBytes = 1 << 20

// maxSelectorLen bounds a selector; anything longer is a mistake, not a
// region.
const maxSelectorLen = 1024

// Text watches the rendered text of one region of the page (Story 4.15). It
// is set on a target only: a region belongs to one page.
type Text struct {
	// Selector names the region; the first element it matches is read.
	Selector string `yaml:"selector"`
	// MustContain are strings that must each occur in the normalized text.
	MustContain []string `yaml:"mustContain,omitempty"`
	// MinLength is the fewest characters the text may have.
	MinLength int `yaml:"minLength,omitempty"`
	// CheckAnchors reports in-page links whose target is missing. On by
	// default; off for a page whose fragment links are driven by script.
	CheckAnchors *bool `yaml:"checkAnchors,omitempty"`
	// MaxBytes is where wsaw cuts the text, and says it did.
	MaxBytes int `yaml:"maxBytes,omitempty"`
	// Check has a model read the text against a prompt (Story 4.16).
	Check *TextCheck `yaml:"check,omitempty"`
}

// TextCheck is a target's prompt check (Story 4.16, AC2).
type TextCheck struct {
	// Prompt is the path of the prompt file, read at load.
	Prompt string `yaml:"prompt"`
	// MaxInputTokens refuses a larger request before it is sent.
	MaxInputTokens int `yaml:"maxInputTokens,omitempty"`
}

// ModelCheck is a target's check as a scan runs it: the prompt's text, not
// its path, so a prompt file changed on disk takes effect on reload and not
// halfway through a day (Tenet 10).
type ModelCheck struct {
	Prompt         string
	MaxInputTokens int
}

// readPrompt reads a check's prompt file; missing or empty is an error.
func readPrompt(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator configuration names the file, never a page
	if err != nil {
		return "", fmt.Errorf("reading the prompt: %w", err)
	}

	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("the prompt file %s is empty", path)
	}

	return string(b), nil
}

// textRegion is what capture needs to read the text.
func (t *Text) textRegion() *capture.TextRegion {
	if t == nil {
		return nil
	}

	return &capture.TextRegion{Selector: t.Selector, MaxBytes: firstInt(t.MaxBytes, 0, DefaultTextMaxBytes)}
}

// textChecks is what a comparison needs to judge the text complete.
func (t *Text) textChecks() *diff.TextChecks {
	if t == nil {
		return nil
	}

	return &diff.TextChecks{
		MustContain:  t.MustContain,
		MinLength:    t.MinLength,
		CheckAnchors: firstBool(t.CheckAnchors, nil, true),
	}
}

// validateText refuses a text block wsaw could not read or check.
func validateText(t *Target, field string, add addFunc) {
	if t.Text == nil {
		return
	}

	field += ".text"

	if msg := selectorProblem(t.Text.Selector); msg != "" {
		add(t.line, field+".selector", "%s", msg)
	}

	for i, s := range t.Text.MustContain {
		if strings.TrimSpace(s) == "" {
			add(t.line, field+".mustContain", "entry %d is empty; an empty string is in every text", i)
		}
	}

	if t.Text.MinLength < 0 {
		add(t.line, field+".minLength", "must not be negative")
	}

	if t.Text.MaxBytes < 0 {
		add(t.line, field+".maxBytes", "must not be negative")
	}

	if t.Text.MaxBytes > 0 && t.Text.MinLength > t.Text.MaxBytes {
		add(t.line, field+".minLength", "is larger than maxBytes, so every text would be reported incomplete")
	}

	if chk := t.Text.Check; chk != nil {
		if strings.TrimSpace(chk.Prompt) == "" {
			add(t.line, field+".check.prompt", "names no file; write the prompt to a file and give its path")
		} else if _, err := readPrompt(chk.Prompt); err != nil {
			add(t.line, field+".check.prompt", "%v", err)
		}

		if chk.MaxInputTokens < 0 {
			add(t.line, field+".check.maxInputTokens", "must not be negative")
		}
	}
}

// selectorProblem catches a selector that cannot be valid CSS, at load time
// (Tenet 15).
//
// shortcut: this checks the shape (balanced brackets and quotes, no control
// characters, no leading combinator), not the full CSS grammar, because the
// standard library has no CSS parser. A selector that passes here and is
// still invalid is recorded on every scan as "invalid selector" with the
// browser's reason. Upgrade to a real parser if that ever shows up in use.
func selectorProblem(sel string) string {
	s := strings.TrimSpace(sel)

	switch {
	case s == "":
		return "is empty; name the element that holds the text, for example main or #content"
	case len(s) > maxSelectorLen:
		return "is longer than 1024 characters"
	case strings.ContainsAny(s[:1], ">+~,"):
		return "starts with a combinator or a comma"
	case strings.ContainsAny(s[len(s)-1:], ">+~,"):
		return "ends with a combinator or a comma"
	}

	var stack []rune

	var quote rune

	escaped := false

	for _, r := range s {
		switch {
		case unicode.IsControl(r):
			return "contains a control character"
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '[' || r == '(':
			stack = append(stack, r)
		case r == ']' || r == ')':
			want := '['
			if r == ')' {
				want = '('
			}

			if len(stack) == 0 || stack[len(stack)-1] != want {
				return "has an unbalanced " + string(r)
			}

			stack = stack[:len(stack)-1]
		}
	}

	switch {
	case quote != 0:
		return "has an unclosed quote"
	case len(stack) > 0:
		return "has an unclosed " + string(stack[len(stack)-1])
	}

	return ""
}
