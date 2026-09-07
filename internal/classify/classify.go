// Package classify decides whether an observed request belongs to the
// scanned site or to a third party.
//
// Classification uses the public suffix list rather than string matching:
// "evil-example.com" is not first-party to "example.com", and
// "foo.github.io" is not first-party to "bar.github.io" even though both sit
// under github.io.
package classify

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Classifier answers first-party questions for one target.
type Classifier struct {
	// firstPartyDomains holds registrable domains treated as the site's own.
	firstPartyDomains map[string]struct{}
}

// New builds a Classifier for the target URL. Additional domains — CDNs the
// site owner controls — are treated as first-party too, reduced to their
// registrable domain so that subdomains are covered.
func New(targetURL string, additional []string) (*Classifier, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("parsing target URL %q: %w", targetURL, err)
	}

	host := hostname(u)
	if host == "" {
		return nil, fmt.Errorf("target URL %q has no host", targetURL)
	}

	c := &Classifier{firstPartyDomains: make(map[string]struct{}, 1+len(additional))}
	c.firstPartyDomains[RegistrableDomain(host)] = struct{}{}

	for _, d := range additional {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" {
			continue
		}

		// Accept a bare domain, a wildcard, or a full URL, since operators
		// write all three in practice.
		d = strings.TrimPrefix(d, "*.")

		if strings.Contains(d, "://") {
			if pu, err := url.Parse(d); err == nil && hostname(pu) != "" {
				d = hostname(pu)
			}
		}

		c.firstPartyDomains[RegistrableDomain(d)] = struct{}{}
	}

	return c, nil
}

// Host holds the classification of one hostname.
type Host struct {
	Host   string
	Domain string
	Party  model.Party
}

// Classify returns the host, registrable domain, and party for a URL. For
// URLs without a host — data:, blob:, about: — the host and domain are empty
// and the party is first, since nothing left the browser.
func (c *Classifier) Classify(raw string) Host {
	u, err := url.Parse(raw)
	if err != nil {
		return Host{Party: model.ThirdParty}
	}

	host := hostname(u)
	if host == "" {
		return Host{Party: model.FirstParty}
	}

	domain := RegistrableDomain(host)

	party := model.ThirdParty
	if _, ok := c.firstPartyDomains[domain]; ok {
		party = model.FirstParty
	}

	return Host{Host: host, Domain: domain, Party: party}
}

// ClassifyCookieDomain classifies a cookie domain, which may carry a leading
// dot and is never a URL.
func (c *Classifier) ClassifyCookieDomain(domain string) model.Party {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return model.FirstParty
	}

	if _, ok := c.firstPartyDomains[RegistrableDomain(domain)]; ok {
		return model.FirstParty
	}

	return model.ThirdParty
}

// FirstPartyDomains returns the configured first-party registrable domains,
// for echoing into a result.
func (c *Classifier) FirstPartyDomains() []string {
	out := make([]string, 0, len(c.firstPartyDomains))
	for d := range c.firstPartyDomains {
		out = append(out, d)
	}

	return out
}

// RegistrableDomain reduces a hostname to eTLD+1. Hosts that have no
// registrable domain — an IP literal, "localhost", an internal single-label
// name — are returned unchanged, because they are still a meaningful identity
// and discarding them would lose the observation.
func RegistrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return ""
	}

	// An IP literal has no registrable domain; it is its own identity.
	if isIPLiteral(host) {
		return host
	}

	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}

	return domain
}

func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") {
		return true
	}

	// A hostname cannot consist only of digits and dots, so this is an IPv4
	// literal or something already invalid.
	for _, r := range host {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}

	return true
}

// hostname returns the lowercase hostname of u, without port or brackets.
func hostname(u *url.URL) string {
	return strings.ToLower(u.Hostname())
}
