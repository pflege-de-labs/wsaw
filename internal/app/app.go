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
	"strings"
	"time"

	"github.com/martint17r/wsaw/internal/browser"
	"github.com/martint17r/wsaw/internal/config"
	"github.com/martint17r/wsaw/internal/consent"
	"github.com/martint17r/wsaw/internal/container"
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

	// Runtime is the container runtime the browser runs in, nil when it runs
	// as a host process.
	Runtime *container.Runtime
	// BrowserImage is the container image in use, empty when local.
	BrowserImage string

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

	logger, logFormat, err := logging.New(logging.Options{
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		Output:  opts.LogOutput,
		Secrets: a.Secrets,
	})
	if err != nil {
		return nil, err
	}

	a.Logger = logger

	// Which format was chosen is stated rather than left to be inferred: with
	// auto-detection an operator otherwise has to guess why output looks the
	// way it does (Story 6.9, AC8).
	logger.Info("logging configured",
		"format", string(logFormat),
		"requested", orAuto(cfg.Logging.Format),
		"level", orInfo(cfg.Logging.Level),
	)
	a.Metrics = metrics.New(opts.Version)

	targets, err := cfg.ResolveTargets(a.Secrets)
	if err != nil {
		return nil, fmt.Errorf("resolving targets: %w", err)
	}

	a.Targets = targets

	if err := a.openStore(); err != nil {
		a.closeAfterFailedStart()

		return nil, err
	}

	rules, err := a.loadRules()
	if err != nil {
		a.closeAfterFailedStart()

		return nil, err
	}

	a.Rules = rules

	if opts.RequireBrowser {
		if err := a.startBrowser(ctx); err != nil {
			a.closeAfterFailedStart()

			return nil, err
		}
	}

	if err := a.buildScanner(); err != nil {
		a.closeAfterFailedStart()

		return nil, err
	}

	a.Metrics.SetReady(a.Pool != nil, true)

	return a, nil
}

func (a *App) openStore() error {
	if a.Config.Store.IsServerStore() {
		return a.openServerStore()
	}

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

	st, err := store.Open(store.Options{
		Path:         path,
		ArtifactDir:  artifacts,
		MaxAttempts:  a.Config.Store.MaxAttempts,
		RetryBackoff: a.Config.Store.RetryBackoff.Duration(),
		OnRetry:      a.logStoreRetry,
	})
	if err != nil {
		return err
	}

	a.adoptStore(st)

	a.Logger.Info("store opened", "driver", st.Driver(), "path", path, "artifacts", artifacts)

	return nil
}

// openServerStore opens a store on PostgreSQL or MySQL (Story 4.7).
//
// Artifacts still live on disk. Screenshots and stored bodies do not belong
// in a row, and keeping them out is what leaves the door open to object
// storage later — so a server-backed deployment still needs somewhere to put
// them, and says where.
func (a *App) openServerStore() error {
	dsn, err := secret.Resolve(a.Config.Store.DSN)
	if err != nil {
		return fmt.Errorf("store.dsn: %w", err)
	}

	// Registered before it is used, so a DSN cannot reach a log line or an
	// error message: it carries a password (Story 4.7, AC6).
	a.Secrets.Add(dsn)

	artifacts := a.Config.Store.ArtifactDir
	if artifacts == "" {
		dir, err := defaultStateDir()
		if err != nil {
			return err
		}

		artifacts = filepath.Join(dir, "artifacts")
	}

	st, err := store.Open(store.Options{
		Driver:          a.Config.Store.StoreDriver(),
		DSN:             dsn,
		ArtifactDir:     artifacts,
		MaxOpenConns:    a.Config.Store.MaxOpenConns,
		MaxIdleConns:    a.Config.Store.MaxIdleConns,
		ConnMaxLifetime: a.Config.Store.ConnMaxLifetime.Duration(),
		MaxAttempts:     a.Config.Store.MaxAttempts,
		RetryBackoff:    a.Config.Store.RetryBackoff.Duration(),
		OnRetry:         a.logStoreRetry,
	})
	if err != nil {
		return err
	}

	a.adoptStore(st)

	a.Logger.Info("store opened",
		"driver", st.Driver(),
		// The DSN's credentials are stripped: the endpoint is useful in a log,
		// the password never is.
		"dsn", secret.RedactURL(dsn.Reveal()),
		"artifacts", artifacts,
	)

	return nil
}

