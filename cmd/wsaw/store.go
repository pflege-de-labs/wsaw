package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/logging"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// cmdStore groups the maintenance a store needs occasionally and a running
// wsaw should not be doing on its own initiative.
//
// It is a command with subcommands rather than a flag on `run` because these
// are deliberate acts on stored evidence: an upgrade that cannot be undone,
// and later a rebuild of the index from the bucket (Story 8.11). An operator
// asks for those by name, at a time of their choosing, and reads what happened
// before starting the daemon again.
func cmdStore(ctx context.Context, args []string) error {
	if len(args) == 0 {
		storeUsage(os.Stderr)

		return fmt.Errorf("store: a subcommand is required")
	}

	switch sub, rest := args[0], args[1:]; sub {
	case "migrate":
		return cmdStoreMigrate(ctx, rest)

	case "prune":
		return cmdStorePrune(ctx, rest)

	case "sweep":
		return cmdStoreSweep(ctx, rest)

	case "help", "-h", "--help":
		storeUsage(os.Stdout)

		return nil

	default:
		storeUsage(os.Stderr)

		return fmt.Errorf("store: unknown subcommand %q", sub)
	}
}

func storeUsage(w *os.File) {
	// Best effort: there is nothing useful to do if the terminal has gone.
	_, _ = fmt.Fprint(w, `wsaw store — maintenance on the result store

Usage:
  wsaw store migrate [--dry-run]   Bring the store's schema up to date
  wsaw store prune   [--dry-run]   Apply retention and reclaim what it orphans
  wsaw store sweep   [--dry-run]   Delete artifacts nothing references any more

"migrate" applies whatever this build's schema needs, including the upgrade
that moves every stored result document out of the database and into the
artifact bucket. It is what starting wsaw would do anyway, run deliberately
and with a report of what it did.

"prune" applies the configured retention — store.maxAge and store.maxPerSeries
— and deletes the documents, screenshots and stored bodies the results it
removed were the last to reference. An artifact another result or a baseline
still names is kept: two scans that captured identical bytes share one object,
and so is one a scan running right now has taken. Both subcommands need the
artifact bucket to answer, and refuse to run when it does not: a bucket that
has gone away reports every key as already deleted.

"sweep" walks the artifact bucket and deletes what no stored result references
any more: objects left by an interrupted write, and keys an earlier prune could
not delete. It is safe to run while wsaw is scanning — an object written or
taken by a running scan within the last day is left alone, because that is what
a scan in progress looks like from outside. An object wsaw did not write is left
alone too, and reported separately. It reads every key wsaw owns, which against
object storage costs requests, so it is asked for rather than scheduled.

A sweep refuses to walk the bucket for a store that holds no results at all:
that is what a lost or restored-without-its-bucket index looks like, and
deleting on that basis cannot be undone. --allow-empty-index says the empty
history is real and sweeps anyway.

--dry-run reports what would be removed and removes nothing. Deleting evidence
is irreversible, so see the consequence of a retention change before it happens.
--limit bounds how many entries the report lists; --limit=0 lists all of them.
`)
}

// cmdStoreMigrate brings a store's schema up to date, or reports what doing so
// would move.
//
// Opening a store is what applies its migrations, so the command is mostly
// about being able to see the result: the store logs its progress through
// wsaw's logger, and what it moved is printed afterwards rather than left to
// be inferred from the log (Story 8.4, AC3 and AC6).
func cmdStoreMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("store migrate", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	dryRun := fs.Bool("dry-run", false, "report what would move, and write nothing")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cf.loadWithoutTargets()
	if err != nil {
		return err
	}

	// A registry of its own, so a DSN resolved from configuration cannot reach
	// the output of this command with its password intact.
	secrets := &secret.Registry{}

	opts, err := app.StoreOptions(cfg, secrets)
	if err != nil {
		return err
	}

	if *dryRun {
		return storeMigrateDryRun(ctx, opts)
	}

	logger, _, err := logging.New(logging.Options{
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		Output:  os.Stderr,
		Secrets: secrets,
	})
	if err != nil {
		return err
	}

	opts.Logger = logger

	return storeMigrateApply(ctx, opts)
}

