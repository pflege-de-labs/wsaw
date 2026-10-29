package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestBrowserBuildRefusesBadFlags covers the refusals that happen before any
// runtime is looked for, so they hold on a machine without one (Story 6.12,
// AC1 and AC4).
func TestBrowserBuildRefusesBadFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"local runtime", []string{"--runtime", "local"}, "--runtime must be auto, podman or docker"},
		{"unknown runtime", []string{"--runtime", "lxc"}, "--runtime must be auto, podman or docker"},
		{"unknown base", []string{"--base", "ubuntu"}, "--base must be alpine or debian"},
		{"injected tag", []string{"--tag", "x; id"}, "is not an image name"},
		{"flag as tag", []string{"--tag", "--network=host"}, "is not an image name"},
		{"digest as tag", []string{"--tag", "localhost/wsaw-browser@sha256:abc"}, "is not an image name"},
		{"removed version flag", []string{"--chromium-version", "155.0-r0"}, "flag provided but not defined"},
		{"stray argument", []string{"now"}, "unexpected argument"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			err := cmdBrowserBuild(context.Background(), tt.args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("cmdBrowserBuild(%q) = %v, want an error containing %q", tt.args, err, tt.want)
			}

			if stdout.Len() > 0 {
				t.Errorf("a refused build printed %q", stdout.String())
			}
		})
	}
}

func TestBrowserNeedsASubcommand(t *testing.T) {
	t.Parallel()

	if err := cmdBrowser(context.Background(), []string{"pull"}); err == nil {
		t.Error(`"wsaw browser pull" was accepted`)
	}
}
