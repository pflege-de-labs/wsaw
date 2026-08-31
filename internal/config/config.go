// Package config loads and validates wsaw's configuration.
//
// The target list is inherently file-shaped, so YAML is the primary source
// and flags and environment variables override it (Tenet 15). Configuration
// is validated completely at load time with line references, because a
// daemon that starts with a broken target list is a watcher that silently
// watches the wrong thing.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/martint17r/wsaw/internal/diff"
	"github.com/martint17r/wsaw/internal/model"
)

// Config is the whole configuration.
type Config struct {
	// Defaults apply to every target unless the target overrides them.
	Defaults Target `yaml:"defaults"`

	Targets []Target `yaml:"targets"`

	Browser   Browser    `yaml:"browser"`
	Store     Store      `yaml:"store"`
	Scheduler Scheduler  `yaml:"scheduler"`
	Normalize Normalize  `yaml:"normalize"`
	Detection Detection  `yaml:"detection"`
	Consent   Consent    `yaml:"consent"`
	API       API        `yaml:"api"`
	Notify    []Notifier `yaml:"notify"`
	Logging   Logging    `yaml:"logging"`
	Metrics   Metrics    `yaml:"metrics"`

	// path records where the config came from, for reload and error messages.
	path string
}

// Target is one URL to watch, or the set of defaults for all of them.
type Target struct {
	Name   string            `yaml:"name"`
	URL    string            `yaml:"url"`
	Labels map[string]string `yaml:"labels,omitempty"`

	// Disabled keeps a target in the file without scanning it, which is
	// better than deleting it and losing its history and baseline.
	Disabled bool `yaml:"disabled,omitempty"`

	// ConsentModes lists the states to scan this target in. Each produces its
	// own independent result.
	ConsentModes []model.ConsentMode `yaml:"consentModes,omitempty"`

	// Schedule controls when this target runs.
	Interval Duration `yaml:"interval,omitempty"`
	Cron     string   `yaml:"cron,omitempty"`

	// FirstPartyDomains are additional domains counted as the site's own.
	FirstPartyDomains []string `yaml:"firstPartyDomains,omitempty"`

	// AllowHosts suppresses findings for expected third parties.
	AllowHosts []string `yaml:"allowHosts,omitempty"`
	// DenyHosts raises a finding whenever a matching host appears.
	DenyHosts []string `yaml:"denyHosts,omitempty"`

	// Capture budget.
	IdleQuiet      Duration `yaml:"idleQuiet,omitempty"`
	HardTimeout    Duration `yaml:"hardTimeout,omitempty"`
	NavTimeout     Duration `yaml:"navTimeout,omitempty"`
	MaxRequests    int      `yaml:"maxRequests,omitempty"`
	MaxBytes       int64    `yaml:"maxBytes,omitempty"`
	DwellAfterLoad Duration `yaml:"dwellAfterLoad,omitempty"`
	ScrollToBottom *bool    `yaml:"scrollToBottom,omitempty"`

	// Browser context.
	ViewportWidth  int      `yaml:"viewportWidth,omitempty"`
	ViewportHeight int      `yaml:"viewportHeight,omitempty"`
	DeviceScale    float64  `yaml:"deviceScaleFactor,omitempty"`
	Mobile         *bool    `yaml:"mobile,omitempty"`
	UserAgent      string   `yaml:"userAgent,omitempty"`
	AcceptLanguage string   `yaml:"acceptLanguage,omitempty"`
	Timezone       string   `yaml:"timezone,omitempty"`
	Latitude       *float64 `yaml:"latitude,omitempty"`
	Longitude      *float64 `yaml:"longitude,omitempty"`

	// ExtraHeaders values may be secret references.
	ExtraHeaders map[string]string `yaml:"extraHeaders,omitempty"`

	BasicAuthUser     string `yaml:"basicAuthUser,omitempty"`
	BasicAuthPassword string `yaml:"basicAuthPassword,omitempty"`

	Proxy string `yaml:"proxy,omitempty"`

	// WarmCache opts out of the cold-cache default.
	WarmCache *bool `yaml:"warmCache,omitempty"`

	// Evidence.
	Screenshots *bool `yaml:"screenshots,omitempty"`
	StoreBodies *bool `yaml:"storeBodies,omitempty"`

	// Politeness.
	Robots      RobotsPolicy `yaml:"robots,omitempty"`
	MinInterval Duration     `yaml:"minInterval,omitempty"`
	Jitter      Duration     `yaml:"jitter,omitempty"`

	// Severity overrides for this target.
	Severity diff.SeverityRules `yaml:"severity,omitempty"`

	// line records where this target was defined, for error messages.
	line int
}

// RobotsPolicy decides whether a target honours robots.txt.
type RobotsPolicy string

// Robots policies.
const (
	// RobotsIgnore scans regardless of robots.txt. Appropriate for one's own
	// properties.
	RobotsIgnore RobotsPolicy = "ignore"
	// RobotsRespect skips a disallowed URL, recording a skipped outcome.
	RobotsRespect RobotsPolicy = "respect"
)

