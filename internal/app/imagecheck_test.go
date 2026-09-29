package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
)

var latestManifest = []byte(`{"schemaVersion":2,"manifests":[]}`)

func latestDigest() string {
	sum := sha256.Sum256(latestManifest)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// imageRegistry serves latestManifest as org/browser:latest, or fails with
// status when it is set, and counts the requests it receives.
func imageRegistry(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if status != 0 {
			w.WriteHeader(status)

			return
		}

		if r.URL.Path != "/v2/org/browser/manifests/latest" {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(latestManifest)
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

func TestCheckBrowserImage(t *testing.T) {
	t.Parallel()

	off := false
	older := "sha256:" + strings.Repeat("0", 64)

	tests := []struct {
		name     string
		status   int
		pinned   string // digest appended to the image; empty for a floating tag
		local    bool
		checkOff *bool
		want     string // expected in the log; empty expects no request at all
	}{
		{name: "newer image published", pinned: older, want: `level=WARN msg="a newer browser image is published`},
		{name: "pinned image is latest", pinned: latestDigest(), want: `level=INFO msg="browser image is the latest published"`},
		{name: "registry fails", pinned: older, status: http.StatusInternalServerError, want: `level=WARN msg="could not check for a newer browser image"`},
		{name: "check turned off", pinned: older, checkOff: &off},
		{name: "local browser", pinned: older, local: true},
		{name: "floating tag", pinned: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, hits := imageRegistry(t, tt.status)

			image := srv.Listener.Addr().String() + "/org/browser:0.3"
			if tt.pinned != "" {
				image += "@" + tt.pinned
			}

			var logged bytes.Buffer

			a := &App{
				Config:       &config.Config{Browser: config.Browser{Container: config.ContainerBrowser{CheckForUpdates: tt.checkOff}}},
				Logger:       slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
				Runtime:      &container.Runtime{Kind: container.KindPodman},
				BrowserImage: image,
				Version:      "test",
			}
			if tt.local {
				a.Runtime, a.BrowserImage = nil, ""
			}

			a.checkBrowserImage(context.Background(), srv.Client())

			if tt.want == "" {
				if n := hits.Load(); n != 0 {
					t.Fatalf("registry received %d requests, want none; log:\n%s", n, logged.String())
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

	srv, _ := imageRegistry(t, 0)
	repo := srv.Listener.Addr().String() + "/org/browser"

	var logged bytes.Buffer

	a := &App{
		Config:       &config.Config{},
		Logger:       slog.New(slog.NewTextHandler(&logged, nil)),
		Runtime:      &container.Runtime{Kind: container.KindDocker},
		BrowserImage: repo + "@sha256:" + strings.Repeat("0", 64),
	}

	a.checkBrowserImage(context.Background(), srv.Client())

	want := `pull="docker pull ` + repo + ":latest@" + latestDigest() + `"`
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("log does not contain %s:\n%s", want, logged.String())
	}
}
