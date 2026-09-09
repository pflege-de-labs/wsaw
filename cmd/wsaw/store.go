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
// are deliberate acts on stored evidence: an upgrade that cannot be undone, a
// deletion that does not come back, and a rebuild of the index from the bucket
// (Story 8.11). An operator asks for those by name, at a time of their
// choosing, and reads what happened before starting the daemon again.
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

	case "rebuild-index":
		return cmdStoreRebuildIndex(ctx, rest)

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
  wsaw store migrate       [--dry-run]   Bring the store's schema up to date
  wsaw store prune         [--dry-run]   Apply retention and reclaim what it orphans
  wsaw store sweep         [--dry-run]   Delete artifacts nothing references any more
  wsaw store rebuild-index [--dry-run]   Rebuild the index from the bucket
  wsaw store rebuild-index --verify      Check the index against the bucket

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

"rebuild-index" derives the index from the documents in the bucket: every
object under the result prefix is read, decoded, and turned into the same index
entry and the same summary the scan that stored it would have written. It is
the recovery procedure for an index that was lost, restored without its bucket,
or left incomplete, and it is the supported way to move a history between store
kinds — point the new store at the same bucket and run it.

It merges and never deletes. An entry it does not find a document for is
reported and left exactly where it is, because a bucket that lost an object and
a listing that has not caught up look the same from here. Interrupting it is
safe: what it wrote stays written, and running it again finishes the job
without repeating a single write — though it re-reads the whole bucket to work
out what that is.

It does not put back a scan retention removed. A pruned result whose evidence a
baseline still names leaves its document in the bucket, and re-indexing it would
undo a deletion made to satisfy a retention policy. Those documents are counted
and named as their own category, and a verify does not call them a disagreement.

What it cannot restore, it says: baselines and their approvals and the audit log
are decisions rather than properties of a scan, and no document contains one. It
prints what survived in the index it found, prominently, and a rebuild into an
empty index reports them as lost rather than presenting a store that merely
looks intact.

--verify changes nothing and compares the two in both directions: entries
naming documents that are gone, documents with no entry, summaries that no
longer match, and — for the bucket-index store — a baseline decision whose
entry in the audit log never landed. It exits non-zero on any of those, and on
any object under the result prefix it could not read or could not understand,
so it can run on a schedule rather than being remembered after an incident. It
does not exit non-zero over evidence a bucket lifecycle rule expired, which is a
recorded outcome rather than a disagreement.

--verify and --dry-run write nothing at all, schema included: against a store
whose schema is behind this build they refuse and say to run "wsaw store
migrate" rather than performing that upgrade on the way past.

