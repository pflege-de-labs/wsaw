package config

import (
	"cmp"
	"fmt"
	"runtime"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/retry"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

const defaultInterval = 24 * time.Hour

// Retry defaults (Story 3.8).
//
// Two attempts rather than one, because a watcher that gives up on a single
// dropped connection leaves a hole in a target's history until the next
// interval — up to a day. Two rather than five, because a target that is
// genuinely broken should be reported as broken rather than hammered.
const (
	DefaultRetryAttempts   = 2
	DefaultRetryBackoff    = 60 * time.Second
	DefaultRetryMaxBackoff = 10 * time.Minute
)

// Resolved is one target with every default already applied, so downstream
// code never has to ask "was this set globally or per target?".
type Resolved struct {
	Name   string
	URL    string
	Labels map[string]string

	ConsentModes []model.ConsentMode

	Interval time.Duration
	Cron     string

	FirstPartyDomains []string

	Allow diff.HostList
	Deny  diff.HostList

	IdleQuiet      time.Duration
	HardTimeout    time.Duration
	NavTimeout     time.Duration
	MaxRequests    int
	MaxBytes       int64
	DwellAfterLoad time.Duration
	// ConsentBannerWait bounds the wait for a banner to appear before the
	// scan concludes the page has none.
	ConsentBannerWait time.Duration
	ScrollToBottom    bool

	// Beacons are the requests idle detection must not wait for: the shipped
	// list unless it was opted out of, plus the global and per-target rules
	// (Story 1.10).
	Beacons []capture.Beacon

	ViewportWidth  int
	ViewportHeight int
	DeviceScale    float64
	Mobile         bool
	UserAgent      string
	AcceptLanguage string
	Timezone       string
	Latitude       *float64
	Longitude      *float64

	ExtraHeaders map[string]secret.Value

	BasicAuthUser     secret.Value
	BasicAuthPassword secret.Value

	FormLogin *capture.FormLogin

	Proxy string

	Screenshots bool
	// Bodies is which bodies a sampled scan stores, and how often a scan is
	// sampled (Story 1.11).
	Bodies BodyPolicy

	Robots      RobotsPolicy
	MinInterval time.Duration
	Jitter      time.Duration

	// Retry is what to do when a scan produces no usable observation
	// (Story 3.8).
	Retry retry.Policy

	Severity diff.SeverityRules
}

// ResolveTargets applies defaults to every enabled target and resolves secret
// references. Resolution happens once at load, so a missing environment
// variable fails at startup rather than mid-scan.
func (c *Config) ResolveTargets(reg *secret.Registry) ([]Resolved, error) {
	out := make([]Resolved, 0, len(c.Targets))

	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Disabled {
			continue
		}

		r, err := c.resolveTarget(t, reg)
		if err != nil {
			return nil, fmt.Errorf("target %q (line %d): %w", t.Name, t.line, err)
		}

		out = append(out, r)
	}

	return out, nil
}

// ResolveAdHocTarget resolves a target that is not in the configuration file:
// a URL somebody typed into the web interface (Story 5.27).
//
// It is the resolution every configured target gets, minus the
// credential-bearing defaults. defaults.basicAuthUser,
// defaults.basicAuthPassword and defaults.extraHeaders exist so that wsaw can
// reach the operator's own sites; sending them to an address somebody typed
// would hand those credentials to whoever typed it.
//
// Those references are not resolved and then dropped: they are never read on
// this path at all, so it takes no secret registry and cannot fail. Resolving
// them here would put the reference — an environment variable's name, a key
// file's path — into an error that this path reports to whoever typed the
// URL, which is a second way to leak a credential the scan was never going to
// send.
func (c *Config) ResolveAdHocTarget(name, rawURL string, mode model.ConsentMode) Resolved {
	t := &Target{Name: name, URL: rawURL, ConsentModes: []model.ConsentMode{mode}}

	return c.resolveTargetFields(t)
}

