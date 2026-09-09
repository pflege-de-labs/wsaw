package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// parseFlags registers the shared flags and parses args through them, which is
// how every command reaches configFlags.
func parseFlags(t *testing.T, args ...string) *configFlags {
	t.Helper()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cf configFlags

	cf.register(fs)

	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}

	return &cf
}

func TestLoadReadsTheConfigurationFile(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	cf := parseFlags(t, "--config", path)

	cfg, err := cf.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Targets) != 1 || cfg.Targets[0].Name != "site" {
		t.Fatalf("targets = %+v, want one target named site", cfg.Targets)
	}
}

func TestLoadReportsAnUnreadableConfigurationFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	cf := parseFlags(t, "--config", missing)

	if _, err := cf.load(); err == nil {
		t.Fatal("load accepted a configuration file that does not exist")
	}
}

// TestLoadRunsWithoutAConfigurationFile covers the one-shot CI path: --url
// alone has to be enough, or wsaw cannot run in a container without a mounted
// file (Tenet 15).
func TestLoadRunsWithoutAConfigurationFile(t *testing.T) {
	cf := parseFlags(t, "--url", "https://example.com/", "--url", "https://other.test/x")

	cfg, err := cf.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Targets) != 2 {
		t.Fatalf("targets = %+v, want two", cfg.Targets)
	}

	if cfg.Targets[0].Name != "example.com" || cfg.Targets[1].Name != "other.test-x" {
		t.Errorf("names = %q, %q; want example.com and other.test-x",
			cfg.Targets[0].Name, cfg.Targets[1].Name)
	}
}

func TestLoadReadsURLsFromAFile(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "urls.txt")

	body := "# a comment\n\nhttps://a.test/\n  https://b.test/  \n\n"
	if err := os.WriteFile(list, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cf := parseFlags(t, "--url-file", list)

	cfg, err := cf.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Targets) != 2 {
		t.Fatalf("targets = %+v, want two (comments and blank lines skipped)", cfg.Targets)
	}

	if cfg.Targets[0].URL != "https://a.test/" || cfg.Targets[1].URL != "https://b.test/" {
		t.Errorf("URLs = %q, %q", cfg.Targets[0].URL, cfg.Targets[1].URL)
	}
}

func TestLoadReportsAMissingURLFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "urls.txt")

	cf := parseFlags(t, "--url-file", missing)

	_, err := cf.load()
	if err == nil {
		t.Fatal("load accepted a URL file that does not exist")
	}

	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name %s", err, missing)
	}
}

// TestLoadRefusesAnEmptyTargetList guards the failure that would otherwise
// look like success: a run with nothing to scan reporting itself as clean.
func TestLoadRefusesAnEmptyTargetList(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "urls.txt")

	if err := os.WriteFile(list, []byte("# nothing but a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cf := parseFlags(t, "--url-file", list)

	_, err := cf.load()
	if err == nil {
		t.Fatal("load accepted a configuration with no targets")
	}

	if !strings.Contains(err.Error(), "no targets are configured") {
		t.Errorf("error = %v, want it to say no targets are configured", err)
	}
}

func TestLoadFromDefaultPathsNamesWhereItLooked(t *testing.T) {
	// An empty working directory means the relative default cannot match
	// either, so every candidate is missing.
	t.Chdir(t.TempDir())

	cf := parseFlags(t)

	_, err := cf.load()
	if err == nil {
		t.Fatal("load found a configuration file in an empty directory")
	}

	if !strings.Contains(err.Error(), "no configuration file found") {
		t.Fatalf("error = %v, want it to report no configuration file", err)
	}

	// The list of places tried is the whole value of this message.
	if !strings.Contains(err.Error(), "wsaw.yaml") {
		t.Errorf("error = %v, want it to name the paths it tried", err)
	}
}

func TestLoadFromDefaultPathsUsesTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	body := oneTargetConfig + "\nstore:\n  path: " + filepath.Join(dir, "wsaw.db") + "\n"
	if err := os.WriteFile("wsaw.yaml", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cf := parseFlags(t)

	cfg, err := cf.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Targets) != 1 {
		t.Fatalf("targets = %+v, want the one from ./wsaw.yaml", cfg.Targets)
	}
}

// TestApplyOverridesBeatTheFile is the flag-over-file rule that makes systemd
// and container operation possible without editing a file.
func TestApplyOverridesBeatTheFile(t *testing.T) {
	cf := parseFlags(t,
		"--consent-modes", "none, accept ,",
		"--concurrency", "7",
		"--log-level", "debug",
		"--log-format", "json",
		"--chrome-path", "/usr/bin/chromium",
		"--no-sandbox",
		"--browser-runtime", "local",
		"--store", "/tmp/wsaw-test.db",
		"--output-dir", "/tmp/wsaw-test-out",
	)

	cfg := config.New()
	cfg.Logging.Level = "warn"
	cfg.Scheduler.Concurrency = 1

	cf.applyOverrides(cfg)

	if got, want := cfg.Defaults.ConsentModes, []model.ConsentMode{model.ConsentNone, model.ConsentAccept}; len(got) != len(want) {
		t.Fatalf("consent modes = %v, want %v", got, want)
	}

	// The empty element after the trailing comma must not become a mode.
	for i, want := range []model.ConsentMode{model.ConsentNone, model.ConsentAccept} {
		if cfg.Defaults.ConsentModes[i] != want {
			t.Errorf("mode %d = %q, want %q", i, cfg.Defaults.ConsentModes[i], want)
		}
	}

	if cfg.Scheduler.Concurrency != 7 {
		t.Errorf("concurrency = %d, want 7", cfg.Scheduler.Concurrency)
	}

	if cfg.Logging.Level != "debug" || cfg.Logging.Format != "json" {
		t.Errorf("logging = %+v, want debug/json", cfg.Logging)
	}

	if cfg.Browser.Path != "/usr/bin/chromium" || cfg.Browser.Runtime != "local" {
		t.Errorf("browser = %+v", cfg.Browser)
	}

	if !cfg.Browser.NoSandbox {
		t.Error("--no-sandbox did not reach the configuration")
	}

	if cfg.Store.Path != "/tmp/wsaw-test.db" || cfg.Store.OutputDir != "/tmp/wsaw-test-out" {
		t.Errorf("store = %+v", cfg.Store)
	}
}

