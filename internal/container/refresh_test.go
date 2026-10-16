package container

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/deploy/browser"
)

// The browser image refresh's runtime half (Story 6.13), against a fake
// runtime: no network, no real Podman or Docker (AC14).

func TestRefreshTagsRoundTrip(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 15, 12, 30, 5, 0, time.FixedZone("CEST", 2*3600))

	build := BuildRefreshTag(at)
	if build != "localhost/wsaw-browser:build-20261015T103005Z" {
		t.Errorf("BuildRefreshTag = %q", build)
	}

	pull := PullRefreshTag("ghcr.io/x/browser:stable", at)

	for _, tt := range []struct {
		name   string
		pulled bool
	}{{build, false}, {pull, true}} {
		tag, ok := ParseRefreshTag(tt.name)
		if !ok {
			t.Fatalf("ParseRefreshTag(%q) did not recognise its own tag", tt.name)
		}

		if !tag.At.Equal(at) || tag.Pulled() != tt.pulled {
			t.Errorf("ParseRefreshTag(%q) = %+v", tt.name, tag)
		}
	}

	if !RefreshTagFrom(pull, true, "ghcr.io/x/browser:stable") || RefreshTagFrom(pull, true, "ghcr.io/x/other:stable") {
		t.Error("a pulled tag is not tied to its source")
	}

	if !RefreshTagFrom(build, false, "") || RefreshTagFrom(build, true, "ghcr.io/x/browser:stable") {
		t.Error("a built tag is not told apart from a pulled one")
	}
}

// TestOnlyTheRefreshsOwnNamesAreRefreshTags is what keeps a prune away from
// every image an operator made (AC9).
func TestOnlyTheRefreshsOwnNamesAreRefreshTags(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		DefaultImage,
		"localhost/wsaw-browser:154.0.8037.57-1",
		"localhost/wsaw-browser:build-latest",
		"localhost/wsaw-browser:build-20261015T103005Z-x",
		"localhost/other:build-20261015T103005Z",
		"ghcr.io/x/browser:stable",
		"localhost/wsaw-browser:pull-xyz-20261015T103005Z",
	} {
		if _, ok := ParseRefreshTag(name); ok {
			t.Errorf("%q was taken for a refresh tag", name)
		}
	}
}

func TestParseInspect(t *testing.T) {
	t.Parallel()

	for out, want := range map[string]Image{
		"sha256:abc 123\n":          {Name: "i", ID: "sha256:abc", Size: 123},
		"abc 123\n":                 {Name: "i", ID: "sha256:abc", Size: 123},
		"WARN something\nabc 456\n": {Name: "i", ID: "sha256:abc", Size: 456},
		"sha256:abc 9000000000\n":   {Name: "i", ID: "sha256:abc", Size: 9000000000},
	} {
		got, err := parseInspect("i", out)
		if err != nil || got != want {
			t.Errorf("parseInspect(%q) = %+v, %v; want %+v", out, got, err, want)
		}
	}

	for _, out := range []string{"", "abc\n", "abc big\n"} {
		if _, err := parseInspect("i", out); err == nil {
			t.Errorf("parseInspect(%q) accepted it", out)
		}
	}
}

