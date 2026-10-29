package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// defaultPruneTimeout bounds a browser image prune: a listing, an inspection
// per image and a removal per image the rule lets go.
const defaultPruneTimeout = 5 * time.Minute

// cmdBrowserPrune removes the browser images the refresh made and the keep
// rule no longer wants, the way the daemon does after a switch (Story 6.13,
// AC12). It works whether or not the daemon is running, and whether or not
// the refresh is on.
func cmdBrowserPrune(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("browser prune", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var cf configFlags

	cf.register(fs)

	runtimeName := fs.String("runtime", string(container.KindAuto), "container runtime holding the images: auto, podman or docker")
	keep := fs.Int("keep", 0, "keep the newest N refresh images, in place of the configured rule")
	keepReferenced := fs.Bool("keep-referenced", false,
		"keep every refresh image a stored scan was rendered with, in place of the configured rule")
	dryRun := fs.Bool("dry-run", false, "list what would be removed and what is kept and why, and remove nothing")
	timeout := fs.Duration("timeout", defaultPruneTimeout, "give up after this long")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() > 0 {
		return fmt.Errorf("browser prune: unexpected argument %q", fs.Arg(0))
	}

	rule, override, err := pruneRuleFlags(fs, *keep, *keepReferenced)
	if err != nil {
		return err
	}

	kind := container.Kind(*runtimeName)
	if !kind.Valid() || kind == container.KindLocal {
		return fmt.Errorf("browser prune: --runtime must be auto, podman or docker, not %q", *runtimeName)
	}

	cfg, err := cf.load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// No browser: this command lists and removes images, and reads which of
	// them stored scans used.
	a, err := app.New(ctx, cfg, app.Options{Version: version, LogOutput: stderr})
	if err != nil {
		return err
	}

	defer func() { _ = a.Close() }()

	rt, err := container.Detect(ctx, kind)
	if err != nil {
		return fmt.Errorf("browser prune: %w", err)
	}

	if rt == nil {
		return fmt.Errorf("browser prune: no working podman or docker found; %w", container.ErrNoRuntime)
	}

	// The image in service is read from the store, where the daemon records
	// it, rather than asked of the daemon, which may not be running (AC9).
	images := app.NewBrowserImages(rt, a.Store, cfg, app.InServiceImage(ctx, a.Store))
	if override {
		images = images.WithRule(rule)
	}

	return pruneBrowserImages(ctx, images, *dryRun, stdout)
}

// pruneRuleFlags reads --keep and --keep-referenced, which replace the
// configured rule for one run and, like the settings, cannot both be given.
func pruneRuleFlags(fs *flag.FlagSet, keep int, referenced bool) (app.KeepRule, bool, error) {
	keepSet := false

	fs.Visit(func(f *flag.Flag) {
		if f.Name == "keep" {
			keepSet = true
		}
	})

	switch {
	case keepSet && referenced:
		return app.KeepRule{}, false, errors.New("browser prune: --keep and --keep-referenced cannot be given together")
	case keepSet && keep < 1:
		return app.KeepRule{}, false, errors.New("browser prune: --keep must be at least 1; the image in service is always kept")
	case keepSet:
		return app.KeepRule{Count: keep}, true, nil
	case referenced:
		return app.KeepRule{Referenced: true}, true, nil
	default:
		return app.KeepRule{}, false, nil
	}
}

// pruneBrowserImages plans, prints, and unless dryRun removes and records.
func pruneBrowserImages(ctx context.Context, images app.BrowserImages, dryRun bool, stdout io.Writer) error {
	started := time.Now()

	plan, planErr := images.Plan(ctx)
	if planErr != nil && !errors.Is(planErr, app.ErrReferencesUnknown) {
		return fmt.Errorf("browser prune: %w", planErr)
	}

	if err := printBrowserImagePlan(stdout, plan); err != nil {
		return err
	}

	if dryRun || planErr != nil {
		if _, err := fmt.Fprintln(stdout, "nothing was removed"); err != nil {
			return err
		}

		if planErr != nil {
			return fmt.Errorf("browser prune: %w", planErr)
		}

		return nil
	}

	stats, pruneErr := images.Prune(ctx, plan)

	after, err := images.Plan(ctx)
	if err != nil && !errors.Is(err, app.ErrReferencesUnknown) {
		after = plan
	}

	if err := images.Record(ctx, store.TriggerCLI, started, after, nil, &stats, pruneErr); err != nil {
		if _, werr := fmt.Fprintf(stdout, "warning: the prune was not recorded: %v\n", err); werr != nil {
			return werr
		}
	}

	// What was removed is reported before any failure: it is gone either way.
	if _, err := fmt.Fprintf(stdout, "removed %d image(s), %s; kept %d\n",
		len(stats.Removed), humanBytes(stats.BytesFreed), len(stats.Kept)); err != nil {
		return err
	}

	for _, f := range stats.Failed {
		if _, err := fmt.Fprintln(stdout, "failed: "+f); err != nil {
			return err
		}
	}

	if pruneErr != nil {
		return fmt.Errorf("browser prune: %w", pruneErr)
	}

	return nil
}

// printBrowserImagePlan lists every image with what the rule makes of it.
func printBrowserImagePlan(w io.Writer, plan store.BrowserImageInventory) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	if _, err := fmt.Fprintf(tw, "rule: %s\n\nIMAGE\tCHROMIUM\tSIZE\tSTORED SCANS\tVERDICT\n", plan.Rule); err != nil {
		return err
	}

	var total, freed int64

	for _, img := range plan.Images {
		verdict := "keep (" + img.State + ")"
		if img.State == app.ImagePrunable {
			verdict = "remove"
			freed += max(img.Size, 0)
		}

		total += max(img.Size, 0)

		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			img.Name, cmp.Or(img.Chromium, "-"), humanBytes(img.Size), img.Scans, verdict); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(tw, "\n%d image(s), %s in all; removing would free about %s\n",
		len(plan.Images), humanBytes(total), humanBytes(freed)); err != nil {
		return err
	}

	if plan.UnrecordedScans > 0 {
		if _, err := fmt.Fprintf(tw, "%d stored result(s) do not record which image rendered them and keep none\n",
			plan.UnrecordedScans); err != nil {
			return err
		}
	}

	return tw.Flush()
}

// humanBytes is report.FormatBytes, with a negative size — one the runtime
// could not report — said as such rather than shown as a number.
func humanBytes(n int64) string {
	if n < 0 {
		return "unknown"
	}

	return report.FormatBytes(n)
}
