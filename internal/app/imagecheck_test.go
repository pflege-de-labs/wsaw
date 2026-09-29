package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
)

const pinnedImage = "ghcr.io/org/browser@sha256:0000000000000000000000000000000000000000000000000000000000000000"

var newerImage = container.ImageUpdate{
	Checked: "ghcr.io/org/browser:latest",
	Latest:  "sha256:1111111111111111111111111111111111111111111111111111111111111111",
}

func TestCheckBrowserImage(t *testing.T) {
	t.Parallel()

	off := false

	tests := []struct {
		name     string
		update   container.ImageUpdate
		err      error
		chromium string // the running browser's version, as the image reports it
		local    bool
		checkOff *bool
		want     string // expected in the log; empty expects no check at all
	}{
		{
			name: "newer image published", update: newerImage, chromium: "Chromium 152.0.7977.82",
			want: `level=WARN msg="a newer browser image is published`,
		},
		{
			name:     "newer image with a newer Chromium",
			update:   withChromium(newerImage, "153.0.8000.10-r0"),
			chromium: "Chromium 152.0.7977.82",
			want:     `latest_chromium=153.0.8000.10-r0`,
		},
		{
			// Every release rebuilds the image, so its digest moves even when
			// the browser does not. That is not a newer browser.
			name:     "a rebuild of the same Chromium",
			update:   withChromium(newerImage, "152.0.7977.82-r0"),
			chromium: "Chromium 152.0.7977.82",
			want:     `level=INFO msg="browser image runs the latest published Chromium"`,
		},
		{
			name:   "pinned image is latest",
			update: container.ImageUpdate{Checked: newerImage.Checked, Latest: newerImage.Latest, Current: true},
			want:   `level=INFO msg="browser image is the latest published"`,
		},
		{
			name: "registry fails", err: errors.New("registry answered 500"),
			want: `level=WARN msg="could not check for a newer browser image"`,
		},
		{
			name: "floating tag", err: container.ErrNotPinned,
			want: `level=DEBUG msg="browser image is not pinned by digest`,
		},
		{name: "check turned off", update: newerImage, checkOff: &off},
		{name: "local browser", update: newerImage, local: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				logged bytes.Buffer
				calls  int
			)

			a := &App{
				Config:       &config.Config{Browser: config.Browser{Container: config.ContainerBrowser{CheckForUpdates: tt.checkOff}}},
				Logger:       slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
				Runtime:      &container.Runtime{Kind: container.KindPodman},
				BrowserImage: pinnedImage,
			}
			a.Chrome.Version = tt.chromium

			if tt.local {
				a.Runtime, a.BrowserImage = nil, ""
			}

			a.checkBrowserImage(context.Background(), func(_ context.Context, image string) (container.ImageUpdate, error) {
				calls++

				if image != pinnedImage {
					t.Errorf("checked %s, want %s", image, pinnedImage)
				}

				return tt.update, tt.err
			})

			if tt.want == "" {
				if calls != 0 {
					t.Fatalf("checked the registry %d times, want none; log:\n%s", calls, logged.String())
				}

				return
			}

			if !strings.Contains(logged.String(), tt.want) {
				t.Fatalf("log does not contain %q:\n%s", tt.want, logged.String())
			}
		})
	}
}

// The warning carries the command that fetches the newer image, pinned, for
// the runtime in use.
func TestCheckBrowserImageNamesPull(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer

	a := &App{
		Config:       &config.Config{},
		Logger:       slog.New(slog.NewTextHandler(&logged, nil)),
		Runtime:      &container.Runtime{Kind: container.KindDocker},
		BrowserImage: pinnedImage,
	}

	a.checkBrowserImage(context.Background(), func(context.Context, string) (container.ImageUpdate, error) {
		return newerImage, nil
	})

	want := `pull="docker pull ghcr.io/org/browser:latest@` + newerImage.Latest + `"`
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("log does not contain %s:\n%s", want, logged.String())
	}
}

func TestSameChromium(t *testing.T) {
	t.Parallel()

	tests := []struct {
		running, label string
		want           bool
	}{
		{running: "Chromium 152.0.7977.82", label: "152.0.7977.82-r0", want: true},
		{running: "Chromium 152.0.7977.82", label: "152.0.7977.82-r3", want: true},
		{running: "Chromium 152.0.7977.82", label: "153.0.8000.10-r0", want: false},
		{running: "Chromium 152.0.7977.82", label: "152.0.7977.8-r0", want: false},
		{running: "Chromium 152.0.7977.82", label: "", want: false},
		{running: "", label: "152.0.7977.82-r0", want: false},
		{running: "HeadlessChrome", label: "unknown", want: false},
	}

	for _, tt := range tests {
		if got := sameChromium(tt.running, tt.label); got != tt.want {
			t.Errorf("sameChromium(%q, %q) = %v, want %v", tt.running, tt.label, got, tt.want)
		}
	}
}

func withChromium(u container.ImageUpdate, version string) container.ImageUpdate {
	u.LatestChromium = version

	return u
}
