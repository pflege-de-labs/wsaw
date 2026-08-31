package config

import (
	"fmt"
	"runtime"
	"time"

	"github.com/martint17r/wsaw/internal/capture"
	"github.com/martint17r/wsaw/internal/diff"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/normalize"
	"github.com/martint17r/wsaw/internal/secret"
)

const defaultInterval = 24 * time.Hour

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
	ScrollToBottom bool

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

	Proxy string

	WarmCache   bool
	Screenshots bool
	StoreBodies bool

	Robots      RobotsPolicy
	MinInterval time.Duration
	Jitter      time.Duration

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

//nolint:gocognit // a wide struct of independent overrides; splitting it would obscure the mapping
func (c *Config) resolveTarget(t *Target, reg *secret.Registry) (Resolved, error) {
	d := c.Defaults

	r := Resolved{
		Name:              t.Name,
		URL:               t.URL,
		Labels:            mergeLabels(d.Labels, t.Labels),
		ConsentModes:      firstModes(t.ConsentModes, d.ConsentModes),
		Cron:              firstString(t.Cron, d.Cron),
		FirstPartyDomains: append(append([]string{}, d.FirstPartyDomains...), t.FirstPartyDomains...),
		IdleQuiet:         firstDuration(t.IdleQuiet, d.IdleQuiet, capture.DefaultIdleQuiet),
		HardTimeout:       firstDuration(t.HardTimeout, d.HardTimeout, capture.DefaultHardTimeout),
		NavTimeout:        firstDuration(t.NavTimeout, d.NavTimeout, capture.DefaultNavTimeout),
		MaxRequests:       firstInt(t.MaxRequests, d.MaxRequests, capture.DefaultMaxRequests),
		MaxBytes:          firstInt64(t.MaxBytes, d.MaxBytes, capture.DefaultMaxBytes),
		DwellAfterLoad:    firstDuration(t.DwellAfterLoad, d.DwellAfterLoad, 0),
		ScrollToBottom:    firstBool(t.ScrollToBottom, d.ScrollToBottom, false),
		ViewportWidth:     firstInt(t.ViewportWidth, d.ViewportWidth, 1280),
		ViewportHeight:    firstInt(t.ViewportHeight, d.ViewportHeight, 800),
		DeviceScale:       firstFloat(t.DeviceScale, d.DeviceScale, 1),
		Mobile:            firstBool(t.Mobile, d.Mobile, false),
		UserAgent:         firstString(t.UserAgent, d.UserAgent),
		AcceptLanguage:    firstString(t.AcceptLanguage, d.AcceptLanguage),
		Timezone:          firstString(t.Timezone, d.Timezone),
		Proxy:             firstString(t.Proxy, d.Proxy),
		WarmCache:         firstBool(t.WarmCache, d.WarmCache, false),
		Screenshots:       firstBool(t.Screenshots, d.Screenshots, false),
		StoreBodies:       firstBool(t.StoreBodies, d.StoreBodies, false),
		Robots:            firstRobots(t.Robots, d.Robots),
		MinInterval:       firstDuration(t.MinInterval, d.MinInterval, c.Scheduler.MinInterval.Or(5*time.Minute)),
		Jitter:            firstDuration(t.Jitter, d.Jitter, c.Scheduler.Jitter.Or(0)),
		Severity:          mergeSeverity(c.Detection.Severity, d.Severity, t.Severity),
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

	headers, err := resolveHeaders(d.ExtraHeaders, t.ExtraHeaders, reg)
	if err != nil {
		return Resolved{}, err
	}

	r.ExtraHeaders = headers

	r.BasicAuthUser, err = resolveSecret(firstString(t.BasicAuthUser, d.BasicAuthUser), "basicAuthUser", reg)
	if err != nil {
		return Resolved{}, err
	}

	r.BasicAuthPassword, err = resolveSecret(firstString(t.BasicAuthPassword, d.BasicAuthPassword), "basicAuthPassword", reg)
	if err != nil {
		return Resolved{}, err
	}

	return r, nil
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
func (c *Config) NormalizeRules() (normalize.Rules, error) {
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

	return r, nil
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
	pick := func(a, b, c diff.Severity) diff.Severity {
		switch {
		case c != "":
			return c
		case b != "":
			return b
		default:
			return a
		}
	}

	return diff.SeverityRules{
		ThirdPartyHostRejectMode:   pick(global.ThirdPartyHostRejectMode, defaults.ThirdPartyHostRejectMode, target.ThirdPartyHostRejectMode),
		ThirdPartyHostPreConsent:   pick(global.ThirdPartyHostPreConsent, defaults.ThirdPartyHostPreConsent, target.ThirdPartyHostPreConsent),
		ThirdPartyHostAdded:        pick(global.ThirdPartyHostAdded, defaults.ThirdPartyHostAdded, target.ThirdPartyHostAdded),
		FirstPartyHostAdded:        pick(global.FirstPartyHostAdded, defaults.FirstPartyHostAdded, target.FirstPartyHostAdded),
		HostRemoved:                pick(global.HostRemoved, defaults.HostRemoved, target.HostRemoved),
		ThirdPartyScriptAdded:      pick(global.ThirdPartyScriptAdded, defaults.ThirdPartyScriptAdded, target.ThirdPartyScriptAdded),
		ThirdPartyAssetAdded:       pick(global.ThirdPartyAssetAdded, defaults.ThirdPartyAssetAdded, target.ThirdPartyAssetAdded),
		FirstPartyScriptAdded:      pick(global.FirstPartyScriptAdded, defaults.FirstPartyScriptAdded, target.FirstPartyScriptAdded),
		FirstPartyAssetAdded:       pick(global.FirstPartyAssetAdded, defaults.FirstPartyAssetAdded, target.FirstPartyAssetAdded),
		AssetRemoved:               pick(global.AssetRemoved, defaults.AssetRemoved, target.AssetRemoved),
		ThirdPartyScriptChanged:    pick(global.ThirdPartyScriptChanged, defaults.ThirdPartyScriptChanged, target.ThirdPartyScriptChanged),
		FirstPartyScriptChanged:    pick(global.FirstPartyScriptChanged, defaults.FirstPartyScriptChanged, target.FirstPartyScriptChanged),
		ThirdPartyCookieRejectMode: pick(global.ThirdPartyCookieRejectMode, defaults.ThirdPartyCookieRejectMode, target.ThirdPartyCookieRejectMode),
		ThirdPartyCookieAdded:      pick(global.ThirdPartyCookieAdded, defaults.ThirdPartyCookieAdded, target.ThirdPartyCookieAdded),
		FirstPartyCookieAdded:      pick(global.FirstPartyCookieAdded, defaults.FirstPartyCookieAdded, target.FirstPartyCookieAdded),
		CookieRemoved:              pick(global.CookieRemoved, defaults.CookieRemoved, target.CookieRemoved),
		StatusBecameError:          pick(global.StatusBecameError, defaults.StatusBecameError, target.StatusBecameError),
		StatusChanged:              pick(global.StatusChanged, defaults.StatusChanged, target.StatusChanged),
		ConsentChanged:             pick(global.ConsentChanged, defaults.ConsentChanged, target.ConsentChanged),
		ConsentDegraded:            pick(global.ConsentDegraded, defaults.ConsentDegraded, target.ConsentDegraded),
		DeniedHost:                 pick(global.DeniedHost, defaults.DeniedHost, target.DeniedHost),
		ScanDegraded:               pick(global.ScanDegraded, defaults.ScanDegraded, target.ScanDegraded),
	}
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

func firstString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}

	return ""
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

func firstRobots(vals ...RobotsPolicy) RobotsPolicy {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}

	return RobotsIgnore
}
