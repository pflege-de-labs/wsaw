package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// cmdArtifacts groups the operations on stored evidence.
func cmdArtifacts(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wsaw artifacts compress [flags]")
	}

	switch args[0] {
	case "compress":
		return cmdArtifactsCompress(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; use compress", args[0])
	}
}

// cmdArtifactsCompress compresses the artifacts already in the bucket (Story 4.9).
//
// New artifacts are compressed as they are written, and nothing rewrites what
// is already stored, because an upgrade that quietly rewrites evidence is an
// upgrade nobody can audit. This command is how an operator asks for that
// rewrite deliberately, for the history they have already accumulated.
func cmdArtifactsCompress(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("artifacts compress", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	dryRun := fs.Bool("dry-run", false, "report what would be compressed, and write nothing")
	verbose := fs.Bool("verbose", false, "print every artifact as it is compressed")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cf.load()
	if err != nil {
		return err
	}

	a, err := app.New(ctx, cfg, app.Options{Version: version})
	if err != nil {
		return err
	}

	defer func() { _ = a.Close() }()

	opts := store.CompactOptions{
		DryRun: *dryRun,
		// Problems are printed as they happen rather than collected: a run
		// over a large bucket takes a while, and an operator watching it
		// should learn about a corrupt artifact then, not at the end.
		OnProblem: func(ref string, err error) {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", ref, err)
		},
	}

	if *verbose {
		opts.OnArtifact = func(ref string, original, stored int64) {
			fmt.Printf("%s  %s -> %s\n", ref, formatBytes(original), formatBytes(stored))
		}
	}

	stats, err := a.Store.CompactArtifacts(ctx, opts)

	// The summary is printed even when the walk was cancelled or failed
	// part-way: the artifacts it compressed are compressed, and an operator
	// who pressed Ctrl-C is owed the state they left behind (Story 4.9, AC9).
	printCompactStats(stats, *dryRun)

	return err
}

func printCompactStats(stats store.CompactStats, dryRun bool) {
	verb := "compressed"
	if dryRun {
		verb = "to compress"
	}

	// Every file the walk saw is accounted for in one of these numbers,
	// because a summary that adds up is the only kind worth printing after
	// an operation that rewrote evidence (Story 4.9, AC8).
	fmt.Printf("%d files examined: %d %s, %d not worth compressing, %d already compressed, %d skipped\n",
		stats.Scanned+stats.AlreadyCompressed, stats.Compressed, verb,
		stats.NotWorthIt, stats.AlreadyCompressed, stats.Problems)

	if stats.Compressed == 0 {
		return
	}

	fmt.Printf("%s -> %s, saving %s (%.0f%%)\n",
		formatBytes(stats.BytesBefore), formatBytes(stats.BytesAfter),
		formatBytes(stats.Saved()),
		100*float64(stats.Saved())/float64(stats.BytesBefore))
}

// formatBytes renders a size for a human reading a terminal, which is the
// only place these numbers go.
func formatBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
