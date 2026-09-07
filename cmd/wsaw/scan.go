package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// cmdScan runs every target once and exits with the CI contract: 0 clean,
// 1 findings at or above the threshold, 2 operational failure (Story 5.5).
//
// It returns an exit code rather than an error because the distinction
// between "found something" and "could not look" is the whole point.
func cmdScan(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	format := fs.String("format", "markdown", "output format: json, jsonl, markdown or csv")
	failOn := fs.String("fail-on", "high", "exit 1 when a finding reaches this severity: info, low, medium, high, critical")
	target := fs.String("target", "", "scan only this target")

	if err := fs.Parse(args); err != nil {
		return exitOperational
	}

	threshold, err := diff.ParseSeverity(*failOn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsaw: --fail-on: %v\n", err)

		return exitOperational
	}

	cfg, err := cf.load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsaw: %v\n", err)

		return exitOperational
	}

	a, err := app.New(ctx, cfg, app.Options{Version: version, RequireBrowser: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsaw: %v\n", err)

		return exitOperational
	}

	defer func() {
		if err := a.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "wsaw: cleanup: %v\n", err)
		}
	}()

	targets := a.Targets

	if *target != "" {
		t, ok := a.TargetByName(*target)
		if !ok {
			fmt.Fprintf(os.Stderr, "wsaw: no target named %q is configured\n", *target)

			return exitOperational
		}

		targets = []config.Resolved{t}
	}

	outcomes, failures := runOnce(ctx, a, targets)

	if err := emit(a, outcomes, *format); err != nil {
		fmt.Fprintf(os.Stderr, "wsaw: %v\n", err)

		return exitOperational
	}

	// An operational failure outranks findings: reporting "clean" when a scan
	// could not run would be the worst possible outcome for a CI gate.
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "wsaw: %d scan(s) did not produce a trustworthy result\n", failures)

		return exitOperational
	}

	for _, out := range outcomes {
		if out.Diff != nil && out.Diff.HasFindingsAtLeast(threshold) {
			return exitFindings
		}
	}

	return exitOK
}

// runOnce scans every target and mode once, bounded by the configured
// concurrency.
func runOnce(ctx context.Context, a *app.App, targets []config.Resolved) ([]scanner.Outcome, int) {
	type job struct {
		target config.Resolved
		mode   model.ConsentMode
	}

	var jobs []job

	for _, t := range targets {
		for _, mode := range t.ConsentModes {
			jobs = append(jobs, job{target: t, mode: mode})
		}
	}

	var (
		mu       sync.Mutex
		outcomes []scanner.Outcome
		failures int
		wg       sync.WaitGroup
	)

	sem := make(chan struct{}, a.Config.Concurrency())

	scanCtx := scanner.WithSource(ctx, scanner.SourceCLI)

	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)

		go func(j job) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			out, err := scanWithRetries(scanCtx, a, j.target, j.mode)

			mu.Lock()
			defer mu.Unlock()

			if out.Result != nil {
				outcomes = append(outcomes, out)

				if !out.Result.OK() {
					failures++
				}
			} else {
				failures++
			}

			if err != nil {
				a.Logger.Warn("scan reported an error",
					"target", j.target.Name, "consent_mode", string(j.mode), "error", err)
			}
		}(j)
	}

	wg.Wait()

	// Deterministic output regardless of completion order.
	sortOutcomes(outcomes)

	return outcomes, failures
}

// scanWithRetries runs one scan, trying again when it produced no usable
// observation (Story 3.8, AC11).
//
// One-shot mode retries for the same reason the daemon does, and it matters
// more here: a CI gate that failed because of one dropped connection teaches
// people to re-run it until it passes, which is the opposite of a gate. The
// exit code reports the final outcome, so the contract in Story 5.5 is
// unchanged.
//
// Unlike the scheduler this waits rather than requeueing. There is no
// schedule to protect — the command exists to finish this list and exit — and
// configuration already refuses a retry schedule that could outlast a
// target's interval.
func scanWithRetries(
	ctx context.Context,
	a *app.App,
	target config.Resolved,
	mode model.ConsentMode,
) (scanner.Outcome, error) {
	policy := target.Retry

	var (
		out      scanner.Outcome
		err      error
		previous string
	)

	for attempt := 1; attempt <= policy.MaxAttempts(); attempt++ {
		if attempt > 1 {
			delay := policy.Delay(attempt, target.Name+"/"+string(mode))

			a.Logger.Info("scan produced no usable observation, retrying",
				"target", target.Name, "consent_mode", string(mode),
				"attempt", attempt-1, "attempts", policy.MaxAttempts(),
				"next_attempt_in", delay.String(), "reason", previous)

			a.Metrics.ScanRetried()

			timer := time.NewTimer(delay)

			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()

				return out, err
			}

			timer.Stop()
		}

		out, err = a.Scanner.Scan(scanner.WithAttempt(ctx, attempt, policy.MaxAttempts(), previous), target, mode)

		if !policy.Retryable(out.Result) {
			return out, err
		}

		previous = retryReason(out, err)

		// A cancelled run stops trying: the caller asked for the command to
		// end, not for it to keep working.
		if ctx.Err() != nil {
			return out, err
		}
	}

	if policy.Enabled() {
		a.Logger.Warn("scan failed on every attempt",
			"target", target.Name, "consent_mode", string(mode), "attempts", policy.MaxAttempts())
		a.Metrics.ScanRetriesExhausted()
	}

	return out, err
}

