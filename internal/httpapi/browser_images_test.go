package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 6.13: the storage page's browser images section (AC13), and the
// result page's mark on a comparison across a change of browser (AC8).

func TestStoragePageSaysWhenNoImageInventoryIsRecorded(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)
	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	if !strings.Contains(html, "Browser images") || !strings.Contains(html, "no browser image inventory has been recorded yet") {
		t.Errorf("the section does not say that there is no inventory; body:\n%s", html)
	}

	// A missing inventory is not an empty one with zero bytes (Tenet 5).
	if strings.Contains(html, "Inventory taken") {
		t.Error("an inventory and its total were shown without one being recorded")
	}
}

func TestStoragePageListsTheRecordedBrowserImages(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)
	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	sql, ok := f.store.(*store.SQL)
	if !ok {
		t.Fatal("the fixture's store is not *store.SQL")
	}

	inv := store.BrowserImageInventory{
		Runtime: "podman 5.6.0",
		Rule:    "keep the newest 2",
		Images: []store.BrowserImage{
			{Name: "localhost/wsaw-browser:build-20261015T120000Z", ID: "sha256:new", Chromium: "Chromium 153.0.8001.4",
				Size: 600 << 20, State: "in-service", Made: time.Now()},
			{Name: "localhost/wsaw-browser:build-20261008T120000Z", ID: "sha256:old", Size: 400 << 20, State: "prunable",
				Scans: 3, NewestScan: time.Now()},
			{Name: "localhost/wsaw-browser:152.0.7977.82-r0", ID: "sha256:default", Size: -1, State: "kept"},
		},
		UnrecordedScans: 2,
	}

	if err := sql.RecordBrowserImages(t.Context(), store.TriggerRefresh, time.Now(), inv, nil); err != nil {
		t.Fatal(err)
	}

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	for _, want := range []string{
		"build-20261015T120000Z", "Chromium 153.0.8001.4", "in service",
		"due to be pruned at the next prune", "not made by the refresh",
		"1000.0 MiB", // the total of the two sizes reported
		"counted in every image that has it", "1 image(s) reported no size",
		"2 stored result(s) do not record which image rendered them",
		"wsaw browser prune --dry-run",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the section does not say %q; body:\n%s", want, html)
		}
	}

	var decoded struct {
		BrowserImages struct {
			Found        bool  `json:"found"`
			TotalBytes   int64 `json:"totalBytes"`
			UnknownSizes int   `json:"unknownSizes"`
			Images       []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"images"`
		} `json:"browserImages"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/storage", "Authorization", storageBearer))), &decoded); err != nil {
		t.Fatal(err)
	}

	bi := decoded.BrowserImages
	if !bi.Found || bi.TotalBytes != 1000<<20 || bi.UnknownSizes != 1 || len(bi.Images) != 3 || bi.Images[1].State != "prunable" {
		t.Errorf("the JSON does not carry what the page shows: %+v", bi)
	}
}

func TestResultPageMarksAComparisonAcrossAChangeOfBrowser(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	now := time.Now()

	f.seed("scan-1", model.ConsentReject, now.Add(-time.Hour), func(r *model.Result) {
		r.Environment.ChromeVersion = "Chromium 152.0.7977.82"
		r.Environment.BrowserImage = "localhost/wsaw-browser:build-20261008T120000Z"
	})
	f.seed("scan-2", model.ConsentReject, now, func(r *model.Result) {
		r.Environment.ChromeVersion = "Chromium 153.0.8001.4"
		r.Environment.BrowserImage = "localhost/wsaw-browser:build-20261015T120000Z"
	})

	html := body(t, f.get("/results/site/reject/scan-2", "Accept", "text/html"))

	for _, want := range []string{`id="browser-changed"`, "Chromium 152.0.7977.82", "Chromium 153.0.8001.4"} {
		if !strings.Contains(html, want) {
			t.Errorf("the result page does not show %q; body:\n%s", want, html)
		}
	}

	plain := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))
	if strings.Contains(plain, `id="browser-changed"`) {
		t.Error("a scan with nothing to compare against was marked")
	}
}
