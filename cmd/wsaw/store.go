package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/pflege-de-labs/wsaw/internal/app"
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

"migrate" applies whatever this build's schema needs, including the upgrade
that moves every stored result document out of the database and into the
artifact bucket. It is what starting wsaw would do anyway, run deliberately
and with a report of what it did.

--dry-run reports what the upgrade would move — how many documents, how many
bytes, and to where — without moving a document or touching a row. The move is
one-way: an older wsaw cannot read a migrated store, so take a database backup
first.
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
