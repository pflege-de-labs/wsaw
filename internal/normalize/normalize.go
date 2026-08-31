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

	// PathReplacements collapse volatile path segments, e.g. build hashes.
	PathReplacements []Replacement

	// DropTrailingSlash normalizes "/a/" to "/a".
	DropTrailingSlash bool
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

// Common query parameters that carry no meaning for asset identity. Offered
// as a starting point; operators decide what applies to their sites.
var DefaultDropQueryParams = []string{
	"_", "cb", "cachebuster", "cache_bust", "rand", "random", "ts", "t", "v", "ver", "version",
	"gclid", "fbclid", "msclkid", "utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
}

// Normalizer applies a compiled rule set. It is safe for concurrent use.
type Normalizer struct {
	dropAllQuery bool
	dropParams   map[string]struct{}
	keepParams   map[string]struct{}
	pathReplace  []Replacement
	dropTrailing bool
}

// New compiles rules into a Normalizer. Invalid path patterns are a
// configuration error and are reported at load time rather than at scan time.
func New(r Rules) (*Normalizer, error) {
	n := &Normalizer{
		dropAllQuery: r.DropAllQuery,
		dropTrailing: r.DropTrailingSlash,
	}

	if len(r.DropQueryParams) > 0 {
		n.dropParams = make(map[string]struct{}, len(r.DropQueryParams))
		for _, p := range r.DropQueryParams {
			n.dropParams[strings.ToLower(p)] = struct{}{}
		}
	}

	if len(r.KeepQueryParams) > 0 {
		n.keepParams = make(map[string]struct{}, len(r.KeepQueryParams))
		for _, p := range r.KeepQueryParams {
			n.keepParams[strings.ToLower(p)] = struct{}{}
		}
	}

	for i, rep := range r.PathReplacements {
		re, err := regexp.Compile(rep.Pattern)
		if err != nil {
			return nil, fmt.Errorf("path replacement %d: compiling %q: %w", i, rep.Pattern, err)
		}

		rep.re = re
		n.pathReplace = append(n.pathReplace, rep)
	}

	return n, nil
}

// Key returns the comparison key for raw. Inputs that are not hierarchical
// URLs — data:, blob:, javascript: — are returned with their opaque payload
// removed, so a diff does not churn on inline content while still recording
// that such a resource existed.
func (n *Normalizer) Key(raw string) string {
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
	query := n.normalizeQuery(u)

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

// normalizeQuery returns the normalized query string, without a leading "?".
func (n *Normalizer) normalizeQuery(u *url.URL) string {
	if n.dropAllQuery || u.RawQuery == "" {
		return ""
	}

	values := u.Query()

	for name := range values {
		lower := strings.ToLower(name)

		switch {
		case n.keepParams != nil:
			if _, keep := n.keepParams[lower]; !keep {
				delete(values, name)
			}
		case n.dropParams != nil:
			if _, drop := n.dropParams[lower]; drop {
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
