package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
)

func TestBrowserRuntimeName(t *testing.T) {
	t.Parallel()

	local := &App{}
	if got := local.BrowserRuntimeName(); got != "local" {
		t.Errorf("BrowserRuntimeName with no runtime = %q, want %q", got, "local")
	}

	containerised := &App{Runtime: &container.Runtime{Kind: container.KindPodman}}
	if got := containerised.BrowserRuntimeName(); got != "podman" {
		t.Errorf("BrowserRuntimeName with a runtime = %q, want %q", got, "podman")
	}
}

func TestBrowserSandboxed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		app     *App
		want    bool
		comment string
	}{
		{
			name: "local, sandbox on",
			app:  &App{Config: &config.Config{Browser: config.Browser{NoSandbox: false}}},
			want: true,
		},
		{
			name: "local, sandbox explicitly disabled",
			app:  &App{Config: &config.Config{Browser: config.Browser{NoSandbox: true}}},
			want: false,
		},
		{
			name: "containerised, sandbox never reported active",
			app: &App{
				Config:  &config.Config{Browser: config.Browser{NoSandbox: false}},
				Runtime: &container.Runtime{Kind: container.KindDocker},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.app.BrowserSandboxed(); got != tc.want {
				t.Errorf("BrowserSandboxed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveBrowserPrefersAnExplicitRemoteURL(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Browser: config.Browser{RemoteURL: "http://127.0.0.1:1/"}}
	a := &App{Config: cfg, Logger: discardLogger()}

	launch := &browser.Options{}

	if err := a.resolveBrowser(context.Background(), launch); err != nil {
		t.Fatalf("resolveBrowser: %v", err)
	}

	if a.Runtime != nil {
		t.Error("resolveBrowser probed for a container runtime despite an explicit remote URL")
	}

	if a.BrowserImage != "" {
		t.Errorf("BrowserImage = %q, want empty for a remote browser", a.BrowserImage)
	}

	// resolveBrowser itself never dials the remote endpoint — that only
	// happens once a scan runs — so this must succeed with nothing listening
	// on the given address.
}

// requireChrome skips the test unless a real Chrome is discoverable, exactly
// like internal/scanner's integration tests: browser-dependent behaviour
// cannot be faked, so it is skipped rather than mocked (Rule 1).
func requireChrome(t *testing.T) browser.Info {
	t.Helper()

	if os.Getenv("WSAW_SKIP_BROWSER_TESTS") != "" {
		t.Skip("skipping browser tests: WSAW_SKIP_BROWSER_TESTS is set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := browser.Discover(ctx, os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("skipping browser test: no usable Chrome found (%v)", err)
	}

	return info
}

func TestResolveBrowserWithLocalRuntimeDiscoversChrome(t *testing.T) {
	requireChrome(t)

	cfg := &config.Config{Browser: config.Browser{Runtime: "local"}}
	a := &App{Config: cfg, Logger: discardLogger()}

	launch := &browser.Options{}

	if err := a.resolveBrowser(context.Background(), launch); err != nil {
		t.Fatalf("resolveBrowser: %v", err)
	}

	if a.Runtime != nil {
		t.Error("resolveBrowser used a container despite Runtime: local")
	}

	if a.Chrome.Path == "" {
		t.Error("resolveBrowser did not record the discovered Chrome path")
	}

	if launch.Info.Path != a.Chrome.Path {
		t.Errorf("launch.Info = %+v, want it to carry the discovered browser", launch.Info)
	}
}

func TestResolveBrowserWarnsWhenTheSandboxIsDisabled(t *testing.T) {
	requireChrome(t)

	cfg := &config.Config{Browser: config.Browser{Runtime: "local", NoSandbox: true}}

	var logged strings.Builder

	a := &App{Config: cfg, Logger: slog.New(slog.NewTextHandler(&logged, nil))}

	if err := a.resolveBrowser(context.Background(), &browser.Options{}); err != nil {
		t.Fatalf("resolveBrowser: %v", err)
	}

	if !strings.Contains(logged.String(), "weakens isolation") {
		t.Errorf("disabling the sandbox was not warned about: %q", logged.String())
	}
}

func TestResolveBrowserFailsWhenChromeIsNotWhereConfigured(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Browser: config.Browser{
		Runtime: "local",
		Path:    filepath.Join(t.TempDir(), "no-such-chrome-binary"),
	}}
	a := &App{Config: cfg, Logger: discardLogger()}

	if err := a.resolveBrowser(context.Background(), &browser.Options{}); err == nil {
		t.Fatal("resolveBrowser accepted a Chrome path that does not exist")
	}
}

// TestNewWithARealLocalBrowserBuildsAPoolAndScanner is the one Phase 2 test
// that needs a real Chrome: it is the only way to reach buildScanner's
// success path (scanner.New with a non-nil pool), which every other test in
// this package deliberately avoids. It launches nothing — NewPool defers
// that to the first scan — so it stays fast.
func TestNewWithARealLocalBrowserBuildsAPoolAndScanner(t *testing.T) {
	requireChrome(t)

	cfg := minimalConfig(t)
	cfg.Browser.Runtime = "local"

	a, err := New(context.Background(), cfg, Options{Version: "test", RequireBrowser: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	if a.Pool == nil {
		t.Error("New with RequireBrowser did not build a browser pool")
	}

	if a.Scanner == nil {
		t.Error("New with a browser pool did not build a scanner")
	}

	if !a.BrowserSandboxed() {
		t.Error("a local, non-container browser should report the sandbox as active by default")
	}

	if got := a.BrowserRuntimeName(); got != "local" {
		t.Errorf("BrowserRuntimeName() = %q, want %q", got, "local")
	}

	if _, ok := a.TargetByName("site"); !ok {
		t.Error("the configured target was not resolved")
	}

	if len(a.RunningScans()) != 0 {
		t.Errorf("RunningScans() = %v, want none with nothing in flight", a.RunningScans())
	}
}
