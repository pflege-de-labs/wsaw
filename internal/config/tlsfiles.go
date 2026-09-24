package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"time"
)

// CheckTLSFiles reads the certificate and key the interface is configured
// with and reports whether they would be served: both readable, both PEM that
// parses, the key belonging to the certificate, and the certificate not yet
// expired (Story 5.33, AC11). Without TLS, or without the API, there is
// nothing to check.
//
// It is separate from Validate on purpose. Validate is what every command
// runs, from any user, and never touches anything but the configuration
// itself; a key file is usually readable only by the user the daemon runs as,
// so reading it there would fail `wsaw scan` and `wsaw ui` over a file neither
// uses. This is called where a configuration is checked for serving: `wsaw
// config` and a reload of the running daemon.
//
// The paths are the operator's configuration, never anything a scanned page
// chose.
func (c *Config) CheckTLSFiles(now time.Time) error {
	if !c.API.Enabled || !c.API.TLSEnabled() {
		return nil
	}

	var errs Errors

	add := func(field, format string, args ...any) {
		errs = append(errs, &Error{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	certPEM, certErr := os.ReadFile(c.API.TLSCert) // #nosec G304 -- api.tlsCert, from the operator's configuration
	if certErr != nil {
		add("api.tlsCert", "%v", certErr)
	}

	keyPEM, keyErr := os.ReadFile(c.API.TLSKey) // #nosec G304 -- api.tlsKey, from the operator's configuration
	if keyErr != nil {
		add("api.tlsKey", "%v", keyErr)
	}

	if len(errs) > 0 {
		return errs
	}

	// X509KeyPair rejects a PEM that does not parse and a key that does not
	// belong to the certificate; its errors describe the mismatch, never the
	// key.
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		add("api.tlsCert", "%s and api.tlsKey %s are not a usable pair: %v", c.API.TLSCert, c.API.TLSKey, err)

		return errs
	}

	if pair.Leaf != nil && !now.Before(pair.Leaf.NotAfter) {
		add("api.tlsCert", "the certificate in %s expired at %s", c.API.TLSCert,
			pair.Leaf.NotAfter.UTC().Format(time.RFC3339))

		return errs
	}

	return nil
}