// storeMigrateDryRun reports what an upgrade would move without moving it.
func storeMigrateDryRun(ctx context.Context, opts store.Options) error {
	plan, err := store.PlanDocumentMigration(ctx, opts)
	if err != nil {
		return err
	}

	fmt.Printf("store:     %s\n", opts.Location())
	fmt.Printf("bucket:    %s\n", plan.Bucket)

	if !plan.Pending() {
		fmt.Println("nothing to move: no result document is held in the database")

		return nil
	}

	fmt.Printf("documents: %d\n", plan.Rows)
	fmt.Printf("bytes:     %d\n", plan.Bytes)
	fmt.Println()
	// "Nothing was moved" rather than "nothing was written": reaching the
	// database and the bucket is how this command learns anything at all, and
	// opening a local artifact bucket creates its directory. Claiming a purity
	// the run does not have would undermine the trust a dry run is for.
	fmt.Println("No document was moved and no row was touched. Running \"wsaw store migrate\",")
	fmt.Println("or simply starting wsaw, moves those documents into the bucket and drops the")
	fmt.Println("column they are in. The move is one-way: back the database up first.")

	return nil
}

// storeMigrateApply opens the store, which is what applies the migrations, and
// reports what the one that moves documents did.
func storeMigrateApply(ctx context.Context, opts store.Options) error {
	s, err := store.Open(ctx, opts)
	if err != nil {
		return err
	}

	// Closed even where the report below fails: the point of a maintenance
	// command is that it leaves nothing open behind it.
	defer func() { _ = s.Close() }()

	// Migrating can take minutes, and a connection that has gone away in the
	// meantime would leave this command reporting success over a store nothing
	// can reach. The check costs one round trip.
	if err := s.Ping(ctx); err != nil {
		return err
	}

	fmt.Printf("store:  %s\n", opts.Location())
	fmt.Printf("driver: %s\n", s.Driver())

	m := s.DocumentMigration()
	if m == nil || m.Moved == 0 {
		fmt.Println("schema is up to date; no result document had to be moved")

		return nil
	}

	fmt.Printf("bucket: %s\n", m.Bucket)
	fmt.Printf("moved:  %s, %d bytes\n", plural(m.Moved, "document"), m.MovedBytes)

	if m.Unsummarised > 0 {
		fmt.Printf("moved but not summarised: %s (they did not decode, so the bytes are stored and the rows say why)\n",
			plural(m.Unsummarised, "document"))
	}

	return nil
}

// plural writes a count with its noun, because a report that says
// "1 documents" reads as a report nobody checked.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// maxListedArtifacts bounds how many keys a dry run prints by default. An
// operator tightening retention on a year of history would otherwise get a
// hundred thousand lines, which is not a report. The count is always exact;
// only the listing is trimmed, and it says so — and --limit raises or removes
// the trim for an operator who means to read the whole plan before deleting
// evidence that does not come back (Story 8.5, AC6).
const maxListedArtifacts = 20

// maintenance is one opened store plus what the subcommand that opened it needs
// to talk about it.
type maintenance struct {
	store  *store.Store
	opts   store.Options
	cfg    *config.Config
	dryRun bool
	// limit is how many results and artifacts a dry run lists. Zero means
	// every one of them.
	limit int
	close func()
}