// retryReason describes why an attempt did not produce an observation.
func retryReason(out scanner.Outcome, scanErr error) string {
	switch {
	case out.Result == nil && scanErr != nil:
		return scanErr.Error()
	case out.Result == nil:
		return "the scan produced no result"
	case out.Result.Error != "":
		return string(out.Result.Termination) + ": " + out.Result.Error
	default:
		return string(out.Result.Termination)
	}
}

func sortOutcomes(outcomes []scanner.Outcome) {
	for i := 1; i < len(outcomes); i++ {
		for j := i; j > 0 && lessOutcome(outcomes[j], outcomes[j-1]); j-- {
			outcomes[j], outcomes[j-1] = outcomes[j-1], outcomes[j]
		}
	}
}

func lessOutcome(a, b scanner.Outcome) bool {
	if a.Result.Target != b.Result.Target {
		return a.Result.Target < b.Result.Target
	}

	return a.Result.ConsentMode < b.Result.ConsentMode
}

// emit writes results in the requested format, to stdout or to the output
// directory.
func emit(a *app.App, outcomes []scanner.Outcome, format string) error {
	dir := a.Config.Store.OutputDir

	if dir == "" {
		return emitStdout(outcomes, format)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating output directory %s: %w", dir, err)
	}

	for _, out := range outcomes {
		base := filepath.Join(dir, sanitize(out.Result.Target)+"-"+string(out.Result.ConsentMode))

		if err := writeAtomic(base+".json", func(f *os.File) error {
			return report.WriteJSON(f, out.Result)
		}); err != nil {
			return err
		}

		if a.Config.Store.WriteReport {
			if err := writeAtomic(base+".md", func(f *os.File) error {
				return report.WriteMarkdown(f, out.Result, out.Diff)
			}); err != nil {
				return err
			}
		}

		if a.Config.Store.WriteHAR {
			if err := writeAtomic(base+".har", func(f *os.File) error {
				return report.WriteHAR(f, out.Result)
			}); err != nil {
				return err
			}
		}

		if a.Config.Store.WriteJSONL {
			if err := writeAtomic(base+".jsonl", func(f *os.File) error {
				return report.WriteJSONL(f, out.Result)
			}); err != nil {
				return err
			}
		}
	}

	return nil
}

func emitStdout(outcomes []scanner.Outcome, format string) error {
	switch format {
	case "json":
		for _, out := range outcomes {
			if err := report.WriteJSON(os.Stdout, out.Result); err != nil {
				return err
			}
		}

	case "jsonl":
		results := make([]*model.Result, 0, len(outcomes))
		for _, out := range outcomes {
			results = append(results, out.Result)
		}

		return report.WriteJSONL(os.Stdout, results...)

	case "csv":
		for _, out := range outcomes {
			if err := report.WriteCSV(os.Stdout, out.Result); err != nil {
				return err
			}
		}

	case "markdown", "md":
		for i, out := range outcomes {
			if i > 0 {
				fmt.Print("\n---\n\n")
			}

			if err := report.WriteMarkdown(os.Stdout, out.Result, out.Diff); err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("unknown format %q; use json, jsonl, markdown or csv", format)
	}

	return nil
}

// writeAtomic writes through a temporary file and renames, so a crash or a
// full disk never leaves a partial result that a reader would trust
// (Story 3.5).
func writeAtomic(path string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wsaw-*")
	if err != nil {
		return fmt.Errorf("creating temporary file for %s: %w", path, err)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("setting permissions on %s: %w", path, err)
	}

	if err := write(tmp); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("finalizing %s: %w", path, err)
	}

	return nil
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}

	if len(out) == 0 {
		return "target"
	}

	return string(out)
}
