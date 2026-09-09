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

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
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

	// Retry of a failed scan (Story 3.8). RetryAttempts counts the first
	// attempt, so 1 disables retrying and 3 means one scan and two retries.
	RetryAttempts   int      `yaml:"retryAttempts,omitempty"`
	RetryBackoff    Duration `yaml:"retryBackoff,omitempty"`
	RetryMaxBackoff Duration `yaml:"retryMaxBackoff,omitempty"`
	// RetryTruncated also retries a scan cut short by a timeout or a cap.
	// Off by default: retrying every slow site doubles the load wsaw puts on
	// it and usually reproduces the same truncation.
	RetryTruncated *bool `yaml:"retryTruncated,omitempty"`

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

	// Runtime selects where the browser runs: auto, podman, docker or local.
	//
	// auto prefers podman, falls back to docker, and uses a local browser
	// when neither is usable. Running in a container puts a boundary the
	// operating system enforces between a hostile page and this host
	// (Story 1.8).
	Runtime string `yaml:"runtime,omitempty"`

	// Container configures the containerised browser.
	Container ContainerBrowser `yaml:"container,omitempty"`

	// ProfileDir is the parent directory for per-browser Chrome profiles.
	// Empty means the system temporary directory. Useful when /tmp is small
	// or mounted noexec.
	ProfileDir string `yaml:"profileDir,omitempty"`

	PoolSize int `yaml:"poolSize,omitempty"`

	// MaxScansPerBrowser is how many scans one browser process may serve
	// before it is replaced. The default is 1, which is what makes per-scan
	// isolation hold by construction: a browser has its own profile
	// directory, so one scan per browser means one cookie jar per scan.
	//
	// Raising it trades that guarantee for fewer browser launches. wsaw still
	// clears cookies and storage between scans and records that the browser
	// was reused in every affected result, but a site can persist state in
	// ways a clear does not reach. Raise it only where throughput matters
	// more than the consent comparison.
	MaxScansPerBrowser int64    `yaml:"maxScansPerBrowser,omitempty"`
	LaunchTimeout      Duration `yaml:"launchTimeout,omitempty"`
}

// ContainerBrowser configures the containerised browser.
type ContainerBrowser struct {
	// Image is pinned by digest by default. A floating tag would change
	// capture behaviour between scans and a diff would report the change as
	// the site's.
	Image string `yaml:"image,omitempty"`

	Memory    string `yaml:"memory,omitempty"`
	PidsLimit int    `yaml:"pidsLimit,omitempty"`

	// SHMSize sizes /dev/shm inside the container, e.g. "1g". Empty selects
	// container.DefaultSHMSize. The runtime default of 64 MB starves Chrome
	// on image-heavy pages, and the requests it then drops look like assets
	// the site stopped loading.
	SHMSize string `yaml:"shmSize,omitempty"`
	// FileDescriptors is the container's open-file limit. Zero selects
	// container.DefaultFileDescriptors.
	FileDescriptors int `yaml:"fileDescriptors,omitempty"`

	StartupTimeout Duration `yaml:"startupTimeout,omitempty"`

	// ExtraArgs are runtime arguments; BrowserArgs are appended to the
	// browser's own command line.
	ExtraArgs   []string `yaml:"extraArgs,omitempty"`
	BrowserArgs []string `yaml:"browserArgs,omitempty"`
}

