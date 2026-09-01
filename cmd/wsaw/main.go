// Command wsaw watches what websites load.
//
// It runs either as a daemon on a schedule, or once for CI use, where its
// exit code is the contract: 0 clean, 1 findings, 2 operational failure
// (Story 5.5).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Build information, set by the linker (Story 6.1).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// cmdNameDebug is the one-scan diagnostic command. Named because the same
// word is also a log level, and the two must not be confused.
const cmdNameDebug = "debug"

// Exit codes are a documented interface, so they are named rather than
// scattered as literals.
const (
	exitOK          = 0
	exitFindings    = 1
	exitOperational = 2
)

func main() {
	// Signals are handled here so every command inherits graceful
	// cancellation: SIGTERM must never leave a browser or temp directory
	// behind (Story 3.5).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := run(ctx, os.Args[1:])

	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)

		return exitOperational
	}

	cmd, rest := args[0], args[1:]

	var err error

	switch cmd {
	case "run", "daemon":
		err = cmdRun(ctx, rest)

	case "scan":
		return cmdScan(ctx, rest)

	case cmdNameDebug:
		err = cmdDebug(ctx, rest)

	case "rules":
		err = cmdRules(ctx, rest)

	case "config":
		err = cmdConfig(rest)

	case "version":
		fmt.Printf("wsaw %s (commit %s, built %s)\n", version, commit, date)

		return exitOK

	case "help", "-h", "--help":
		usage(os.Stdout)

		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)

		return exitOperational
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A clean shutdown is not a failure.
			return exitOK
		}

		fmt.Fprintf(os.Stderr, "wsaw: %v\n", err)

		return exitOperational
	}

	return exitOK
}

func usage(w *os.File) {
	// Writing usage is best effort: there is nothing useful to do if the
	// terminal has gone away.
	_, _ = fmt.Fprint(w, `wsaw — website asset watcher

Records every network fetch a page performs, with configurable cookie-banner
handling, and reports what changed.

Usage:
  wsaw <command> [flags]

Commands:
  run            Run as a daemon, scanning on a schedule
  scan           Scan once and exit; the exit code reports findings
  debug          Scan one URL with verbose output, for rule authoring
  rules          Inspect or test consent rules
  config         Validate and print the effective configuration
  version        Print build information

Exit codes for "scan":
  0  no findings at or above the threshold
  1  findings at or above the threshold
  2  operational failure (bad configuration, no usable browser, and so on)

Run "wsaw <command> --help" for the flags of a command.
`)
}