// logStoreRetry makes a retry visible. A database that is flapping while
// every scan quietly succeeds on the second attempt is exactly the kind of
// degradation an operator should be told about before it becomes an outage
// (Tenet 8).
func (a *App) logStoreRetry(op string, attempt int, err error) {
	a.Logger.Warn("store operation failed, retrying",
		"operation", op, "attempt", attempt, "error", err)

	a.Metrics.StoreRetried()
}

func (a *App) adoptStore(st *store.Store) {
	a.Store = st
	a.closers = append(a.closers, st.Close)
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
	launch := browser.Options{
		RemoteURL:     a.Config.Browser.RemoteURL,
		NoSandbox:     a.Config.Browser.NoSandbox,
		ProfileDir:    a.Config.Browser.ProfileDir,
		ExtraArgs:     a.Config.Browser.ExtraArgs,
		LaunchTimeout: a.Config.Browser.LaunchTimeout.Or(30 * time.Second),
		Logger:        a.Logger,
	}

	if err := a.resolveBrowser(ctx, &launch); err != nil {
		return err
	}

	a.Pool = browser.NewPool(browser.PoolOptions{
		Size:               a.Config.PoolSize(),
		MaxScansPerBrowser: a.Config.Browser.MaxScansPerBrowser,
		OnRestart:          func(reason string) { a.Metrics.BrowserRestarted("", reason) },
		Launch:             launch,
	})

	a.closers = append(a.closers, a.Pool.Close)

	return nil
}

// resolveBrowser decides where the browser runs and fills in the launch
// options accordingly.
//
// A container is preferred where a runtime is available, because it puts a
// boundary the operating system enforces between a hostile page and this
// host. Where none is, the local browser is used exactly as before — an
// absent runtime is not a reason to refuse to work (Story 1.8, AC1).
func (a *App) resolveBrowser(ctx context.Context, launch *browser.Options) error {
	// An explicitly configured remote endpoint wins: the operator has already
	// said where the browser is.
	if a.Config.Browser.RemoteURL != "" {
		a.Logger.Info("using a remote browser", "url", a.Config.Browser.RemoteURL)

		return nil
	}

	kind := container.Kind(strings.ToLower(strings.TrimSpace(a.Config.Browser.Runtime)))

	runtime, err := container.Detect(ctx, kind)
	if err != nil {
		return err
	}

	if runtime == nil {
		return a.resolveLocalBrowser(ctx, launch, kind)
	}

	image := a.Config.Browser.Container.Image
	if image == "" {
		image = container.DefaultImage
	}

	// Doubles as the check that the image is present, so a missing image
	// fails at startup with the command to fetch it rather than on the first
	// scan (AC6).
	version, err := runtime.BrowserVersion(ctx, image)
	if err != nil {
		return err
	}

	// Orphans from an earlier run that did not shut down cleanly. Only
	// containers whose owning process is gone are touched (AC4).
	if removed, err := runtime.Reap(ctx, a.Logger); err != nil {
		a.Logger.Warn("could not check for orphaned browser containers", "error", err)
	} else if removed > 0 {
		a.Logger.Info("cleaned up orphaned browser containers", "removed", removed)
	}

	a.Runtime = runtime
	a.BrowserImage = image
	a.Chrome = browser.Info{Version: version, Path: image}

	launch.Container = &containerLauncher{
		runtime: runtime,
		spec: container.Spec{
			Image:          image,
			Memory:         a.Config.Browser.Container.Memory,
			PidsLimit:      a.Config.Browser.Container.PidsLimit,
			ExtraArgs:      a.Config.Browser.Container.ExtraArgs,
			BrowserArgs:    a.Config.Browser.Container.BrowserArgs,
			StartupTimeout: a.Config.Browser.Container.StartupTimeout.Or(90 * time.Second),
			Logger:         a.Logger,
		},
	}

	a.Logger.Info("browser runs in a container",
		"runtime", string(runtime.Kind),
		"runtime_version", runtime.Version,
		"image", image,
		"browser", version,
	)

	// Stated rather than left implicit: the shipped image disables Chrome's
	// own sandbox, because nesting it inside a container needs privileges
	// that would weaken the container boundary itself. The container is the
	// boundary here, and a reader of the result can see which it was.
	a.Logger.Info("the container is the isolation boundary; Chrome's in-container sandbox is disabled by the image")

	return nil
}