// TestFloorBuildTakesANewerChromium is AC2: a refresh build passes the
// constraint that lets apk choose a newer package, skips the layer cache, and
// is tagged with the name it was given.
func TestFloorBuildTakesANewerChromium(t *testing.T) {
	t.Parallel()

	rt, argsFile := fakeRuntime(t, 0)
	tag := BuildRefreshTag(time.Now())

	got, err := rt.BuildImage(context.Background(), BuildOptions{
		Context:         browser.Context,
		Base:            browser.BaseAlpine,
		ChromiumVersion: browser.ChromiumVersion(browser.BaseAlpine),
		Output:          &bytes.Buffer{},
		Floor:           true,
		Tag:             tag,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got != tag {
		t.Errorf("tag = %q, want %q", got, tag)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"--no-cache\n", "CHROMIUM_CONSTRAINT=>=\n", "--tag\n" + tag + "\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("the build was not given %q: %s", want, args)
		}
	}

	if _, err := rt.BuildImage(context.Background(), BuildOptions{
		Context: browser.Context, Base: browser.BaseDebian, ChromiumVersion: browser.ChromiumVersion(browser.BaseDebian),
		Output: &bytes.Buffer{}, Floor: true,
	}); err == nil {
		t.Error("a floor build of the Debian snapshot was accepted")
	}
}

// TestTheBuildFileTakesTheConstraint keeps the build file and the argument
// BuildImage passes in step: a file that lost it would rebuild the same
// Chromium for ever and report every refresh as unchanged.
func TestTheBuildFileTakesTheConstraint(t *testing.T) {
	t.Parallel()

	data, err := browser.Context.ReadFile(browser.BaseAlpine.File())
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`ARG CHROMIUM_CONSTRAINT="="`, `chromium-headless-shell${CHROMIUM_CONSTRAINT}${CHROMIUM_VERSION}`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("%s does not contain %s", browser.BaseAlpine.File(), want)
		}
	}
}

// scriptedRuntime writes a runtime that answers `images` with the given
// names, `image inspect` with an ID and size derived from the name, and
// records every other invocation; `image rm` fails for a name in refuse.
func scriptedRuntime(t *testing.T, names []string, refuse string) (*Runtime, string) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + log + "'\n" +
		"case \"$1 $2\" in\n" +
		"  'images --format') printf '%s\\n' " + shellWords(names) + " 'docker.io/library/alpine:3' ;;\n" +
		"  'image inspect') for last; do :; done; printf 'id-%s 100\\n' \"${last##*:}\" ;;\n" +
		"  'image rm') [ \"$3\" = '" + refuse + "' ] && { echo 'image is in use by a container' >&2; exit 2; } ;;\n" +
		"esac\n" +
		"exit 0\n"

	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return &Runtime{Kind: KindPodman, Path: path}, log
}

func shellWords(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}

	return strings.Join(quoted, " ")
}

func TestRefreshImagesListsOnlyTheRefreshsNewestFirst(t *testing.T) {
	t.Parallel()

	older := "localhost/wsaw-browser:build-20261001T000000Z"
	newer := "localhost/wsaw-browser:build-20261008T000000Z"

	rt, _ := scriptedRuntime(t, []string{older, DefaultImage, newer, "localhost/wsaw-browser:<none>"}, "")

	images, err := rt.RefreshImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(images) != 2 || images[0].Name != newer || images[1].Name != older {
		t.Fatalf("RefreshImages = %+v, want %s then %s", images, newer, older)
	}

	if images[0].ID != "sha256:id-build-20261008T000000Z" || images[0].Size != 100 {
		t.Errorf("RefreshImages did not inspect each image: %+v", images[0])
	}
}

// TestRemoveImageNeverForces is AC9 and AC12: the runtime's refusal to remove
// an image a container uses is passed on, not overridden.
func TestRemoveImageNeverForces(t *testing.T) {
	t.Parallel()

	inUse := "localhost/wsaw-browser:build-20261001T000000Z"
	rt, log := scriptedRuntime(t, nil, inUse)

	if err := rt.RemoveImage(context.Background(), inUse); err == nil {
		t.Error("removing an image in use reported success")
	}

	if err := rt.RemoveImage(context.Background(), "localhost/wsaw-browser:build-20261002T000000Z"); err != nil {
		t.Error(err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "--force") || strings.Contains(string(data), "-f ") {
		t.Errorf("an image removal was forced: %s", data)
	}
}

func TestFetchHintForARefreshImage(t *testing.T) {
	t.Parallel()

	rt := &Runtime{Kind: KindPodman}

	hint := rt.FetchHint(BuildRefreshTag(time.Now()))
	if !strings.Contains(hint, "browser image refresh") || strings.Contains(hint, "--chromium-version") {
		t.Errorf("FetchHint for a refresh image = %q", hint)
	}
}
