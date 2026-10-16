package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// This file decides which browser images the refresh made are kept, for the
// daemon after a switch and for `wsaw browser prune` alike (Story 6.13, AC9
// and AC12). There is one planner so that a scheduled prune and a typed one
// can never disagree about what goes.
//
// What the planner may remove is narrow on purpose. Only an image under a
// refresh tag is a candidate; an image an operator built, pulled or named is
// not, whatever it holds. Of the candidates it never removes the image in
// service, the one the configuration names, or the newest one the refresh
// made — the image in service, or the one about to be — and the runtime
// itself refuses to remove an image a running container uses.

// Image states, as store.BrowserImage.State records them.
const (
	ImageInService  = "in-service"
	ImagePinned     = "pinned"
	ImageKept       = "kept"
	ImageReferenced = "referenced"
	ImagePrunable   = "prunable"
)

// KeepRule is which of the refresh's images a prune keeps: the newest Count,
// or every one a stored scan was rendered with.
type KeepRule struct {
	Count      int
	Referenced bool
}

// KeepRuleFor reads the rule out of configuration.
func KeepRuleFor(ct config.ContainerBrowser) KeepRule {
	if ct.KeepReferenced() {
		return KeepRule{Referenced: true}
	}

	return KeepRule{Count: ct.KeepCount()}
}

// String names the rule for a log line, a receipt and the storage page.
func (k KeepRule) String() string {
	if k.Referenced {
		return "keep every image a stored scan was rendered with"
	}

	return "keep the newest " + strconv.Itoa(k.Count)
}

// imageRuntime is what planning and pruning ask of a container runtime. It is
// declared here, where it is consumed, so the planner is tested against a fake
// in Go rather than against a runtime.
type imageRuntime interface {
	RefreshImages(ctx context.Context) ([]container.Image, error)
	InspectImage(ctx context.Context, image string) (container.Image, error)
	RemoveImage(ctx context.Context, name string) error
}

// browserImageStore is what the refresh and the prune ask of a store: which
// images stored scans name, and the two records they leave. Only the SQL
// store has them, and a caller without one plans without references.
type browserImageStore interface {
	BrowserImageReferences(ctx context.Context) (store.BrowserImageReferences, error)
	MaintenanceRuns(ctx context.Context, kind string, limit int) ([]store.MaintenanceRun, error)
	RecordBrowserImages(ctx context.Context, trigger string, taken time.Time, inv store.BrowserImageInventory, err error) error
	RecordBrowserPruneRun(ctx context.Context, trigger string, started time.Time, stats store.BrowserPruneStats, err error) error
}

// ErrReferencesUnknown is why a prune under the referenced rule removed
// nothing: without the stored scans' references, every image might be one.
var ErrReferencesUnknown = errors.New("which images stored scans use could not be read, so nothing is pruned")

// BrowserImages plans and prunes the browser images on one runtime.
type BrowserImages struct {
	Runtime imageRuntime
	// Store is nil where there is none; then nothing is referenced, and the
	// referenced rule prunes nothing.
	Store browserImageStore
	Rule  KeepRule
	// InService is the image new browsers start from, as far as is known.
	InService browser.Identity
	// Pinned is the image the configuration names, never removed.
	Pinned string
	// RuntimeName is recorded with the inventory.
	RuntimeName string
}

// Plan lists the images and decides each one's state, removing nothing.
// The error is ErrReferencesUnknown, with the plan, when the referenced rule
// is in force and the store could not answer: the plan then keeps everything.
func (b BrowserImages) Plan(ctx context.Context) (store.BrowserImageInventory, error) {
	images, err := b.Runtime.RefreshImages(ctx)
	if err != nil {
		return store.BrowserImageInventory{}, err
	}

	images = b.withInService(ctx, images)

	refs, refsErr := b.references(ctx)

	inv := store.BrowserImageInventory{
		Runtime:         b.RuntimeName,
		Rule:            b.Rule.String(),
		Images:          planImages(images, b, refs),
		UnrecordedScans: refs.Unrecorded,
	}

	if refsErr != nil && b.Rule.Referenced {
		for i := range inv.Images {
			if inv.Images[i].State == ImagePrunable {
				inv.Images[i].State = ImageKept
			}
		}

		return inv, fmt.Errorf("%w: %w", ErrReferencesUnknown, refsErr)
	}

	b.carryVersions(ctx, &inv)

	return inv, nil
}

