package scanner

import (
	"net/url"
	"strings"

	"github.com/martint17r/wsaw/internal/classify"
)

// hostAndDomain extracts the host and registrable domain of a target URL, for
// matching consent rules against.
func hostAndDomain(rawURL string) (host, domain string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}

	host = strings.ToLower(u.Hostname())

	return host, classify.RegistrableDomain(host)
}
