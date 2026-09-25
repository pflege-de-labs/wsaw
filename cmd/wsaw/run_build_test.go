package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/certtest"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// testApp is the parts of an App the daemon builders touch. It deliberately
// stops short of app.New: these functions turn configuration into notifiers, a
// signer and a server, and none of that needs a browser or a scanner.
func testApp(t *testing.T, cfg *config.Config) *app.App {
	t.Helper()

	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return &app.App{
		Config:  cfg,
		Logger:  slog.New(slog.DiscardHandler),
		Metrics: metrics.New("test"),
		Secrets: &secret.Registry{},
		Store:   st,
		Version: "test",
	}
}

func TestPluralSettings(t *testing.T) {
	t.Parallel()

	if got := pluralSettings(1); got != "it" {
		t.Errorf("pluralSettings(1) = %q, want %q", got, "it")
	}

	for _, n := range []int{0, 2, 7} {
		if got := pluralSettings(n); got != "them" {
			t.Errorf("pluralSettings(%d) = %q, want %q", n, got, "them")
		}
	}
}

func TestResolveNotifierResolvesSecrets(t *testing.T) {
	t.Setenv("WSAW_TEST_HOOK_URL", "https://hook.test/secret-path")
	t.Setenv("WSAW_TEST_HOOK_HEADER", "s3cret")

	a := testApp(t, config.New())

	got, err := resolveNotifier(a, config.Notifier{
		Name:        "ops",
		URL:         "${env:WSAW_TEST_HOOK_URL}",
		MinSeverity: "high",
		Headers:     map[string]string{"X-Token": "${env:WSAW_TEST_HOOK_HEADER}"},
	})
	if err != nil {
		t.Fatalf("resolveNotifier: %v", err)
	}

	if got.url.Reveal() != "https://hook.test/secret-path" {
		t.Errorf("url = %q", got.url.Reveal())
	}

	if got.headers["X-Token"].Reveal() != "s3cret" {
		t.Errorf("header = %q", got.headers["X-Token"].Reveal())
	}

	if got.minSeverity != diff.SeverityHigh {
		t.Errorf("minSeverity = %v, want high", got.minSeverity)
	}

	// A Workflow URL carries its authorisation in the query string, so the URL
	// is registered as a secret whatever the notifier kind (Story 5.14, AC2).
	scrubbed := a.Secrets.Scrub("posting to https://hook.test/secret-path with s3cret")

	if strings.Contains(scrubbed, "secret-path") || strings.Contains(scrubbed, "s3cret") {
		t.Errorf("the notifier secrets are not redacted: %q", scrubbed)
	}
}

func TestResolveNotifierRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		notifier config.Notifier
		want     string
	}{
		{
			name:     "unresolvable url",
			notifier: config.Notifier{Name: "ops", URL: "${env:WSAW_TEST_ABSENT_URL}"},
			want:     "url",
		},
		{
			name: "unresolvable header",
			notifier: config.Notifier{
				Name:    "ops",
				URL:     "https://hook.test/",
				Headers: map[string]string{"X-Token": "${env:WSAW_TEST_ABSENT_HEADER}"},
			},
			want: "header X-Token",
		},
		{
			name:     "unknown severity",
			notifier: config.Notifier{Name: "ops", URL: "https://hook.test/", MinSeverity: "catastrophic"},
			want:     "ops",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t, config.New())

			_, err := resolveNotifier(a, tc.notifier)
			if err == nil {
				t.Fatal("resolveNotifier accepted input it cannot resolve")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}

			// The message names the notifier, or an operator with six of them
			// cannot tell which one to fix.
			if !strings.Contains(err.Error(), "ops") {
				t.Errorf("error = %v, want it to name the notifier", err)
			}
		})
	}
}

func TestBuildDispatcherIsAbsentWithoutNotifiers(t *testing.T) {
	a := testApp(t, config.New())

	d, err := buildDispatcher(a)
	if err != nil {
		t.Fatalf("buildDispatcher: %v", err)
	}

	if d != nil {
		t.Error("buildDispatcher built a dispatcher with nothing configured")
	}
}

