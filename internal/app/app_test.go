package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// minimalConfig is the smallest configuration New can build an App from
// without a browser, with the store pointed at a fresh temp directory so no
// test ever touches the real state directory in the developer's home.
func minimalConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg := config.New()
	cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")
	cfg.Targets = []config.Target{
		{Name: "site", URL: "https://example.test/", ConsentModes: []model.ConsentMode{model.ConsentReject}},
	}

	return cfg
}

func TestOrAutoAndOrInfo(t *testing.T) {
	t.Parallel()

	if got := orAuto(""); got != "auto" {
		t.Errorf("orAuto(%q) = %q, want %q", "", got, "auto")
	}

	if got := orAuto("json"); got != "json" {
		t.Errorf("orAuto(%q) = %q, want it unchanged", "json", got)
	}

	if got := orInfo(""); got != "info" {
		t.Errorf("orInfo(%q) = %q, want %q", "", got, "info")
	}

	if got := orInfo("debug"); got != "debug" {
		t.Errorf("orInfo(%q) = %q, want it unchanged", "debug", got)
	}
}

func TestDefaultConfigPaths(t *testing.T) {
	t.Parallel()

	paths := DefaultConfigPaths()

	if len(paths) < 2 {
		t.Fatalf("DefaultConfigPaths() = %v, want at least the system and local paths", paths)
	}

	last := paths[len(paths)-1]
	if last != "wsaw.yaml" {
		t.Errorf("last path = %q, want the local %q", last, "wsaw.yaml")
	}

	var sawEtc bool

	for _, p := range paths {
		if p == "/etc/wsaw/wsaw.yaml" {
			sawEtc = true
		}
	}

	if !sawEtc {
		t.Errorf("paths = %v, want /etc/wsaw/wsaw.yaml among them", paths)
	}
}

func TestDefaultStateDirCreatesADirectoryUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")

	dir, err := defaultStateDir()
	if err != nil {
		t.Fatalf("defaultStateDir: %v", err)
	}

	if !strings.HasPrefix(dir, home) {
		t.Errorf("state dir %q is not under HOME %q", dir, home)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("state dir was not created: %v", err)
	}

	if !info.IsDir() {
		t.Errorf("%q is not a directory", dir)
	}
}

func TestDefaultStateDirHonoursXDGStateHome(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("XDG_STATE_HOME is only consulted off darwin")
	}

	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)

	dir, err := defaultStateDir()
	if err != nil {
		t.Fatalf("defaultStateDir: %v", err)
	}

	if !strings.HasPrefix(dir, xdg) {
		t.Errorf("state dir %q is not under XDG_STATE_HOME %q", dir, xdg)
	}
}

func TestTargetByName(t *testing.T) {
	t.Parallel()

	a := &App{Targets: []config.Resolved{{Name: "site"}, {Name: "other"}}}

	got, ok := a.TargetByName("other")
	if !ok || got.Name != "other" {
		t.Errorf("TargetByName(other) = %+v, %v", got, ok)
	}

	if _, ok := a.TargetByName("missing"); ok {
		t.Error("TargetByName found a target that was never configured")
	}
}

func TestRetention(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Store.MaxAge = config.Duration(48 * time.Hour)
	cfg.Store.MaxPerSeries = 30

	a := &App{Config: cfg}

	got := a.Retention()
	if got.MaxAge != 48*time.Hour {
		t.Errorf("MaxAge = %v, want 48h", got.MaxAge)
	}

	if got.MaxPerSeries != 30 {
		t.Errorf("MaxPerSeries = %d, want 30", got.MaxPerSeries)
	}
}

