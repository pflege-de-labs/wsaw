package capture

import (
	"fmt"
	"regexp"
	"strings"
)

// Beacon names requests that must not hold a scan open (Story 1.10).
//
// A time-on-site tracker keeps reporting for as long as the tab is open, so
// its request stream has no end for idle detection to wait for. Counting it
// means a scan of an ordinary commercial site ends on its hard timeout rather
// than when the page went quiet, and whether it ends at all depends on where
// the heartbeat's schedule happens to fall — the same target reporting "idle"
// on one run and "timeout" on the next.
//
// Which endpoints those are is configuration, not a judgement capture makes
// on its own (Tenet 10): a rule matches on Host, on URLPattern, or on both
// together, and a rule with neither matches nothing and is refused at load.
type Beacon struct {
	// Host is a host pattern with the same semantics as an allow or deny
	// list: an exact host, a bare domain that also covers its subdomains, or
	// a leading "*." wildcard.
	Host string
	// URLPattern is a regular expression matched against the raw URL. It is
	// how a host that serves both scripts and telemetry is matched by path,
	// which is the only safe way to match one: excluding a script from idle
	// accounting would end the scan before the assets that script loads were
	// ever requested.
	URLPattern string
}

// DefaultBeacons are the periodic beacons known to hold scans open. They are
// applied unless configuration opts out, because without them the first scan
// of an ordinary commercial site reports a timeout.
//
// Every entry names an endpoint that exists to receive telemetry. Where the
// vendor serves its script from the same host, the rule is scoped by path.
var DefaultBeacons = []Beacon{
	// Taboola's time-on-site heartbeat, every 10s for as long as the tab
	// lives. The host serves events only; recommendations come from trc.
	{Host: "trc-events.taboola.com"},

	// Outbrain's pixel endpoint.
	{Host: "tr.outbrain.com"},

	// Microsoft Clarity uploads session data on an interval. Its tag and
	// script come from www. and scripts.clarity.ms, so the rule is scoped to
	// the ingest path rather than to the domain.
	{Host: "clarity.ms", URLPattern: `/collect(\?|$)`},

	// GA4 keeps sending while the page is open. google-analytics.com also
	// serves analytics.js and gtag, hence the path.
	{URLPattern: `^https?://([a-z0-9-]+\.)?(google-analytics\.com|analytics\.google\.com)/(g/)?collect`},

	// Yandex Metrica's hit endpoint; mc.yandex.ru also serves the tag.
	{URLPattern: `^https?://mc\.yandex\.(ru|com)/watch/`},

	// Matomo and its Piwik-era endpoint, wherever they are self-hosted.
	{URLPattern: `/(matomo|piwik)\.php(\?|$)`},

	// Sentry's envelope endpoint, both SaaS and self-hosted.
	{URLPattern: `/api/[0-9]+/(envelope|store)/`},

	// New Relic Browser harvests on a fixed cycle.
	{Host: "nr-data.net"},

	// Datadog RUM intake. Regional hosts share the suffix.
	{Host: "browser-intake-datadoghq.com"},
	{Host: "browser-intake-datadoghq.eu"},

	// FullStory bundle uploads. Its script comes from edge.fullstory.com.
	{Host: "rs.fullstory.com"},

	// LogRocket ingest. Its script comes from cdn.lr-ingest.io.
	{Host: "r.lr-ingest.io"},

	// Hotjar's ingest and client endpoints; static.hotjar.com keeps serving
	// the script normally.
	{Host: "in.hotjar.com"},
	{Host: "vc.hotjar.io"},

	// Chartbeat pings every 15s while the page is visible.
	{Host: "ping.chartbeat.net"},

	// Parse.ly's engaged-time heartbeat.
	{Host: "p1.parsely.com"},

	// Segment, Amplitude and Mixpanel event intake. All three serve their
	// SDKs from separate CDN hosts.
	{Host: "api.segment.io"},
	{Host: "api2.amplitude.com"},
	{Host: "api.eu.amplitude.com"},
	{Host: "api-js.mixpanel.com"},

	// Adobe Analytics image beacons.
	{Host: "sc.omtrdc.net"},
	{Host: "2o7.net"},

	// Cloudflare Web Analytics, served from the site's own origin.
	{URLPattern: `/cdn-cgi/rum(\?|$)`},
}

// Beacons is a compiled rule set. The zero value matches nothing, and a
// compiled set is safe for concurrent use.
type Beacons struct {
	rules []compiledBeacon
}

type compiledBeacon struct {
	// host is the pattern with any "*." prefix removed. It matches the host
	// itself and any subdomain of it.
	host string
	url  *regexp.Regexp
}

// CompileBeacons compiles rules, reporting a bad one at load rather than at
// scan time. A rule that names neither a host nor a pattern would silently
// match every request, which is the opposite of what an operator writing it
// meant, so it is an error.
func CompileBeacons(rules []Beacon) (Beacons, error) {
	var out Beacons

	for i, rule := range rules {
		host := strings.ToLower(strings.TrimSpace(rule.Host))
		host = strings.TrimPrefix(host, "*.")
		pattern := strings.TrimSpace(rule.URLPattern)

		if host == "" && pattern == "" {
			return Beacons{}, fmt.Errorf("beacon %d: needs a host or a urlPattern", i)
		}

		c := compiledBeacon{host: host}

		if pattern != "" {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return Beacons{}, fmt.Errorf("beacon %d: compiling urlPattern %q: %w", i, pattern, err)
			}

			c.url = re
		}

		out.rules = append(out.rules, c)
	}

	return out, nil
}

// Empty reports whether the set matches nothing.
func (b Beacons) Empty() bool { return len(b.rules) == 0 }

// Matches reports whether a request is a beacon. Both parts of a rule must
// match, so a host can be narrowed by path.
func (b Beacons) Matches(rawURL, host string) bool {
	if len(b.rules) == 0 {
		return false
	}

	host = strings.ToLower(host)

	for _, rule := range b.rules {
		if rule.host != "" && !hostMatches(host, rule.host) {
			continue
		}

		if rule.url != nil && !rule.url.MatchString(rawURL) {
			continue
		}

		return true
	}

	return false
}

func hostMatches(host, pattern string) bool {
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}
