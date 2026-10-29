package main

import (
	"cmp"
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

	case cmdNamePrune:
		return cmdBrowserPrune(ctx, rest, os.Stdout, os.Stderr)

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
                     [--tag NAME] [--timeout D]
                     Build the per-scan browser image on this machine
  wsaw browser prune [--runtime auto|podman|docker] [--keep N | --keep-referenced]
                     [--dry-run] [--timeout D] [-config FILE]
                     Remove browser images the refresh made and no longer needs

wsaw does not distribute Chromium. "build" makes the browser image locally,
from the build files embedded in this binary, downloading the base image and
its Chromium package now, while you watch, rather than with a wsaw release.
Each build installs the newest Chromium the distribution holds that day.

  --base alpine   builds %s, the image wsaw uses by default
  --base debian   builds %s, a newer Chromium from Debian
                  testing; name it in browser.container.image to use it
  --tag NAME      names the image NAME instead, e.g. to push it to a registry
                  from CI or to keep a browser that a later build will not move

A localhost/ name is one no registry serves, so nothing is ever pulled in its
place. A daemon keeps the browser it started with until it restarts or its
browser image refresh switches.

"prune" applies the rule browser.container.refreshKeep or refreshKeepReferenced
sets — or --keep / --keep-referenced for this run — to the images the browser
image refresh made. It never removes the image in service, the one the
configuration names, the newest one the refresh made, or any image the refresh
did not make, and the runtime refuses to remove one a container is running.
`, container.DefaultImage, container.BuildTag(browser.BaseDebian))
}

// cmdBrowserBuild builds the browser image (Story 6.12).
func cmdBrowserBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("browser build", flag.ContinueOnError)
	fs.SetOutput(stderr)

	runtimeName := fs.String("runtime", string(container.KindAuto),
		"container runtime to build with: auto, podman or docker; use the one browser.runtime runs scans with")
	baseName := fs.String("base", string(browser.BaseAlpine),
		"distribution to build from: alpine (deploy/browser/Dockerfile, the default image) "+
			"or debian (deploy/browser/Containerfile, a newer Chromium from Debian testing)")
	tagName := fs.String("tag", "",
		"name to give the image (default: "+container.DefaultImage+", or "+
			container.BuildTag(browser.BaseDebian)+" with --base debian)")
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

	tag := cmp.Or(*tagName, container.BuildTag(base))
	if !container.ValidBuildTag(tag) {
		return fmt.Errorf("browser build: --tag %q is not an image name, such as %s", tag, container.DefaultImage)
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

	if _, err := fmt.Fprintf(stderr, "building %s with %s\n", tag, rt); err != nil {
		return err
	}

	tag, err = rt.BuildImage(ctx, container.BuildOptions{
		Context: browser.Context,
		Base:    base,
		Output:  stderr,
		Tag:     tag,
	})
	if err != nil {
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