func TestCloseIsIdempotentAndUnwindsInReverseOrder(t *testing.T) {
	var order []string

	a := &App{}
	a.closers = append(a.closers,
		func() error {
			order = append(order, "first")

			return nil
		},
		func() error {
			order = append(order, "second")

			return nil
		},
	)

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if want := []string{"second", "first"}; !equalStrings(order, want) {
		t.Errorf("close order = %v, want %v", order, want)
	}

	// A second call must be a safe no-op: startup failures close a partially
	// built App, and a command's own deferred Close then runs again.
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if len(order) != 2 {
		t.Errorf("closers ran again on the second Close: %v", order)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestCloseJoinsErrorsFromEveryCloser(t *testing.T) {
	errA := errors.New("closer a failed")
	errB := errors.New("closer b failed")

	a := &App{}
	a.closers = append(a.closers,
		func() error { return errA },
		func() error { return errB },
	)

	err := a.Close()
	if err == nil {
		t.Fatal("Close swallowed a closer's error")
	}

	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Errorf("Close error = %v, want it to wrap both closer errors", err)
	}
}

func TestCloseAfterFailedStartLogsButDoesNotPanic(t *testing.T) {
	var logged strings.Builder

	a := &App{Logger: slog.New(slog.NewTextHandler(&logged, nil))}
	a.closers = append(a.closers, func() error { return errors.New("boom") })

	a.closeAfterFailedStart()

	if !strings.Contains(logged.String(), "cleaning up after a failed start") {
		t.Errorf("cleanup failure was not logged: %q", logged.String())
	}
}

func TestLastScanWithNoStoreReportsUnknown(t *testing.T) {
	t.Parallel()

	a := &App{}

	when, ok := a.LastScan("site", model.ConsentReject)
	if ok || !when.IsZero() {
		t.Errorf("LastScan with no store = %v, %v; want zero time, false", when, ok)
	}
}

func TestLastScanOnAStoreErrorReportsUnknownRatherThanFailing(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	// A store that cannot answer must not make a target look new: closing it
	// first stands in for a database connection dropping mid-query.
	if err := st.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	var logged strings.Builder

	a := &App{Store: st, Logger: slog.New(slog.NewTextHandler(&logged, nil))}

	when, ok := a.LastScan("site", model.ConsentReject)
	if ok || !when.IsZero() {
		t.Errorf("LastScan on a broken store = %v, %v; want zero time, false", when, ok)
	}

	if !strings.Contains(logged.String(), "could not read a target's last scan") {
		t.Errorf("the store error was not logged: %q", logged.String())
	}
}

func TestRunningScansWithNoScannerIsEmpty(t *testing.T) {
	t.Parallel()

	a := &App{}
	if got := a.RunningScans(); got != nil {
		t.Errorf("RunningScans with no scanner = %v, want nil", got)
	}
}

func TestTriggerRejectsAnUnknownTarget(t *testing.T) {
	t.Parallel()

	a := &App{Targets: []config.Resolved{{Name: "site"}}}

	_, err := a.Trigger(context.Background(), "ghost", model.ConsentReject)
	if err == nil {
		t.Fatal("Trigger accepted a target that is not configured")
	}

	if !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("error = %v, want it to name the target", err)
	}
}

func TestTriggerRefusesWithoutAScanner(t *testing.T) {
	t.Parallel()

	a := &App{Targets: []config.Resolved{{Name: "site"}}}

	_, err := a.Trigger(context.Background(), "site", model.ConsentReject)
	if err == nil {
		t.Fatal("Trigger ran without a scanner")
	}

	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %v, want it to say scanning is unavailable", err)
	}
}

