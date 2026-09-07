package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/logging"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/retry"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// io_EOF is aliased so Parse can recognize an empty document without
// importing io at the call site.
var io_EOF = io.EOF //nolint:revive // deliberate local alias

// The URL schemes wsaw accepts anywhere a URL is configured.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

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
	c.validateAPIRefresh(add)
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

	if u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS {
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

	validateTargetRetry(t, field, add)

	if t.HardTimeout > 0 && t.IdleQuiet > 0 && t.IdleQuiet >= t.HardTimeout {
		// Otherwise every scan would end on the hard timeout and never report
		// a clean idle termination.
		add(t.line, field, "idleQuiet (%s) must be shorter than hardTimeout (%s)",
			t.IdleQuiet.Duration(), t.HardTimeout.Duration())
	}
}

// validateTargetRetry refuses a retry schedule that cannot work (Story 3.8).
func validateTargetRetry(t *Target, field string, add addFunc) {
	if t.RetryAttempts < 0 {
		add(t.line, field+".retryAttempts", "must not be negative; 1 disables retrying")
	}

	if t.RetryBackoff < 0 || t.RetryMaxBackoff < 0 {
		add(t.line, field+".retryBackoff", "must not be negative")
	}

	if t.RetryMaxBackoff > 0 && t.RetryBackoff > t.RetryMaxBackoff {
		add(t.line, field+".retryMaxBackoff",
			"retryMaxBackoff (%s) must not be shorter than retryBackoff (%s)",
			t.RetryMaxBackoff.Duration(), t.RetryBackoff.Duration())
	}

	// AC9: retrying must finish inside the target's own schedule. A retry
	// schedule longer than the scan schedule means the next scheduled scan of
	// a target starts while its retries are still going, which is two runs of
	// one target at once — the thing the minimum interval exists to prevent.
	if t.Interval <= 0 || t.RetryAttempts <= 1 {
		return
	}

	policy := retry.Policy{
		Attempts:   t.RetryAttempts,
		Backoff:    t.RetryBackoff.Duration(),
		MaxBackoff: t.RetryMaxBackoff.Duration(),
	}

	if policy.Backoff == 0 {
		policy.Backoff = DefaultRetryBackoff
	}

	if total := policy.TotalDelay(t.Name); total >= t.Interval.Duration() {
		add(t.line, field+".retryAttempts",
			"%d attempts with a %s backoff wait up to %s between them, which is not shorter than this target's interval of %s: "+
				"retries would still be running when the next scheduled scan starts",
			t.RetryAttempts, policy.Backoff, total, t.Interval.Duration())
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

	if !container.Kind(strings.ToLower(c.Browser.Runtime)).Valid() {
		add(0, "browser.runtime", "%q is not valid; use auto, podman, docker or local", c.Browser.Runtime)
	}

	if c.Browser.Container.PidsLimit < 0 {
		add(0, "browser.container.pidsLimit", "must not be negative")
	}

	if c.Browser.RemoteURL != "" {
		u, err := url.Parse(c.Browser.RemoteURL)
		if err != nil || u.Host == "" {
			add(0, "browser.remoteUrl", "%q is not a valid URL", c.Browser.RemoteURL)
		}
	}

	validateStoreDriver(c.Store, add)

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

// validateStoreDriver refuses a store that cannot work, at load time. A
// daemon that starts and then cannot write a result is a watcher that is not
// watching (Tenet 8).
func validateStoreDriver(st Store, add addFunc) {
	known := false

	for _, d := range store.Drivers() {
		if st.StoreDriver() == d {
			known = true

			break
		}
	}

	if !known {
		add(0, "store.driver", "%q is not valid; use %s",
			st.Driver, strings.Join(store.Drivers(), ", "))

		return
	}

	if st.MaxAttempts < 0 {
		add(0, "store.maxAttempts", "must not be negative; 1 disables retrying")
	}

	if st.RetryBackoff < 0 {
		add(0, "store.retryBackoff", "must not be negative")
	}

	if st.IsServerStore() {
		if st.DSN == "" {
			add(0, "store.dsn", "the %s driver needs a connection string", st.StoreDriver())
		}

		if st.Path != "" {
			add(0, "store.path",
				"path is a file for the sqlite driver and means nothing to %s; remove it or set driver: sqlite",
				st.StoreDriver())
		}

		return
	}

	if st.DSN != "" {
		add(0, "store.dsn", "dsn belongs to a server database; the sqlite driver uses store.path")
	}

	if st.MaxOpenConns != 0 || st.MaxIdleConns != 0 || st.ConnMaxLifetime != 0 {
		add(0, "store.maxOpenConns",
			"connection-pool settings apply to a server database; sqlite is deliberately serialised")
	}
}

// validateAPIRefresh bounds the interface's default refresh interval
// (Story 5.16, AC8). Every refresh re-renders the dashboard and reads every
// series' latest result, so a one-second default across a large target list
// would be a denial of service against one's own daemon.
func (c *Config) validateAPIRefresh(add addFunc) {
	const floor = 5 * time.Second

	d := c.API.RefreshInterval.Duration()

	switch {
	case d < 0:
		add(0, "api.refreshInterval", "must not be negative; 0 disables auto-refresh")
	case d > 0 && d < floor:
		add(0, "api.refreshInterval",
			"%s is below the %s floor: every refresh re-renders the dashboard and reads every target's latest result",
			d, floor)
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
			if err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) {
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

		validateNotifierKind(n, field, add)
	}
}

// validateNotifierKind rejects settings that belong to the other kind of
// notifier. Ignoring them silently would leave an operator believing a
// template or a filter is in force when it is not.
func validateNotifierKind(n Notifier, field string, add addFunc) {
	switch n.NotifierKind() {
	case NotifyWebhook:
		if n.Format != "" {
			add(0, field+".format", "format applies only to a teams notifier")
		}

		if n.BaseURL != "" {
			add(0, field+".baseUrl", "baseUrl applies only to a teams notifier")
		}

		if n.MaxChanges != 0 {
			add(0, field+".maxChanges", "maxChanges applies only to a teams notifier, which sends one card per scan")
		}

	case NotifyTeams:
		switch n.TeamsFormat() {
		case TeamsAdaptive, TeamsMessageCard:
		default:
			add(0, field+".format", "%q is not valid; use adaptive or the deprecated messagecard", n.Format)
		}

		if n.Template != "" {
			add(0, field+".template",
				"a teams notifier builds its card in wsaw and takes no template: "+
					"a hand-written card that is malformed is accepted by Teams and posts nothing, "+
					"so the format is not left to configuration")
		}

		if len(n.ChangeTypes) > 0 {
			add(0, field+".changeTypes",
				"a teams notifier sends one card per scan, so it cannot filter by change type; "+
					"use minSeverity to decide which scans are worth a message")
		}

		if n.MaxChanges < 0 {
			add(0, field+".maxChanges", "must not be negative")
		}

		if n.BaseURL != "" {
			u, err := url.Parse(n.BaseURL)
			if err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "" {
				add(0, field+".baseUrl",
					"%q is not an absolute http or https URL such as \"https://wsaw.example.com\"", n.BaseURL)
			}
		}

	default:
		add(0, field+".kind", "%q is not valid; use webhook or teams", n.Kind)
	}
}

func (c *Config) validateLogging(add addFunc) {
	switch strings.ToLower(c.Logging.Level) {
	case "", "debug", "info", "warn", "error":
	default:
		add(0, "logging.level", "%q is not valid; use debug, info, warn or error", c.Logging.Level)
	}

	if !logging.Format(strings.ToLower(c.Logging.Format)).Valid() {
		add(0, "logging.format", "%q is not valid; use auto, pretty, json or text", c.Logging.Format)
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
