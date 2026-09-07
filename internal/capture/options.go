// Package capture records every network fetch a page performs.
//
// Capture records what the browser did; it does not judge it. Classification,
// normalization, diffing, and severity all live downstream (Tenet 3), so a
// change to interpretation can be re-applied to stored results without
// re-scanning.
package capture

import (
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Defaults for the capture budget. They exist so that a hostile or broken
// page degrades one scan rather than hanging the daemon (Story 6.6).
const (
	DefaultIdleQuiet    = 2 * time.Second
	DefaultHardTimeout  = 45 * time.Second
	DefaultMaxRequests  = 3000
	DefaultMaxBytes     = 256 << 20 // 256 MiB
	DefaultNavTimeout   = 30 * time.Second
	DefaultBodyTimeout  = 5 * time.Second
	DefaultMaxBodyBytes = 8 << 20 // 8 MiB

	// DefaultStallAfter is how long a request may be in flight before it
	// stops counting towards network idle. Cross-origin iframes are handed to
	// out-of-process targets whose completion events never reach this
	// session, so without this every page embedding one would run to its hard
	// timeout.
	DefaultStallAfter = 10 * time.Second
)

// HashResourceTypes are the resource types whose bodies are fingerprinted by
// default. Scripts are the ones that matter: a silently changed third-party
// script is the supply-chain finding wsaw exists to catch.
var HashResourceTypes = []string{"script"}

// Options is the complete capture budget and environment for one scan. Every
// applied override is echoed into the result so a scan is reproducible.
type Options struct {
	// URL is the page to load.
	URL string

	// Target is the configured target name, copied into the result.
	Target string
	// Labels are the target's labels, copied into the result.
	Labels map[string]string

	// ConsentMode is recorded in the result; capture itself does not act on
	// it. The interaction is performed by the Hooks.
	ConsentMode model.ConsentMode

	// FirstPartyDomains are additional domains treated as the site's own.
	FirstPartyDomains []string

	// Normalizer derives comparison keys. Required.
	Normalizer *normalize.Normalizer

	// IdleQuiet is how long the network must be quiet to count as idle.
	IdleQuiet time.Duration
	// HardTimeout bounds the whole capture.
	HardTimeout time.Duration
	// NavTimeout bounds the initial navigation.
	NavTimeout time.Duration

	// MaxRequests and MaxBytes cap a runaway page. Reaching either ends the
	// scan with a recorded reason rather than a crash.
	MaxRequests int
	MaxBytes    int64

	// StallAfter is how long a request may be in flight before idle detection
	// stops waiting for it. Zero means DefaultStallAfter.
	StallAfter time.Duration

	// ConsentBudget is how long the consent hook may take. Capture reserves
	// it out of HardTimeout so that a page which never reaches network idle
	// cannot consume the whole scan before the banner is ever touched.
	ConsentBudget time.Duration

	// DwellAfterLoad keeps the page open after it settles, for sites that
	// fire trackers on a timer.
	DwellAfterLoad time.Duration
	// ScrollToBottom triggers lazy-loaded assets before finishing.
	ScrollToBottom bool

	// HashResourceTypes selects which response bodies are fingerprinted.
	// Empty means HashResourceTypes.
	HashResourceTypes []string
	// MaxBodyBytes caps how much of a body is read for hashing.
	MaxBodyBytes int64
	// StoreBodies keeps raw bodies via BodySink. Off by default: bodies are
	// large and may contain personal data (Tenet 19).
	StoreBodies bool
	// BodySink receives raw bodies when StoreBodies is set. It returns an
	// opaque reference stored in the result.
	BodySink BodySink

	// Screenshots enables before/after consent evidence capture.
	Screenshots bool
	// ScreenshotSink receives screenshots.
	ScreenshotSink BodySink

	// Viewport and emulation.
	ViewportWidth  int
	ViewportHeight int
	DeviceScale    float64
	Mobile         bool

	// UserAgent overrides Chrome's own. Empty keeps the browser's default,
	// which is the honest choice (NFR §8).
	UserAgent      string
	AcceptLanguage string
	Timezone       string
	Latitude       *float64
	Longitude      *float64

	// ExtraHeaders are added to every request. Values may be secrets and are
	// never serialized into results.
	ExtraHeaders map[string]secret.Value

	// BasicAuthUser and BasicAuthPassword authenticate to the target.
	BasicAuthUser     secret.Value
	BasicAuthPassword secret.Value

	// Proxy is recorded, with credentials stripped. The browser is configured
	// with it at launch, not here.
	Proxy string

	// WarmCache opts out of the cold-cache default. Flagged in the result
	// because it changes what the numbers mean.
	WarmCache bool

	// BrowserReused records that this scan is not the first on its browser.
	BrowserReused bool

	// BrowserRuntime, BrowserImage and BrowserSandbox describe what rendered
	// the page, and are recorded in the result.
	BrowserRuntime string
	BrowserImage   string
	BrowserSandbox bool

	// WsawVersion and ChromeVersion are recorded for reproducibility.
	WsawVersion   string
	ChromeVersion string

	// Secrets scrubs registered plaintexts out of error text before it is
	// stored or logged.
	Secrets *secret.Registry
}

// BodySink stores a captured artifact and returns a reference to it.
type BodySink func(kind string, data []byte) (ref string, err error)

func (o *Options) withDefaults() Options {
	out := *o

	if out.IdleQuiet <= 0 {
		out.IdleQuiet = DefaultIdleQuiet
	}

	if out.HardTimeout <= 0 {
		out.HardTimeout = DefaultHardTimeout
	}

	if out.NavTimeout <= 0 {
		out.NavTimeout = DefaultNavTimeout
	}

	if out.MaxRequests <= 0 {
		out.MaxRequests = DefaultMaxRequests
	}

	if out.MaxBytes <= 0 {
		out.MaxBytes = DefaultMaxBytes
	}

	if out.MaxBodyBytes <= 0 {
		out.MaxBodyBytes = DefaultMaxBodyBytes
	}

	if out.StallAfter <= 0 {
		out.StallAfter = DefaultStallAfter
	}

	if len(out.HashResourceTypes) == 0 {
		out.HashResourceTypes = HashResourceTypes
	}

	if out.ViewportWidth <= 0 {
		out.ViewportWidth = 1280
	}

	if out.ViewportHeight <= 0 {
		out.ViewportHeight = 800
	}

	if out.DeviceScale <= 0 {
		out.DeviceScale = 1
	}

	return out
}

// consentReserve is the slice of the scan budget kept aside for the consent
// interaction and the settle that follows it.
func (o *Options) consentReserve() time.Duration {
	budget := o.ConsentBudget
	if budget <= 0 {
		budget = 30 * time.Second
	}

	reserve := budget + o.IdleQuiet

	if half := o.HardTimeout / 2; reserve > half {
		reserve = half
	}

	return reserve
}

// Hooks lets the caller act on the page between the initial load and the
// final capture, without capture itself knowing anything about consent.
type Hooks struct {
	// AfterLoad runs once the initial load has settled. Whatever it returns
	// is recorded as the scan's consent outcome, and every request observed
	// after it returns is marked post-interaction.
	//
	// It must respect its context: capture's budget still applies.
	AfterLoad func(ctx Context) (model.Consent, error)
}
