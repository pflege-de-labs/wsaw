// Package normalize derives stable comparison keys from observed URLs.
//
// Diffing raw URLs is useless: cache busters, session identifiers, and
// content hashes in paths make every scan look different. Normalization
// exists so a diff shows real change (Tenet 6). Both the raw URL and the
// normalized key are stored, so a reader can always see what was compared.
package normalize

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Rules configures normalization. The zero value performs only the
// structural normalization that is always safe: lowercasing the host,
// dropping the fragment, and dropping a default port.
type Rules struct {
	// DropQueryParams removes these query parameters by name.
	DropQueryParams []string
	// DropAllQuery removes the entire query string. Blunt, but the right
	// answer for sites that sign every asset URL.
	DropAllQuery bool
	// KeepQueryParams, when non-empty, keeps only these parameters and drops
	// everything else. Takes precedence over DropQueryParams.
	KeepQueryParams []string

	// QueryRules replace the query handling above for the requests they
	// match. The first matching rule wins; a request no rule matches is
	// handled by the global settings.
	QueryRules []QueryRule

	// PathReplacements collapse volatile path segments, e.g. build hashes.
	PathReplacements []Replacement

	// DropTrailingSlash normalizes "/a/" to "/a".
	DropTrailingSlash bool

	// BodyIdentities lift a self-published version out of a response body,
	// for scripts whose bytes change more often than their content does.
	BodyIdentities []BodyIdentity

	// VolatileBodies name responses whose body is different on every fetch
	// by design, and declares no version a BodyIdentity could read.
	VolatileBodies []VolatileBody
}

// Replacement rewrites part of a path with a fixed placeholder.
type Replacement struct {
	// Pattern is a regular expression matched against the path.
	Pattern string
	// With is the literal replacement, conventionally a placeholder such as
	// "{hash}" so the collapse is visible in output.
	With string

	re *regexp.Regexp
}

// QueryRule scopes query-string handling to some requests.
//
// It exists for tracking pixels and conversion beacons: the same endpoint,
// on the same host and path, called with a different query string on every
// visit. A global rule broad enough to quiet them would also strip the query
// from first-party URLs where it carries meaning, so the rule is scoped to
// the requests that need it, and keeps the parameters that name an account
// or container so a switch of those is still a change.
type QueryRule struct {
	// URLPattern selects requests by their raw URL. Empty matches any URL.
	URLPattern string
	// Party selects requests by party. Empty matches either.
	Party model.Party

	// Exactly one of the three below is set; validation enforces it.
	KeepQueryParams []string
	DropQueryParams []string
	DropAllQuery    bool

	urlRe *regexp.Regexp
	query queryPolicy
}

// queryPolicy is a compiled set of query-string instructions, shared by the
// global settings and by each QueryRule.
type queryPolicy struct {
	dropAll bool
	drop    map[string]struct{}
	keep    map[string]struct{}
}

func newQueryPolicy(dropAll bool, drop, keep []string) queryPolicy {
	return queryPolicy{dropAll: dropAll, drop: lowerSet(drop), keep: lowerSet(keep)}
}

// lowerSet returns nil for an empty list, which queryPolicy reads as "not
// configured" rather than as "keep nothing".
func lowerSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}

	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = struct{}{}
	}

	return out
}

func (r *QueryRule) matches(raw string, party model.Party) bool {
	if r.Party != "" && r.Party != party {
		return false
	}

	return r.urlRe == nil || r.urlRe.MatchString(raw)
}