func (c *Config) resolveTarget(t *Target, reg *secret.Registry) (Resolved, error) {
	r := c.resolveTargetFields(t)
	d := c.Defaults

	headers, err := resolveHeaders(d.ExtraHeaders, t.ExtraHeaders, reg)
	if err != nil {
		return Resolved{}, err
	}

	r.ExtraHeaders = headers

	r.BasicAuthUser, err = resolveSecret(cmp.Or(t.BasicAuthUser, d.BasicAuthUser), "basicAuthUser", reg)
	if err != nil {
		return Resolved{}, err
	}

	r.BasicAuthPassword, err = resolveSecret(cmp.Or(t.BasicAuthPassword, d.BasicAuthPassword), "basicAuthPassword", reg)
	if err != nil {
		return Resolved{}, err
	}

	r.FormLogin, err = resolveFormLogin(t.FormLogin, reg)
	if err != nil {
		return Resolved{}, err
	}

	return r, nil
}

// resolveFormLogin resolves every field as a secret, since which of a form's
// fields is the password is the form's business, not wsaw's.
func resolveFormLogin(l *FormLogin, reg *secret.Registry) (*capture.FormLogin, error) {
	if l == nil {
		return nil, nil
	}

	fields := make(map[string]secret.Value, len(l.Fields))

	for name, ref := range l.Fields {
		v, err := resolveSecret(ref, "formLogin.fields."+name, reg)
		if err != nil {
			return nil, err
		}

		fields[name] = v
	}

	return &capture.FormLogin{URL: l.URL, Fields: fields}, nil
}

// resolveTargetFields applies defaults to everything that is not a credential.
// It reads no secret reference, so it cannot fail.
//
//nolint:gocognit // a wide struct of independent overrides; splitting it would obscure the mapping
func (c *Config) resolveTargetFields(t *Target) Resolved {
	d := c.Defaults

	r := Resolved{
		Name:              t.Name,
		URL:               t.URL,
		Labels:            mergeLabels(d.Labels, t.Labels),
		ConsentModes:      firstModes(t.ConsentModes, d.ConsentModes),
		Cron:              cmp.Or(t.Cron, d.Cron),
		FirstPartyDomains: append(append([]string{}, d.FirstPartyDomains...), t.FirstPartyDomains...),
		IdleQuiet:         firstDuration(t.IdleQuiet, d.IdleQuiet, capture.DefaultIdleQuiet),
		HardTimeout:       firstDuration(t.HardTimeout, d.HardTimeout, capture.DefaultHardTimeout),
		NavTimeout:        firstDuration(t.NavTimeout, d.NavTimeout, capture.DefaultNavTimeout),
		MaxRequests:       firstInt(t.MaxRequests, d.MaxRequests, capture.DefaultMaxRequests),
		MaxBytes:          firstInt64(t.MaxBytes, d.MaxBytes, capture.DefaultMaxBytes),
		DwellAfterLoad:    firstDuration(t.DwellAfterLoad, d.DwellAfterLoad, 0),
		ConsentBannerWait: firstDuration(t.ConsentBannerWait, d.ConsentBannerWait,
			c.Consent.BannerWait.Or(consent.DefaultBannerWait)),
		ScrollToBottom: firstBool(t.ScrollToBottom, d.ScrollToBottom, false),
		Beacons:        c.beaconRules(t),
		ViewportWidth:  firstInt(t.ViewportWidth, d.ViewportWidth, 1280),
		ViewportHeight: firstInt(t.ViewportHeight, d.ViewportHeight, 800),
		DeviceScale:    firstFloat(t.DeviceScale, d.DeviceScale, 1),
		Mobile:         firstBool(t.Mobile, d.Mobile, false),
		UserAgent:      cmp.Or(t.UserAgent, d.UserAgent),
		AcceptLanguage: cmp.Or(t.AcceptLanguage, d.AcceptLanguage),
		Timezone:       cmp.Or(t.Timezone, d.Timezone),
		Proxy:          cmp.Or(t.Proxy, d.Proxy),
		Screenshots:    firstBool(t.Screenshots, d.Screenshots, false),
		Bodies:         c.bodyPolicy(t),
		Robots:         cmp.Or(t.Robots, d.Robots, RobotsIgnore),
		MinInterval:    firstDuration(t.MinInterval, d.MinInterval, c.Scheduler.MinInterval.Or(5*time.Minute)),
		Jitter:         firstDuration(t.Jitter, d.Jitter, c.Scheduler.Jitter.Or(0)),
		Retry: retry.Policy{
			Attempts:   firstInt(t.RetryAttempts, d.RetryAttempts, DefaultRetryAttempts),
			Backoff:    firstDuration(t.RetryBackoff, d.RetryBackoff, DefaultRetryBackoff),
			MaxBackoff: firstDuration(t.RetryMaxBackoff, d.RetryMaxBackoff, DefaultRetryMaxBackoff),
			Truncated:  firstBool(t.RetryTruncated, d.RetryTruncated, false),
		},
		Severity: mergeSeverity(c.Detection.Severity, d.Severity, t.Severity),
	}

	// An explicit target cron clears an inherited interval, and vice versa,
	// so the two schedule kinds cannot silently combine.
	switch {
	case t.Cron != "":
		r.Cron = t.Cron
	case t.Interval > 0:
		r.Interval = t.Interval.Duration()
		r.Cron = ""
	case d.Cron != "":
		r.Cron = d.Cron
	case d.Interval > 0:
		r.Interval = d.Interval.Duration()
	case c.Scheduler.Cron != "":
		r.Cron = c.Scheduler.Cron
	default:
		r.Interval = c.Scheduler.Interval.Or(defaultInterval)
	}

	if t.Latitude != nil {
		r.Latitude, r.Longitude = t.Latitude, t.Longitude
	} else if d.Latitude != nil {
		r.Latitude, r.Longitude = d.Latitude, d.Longitude
	}

	r.Allow = diff.NewHostList(concat(c.Detection.AllowHosts, d.AllowHosts, t.AllowHosts))
	r.Deny = diff.NewHostList(concat(c.Detection.DenyHosts, d.DenyHosts, t.DenyHosts))

	return r
}