// openForMaintenance parses the flags every store subcommand shares, loads
// configuration, and opens the store.
//
// Opening it is not incidental: a store cannot be pruned or swept until its
// schema is current, so these commands go through exactly the path `wsaw run`
// would and inherit its migrations and its logging rather than a second way in.
// The subcommand's own flags are registered through extra, so a flag that only
// one of them has — the sweep's acknowledgement of an empty index — does not
// appear in the other's help.
func openForMaintenance(
	ctx context.Context, name string, args []string, extra func(fs *flag.FlagSet),
) (*maintenance, error) {
	fs := flag.NewFlagSet("store "+name, flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	dryRun := fs.Bool("dry-run", false, "report what would be removed, and remove nothing")
	limit := fs.Int("limit", maxListedArtifacts,
		"how many results and artifacts a dry run lists; 0 lists every one of them")

	if extra != nil {
		extra(fs)
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *limit < 0 {
		return nil, fmt.Errorf("store %s: --limit cannot be negative", name)
	}

	cfg, err := cf.loadWithoutTargets()
	if err != nil {
		return nil, err
	}

	// A registry of its own, so a DSN resolved from configuration cannot reach
	// the output of this command with its password intact.
	secrets := &secret.Registry{}

	opts, err := app.StoreOptions(cfg, secrets)
	if err != nil {
		return nil, err
	}

	logger, _, err := logging.New(logging.Options{
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		Output:  os.Stderr,
		Secrets: secrets,
	})
	if err != nil {
		return nil, err
	}

	opts.Logger = logger

	s, err := store.Open(ctx, opts)
	if err != nil {
		return nil, err
	}

	return &maintenance{
		store:  s,
		opts:   opts,
		cfg:    cfg,
		dryRun: *dryRun,
		limit:  *limit,
		// Closed by the caller on every path: a maintenance command leaves
		// nothing open behind it.
		close: func() { _ = s.Close() },
	}, nil
}

// cmdStorePrune applies the configured retention and reclaims the artifacts it
// orphans, or reports what doing so would remove (Story 8.5, AC6).
func cmdStorePrune(ctx context.Context, args []string) error {
	m, err := openForMaintenance(ctx, "prune", args, nil)
	if err != nil {
		return err
	}

	defer m.close()

	retention := app.RetentionFor(m.cfg)

	fmt.Printf("store:     %s\n", m.opts.Location())
	fmt.Printf("bucket:    %s\n", m.opts.ArtifactLocation())
	fmt.Printf("retention: %s\n", describeRetention(retention))

	if retention.MaxAge <= 0 && retention.MaxPerSeries <= 0 {
		// Said rather than reported as a prune that removed nothing: an
		// operator who expected a reclaim needs to know that the reason is
		// their configuration and not an empty store.
		fmt.Println()
		fmt.Println("Nothing was removed: no retention is configured, so wsaw keeps every result.")
		fmt.Println("Set store.maxAge or store.maxPerSeries to bound the history.")

		return nil
	}

	stats, err := prune(ctx, m, retention)

	// Reported before the failure, and from the same statistics either way. A
	// prune commits its row deletions and then collects artifacts outside that
	// transaction, so a run that failed part way through the collection has
	// still removed everything it counted — and printing the error alone would
	// leave an operator with no account of a deletion that has already
	// happened and cannot be undone (AC5).
	reportPrune(stats, m.dryRun, m.limit)

	return err
}

func prune(ctx context.Context, m *maintenance, retention store.Retention) (store.PruneStats, error) {
	if m.dryRun {
		return m.store.PlanPrune(ctx, time.Now(), retention)
	}

	return m.store.Prune(ctx, time.Now(), retention)
}

// describeRetention states the policy in the words the configuration uses, so
// the report names the setting an operator would change.
func describeRetention(r store.Retention) string {
	var parts []string

	if r.MaxAge > 0 {
		parts = append(parts, "maxAge "+r.MaxAge.String())
	}

	if r.MaxPerSeries > 0 {
		parts = append(parts, fmt.Sprintf("maxPerSeries %d", r.MaxPerSeries))
	}

	if len(parts) == 0 {
		return "none configured"
	}

	return strings.Join(parts, ", ")
}

func reportPrune(stats store.PruneStats, dryRun bool, limit int) {
	verb := "removed"
	if dryRun {
		verb = "would be removed"
	}

	fmt.Printf("results:   %s %s\n", plural(stats.ResultsDeleted, "result"), verb)
	fmt.Printf("artifacts: %s %s, %d bytes\n",
		plural(stats.ArtifactsDeleted, "artifact"), verb, stats.BytesFreed)

	reportKept(stats.ArtifactsFailed, stats.ArtifactsProtected, stats.UnknownReferences)

	if !dryRun {
		return
	}

	listResults(stats.Results, limit)
	listArtifacts(stats.Artifacts, limit)

	fmt.Println()
	fmt.Println("Nothing was deleted. Run \"wsaw store prune\" without --dry-run to apply it.")
	fmt.Println("Deleted evidence does not come back: it is gone from the bucket, not from a")
	fmt.Println("recycle bin.")
}

// reportKept explains what was left behind and why, because an artifact that
// was not deleted is the interesting half of a retention run.
func reportKept(failed, protected, unknown int) {
	if failed > 0 {
		fmt.Printf("refused:   %s the bucket would not delete, left for the next sweep\n",
			plural(failed, "artifact"))
	}

	if protected > 0 {
		fmt.Printf("protected: %s left alone\n", plural(protected, "artifact"))
	}

	if unknown > 0 {
		fmt.Printf("unknown:   %s do not say which artifacts they reference, so no screenshot\n",
			plural(unknown, "result"))
		fmt.Println("           or stored body was collected; their documents are missing from the")
		fmt.Println("           bucket or no longer decode")
	}
}

func listResults(results []store.PrunedResult, limit int) {
	if len(results) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("results that would go:")

	for i, r := range results {
		if trimmed(i, len(results), limit) {
			break
		}

		fmt.Printf("  %s %s/%s %s\n",
			r.StartedAt.Format(time.RFC3339), r.Target, r.ConsentMode, r.ScanID)
	}
}

func listArtifacts(refs []string, limit int) {
	if len(refs) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("artifacts that would go:")

	for i, ref := range refs {
		if trimmed(i, len(refs), limit) {
			break
		}

		fmt.Printf("  %s\n", ref)
	}
}

// trimmed reports whether a listing has printed as much as it was asked to,
// and says how much it is leaving out when it has.
//
// A limit of zero prints everything. That is what --limit=0 is for: the counts
// above a trimmed listing are exact, but an operator about to delete evidence
// irreversibly may need to read the whole list rather than the first twenty of
// it (AC6).
func trimmed(printed, total, limit int) bool {
	if limit <= 0 || printed < limit {
		return false
	}

	fmt.Printf("  … and %d more (pass --limit=0 to list them all)\n", total-limit)

	return true
}

// cmdStoreSweep collects artifacts nothing references any more, or reports what
// doing so would collect (Story 8.5, AC3 and AC6).
func cmdStoreSweep(ctx context.Context, args []string) error {
	var allowEmptyIndex bool

	m, err := openForMaintenance(ctx, "sweep", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&allowEmptyIndex, "allow-empty-index", false,
			"sweep even though this store holds no results, deleting everything in the bucket")
	})
	if err != nil {
		return err
	}

	defer m.close()

	fmt.Printf("store:     %s\n", m.opts.Location())
	fmt.Printf("bucket:    %s\n", m.opts.ArtifactLocation())

	stats, err := sweep(ctx, m, store.SweepOptions{AllowEmptyIndex: allowEmptyIndex})

	// Printed before the failure is returned, for the same reason a prune's
	// report is: a sweep deletes as it walks, so what it managed to collect
	// before it stopped has happened and belongs in the account of it.
	reportSweep(stats, m.dryRun, m.limit)

	if errors.Is(err, store.ErrEmptyIndex) {
		fmt.Println()
		fmt.Println("Nothing in the bucket was examined. A store with no results references nothing,")
		fmt.Println("so every object in the bucket would look like garbage — including a full history")
		fmt.Println("whose database was restored without it, or a bucket that belongs to another")
		fmt.Println("wsaw. If this bucket really does hold nothing but leftovers, run")
		fmt.Println("\"wsaw store sweep --allow-empty-index\".")
	}

	return err
}