func TestBuildDispatcherBuildsEachKind(t *testing.T) {
	cfg := config.New()
	cfg.Notify = []config.Notifier{
		{Name: "hook", URL: "https://hook.test/", MinSeverity: "low", ChangeTypes: []string{"newThirdParty"}},
		{Name: "teams", Kind: config.NotifyTeams, URL: "https://teams.test/", BaseURL: "https://wsaw.test"},
	}

	a := testApp(t, cfg)

	d, err := buildDispatcher(a)
	if err != nil {
		t.Fatalf("buildDispatcher: %v", err)
	}

	if d == nil {
		t.Fatal("buildDispatcher returned nothing with two notifiers configured")
	}
}

// TestBuildDispatcherWarnsAboutTheRetiredTeamsFormat covers the deprecation
// path: the connector format still works, and saying nothing about it would
// leave an operator to discover the retirement from silence.
func TestBuildDispatcherWarnsAboutTheRetiredTeamsFormat(t *testing.T) {
	cfg := config.New()
	cfg.Notify = []config.Notifier{
		{
			Name:    "teams",
			Kind:    config.NotifyTeams,
			URL:     "https://teams.test/",
			Format:  config.TeamsMessageCard,
			BaseURL: "https://wsaw.test",
		},
	}

	logged := &strings.Builder{}
	a := testApp(t, cfg)
	a.Logger = slog.New(slog.NewTextHandler(logged, nil))

	if _, err := buildDispatcher(a); err != nil {
		t.Fatalf("buildDispatcher: %v", err)
	}

	if !strings.Contains(logged.String(), "retired") {
		t.Errorf("the retired format was accepted without a warning: %q", logged.String())
	}
}

func TestBuildDispatcherFailsOnABadNotifier(t *testing.T) {
	cfg := config.New()
	cfg.Notify = []config.Notifier{{Name: "ops", URL: "https://hook.test/", MinSeverity: "catastrophic"}}

	a := testApp(t, cfg)

	if _, err := buildDispatcher(a); err == nil {
		t.Fatal("buildDispatcher accepted a notifier it could not resolve")
	}
}

// TestBuildShareSignerIsOffByDefault is a deliberate default: a share link
// publishes data and cannot be revoked before it expires (Story 5.19, AC12).
func TestBuildShareSignerIsOffByDefault(t *testing.T) {
	a := testApp(t, config.New())

	signer, err := buildShareSigner(a)
	if err != nil {
		t.Fatalf("buildShareSigner: %v", err)
	}

	if signer != nil {
		t.Error("sharing was enabled without being asked for")
	}
}

func TestBuildShareSignerRegistersTheKeyAsASecret(t *testing.T) {
	cfg := config.New()
	cfg.API.Share.Enabled = true
	cfg.API.Share.Key = shareKey

	a := testApp(t, cfg)

	signer, err := buildShareSigner(a)
	if err != nil {
		t.Fatalf("buildShareSigner: %v", err)
	}

	if signer == nil {
		t.Fatal("sharing is enabled but no signer was built")
	}

	// The key signs credentials, so it must never survive into a log line
	// (Story 5.19, AC11).
	if scrubbed := a.Secrets.Scrub("key is " + shareKey); strings.Contains(scrubbed, shareKey) {
		t.Errorf("the signing key is not redacted: %q", scrubbed)
	}
}

func TestBuildShareSignerRejectsABadKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "unresolvable", key: "${env:WSAW_TEST_ABSENT_SHARE_KEY}"},
		{name: "too short", key: "short"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			cfg.API.Share.Enabled = true
			cfg.API.Share.Key = tc.key

			a := testApp(t, cfg)

			if _, err := buildShareSigner(a); err == nil {
				t.Errorf("buildShareSigner accepted the %s key", tc.name)
			}
		})
	}
}

func TestBuildServerWiresTheAPI(t *testing.T) {
	cfg := config.New()
	cfg.API.Enabled = true
	cfg.API.Listen = "127.0.0.1:0"

	a := testApp(t, cfg)

	srv, err := buildServer(a, nil, func() []config.Resolved { return nil })
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}

	if srv == nil {
		t.Fatal("buildServer returned no server")
	}
}

