package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is the store's half of the browser image refresh (Story 6.13).
//
// A prune that keeps "every image a stored scan was rendered with" asks the
// index which images its results name, so each results row carries the ID of
// the image that rendered it, copied out of the document the way the summary
// columns are. The prune's receipt and the image inventory the storage page
// shows are maintenance runs of their own kinds: a receipt is what that log
// is for, and an inventory is a snapshot taken at a moment, read back newest
// first, which is the same shape.

// schemaBrowserImageIDs is the schema version that adds results'
// browser_image_id column.
const schemaBrowserImageIDs = 13

// browserImageIDColumn is the results column holding the image a scan was
// rendered with.
const browserImageIDColumn = "browser_image_id"

// imageIDUnrecorded is what browser_image_id holds for a containerised scan
// whose document names no image ID, and what every row written before
// schemaBrowserImageIDs defaults to. Such a row references no image: wsaw
// cannot say which one it used, and treating it as a reference to all of them
// would keep every image for ever. It is counted instead, so the caller can
// say how many results protect nothing (AC9, Tenet 5). A scan rendered by a
// local browser holds the empty string: it used no image at all.
const imageIDUnrecorded = "unrecorded"

// The maintenance run kinds this file adds.
const (
	MaintenanceKindBrowserPrune  = "browser-prune"
	MaintenanceKindBrowserImages = "browser-images"
)

// The audit actions a change of browser records (Story 6.13, AC16): a
// refresh switching the pool, and a daemon starting on another browser than
// the one last recorded in service.
const (
	AuditBrowserSwitched = "browser-switched"
	AuditBrowserChanged  = "browser-changed"
)

// The triggers an inventory records besides TriggerSchedule and TriggerCLI:
// the daemon's start, and a refresh. The refresh schedule is kept by the last
// inventory a refresh recorded, so a restart does not rebuild at once.
const (
	TriggerStartup = "startup"
	TriggerRefresh = "refresh"
)

// resultImage is the column list resultUpdate gains for it. A list of its own
// for the reason resultConfidence is one: the document migration at version 3
// writes exactly resultDerived, long before this column exists.
var resultImage = []string{browserImageIDColumn}

// imageIDOf is the browser_image_id a result's row carries.
func imageIDOf(res *model.Result) string {
	switch {
	case res.Environment.BrowserImageID != "":
		return res.Environment.BrowserImageID
	case res.Environment.BrowserImage != "":
		return imageIDUnrecorded
	default:
		return ""
	}
}

// BrowserImageReference is how many stored scans name one image, and when
// the newest of them started.
type BrowserImageReference struct {
	Scans      int       `json:"scans"`
	NewestScan time.Time `json:"newestScan"`
}

// BrowserImageReferences is what the stored results say about the images
// that rendered them.
type BrowserImageReferences struct {
	// ByID maps an image ID to the stored scans it rendered.
	ByID map[string]BrowserImageReference
	// Unrecorded counts the stored results that do not record which image
	// rendered them. They reference nothing.
	Unrecorded int
}

// BrowserImageReferences reads, from the index alone, which images the stored
// results were rendered with.
func (s *SQL) BrowserImageReferences(ctx context.Context) (BrowserImageReferences, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select ` + browserImageIDColumn + `, count(*), max(started_at) from ` + resultsTable +
		` where ` + browserImageIDColumn + ` <> '' group by ` + browserImageIDColumn

	var out BrowserImageReferences

	err := s.retry(ctx, "reading which browser images stored scans used", func(ctx context.Context) error {
		out = BrowserImageReferences{ByID: map[string]BrowserImageReference{}}

		rows, err := s.db.QueryContext(ctx, s.q(q))
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				id     string
				scans  int
				newest int64
			)

			if err := rows.Scan(&id, &scans, &newest); err != nil {
				return err
			}

			if id == imageIDUnrecorded {
				out.Unrecorded = scans

				continue
			}

			out.ByID[id] = BrowserImageReference{Scans: scans, NewestScan: time.Unix(0, newest).UTC()}
		}

		return rows.Err()
	})
	if err != nil {
		return BrowserImageReferences{}, fmt.Errorf("reading browser image references: %w", err)
	}

	return out, nil
}

// BrowserImage is one image as a prune or an inventory saw it.
type BrowserImage struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// Chromium is the version the image's browser reports, where known.
	Chromium string `json:"chromium,omitempty"`
	// Made is when the refresh built or pulled it; zero for an image the
	// refresh did not make.
	Made time.Time `json:"made,omitzero"`
	// Size is what the runtime reports the image occupies. Layers shared
	// with other images are counted in each.
	Size int64 `json:"size"`
	// State is what the prune rule makes of it: in-service, kept, referenced,
	// pinned, or prunable.
	State string `json:"state"`
	// Scans and NewestScan say which stored scans it rendered.
	Scans      int       `json:"scans"`
	NewestScan time.Time `json:"newestScan,omitzero"`
}

// BrowserImageInventory is the set of browser images on this machine at one
// moment, as the storage page shows it.
type BrowserImageInventory struct {
	Runtime string `json:"runtime"`
	// Rule is the keep rule the states were decided by.
	Rule   string         `json:"rule"`
	Images []BrowserImage `json:"images"`
	// UnrecordedScans counts stored results that name no image ID.
	UnrecordedScans int `json:"unrecordedScans"`
}

// BrowserPruneStats is what a browser image prune did.
type BrowserPruneStats struct {
	Rule    string         `json:"rule"`
	Removed []BrowserImage `json:"removed"`
	Kept    []BrowserImage `json:"kept"`
	// Failed lists the images whose removal the runtime refused, with why.
	Failed     []string `json:"failed,omitempty"`
	BytesFreed int64    `json:"bytesFreed"`
}

// RecordBrowserPruneRun records a completed browser image prune, never a plan.
func (s *SQL) RecordBrowserPruneRun(
	ctx context.Context, trigger string, started time.Time, stats BrowserPruneStats, runErr error,
) error {
	return s.recordMaintenanceRun(ctx, MaintenanceKindBrowserPrune, trigger, started, stats, runErr)
}

// RecordBrowserImages records an inventory of the browser images. The trigger
// says what prompted it: the daemon's start, a refresh, or a prune.
func (s *SQL) RecordBrowserImages(
	ctx context.Context, trigger string, taken time.Time, inv BrowserImageInventory, runErr error,
) error {
	return s.recordMaintenanceRun(ctx, MaintenanceKindBrowserImages, trigger, taken, inv, runErr)
}

// BrowserPruneStats decodes a browser image prune's receipt.
func (r MaintenanceRun) BrowserPruneStats() (BrowserPruneStats, error) {
	var s BrowserPruneStats

	err := json.Unmarshal(r.Stats, &s)

	return s, err
}

// BrowserImages decodes a browser image inventory.
func (r MaintenanceRun) BrowserImages() (BrowserImageInventory, error) {
	var inv BrowserImageInventory

	err := json.Unmarshal(r.Stats, &inv)

	return inv, err
}