func reportSweep(stats store.SweepStats, dryRun bool, limit int) {
	verb := "deleted"
	if dryRun {
		verb = "would be deleted"
	}

	fmt.Printf("scanned:   %s, %d bytes\n", plural(stats.ArtifactsScanned, "artifact"), stats.BytesScanned)
	fmt.Printf("collected: %s %s, %d bytes\n",
		plural(stats.ArtifactsDeleted, "artifact"), verb, stats.BytesFreed)

	if stats.ForeignObjects > 0 {
		// Named rather than folded into the refused count, because the two
		// call for different actions: this one says the bucket holds something
		// wsaw did not write, which is a configuration to look at rather than
		// a failure to retry.
		fmt.Printf("foreign:   %s in the bucket were not written by wsaw and were left alone\n",
			plural(stats.ForeignObjects, "object"))
	}

	reportKept(stats.ArtifactsFailed, stats.ArtifactsProtected, stats.UnknownReferences)

	if !dryRun {
		return
	}

	listArtifacts(stats.Artifacts, limit)

	fmt.Println()
	fmt.Println("Nothing was deleted. Run \"wsaw store sweep\" without --dry-run to collect it.")
}

func sweep(ctx context.Context, m *maintenance, opts store.SweepOptions) (store.SweepStats, error) {
	if m.dryRun {
		return m.store.PlanSweep(ctx, time.Now(), opts)
	}

	return m.store.Sweep(ctx, time.Now(), opts)
}