// DefaultQueryRules are tracking and conversion endpoints whose query string
// is per-visit state: consent flags, user-agent hints, timestamps, page
// titles. Each keeps only the parameters that identify the account or
// container the hit is for, so a new tag ID is still reported. Offered as a
// starting point; operators can opt out or put their own rules first.
var DefaultQueryRules = []QueryRule{
	// The gtag loader's cx and gtm parameters change with Google's rollout
	// state; id names the tag.
	{URLPattern: `^https?://www\.googletagmanager\.com/gtag/(js|destination)(\?|$)`, KeepQueryParams: []string{"id"}},

	// GA4 and Universal Analytics hits, on Google's hosts or a server-side
	// tagging host of the site's own; tid names the property.
	{URLPattern: `^https?://[^/?#]+/[gj]/collect(\?|$)`, KeepQueryParams: []string{"tid"}},

	// Google Ads, Floodlight and consent-mode pings. The conversion or
	// audience ID is in the path, so nothing in the query is identity.
	{
		URLPattern: `^https?://([a-z0-9-]+\.)*(google\.[a-z.]+|doubleclick\.net|googlesyndication\.com|googleadservices\.com)` +
			`/(ccm(/s)?/collect|rmkt/collect|pagead/(viewthroughconversion|1p-user-list|1p-conversion|conversion)/|ads/ga-audiences)`,
		DropAllQuery: true,
	},

	// Microsoft UET and its cookie-sync pixels; ti names the UET tag.
	{URLPattern: `^https?://bat\.bing\.com/actionp?/`, KeepQueryParams: []string{"ti"}},
	{URLPattern: `^https?://c\.(bing\.com|clarity\.ms)/c\.gif(\?|$)`, DropAllQuery: true},

	// Meta pixel; id names the pixel.
	{URLPattern: `^https?://www\.facebook\.com/tr/?(\?|$)`, KeepQueryParams: []string{"id"}},

	// Pinterest tag; tid names the tag.
	{URLPattern: `^https?://ct\.pinterest\.com/v3/?(\?|$)`, KeepQueryParams: []string{"tid"}},
}

// VolatileBody marks responses whose content is per-visit, so comparing
// their digests reports a change on every scan.
//
// It exists for beacons that answer with a script built for the one visit —
// Google Ads' viewthroughconversion embeds the visit's own parameters in the
// script it returns. Such a response is still an asset, compared by its key
// like any other: a new conversion ID in the path is still reported. Only
// its digest is not compared, and that is a rule an operator wrote or kept,
// never something capture concluded on its own.
type VolatileBody struct {
	// URLPattern selects requests by their raw URL.
	URLPattern string

	re *regexp.Regexp
}

// DefaultVolatileBodies are the per-visit responses the shipped query rules
// make comparable by key: once two visits share a key, their bodies meet,
// and these differ on every visit. Offered as a starting point; operators
// can opt out.
var DefaultVolatileBodies = []VolatileBody{
	{URLPattern: `^https?://([a-z0-9-]+\.)*(doubleclick\.net|google\.[a-z.]+|googleadservices\.com)/pagead/viewthroughconversion/`},
}

// BodyIdentity extracts a stable identifier from a response body.
//
// It exists for one shape of false positive: a script served from a stable
// URL whose bytes differ on nearly every fetch, while the version it declares
// about itself stays put. Hashing such a body reports a change every time and
// buries the one publish that mattered. Extracting the declared version turns
// that stream of noise into a single change with a number in it.
type BodyIdentity struct {
	// URLPattern selects which requests the rule applies to. It is matched
	// against the raw URL, not the normalized key, so a rule can key on a
	// query parameter that normalization drops.
	URLPattern string
	// Extract is a regular expression with exactly one capturing group; the
	// group is the identity.
	Extract string
	// Label names the identity for reports, e.g. "GTM container version".
	Label string

	urlRe, extractRe *regexp.Regexp
}

// Common query parameters that carry no meaning for asset identity. Offered
// as a starting point; operators decide what applies to their sites.
var DefaultDropQueryParams = []string{
	"_", "cb", "cachebuster", "cache_bust", "rand", "random", "ts", "t", "v", "ver", "version",
	"gclid", "fbclid", "msclkid", "utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
}

// Normalizer applies a compiled rule set. It is safe for concurrent use.
type Normalizer struct {
	query        queryPolicy
	queryRules   []QueryRule
	pathReplace  []Replacement
	dropTrailing bool
	bodyIdents   []BodyIdentity
	volatile     []VolatileBody
}

