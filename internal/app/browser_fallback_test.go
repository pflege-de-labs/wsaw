package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
)

// The startup check that the browser image is present, and what happens when
// it is not, is exercised against fake runtimes: shell scripts named podman
// and docker on a PATH holding nothing else, answering the handful of
// commands resolveBrowser issues. A real runtime would need the half-gigabyte
// image pulled, which a test must not do.

const fakeImage = "localhost/wsaw-browser-test@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// fakeRuntime writes a runtime named kind into dir. With hasImage it runs the
// image and reports a browser; without, it answers `run` the way Podman does
// for an image its store lacks.
func fakeRuntime(t *testing.T, dir string, kind container.Kind, hasImage bool) {
	t.Helper()

	run := `echo "Error: ` + fakeImage + `: image not known" >&2; exit 125`
	if hasImage {
		run = `echo "Chromium 154.0.8037.57"`
	}

	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"info) echo linux ;;\n" +
		"version) echo 5.0.0 ;;\n" +
		"run) " + run + " ;;\n" +
		"image) echo enabled ;;\n" +
		"ps) ;;\n" +
		"*) exit 1 ;;\n" +
		"esac\n"

	writeScript(t, filepath.Join(dir, string(kind)), script)
}

// fakeChrome writes a local browser that reports a supported version.
func fakeChrome(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "chromium")
	writeScript(t, path, "#!/bin/sh\necho 'Chromium 154.0.8037.57'\n")

	return path
}

func writeScript(t *testing.T, path, script string) {
	t.Helper()

	// #nosec G306 -- a test fixture that must be executable.
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func imageCheckApp(runtime, chromePath string, logged *strings.Builder) *App {
	cfg := &config.Config{Browser: config.Browser{
		Runtime:   runtime,
		Path:      chromePath,
		Container: config.ContainerBrowser{Image: fakeImage},
	}}

	return &App{Config: cfg, Logger: slog.New(slog.NewTextHandler(logged, nil))}
}

func TestResolveBrowserFallsBackToLocalWhenNoRuntimeHasTheImage(t *testing.T) {
	// Not parallel: t.Setenv is forbidden in a parallel test.
	dir := t.TempDir()
	fakeRuntime(t, dir, container.KindPodman, false)
	t.Setenv("PATH", dir)

	chrome := fakeChrome(t)

	var logged strings.Builder

	a := imageCheckApp("auto", chrome, &logged)
	launch := &browser.Options{}

	if err := a.resolveBrowser(context.Background(), launch); err != nil {
		t.Fatalf("resolveBrowser: %v", err)
	}

	if a.Runtime != nil || launch.Container != nil {
		t.Errorf("a runtime without the image was used: %+v", a.Runtime)
	}

	if a.BrowserImage != "" {
		t.Errorf("BrowserImage = %q, want empty when the browser runs on the host", a.BrowserImage)
	}

	if launch.Info.Path != chrome {
		t.Errorf("launch.Info.Path = %q, want the local browser %q", launch.Info.Path, chrome)
	}

	out := logged.String()
	for _, want := range []string{
		"level=WARN",
		"podman pull " + fakeImage,
		"falling back to the browser on this host",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q:\n%s", want, out)
		}
	}
}

func TestResolveBrowserTriesTheNextRuntimeForTheImage(t *testing.T) {
	// Not parallel: t.Setenv is forbidden in a parallel test.
	dir := t.TempDir()
	fakeRuntime(t, dir, container.KindPodman, false)
	fakeRuntime(t, dir, container.KindDocker, true)
	t.Setenv("PATH", dir)

	var logged strings.Builder

	a := imageCheckApp("auto", "", &logged)
	launch := &browser.Options{}

	if err := a.resolveBrowser(context.Background(), launch); err != nil {
		t.Fatalf("resolveBrowser: %v", err)
	}

	if a.Runtime == nil || a.Runtime.Kind != container.KindDocker {
		t.Fatalf("Runtime = %v, want docker, which holds the image", a.Runtime)
	}

	if launch.Container == nil {
		t.Error("no container launcher set for the runtime holding the image")
	}

	if a.Chrome.Version != "Chromium 154.0.8037.57" {
		t.Errorf("Chrome.Version = %q, want the image's browser", a.Chrome.Version)
	}

	if out := logged.String(); !strings.Contains(out, "podman pull "+fakeImage) {
		t.Errorf("passing over podman was not logged with the pull command:\n%s", out)
	}
}

// TestResolveBrowserExplicitRuntimeWithoutTheImageFails keeps AC6 for an
// operator who named the runtime: they get that runtime or an error.
func TestResolveBrowserExplicitRuntimeWithoutTheImageFails(t *testing.T) {
	// Not parallel: t.Setenv is forbidden in a parallel test.
	dir := t.TempDir()
	fakeRuntime(t, dir, container.KindPodman, false)
	t.Setenv("PATH", dir)

	var logged strings.Builder

	a := imageCheckApp("podman", fakeChrome(t), &logged)

	err := a.resolveBrowser(context.Background(), &browser.Options{})
	if !errors.Is(err, container.ErrImageMissing) {
		t.Fatalf("err = %v, want ErrImageMissing", err)
	}

	if !strings.Contains(err.Error(), "podman pull "+fakeImage) {
		t.Errorf("error does not carry the pull command: %v", err)
	}

	if a.Chrome.Path != "" {
		t.Errorf("fell back to the local browser %q despite runtime: podman", a.Chrome.Path)
	}
}

func TestResolveBrowserWithNoImageAndNoLocalBrowserNamesBoth(t *testing.T) {
	// Not parallel: t.Setenv is forbidden in a parallel test.
	dir := t.TempDir()
	fakeRuntime(t, dir, container.KindPodman, false)
	t.Setenv("PATH", dir)

	var logged strings.Builder

	a := imageCheckApp("auto", filepath.Join(t.TempDir(), "no-such-chrome"), &logged)

	err := a.resolveBrowser(context.Background(), &browser.Options{})
	if !errors.Is(err, container.ErrImageMissing) {
		t.Fatalf("err = %v, want it to wrap ErrImageMissing", err)
	}

	for _, want := range []string{"podman pull " + fakeImage, "no-such-chrome"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q: %v", want, err)
		}
	}
}
