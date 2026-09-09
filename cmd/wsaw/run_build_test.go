package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/app"
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

	st, err := store.Open(store.Options{
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

	targets, err := reload(a, *cf)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
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