// withInService adds the image in service when it is not a refresh image —
// the default image, or one an operator named — so the inventory shows it.
func (b BrowserImages) withInService(ctx context.Context, images []container.Image) []container.Image {
	name := b.InService.Image
	if name == "" {
		return images
	}

	for _, img := range images {
		if img.Name == name {
			return images
		}
	}

	img, err := b.Runtime.InspectImage(ctx, name)
	if err != nil {
		// Shown without a size rather than left out: it is still what new
		// browsers start from.
		img = container.Image{Name: name, ID: b.InService.ImageID, Size: -1}
	}

	return append(images, img)
}

// references reads which images stored scans name. With no store, or one
// that cannot answer, it returns empty references and says why.
func (b BrowserImages) references(ctx context.Context) (store.BrowserImageReferences, error) {
	if b.Store == nil {
		return store.BrowserImageReferences{}, errors.New("no store is configured")
	}

	return b.Store.BrowserImageReferences(ctx)
}

// carryVersions fills in each image's Chromium from the image in service and
// from the newest earlier inventory, since asking an image costs starting it.
func (b BrowserImages) carryVersions(ctx context.Context, inv *store.BrowserImageInventory) {
	known := map[string]string{}

	if prev, ok := lastInventory(ctx, b.Store); ok {
		for _, img := range prev.Images {
			if img.Chromium != "" {
				known[img.ID] = img.Chromium
			}
		}
	}

	if b.InService.ImageID != "" {
		known[b.InService.ImageID] = b.InService.Version
	}

	for i := range inv.Images {
		if inv.Images[i].Chromium == "" {
			inv.Images[i].Chromium = known[inv.Images[i].ID]
		}
	}
}

// planImages decides each image's state. Refresh images come newest first;
// anything else is the image in service and is listed last.
func planImages(images []container.Image, b BrowserImages, refs store.BrowserImageReferences) []store.BrowserImage {
	out := make([]store.BrowserImage, 0, len(images))
	kept, newest := 0, true

	for _, img := range images {
		row := store.BrowserImage{Name: img.Name, ID: img.ID, Size: img.Size}

		tag, refreshed := container.ParseRefreshTag(img.Name)
		if refreshed {
			row.Made = tag.At
		}

		if ref, ok := refs.ByID[img.ID]; ok {
			row.Scans, row.NewestScan = ref.Scans, ref.NewestScan
		}

		row.State = imageState(img, b, row.Scans, refreshed, newest, &kept)

		if refreshed {
			newest = false
		}

		out = append(out, row)
	}

	return out
}

// imageState applies the rule to one image. kept counts the images the count
// rule has kept so far, the image in service among them.
func imageState(img container.Image, b BrowserImages, scans int, refreshed, newest bool, kept *int) string {
	inService := img.Name == b.InService.Image || (img.ID != "" && img.ID == b.InService.ImageID)

	switch {
	case inService:
		*kept++

		return ImageInService
	case img.Name == b.Pinned:
		return ImagePinned
	case !refreshed:
		// Not the refresh's to remove.
		return ImageKept
	case b.Rule.Referenced && scans > 0:
		return ImageReferenced
	case b.Rule.Referenced:
		if newest {
			return ImageKept
		}

		return ImagePrunable
	case *kept < b.Rule.Count || newest:
		*kept++

		return ImageKept
	default:
		return ImagePrunable
	}
}