// Store configures persistence.
type Store struct {
	// Driver is sqlite (the default), postgres, mysql, or blob. SQLite needs
	// no server and is what the single-binary deployment assumes; the two
	// server databases exist for a deployment that already runs one
	// (Story 4.7); blob keeps the index as objects in the artifact bucket, so
	// there is no database at all (Story 8.10).
	Driver string `yaml:"driver,omitempty"`
	// DSN is the connection string for a server database. It may be a secret
	// reference, and should be: a DSN carries a password.
	DSN string `yaml:"dsn,omitempty"`

	// MaxOpenConns and MaxIdleConns bound the connection pool for a server
	// database. Ignored for SQLite, which is deliberately serialised.
	MaxOpenConns int `yaml:"maxOpenConns,omitempty"`
	MaxIdleConns int `yaml:"maxIdleConns,omitempty"`
	// ConnMaxLifetime retires a pooled connection before the server does.
	ConnMaxLifetime Duration `yaml:"connMaxLifetime,omitempty"`

	// MaxAttempts is how many times a store operation is tried when the
	// failure is transient — a dropped connection, a restarted server, a
	// deadlock. 1 disables retrying; empty takes the default.
	MaxAttempts int `yaml:"maxAttempts,omitempty"`
	// RetryBackoff is the delay before the second attempt, doubling after
	// that.
	RetryBackoff Duration `yaml:"retryBackoff,omitempty"`

	Path string `yaml:"path,omitempty"`

	// ArtifactDir is a directory on local disk holding the evidence:
	// screenshots, stored bodies, and every result document. Empty puts it
	// beside the database, which is what the single-binary deployment wants
	// and why it needs no configuration (Tenet 14).
	ArtifactDir string `yaml:"artifactDir,omitempty"`
	// ArtifactURL names an object-storage bucket to hold the same evidence
	// instead, as a URL — "s3://bucket", "gs://bucket", "azblob://container".
	// It exists so that pointing wsaw at storage an organisation already runs
	// is one line rather than a second implementation of everything
	// (Story 8.6, AC1).
	//
	// Exactly one of the two is set. Setting both is refused rather than
	// resolved by preferring one, because an operator who wrote both has two
	// ideas about where their evidence is and only one of them is true.
	//
	// A credential in the URL is a secret reference and is redacted wherever
	// wsaw prints it, but the provider's own credential chain is the way this
	// is meant to be authenticated (AC3).
	ArtifactURL string `yaml:"artifactURL,omitempty"`

	// ArtifactSignedURLs lets wsaw answer a request for a screenshot or a
	// stored body with a redirect to the bucket instead of copying the bytes
	// through itself.
	//
	// Off by default, and deliberately: a redirect moves access control from
	// wsaw — which checks the API token, or that a share link actually covers
	// the file being asked for — to a URL that anyone holding it can replay
	// until it expires. That is a decision about who guards the evidence, not
	// an optimisation, so it is taken by an operator rather than by wsaw
	// (Story 8.7, AC3).
	ArtifactSignedURLs bool `yaml:"artifactSignedURLs,omitempty"`
	// ArtifactSignedURLTTL is how long such a redirect stays usable. Empty
	// takes DefaultArtifactSignedURLTTL, and MaxArtifactSignedURLTTL is the
	// ceiling: a signed URL cannot be withdrawn before it expires, so a long
	// one is a standing grant on evidence rather than a link to it.
	//
	// A redirect issued from a share link is additionally cut to what is left
	// of that link, because a reader must not come away with access outliving
	// the link that gave it to them.
	ArtifactSignedURLTTL Duration `yaml:"artifactSignedURLTTL,omitempty"`

	OutputDir    string   `yaml:"outputDir,omitempty"`
	MaxAge       Duration `yaml:"maxAge,omitempty"`
	MaxPerSeries int      `yaml:"maxPerSeries,omitempty"`

	// WriteJSONL mirrors results to newline-delimited JSON files.
	WriteJSONL bool `yaml:"writeJsonl,omitempty"`
	// WriteHAR emits a HAR file per scan. Off by default because of size.
	WriteHAR bool `yaml:"writeHar,omitempty"`
	// WriteReport emits a Markdown report per scan.
	WriteReport bool `yaml:"writeReport,omitempty"`

	// lines records which line each store setting was written on, so a
	// validation failure can point at the line to change (Tenet 15).
	lines map[string]int
}

// UnmarshalYAML decodes the store section and records where each of its
// settings was written.
//
// The line matters here for the same reason it matters for a target: two
// settings that contradict each other — a directory and a bucket URL — are
// fixed by editing one line, and an operator should be told which one rather
// than being sent to search the file (Story 8.6, AC4).
func (s *Store) UnmarshalYAML(node *yaml.Node) error {
	// A distinct type avoids recursing into this method.
	type plain Store

	// The outer decoder's strict setting does not reach a type with its own
	// unmarshaller, and node.Decode has no strict mode. Re-encoding the node
	// and decoding it strictly restores the guarantee, exactly as Target does:
	// a misspelled store setting must be an error, not a line that silently
	// does nothing.
	raw, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("line %d: re-encoding the store section: %w", node.Line, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)

	var p plain

	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("store section starting at line %d: %w", node.Line, err)
	}

	*s = Store(p)
	s.lines = keyLines(node)

	return nil
}

// keyLines maps each key of a mapping node to the line it was written on.
func keyLines(node *yaml.Node) map[string]int {
	if node.Kind != yaml.MappingNode {
		return nil
	}

	lines := make(map[string]int, len(node.Content)/2)

	for i := 0; i+1 < len(node.Content); i += 2 {
		lines[node.Content[i].Value] = node.Content[i].Line
	}

	return lines
}

// Line reports the line a store setting was written on, and zero when the
// configuration did not come from a file — which is why every message that
// uses it must still read correctly without a line.
func (s Store) Line(key string) int { return s.lines[key] }

