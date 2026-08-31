// Package app wires wsaw's components together from configuration.
//
// Wiring lives here rather than in main so that it is testable and so the CLI
// stays a thin layer of flag parsing over it.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/martint17r/wsaw/internal/browser"
	"github.com/martint17r/wsaw/internal/config"
	"github.com/martint17r/wsaw/internal/consent"
	"github.com/martint17r/wsaw/internal/logging"
	"github.com/martint17r/wsaw/internal/metrics"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/normalize"
	"github.com/martint17r/wsaw/internal/robots"
	"github.com/martint17r/wsaw/internal/scanner"
	"github.com/martint17r/wsaw/internal/secret"
	"github.com/martint17r/wsaw/internal/store"
)

// App holds everything a running wsaw needs.
type App struct {
	Config  *config.Config
	Targets []config.Resolved

	Logger  *slog.Logger
	Metrics *metrics.Registry
	Secrets *secret.Registry

	Store   *store.Store
	Pool    *browser.Pool
	Scanner *scanner.Scanner
	Rules   *consent.RuleSet

	Chrome browser.Info

	Version string

	closers []func() error
}

// Options are the inputs that come from flags rather than the config file.
type Options struct {
	Version string

	// LogOutput receives log lines; defaults to stderr.
	LogOutput io.Writer

	// RequireBrowser controls whether a missing Chrome is fatal. Commands
	// that only read stored results do not need one.
	RequireBrowser bool
}

// New builds an App from configuration. Everything that can fail — Chrome
// discovery, secret resolution, store access, rule parsing — fails here, at
// startup, rather than during the first scan (NFR §7).
func New(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	if opts.LogOutput == nil {
		opts.LogOutput = os.Stderr
	}

	a := &App{Config: cfg, Version: opts.Version, Secrets: &secret.Registry{}}

	logger, err := logging.New(logging.Options{
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		Output:  opts.LogOutput,
		Secrets: a.Secrets,
	})
	if err != nil {
		return nil, err
	}

	a.Logger = logger
	a.Metrics = metrics.New(opts.Version)

	targets, err := cfg.ResolveTargets(a.Secrets)
	if err != nil {
		return nil, fmt.Errorf("resolving targets: %w", err)
	}

	a.Targets = targets

	if err := a.openStore(); err != nil {
		a.Close()

		return nil, err
	}

	rules, err := a.loadRules()
	if err != nil {
		a.Close()

		return nil, err
	}

	a.Rules = rules

	if opts.RequireBrowser {
		if err := a.startBrowser(ctx); err != nil {
			a.Close()

			return nil, err
		}
	}

	if err := a.buildScanner(); err != nil {
		a.Close()

		return nil, err
	}

	a.Metrics.SetReady(a.Pool != nil, true)

	return a, nil
}

func (a *App) openStore() error {
	path := a.Config.Store.Path
	if path == "" {
		dir, err := defaultStateDir()
		if err != nil {
			return err
		}

		path = filepath.Join(dir, "wsaw.db")
	}

	artifacts := a.Config.Store.ArtifactDir
	if artifacts == "" {
		artifacts = filepath.Join(filepath.Dir(path), "artifacts")
	}

	st, err := store.Open(store.Options{Path: path, ArtifactDir: artifacts})
	if err != nil {
		return err
	}

	a.Store = st
	a.closers = append(a.closers, st.Close)

	a.Logger.Info("store opened", "path", path, "artifacts", artifacts)

	return nil
}

func (a *App) loadRules() (*consent.RuleSet, error) {
	builtin, err := consent.LoadBuiltinRules()
	if err != nil {
		return nil, err
	}

	if len(a.Config.Consent.RuleFiles) == 0 {
		return builtin, nil
	}

	user, err := consent.LoadRuleFiles(a.Config.Consent.RuleFiles...)
	if err != nil {
		return nil, err
	}

	a.Logger.Info("consent rules loaded", "builtin", builtin.Len(), "user", user.Len())

	return consent.Merge(builtin, user), nil
}

func (a *App) startBrowser(ctx context.Context) error {
	info, err := browser.Discover(ctx, a.Config.Browser.Path)
	if err != nil {
		return err
	}

	a.Chrome = info

	if a.Config.Browser.NoSandbox {
		// Opt-in only, and loud about it: the sandbox is the boundary between
		// a hostile page and the host.
		a.Logger.Warn("the Chrome sandbox is disabled; this weakens isolation between scanned pages and this host")
	}

	a.Logger.Info("browser discovered", "path", info.Path, "version", info.Version)

	a.Pool = browser.NewPool(browser.PoolOptions{
		Size:               a.Config.PoolSize(),
		MaxScansPerBrowser: a.Config.Browser.MaxScansPerBrowser,
		OnRestart:          func(reason string) { a.Metrics.BrowserRestarted("", reason) },
		Launch: browser.Options{
			Info:          info,
			RemoteURL:     a.Config.Browser.RemoteURL,
			NoSandbox:     a.Config.Browser.NoSandbox,
			ExtraArgs:     a.Config.Browser.ExtraArgs,
			LaunchTimeout: a.Config.Browser.LaunchTimeout.Or(30 * time.Second),
			Logger:        a.Logger,
		},
	})

	a.closers = append(a.closers, a.Pool.Close)

	return nil
}