func (a *App) resolveLocalBrowser(ctx context.Context, launch *browser.Options, kind container.Kind) error {
	info, err := browser.Discover(ctx, a.Config.Browser.Path)
	if err != nil {
		return err
	}

	a.Chrome = info
	launch.Info = info

	if a.Config.Browser.NoSandbox {
		// Opt-in only, and loud about it: with no container around it, the
		// sandbox is the only boundary between a hostile page and the host.
		a.Logger.Warn("the Chrome sandbox is disabled; this weakens isolation between scanned pages and this host")
	}

	switch kind {
	case container.KindLocal:
		a.Logger.Info("browser runs on this host", "path", info.Path, "version", info.Version)
	default:
		a.Logger.Info("browser runs on this host; no container runtime was found",
			"path", info.Path, "version", info.Version,
			"hint", "install podman or docker to render pages behind an operating-system boundary")
	}

	return nil
}

// containerLauncher adapts a container runtime to the browser package's seam,
// which keeps that package free of any container dependency.
type containerLauncher struct {
	runtime *container.Runtime
	spec    container.Spec
}

func (l *containerLauncher) Start(ctx context.Context) (browser.ContainerInstance, error) {
	return l.runtime.Start(ctx, l.spec)
}

// BrowserRuntimeName reports what renders pages, for recording in results.
func (a *App) BrowserRuntimeName() string {
	if a.Runtime == nil {
		return string(container.KindLocal)
	}

	return string(a.Runtime.Kind)
}

// BrowserSandboxed reports whether Chrome's own sandbox is active.
//
// In a container it is not: the shipped image disables it, because nesting a
// namespace sandbox needs privileges that would weaken the container boundary
// that replaced it.
func (a *App) BrowserSandboxed() bool {
	if a.Runtime != nil {
		return false
	}

	return !a.Config.Browser.NoSandbox
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
		BrowserRuntime:        a.BrowserRuntimeName(),
		BrowserImage:          a.BrowserImage,
		BrowserSandbox:        a.BrowserSandboxed(),
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

	// Labelled so the interface can distinguish a scan somebody asked for from
	// one the scheduler started on its own (Story 5.12).
	return a.Scanner.Scan(scanner.WithSource(ctx, scanner.SourceAPI), t, mode)
}

// RunningScans lists the scans in flight, for the API and the web interface.
// It is empty rather than an error when no scanner exists, because a read-only
// command legitimately has none.
func (a *App) RunningScans() []scanner.Running {
	if a.Scanner == nil {
		return nil
	}

	return a.Scanner.Running()
}

// Retention returns the configured retention policy.
func (a *App) Retention() store.Retention {
	return store.Retention{
		MaxAge:       a.Config.Store.MaxAge.Duration(),
		MaxPerSeries: a.Config.Store.MaxPerSeries,
	}
}

// closeAfterFailedStart unwinds a partial startup. The original error is what
// the caller needs, so a cleanup failure is logged rather than returned — but
// it is not discarded, because a resource that would not close is worth
// knowing about.
func (a *App) closeAfterFailedStart() {
	if err := a.Close(); err != nil {
		a.Logger.Warn("cleaning up after a failed start", "error", err)
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

func orAuto(s string) string {
	if s == "" {
		return "auto"
	}

	return s
}

func orInfo(s string) string {
	if s == "" {
		return "info"
	}

	return s
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

	// #nosec G703 -- base is assembled from the process's own environment and
	// home directory, never from a scanned page or an API request.
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
