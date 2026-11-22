package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/textcheck"
)

// writePrompt puts a prompt file where a target can name it.
func writePrompt(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "impressum.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func checkedTarget(prompt, model string) string {
	return `
model:
` + model + `
targets:
  - name: imprint
    url: https://www.example.com/impressum/
    text:
      selector: main
      check:
        prompt: ` + prompt + `
        maxInputTokens: 20000
`
}

// Story 4.16, AC2: the prompt is read at load and carried as text; the model
// settings resolve with their defaults and the key from its reference.
func TestATextCheckResolvesItsPromptAndTheModel(t *testing.T) {
	t.Setenv("WSAW_TEST_MODEL_KEY", "sk-test")

	cfg := parse(t, checkedTarget(writePrompt(t, "Prüfe das Impressum."),
		"  model: claude-opus-5-5\n  apiKey: ${env:WSAW_TEST_MODEL_KEY}"))

	resolved, err := cfg.ResolveTargets(nil)
	if err != nil {
		t.Fatal(err)
	}

	if c := resolved[0].ModelCheck; c == nil || c.Prompt != "Prüfe das Impressum." || c.MaxInputTokens != 20000 {
		t.Errorf("check = %+v", c)
	}

	reg := &secret.Registry{}

	s, err := cfg.TextCheckSettings(reg)
	if err != nil {
		t.Fatal(err)
	}

	if s.Key != "sk-test" || s.Model != "claude-opus-5-5" || s.Timeout != textcheck.DefaultTimeout || s.Retries != textcheck.DefaultRetries {
		t.Errorf("settings = %+v", s)
	}

	if got := reg.Scrub("key sk-test"); strings.Contains(got, "sk-test") {
		t.Errorf("the key is not registered for redaction: %q", got)
	}

	if !cfg.UsesModel() {
		t.Error("UsesModel = false with a check configured")
	}
}

// Story 4.16, AC2 and AC10: what would send the key or the page text where
// the operator did not mean, or check nothing, fails at load (Tenet 15).
func TestATextCheckRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	good := writePrompt(t, "Prüfe.")
	empty := writePrompt(t, "  \n")

	tests := []struct {
		name, body, want string
	}{
		{"missing prompt file", checkedTarget("/nonexistent/prompt.txt", "  model: m"), "text.check.prompt"},
		{"empty prompt file", checkedTarget(empty, "  model: m"), "is empty"},
		{"no model", checkedTarget(good, "  timeout: 30s"), "model.model"},
		{"plain http elsewhere", checkedTarget(good, "  model: m\n  baseURL: http://models.example:11434"), "unencrypted"},
		{"not a URL", checkedTarget(good, "  model: m\n  baseURL: models"), "model.baseURL"},
		{"unknown auth", checkedTarget(good, "  model: m\n  auth: basic"), "model.auth"},
		{"a literal key", checkedTarget(good, "  model: m\n  apiKey: sk-ant-123"), "not the key itself"},
		{"negative cap", checkedTarget(good, "  model: m\n  dailyRequests: -1"), "must not be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := parseErr(t, tt.body); !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// AC10: a local server over plain http is fine on this host, and so is a
// model section with no target using it.
func TestALocalModelServerIsAllowed(t *testing.T) {
	t.Parallel()

	good := writePrompt(t, "Prüfe.")

	for _, base := range []string{"http://localhost:11434", "http://127.0.0.1:11434", "http://[::1]:11434", "https://ollama.com"} {
		cfg := parse(t, checkedTarget(good, "  model: llama\n  auth: bearer\n  retries: 0\n  timeout: 2m\n  baseURL: "+base))

		s, err := cfg.TextCheckSettings(nil)
		if err != nil || s.BaseURL != base || s.Auth != textcheck.AuthBearer || s.Timeout != 2*time.Minute || s.Retries != 0 {
			t.Errorf("%s: %+v, %v", base, s, err)
		}
	}

	if cfg := parse(t, "model:\n  baseURL: https://api.anthropic.com\ntargets: []\n"); cfg.UsesModel() {
		t.Error("UsesModel = true with no check")
	}
}