// Prune removes every image the plan marks prunable. A removal the runtime
// refuses is recorded and the rest go on: the stats say what was freed and
// what was not, and the error says that not everything went (Tenet 5).
func (b BrowserImages) Prune(ctx context.Context, plan store.BrowserImageInventory) (store.BrowserPruneStats, error) {
	stats := store.BrowserPruneStats{Rule: plan.Rule, Removed: []store.BrowserImage{}, Kept: []store.BrowserImage{}}

	for _, img := range plan.Images {
		if img.State != ImagePrunable {
			stats.Kept = append(stats.Kept, img)

			continue
		}

		if err := b.Runtime.RemoveImage(ctx, img.Name); err != nil {
			stats.Failed = append(stats.Failed, err.Error())
			stats.Kept = append(stats.Kept, img)

			continue
		}

		stats.Removed = append(stats.Removed, img)
		stats.BytesFreed += max(img.Size, 0)
	}

	if len(stats.Failed) > 0 {
		return stats, fmt.Errorf("%d of %d browser images could not be removed",
			len(stats.Failed), len(stats.Failed)+len(stats.Removed))
	}

	return stats, nil
}

// lastInventory reads the newest recorded inventory.
func lastInventory(ctx context.Context, st browserImageStore) (store.BrowserImageInventory, bool) {
	run, ok := lastBrowserImagesRun(ctx, st, "")
	if !ok {
		return store.BrowserImageInventory{}, false
	}

	inv, err := run.BrowserImages()
	if err != nil {
		return store.BrowserImageInventory{}, false
	}

	return inv, true
}

// lastBrowserImagesRun reads the newest inventory record, of one trigger when
// trigger is set.
func lastBrowserImagesRun(ctx context.Context, st browserImageStore, trigger string) (store.MaintenanceRun, bool) {
	if st == nil {
		return store.MaintenanceRun{}, false
	}

	runs, err := st.MaintenanceRuns(ctx, store.MaintenanceKindBrowserImages, 0)
	if err != nil {
		return store.MaintenanceRun{}, false
	}

	for _, run := range runs {
		if trigger == "" || run.Trigger == trigger {
			return run, true
		}
	}

	return store.MaintenanceRun{}, false
}

// InServiceImage is the image the newest inventory says new browsers start
// from, for a caller that is not the daemon: `wsaw browser prune` must not
// remove it, and asks the store rather than the daemon (AC9).
func InServiceImage(ctx context.Context, st store.Store) browser.Identity {
	bs, ok := st.(browserImageStore)
	if !ok {
		return browser.Identity{}
	}

	inv, ok := lastInventory(ctx, bs)
	if !ok {
		return browser.Identity{}
	}

	for _, img := range inv.Images {
		if img.State == ImageInService {
			return browser.Identity{Version: img.Chromium, Image: img.Name, ImageID: img.ID}
		}
	}

	return browser.Identity{}
}

// NewBrowserImages plans and prunes the images on rt under the configured
// rule, with st's references and records where it has them. It is how the
// daemon and `wsaw browser prune` build the same planner from the same
// configuration.
func NewBrowserImages(rt *container.Runtime, st store.Store, cfg *config.Config, inService browser.Identity) BrowserImages {
	b := BrowserImages{
		Runtime:     rt,
		Rule:        KeepRuleFor(cfg.Browser.Container),
		InService:   inService,
		Pinned:      cfg.Browser.Container.Image,
		RuntimeName: rt.String(),
	}

	if bs, ok := st.(browserImageStore); ok {
		b.Store = bs
	}

	return b
}

// WithRule returns the planner under another rule, for the prune command's
// --keep and --keep-referenced, which replace the configured rule for one run.
func (b BrowserImages) WithRule(rule KeepRule) BrowserImages {
	b.Rule = rule

	return b
}

// Record writes the inventory, and the prune's receipt when there was one, to
// the store. A store that cannot take them is reported, never fatal: what was
// pruned is pruned either way.
func (b BrowserImages) Record(ctx context.Context, trigger string, started time.Time,
	inv store.BrowserImageInventory, invErr error, prune *store.BrowserPruneStats, pruneErr error,
) error {
	if b.Store == nil {
		return nil
	}

	var errs []error

	if prune != nil {
		errs = append(errs, b.Store.RecordBrowserPruneRun(ctx, trigger, started, *prune, pruneErr))
	}

	errs = append(errs, b.Store.RecordBrowserImages(ctx, trigger, started, inv, invErr))

	return errors.Join(errs...)
}
