package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// TestBrowserImageReferencesComeFromTheIndex is Story 6.13, AC9: which images
// the stored scans were rendered with is read from the index, a result that
// names an image but no ID protects nothing and is counted, and a local
// browser's result is neither.
func TestBrowserImageReferencesComeFromTheIndex(t *testing.T) {
	t.Parallel()

	s := openSQL(t)
	now := time.Now().UTC().Truncate(time.Second)

	put := func(id string, at time.Time, image, imageID string) {
		t.Helper()

		res := result(id, at, model.ConsentReject)
		res.Environment.BrowserImage = image
		res.Environment.BrowserImageID = imageID

		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	put("a1", now.Add(-3*time.Hour), "localhost/wsaw-browser:build-1", "sha256:aaa")
	put("a2", now.Add(-time.Hour), "localhost/wsaw-browser:build-1", "sha256:aaa")
	put("b1", now.Add(-2*time.Hour), "localhost/wsaw-browser:build-2", "sha256:bbb")
	put("old", now.Add(-4*time.Hour), "localhost/wsaw-browser:152.0.7977.82-r0", "")
	put("local", now, "", "")

	refs, err := s.BrowserImageReferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if got := refs.ByID["sha256:aaa"]; got.Scans != 2 || !got.NewestScan.Equal(now.Add(-time.Hour)) {
		t.Errorf("sha256:aaa = %+v, want 2 scans, newest an hour ago", got)
	}

	if got := refs.ByID["sha256:bbb"]; got.Scans != 1 {
		t.Errorf("sha256:bbb = %+v, want 1 scan", got)
	}

	if len(refs.ByID) != 2 {
		t.Errorf("ByID = %v, want only the two recorded images", refs.ByID)
	}

	if refs.Unrecorded != 1 {
		t.Errorf("Unrecorded = %d, want the one containerised result without an ID", refs.Unrecorded)
	}

	// Retention drops a scan and its reference with it.
	if _, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{Keep: &store.Keep{Last: 1}}); err != nil {
		t.Fatal(err)
	}

	refs, err = s.BrowserImageReferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := refs.ByID["sha256:bbb"]; ok || refs.Unrecorded != 0 {
		t.Errorf("after keeping only the newest scan, references = %+v", refs)
	}
}

// TestTheImageIDSurvivesTheRoundTrip keeps the document the record: the ID a
// scan recorded comes back with the result (Tenet 4).
func TestTheImageIDSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	s := openSQL(t)

	res := result("scan", time.Now(), model.ConsentNone)
	res.Environment.BrowserImage = "localhost/wsaw-browser:build-1"
	res.Environment.BrowserImageID = "sha256:aaa"

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetResult(res.Target, res.ConsentMode, res.ScanID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Environment.BrowserImageID != "sha256:aaa" {
		t.Errorf("BrowserImageID = %q after a round trip", got.Environment.BrowserImageID)
	}
}

// TestBrowserImageRunsReadBack is AC12 and AC13: a prune's receipt and an
// inventory are recorded as runs of their own kinds and read back as written.
func TestBrowserImageRunsReadBack(t *testing.T) {
	t.Parallel()

	s := openSQL(t)
	started := time.Now()

	img := store.BrowserImage{Name: "localhost/wsaw-browser:build-1", ID: "sha256:aaa", Size: 300, State: "prunable"}
	stats := store.BrowserPruneStats{Rule: "newest 2", Removed: []store.BrowserImage{img}, BytesFreed: 300}

	if err := s.RecordBrowserPruneRun(t.Context(), store.TriggerCLI, started, stats, errors.New("one refused")); err != nil {
		t.Fatal(err)
	}

	inv := store.BrowserImageInventory{Runtime: "podman", Rule: "newest 2", Images: []store.BrowserImage{img}, UnrecordedScans: 4}
	if err := s.RecordBrowserImages(t.Context(), store.TriggerSchedule, started, inv, nil); err != nil {
		t.Fatal(err)
	}

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindBrowserPrune)
	if err != nil || !found {
		t.Fatalf("LastMaintenanceRun(browser-prune) = %v, %v", found, err)
	}

	gotStats, err := run.BrowserPruneStats()
	if err != nil || gotStats.BytesFreed != 300 || len(gotStats.Removed) != 1 || run.Error != "one refused" ||
		run.Trigger != store.TriggerCLI {
		t.Errorf("browser-prune receipt = %+v %+v, %v", run, gotStats, err)
	}

	run, found, err = s.LastMaintenanceRun(t.Context(), store.MaintenanceKindBrowserImages)
	if err != nil || !found {
		t.Fatalf("LastMaintenanceRun(browser-images) = %v, %v", found, err)
	}

	gotInv, err := run.BrowserImages()
	if err != nil || gotInv.Runtime != "podman" || gotInv.UnrecordedScans != 4 || gotInv.Images[0] != img {
		t.Errorf("inventory = %+v, %v", gotInv, err)
	}

	// The prune log is untouched by either.
	if _, found, _ := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune); found {
		t.Error("a browser image run was read back as a retention prune")
	}
}
