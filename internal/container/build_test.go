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

func TestValidChromiumVersion(t *testing.T) {
	t.Parallel()

	alpine, debian := browser.BaseAlpine, browser.BaseDebian

	tests := []struct {
		base browser.Base
		v    string
		want bool
	}{
		{alpine, "152.0.7977.82-r0", true},
		{alpine, "153.0.1-r12", true},
		{alpine, "154.0.8037.57-1", false},
		{debian, "154.0.8037.57-1", true},
		{debian, "152.0.7977.82-r0", false},
		{alpine, "152.0.7977.82", false},
		{debian, "152.0.7977.82", false},
		{alpine, "latest", false},
		{alpine, "", false},
		{alpine, `152.0-r0"; rm -rf /; "`, false},
		{debian, "154.0-1 --network=host", false},
		{alpine, "-r0", false},
		{"ubuntu", "154.0-1", false},
	}

	for _, tt := range tests {
		if got := ValidChromiumVersion(tt.base, tt.v); got != tt.want {
			t.Errorf("ValidChromiumVersion(%s, %q) = %v, want %v", tt.base, tt.v, got, tt.want)
		}
	}
}

// TestDefaultImageFollowsTheDockerfile is Story 6.12, AC3: the default is the
// image the embedded Dockerfile builds, under a name no registry serves.
func TestDefaultImageFollowsTheDockerfile(t *testing.T) {
	t.Parallel()

	want := "localhost/wsaw-browser:" + browser.ChromiumVersion(browser.BaseAlpine)
	if DefaultImage != want {
		t.Errorf("DefaultImage = %q, want %q", DefaultImage, want)
	}

	for _, base := range []browser.Base{browser.BaseAlpine, browser.BaseDebian} {
		if v := browser.ChromiumVersion(base); !ValidChromiumVersion(base, v) {
			t.Errorf("%s's CHROMIUM_VERSION %q would not be accepted by BuildImage", base.File(), v)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	t.Parallel()

	got := strings.Join(buildArgs("/ctx", "Containerfile", "localhost/wsaw-browser:1.2-3", "1.2-3"), " ")
	want := "build --tag localhost/wsaw-browser:1.2-3 --build-arg CHROMIUM_VERSION=1.2-3 --file /ctx/Containerfile /ctx"

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
		{"localhost/wsaw-browser:160.1-r0", "build it with: wsaw browser build --runtime docker --chromium-version 160.1-r0"},
		{ImageTag(browser.ChromiumVersion(browser.BaseDebian)), "build it with: wsaw browser build --runtime docker --base debian"},
		{"localhost/wsaw-browser:160.1-2", "build it with: wsaw browser build --runtime docker --base debian --chromium-version 160.1-2"},
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
		"grep -q 'ARG CHROMIUM_VERSION' \"$file\" || exit 9\n" +
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
// and the version, its output reaches the operator, and the temporary context
// is gone afterwards whether the build succeeded or not.
func TestBuildImage(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		base    browser.Base
		version string
		file    string
		code    int
		wantErr bool
	}{
		{"alpine succeeds", browser.BaseAlpine, "160.1-r2", "Dockerfile", 0, false},
		{"debian succeeds", browser.BaseDebian, "160.1-2", "Containerfile", 0, false},
		{"fails", browser.BaseAlpine, "160.1-r2", "Dockerfile", 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rt, argsFile := fakeRuntime(t, tt.code)

			var out bytes.Buffer

			tag, err := rt.BuildImage(context.Background(), BuildOptions{
				Context:         browser.Context,
				Base:            tt.base,
				ChromiumVersion: tt.version,
				Output:          &out,
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("BuildImage error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && tag != "localhost/wsaw-browser:"+tt.version {
				t.Errorf("tag = %q", tag)
			}

			if !strings.Contains(out.String(), "STEP 1/2") {
				t.Errorf("the runtime's output was not passed on: %q", out.String())
			}

			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("the runtime was not run: %v", err)
			}

			if !strings.Contains(string(args), "CHROMIUM_VERSION="+tt.version+"\n") {
				t.Errorf("the version was not passed as a build argument: %s", args)
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

// TestBuildImageRefusesAnInvalidVersion is AC4: an invalid version never
// reaches the runtime.
func TestBuildImageRefusesAnInvalidVersion(t *testing.T) {
	t.Parallel()

	rt, argsFile := fakeRuntime(t, 0)

	_, err := rt.BuildImage(context.Background(), BuildOptions{
		Context:         fstest.MapFS{"Dockerfile": {Data: []byte("FROM scratch\n")}},
		Base:            browser.BaseAlpine,
		ChromiumVersion: "latest",
		Output:          &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("BuildImage accepted the version \"latest\"")
	}

	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Error("the runtime was run with an invalid version")
	}
}