// New compiles rules into a Normalizer. Invalid path patterns are a
// configuration error and are reported at load time rather than at scan time.
func New(r Rules) (*Normalizer, error) {
	n := &Normalizer{
		query:        newQueryPolicy(r.DropAllQuery, r.DropQueryParams, r.KeepQueryParams),
		dropTrailing: r.DropTrailingSlash,
	}

	for i, qr := range r.QueryRules {
		if qr.URLPattern != "" {
			re, err := regexp.Compile(qr.URLPattern)
			if err != nil {
				return nil, fmt.Errorf("query rule %d: compiling urlPattern %q: %w", i, qr.URLPattern, err)
			}

			qr.urlRe = re
		}

		qr.query = newQueryPolicy(qr.DropAllQuery, qr.DropQueryParams, qr.KeepQueryParams)
		n.queryRules = append(n.queryRules, qr)
	}

	for i, rep := range r.PathReplacements {
		re, err := regexp.Compile(rep.Pattern)
		if err != nil {
			return nil, fmt.Errorf("path replacement %d: compiling %q: %w", i, rep.Pattern, err)
		}

		rep.re = re
		n.pathReplace = append(n.pathReplace, rep)
	}

	for i, id := range r.BodyIdentities {
		urlRe, err := regexp.Compile(id.URLPattern)
		if err != nil {
			return nil, fmt.Errorf("body identity %d: compiling urlPattern %q: %w", i, id.URLPattern, err)
		}

		extractRe, err := regexp.Compile(id.Extract)
		if err != nil {
			return nil, fmt.Errorf("body identity %d: compiling extract %q: %w", i, id.Extract, err)
		}

		// One capturing group, checked here rather than at extraction time:
		// a rule that can never produce a value is a configuration mistake,
		// and it should be reported at load rather than silently yielding
		// nothing on every scan.
		if got := extractRe.NumSubexp(); got != 1 {
			return nil, fmt.Errorf(
				"body identity %d: extract %q has %d capturing groups, want exactly 1",
				i, id.Extract, got)
		}

		id.urlRe, id.extractRe = urlRe, extractRe
		n.bodyIdents = append(n.bodyIdents, id)
	}

	for i, v := range r.VolatileBodies {
		re, err := regexp.Compile(v.URLPattern)
		if err != nil {
			return nil, fmt.Errorf("volatile body %d: compiling urlPattern %q: %w", i, v.URLPattern, err)
		}

		v.re = re
		n.volatile = append(n.volatile, v)
	}

	return n, nil
}

// VolatileBody reports whether rawURL names a response whose body is
// per-visit, and so must not be compared by digest.
func (n *Normalizer) VolatileBody(rawURL string) bool {
	for i := range n.volatile {
		if n.volatile[i].re.MatchString(rawURL) {
			return true
		}
	}

	return false
}

// BodyIdentity returns the label and value of the first matching identity
// rule, or empty strings when no rule applies or the body does not carry the
// identity. A rule that matches the URL but not the body yields nothing,
// which a comparison must treat as "not comparable" rather than as a change.
func (n *Normalizer) BodyIdentity(rawURL, body string) (label, value string) {
	for i := range n.bodyIdents {
		id := &n.bodyIdents[i]

		if !id.urlRe.MatchString(rawURL) {
			continue
		}

		m := id.extractRe.FindStringSubmatch(body)
		if m == nil {
			return "", ""
		}

		return id.Label, m[1]
	}

	return "", ""
}

// HasBodyIdentities reports whether any identity rule is configured, so a
// caller can skip the work of holding on to a body when none is.
func (n *Normalizer) HasBodyIdentities() bool { return len(n.bodyIdents) > 0 }

// Key returns the comparison key for raw, a request of the given party. An
// empty party matches only query rules that name none. Inputs that are not
// hierarchical URLs — data:, blob:, javascript: — are returned with their
// opaque payload removed, so a diff does not churn on inline content while
// still recording that such a resource existed.
func (n *Normalizer) Key(raw string, party model.Party) string {
	if raw == "" {
		return ""
	}

	if scheme, rest, ok := opaqueScheme(raw); ok {
		return scheme + ":" + opaquePrefix(scheme, rest)
	}

	u, err := url.Parse(raw)
	if err != nil {
		// An unparseable URL is still a fact about the page; keep it verbatim
		// rather than discarding the observation.
		return raw
	}

	host := normalizeHost(u.Scheme, u.Host)
	path := n.normalizePath(u.Path)
	query := n.policyFor(raw, party).apply(u)

	// The key is assembled by hand rather than via url.String(), which
	// percent-encodes the braces in placeholders like "{hash}" and would make
	// collapsed keys unreadable. A key is a comparison token, not a URL to
	// fetch, so readability wins.
	var b strings.Builder

	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}

	if u.User != nil {
		// Userinfo in an asset URL is a credential; never carry it into a
		// stored, exported, or displayed key.
		b.WriteString("…@")
	}

	b.WriteString(host)
	b.WriteString(path)

	if query != "" {
		b.WriteString("?")
		b.WriteString(query)
	}

	return b.String()
}