func resolveHeaders(defaults, overrides map[string]string, reg *secret.Registry) (map[string]secret.Value, error) {
	merged := make(map[string]string, len(defaults)+len(overrides))

	for k, v := range defaults {
		merged[k] = v
	}

	for k, v := range overrides {
		merged[k] = v
	}

	if len(merged) == 0 {
		return nil, nil
	}

	out := make(map[string]secret.Value, len(merged))

	for name, ref := range merged {
		v, err := resolveSecret(ref, "extraHeaders."+name, reg)
		if err != nil {
			return nil, err
		}

		out[name] = v
	}

	return out, nil
}

func resolveSecret(ref, field string, reg *secret.Registry) (secret.Value, error) {
	v, err := secret.Resolve(ref)
	if err != nil {
		return secret.Value{}, fmt.Errorf("%s: %w", field, err)
	}

	if reg != nil {
		reg.Add(v)
	}

	return v, nil
}

// NormalizeRules builds the comparison rules from configuration.
func (c *Config) NormalizeRules() normalize.Rules {
	r := normalize.Rules{
		KeepQueryParams:   c.Normalize.KeepQueryParams,
		DropAllQuery:      c.Normalize.DropAllQuery,
		DropTrailingSlash: c.Normalize.DropTrailingSlash,
	}

	drop := c.Normalize.DropQueryParams

	// The shipped noise list is opt-out rather than opt-in: without it, the
	// first scan of almost any real site produces a wall of cache-buster
	// churn, and an operator learns to ignore the output.
	if c.Normalize.UseDefaultDropParams == nil || *c.Normalize.UseDefaultDropParams {
		drop = append(append([]string{}, normalize.DefaultDropQueryParams...), drop...)
	}

	r.DropQueryParams = drop

	for _, rep := range c.Normalize.PathReplacements {
		r.PathReplacements = append(r.PathReplacements, normalize.Replacement{
			Pattern: rep.Pattern,
			With:    rep.With,
		})
	}

	for _, q := range c.Normalize.QueryRules {
		r.QueryRules = append(r.QueryRules, normalize.QueryRule{
			URLPattern:      q.URLPattern,
			Party:           q.Party,
			KeepQueryParams: q.KeepQueryParams,
			DropQueryParams: q.DropQueryParams,
			DropAllQuery:    q.DropAllQuery,
		})
	}

	// Configured rules come first, so an operator can override a shipped
	// rule for one endpoint without opting out of all of them. Opt-out for
	// the same reason as the drop list: without them, every tracking pixel a
	// site fires reports as a new asset on every scan.
	if c.Normalize.UseDefaultQueryRules == nil || *c.Normalize.UseDefaultQueryRules {
		r.QueryRules = append(r.QueryRules, normalize.DefaultQueryRules...)
	}

	for _, id := range c.Normalize.BodyIdentities {
		r.BodyIdentities = append(r.BodyIdentities, normalize.BodyIdentity{
			URLPattern: id.URLPattern,
			Extract:    id.Extract,
			Label:      id.Label,
		})
	}

	for _, v := range c.Normalize.VolatileBodies {
		r.VolatileBodies = append(r.VolatileBodies, normalize.VolatileBody{URLPattern: v.URLPattern})
	}

	// Opt-out, because the shipped query rules are what make these responses
	// meet: without this list, turning those rules on trades an added and a
	// removed asset per visit for a changed script per visit.
	if c.Normalize.UseDefaultVolatileBodies == nil || *c.Normalize.UseDefaultVolatileBodies {
		r.VolatileBodies = append(r.VolatileBodies, normalize.DefaultVolatileBodies...)
	}

	return r
}

