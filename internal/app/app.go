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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/logging"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/robots"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// App holds everything a running wsaw needs.
type App struct {
	Config  *config.Config
	Targets []config.Resolved

	Logger  *slog.Logger
	Metrics *metrics.Registry
	Secrets *secret.Registry

	// Store is whichever kind of store configuration asked for; New opens it
	// through store.Open and assigns only a store that opened. Nil therefore
	// means there is no store, which is what the readers that can do without
	// one check for. A nil pointer wrapped in this interface would pass that
	// check and panic on the first call, so nothing may put one here.
	Store   store.Store
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
	logger.Info(
		"logging configured",
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

	if err := a.openStore(ctx); err != nil {
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

// openStore takes the start-up context because opening a store is not always
// quick: a store written before Story 8.2 moves every document it holds into
// the artifact bucket on its first open, and a service manager stopping wsaw
// during that has to be able to interrupt it (Story 8.4, AC3).
//
// It also writes to the bucket before returning. A bucket that cannot be
// written to has to fail the start, naming itself, rather than turning the
// first scan of the night into evidence nobody can store (Story 8.6, AC4).
func (a *App) openStore(ctx context.Context) error {
	opts, err := StoreOptions(a.Config, a.Secrets)
	if err != nil {
		return err
	}

	opts.OnRetry = a.logStoreRetry
	// The store reports its own progress through wsaw's logger, because one
	// thing it does is not instantaneous: upgrading a store written before
	// Story 8.2 moves every stored document into the artifact bucket, which on
	// a large store is minutes of work an operator has to be able to watch
	// (Story 8.4, AC3).
	opts.Logger = a.Logger
	// Provenance for the one object a store writes about itself: the layout
	// marker of a bucket index, which is all an operator staring at a bucket
	// with no database beside it has to say which wsaw laid it out (Story
	// 8.10). The SQL stores ignore it.
	opts.Version = a.Version

	st, err := store.Open(ctx, opts)
	if err != nil {
		return err
	}

	a.adoptStore(st)

	// Adopted before it is probed, so that a store which opens and then fails
	// its probe is still closed on the way out (Story 8.6, AC4).
	if err := st.ProbeArtifactBucket(ctx); err != nil {
		return fmt.Errorf("the artifact bucket is not usable: %w", err)
	}

	a.Logger.Info(
		"store opened",
		"driver", st.Driver(),
		// Both locations are redacted: a DSN carries a password, and a bucket
		// URL can carry credentials in its userinfo. The endpoint is useful in
		// a log; what authenticates to it never is.
		"location", opts.Location(),
		"artifacts", opts.ArtifactLocation(),
	)

	return nil
}

// StoreOptions resolves where the store and its evidence live, from
// configuration alone.
//
// It is exported because opening a store is not the only thing that needs the
// answer: `wsaw store migrate --dry-run` has to report what an upgrade would
// move without applying it (Story 8.4, AC6), which means knowing which
// database and which bucket without opening either as a running wsaw would.
//
// A DSN is registered with the secret registry when one is given, so it cannot
// reach a log line or an error message: it carries a password (Story 4.7,
// AC6). A bucket URL that carries a credential is registered on the same
// argument (Story 8.6, AC3). Passing a nil registry is for a caller that has
// no logger to protect.
//
// Where the evidence goes is store.artifactURL when a bucket is configured and
// store.artifactDir when a local directory is, and with neither set it is
// derived: for SQLite, artifacts sit beside the database file; for a server
// database they sit in the state directory, since there is no file to sit
// beside. That derived default is the whole of what the single-binary
// deployment needs to configure about storage, which is to say nothing
// (Story 8.6, AC1).
// Screenshots, stored bodies and — since Story 8.2 — result documents do not
// belong in a row, so a server-backed deployment still needs somewhere to put
// them and says where.
func StoreOptions(cfg *config.Config, secrets *secret.Registry) (store.Options, error) {
	artifacts, err := artifactLocation(cfg, secrets)
	if err != nil {
		return store.Options{}, err
	}

	opts := store.Options{
		ArtifactDir:  artifacts,
		MaxAttempts:  cfg.Store.MaxAttempts,
		RetryBackoff: cfg.Store.RetryBackoff.Duration(),
	}

	if cfg.Store.IsBucketStore() {
		// Nothing else to resolve. There is no DSN, no file to place and no
		// pool to size, and the artifact location is not defaulted the way it
		// is for the other two: for this store the bucket is the store, so a
		// deployment that did not name one is refused at load rather than
		// started against a guess (Story 8.10, AC1).
		opts.Driver = cfg.Store.StoreDriver()

		return opts, nil
	}

	if cfg.Store.IsServerStore() {
		dsn, err := secret.Resolve(cfg.Store.DSN)
		if err != nil {
			return store.Options{}, fmt.Errorf("store.dsn: %w", err)
		}

		if secrets != nil {
			secrets.Add(dsn)
		}

		opts.Driver = cfg.Store.StoreDriver()
		opts.DSN = dsn
		opts.MaxOpenConns = cfg.Store.MaxOpenConns
		opts.MaxIdleConns = cfg.Store.MaxIdleConns
		opts.ConnMaxLifetime = cfg.Store.ConnMaxLifetime.Duration()

		if opts.ArtifactDir == "" {
			dir, err := defaultStateDir()
			if err != nil {
				return store.Options{}, err
			}

			opts.ArtifactDir = filepath.Join(dir, "artifacts")
		}

		return opts, nil
	}

	opts.Path = cfg.Store.Path
	if opts.Path == "" {
		dir, err := defaultStateDir()
		if err != nil {
			return store.Options{}, err
		}

		opts.Path = filepath.Join(dir, "wsaw.db")
	}

	if opts.ArtifactDir == "" {
		opts.ArtifactDir = filepath.Join(filepath.Dir(opts.Path), "artifacts")
	}

	return opts, nil
}

// artifactLocation resolves where evidence goes, from the two settings that
// can name it, and protects whatever credential the answer carries.
//
// store.artifactDir is a path and is taken literally: a directory is not a
// credential. store.artifactURL may be a secret reference and should be
// wherever it carries one — an S3-compatible endpoint authenticated by query
// string is the ordinary case, and the AWS, Google and Azure chains are the
// way everything else is meant to authenticate (Story 8.6, AC3).
//
// What joins the secret registry depends on how the URL was written. A setting
// written as a secret reference is registered whole: its author declared the
// value secret, and wsaw is in no position to argue about which part of it is.
// A URL written out in full is registered piece by piece — the password in its
// userinfo, and the value of any query parameter whose name says it carries a
// credential — because the rest of it is the bucket's own name, and scrubbing
// that would cost an operator the one detail those log lines exist for.
//
// Registering anything at all is necessary because the URL does not only
// travel through wsaw's own formatting. store.Options.ArtifactLocation drops
// the query string and the password from what wsaw prints, but gocloud quotes
// the whole URL back in the errors its openers return, so an inline
// "s3://bucket?...secret_access_key=..." reaches a wrapped error, the startup
// log and stderr unless the central scrubber has been told the value. That is
// the case the registry's own doc comment names as its reason to exist (Story
// 8.6, AC3).
func artifactLocation(cfg *config.Config, secrets *secret.Registry) (string, error) {
	if cfg.Store.ArtifactURL == "" {
		return cfg.Store.ArtifactDir, nil
	}

	ref, err := secret.Resolve(cfg.Store.ArtifactURL)
	if err != nil {
		return "", fmt.Errorf("store.artifactURL: %w", err)
	}

	if secrets != nil {
		registerArtifactURLSecrets(secrets, cfg.Store.ArtifactURL, ref)
	}

	return ref.Reveal(), nil
}

// registerArtifactURLSecrets tells the scrubber whatever the artifact URL
// carries that must never be printed.
func registerArtifactURLSecrets(secrets *secret.Registry, setting string, resolved secret.Value) {
	if secret.IsReference(setting) {
		secrets.Add(resolved)

		return
	}

	location := resolved.Reveal()

	if !store.IsArtifactURL(location) {
		// A directory on local disk, named through store.artifactURL. A path
		// is not a credential.
		return
	}

	u, err := url.Parse(location)
	if err != nil {
		// Registered whole rather than parsed: wsaw cannot establish which
		// part of a URL it could not read holds a credential, and the cost of
		// being wrong in this direction is a bucket name missing from a log
		// line rather than a leaked key. The URL is refused by validation
		// anyway; this is about what gets printed while refusing it.
		secrets.Add(resolved)

		return
	}

	if password, set := u.User.Password(); set {
		secrets.Add(secret.Literal(password))
	}

	for name, values := range u.Query() {
		if !namesACredential(name) {
			continue
		}

		for _, v := range values {
			secrets.Add(secret.Literal(v))
		}
	}
}

// credentialParamHints are what a URL query parameter is called when it
// carries a credential.
//
// Substrings rather than exact names, because there are four providers and
// each spells it differently — access_key_id and secret_access_key for S3,
// sas_token for Azure, private_key_path and access_id for GCS — and a new
// parameter in a library upgrade must be covered by default rather than after
// somebody notices it in a log. Matching too widely costs a redacted region
// name in one log line; matching too narrowly costs a leaked key.
var credentialParamHints = []string{"secret", "key", "token", "password", "credential", "signature", "sas"}

// namesACredential reports whether a query parameter's name says its value is
// one.
func namesACredential(name string) bool {
	lower := strings.ToLower(name)

	for _, hint := range credentialParamHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}

	return false
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

func (a *App) adoptStore(st store.Store) {
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
			Image:           image,
			Memory:          a.Config.Browser.Container.Memory,
			PidsLimit:       a.Config.Browser.Container.PidsLimit,
			SHMSize:         a.Config.Browser.Container.SHMSize,
			FileDescriptors: a.Config.Browser.Container.FileDescriptors,
			ExtraArgs:       a.Config.Browser.Container.ExtraArgs,
			BrowserArgs:     a.Config.Browser.Container.BrowserArgs,
			StartupTimeout:  a.Config.Browser.Container.StartupTimeout.Or(90 * time.Second),
			Logger:          a.Logger,
		},
	}

	a.Logger.Info(
		"browser runs in a container",
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
		DegradedFailureRatio:  a.Config.Detection.DegradedFailureRatio,
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

// LastScan reports when a target and consent mode were last scanned,
// according to the store.
//
// The scheduler needs it at startup: its own record of the last run is
// in-memory, so without this a restart makes every target look as though it
// had never been scanned and the whole list is due at once (Tenet 17).
//
// The last *attempt* is what matters here, not the last success. A scan that
// failed still asked the site for the page, so it still counts against how
// often wsaw is willing to ask again.
func (a *App) LastScan(target string, mode model.ConsentMode) (time.Time, bool) {
	if a.Store == nil {
		return time.Time{}, false
	}

	summaries, err := a.Store.ListResults(target, mode, 1)
	if err != nil {
		// A store that cannot answer must not make the scheduler think the
		// target is new — that is the behaviour this exists to prevent. It is
		// logged and reported as unknown, which leaves the target due soon
		// rather than silently never.
		a.Logger.Warn("could not read a target's last scan; its schedule will restart",
			"target", target, "consent_mode", string(mode), "error", err)

		return time.Time{}, false
	}

	if len(summaries) == 0 {
		return time.Time{}, false
	}

	return summaries[0].StartedAt, true
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
func (a *App) Retention() store.Retention { return RetentionFor(a.Config) }

// RetentionFor reads a retention policy out of configuration without needing a
// running App.
//
// It exists for the maintenance command that applies retention on request
// (Story 8.5, AC6): that command loads configuration and opens a store, and
// nothing else. Reading the two settings there instead would be a second place
// that decides what `maxAge` and `maxPerSeries` mean, and a dry run whose
// retention differed from the daemon's would be a dry run of the wrong thing.
func RetentionFor(cfg *config.Config) store.Retention {
	return store.Retention{
		MaxAge:       cfg.Store.MaxAge.Duration(),
		MaxPerSeries: cfg.Store.MaxPerSeries,
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

	paths = append(
		paths,
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
			a.pruneOnce(ctx, now, retention)
		}
	}
}

// pruneOnce applies retention once and reports what it reclaimed.
//
// Pruning deletes artifacts as well as rows now (Story 8.5), so one number is
// no longer the whole story: an operator watching a bucket needs the bytes, and
// a bucket that refuses a delete has to be visible rather than quietly leaking
// (AC5). The counts also go to the metrics registry, for the same reason every
// other store degradation does — one nobody can see is one nobody fixes
// (Tenet 8).
func (a *App) pruneOnce(ctx context.Context, now time.Time, retention store.Retention) {
	stats, err := a.Store.Prune(ctx, now, retention)

	// Recorded before the failure is handled, and from the same stats either
	// way. A prune deletes its rows in a committed transaction and collects
	// artifacts afterwards, outside it, so a run that failed half way through
	// the collection has still removed everything it counted. Dropping those
	// counts because the run ended badly would leave an operator watching a
	// bucket reclaim gigabytes with every metric flat — the invisible
	// degradation this function's own reporting exists to prevent (Tenet 8).
	a.Metrics.Pruned(stats.ResultsDeleted, stats.ArtifactsDeleted, stats.BytesFreed)
	a.Metrics.ArtifactDeletionsFailed(stats.ArtifactsFailed)

	if err != nil {
		a.Logger.Error("pruning old results failed", "error", err,
			"results_deleted", stats.ResultsDeleted,
			"artifacts_deleted", stats.ArtifactsDeleted,
			"bytes_freed", stats.BytesFreed)

		return
	}

	if stats.ArtifactsFailed > 0 {
		a.Logger.Warn("some unreferenced artifacts could not be deleted and were left for the next sweep",
			"artifacts_failed", stats.ArtifactsFailed)
	}

	if stats.UnknownReferences > 0 {
		a.Logger.Warn("some stored results do not record which artifacts they reference, so screenshots and stored bodies were left alone",
			"results", stats.UnknownReferences)
	}

	if stats.ResultsDeleted > 0 || stats.ArtifactsDeleted > 0 {
		a.Logger.Info("pruned old results",
			"results_deleted", stats.ResultsDeleted,
			"artifacts_deleted", stats.ArtifactsDeleted,
			"bytes_freed", stats.BytesFreed)
	}
}