--read-concurrency bounds how many objects are read at once (default 8). It
matters because a rebuild reads every stored document, so against object
storage the request count and the bytes are proportional to the whole history;
the run prints both.

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
//
// It opens a SQL store by name rather than through store.Open, because what it
// reports — DocumentMigration — is a fact about a schema, and a schema is what
// a store keeping its index in rows has. A store of another kind has nothing
// here to migrate and is not silently handed a command that would say nothing
// about it.
func storeMigrateApply(ctx context.Context, opts store.Options) error {
	s, err := store.OpenSQL(ctx, opts)
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

// entries writes a count of index entries. It is its own function because
// plural() appends an "s" and would say "3 entrys", and teaching plural() every
// English plural would be a worse trade than one more four-line function.
func entries(n int) string {
	if n == 1 {
		return "1 entry"
	}

	return fmt.Sprintf("%d entries", n)
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

// maintenanceStore is the part of a store the maintenance commands are allowed
// to reach: retention, the dry run of it, and the rebuild of an index from the
// bucket.
//
// It is declared here rather than the whole store.Store being passed in for the
// reason scanner.ResultStore is declared in the scanner — the seam is where the
// authority a caller has is stated, rather than in a rule somebody has to
// remember. A prune, a sweep and a rebuild given store.Store could store a
// result, approve a baseline or withdraw one, and none of them has any business
// doing any of those; five methods is the whole of what `wsaw store prune`,
// `wsaw store sweep` and `wsaw store rebuild-index` do. Closing the store is
// not on it because the store is closed through the closure openForMaintenance
// builds, which holds the concrete handle.
type maintenanceStore interface {
	Prune(ctx context.Context, now time.Time, r store.Retention) (store.PruneStats, error)
	PlanPrune(ctx context.Context, now time.Time, r store.Retention) (store.PruneStats, error)
	Sweep(ctx context.Context, now time.Time, opts store.SweepOptions) (store.SweepStats, error)
	PlanSweep(ctx context.Context, now time.Time, opts store.SweepOptions) (store.SweepStats, error)
	RebuildIndex(ctx context.Context, opts store.RebuildOptions) (store.RebuildStats, error)
}

// maintenance is one opened store plus what the subcommand that opened it needs
// to talk about it.
type maintenance struct {
	store  maintenanceStore
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
//
// readOnly is the exception, and it is `rebuild-index --verify` and
// `--dry-run`. Those two promise to change nothing, and applying a migration on
// the way in is a change — a large, one-way one for a store that predates
// Epic 8. They open through store.OpenForInspection, which refuses a store
// whose schema is behind rather than upgrading it; see that function.
func openForMaintenance(
	ctx context.Context, name string, args []string, extra func(fs *flag.FlagSet),
) (*maintenance, error) {
	return openStoreFor(ctx, name, args, extra, nil)
}

// openStoreFor is openForMaintenance with a say in how the store is opened.
//
// The decision is a closure rather than a parameter because it is a function of
// the parsed flags: whether a rebuild writes is `--verify` and `--dry-run`, and
// neither is known until after the flag set this function owns has parsed them.
// A nil closure means the ordinary migrating open. The shared --dry-run is
// passed in, because it is parsed by the flag set this function owns.
func openStoreFor(
	ctx context.Context, name string, args []string, extra func(fs *flag.FlagSet),
	readOnly func(dryRun bool) bool,
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

	open := store.Open
	if readOnly != nil && readOnly(*dryRun) {
		open = store.OpenForInspection
	}

	s, err := open(ctx, opts)
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

	if stats.IndexKeysFailed > 0 {
		// Named apart from the refused artifacts because it calls for something
		// different: these results are out of every listing and still fetchable
		// by scan ID, so a share link to one still resolves until a sweep
		// collects the key.
		fmt.Printf("reachable: %s could not be removed from the index and are still fetchable by scan ID;\n",
			plural(stats.IndexKeysFailed, "pruned result"))
		fmt.Println("           run \"wsaw store sweep\" to collect them")
	}

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

	if stats.RebuildInProgress > 0 {
		// Printed before anything else about what was kept, because it is why
		// nothing was judged at all: the sweep found a rebuild of the index
		// half done and refused rather than deleting the evidence of the part
		// that is not indexed yet (Story 8.10). Not an integrity problem, so
		// it must not read like one.
		fmt.Printf("rebuild:   %s found, so nothing in the bucket was examined or collected\n",
			plural(stats.RebuildInProgress, "index rebuild marker"))
		fmt.Println("           let the rebuild finish, or delete the object below, and sweep again")

		// Named, because deleting it is the recovery and an operator told to
		// "clear its marker" without the key has been given a puzzle. A marker
		// more than a day old is not in this list at all: the sweep treats it
		// as the leftover of a run that was killed and carries on.
		for _, key := range stats.RebuildMarkers {
			fmt.Printf("             %s\n", key)
		}
	}

	if stats.ResultsWithoutEntry > 0 {
		// Not garbage and not a failure: a result document the index does not
		// point at decodes to the scan it records, so it is something a rebuild
		// can restore. A sweep never collects one, and says how many it saw.
		fmt.Printf("orphaned:  %s in the bucket that the index does not point at were left in place\n",
			plural(stats.ResultsWithoutEntry, "result document"))
	}

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

// cmdStoreRebuildIndex derives the index from the documents in the bucket,
// reports what doing so would change, or verifies the two against each other
// (Story 8.11).
//
// It shares openForMaintenance with the prune and the sweep, so it inherits the
// same configuration, the same logging and the same way in — a rebuild of a SQL
// store's index needs that store's schema to be current, and going through a
// second door would be a second answer to what "open the configured store"
// means.
func cmdStoreRebuildIndex(ctx context.Context, args []string) error {
	var (
		verify      bool
		concurrency int
	)

	m, err := openStoreFor(ctx, "rebuild-index", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&verify, "verify", false,
			"compare the index against the bucket, change nothing, and exit non-zero on any disagreement")
		// Not "--concurrency": that one is already registered by the shared
		// configuration flags and means parallel scans. Two flags with one
		// name is a panic, and one flag meaning two things would be worse.
		fs.IntVar(&concurrency, "read-concurrency", 0,
			"how many stored documents to read at once; 0 takes the default")
	}, func(dryRun bool) bool { return verify || dryRun })
	if err != nil {
		return err
	}

	defer m.close()

	mode, err := rebuildMode(m.dryRun, verify)
	if err != nil {
		return err
	}

	fmt.Printf("store:     %s\n", m.opts.Location())
	fmt.Printf("bucket:    %s\n", m.opts.ArtifactLocation())
	fmt.Printf("mode:      %s\n", mode)

	stats, err := m.store.RebuildIndex(ctx, store.RebuildOptions{
		Mode:        mode,
		Concurrency: concurrency,
		Limit:       m.limit,
	})

	// Printed before the failure is returned, and from the same statistics
	// either way. A rebuild writes as it goes, so what it managed to record
	// before it stopped has happened and belongs in the account of it — and a
	// verify that found drift returns an error whose whole content is the
	// report below.
	reportRebuild(stats, mode, m.limit)

	if errors.Is(err, store.ErrIndexDrift) {
		// The drift is already printed above, item by item. Repeating the
		// sentinel's own text would say it a second time in less detail.
		return errors.New("the index and the bucket do not agree; see the disagreements above")
	}

	return err
}

// rebuildMode turns the two flags into the one mode, and refuses the
// combination that means two things at once.
//
// A dry run of a verify is not a thing: a verify already writes nothing, so
// asking for both is asking for a mode that does not exist, and guessing which
// one was meant would be guessing on a command an operator reaches for when
// they are not sure what is safe.
func rebuildMode(dryRun, verify bool) (store.RebuildMode, error) {
	switch {
	case dryRun && verify:
		return store.RebuildVerify, errors.New(
			"store rebuild-index: --dry-run and --verify cannot be combined; --verify already writes nothing",
		)
	case verify:
		return store.RebuildVerify, nil
	case dryRun:
		return store.RebuildPlan, nil
	default:
		return store.RebuildApply, nil
	}
}

// reportRebuild prints the whole account of one run.
//
// The order is chosen rather than inherited: what a rebuild cannot restore
// comes before what it did, because that is the part an operator is most likely
// to stop reading before reaching, and it is the part AC6 says must not be a
// footnote. Everything after it is the bucket, then the index, then what the
// run cost.
func reportRebuild(stats store.RebuildStats, mode store.RebuildMode, limit int) {
	reportUnrecoverable(stats, mode)

	fmt.Println()
	fmt.Printf("objects:   %s under the result prefix, %d bytes\n",
		plural(stats.ObjectsFound, "object"), stats.ObjectBytes)
	fmt.Printf("decoded:   %s\n", plural(stats.ResultsDecoded, "result"))

	if stats.Damaged > 0 {
		fmt.Printf("damaged:   %s could not be read back as a scan and %s skipped\n",
			plural(stats.Damaged, "object"), oneOf(stats.Damaged, "was", "were"))
	}

	if stats.DocumentsNewer > 0 {
		fmt.Printf("newer:     %s written by a newer wsaw than this one, so no index entry\n",
			plural(stats.DocumentsNewer, "document"))
		fmt.Println("           was derived from them — this build would derive a summary missing")
		fmt.Println("           whatever it does not know about, and write it over the one that is")
		fmt.Println("           there. Upgrade wsaw and run this again.")

		for _, scan := range stats.NewerDocuments {
			fmt.Printf("             %s\n", scan)
		}

		andMore(len(stats.NewerDocuments), stats.DocumentsNewer)
	}

	reportRebuiltIndex(stats, mode)
	reportRebuildEvidence(stats)
	reportRebuildBucket(stats)
	reportDamaged(stats.DamagedKeys, limit)
	reportDrift(stats.DriftList, stats.Drift, limit)

	fmt.Println()
	fmt.Printf("cost:      %s, %d bytes read\n", plural(stats.Requests(), "request"), stats.BytesRead)
	fmt.Printf("           %s, %s, %s, %s\n",
		plural(stats.Reads, "document read"), plural(stats.Checks, "index check"),
		plural(stats.Writes, "index write"), plural(stats.Listings, "listing"))

	reportRebuildOutcome(stats, mode)
}

// reportUnrecoverable is AC6, printed first and never as a footnote.
//
// A rebuild derives the index from the documents, and a document records a
// scan. What is not a property of a scan cannot come back, so this says how
// much of it the index still holds — and where it holds none, says that in as
// many words rather than presenting a store that merely looks intact.
func reportUnrecoverable(stats store.RebuildStats, mode store.RebuildMode) {
	fmt.Println()
	fmt.Println("What a rebuild cannot restore")

	if !stats.Unrecoverable.Counted {
		fmt.Println("  This store could not say how many baselines and audit entries it holds.")

		return
	}

	fmt.Printf("  baselines:     %d\n", stats.Unrecoverable.Baselines)
	fmt.Printf("  audit entries: %d\n", stats.Unrecoverable.AuditEntries)
	fmt.Println()

	if stats.Unrecoverable.Empty() && stats.ObjectsFound == 0 && stats.EntriesFound == 0 {
		// An empty store, rather than a store that lost something. Saying the
		// paragraph below over one would be raising an alarm about a history
		// that was never there.
		fmt.Println("  This store holds nothing at all, so there is nothing here to have lost.")

		return
	}

	if stats.Unrecoverable.Empty() {
		fmt.Println("  This index holds no baseline and no audit entry, and a rebuild cannot")
		fmt.Println("  produce either: an approval is a decision somebody took about a scan, not")
		fmt.Println("  a property of one, and no stored document contains it. If this store had")
		fmt.Println("  baselines before, they are gone, and the results below come back without")
		fmt.Println("  them — every target will report as never having been approved. Restore them")
		fmt.Println("  from a backup of the index, or approve them again. Share links need no")
		fmt.Println("  restoring: they are signed, not stored.")

		return
	}

	fmt.Println("  These are preserved exactly as they are: nothing here reads or rewrites")
	fmt.Println("  them, and nothing here can put back one that is missing. Share links need")
	fmt.Println("  no restoring — they are signed, not stored.")

	if mode == store.RebuildApply {
		fmt.Println("  A baseline whose scan is not in the index yet becomes readable again as")
		fmt.Println("  soon as the run below puts that scan back.")
	}
}

// reportRebuiltIndex is what the run did, or would do, to the index.
func reportRebuiltIndex(stats store.RebuildStats, mode store.RebuildMode) {
	verb := "were"
	if mode != store.RebuildApply {
		verb = "would be"
	}

	fmt.Println()
	fmt.Printf("index:     %s recorded before this run\n", plural(stats.EntriesFound, "scan"))
	fmt.Printf("added:     %s %s added\n", entries(stats.EntriesAdded), verb)

	if stats.EntriesRepaired > 0 {
		fmt.Printf("repaired:  %s %s completed, having been written only in part\n",
			entries(stats.EntriesRepaired), verb)
	}

	if stats.EntriesRefreshed > 0 {
		fmt.Printf("refreshed: %s %s re-derived, their summary no longer matching the document\n",
			entries(stats.EntriesRefreshed), verb)
	}

	fmt.Printf("unchanged: %s already agreed with the document\n", entries(stats.EntriesUnchanged))
	fmt.Printf("removed:   %s — this command removes none\n", entries(stats.EntriesRemoved))

	reportPrunedDocuments(stats, mode)

	if stats.Conflicts > 0 {
		fmt.Printf("conflicts: %s %s a document other than the one the index records for\n",
			plural(stats.Conflicts, "scan ID"), oneOf(stats.Conflicts, "names", "name"))
		fmt.Println("           it, and neither was touched: resolving one means choosing which of")
		fmt.Println("           two scans to discard, which is not a recovery command's decision")
	}

	if stats.IndexUnreadable > 0 {
		fmt.Printf("unreadable: %s %s present and does not decode, at a key that is already\n",
			plural(stats.IndexUnreadable, "index object"),
			oneOf(stats.IndexUnreadable, "is", "are"))
		fmt.Println("           taken, so it was reported and left where it is")
	}

	if stats.SummariesStale > 0 {
		fmt.Printf("stale:     %s %s a summary the document no longer produces, and this store\n",
			entries(stats.SummariesStale), oneOf(stats.SummariesStale, "carries", "carry"))
		fmt.Println("           does not rewrite an index object, so it was left as it is")
	}

	if stats.EntriesStale > 0 {
		fmt.Printf("kept:      %s %s a document the bucket does not hold, and %s left in place\n",
			entries(stats.EntriesStale),
			oneOf(stats.EntriesStale, "names", "name"),
			oneOf(stats.EntriesStale, "was", "were"))
		fmt.Println("           rather than deleted: a lost object and a listing that has not caught")
		fmt.Println("           up look the same from here, and only one of them is a reason to")
		fmt.Println("           delete a record of a scan")
	}
}

// reportPrunedDocuments is what a rebuild found and deliberately did not put
// back: the documents of scans this index records as removed.
//
// Named as its own category rather than folded into "added", because AC5 asks a
// dry run to let an operator see the outcome before choosing it, and "N entries
// would be added" reads identically for a scan whose entry never landed and a
// scan retention deleted. The two need different decisions from a person.
func reportPrunedDocuments(stats store.RebuildStats, mode store.RebuildMode) {
	if stats.DocumentsPruned == 0 {
		return
	}

	verb := "were"
	if mode != store.RebuildApply {
		verb = "would be"
	}

	fmt.Printf("pruned:    %s of %s this index records as removed, and %s not put back\n",
		plural(stats.DocumentsPruned, "document"),
		oneOf(stats.DocumentsPruned, "a scan", "scans"), verb)
	fmt.Println("           Retention deleted those results and kept their evidence because")
	fmt.Println("           something else still names it — a baseline holds a copy of the scan it")
	fmt.Println("           approved. Re-indexing them would undo a deletion made to satisfy a")
	fmt.Println("           retention policy. The documents are still in the bucket.")

	for _, scan := range stats.PrunedScans {
		fmt.Printf("             %s\n", scan)
	}

	andMore(len(stats.PrunedScans), stats.DocumentsPruned)
}

// andMore says how many of a list the report did not print.
//
// The store caps the named items at --limit as it collects them, so the slice
// the report receives is already short and the count beside it is the true one.
// Saying nothing would present the short list as the whole of it, which is the
// quiet kind of wrong this command exists to avoid.
func andMore(listed, total int) {
	if total <= listed {
		return
	}

	fmt.Printf("             … and %d more (pass --limit=0 to list them all)\n", total-listed)
}

// reportRebuildEvidence is AC8: what the rebuilt results name and the bucket
// does not hold.
//
// It is not drift and it does not change the exit code; see the note beside the
// drift kinds in the store.
func reportRebuildEvidence(stats store.RebuildStats) {
	if stats.EvidenceMissing == 0 {
		return
	}

	fmt.Println()
	fmt.Printf("evidence:  %s named by %s %s no longer in the bucket\n",
		plural(stats.EvidenceMissing, "screenshot or stored body"),
		plural(stats.ResultsMissingEvidence, "result"),
		oneOf(stats.EvidenceMissing, "is", "are"))
	fmt.Println("           The references were kept, so those results read as evidence that is no")
	fmt.Println("           longer stored rather than as scans that captured nothing.")

	for _, gone := range stats.MissingEvidence {
		fmt.Printf("             %s — %s\n", gone.Scan, gone.Artifact)
	}

	andMore(len(stats.MissingEvidence), stats.EvidenceMissing)
}

// oneOf picks between the singular and the plural form of a word for a count,
// because a report that says "1 screenshot are missing" or "1 entry name a
// document" reads as a report nobody checked.
func oneOf(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}

	return plural
}

// reportRebuildBucket is AC9: what the whole bucket holds, which a rebuild is
// the cheapest place to learn.
func reportRebuildBucket(stats store.RebuildStats) {
	fmt.Println()

	if stats.OrphanNote != "" {
		fmt.Printf("bucket:    not surveyed — %s\n", stats.OrphanNote)

		return
	}

	fmt.Printf("bucket:    %s wsaw wrote, %d bytes\n", plural(stats.BucketObjects, "artifact"), stats.BucketBytes)
	fmt.Printf("orphans:   %s that no result references, %d bytes\n",
		plural(stats.Unreferenced, "artifact"), stats.UnreferencedBytes)

	if stats.Unreferenced > 0 {
		fmt.Println("           \"wsaw store sweep\" is what collects them")
	}

	if stats.ForeignObjects > 0 {
		fmt.Printf("foreign:   %s in the bucket %s not written by wsaw\n",
			plural(stats.ForeignObjects, "object"), oneOf(stats.ForeignObjects, "was", "were"))
	}
}

// reportDamaged names the objects that could not be read back as a scan (AC7).
func reportDamaged(damaged []store.DamagedObject, limit int) {
	if len(damaged) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("objects that could not be read back as a scan:")

	for i, object := range damaged {
		if trimmed(i, len(damaged), limit) {
			break
		}

		fmt.Printf("  %s\n      %s\n", object.Key, object.Reason)
	}
}

// reportDrift names the disagreements between the index and the bucket (AC10).
func reportDrift(drift []store.IndexDrift, total, limit int) {
	if total == 0 {
		return
	}

	fmt.Println()
	fmt.Printf("drift:     %s between the index and the bucket\n", plural(total, "disagreement"))

	for i, d := range drift {
		if trimmed(i, len(drift), limit) {
			break
		}

		fmt.Printf("  %s\n      %s — %s\n", d.Kind, d.Subject, d.Detail)
	}
}

// reportRebuildOutcome is the closing sentence, which differs per mode because
// what an operator should do next differs per mode.
func reportRebuildOutcome(stats store.RebuildStats, mode store.RebuildMode) {
	fmt.Println()

	switch mode {
	case store.RebuildPlan:
		fmt.Println("Nothing was written. Run \"wsaw store rebuild-index\" without --dry-run to apply it.")
		fmt.Println("A rebuild adds and never removes, so applying it cannot lose a result: an entry")
		fmt.Println("it cannot account for is reported and kept.")

	case store.RebuildVerify:
		if stats.Drift == 0 && stats.Unjudged() == 0 {
			fmt.Println("The index and the bucket agree.")

			return
		}

		if stats.Drift == 0 {
			// Agreement cannot be claimed over objects the run could not read
			// or could not understand, and this is the branch where that is
			// the whole of what went wrong.
			fmt.Println("The index agrees with every document this build could read, and there are")
			fmt.Println("documents it could not. Nothing above is a disagreement; the objects named")
			fmt.Println("are ones no opinion could be formed about.")

			return
		}

		fmt.Println("Run \"wsaw store rebuild-index\" to put back the entries the bucket can produce.")
		fmt.Println("What it cannot put back — a missing document, a decision with no audit entry —")
		fmt.Println("is listed above and needs a person.")

	case store.RebuildApply:
		if !stats.Changed() && stats.Complete() {
			fmt.Println("The index already had everything the bucket can produce; nothing was written.")

			return
		}

		if !stats.Complete() {
			// A completion claim over a partial recovery is what the drift
			// list, which --limit trims, would otherwise be the only
			// contradiction of.
			fmt.Println("The index records every scan this run could derive one from, and some it")
			fmt.Println("could not: see the damaged objects, the conflicts and the documents above.")
			fmt.Println("Run \"wsaw store rebuild-index --verify\" once those are dealt with.")

			return
		}

		fmt.Println("The index now records every scan the bucket could produce one for.")
		fmt.Println("Run \"wsaw store rebuild-index --verify\" to have that checked rather than assumed.")
	}
}