func TestBuildServerRejectsAnUnresolvableToken(t *testing.T) {
	cfg := config.New()
	cfg.API.Enabled = true
	cfg.API.Token = "${env:WSAW_TEST_ABSENT_TOKEN}"

	a := testApp(t, cfg)

	_, err := buildServer(a, nil, func() []config.Resolved { return nil })
	if err == nil {
		t.Fatal("buildServer accepted a token it could not resolve")
	}

	if !strings.Contains(err.Error(), "api.token") {
		t.Errorf("error = %v, want it to name the setting", err)
	}
}

func TestReloadAdoptsANewTargetList(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)

	// Only the target list changes, which is the one thing a reload can apply.
	updated := `targets:
  - name: site
    url: https://example.com/
    consentModes: [reject]
  - name: other
    url: https://other.test/
    consentModes: [reject]
`

	body := updated + "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	next, err := reload(a, *cf)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(next.targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(next.targets))
	}
}

// TestReloadHandsOnTheSweepSchedule is Story 4.12, AC8: store.sweep and
// store.sweepInterval are applied by a reload rather than refused as
// startup-only, and reload returns the schedule for the maintenance loop.
func TestReloadHandsOnTheSweepSchedule(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)

	body := oneTargetConfig + "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") +
		"\n  sweepInterval: 6h\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	next, err := reload(a, *cf)
	if err != nil {
		t.Fatalf("reload refused a new sweep interval: %v", err)
	}

	if want := (app.SweepSchedule{Enabled: true, Interval: 6 * time.Hour}); next.sweep != want {
		t.Errorf("reload handed on %+v, want %+v", next.sweep, want)
	}
}

// TestReloadHandsOnTheVacuumSchedule is Story 4.13, AC11: a reload that
// changes the vacuum schedule is accepted and hands the new one on.
func TestReloadHandsOnTheVacuumSchedule(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)

	body := oneTargetConfig + "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") +
		"\n  vacuumInterval: 24h\n  vacuumMinFreeRatio: 0.5\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	next, err := reload(a, *cf)
	if err != nil {
		t.Fatalf("reload refused a new vacuum schedule: %v", err)
	}

	if want := (app.VacuumSchedule{Enabled: true, Interval: 24 * time.Hour, MinFreeRatio: 0.5}); next.vacuum != want {
		t.Errorf("reload handed on %+v, want %+v", next.vacuum, want)
	}
}

// TestOfferScheduleNeverBlocksAndKeepsTheLatest: the reload path must
// not wait for a maintenance loop that is busy sweeping, and when two reloads
// arrive before the loop takes either, the newer one is what it gets.
func TestOfferScheduleNeverBlocksAndKeepsTheLatest(t *testing.T) {
	t.Parallel()

	sweeps := make(chan app.SweepSchedule, 1)

	offerSchedule(sweeps, app.SweepSchedule{Enabled: true, Interval: time.Hour})
	offerSchedule(sweeps, app.SweepSchedule{Enabled: false, Interval: time.Hour})

	if got := <-sweeps; got.Enabled {
		t.Errorf("the loop was handed %+v, want the later reload's schedule", got)
	}
}

// tlsAPI is an api section serving the given pair.
func tlsAPI(certPath, keyPath string) string {
	return "\napi:\n  enabled: true\n  tlsCert: " + certPath + "\n  tlsKey: " + keyPath + "\n"
}

// TestReloadMovesTheCertificateButDoesNotTurnTLSOff is Story 5.33, AC6: new
// paths are handed on for the server to load, while turning TLS off would
// need a new listener and is refused like any other startup-only change.
func TestReloadMovesTheCertificateButDoesNotTurnTLSOff(t *testing.T) {
	path := writeConfig(t, oneTargetConfig+tlsAPI(certtest.Write(t)))

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)
	store := "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") + "\n"

	movedCert, movedKey := certtest.Write(t)
	if err := os.WriteFile(path, []byte(oneTargetConfig+tlsAPI(movedCert, movedKey)+store), 0o600); err != nil {
		t.Fatal(err)
	}

	next, err := reload(a, *cf)
	if err != nil {
		t.Fatalf("reload refused a moved certificate: %v", err)
	}

	if want := (tlsPaths{cert: movedCert, key: movedKey}); next.certPaths != want {
		t.Errorf("reload handed on %+v, want %+v", next.certPaths, want)
	}

	if err := os.WriteFile(path, []byte(oneTargetConfig+"\napi:\n  enabled: true\n"+store), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = reload(a, *cf)
	if err == nil || !strings.Contains(err.Error(), "api.tlsCert") {
		t.Errorf("turning TLS off by reload returned %v, want a refusal naming api.tlsCert", err)
	}
}

