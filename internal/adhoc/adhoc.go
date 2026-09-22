// Package adhoc decides whether a URL somebody typed may be scanned, and
// under what name its results are stored.
//
// A configured target is an operator's decision, made in a file. A URL typed
// into the web interface is somebody else's decision, made at a keyboard, so
// it passes through here before a browser is ever pointed at it: wsaw fetches
// what it is told to fetch, and a service that fetches any address on request
// is a request-forgery primitive aimed at whatever network it runs in
// (Story 5.27).
//
// The name a result is stored under is derived the same way `wsaw scan --url`
// derives it, so the same URL scanned from either place shares one history.
package adhoc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Policy is what a deployment lets a typed URL be.
type Policy struct {
	// AllowPrivateHosts permits addresses that are not routable on the public
	// internet: loopback, the private ranges, link-local — the cloud metadata
	// service among them. Off by default. On, only for a deployment whose
	// operator means "scan our own staging environment" and has read what
	// that opens up.
	AllowPrivateHosts bool

	// Resolver looks the host up. Nil is the system resolver; tests supply
	// their own, so that no test needs a network (Tenet 13).
	Resolver Resolver
}

// Resolver is the name lookup a host check needs. It is declared here because
// this is where it is consumed (Tenet 12).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// maxURLLength bounds what is accepted from a form field. Nothing wsaw does
// needs a longer address, and an unbounded one is free work for whoever sends
// it.
const maxURLLength = 2048

// maxName is how long a derived name may be. Long enough for a hostname and a
// path fragment, short enough to read in a table.
const maxName = 80

// The schemes a typed URL may use. A file: or javascript: target would be a
// way to make the browser do something other than fetch a page.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// Accept validates a typed URL and reports the target name its results are
// stored under, or why it will not be scanned.
//
// The refusals are worded for whoever typed the URL, because that is who
// reads them.
//
// The host is checked as it resolves now, which is not a promise about how it
// resolves when the browser navigates a moment later: a name whose answer
// changes between the two lookups is not caught here, and cannot be — the
// browser does its own resolution. This is the reason AllowPrivateHosts is
// off by default rather than the reason it can be ignored.
func Accept(ctx context.Context, rawURL string, p Policy) (string, error) {
	raw := strings.TrimSpace(rawURL)

	u, err := parse(raw)
	if err != nil {
		return "", err
	}

	name := Name(raw)
	if name == "" {
		return "", errors.New("no result name can be derived from that URL")
	}

	if err := checkHost(ctx, u.Hostname(), p); err != nil {
		return "", err
	}

	return name, nil
}

// parse reads the URL and refuses the shapes wsaw will not scan.
func parse(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("no URL was given")
	}

	if len(raw) > maxURLLength {
		return nil, fmt.Errorf("the URL is %d characters long; the limit is %d", len(raw), maxURLLength)
	}

	u, err := url.Parse(raw)
	if err != nil {
		// The parse error's own text is not repeated: it quotes the whole URL
		// back, which in an HTML page is one more place page-shaped input is
		// echoed for no gain.
		return nil, errors.New("that is not a valid URL")
	}

	switch u.Scheme {
	case schemeHTTP, schemeHTTPS:
	case "":
		return nil, errors.New("the URL needs a scheme; write it as https://example.com/")
	default:
		return nil, fmt.Errorf("scheme %q is not supported; use http or https", u.Scheme)
	}

	if u.Host == "" {
		return nil, errors.New("the URL has no host")
	}

	if u.User != nil {
		// Credentials in the URL would be stored in every result and logged on
		// every scan. A site that needs them needs a configured target, where
		// basicAuthUser and basicAuthPassword keep them out of the result.
		return nil, errors.New("the URL contains credentials; scan it without them, " +
			"or configure it as a target with basicAuthUser and basicAuthPassword")
	}

	return u, nil
}

// checkHost refuses an address this wsaw should not be asked to fetch.
func checkHost(ctx context.Context, host string, p Policy) error {
	if host == "" {
		return errors.New("the URL has no host")
	}

	if p.AllowPrivateHosts {
		return nil
	}

	// "localhost" is loopback by definition (RFC 6761), whatever a resolver
	// on this machine has been persuaded to answer.
	if isLoopbackName(host) {
		return fmt.Errorf("%s names this machine, which is not a public internet address; "+
			"wsaw will not fetch it on request. Configure it as a target, "+
			"or set api.adHocUrls.allowPrivateHosts for this deployment", host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !isPublic(ip) {
			return refusal(host, ip)
		}

		return nil
	}

	addrs, err := resolver(p).LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("%s could not be resolved", host)
	}

	if len(addrs) == 0 {
		return fmt.Errorf("%s resolves to no address", host)
	}

	// Every answer has to be acceptable, not just the first: a name that
	// answers with one public and one internal address would otherwise decide
	// for itself which one the browser connects to.
	for _, a := range addrs {
		if !isPublic(a.IP) {
			return refusal(host, a.IP)
		}
	}

	return nil
}

// isLoopbackName reports whether a name is reserved for this machine.
func isLoopbackName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))

	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

func resolver(p Policy) Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}

	return net.DefaultResolver
}

// refusal says which address was refused, because "not allowed" without the
// address is not something a reader can act on — a name that resolves
// internally is exactly the case worth seeing.
func refusal(host string, ip net.IP) error {
	return fmt.Errorf("%s resolves to %s, which is not a public internet address; "+
		"wsaw will not fetch it on request. Configure it as a target, "+
		"or set api.adHocUrls.allowPrivateHosts for this deployment", host, ip)
}

// isPublic reports whether an address is one the public internet routes to.
//
// The unroutable and special-purpose ranges are all refused together: what
// they have in common is that reaching them means reaching something inside
// this deployment's own network rather than the website somebody asked about.
func isPublic(ip net.IP) bool {
	if ip == nil {
		return false
	}

	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		return isPublicV4(ip4)
	}

	return true
}

// isPublicV4 covers the IPv4 special-purpose ranges the standard library's
// predicates do not: "this network", carrier-grade NAT, the IETF protocol
// assignments block, and the benchmarking range.
func isPublicV4(ip net.IP) bool {
	switch {
	case ip[0] == 0:
		return false
	case ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127:
		return false
	case ip[0] == 192 && ip[1] == 0 && ip[2] == 0:
		return false
	case ip[0] == 198 && (ip[1] == 18 || ip[1] == 19):
		return false
	default:
		return true
	}
}

// Name derives the target name a URL's results are stored under. It is empty
// when nothing usable can be derived.
//
// Stability is the point: repeated scans of the same URL share one history,
// and a scan started from the web interface lands in the same series as
// `wsaw scan --url` of the same address.
func Name(rawURL string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(rawURL), "https://"), "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")

	var b strings.Builder

	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	name := strings.Trim(b.String(), "-.")

	// Truncated without re-trimming: a trailing dash is a valid name, and
	// trimming one off here would give a long URL a different name than the
	// one `wsaw scan --url` has always stored it under.
	if len(name) > maxName {
		name = name[:maxName]
	}

	return name
}