func (a *App) buildScanner() error {
	rules, err := a.Config.NormalizeRules()
	if err != nil {
		return err
	}

	normalizer, err := normalize.New(rules)
	if err != nil {
		return fmt.Errorf("compiling normalization rules: %w", err)
	}

	if a.Pool == nil {
		// Read-only commands have no scanner, which is legitimate.
		return nil
	}

	checker := robots.NewChecker(robots.Options{
		FallbackAllow: true,
		Client:        &http.Client{Timeout: 10 * time.Second},
	})

	failurePolicy := consent.FailFlag
	if a.Config.Consent.OnFailure == "fail" {
		failurePolicy = consent.FailScan
	}

	allowHeuristic := true
	if a.Config.Consent.AllowHeuristic != nil {
		allowHeuristic = *a.Config.Consent.AllowHeuristic
	}

	s, err := scanner.New(scanner.Deps{
		Pool:    a.Pool,
		Store:   a.Store,
		Robots:  checker,
		Rules:   a.Rules,
		Secrets: a.Secrets,
		Logger:  a.Logger,
		Metrics: scanner.Metrics{
			ScanStarted:    a.Metrics.ScanStarted,
			ScanFinished:   a.Metrics.ScanFinished,
			ScanFailed:     a.Metrics.ScanFailed,
			BrowserRestart: a.Metrics.BrowserRestarted,
		},
	}, scanner.Options{
		Normalizer:            normalizer,
		Baseline:              a.Config.Detection.Baseline,
		HashResourceTypes:     a.Config.Detection.HashResourceTypes,
		ConsentStepTimeout:    a.Config.Consent.StepTimeout.Or(consent.DefaultStepTimeout),
		ConsentTotalTimeout:   a.Config.Consent.TotalTimeout.Or(consent.DefaultTotalTimeout),
		AllowHeuristicConsent: allowHeuristic,
		ConsentOnFailure:      failurePolicy,
		WsawVersion:           a.Version,
		ChromeVersion:         a.Chrome.Version,
	})
	if err != nil {
		return err
	}

	a.Scanner = s

	return nil
}

// Normalizer rebuilds the normalizer, for commands that need one without a
// scanner.
func (a *App) Normalizer() (*normalize.Normalizer, error) {
	rules, err := a.Config.NormalizeRules()
	if err != nil {
		return nil, err
	}

	return normalize.New(rules)
}

// TargetByName finds a resolved target.
func (a *App) TargetByName(name string) (config.Resolved, bool) {
	for _, t := range a.Targets {
		if t.Name == name {
			return t, true
		}
	}

	return config.Resolved{}, false
}

// Trigger runs one ad-hoc scan of a configured target, satisfying the API's
// ScanTrigger interface.
func (a *App) Trigger(ctx context.Context, target string, mode model.ConsentMode) (scanner.Outcome, error) {
	t, ok := a.TargetByName(target)
	if !ok {
		return scanner.Outcome{}, fmt.Errorf("no target named %q is configured", target)
	}

	if a.Scanner == nil {
		return scanner.Outcome{}, errors.New("scanning is not available in this mode")
	}

	return a.Scanner.Scan(ctx, t, mode)
}

// Retention returns the configured retention policy.
func (a *App) Retention() store.Retention {
	return store.Retention{
		MaxAge:       a.Config.Store.MaxAge.Duration(),
		MaxPerSeries: a.Config.Store.MaxPerSeries,
	}
}

// Close releases every resource, in reverse order of acquisition.
func (a *App) Close() error {
	var errs []error

	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}

	a.closers = nil

	return errors.Join(errs...)
}

// defaultStateDir returns the platform-appropriate state directory: XDG on
// Linux, Application Support on macOS (Story 6.2).
func defaultStateDir() (string, error) {
	var base string

	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the home directory: %w", err)
		}

		base = filepath.Join(home, "Library", "Application Support", "wsaw")

	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			base = filepath.Join(dir, "wsaw")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locating the home directory: %w", err)
			}

			base = filepath.Join(home, ".local", "state", "wsaw")
		}
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("creating state directory %s: %w", base, err)
	}

	return base, nil
}

// DefaultConfigPaths lists where wsaw looks for its configuration when none
// is given.
func DefaultConfigPaths() []string {
	var paths []string

	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "wsaw", "wsaw.yaml"))
	}

	paths = append(paths,
		"/etc/wsaw/wsaw.yaml",
		"wsaw.yaml",
	)

	return paths
}

// PruneLoop enforces retention on a schedule. Without it, a daemon that runs
// for months grows without bound (NFR §1); with it, retention is observable
// because every prune is logged.
func (a *App) PruneLoop(ctx context.Context) {
	retention := a.Retention()
	if retention.MaxAge <= 0 && retention.MaxPerSeries <= 0 {
		return
	}

	const interval = time.Hour

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case now := <-ticker.C:
			stats, err := a.Store.Prune(now, retention)
			if err != nil {
				a.Logger.Error("pruning old results failed", "error", err)

				continue
			}

			if stats.ResultsDeleted > 0 {
				a.Logger.Info("pruned old results", "results_deleted", stats.ResultsDeleted)
			}
		}
	}
}
