package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pflege-de-labs/wsaw/deploy/browser"
	"github.com/pflege-de-labs/wsaw/internal/container"
)

// defaultBuildTimeout bounds a browser image build. A first build downloads
// a base image and Chromium with its dependencies, a few hundred megabytes,
// so it is generous; it exists so that a stalled mirror ends the command
// rather than leaving it hanging.
const defaultBuildTimeout = 30 * time.Minute

// cmdBrowser groups the commands about the browser wsaw runs.
func cmdBrowser(ctx context.Context, args []string) error {
	if len(args) == 0 {
		browserUsage(os.Stderr)

		return fmt.Errorf("browser: a subcommand is required")
	}

	switch sub, rest := args[0], args[1:]; sub {
	case "build":
		return cmdBrowserBuild(ctx, rest, os.Stdout, os.Stderr)

	case cmdNameHelp, "-h", argHelp:
		browserUsage(os.Stdout)

		return nil

	default:
		browserUsage(os.Stderr)

		return fmt.Errorf("browser: unknown subcommand %q", sub)
	}
}

func browserUsage(w *os.File) {
	// Best effort: there is nothing useful to do if the terminal has gone.
	_, _ = fmt.Fprintf(w, `wsaw browser — the browser wsaw renders pages with

Usage:
  wsaw browser build [--runtime auto|podman|docker] [--base alpine|debian]
                     [--chromium-version V] [--timeout D]
                     Build the per-scan browser image on this machine

wsaw does not distribute Chromium. "build" makes the browser image locally,
from the build files embedded in this binary, downloading the base image and
its Chromium package now, while you watch, rather than with a wsaw release.
The image is tagged %s:<chromium version>, a name no registry
serves, so nothing is ever pulled in its place.

  --base alpine   builds %s, the image wsaw uses by default
  --base debian   builds %s, a newer Chromium from a dated
                  Debian snapshot; name it in browser.container.image to use it
`, container.ImageName, container.DefaultImage,
		container.ImageTag(browser.ChromiumVersion(browser.BaseDebian)))
}

// cmdBrowserBuild builds the browser image (Story 6.12).
func cmdBrowserBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("browser build", flag.ContinueOnError)
	fs.SetOutput(stderr)

	runtimeName := fs.String("runtime", string(container.KindAuto),
		"container runtime to build with: auto, podman or docker; use the one browser.runtime runs scans with")
	baseName := fs.String("base", string(browser.BaseAlpine),
		"distribution to build from: alpine (deploy/browser/Dockerfile, the default image) "+
			"or debian (deploy/browser/Containerfile, a newer Chromium from a dated snapshot)")
	chromiumVersion := fs.String("chromium-version", "",
		"chromium-headless-shell package version to install (default: the one the build file pins)")
	timeout := fs.Duration("timeout", defaultBuildTimeout, "give up on the build after this long")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() > 0 {
		return fmt.Errorf("browser build: unexpected argument %q", fs.Arg(0))
	}

	kind := container.Kind(*runtimeName)
	if !kind.Valid() || kind == container.KindLocal {
		return fmt.Errorf("browser build: --runtime must be auto, podman or docker, not %q", *runtimeName)
	}

	base := browser.Base(*baseName)
	if !base.Valid() {
		return fmt.Errorf("browser build: --base must be alpine or debian, not %q", *baseName)
	}

	if *chromiumVersion == "" {
		*chromiumVersion = browser.ChromiumVersion(base)
	}

	if !container.ValidChromiumVersion(base, *chromiumVersion) {
		return fmt.Errorf("browser build: --chromium-version %q is not a package version of the %s build, such as %s",
			*chromiumVersion, base, browser.ChromiumVersion(base))
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	rt, err := container.Detect(ctx, kind)
	if err != nil {
		return fmt.Errorf("browser build: %w", err)
	}

	if rt == nil {
		return fmt.Errorf("browser build: no working podman or docker found; %w", container.ErrNoRuntime)
	}

	if _, err := fmt.Fprintf(stderr, "building %s with %s\n",
		container.ImageTag(*chromiumVersion), rt); err != nil {
		return err
	}

	tag, err := rt.BuildImage(ctx, container.BuildOptions{
		Context:         browser.Context,
		Base:            base,
		ChromiumVersion: *chromiumVersion,
		Output:          stderr,
	})
	if err != nil {
		if base == browser.BaseAlpine && ctx.Err() == nil {
			return fmt.Errorf("browser build: %w; Alpine keeps only the newest Chromium in a branch, "+
				"so if apk cannot select the package, pass the current version with --chromium-version "+
				"or build from a Debian snapshot with --base debian", err)
		}

		return fmt.Errorf("browser build: %w", err)
	}

	return verifyBuiltImage(ctx, rt, tag, stdout)
}

// verifyBuiltImage checks the image the way startup will, so a build that
// produced an image wsaw would refuse is reported as a failure here rather
// than at the daemon's next start (Story 6.12, AC6).
func verifyBuiltImage(ctx context.Context, rt *container.Runtime, tag string, stdout io.Writer) error {
	version, err := rt.BrowserVersion(ctx, tag)
	if err != nil {
		return fmt.Errorf("browser build: the built image does not run: %w", err)
	}

	sandboxed, err := rt.ImageKeepsSandbox(ctx, tag)
	if err != nil {
		return fmt.Errorf("browser build: %w", err)
	}

	if !sandboxed {
		return fmt.Errorf("browser build: %s does not declare %s=%s",
			tag, container.LabelSandbox, container.SandboxEnabled)
	}

	if _, err := fmt.Fprintf(stdout, "built %s (%s, sandbox enabled)\n", tag, version); err != nil {
		return err
	}

	if tag == container.DefaultImage {
		_, err = fmt.Fprintln(stdout, "it is the default browser image; no configuration is needed")
	} else {
		_, err = fmt.Fprintf(stdout, "to use it, set in the configuration:\n  browser:\n    container:\n      image: %q\n", tag)
	}

	return err
}
