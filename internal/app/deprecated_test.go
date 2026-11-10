package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

// A deprecated setting still loads, so the operator who set it is told it
// does nothing rather than left believing it works.
func TestDeprecatedSettingIsWarnedAtStartup(t *testing.T) {
	t.Parallel()

	// Set as an operator sets it, in the file: the field itself is deprecated.
	cfg, err := config.Parse([]byte("detection:\n  baseline: approved\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	base := minimalConfig(t)
	cfg.Store, cfg.Targets = base.Store, base.Targets

	var logged bytes.Buffer

	a, err := New(t.Context(), cfg, Options{Version: "test", LogOutput: &logged})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	if out := logged.String(); !strings.Contains(out, "WARN") || !strings.Contains(out, "detection.baseline") {
		t.Errorf("no warning naming detection.baseline in:\n%s", out)
	}
}
