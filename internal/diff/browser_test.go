package diff_test

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// TestAComparisonAcrossABrowserChangeIsMarked is Story 6.13, AC8: a pair of
// scans rendered by different browsers says so, names both, and keeps every
// change it found at the severity it found it.
func TestAComparisonAcrossABrowserChangeIsMarked(t *testing.T) {
	t.Parallel()

	env := func(version, image, id string) func(*model.Result) {
		return func(r *model.Result) {
			r.Environment.BrowserRuntime = "podman"
			r.Environment.ChromeVersion = version
			r.Environment.BrowserImage = image
			r.Environment.BrowserImageID = id
		}
	}

	tests := []struct {
		name          string
		before, after func(*model.Result)
		marked        bool
	}{
		{"same browser", env("Chromium 152", "i:1", "sha256:a"), env("Chromium 152", "i:1", "sha256:a"), false},
		{"new version", env("Chromium 152", "i:1", "sha256:a"), env("Chromium 153", "i:2", "sha256:b"), true},
		{"same version, new image", env("Chromium 152", "i:1", "sha256:a"), env("Chromium 152", "i:2", "sha256:b"), true},
		// A result from before IDs were recorded says nothing about its
		// image's content; only the version can tell.
		{"one side unrecorded", env("Chromium 152", "i:1", ""), env("Chromium 152", "i:2", "sha256:b"), false},
		{"new name, same image", env("Chromium 152", "i:1", "sha256:a"), env("Chromium 152", "i:2", "sha256:a"), false},
		{
			"container to local",
			env("Chromium 152", "i:1", "sha256:a"),
			func(r *model.Result) {
				r.Environment.BrowserRuntime = "local"
				r.Environment.ChromeVersion = "Chromium 152"
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := result(model.ConsentReject, req("https://example.com/a.js", "example.com", model.FirstParty))
			after := result(model.ConsentReject,
				req("https://example.com/a.js", "example.com", model.FirstParty),
				req("https://tracker.test/t.js", "tracker.test", model.ThirdParty, pre))

			tt.before(before)
			tt.after(after)

			rep := diff.Compare(before, after, diff.Options{})

			if (rep.BrowserChanged != nil) != tt.marked {
				t.Fatalf("BrowserChanged = %+v, want marked=%v", rep.BrowserChanged, tt.marked)
			}

			// The mark explains; it removes and softens nothing (Tenet 5).
			unmarked := diff.Compare(before, after, diff.Options{})
			unmarked.BrowserChanged = nil

			if c := find(t, rep, diff.HostAdded, "tracker.test"); c.Severity != find(t, unmarked, diff.HostAdded, "tracker.test").Severity {
				t.Errorf("the mark changed a finding's severity: %+v", c)
			}

			if tt.marked && (rep.BrowserChanged.Before.Version != before.Environment.ChromeVersion ||
				rep.BrowserChanged.After.Version != after.Environment.ChromeVersion) {
				t.Errorf("the mark does not name both browsers: %+v", rep.BrowserChanged)
			}
		})
	}
}

func TestBrowserString(t *testing.T) {
	t.Parallel()

	for b, want := range map[diff.Browser]string{
		{Version: "Chromium 153", Image: "localhost/wsaw-browser:build-1"}: "Chromium 153 (localhost/wsaw-browser:build-1)",
		{Version: "Chromium 153"}: "Chromium 153",
		{}:                        "an unrecorded browser",
	} {
		if got := b.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", b, got, want)
		}
	}
}
