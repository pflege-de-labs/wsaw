package container

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pflege-de-labs/wsaw/deploy/browser"
)

func TestValidBuildTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{"localhost/wsaw-browser:latest", true},
		{"localhost/wsaw-browser:debian", true},
		{"registry.example.com:5000/ops/wsaw-browser:2026-10-29", true},
		{"ghcr.io/org/wsaw-browser", true},
		{"wsaw-browser", true},
		{"", false},
		{"--network=host", false},
		{"-t", false},
		{"Localhost/Wsaw", false},
		{"localhost/wsaw-browser:latest --pull", false},
		{"localhost/wsaw-browser@sha256:abc", false},
		{"localhost/wsaw-browser:", false},
		{"localhost//wsaw", false},
		{`x"; rm -rf /; "`, false},
	}

	for _, tt := range tests {
		if got := ValidBuildTag(tt.name); got != tt.want {
			t.Errorf("ValidBuildTag(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestDefaultImage is Story 6.12, AC3: the default is the image the embedded
// Dockerfile builds, under a name no registry serves, and building Debian
// never takes that name by default.
func TestDefaultImage(t *testing.T) {
	t.Parallel()

	if DefaultImage != "localhost/wsaw-browser:latest" {
		t.Errorf("DefaultImage = %q", DefaultImage)
	}

	if got := BuildTag(browser.BaseAlpine); got != DefaultImage {
		t.Errorf("BuildTag(alpine) = %q, want %q", got, DefaultImage)
	}

	if got := BuildTag(browser.BaseDebian); got == DefaultImage || !ValidBuildTag(got) {
		t.Errorf("BuildTag(debian) = %q, want a valid name other than the default", got)
	}
}

// TestBuildArgs: every build skips the layer cache, since the build files do
// not pin Chromium and a cached layer would keep the old one.
func TestBuildArgs(t *testing.T) {
	t.Parallel()

	got := strings.Join(buildArgs("/ctx", "Containerfile", "localhost/wsaw-browser:debian"), " ")
	want := "build --no-cache --tag localhost/wsaw-browser:debian --file /ctx/Containerfile /ctx"

	if got != want {
		t.Errorf("buildArgs = %q, want %q", got, want)
	}
}

// TestFetchHint is AC5: a built image is fetched by building it, anything
// else by pulling it.
func TestFetchHint(t *testing.T) {
	t.Parallel()

	rt := &Runtime{Kind: KindDocker}

	tests := []struct {
		image, want string
	}{
		{DefaultImage, "build it with: wsaw browser build --runtime docker"},
		{BuildTag(browser.BaseDebian), "build it with: wsaw browser build --runtime docker --base debian"},
		{"localhost/wsaw-browser:pinned", "build it with: wsaw browser build --runtime docker --tag localhost/wsaw-browser:pinned"},
		{"localhost/wsaw-browser:155.0.8059.84-r0",
			"build it with: wsaw browser build --runtime docker --tag localhost/wsaw-browser:155.0.8059.84-r0"},
		{"ghcr.io/pflege-de-labs/wsaw-browser@sha256:abc", "fetch it with: docker pull ghcr.io/pflege-de-labs/wsaw-browser@sha256:abc"},
	}

	for _, tt := range tests {
		if got := rt.FetchHint(tt.image); got != tt.want {
			t.Errorf("FetchHint(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

// fakeRuntime writes a runtime executable that records its arguments and
// checks the build context it is handed, then exits with code.
func fakeRuntime(t *testing.T, code int) (rt *Runtime, argsFile string) {
	t.Helper()

	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + argsFile + "'\n" +
		"prev=; file=\n" +
		"for a; do [ \"$prev\" = --file ] && file=$a; prev=$a; done\n" +
		"grep -q 'chromium-headless-shell' \"$file\" || exit 9\n" +
		"test -f \"$prev/entrypoint.sh\" || exit 9\n" +
		"echo 'STEP 1/2: FROM alpine'\n" +
		"exit " + strconv.Itoa(code) + "\n"

	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return &Runtime{Kind: KindPodman, Path: path}, argsFile
}

// recordedContext returns the build context directory the fake was given,
// its last argument.
func recordedContext(t *testing.T, argsFile string) string {
	t.Helper()

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the runtime was not run: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	return lines[len(lines)-1]
}

// TestBuildImage is AC7 and AC10: the runtime is handed the embedded context
// and the name, its output reaches the operator, and the temporary context
// is gone afterwards whether the build succeeded or not.
func TestBuildImage(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		base    browser.Base
		tag     string
		want    string
		file    string
		code    int
		wantErr bool
	}{
		{"alpine succeeds", browser.BaseAlpine, "", DefaultImage, "Dockerfile", 0, false},
		{"debian succeeds", browser.BaseDebian, "", "localhost/wsaw-browser:debian", "Containerfile", 0, false},
		{"named", browser.BaseAlpine, "registry.example.com/ops/wsaw-browser:latest",
			"registry.example.com/ops/wsaw-browser:latest", "Dockerfile", 0, false},
		{"fails", browser.BaseAlpine, "", DefaultImage, "Dockerfile", 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt, argsFile := fakeRuntime(t, tt.code)

			var out bytes.Buffer

			tag, err := rt.BuildImage(context.Background(), BuildOptions{
				Context: browser.Context,
				Base:    tt.base,
				Output:  &out,
				Tag:     tt.tag,
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("BuildImage error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && tag != tt.want {
				t.Errorf("tag = %q, want %q", tag, tt.want)
			}

			if !strings.Contains(out.String(), "STEP 1/2") {
				t.Errorf("the runtime's output was not passed on: %q", out.String())
			}

			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("the runtime was not run: %v", err)
			}

			if !strings.Contains(string(args), "--tag\n"+tt.want+"\n") {
				t.Errorf("the build was not tagged %s: %s", tt.want, args)
			}

			if !strings.Contains(string(args), "/"+tt.file+"\n") {
				t.Errorf("the build was not given %s: %s", tt.file, args)
			}

			if _, err := os.Stat(recordedContext(t, argsFile)); !os.IsNotExist(err) {
				t.Errorf("the build context was left behind: %v", err)
			}
		})
	}
}

// TestBuildImageRefusesAnInvalidTag is AC4: a name that is not one never
// reaches the runtime's command line.
func TestBuildImageRefusesAnInvalidTag(t *testing.T) {
	t.Parallel()

	rt, argsFile := fakeRuntime(t, 0)

	_, err := rt.BuildImage(context.Background(), BuildOptions{
		Context: fstest.MapFS{"Dockerfile": {Data: []byte("FROM scratch\n")}},
		Base:    browser.BaseAlpine,
		Output:  &bytes.Buffer{},
		Tag:     "--network=host",
	})
	if err == nil {
		t.Fatal("BuildImage accepted the tag \"--network=host\"")
	}

	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Error("the runtime was run with an invalid tag")
	}
}

// TestBuildImagePutsTheContextInTempDir: a service with a private /tmp has to
// hand rootless podman a path its pause process can see, so the context goes
// where the caller says rather than into os.TempDir.
func TestBuildImagePutsTheContextInTempDir(t *testing.T) {
	t.Parallel()

	rt, argsFile := fakeRuntime(t, 0)
	parent := t.TempDir()

	_, err := rt.BuildImage(context.Background(), BuildOptions{
		Context: browser.Context,
		Base:    browser.BaseAlpine,
		Output:  &bytes.Buffer{},
		TempDir: parent,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := filepath.Dir(recordedContext(t, argsFile)); got != parent {
		t.Errorf("the build context was made in %s, want %s", got, parent)
	}
}