// TestReloadRefusesACertificateTheServerCouldNotLoad is Story 5.33, AC11: a
// reload whose certificate is unusable is refused, naming the setting,
// rather than logged as "configuration reloaded" over a pair that is not
// being served.
func TestReloadRefusesACertificateTheServerCouldNotLoad(t *testing.T) {
	certPath, keyPath := certtest.Write(t)
	path := writeConfig(t, oneTargetConfig+tlsAPI(certPath, keyPath))

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)

	// The same paths, and a certificate that no longer matches its key: the
	// renewal nobody finished.
	if err := os.WriteFile(certPath, certtest.Issue(t, time.Now(), time.Hour).Cert, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = reload(a, *cf)
	if err == nil || !strings.Contains(err.Error(), "api.tlsCert") {
		t.Errorf("reload with an unusable certificate returned %v, want a refusal naming api.tlsCert", err)
	}
}

// TestReloadAcceptsAnExpiredCertificateAndSaysSo is Story 5.33, AC11:
// startup serves an expired certificate and logs an error, so a reload
// accepts one too, logging the same thing. A restart would accept the file,
// and a reload refusing it would block every target change until someone
// renewed a certificate.
func TestReloadAcceptsAnExpiredCertificateAndSaysSo(t *testing.T) {
	certPath, keyPath := certtest.Write(t)
	path := writeConfig(t, oneTargetConfig+tlsAPI(certPath, keyPath))

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)
	logged := &strings.Builder{}
	a.Logger = slog.New(slog.NewTextHandler(logged, nil))

	expiredCert, expiredKey := certtest.WritePair(t, certtest.Issue(t, time.Now().Add(-48*time.Hour), 24*time.Hour))
	store := "\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") + "\n"

	if err := os.WriteFile(path, []byte(oneTargetConfig+tlsAPI(expiredCert, expiredKey)+store), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := reload(a, *cf); err != nil {
		t.Fatalf("reload refused an expired certificate: %v", err)
	}

	if !strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "has expired") {
		t.Errorf("the expired certificate was not logged as an error:\n%s", logged.String())
	}
}

// TestReloadRefusesWhatItCannotApply is the regression this behaviour exists
// for: adopting the reloadable half while reporting "configuration reloaded"
// would leave an operator believing the rest had applied too.
func TestReloadRefusesWhatItCannotApply(t *testing.T) {
	path := writeConfig(t, oneTargetConfig+"\nscheduler:\n  concurrency: 2\n")

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)

	body := oneTargetConfig + "\nscheduler:\n  concurrency: 8\n" +
		"\nstore:\n  path: " + filepath.Join(filepath.Dir(path), "wsaw.db") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = reload(a, *cf)
	if err == nil {
		t.Fatal("reload accepted a change it cannot apply")
	}

	if !strings.Contains(err.Error(), "cannot change without a restart") {
		t.Errorf("error = %v, want it to refuse the change", err)
	}

	// The refusal names what moved and says the running configuration stands.
	if !strings.Contains(err.Error(), "scheduler.concurrency") {
		t.Errorf("error = %v, want it to name the setting", err)
	}

	if a.Config.Scheduler.Concurrency != 2 {
		t.Errorf("concurrency = %d; the running configuration was changed anyway",
			a.Config.Scheduler.Concurrency)
	}
}

func TestReloadReportsABrokenFile(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	cf := parseFlags(t, "--config", path)

	running, err := cf.load()
	if err != nil {
		t.Fatalf("loading the running configuration: %v", err)
	}

	a := testApp(t, running)

	if err := os.WriteFile(path, []byte("targets: [oh dear\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := reload(a, *cf); err == nil {
		t.Fatal("reload accepted a file it could not parse")
	}
}
