package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// logLevelDebug is the verbose log level the diagnostic commands select.
const logLevelDebug = "debug"

// cmdDebug scans one URL with verbose output. It exists because authoring a
// consent rule is impractical without a fast way to see what the page does
// and whether the rule fired (Story 6.7).
func cmdDebug(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet(cmdNameDebug, flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	mode := fs.String("consent-mode", "reject", "consent mode to use: none, reject or accept")
	timeout := fs.Duration("timeout", 60*time.Second, "hard timeout for the scan")
	screenshots := fs.Bool("screenshots", false, "capture screenshots before and after the consent interaction")

	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) != 1 && len(cf.urls) == 0 {
		return fmt.Errorf("usage: wsaw debug [flags] <url>")
	}

	url := ""
	if len(rest) == 1 {
		url = rest[0]
	} else {
		url = cf.urls[0]
	}

	consentMode := model.ConsentMode(*mode)
	if !consentMode.Valid() {
		return fmt.Errorf("--consent-mode %q is not valid; use none, reject or accept", *mode)
	}

	// Debug always runs verbose, since that is the entire point of it.
	cfg := config.New()
	cfg.Logging.Level = logLevelDebug
	// auto, so debugging by hand is readable but piping it into a file still
	// yields something a machine can read.
	cfg.Logging.Format = "auto"
	cfg.Targets = []config.Target{{
		Name:         adHocName(url, 0),
		URL:          url,
		ConsentModes: []model.ConsentMode{consentMode},
	}}

	cf.applyOverrides(cfg)

	if *screenshots {
		yes := true
		cfg.Defaults.Screenshots = &yes
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	a, err := app.New(ctx, cfg, app.Options{Version: version, RequireBrowser: true})
	if err != nil {
		return err
	}

	defer func() {
		if err := a.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "wsaw: cleanup: %v\n", err)
		}
	}()

	target := a.Targets[0]
	target.HardTimeout = *timeout

	fmt.Fprintf(os.Stderr, "scanning %s in %s mode with %s\n", url, consentMode, a.Chrome)

	out, scanErr := a.Scanner.Scan(scanner.WithSource(ctx, scanner.SourceCLI), target, consentMode)
	if out.Result == nil {
		return scanErr
	}

	if err := report.WriteMarkdown(os.Stdout, out.Result, out.Diff); err != nil {
		return err
	}

	// Debug reports a scan error on stderr but still exits zero: the report
	// above is the useful output, and a non-zero exit here would look like a
	// tooling failure rather than a finding.
	if scanErr != nil {
		fmt.Fprintf(os.Stderr, "\nthe scan reported: %v\n", scanErr)
	}

	return nil
}

// cmdRules inspects and tests consent rules.
func cmdRules(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wsaw rules list|test [flags]")
	}

	switch args[0] {
	case "list":
		return cmdRulesList(args[1:])
	case "test":
		return cmdRulesTest(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; use list or test", args[0])
	}
}

func cmdRulesList(args []string) error {
	fs := flag.NewFlagSet("rules list", flag.ContinueOnError)

	files := fs.String("rules", "", "comma-separated additional rule files")

	if err := fs.Parse(args); err != nil {
		return err
	}

	set, err := consent.LoadBuiltinRules()
	if err != nil {
		return err
	}

	if *files != "" {
		user, err := consent.LoadRuleFiles(strings.Split(*files, ",")...)
		if err != nil {
			return err
		}

		set = consent.Merge(set, user)
	}

	fmt.Printf("%-24s %-18s %-8s %-9s %s\n", "RULE", "VENDOR", "PRIORITY", "HEURISTIC", "MODES")

	for _, r := range set.Rules() {
		modes := make([]string, 0, 2)

		if len(r.Accept) > 0 {
			modes = append(modes, "accept")
		}

		if len(r.Reject) > 0 {
			modes = append(modes, "reject")
		}

		fmt.Printf("%-24s %-18s %-8d %-9t %s\n",
			r.Name, r.Vendor, r.Priority, r.Heuristic, strings.Join(modes, ","))
	}

	fmt.Printf("\n%d rule(s). Rules are data: edit a rule pack rather than rebuilding wsaw.\n", set.Len())

	return nil
}

// cmdRulesTest runs a scan against a live URL and reports which rule matched
// and what the outcome was, which is the feedback loop rule authoring needs.
func cmdRulesTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rules test", flag.ContinueOnError)

	files := fs.String("rules", "", "comma-separated additional rule files")
	mode := fs.String("consent-mode", "reject", "consent mode to test: reject or accept")
	chromePath := fs.String("chrome-path", "", "path to the Chrome or Chromium binary")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 {
		return fmt.Errorf("usage: wsaw rules test [flags] <url>")
	}

	url := fs.Arg(0)

	consentMode := model.ConsentMode(*mode)
	if !consentMode.Valid() || consentMode == model.ConsentNone {
		return fmt.Errorf("--consent-mode must be reject or accept")
	}

	cfg := config.New()
	cfg.Logging.Level = logLevelDebug
	cfg.Logging.Format = "auto"
	cfg.Browser.Path = *chromePath
	cfg.Targets = []config.Target{{
		Name:         adHocName(url, 0),
		URL:          url,
		ConsentModes: []model.ConsentMode{consentMode},
	}}

	if *files != "" {
		cfg.Consent.RuleFiles = strings.Split(*files, ",")
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	a, err := app.New(ctx, cfg, app.Options{Version: version, RequireBrowser: true})
	if err != nil {
		return err
	}

	defer func() { _ = a.Close() }()

	out, scanErr := a.Scanner.Scan(scanner.WithSource(ctx, scanner.SourceCLI), a.Targets[0], consentMode)
	if out.Result == nil {
		return scanErr
	}

	c := out.Result.Consent

	fmt.Printf("URL:        %s\n", url)
	fmt.Printf("Mode:       %s\n", consentMode)
	fmt.Printf("Outcome:    %s\n", c.Outcome)

	if c.Reason != "" {
		fmt.Printf("Reason:     %s\n", c.Reason)
	}

	fmt.Printf("CMP:        %s\n", orDash(c.CMP))
	fmt.Printf("Detection:  %s\n", orDash(c.Detection))
	fmt.Printf("Mechanism:  %s\n", orDash(c.Mechanism))

	if c.Heuristic {
		fmt.Println("            (heuristic label matching — a vendor rule would be more reliable)")
	}

	if c.TCString != "" {
		fmt.Printf("TC string:  %s\n", c.TCString)
	}

	fmt.Printf("\nThird parties before the interaction: %v\n", out.Result.ThirdPartyDomains(model.PhasePre))
	fmt.Printf("Third parties after the interaction:  %v\n", out.Result.ThirdPartyDomains(model.PhasePost))

	if scanErr != nil {
		fmt.Fprintf(os.Stderr, "\nthe scan reported: %v\n", scanErr)
	}

	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}

	return s
}