// TestApplyOverridesLeaveTheFileAloneWhenUnset is the other half of the rule:
// an unset flag must not overwrite a configured value with a zero one.
func TestApplyOverridesLeaveTheFileAloneWhenUnset(t *testing.T) {
	cf := parseFlags(t)

	cfg := config.New()
	cfg.Logging.Level = "warn"
	cfg.Scheduler.Concurrency = 3
	cfg.Browser.Path = "/opt/chrome"
	cfg.Store.Path = "/var/lib/wsaw/wsaw.db"

	cf.applyOverrides(cfg)

	if cfg.Logging.Level != "warn" || cfg.Scheduler.Concurrency != 3 {
		t.Errorf("unset flags overwrote logging or concurrency: %+v %+v", cfg.Logging, cfg.Scheduler)
	}

	if cfg.Browser.Path != "/opt/chrome" || cfg.Store.Path != "/var/lib/wsaw/wsaw.db" {
		t.Errorf("unset flags overwrote a path: %+v %+v", cfg.Browser, cfg.Store)
	}

	if cfg.Browser.NoSandbox {
		t.Error("the sandbox was disabled without --no-sandbox")
	}
}

func TestAdHocName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		url   string
		index int
		want  string
	}{
		{name: "https is stripped", url: "https://example.com/", want: "example.com"},
		{name: "http is stripped", url: "http://example.com", want: "example.com"},
		{name: "path becomes dashes", url: "https://a.test/b/c", want: "a.test-b-c"},
		{name: "query becomes dashes", url: "https://a.test/?q=1&r=2", want: "a.test--q-1-r-2"},
		{name: "port survives as a dash", url: "https://a.test:8080/", want: "a.test-8080"},
		{name: "nothing usable falls back to the index", url: "https://", index: 2, want: "target-3"},
		{name: "empty falls back to the index", url: "", want: "target-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := adHocName(tc.url, tc.index); got != tc.want {
				t.Errorf("adHocName(%q, %d) = %q, want %q", tc.url, tc.index, got, tc.want)
			}
		})
	}
}

// TestAdHocNameIsBounded keeps a page-supplied name from growing without limit
// — the name reaches a filesystem path through sanitize.
func TestAdHocNameIsBounded(t *testing.T) {
	t.Parallel()

	name := adHocName("https://a.test/"+strings.Repeat("x", 500), 0)

	if len(name) != 80 {
		t.Errorf("length = %d, want it capped at 80", len(name))
	}
}

// TestAdHocNameIsStable is the reason the name is derived rather than
// generated: repeated one-shot runs of the same URL must share history.
func TestAdHocNameIsStable(t *testing.T) {
	t.Parallel()

	first := adHocName("https://example.com/a", 0)
	second := adHocName("https://example.com/a", 9)

	if first != second {
		t.Errorf("names differ across runs: %q and %q", first, second)
	}
}

func TestCmdConfigChecksWithoutPrinting(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	var err error

	stdout, _ := capture(t, func() {
		err = cmdConfig([]string{"--config", path, "--check"})
	})

	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(stdout, "configuration is valid: 1 target(s)") {
		t.Errorf("stdout = %q, want the validation summary", stdout)
	}
}

// TestCmdConfigPrintsTheResolvedView prints what actually runs rather than the
// file, which is the point of the command.
func TestCmdConfigPrintsTheResolvedView(t *testing.T) {
	path := writeConfig(t, oneTargetConfig)

	var err error

	stdout, _ := capture(t, func() {
		err = cmdConfig([]string{"--config", path})
	})

	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	for _, want := range []string{"resolvedTargets", "concurrency", "browserPoolSize", "site"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q; got:\n%s", want, stdout)
		}
	}
}

func TestCmdConfigRejectsBadInput(t *testing.T) {
	invalid := writeConfig(t, "targets:\n  - name: site\n    url: not-a-url\n")

	cases := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--nope"}},
		{name: "invalid configuration", args: []string{"--config", invalid}},
		{name: "missing file", args: []string{"--config", filepath.Join(t.TempDir(), "absent.yaml")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			capture(t, func() {
				err = cmdConfig(tc.args)
			})

			if err == nil {
				t.Errorf("cmdConfig(%v) succeeded, want an error", tc.args)
			}
		})
	}
}
