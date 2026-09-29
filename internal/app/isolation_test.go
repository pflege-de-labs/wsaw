package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
)

// fallbackApp is an App whose pool cannot launch a browser, so the startup
// probe always falls back — without needing Chrome installed.
func fallbackApp(t *testing.T, b config.Browser) (*App, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer

	pool := browser.NewPool(browser.PoolOptions{
		MaxScansPerBrowser: b.MaxScansPerBrowser,
		Launch: browser.Options{
			Info:          browser.Info{Path: filepath.Join(t.TempDir(), "no-such-chrome")},
			RemoteURL:     b.RemoteURL,
			ProfileDir:    t.TempDir(),
			LaunchTimeout: 10 * time.Second,
		},
	})

	t.Cleanup(func() { _ = pool.Close() })

	return &App{
		Config: &config.Config{Browser: b},
		Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Pool:   pool,
	}, &logs
}

// fallbackLine returns the one log line the probe wrote.
func fallbackLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("the probe wrote %d log lines, want 1:\n%s", len(lines), logs.String())
	}

	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("decoding %q: %v", lines[0], err)
	}

	return line
}

func TestFallbackOverridesASharingLimitAndSaysHowToFixIt(t *testing.T) {
	t.Parallel()

	a, logs := fallbackApp(t, config.Browser{MaxScansPerBrowser: 10})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a.checkBrowserContexts(ctx)

	if got := a.Config.Browser.MaxScansPerBrowser; got != 1 {
		t.Errorf("browser.maxScansPerBrowser = %d after the fallback, want it overridden to 1", got)
	}

	line := fallbackLine(t, logs)

	if line["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", line["level"])
	}

	if line["configured_max_scans_per_browser"] != float64(10) || line["max_scans_per_browser"] != float64(1) {
		t.Errorf("the line does not state the override: configured=%v effective=%v",
			line["configured_max_scans_per_browser"], line["max_scans_per_browser"])
	}

	if line["overridden"] != true {
		t.Errorf("overridden = %v, want true", line["overridden"])
	}

	instruction, _ := line["instruction"].(string)
	if !strings.Contains(instruction, "silenceContextFallbackWarning") {
		t.Errorf("instruction %q does not say how to silence the warning", instruction)
	}
}

func TestSilencedFallbackIsLoggedAtInfo(t *testing.T) {
	t.Parallel()

	a, logs := fallbackApp(t, config.Browser{MaxScansPerBrowser: 1, SilenceContextFallbackWarning: true})

	a.checkBrowserContexts(context.Background())

	line := fallbackLine(t, logs)

	if line["level"] != "INFO" {
		t.Errorf("level = %v with the warning silenced, want INFO", line["level"])
	}

	if line["overridden"] != false {
		t.Errorf("overridden = %v for a limit that was already 1, want false", line["overridden"])
	}
}

// A remote browser is shared by every attachment, so one scan per attachment
// isolates nothing and the warning stays a warning.
func TestRemoteFallbackCannotBeSilenced(t *testing.T) {
	t.Parallel()

	a, logs := fallbackApp(t, config.Browser{
		MaxScansPerBrowser:            10,
		RemoteURL:                     "ws://127.0.0.1:1/devtools/browser/none",
		SilenceContextFallbackWarning: true,
	})

	a.checkBrowserContexts(context.Background())

	line := fallbackLine(t, logs)

	if line["level"] != "WARN" {
		t.Errorf("level = %v for a remote browser, want WARN despite the silencing setting", line["level"])
	}

	instruction, _ := line["instruction"].(string)
	if !strings.Contains(instruction, "browser.remoteUrl") {
		t.Errorf("instruction %q does not point at browser.remoteUrl", instruction)
	}
}

// A reload reads the file again, which still says what the operator wrote. The
// override is applied to it before the comparison, or every reload after a
// fallback would be refused for a change nobody made.
func TestApplyOverridesKeepsAReloadComparable(t *testing.T) {
	t.Parallel()

	a, _ := fallbackApp(t, config.Browser{MaxScansPerBrowser: 10})

	a.checkBrowserContexts(context.Background())

	next := &config.Config{Browser: config.Browser{MaxScansPerBrowser: 10}}
	a.ApplyOverrides(next)

	if changed := config.NonReloadableChanges(a.Config, next); len(changed) != 0 {
		t.Errorf("an unchanged file after the fallback reports %v as changed", changed)
	}
}

func TestApplyOverridesDoesNothingWithoutAFallback(t *testing.T) {
	t.Parallel()

	a := &App{}
	next := &config.Config{Browser: config.Browser{MaxScansPerBrowser: 10}}

	a.ApplyOverrides(next)

	if got := next.Browser.MaxScansPerBrowser; got != 10 {
		t.Errorf("maxScansPerBrowser = %d with no fallback, want the file's 10", got)
	}
}
