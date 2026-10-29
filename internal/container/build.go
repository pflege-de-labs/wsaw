package container

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/deploy/browser"
)

// buildTag is the shape of a name BuildImage tags an image with: a
// repository, optionally behind a registry host and port, and an optional
// tag, in the lowercase form the runtimes accept. It is passed to the runtime
// as an argument, so it must not start with a dash or carry anything else.
var buildTag = regexp.MustCompile(
	`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$`)

// ValidBuildTag reports whether name is an image name BuildImage will tag a
// build with.
func ValidBuildTag(name string) bool {
	return buildTag.MatchString(name)
}

// BuildOptions says what BuildImage builds.
type BuildOptions struct {
	// Context is the build context, with the build files at its root.
	Context fs.FS

	// Base selects the build file.
	Base browser.Base

	// Output receives the runtime's own build output, so an operator can see
	// what is being downloaded.
	Output io.Writer

	// Tag names the image, in place of BuildTag(Base).
	Tag string

	// TempDir is where the temporary build context is made, os.TempDir when
	// empty. Rootless podman reads the context from inside its pause
	// process's mount namespace, so a service with systemd's PrivateTmp has
	// to name a directory outside its private /tmp.
	TempDir string
}

// buildWaitDelay bounds how long a cancelled build may keep its output open
// after the runtime has been told to stop.
const buildWaitDelay = 10 * time.Second

// BuildImage builds the browser image on this machine and returns its tag
// (Story 6.12). The base image and Chromium are fetched by the runtime now,
// because the operator asked, and never during a scan.
//
// It always builds without the layer cache: the build files do not pin
// Chromium, so a cached package layer would hand back yesterday's browser
// under today's name.
//
// The build context is written to a temporary directory that is removed
// however the build ends, cancellation included.
func (r *Runtime) BuildImage(ctx context.Context, opts BuildOptions) (tag string, err error) {
	if !opts.Base.Valid() {
		return "", fmt.Errorf("unknown browser image base %q", opts.Base)
	}

	tag = cmp.Or(opts.Tag, BuildTag(opts.Base))
	if !ValidBuildTag(tag) {
		return "", fmt.Errorf("%q is not an image name to build, such as %s", tag, DefaultImage)
	}

	dir, err := os.MkdirTemp(opts.TempDir, "wsaw-browser-build-")
	if err != nil {
		return "", fmt.Errorf("creating the build context: %w", err)
	}

	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("removing the build context %s: %w", dir, rmErr))
		}
	}()

	if err := writeContext(dir, opts.Context); err != nil {
		return "", err
	}

	// #nosec G204 -- r.Path comes from exec.LookPath over a fixed runtime
	// list, and the only operator values, base and tag, were validated above.
	cmd := exec.CommandContext(ctx, r.Path, buildArgs(dir, opts.Base.File(), tag)...)
	cmd.Stdout = opts.Output
	cmd.Stderr = opts.Output
	cmd.WaitDelay = buildWaitDelay

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("building %s: %w", tag, ctx.Err())
		}

		return "", fmt.Errorf("%s build of %s: %w", r.Kind, tag, err)
	}

	return tag, nil
}

// buildArgs is the runtime's command line. Podman and Docker accept the same
// one; Docker needs BuildKit for the build files' COPY --chmod, which is its
// default builder.
func buildArgs(dir, file, tag string) []string {
	return []string{
		"build",
		"--no-cache",
		"--tag", tag,
		"--file", filepath.Join(dir, file),
		dir,
	}
}

// writeContext copies the build context into dir.
func writeContext(dir string, src fs.FS) error {
	err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		target := filepath.Join(dir, filepath.FromSlash(path))

		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}

		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}

		// The build files set the modes files get inside the image, so
		// these only need to be readable by the runtime's client.
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		return fmt.Errorf("writing the build context: %w", err)
	}

	return nil
}

// FetchHint is the command that makes a missing image present: for an image
// `wsaw browser build` names, the build that makes it, since no registry
// serves it; for any other, a pull.
func (r *Runtime) FetchHint(image string) string {
	if !strings.HasPrefix(image, ImageName+":") {
		return fmt.Sprintf("fetch it with: %s pull %s", r.Kind, image)
	}

	if _, refreshed := ParseRefreshTag(image); refreshed {
		// No command makes this exact image again: the refresh made it, and a
		// later one has a name of its own.
		return "it was made by the browser image refresh and is gone; leave browser.container.image unset " +
			"to let the refresh choose, or build one with: wsaw browser build --runtime " + string(r.Kind)
	}

	cmd := "wsaw browser build --runtime " + string(r.Kind)

	switch image {
	case DefaultImage:
	case BuildTag(browser.BaseDebian):
		cmd += " --base debian"
	default:
		cmd += " --tag " + image
	}

	return "build it with: " + cmd
}
