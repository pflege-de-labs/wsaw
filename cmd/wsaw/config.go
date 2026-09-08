package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// configFlags are shared by every command that needs configuration.
type configFlags struct {
	path        string
	urls        []string
	urlFile     string
	modes       string
	concurrency int
	logLevel    string
	logFormat   string
	chromePath  string
	noSandbox   bool
	runtime     string
	storePath   string
	outputDir   string
}

func (c *configFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.path, "config", "", "path to the configuration file (default: the first of the standard locations that exists)")
	fs.Func("url", "scan this URL; may be repeated. Implies a config-file-less run", func(v string) error {
		c.urls = append(c.urls, v)

		return nil
	})
	fs.StringVar(&c.urlFile, "url-file", "", "read newline-delimited URLs from this file")
	fs.StringVar(&c.modes, "consent-modes", "", "comma-separated consent modes to scan (none, reject, accept)")
	fs.IntVar(&c.concurrency, "concurrency", 0, "parallel scans (default: derived from the machine)")
	fs.StringVar(&c.logLevel, "log-level", "", "debug, info, warn or error")
	fs.StringVar(&c.logFormat, "log-format", "", "auto, pretty, json or text (auto: pretty on a terminal, json when piped)")
	fs.StringVar(&c.chromePath, "chrome-path", "", "path to the Chrome or Chromium binary")
	fs.BoolVar(&c.noSandbox, "no-sandbox", false, "disable the Chrome sandbox (weakens isolation; only for constrained containers)")
	fs.StringVar(&c.runtime, "browser-runtime", "", "where the browser runs: auto, podman, docker or local (auto: a container when a runtime is available)")
	fs.StringVar(&c.storePath, "store", "", "path to the result database")
	fs.StringVar(&c.outputDir, "output-dir", "", "write results and reports into this directory")
}

// load builds the configuration and insists on having something to scan.
// Every command that scans, serves or reads a target's history needs that;
// the one that maintains the store does not (loadWithoutTargets).
func (c *configFlags) load() (*config.Config, error) {
	cfg, err := c.loadWithoutTargets()
	if err != nil {
		return nil, err
	}

	if len(cfg.Targets) == 0 {
		return nil, errors.New("no targets are configured; add them to the configuration file or pass --url")
	}

	return cfg, nil
}

// loadWithoutTargets builds the configuration from a file, ad-hoc URLs, or
// both, then applies flag overrides. Flags override the file, which is what
// makes container and systemd operation possible without editing a file
// (Tenet 15).
//
// It stops short of requiring a target, because store maintenance is about the
// store: an operator upgrading one should not have to have a target list
// configured to be allowed to do it (Story 8.4).
func (c *configFlags) loadWithoutTargets() (*config.Config, error) {
	var (
		cfg *config.Config
		err error
	)

	switch {
	case c.path != "":
		cfg, err = config.Load(c.path)
		if err != nil {
			return nil, err
		}

	case len(c.urls) > 0 || c.urlFile != "":
		// A config-file-less run is supported for one-shot and CI use.
		cfg = config.New()

	default:
		cfg, err = loadFromDefaultPaths()
		if err != nil {
			return nil, err
		}
	}

	if err := c.addAdHocTargets(cfg); err != nil {
		return nil, err
	}

	c.applyOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func loadFromDefaultPaths() (*config.Config, error) {
	var tried []string

	for _, path := range app.DefaultConfigPaths() {
		if _, err := os.Stat(path); err != nil {
			tried = append(tried, path)

			continue
		}

		return config.Load(path)
	}

	return nil, fmt.Errorf("no configuration file found; looked in %s. Pass --config, or use --url for a one-shot run",
		strings.Join(tried, ", "))
}

func (c *configFlags) addAdHocTargets(cfg *config.Config) error {
	urls := append([]string{}, c.urls...)

	if c.urlFile != "" {
		b, err := os.ReadFile(c.urlFile) //nolint:gosec // operator-supplied path
		if err != nil {
			return fmt.Errorf("reading URL file %s: %w", c.urlFile, err)
		}

		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			urls = append(urls, line)
		}
	}

	for i, u := range urls {
		cfg.Targets = append(cfg.Targets, config.Target{
			Name: adHocName(u, i),
			URL:  u,
		})
	}

	return nil
}

// adHocName derives a stable name from a URL so that repeated one-shot runs
// of the same URL share history, rather than starting fresh each time.
func adHocName(rawURL string, index int) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")

	var b strings.Builder

	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return fmt.Sprintf("target-%d", index+1)
	}

	const maxName = 80
	if len(name) > maxName {
		name = name[:maxName]
	}

	return name
}

func (c *configFlags) applyOverrides(cfg *config.Config) {
	if c.modes != "" {
		var modes []model.ConsentMode

		for _, m := range strings.Split(c.modes, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modes = append(modes, model.ConsentMode(m))
			}
		}

		cfg.Defaults.ConsentModes = modes
	}

	if c.concurrency > 0 {
		cfg.Scheduler.Concurrency = c.concurrency
	}

	if c.logLevel != "" {
		cfg.Logging.Level = c.logLevel
	}

	if c.logFormat != "" {
		cfg.Logging.Format = c.logFormat
	}

	if c.chromePath != "" {
		cfg.Browser.Path = c.chromePath
	}

	if c.noSandbox {
		cfg.Browser.NoSandbox = true
	}

	if c.runtime != "" {
		cfg.Browser.Runtime = c.runtime
	}

	if c.storePath != "" {
		cfg.Store.Path = c.storePath
	}

	if c.outputDir != "" {
		cfg.Store.OutputDir = c.outputDir
	}
}

// cmdConfig validates configuration and prints what wsaw actually resolved,
// which is the fastest way to see why a target behaves unexpectedly.
func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	checkOnly := fs.Bool("check", false, "validate and exit without printing")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cf.load()
	if err != nil {
		return err
	}

	targets, err := cfg.ResolveTargets(nil)
	if err != nil {
		return err
	}

	if *checkOnly {
		fmt.Printf("configuration is valid: %d target(s)\n", len(targets))

		return nil
	}

	// Resolved targets are printed rather than the raw file, because the
	// resolved view is what actually runs. Secrets render as their redacted
	// form through their own marshaller.
	out, err := yaml.Marshal(struct {
		Targets     []config.Resolved `yaml:"resolvedTargets"`
		Concurrency int               `yaml:"concurrency"`
		PoolSize    int               `yaml:"browserPoolSize"`
	}{
		Targets:     targets,
		Concurrency: cfg.Concurrency(),
		PoolSize:    cfg.PoolSize(),
	})
	if err != nil {
		return fmt.Errorf("rendering configuration: %w", err)
	}

	fmt.Print(string(out))

	return nil
}
