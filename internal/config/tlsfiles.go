package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

// CheckTLSFiles reads the certificate and key the interface is configured
// with and reports whether they could be served: both readable, both PEM that
// parses, and the key belonging to the certificate (Story 5.33, AC11). On
// success it returns when the certificate expires. Without TLS, or without
// the API, there is nothing to check, and the time is zero.
//
// An expired certificate is not an error here. The daemon serves one at
// startup and logs an error rather than refusing to start, and a check that
// failed where startup does not would block a reload over something a restart
// accepts. The caller compares the time with the clock and says so.
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
func (c *Config) CheckTLSFiles() (notAfter time.Time, err error) {
	if !c.API.Enabled || !c.API.TLSEnabled() {
		return time.Time{}, nil
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
		return time.Time{}, errs
	}

	// X509KeyPair rejects a PEM that does not parse and a key that does not
	// belong to the certificate; its errors describe the mismatch, never the
	// key.
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		add("api.tlsCert", "%s and api.tlsKey %s are not a usable pair: %v", c.API.TLSCert, c.API.TLSKey, err)

		return time.Time{}, errs
	}

	// X509KeyPair fills in Leaf, and refuses a file without a certificate.
	// Parsing it again only matters under GODEBUG=x509keypairleaf=0.
	leaf := pair.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			add("api.tlsCert", "%s: %v", c.API.TLSCert, err)

			return time.Time{}, errs
		}
	}

	return leaf.NotAfter, nil
}
