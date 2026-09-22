package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// cmdPrune applies retention once, outside the daemon (Story 4.10, AC10).
//
// It exists mostly for its dry run. A retention policy is retroactive — the
// first prune after an edit applies the new policy to everything already
// stored — and deleting evidence cannot be undone, so an operator needs to
// read the consequence before it happens rather than after.
func cmdPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	dryRun := fs.Bool("dry-run", false, "print what would be kept and deleted, and delete nothing")
	verbose := fs.Bool("all", false, "with --dry-run, list results that would be kept as well as deleted")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cf.load()
	if err != nil {
		return err
	}

	// No browser: this command reads and deletes stored results.
	a, err := app.New(ctx, cfg, app.Options{Version: version})
	if err != nil {
		return err
	}

	defer func() { _ = a.Close() }()

	retention, err := a.Retention()
	if err != nil {
		return err
	}

	if !retention.Active() {
		return fmt.Errorf("prune: no retention is configured; set store.keep, or store.maxAge and store.maxPerSeries")
	}

	now := time.Now()

	if *dryRun {
		stats, err := a.Store.PlanPrune(ctx, now, retention)
		if err != nil {
			return err
		}

		return printPrunePlan(os.Stdout, stats.Plans, *verbose)
	}

	stats, err := a.Store.Prune(ctx, now, retention)

	// What a partial prune managed to delete is reported before its error:
	// those results are gone either way, and a bare failure would hide it.
	//
	// The bytes are here because pruning reclaims evidence from the bucket as
	// well as rows now (Story 8.5): a policy that deleted a thousand results
	// and freed nothing is a policy whose evidence something else still names,
	// and that is worth seeing. "wsaw store prune" reports the whole account.
	fmt.Printf("deleted %d result(s) across %d series, kept %d; reclaimed %d artifact(s), %d bytes\n",
		stats.ResultsDeleted, stats.SeriesPruned, stats.ResultsKept,
		stats.ArtifactsDeleted, stats.BytesFreed)

	return err
}

// printPrunePlan writes one block per series, newest result first.
func printPrunePlan(w *os.File, plans []store.SeriesPlan, all bool) error {
	var deleted, kept int

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	for _, plan := range plans {
		deleted += plan.Deleted()
		kept += plan.Kept()

		if plan.Deleted() == 0 && !all {
			continue
		}

		if _, err := fmt.Fprintf(tw, "\n%s\t%s\t%d kept, %d to delete\n",
			plan.Series.Target, plan.Series.Mode, plan.Kept(), plan.Deleted()); err != nil {
			return err
		}

		for _, d := range plan.Decisions {
			if d.Keep && !all {
				continue
			}

			verdict := "delete"
			if d.Keep {
				verdict = "keep (" + d.Rule + ")"
			}

			if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				d.StartedAt.Format(time.RFC3339), d.ScanID, d.Termination, verdict); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprintf(tw, "\nwould delete %d result(s), keep %d; nothing was deleted\n",
		deleted, kept); err != nil {
		return err
	}

	return tw.Flush()
}