// Browser configures Chrome discovery and pooling.
type Browser struct {
	Path      string   `yaml:"path,omitempty"`
	RemoteURL string   `yaml:"remoteUrl,omitempty"`
	NoSandbox bool     `yaml:"noSandbox,omitempty"`
	ExtraArgs []string `yaml:"extraArgs,omitempty"`

	PoolSize           int      `yaml:"poolSize,omitempty"`
	MaxScansPerBrowser int64    `yaml:"maxScansPerBrowser,omitempty"`
	LaunchTimeout      Duration `yaml:"launchTimeout,omitempty"`
}

// Store configures persistence.
type Store struct {
	Path         string   `yaml:"path,omitempty"`
	ArtifactDir  string   `yaml:"artifactDir,omitempty"`
	OutputDir    string   `yaml:"outputDir,omitempty"`
	MaxAge       Duration `yaml:"maxAge,omitempty"`
	MaxPerSeries int      `yaml:"maxPerSeries,omitempty"`

	// WriteJSONL mirrors results to newline-delimited JSON files.
	WriteJSONL bool `yaml:"writeJsonl,omitempty"`
	// WriteHAR emits a HAR file per scan. Off by default because of size.
	WriteHAR bool `yaml:"writeHar,omitempty"`
	// WriteReport emits a Markdown report per scan.
	WriteReport bool `yaml:"writeReport,omitempty"`
}

// Scheduler configures the daemon loop.
type Scheduler struct {
	Concurrency int      `yaml:"concurrency,omitempty"`
	Interval    Duration `yaml:"interval,omitempty"`
	Cron        string   `yaml:"cron,omitempty"`

	// Jitter spreads scans so wsaw does not hit one origin in a burst.
	Jitter Duration `yaml:"jitter,omitempty"`
	// MinInterval is the floor between two scans of the same target.
	MinInterval Duration `yaml:"minInterval,omitempty"`
	// PerOriginConcurrency limits simultaneous scans against one origin.
	PerOriginConcurrency int `yaml:"perOriginConcurrency,omitempty"`

	// CatchUp runs schedules missed while the daemon was down. Off by
	// default: a restart should not produce a burst of scans.
	CatchUp bool `yaml:"catchUp,omitempty"`

	// ShutdownGrace is how long in-flight scans may finish on SIGTERM.
	ShutdownGrace Duration `yaml:"shutdownGrace,omitempty"`
}

// Normalize configures URL comparison keys.
type Normalize struct {
	DropQueryParams   []string      `yaml:"dropQueryParams,omitempty"`
	KeepQueryParams   []string      `yaml:"keepQueryParams,omitempty"`
	DropAllQuery      bool          `yaml:"dropAllQuery,omitempty"`
	DropTrailingSlash bool          `yaml:"dropTrailingSlash,omitempty"`
	PathReplacements  []Replacement `yaml:"pathReplacements,omitempty"`

	// UseDefaultDropParams adds the shipped list of noise parameters.
	UseDefaultDropParams *bool `yaml:"useDefaultDropParams,omitempty"`
}

// Replacement collapses a volatile path segment.
type Replacement struct {
	Pattern string `yaml:"pattern"`
	With    string `yaml:"with"`
}

// Detection configures change detection.
type Detection struct {
	// Baseline selects what a scan is compared against.
	Baseline BaselineMode `yaml:"baseline,omitempty"`
	// FlapWindow collapses a change and its reverse inside this window.
	FlapWindow Duration `yaml:"flapWindow,omitempty"`
	// HashResourceTypes selects which bodies are fingerprinted.
	HashResourceTypes []string `yaml:"hashResourceTypes,omitempty"`

	AllowHosts []string `yaml:"allowHosts,omitempty"`
	DenyHosts  []string `yaml:"denyHosts,omitempty"`

	Severity diff.SeverityRules `yaml:"severity,omitempty"`
}

// BaselineMode selects the comparison target.
type BaselineMode string

// Baseline modes.
const (
	// BaselineApproved compares against an explicitly approved scan.
	BaselineApproved BaselineMode = "approved"
	// BaselinePrevious compares against the previous scan.
	BaselinePrevious BaselineMode = "previous"
)

// Consent configures banner handling.
type Consent struct {
	// RuleFiles are extra rule packs, applied after the builtin pack.
	RuleFiles []string `yaml:"ruleFiles,omitempty"`
	// AllowHeuristic permits label guessing when no rule matches.
	AllowHeuristic *bool `yaml:"allowHeuristic,omitempty"`
	// OnFailure decides whether a failed interaction fails the scan.
	OnFailure    string   `yaml:"onFailure,omitempty"`
	StepTimeout  Duration `yaml:"stepTimeout,omitempty"`
	TotalTimeout Duration `yaml:"totalTimeout,omitempty"`
}