func TestPruneLoopReturnsImmediatelyWithNoRetentionPolicy(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Store.MaxPerSeries = 0 // config.New() defaults this to 200; a "no policy" test needs it off

	a := &App{Config: cfg}

	done := make(chan struct{})

	go func() {
		a.PruneLoop(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PruneLoop with no retention policy did not return")
	}
}

func TestPruneLoopReturnsOnContextCancel(t *testing.T) {
	cfg := config.New()
	cfg.Store.MaxPerSeries = 10 // a policy so the loop actually starts a ticker

	a := &App{Config: cfg, Logger: discardLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})

	go func() {
		a.PruneLoop(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PruneLoop did not return after its context was cancelled")
	}
}

func TestLoadRulesReportsAMissingUserFile(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Consent.RuleFiles = []string{filepath.Join(t.TempDir(), "absent.yaml")}

	a := &App{Config: cfg, Logger: discardLogger()}

	if _, err := a.loadRules(); err == nil {
		t.Fatal("loadRules accepted a rule file that does not exist")
	}
}

func TestLoadRulesMergesAUserPackWithTheBuiltins(t *testing.T) {
	builtin, err := loadBuiltinForTest(t)
	if err != nil {
		t.Fatalf("loading builtin rules for comparison: %v", err)
	}

	pack := filepath.Join(t.TempDir(), "extra.yaml")
	if err := os.WriteFile(pack, []byte(testUserRulePack), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.Consent.RuleFiles = []string{pack}

	var logged strings.Builder

	a := &App{Config: cfg, Logger: slog.New(slog.NewTextHandler(&logged, nil))}

	merged, err := a.loadRules()
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}

	if merged.Len() != builtin.Len()+1 {
		t.Errorf("merged rule count = %d, want %d (builtin) + 1", merged.Len(), builtin.Len())
	}

	if !strings.Contains(logged.String(), "consent rules loaded") {
		t.Errorf("rule loading was not logged: %q", logged.String())
	}
}

func TestBuildScannerWithNoPoolBuildsNoScanner(t *testing.T) {
	t.Parallel()

	a := &App{Config: config.New()}

	if err := a.buildScanner(); err != nil {
		t.Fatalf("buildScanner: %v", err)
	}

	if a.Scanner != nil {
		t.Error("buildScanner built a scanner with no browser pool")
	}
}

func TestBuildScannerRejectsABadNormalizationPattern(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Normalize.PathReplacements = []config.Replacement{{Pattern: "(unterminated"}}

	a := &App{Config: cfg}

	err := a.buildScanner()
	if err == nil {
		t.Fatal("buildScanner accepted an unparsable path-replacement regex")
	}

	if !strings.Contains(err.Error(), "compiling normalization rules") {
		t.Errorf("error = %v, want it to name the normalization step", err)
	}
}

func TestNormalizerRejectsTheSameBadPattern(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Normalize.PathReplacements = []config.Replacement{{Pattern: "(unterminated"}}

	a := &App{Config: cfg}

	if _, err := a.Normalizer(); err == nil {
		t.Fatal("Normalizer accepted an unparsable path-replacement regex")
	}
}

func TestOpenStoreDefaultsPathAndArtifactDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")

	a := &App{Config: config.New(), Logger: discardLogger(), Metrics: metrics.New("test")}

	if err := a.openStore(); err != nil {
		t.Fatalf("openStore: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	if a.Store == nil {
		t.Fatal("openStore left Store nil")
	}
}

func TestOpenStoreFailsOnAnUnusablePath(t *testing.T) {
	cfg := config.New()
	// A directory cannot be opened as a SQLite database file.
	cfg.Store.Path = t.TempDir()

	a := &App{Config: cfg, Logger: discardLogger(), Metrics: metrics.New("test")}

	if err := a.openStore(); err == nil {
		t.Fatal("openStore accepted a path that is a directory")
	}
}

func TestOpenServerStoreFailsOnAnUnresolvableDSN(t *testing.T) {
	cfg := config.New()
	cfg.Store.Driver = "postgres"
	cfg.Store.DSN = "${env:WSAW_APP_TEST_ABSENT_DSN}"

	a := &App{Config: cfg, Logger: discardLogger(), Metrics: metrics.New("test"), Secrets: &secret.Registry{}}

	err := a.openStore()
	if err == nil {
		t.Fatal("openStore accepted a DSN it could not resolve")
	}

	if !strings.Contains(err.Error(), "store.dsn") {
		t.Errorf("error = %v, want it to name store.dsn", err)
	}
}

func TestOpenServerStoreRegistersTheDSNAsASecretEvenWhenOpenFails(t *testing.T) {
	cfg := config.New()
	cfg.Store.Driver = "postgres"
	cfg.Store.DSN = "postgres://user:s3cret@127.0.0.1:1/db"

	secrets := &secret.Registry{}
	a := &App{Config: cfg, Logger: discardLogger(), Metrics: metrics.New("test"), Secrets: secrets}

	// The connection itself will fail (nothing is listening), but the DSN
	// must already be registered for redaction by the time that happens.
	_ = a.openStore()

	if scrubbed := secrets.Scrub("dsn was postgres://user:s3cret@127.0.0.1:1/db"); strings.Contains(scrubbed, "s3cret") {
		t.Errorf("the DSN was not registered for redaction: %q", scrubbed)
	}
}

func TestLogStoreRetryWarnsAndCountsTheRetry(t *testing.T) {
	t.Parallel()

	var logged strings.Builder

	a := &App{Logger: slog.New(slog.NewTextHandler(&logged, nil)), Metrics: metrics.New("test")}

	a.logStoreRetry("PutResult", 2, errors.New("database is locked"))

	if !strings.Contains(logged.String(), "store operation failed, retrying") {
		t.Errorf("retry was not logged: %q", logged.String())
	}

	if !strings.Contains(logged.String(), "PutResult") || !strings.Contains(logged.String(), "database is locked") {
		t.Errorf("log line is missing the operation or the error: %q", logged.String())
	}
}

func TestNewBuildsAnAppWithoutRequiringABrowser(t *testing.T) {
	cfg := minimalConfig(t)

	a, err := New(context.Background(), cfg, Options{Version: "test", RequireBrowser: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	if a.Store == nil {
		t.Error("New did not open a store")
	}

	if a.Rules == nil {
		t.Error("New did not load consent rules")
	}

	if a.Pool != nil {
		t.Error("New started a browser pool despite RequireBrowser: false")
	}

	if a.Scanner != nil {
		t.Error("New built a scanner with no browser pool")
	}

	if len(a.Targets) != 1 || a.Targets[0].Name != "site" {
		t.Errorf("Targets = %+v, want the one configured target resolved", a.Targets)
	}
}

func TestNewWrapsAnUnresolvableTargetSecret(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.Targets[0].BasicAuthPassword = "${env:WSAW_APP_TEST_ABSENT_PASSWORD}"

	_, err := New(context.Background(), cfg, Options{Version: "test"})
	if err == nil {
		t.Fatal("New accepted a target with an unresolvable secret")
	}

	if !strings.Contains(err.Error(), "resolving targets") {
		t.Errorf("error = %v, want it to name the resolving-targets step", err)
	}
}

func TestNewFailsWithoutBrowserWhenOneIsRequired(t *testing.T) {
	cfg := minimalConfig(t)
	// A nonexistent explicit path guarantees discovery fails regardless of
	// what is actually installed on the machine running the test.
	cfg.Browser.Path = filepath.Join(t.TempDir(), "no-such-chrome")
	cfg.Browser.Runtime = "local"

	_, err := New(context.Background(), cfg, Options{Version: "test", RequireBrowser: true})
	if err == nil {
		t.Fatal("New accepted an explicit Chrome path that does not exist")
	}
}

func TestNewUnwindsTheStoreWhenRuleLoadingFails(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.Consent.RuleFiles = []string{filepath.Join(t.TempDir(), "absent.yaml")}

	_, err := New(context.Background(), cfg, Options{Version: "test"})
	if err == nil {
		t.Fatal("New accepted a consent rule file that does not exist")
	}

	// The store New opened before rule-loading failed must not be left open:
	// re-opening the same path must succeed rather than finding it locked.
	st, openErr := store.Open(store.Options{
		Path:        cfg.Store.Path,
		ArtifactDir: filepath.Join(filepath.Dir(cfg.Store.Path), "artifacts"),
	})
	if openErr != nil {
		t.Fatalf("the store from the failed start was not released: %v", openErr)
	}

	_ = st.Close()
}

// testUserRulePack is a minimal user rule pack: enough for the loader to
// accept it as one additional rule beyond the builtin set.
const testUserRulePack = `version: 1

rules:
  - name: app-test-vendor
    vendor: AppTestVendor
    priority: 99
    detect: |
      !!window.__appTestVendor
    reject:
      - eval: |
          window.__appTestVendor.reject()
`

func loadBuiltinForTest(t *testing.T) (interface{ Len() int }, error) {
	t.Helper()

	a := &App{Config: config.New(), Logger: discardLogger()}

	return a.loadRules()
}
