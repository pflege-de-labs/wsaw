package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// `wsaw browser prune` (Story 6.13, AC12), against a runtime and a store
// faked in Go.

// TestBrowserPruneRefusesBadFlags covers the refusals made before any
// runtime or configuration is looked for.
func TestBrowserPruneRefusesBadFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"both rules", []string{"--keep", "2", "--keep-referenced"}, "cannot be given together"},
		{"keep zero", []string{"--keep", "0"}, "at least 1"},
		{"local runtime", []string{"--runtime", "local"}, "--runtime must be auto, podman or docker"},
		{"stray argument", []string{"now"}, "unexpected argument"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			err := cmdBrowserPrune(context.Background(), tt.args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("cmdBrowserPrune(%q) = %v, want an error containing %q", tt.args, err, tt.want)
			}
		})
	}
}

type pruneRuntime struct {
	images  []container.Image
	refuse  string
	removed []string
}

func (r *pruneRuntime) RefreshImages(context.Context) ([]container.Image, error) {
	var out []container.Image

	for _, img := range r.images {
		if _, ok := container.ParseRefreshTag(img.Name); ok {
			out = append(out, img)
		}
	}

	return out, nil
}

func (r *pruneRuntime) InspectImage(_ context.Context, name string) (container.Image, error) {
	for _, img := range r.images {
		if img.Name == name {
			return img, nil
		}
	}

	return container.Image{}, container.ErrImageMissing
}

func (r *pruneRuntime) RemoveImage(_ context.Context, name string) error {
	if name == r.refuse {
		return fmt.Errorf("removing browser image %s: image is in use by a container", name)
	}

	r.removed = append(r.removed, name)

	for i, img := range r.images {
		if img.Name == name {
			r.images = append(r.images[:i], r.images[i+1:]...)

			break
		}
	}

	return nil
}

type pruneStore struct {
	runs []store.MaintenanceRun
}

func (s *pruneStore) BrowserImageReferences(context.Context) (store.BrowserImageReferences, error) {
	return store.BrowserImageReferences{}, nil
}

func (s *pruneStore) MaintenanceRuns(_ context.Context, kind string, _ int) ([]store.MaintenanceRun, error) {
	var out []store.MaintenanceRun

	for _, r := range s.runs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}

	return out, nil
}

func (s *pruneStore) add(kind, trigger string, stats any, err error) error {
	raw, mErr := json.Marshal(stats)
	if mErr != nil {
		return mErr
	}

	run := store.MaintenanceRun{Kind: kind, Trigger: trigger, Stats: raw}
	if err != nil {
		run.Error = err.Error()
	}

	s.runs = append([]store.MaintenanceRun{run}, s.runs...)

	return nil
}

func (s *pruneStore) RecordBrowserImages(_ context.Context, trigger string, _ time.Time,
	inv store.BrowserImageInventory, err error,
) error {
	return s.add(store.MaintenanceKindBrowserImages, trigger, inv, err)
}

func (s *pruneStore) RecordBrowserPruneRun(_ context.Context, trigger string, _ time.Time,
	stats store.BrowserPruneStats, err error,
) error {
	return s.add(store.MaintenanceKindBrowserPrune, trigger, stats, err)
}

func refreshTag(day int) string {
	return container.BuildRefreshTag(time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC))
}

func pruneFixture() (*pruneRuntime, *pruneStore, app.BrowserImages) {
	rt := &pruneRuntime{images: []container.Image{
		{Name: refreshTag(4), ID: "sha256:d4", Size: 300 << 20},
		{Name: refreshTag(3), ID: "sha256:d3", Size: 300 << 20},
		{Name: refreshTag(2), ID: "sha256:d2", Size: 300 << 20},
		{Name: refreshTag(1), ID: "sha256:d1", Size: 300 << 20},
	}}
	st := &pruneStore{}

	images := app.BrowserImages{
		Runtime:   rt,
		Store:     st,
		Rule:      app.KeepRule{Count: 2},
		InService: browser.Identity{Image: refreshTag(3), ImageID: "sha256:d3"},
	}

	return rt, st, images
}

func TestBrowserPruneDryRunRemovesNothingAndSaysWhy(t *testing.T) {
	t.Parallel()

	rt, st, images := pruneFixture()

	var out bytes.Buffer

	if err := pruneBrowserImages(t.Context(), images, true, &out); err != nil {
		t.Fatal(err)
	}

	if len(rt.removed) != 0 || len(st.runs) != 0 {
		t.Errorf("a dry run removed %v and recorded %d runs", rt.removed, len(st.runs))
	}

	text := out.String()

	for _, want := range []string{
		"keep the newest 2", "keep (in-service)", "keep (kept)", "remove", "600.0 MiB", "nothing was removed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, text)
		}
	}
}

func TestBrowserPruneRemovesAndLeavesAReceipt(t *testing.T) {
	t.Parallel()

	rt, st, images := pruneFixture()

	var out bytes.Buffer

	if err := pruneBrowserImages(t.Context(), images, false, &out); err != nil {
		t.Fatal(err)
	}

	if strings.Join(rt.removed, ",") != refreshTag(2)+","+refreshTag(1) {
		t.Errorf("removed %v, want the two oldest", rt.removed)
	}

	if len(st.runs) != 2 || st.runs[0].Kind != store.MaintenanceKindBrowserImages ||
		st.runs[1].Kind != store.MaintenanceKindBrowserPrune || st.runs[1].Trigger != store.TriggerCLI {
		t.Fatalf("records = %+v, want a cli receipt and an inventory", st.runs)
	}

	if !strings.Contains(out.String(), "removed 2 image(s), 600.0 MiB") {
		t.Errorf("output = %q", out.String())
	}
}

func TestBrowserPruneFailsWhenTheRuntimeRefuses(t *testing.T) {
	t.Parallel()

	rt, st, images := pruneFixture()
	rt.refuse = refreshTag(1)

	var out bytes.Buffer

	err := pruneBrowserImages(t.Context(), images, false, &out)
	if err == nil {
		t.Fatal("a prune that could not remove an image reported success")
	}

	if !strings.Contains(out.String(), "removed 1 image(s)") || !strings.Contains(out.String(), "failed: ") {
		t.Errorf("output does not name what went and what did not:\n%s", out.String())
	}

	if receipt := st.runs[1]; receipt.Error == "" {
		t.Errorf("the receipt does not carry the failure: %+v", receipt)
	}
}