func (n *Normalizer) normalizePath(path string) string {
	for _, rep := range n.pathReplace {
		path = rep.re.ReplaceAllString(path, rep.With)
	}

	if n.dropTrailing && len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimRight(path, "/")
		if path == "" {
			path = "/"
		}
	}

	return path
}

// Rekey derives req's comparison key again under the current rules, from
// the raw URL stored beside the old key (Tenet 4). It is what lets a rule
// change apply to history, and what keeps the first scan after one from
// reporting every re-keyed asset as removed and added again.
//
// An opaque URL keeps its stored key: a data: URL is stored truncated, and
// its key was derived from the full one at capture.
func (n *Normalizer) Rekey(req *model.Request) string {
	if req.URL == "" {
		return req.NormalizedURL
	}

	if _, _, opaque := opaqueScheme(req.URL); opaque {
		return req.NormalizedURL
	}

	return n.Key(req.URL, req.Party)
}

func (n *Normalizer) policyFor(raw string, party model.Party) *queryPolicy {
	for i := range n.queryRules {
		if n.queryRules[i].matches(raw, party) {
			return &n.queryRules[i].query
		}
	}

	return &n.query
}

// apply returns the normalized query string, without a leading "?".
func (p *queryPolicy) apply(u *url.URL) string {
	if p.dropAll || u.RawQuery == "" {
		return ""
	}

	values := u.Query()

	for name := range values {
		lower := strings.ToLower(name)

		switch {
		case p.keep != nil:
			if _, keep := p.keep[lower]; !keep {
				delete(values, name)
			}
		case p.drop != nil:
			if _, drop := p.drop[lower]; drop {
				delete(values, name)
			}
		}
	}

	// url.Values.Encode sorts by key, which is what makes the key stable
	// across scans regardless of the order the page used.
	return values.Encode()
}

// normalizeHost lowercases the host and removes the port when it is the
// scheme's default.
func normalizeHost(scheme, host string) string {
	host = strings.ToLower(host)

	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	case scheme == "ws" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case scheme == "wss" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	default:
		return host
	}
}

// opaqueSchemes are the non-hierarchical schemes wsaw records but cannot
// meaningfully compare byte-for-byte.
var opaqueSchemes = []string{"data", "blob", "javascript", "filesystem", "about"}

func opaqueScheme(raw string) (scheme, rest string, ok bool) {
	i := strings.IndexByte(raw, ':')
	if i <= 0 {
		return "", "", false
	}

	scheme = strings.ToLower(raw[:i])

	for _, s := range opaqueSchemes {
		if scheme == s {
			return scheme, raw[i+1:], true
		}
	}

	return "", "", false
}

// uuidPattern matches the per-load identifier Chrome mints for blob: and
// filesystem: URLs. It is random on every page load, so leaving it in a key
// would report a brand-new asset on every scan.
var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// opaquePrefix keeps only the stable, descriptive part of an opaque URL — the
// MIME type of a data: URL, the origin of a blob: URL — and discards the
// volatile payload or identifier.
func opaquePrefix(scheme, rest string) string {
	switch scheme {
	case "blob", "filesystem":
		// Keep the origin, which is the informative part, and collapse the
		// random object identifier.
		return uuidPattern.ReplaceAllString(rest, "{uuid}")

	case "data":
		// Everything after the comma is content, which changes constantly and
		// can be large. The media type is the part worth comparing.
		if i := strings.IndexByte(rest, ','); i >= 0 {
			return rest[:i] + ",…"
		}
	}

	const maxOpaque = 64
	if len(rest) > maxOpaque {
		return rest[:maxOpaque] + "…"
	}

	return rest
}

// Sorted returns keys in a deterministic order, for callers building diff
// output.
func Sorted(keys []string) []string {
	out := make([]string, len(keys))
	copy(out, keys)
	sort.Strings(out)

	return out
}