// ArtifactLocation returns where the evidence goes, resolved from the two
// settings that can name it. Empty means nothing was configured, and the
// caller supplies the default beside the database (Story 8.6, AC1).
//
// Both being set is a validation failure, so this preferring one is not a
// silent resolution of the conflict: nothing reaches this method until
// Validate has refused that configuration.
func (s Store) ArtifactLocation() string {
	if s.ArtifactURL != "" {
		return s.ArtifactURL
	}

	return s.ArtifactDir
}

// Signed artifact URL bounds (Story 8.7, AC3).
//
// Five minutes is long enough for a browser to follow a redirect and load the
// image behind it, including a page holding a dozen of them, and short enough
// that a URL copied out of a network tab is worthless by the time anyone
// looks. The hour is a ceiling rather than a suggestion: a signed URL cannot
// be revoked, so its lifetime is the window in which evidence is readable by
// whoever holds the string.
const (
	DefaultArtifactSignedURLTTL = 5 * time.Minute
	MaxArtifactSignedURLTTL     = time.Hour
)

// SignedURLTTL returns how long a signed artifact URL may live, or the
// default.
func (s Store) SignedURLTTL() time.Duration {
	return s.ArtifactSignedURLTTL.Or(DefaultArtifactSignedURLTTL)
}

// StoreDriver returns the configured driver, defaulting to SQLite.
func (s Store) StoreDriver() string {
	if s.Driver == "" {
		return store.DriverSQLite
	}

	return s.Driver
}

// IsServerStore reports whether the store is a database with a server, which
// is what decides whether a DSN is required and a path is meaningless.
//
// It names the two drivers rather than everything that is not SQLite, which is
// what it used to do. Since Story 8.10 there is a driver that is neither: blob
// keeps no rows anywhere, so it needs no DSN and would have been asked for one.
func (s Store) IsServerStore() bool {
	switch s.StoreDriver() {
	case store.DriverPostgres, store.DriverMySQL:
		return true
	default:
		return false
	}
}

// IsBucketStore reports whether the store keeps its index in the artifact
// bucket rather than in rows (Story 8.10).
//
// It is the question that decides two things nothing else decides: that an
// artifact location is required rather than derived, because the bucket is the
// store and not somewhere its evidence goes, and that every setting describing
// a database — a DSN, a file path, a connection pool — means nothing here and
// is refused rather than ignored.
func (s Store) IsBucketStore() bool { return s.StoreDriver() == store.DriverBlob }

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

	// BodyIdentities compare a script by a version it publishes about itself
	// instead of by the digest of its bytes.
	BodyIdentities []BodyIdentity `yaml:"bodyIdentity,omitempty"`

	// UseDefaultDropParams adds the shipped list of noise parameters.
	UseDefaultDropParams *bool `yaml:"useDefaultDropParams,omitempty"`
}

// BodyIdentity extracts a stable identifier out of a response body.
//
// It is for scripts whose bytes change more often than their content does. A
// tag-manager container folds experiment flags into every response, so its
// digest moves on almost every scan while the container version it declares
// stays put — and it is the version an operator needs to hear about.
type BodyIdentity struct {
	// URLPattern selects the requests the rule applies to, matched against
	// the raw URL.
	URLPattern string `yaml:"urlPattern"`
	// Extract is a regular expression with exactly one capturing group.
	Extract string `yaml:"extract"`
	// Label names the identity in reports, e.g. "GTM container version".
	Label string `yaml:"label,omitempty"`
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

	// DegradedFailureRatio is the share of a scan's requests that may fail
	// for capture reasons before its asset list stops being trusted for
	// removals. Zero selects diff.DefaultDegradedFailureRatio; a value above
	// 1 disables the check.
	DegradedFailureRatio float64 `yaml:"degradedFailureRatio,omitempty"`

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

	// Share configures the expiring links that let somebody read one result
	// without this token (Story 5.19).
	Share Share `yaml:"share,omitempty"`

	// RefreshInterval is how often the interface reloads itself for a viewer
	// who has expressed no preference; zero means not at all. It is only the
	// default — the choice belongs to whoever is looking, and is made on the
	// page (Story 5.16).
	RefreshInterval Duration `yaml:"refreshInterval,omitempty"`
	// ReadOnly disables every write action, for shared reviewer deployments.
	ReadOnly bool `yaml:"readOnly,omitempty"`
	// AllowAdHocScan permits triggering scans through the API.
	AllowAdHocScan *bool `yaml:"allowAdHocScan,omitempty"`
}