// Concurrency resolves the worker count, defaulting from the machine.
func (c *Config) Concurrency() int {
	if c.Scheduler.Concurrency > 0 {
		return c.Scheduler.Concurrency
	}

	// Chrome is memory-hungry, so the default is deliberately below NumCPU.
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}

	if n > 8 {
		n = 8
	}

	return n
}

// PoolSize resolves the browser pool size.
func (c *Config) PoolSize() int {
	if c.Browser.PoolSize > 0 {
		return c.Browser.PoolSize
	}

	return c.Concurrency()
}

func mergeLabels(defaults, overrides map[string]string) map[string]string {
	if len(defaults) == 0 && len(overrides) == 0 {
		return nil
	}

	out := make(map[string]string, len(defaults)+len(overrides))

	for k, v := range defaults {
		out[k] = v
	}

	for k, v := range overrides {
		out[k] = v
	}

	return out
}

func mergeSeverity(global, defaults, target diff.SeverityRules) diff.SeverityRules {
	return target.Over(defaults.Over(global))
}

// beaconRules collects every rule that applies to a target, in the order the
// shipped defaults, the global list and the target's own additions were
// written. Rules add up rather than replace: a site's own session ping is
// expressible per target without restating the shared list.
func (c *Config) beaconRules(t *Target) []capture.Beacon {
	var out []capture.Beacon

	// The shipped list is opt-out rather than opt-in: without it the first
	// scan of an ordinary commercial site ends on its hard timeout, and a
	// truncated result that says nothing about the site teaches an operator
	// to ignore the outcome.
	if c.Capture.UseDefaultBeacons == nil || *c.Capture.UseDefaultBeacons {
		out = append(out, capture.DefaultBeacons...)
	}

	for _, list := range [][]Beacon{c.Capture.Beacons, c.Defaults.Beacons, t.Beacons} {
		for _, b := range list {
			out = append(out, capture.Beacon{Host: b.Host, URLPattern: b.URLPattern})
		}
	}

	return out
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}

	return out
}

func firstModes(a, b []model.ConsentMode) []model.ConsentMode {
	if len(a) > 0 {
		return a
	}

	if len(b) > 0 {
		return b
	}

	return []model.ConsentMode{model.ConsentReject}
}

func firstDuration(a, b Duration, fallback time.Duration) time.Duration {
	if a > 0 {
		return a.Duration()
	}

	if b > 0 {
		return b.Duration()
	}

	return fallback
}

func firstInt(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}

	return 0
}

func firstInt64(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}

	return 0
}

func firstFloat(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}

	return 0
}

func firstBool(a, b *bool, fallback bool) bool {
	if a != nil {
		return *a
	}

	if b != nil {
		return *b
	}

	return fallback
}
