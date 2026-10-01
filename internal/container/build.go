package container

import (
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

// packageVersions are the shapes of a Chromium package version per base,
// e.g. 152.0.7977.82-r0 on Alpine and 154.0.8037.57-1 on Debian. The value
// ends up inside the build file's RUN line and in the image's tag, so
// anything else is refused before it reaches the build.
var packageVersions = map[browser.Base]*regexp.Regexp{
	browser.BaseAlpine: regexp.MustCompile(`^[0-9]+(\.[0-9]+)*-r[0-9]+$`),
	browser.BaseDebian: regexp.MustCompile(`^[0-9]+(\.[0-9]+)*-[0-9]+$`),
}

// ValidChromiumVersion reports whether v is a package version of base that
// BuildImage will accept.
func ValidChromiumVersion(base browser.Base, v string) bool {
	shape, ok := packageVersions[base]

	return ok && shape.MatchString(v)
}

// BuildOptions says what BuildImage builds.
type BuildOptions struct {
	// Context is the build context, with the build files at its root.
	Context fs.FS

	// Base selects the build file.
	Base browser.Base

	// ChromiumVersion is the chromium-headless-shell package to install. The
	// image is tagged ImageTag(ChromiumVersion).
	ChromiumVersion string

	// Output receives the runtime's own build output, so an operator can see
	// what is being downloaded.
	Output io.Writer
}

// buildWaitDelay bounds how long a cancelled build may keep its output open
// after the runtime has been told to stop.
const buildWaitDelay = 10 * time.Second

// BuildImage builds the browser image on this machine and returns its tag
// (Story 6.12). The base image and Chromium are fetched by the runtime now,
// because the operator asked, and never during a scan.
//
// The build context is written to a temporary directory that is removed
// however the build ends, cancellation included.
func (r *Runtime) BuildImage(ctx context.Context, opts BuildOptions) (tag string, err error) {
	if !opts.Base.Valid() {
		return "", fmt.Errorf("unknown browser image base %q", opts.Base)
	}

	if !ValidChromiumVersion(opts.Base, opts.ChromiumVersion) {
		return "", fmt.Errorf("chromium version %q is not a package version of the %s build, such as %s",
			opts.ChromiumVersion, opts.Base, browser.ChromiumVersion(opts.Base))
	}

	dir, err := os.MkdirTemp("", "wsaw-browser-build-")
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

	tag = ImageTag(opts.ChromiumVersion)

	// #nosec G204 -- r.Path comes from exec.LookPath over a fixed runtime
	// list, and the only operator values, base and version, were validated
	// above.
	cmd := exec.CommandContext(ctx, r.Path,
		buildArgs(dir, opts.Base.File(), tag, opts.ChromiumVersion)...)
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
func buildArgs(dir, file, tag, chromiumVersion string) []string {
	return []string{
		"build",
		"--tag", tag,
		"--build-arg", "CHROMIUM_VERSION=" + chromiumVersion,
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
	v, ok := strings.CutPrefix(image, ImageName+":")
	if !ok {
		return fmt.Sprintf("fetch it with: %s pull %s", r.Kind, image)
	}

	cmd := "wsaw browser build --runtime " + string(r.Kind)

	switch {
	case image == DefaultImage:
	case v == browser.ChromiumVersion(browser.BaseDebian):
		cmd += " --base debian"
	case ValidChromiumVersion(browser.BaseDebian, v):
		cmd += " --base debian --chromium-version " + v
	default:
		cmd += " --chromium-version " + v
	}

	return "build it with: " + cmd
}