// API configures the HTTP interface and web UI.
type API struct {
	// Enabled turns the HTTP server on. Off by default in one-shot mode.
	Enabled bool `yaml:"enabled,omitempty"`
	// Listen defaults to localhost: remote exposure must be deliberate.
	Listen string `yaml:"listen,omitempty"`

	// Token protects the API and UI. May be a secret reference.
	Token string `yaml:"token,omitempty"`

	TLSCert string `yaml:"tlsCert,omitempty"`
	TLSKey  string `yaml:"tlsKey,omitempty"`

	// WebUI serves the browser interface.
	WebUI *bool `yaml:"webui,omitempty"`
	// ReadOnly disables every write action, for shared reviewer deployments.
	ReadOnly bool `yaml:"readOnly,omitempty"`
	// AllowAdHocScan permits triggering scans through the API.
	AllowAdHocScan *bool `yaml:"allowAdHocScan,omitempty"`
}

// Notifier delivers change events.
type Notifier struct {
	Name string `yaml:"name"`
	// URL may be a secret reference, since webhook URLs carry tokens.
	URL string `yaml:"url"`

	// MinSeverity filters what is delivered.
	MinSeverity string `yaml:"minSeverity,omitempty"`
	// Targets and Labels restrict which targets notify through this channel.
	Targets []string          `yaml:"targets,omitempty"`
	Labels  map[string]string `yaml:"labels,omitempty"`
	// ChangeTypes restricts which kinds of change are delivered.
	ChangeTypes []string `yaml:"changeTypes,omitempty"`

	// Template renders the payload. Empty sends the raw event JSON.
	Template string            `yaml:"template,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty"`

	Timeout    Duration `yaml:"timeout,omitempty"`
	MaxRetries int      `yaml:"maxRetries,omitempty"`
	QueueSize  int      `yaml:"queueSize,omitempty"`
}

// Logging configures slog.
type Logging struct {
	Level  string `yaml:"level,omitempty"`
	Format string `yaml:"format,omitempty"`
}

// Metrics configures self-observability.
type Metrics struct {
	// Enabled serves Prometheus metrics on the API listener.
	Enabled bool   `yaml:"enabled,omitempty"`
	Path    string `yaml:"path,omitempty"`
}

// Duration is a YAML-friendly time.Duration that accepts "45s", "10m", "1h".
type Duration time.Duration

// UnmarshalYAML parses a duration string, reporting the line on failure.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string such as \"30s\": %w", node.Line, err)
	}

	if s == "" {
		*d = 0

		return nil
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a valid duration (use forms like \"500ms\", \"30s\", \"15m\", \"2h\")", node.Line, s)
	}

	if parsed < 0 {
		return fmt.Errorf("line %d: duration %q must not be negative", node.Line, s)
	}

	*d = Duration(parsed)

	return nil
}

// MarshalYAML renders a duration back to its string form.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Duration returns the standard library value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Or returns d, or fallback when d is zero.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}

	return time.Duration(d)
}

// UnmarshalYAML records the line a target was defined on, so validation can
// point an operator at the exact place to fix.
func (t *Target) UnmarshalYAML(node *yaml.Node) error {
	// A distinct type avoids recursing into this method.
	type plain Target

	// The strict setting of the outer decoder does not reach a type with its
	// own unmarshaller, and node.Decode has no strict mode. Re-encoding the
	// node and decoding it strictly restores the guarantee: a misspelled key
	// inside a target must be an error, not a line that silently does
	// nothing.
	raw, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("line %d: re-encoding target: %w", node.Line, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)

	var p plain

	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("target starting at line %d: %w", node.Line, err)
	}

	*t = Target(p)
	t.line = node.Line

	return nil
}

// Line reports where the target was defined in the config file.
func (t *Target) Line() int { return t.line }

// Path returns the file this config was loaded from, empty when it was built
// without a file.
func (c *Config) Path() string { return c.path }

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg.path = path

	return cfg, nil
}

// Parse decodes and validates configuration bytes.
//
// Decoding is strict: an unknown field is an error rather than a silently
// ignored line, because a misspelled key that does nothing is indistinguishable
// from a setting that does not work.
func Parse(b []byte) (*Config, error) {
	cfg := New()

	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)

	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io_EOF) {
			// An empty file is valid only if targets come from elsewhere.
			return cfg, nil
		}

		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// New returns a config with the shipped defaults applied.
func New() *Config {
	return &Config{
		Defaults: Target{
			ConsentModes: []model.ConsentMode{model.ConsentReject},
			Robots:       RobotsIgnore,
		},
		Browser: Browser{
			PoolSize:           0, // resolved from NumCPU at runtime
			MaxScansPerBrowser: 50,
		},
		Store: Store{
			MaxPerSeries: 200,
		},
		Scheduler: Scheduler{
			Interval:      Duration(24 * time.Hour),
			Jitter:        Duration(5 * time.Minute),
			MinInterval:   Duration(5 * time.Minute),
			ShutdownGrace: Duration(30 * time.Second),
		},
		Detection: Detection{
			Baseline:   BaselinePrevious,
			FlapWindow: Duration(6 * time.Hour),
		},
		API: API{
			Listen: "127.0.0.1:8712",
		},
		Logging: Logging{Level: "info", Format: "json"},
		Metrics: Metrics{Path: "/metrics"},
	}
}
