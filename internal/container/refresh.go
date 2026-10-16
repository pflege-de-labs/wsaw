package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file holds what the browser image refresh asks of a runtime (Story
// 6.13): a pull, a tag, an inspection, a listing of the images it made, and a
// removal that the runtime is allowed to refuse.
//
// Every image a refresh makes is tagged under ImageName with a name that says
// so — build-<time> for a build, pull-<source>-<time> for a pull — and that
// name is the only thing that marks it as the refresh's. A prune removes only
// such names, so an image an operator built, pulled or named by hand is never
// touched, whatever it holds.

// refreshStamp is the UTC time format in a refresh tag. It sorts as it reads.
const refreshStamp = "20060102T150405Z"

// refreshTagPattern matches a tag the refresh made: build-<stamp>, or
// pull-<8 hex digits of the source's hash>-<stamp>.
var refreshTagPattern = regexp.MustCompile(
	`^` + regexp.QuoteMeta(ImageName) + `:(build|pull-[0-9a-f]{8})-([0-9]{8}T[0-9]{6}Z)$`)

// RefreshTag is a tag the refresh made, read back.
type RefreshTag struct {
	// Kind is "build", or "pull-" and the source's hash.
	Kind string
	// At is when the refresh made it.
	At time.Time
}

// Pulled reports whether the tag names a pulled image.
func (t RefreshTag) Pulled() bool { return strings.HasPrefix(t.Kind, "pull-") }

// BuildRefreshTag names an image a refresh built at the given time.
func BuildRefreshTag(at time.Time) string {
	return ImageName + ":build-" + at.UTC().Format(refreshStamp)
}

// PullRefreshTag names the image a refresh pulled from source at the given
// time. The source is reduced to a hash because a registry reference holds
// characters a tag may not, and the hash still ties the image to the
// configured source, so a restart picks up only an image pulled from it.
func PullRefreshTag(source string, at time.Time) string {
	return ImageName + ":" + pullKind(source) + "-" + at.UTC().Format(refreshStamp)
}

func pullKind(source string) string {
	sum := sha256.Sum256([]byte(source))

	return "pull-" + hex.EncodeToString(sum[:4])
}

// ParseRefreshTag reads a refresh tag, and reports false for any other name.
func ParseRefreshTag(name string) (RefreshTag, bool) {
	m := refreshTagPattern.FindStringSubmatch(name)
	if m == nil {
		return RefreshTag{}, false
	}

	at, err := time.Parse(refreshStamp, m[2])
	if err != nil {
		return RefreshTag{}, false
	}

	return RefreshTag{Kind: m[1], At: at}, true
}

// RefreshTagFrom reports whether name is a refresh tag made in the given mode
// — a build, or a pull of source.
func RefreshTagFrom(name string, pull bool, source string) bool {
	tag, ok := ParseRefreshTag(name)
	if !ok {
		return false
	}

	if pull {
		return tag.Kind == pullKind(source)
	}

	return tag.Kind == "build"
}

// Image is one image in the runtime's local store.
type Image struct {
	// Name is the reference it was found under.
	Name string
	// ID identifies its content: the digest of its configuration, the same
	// for every name it carries and unique to what it holds.
	ID string
	// Size is what the runtime reports the image occupies, in bytes. Layers
	// it shares with other images are counted in each of them.
	Size int64
}

// InspectImage reads an image's ID and size.
func (r *Runtime) InspectImage(ctx context.Context, image string) (Image, error) {
	const inspectTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	out, err := run(ctx, r.Path, "image", "inspect", "--format", "{{.Id}} {{.Size}}", image)
	if err != nil {
		if isImageMissing(err.Error() + out) {
			return Image{}, fmt.Errorf("%w: %s", ErrImageMissing, image)
		}

		return Image{}, fmt.Errorf("inspecting browser image %s: %w", image, err)
	}

	return parseInspect(image, out)
}

// parseInspect reads `image inspect` output: the last non-empty line, since a
// runtime may print warnings first, holding the ID and the size.
func parseInspect(image, out string) (Image, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Fields(lines[len(lines)-1])

	if len(fields) != 2 {
		return Image{}, fmt.Errorf("unexpected inspection of %s: %q", image, out)
	}

	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return Image{}, fmt.Errorf("unexpected size of %s: %q", image, fields[1])
	}

	// Docker prefixes the algorithm and Podman does not; one spelling, so an
	// ID a result recorded under one matches the other.
	id := fields[0]
	if !strings.HasPrefix(id, "sha256:") {
		id = "sha256:" + id
	}

	return Image{Name: image, ID: id, Size: size}, nil
}

// RefreshImages lists the images under a refresh tag, newest first.
func (r *Runtime) RefreshImages(ctx context.Context) ([]Image, error) {
	const listTimeout = 30 * time.Second

	listCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	// Every image is listed and the refresh's picked out here, because the
	// two runtimes do not agree on what a reference filter matches.
	out, err := run(listCtx, r.Path, "images", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, fmt.Errorf("listing browser images: %w", err)
	}

	var names []string

	for line := range strings.Lines(out) {
		if name := strings.TrimSpace(line); refreshTagPattern.MatchString(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	// Newest first: the stamp sorts as it reads.
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(b, a) })

	images := make([]Image, 0, len(names))

	for _, name := range names {
		img, err := r.InspectImage(ctx, name)
		if err != nil {
			return nil, err
		}

		images = append(images, img)
	}

	return images, nil
}

// Pull fetches image from its registry, with whatever credentials the runtime
// already holds: wsaw passes none and reads none (AGENTS.md §3.3).
func (r *Runtime) Pull(ctx context.Context, image string, output io.Writer) error {
	// #nosec G204 -- r.Path comes from exec.LookPath over a fixed runtime
	// list, and the image is the operator's configured one, validated as a
	// tag at load.
	cmd := exec.CommandContext(ctx, r.Path, "pull", image)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = buildWaitDelay

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("pulling %s: %w", image, ctx.Err())
		}

		return fmt.Errorf("%s pull %s: %w", r.Kind, image, err)
	}

	return nil
}

// Tag gives an image a second name.
func (r *Runtime) Tag(ctx context.Context, image, name string) error {
	if _, err := run(ctx, r.Path, "tag", image, name); err != nil {
		return fmt.Errorf("tagging %s as %s: %w", image, name, err)
	}

	return nil
}

// RemoveImage removes a name, and the image with it once no other name holds
// it. It never forces: a runtime refuses to remove an image a container is
// running, and that refusal is what keeps a prune from pulling the browser
// out from under a scan.
func (r *Runtime) RemoveImage(ctx context.Context, name string) error {
	const removeTimeout = 60 * time.Second

	ctx, cancel := context.WithTimeout(ctx, removeTimeout)
	defer cancel()

	if _, err := run(ctx, r.Path, "image", "rm", name); err != nil {
		return fmt.Errorf("removing browser image %s: %w", name, err)
	}

	return nil
}
