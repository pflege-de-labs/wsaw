package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/martint17r/wsaw/internal/diff"
	"github.com/martint17r/wsaw/internal/model"
)

// io_EOF is aliased so Parse can recognize an empty document without
// importing io at the call site.
var io_EOF = io.EOF //nolint:revive,stylecheck // deliberate local alias

// Error is a validation failure that points at a line.
type Error struct {
	Line    int
	Field   string
	Message string
}

func (e *Error) Error() string {
	switch {
	case e.Line > 0 && e.Field != "":
		return fmt.Sprintf("line %d: %s: %s", e.Line, e.Field, e.Message)
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	case e.Field != "":
		return fmt.Sprintf("%s: %s", e.Field, e.Message)
	default:
		return e.Message
	}
}

// Errors is a collection of validation failures, reported together so an
// operator fixes everything in one pass instead of one error per restart.
type Errors []*Error

func (e Errors) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%d configuration problems:", len(e))

	for _, err := range e {
		b.WriteString("\n  - ")
		b.WriteString(err.Error())
	}

	return b.String()
}

// Validate checks the whole configuration. Every problem is collected rather
// than returning on the first, and each carries a line where one is known.
func (c *Config) Validate() error {
	var errs Errors

	add := func(line int, field, format string, args ...any) {
		errs = append(errs, &Error{Line: line, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	c.validateTargets(add)
	c.validateScheduler(add)
	c.validateNormalize(add)
	c.validateDetection(add)
	c.validateConsent(add)
	c.validateAPI(add)
	c.validateNotifiers(add)
	c.validateLogging(add)

	if len(errs) > 0 {
		return errs
	}

	return nil
}

type addFunc func(line int, field, format string, args ...any)

var nameFormat = regexp.MustCompile(`\A[a-zA-Z0-9][a-zA-Z0-9._-]*\z`)

func (c *Config) validateTargets(add addFunc) {
	seen := make(map[string]int, len(c.Targets))

	for i := range c.Targets {
		t := &c.Targets[i]
		field := fmt.Sprintf("targets[%d]", i)

		if t.Name == "" {
			// A name is required because it is the identity results and
			// baselines are stored under; deriving it from the URL would mean
			// a URL change silently orphans all history.
			add(t.line, field, "target has no name")
		} else if !nameFormat.MatchString(t.Name) {
			add(t.line, field+".name",
				"%q is not a valid name; use letters, digits, dot, dash and underscore", t.Name)
		}

		if prev, dup := seen[t.Name]; dup && t.Name != "" {
			add(t.line, field+".name", "duplicate target name %q, already defined as targets[%d]", t.Name, prev)
		} else if t.Name != "" {
			seen[t.Name] = i
		}

		c.validateTargetURL(t, field, add)
		c.validateTargetModes(t, field, add)
		c.validateTargetSchedule(t, field, add)
		c.validateTargetBudget(t, field, add)

		if t.Robots != "" && t.Robots != RobotsIgnore && t.Robots != RobotsRespect {
			add(t.line, field+".robots", "%q is not a valid policy; use \"ignore\" or \"respect\"", t.Robots)
		}

		validateSeverityRules(t.Severity, t.line, field+".severity", add)
	}

	// Defaults are validated for the parts that make sense globally.
	c.validateTargetModes(&c.Defaults, "defaults", add)
	c.validateTargetSchedule(&c.Defaults, "defaults", add)

	if c.Defaults.Robots != "" && c.Defaults.Robots != RobotsIgnore && c.Defaults.Robots != RobotsRespect {
		add(c.Defaults.line, "defaults.robots", "%q is not a valid policy; use \"ignore\" or \"respect\"", c.Defaults.Robots)
	}
}

func (c *Config) validateTargetURL(t *Target, field string, add addFunc) {
	if t.URL == "" {
		add(t.line, field+".url", "target has no url")

		return
	}

	u, err := url.Parse(t.URL)
	if err != nil {
		add(t.line, field+".url", "%q is not a valid URL: %v", t.URL, err)

		return
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		// Non-HTTP schemes are refused rather than attempted: wsaw watches the
		// web, and a file: or javascript: target would be a way to make the
		// browser do something other than fetch a page.
		add(t.line, field+".url", "scheme %q is not supported; use http or https", u.Scheme)
	}

	if u.Host == "" {
		add(t.line, field+".url", "%q has no host", t.URL)
	}

	if u.User != nil {
		// Credentials in the URL would be stored in every result and logged
		// on every scan; the basicAuth fields exist for this.
		add(t.line, field+".url",
			"URL contains embedded credentials; use basicAuthUser and basicAuthPassword instead")
	}
}

func (c *Config) validateTargetModes(t *Target, field string, add addFunc) {
	seen := make(map[model.ConsentMode]bool, len(t.ConsentModes))

	for _, m := range t.ConsentModes {
		if !m.Valid() {
			add(t.line, field+".consentModes",
				"%q is not a valid consent mode; use \"none\", \"reject\" or \"accept\"", m)

			continue
		}

		if seen[m] {
			add(t.line, field+".consentModes", "consent mode %q is listed more than once", m)
		}

		seen[m] = true
	}
}

func (c *Config) validateTargetSchedule(t *Target, field string, add addFunc) {
	if t.Cron == "" {
		return
	}

	if _, err := cron.ParseStandard(t.Cron); err != nil {
		add(t.line, field+".cron", "%q is not a valid cron expression: %v", t.Cron, err)
	}

	if t.Interval > 0 {
		// Both would be ambiguous, and guessing which one the operator meant
		// is exactly the kind of silent behaviour this config avoids.
		add(t.line, field, "both interval and cron are set; use one or the other")
	}
}

func (c *Config) validateTargetBudget(t *Target, field string, add addFunc) {
	if t.MaxRequests < 0 {
		add(t.line, field+".maxRequests", "must not be negative")
	}

	if t.MaxBytes < 0 {
		add(t.line, field+".maxBytes", "must not be negative")
	}

	if t.ViewportWidth < 0 || t.ViewportHeight < 0 {
		add(t.line, field, "viewport dimensions must not be negative")
	}

	if t.DeviceScale < 0 {
		add(t.line, field+".deviceScaleFactor", "must not be negative")
	}

	if (t.Latitude == nil) != (t.Longitude == nil) {
		add(t.line, field, "latitude and longitude must be set together")
	}

	if t.Latitude != nil && (*t.Latitude < -90 || *t.Latitude > 90) {
		add(t.line, field+".latitude", "must be between -90 and 90")
	}

	if t.Longitude != nil && (*t.Longitude < -180 || *t.Longitude > 180) {
		add(t.line, field+".longitude", "must be between -180 and 180")
	}

	if t.Proxy != "" {
		if _, err := url.Parse(t.Proxy); err != nil {
			add(t.line, field+".proxy", "is not a valid URL: %v", err)
		}
	}

	if t.HardTimeout > 0 && t.IdleQuiet > 0 && t.IdleQuiet >= t.HardTimeout {
		// Otherwise every scan would end on the hard timeout and never report
		// a clean idle termination.
		add(t.line, field, "idleQuiet (%s) must be shorter than hardTimeout (%s)",
			t.IdleQuiet.Duration(), t.HardTimeout.Duration())
	}
}

func (c *Config) validateScheduler(add addFunc) {
	s := c.Scheduler

	if s.Concurrency < 0 {
		add(0, "scheduler.concurrency", "must not be negative")
	}

	if s.PerOriginConcurrency < 0 {
		add(0, "scheduler.perOriginConcurrency", "must not be negative")
	}

	if s.Cron != "" {
		if _, err := cron.ParseStandard(s.Cron); err != nil {
			add(0, "scheduler.cron", "%q is not a valid cron expression: %v", s.Cron, err)
		}

		if s.Interval > 0 && s.Interval != Duration(defaultInterval) {
			add(0, "scheduler", "both interval and cron are set; use one or the other")
		}
	}

	if c.Browser.PoolSize < 0 {
		add(0, "browser.poolSize", "must not be negative")
	}

	if c.Browser.RemoteURL != "" {
		u, err := url.Parse(c.Browser.RemoteURL)
		if err != nil || u.Host == "" {
			add(0, "browser.remoteUrl", "%q is not a valid URL", c.Browser.RemoteURL)
		}
	}

	if c.Store.MaxPerSeries < 0 {
		add(0, "store.maxPerSeries", "must not be negative")
	}
}

func (c *Config) validateNormalize(add addFunc) {
	for i, r := range c.Normalize.PathReplacements {
		field := fmt.Sprintf("normalize.pathReplacements[%d]", i)

		if r.Pattern == "" {
			add(0, field+".pattern", "is empty")

			continue
		}

		if _, err := regexp.Compile(r.Pattern); err != nil {
			add(0, field+".pattern", "%q is not a valid regular expression: %v", r.Pattern, err)
		}
	}

	if len(c.Normalize.KeepQueryParams) > 0 && len(c.Normalize.DropQueryParams) > 0 {
		add(0, "normalize", "keepQueryParams and dropQueryParams are both set; keepQueryParams takes precedence and dropQueryParams will be ignored")
	}
}

func (c *Config) validateDetection(add addFunc) {
	switch c.Detection.Baseline {
	case "", BaselineApproved, BaselinePrevious:
	default:
		add(0, "detection.baseline", "%q is not valid; use \"approved\" or \"previous\"", c.Detection.Baseline)
	}

	validateSeverityRules(c.Detection.Severity, 0, "detection.severity", add)
}

func validateSeverityRules(s diff.SeverityRules, line int, field string, add addFunc) {
	check := func(name string, v diff.Severity) {
		if v == "" {
			return
		}

		if _, err := diff.ParseSeverity(string(v)); err != nil {
			add(line, field+"."+name, "%v", err)
		}
	}

	check("thirdPartyHostRejectMode", s.ThirdPartyHostRejectMode)
	check("thirdPartyHostPreConsent", s.ThirdPartyHostPreConsent)
	check("thirdPartyHostAdded", s.ThirdPartyHostAdded)
	check("firstPartyHostAdded", s.FirstPartyHostAdded)
	check("hostRemoved", s.HostRemoved)
	check("thirdPartyScriptAdded", s.ThirdPartyScriptAdded)
	check("thirdPartyAssetAdded", s.ThirdPartyAssetAdded)
	check("firstPartyScriptAdded", s.FirstPartyScriptAdded)
	check("firstPartyAssetAdded", s.FirstPartyAssetAdded)
	check("assetRemoved", s.AssetRemoved)
	check("thirdPartyScriptChanged", s.ThirdPartyScriptChanged)
	check("firstPartyScriptChanged", s.FirstPartyScriptChanged)
	check("thirdPartyCookieRejectMode", s.ThirdPartyCookieRejectMode)
	check("thirdPartyCookieAdded", s.ThirdPartyCookieAdded)
	check("firstPartyCookieAdded", s.FirstPartyCookieAdded)
	check("cookieRemoved", s.CookieRemoved)
	check("statusBecameError", s.StatusBecameError)
	check("statusChanged", s.StatusChanged)
	check("consentChanged", s.ConsentChanged)
	check("consentDegraded", s.ConsentDegraded)
	check("deniedHost", s.DeniedHost)
	check("scanDegraded", s.ScanDegraded)
}

func (c *Config) validateConsent(add addFunc) {
	switch c.Consent.OnFailure {
	case "", "flag", "fail":
	default:
		add(0, "consent.onFailure", "%q is not valid; use \"flag\" or \"fail\"", c.Consent.OnFailure)
	}
}

func (c *Config) validateAPI(add addFunc) {
	if !c.API.Enabled {
		return
	}

	if c.API.Listen == "" {
		add(0, "api.listen", "is empty")
	}

	if (c.API.TLSCert == "") != (c.API.TLSKey == "") {
		add(0, "api", "tlsCert and tlsKey must be set together")
	}

	// Binding beyond localhost without authentication would expose scan
	// results, which can contain personal data, to the network.
	if isRemoteListen(c.API.Listen) && c.API.Token == "" {
		add(0, "api",
			"listen address %q is not loopback but no token is set; remote exposure requires authentication",
			c.API.Listen)
	}
}

func isRemoteListen(listen string) bool {
	host := listen

	if i := strings.LastIndex(listen, ":"); i >= 0 {
		host = listen[:i]
	}

	host = strings.Trim(host, "[]")

	switch host {
	case "", "127.0.0.1", "localhost", "::1":
		return false
	default:
		return true
	}
}

func (c *Config) validateNotifiers(add addFunc) {
	seen := make(map[string]bool, len(c.Notify))

	for i, n := range c.Notify {
		field := fmt.Sprintf("notify[%d]", i)

		if n.Name == "" {
			add(0, field+".name", "notifier has no name")
		} else if seen[n.Name] {
			add(0, field+".name", "duplicate notifier name %q", n.Name)
		}

		seen[n.Name] = true

		if n.URL == "" {
			add(0, field+".url", "notifier has no url")
		} else if !strings.HasPrefix(n.URL, "${") {
			u, err := url.Parse(n.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				add(0, field+".url", "%q is not a valid http or https URL", n.URL)
			}
		}

		if n.MinSeverity != "" {
			if _, err := diff.ParseSeverity(n.MinSeverity); err != nil {
				add(0, field+".minSeverity", "%v", err)
			}
		}

		if n.MaxRetries < 0 {
			add(0, field+".maxRetries", "must not be negative")
		}
	}
}

func (c *Config) validateLogging(add addFunc) {
	switch strings.ToLower(c.Logging.Level) {
	case "", "debug", "info", "warn", "error":
	default:
		add(0, "logging.level", "%q is not valid; use debug, info, warn or error", c.Logging.Level)
	}

	switch strings.ToLower(c.Logging.Format) {
	case "", "json", "text":
	default:
		add(0, "logging.format", "%q is not valid; use json or text", c.Logging.Format)
	}
}

// AsError returns the errors as a plain error, or nil.
func (e Errors) AsError() error {
	if len(e) == 0 {
		return nil
	}

	return e
}

// IsValidationError reports whether err came from config validation, so
// callers can present it differently from an I/O failure.
func IsValidationError(err error) bool {
	var errs Errors

	return errors.As(err, &errs)
}