// Share configures result sharing by expiring link (Story 5.19).
//
// Off until configured: a link publishes data that can be personal to whoever
// holds it, and it cannot be revoked before it expires (NFR §4, Tenet 19).
type Share struct {
	Enabled bool `yaml:"enabled,omitempty"`

	// Key signs the links. It may be a secret reference, and should be — it
	// is a credential, and deliberately not the API token: sharing must not
	// be derivable from admin access, and withdrawing sharing must not mean
	// rotating the operator's own credential.
	Key string `yaml:"key,omitempty"`

	// Validity is how long a link lasts by default, and MaxValidity the
	// longest any request may ask for.
	Validity    Duration `yaml:"validity,omitempty"`
	MaxValidity Duration `yaml:"maxValidity,omitempty"`

	// BaseURL is where this wsaw is reachable from, so a minted link is
	// something an operator can copy and send rather than assemble.
	BaseURL string `yaml:"baseUrl,omitempty"`
}

// Share defaults. A week is long enough for a review round and short enough
// that a forgotten link stops working.
const (
	DefaultShareValidity    = 7 * 24 * time.Hour
	DefaultShareMaxValidity = 30 * 24 * time.Hour
)

// ShareValidity returns the configured default validity, or the default.
func (s Share) ShareValidity() time.Duration {
	return s.Validity.Or(DefaultShareValidity)
}

// ShareMaxValidity returns the configured maximum, or the default.
func (s Share) ShareMaxValidity() time.Duration {
	return s.MaxValidity.Or(DefaultShareMaxValidity)
}

// Notifier kinds.
const (
	// NotifyWebhook posts one JSON event per change, optionally templated.
	NotifyWebhook = "webhook"
	// NotifyTeams posts one Adaptive Card per scan to a Power Automate
	// Workflow webhook.
	NotifyTeams = "teams"
)

// Teams card formats.
const (
	// TeamsAdaptive is the current format, an Adaptive Card in a message
	// envelope.
	TeamsAdaptive = "adaptive"
	// TeamsMessageCard is the retired connector format, kept for tenants that
	// still have a working connector webhook.
	TeamsMessageCard = "messagecard"
)

// Notifier delivers change events.
type Notifier struct {
	Name string `yaml:"name"`
	// Kind selects the notifier: "webhook" (the default) or "teams".
	Kind string `yaml:"kind,omitempty"`
	// URL may be a secret reference, since webhook URLs carry tokens.
	URL string `yaml:"url"`

	// MinSeverity filters what is delivered.
	MinSeverity string `yaml:"minSeverity,omitempty"`
	// Targets and Labels restrict which targets notify through this channel.
	Targets []string          `yaml:"targets,omitempty"`
	Labels  map[string]string `yaml:"labels,omitempty"`
	// ChangeTypes restricts which kinds of change are delivered.
	ChangeTypes []string `yaml:"changeTypes,omitempty"`

	// Template renders the payload. Empty sends the raw event JSON. Webhook
	// notifiers only: a Teams card is built in Go because a malformed one
	// fails silently.
	Template string            `yaml:"template,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty"`

	// Format selects the Teams card format: "adaptive" (the default) or the
	// deprecated "messagecard".
	Format string `yaml:"format,omitempty"`
	// BaseURL is wsaw's externally reachable web interface, used to link a
	// card back to the full result.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// MaxChanges caps how many changes one card lists before it reports the
	// rest as a count.
	MaxChanges int `yaml:"maxChanges,omitempty"`

	Timeout    Duration `yaml:"timeout,omitempty"`
	MaxRetries int      `yaml:"maxRetries,omitempty"`
	QueueSize  int      `yaml:"queueSize,omitempty"`
}

// NotifierKind returns the configured kind, defaulting to a plain webhook.
func (n Notifier) NotifierKind() string {
	if n.Kind == "" {
		return NotifyWebhook
	}

	return n.Kind
}

// TeamsFormat returns the configured card format, defaulting to Adaptive.
func (n Notifier) TeamsFormat() string {
	if n.Format == "" {
		return TeamsAdaptive
	}

	return n.Format
}

// Logging configures slog.
type Logging struct {
	Level string `yaml:"level,omitempty"`
	// Format is auto, pretty, json or text. auto resolves to pretty on an
	// interactive terminal and json otherwise.
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
			PoolSize: 0, // resolved from NumCPU at runtime
			// One scan per browser: see the field comment. Consent state
			// leaking between scans invalidates every comparison wsaw makes.
			MaxScansPerBrowser: 1,
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
		// auto keeps machine-readable output wherever something is collecting
		// it, and readable output when a person is watching (Story 6.9).
		Logging: Logging{Level: "info", Format: "auto"},
		Metrics: Metrics{Path: "/metrics"},
	}
}
