package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/metrics"
)

// certReloader serves the interface's certificate from memory, and replaces
// it when the files it came from change (Story 5.33).
//
// http.Server.ServeTLS reads the pair once, when the listener opens, and
// never again. Every certificate an operator is likely to use is renewed by
// rewriting those files in place, so a daemon that only read them at startup
// went on serving the old certificate until somebody restarted it — or until
// it expired and clients refused it.
//
// A renewal is two writes, the certificate and then the key, and between
// them the files disagree. That moment, and every other way a renewal can be
// half-done, is a failed reload that leaves the pair being served where it
// is: a listener that stopped serving because a new certificate was not yet
// usable would turn a renewal into an outage.
type certReloader struct {
	logger  *slog.Logger
	metrics *metrics.Registry
	now     func() time.Time

	// served is read on every handshake, so it is swapped atomically rather
	// than behind mu.
	served atomic.Pointer[servedCert]

	// mu serialises attempts: the periodic check and a SIGHUP may arrive
	// together.
	mu       sync.Mutex
	certPath string
	keyPath  string
	// tried is the content of the last attempt, so a pair that failed and
	// has not changed since is not loaded, logged and counted again on every
	// check. It is what the next write to either file is compared with.
	tried [sha256.Size]byte
	// failure is why the last attempt failed, nil once one succeeds.
	failure error
	// escalated records that the failure has been reported at Error, so
	// the escalation is logged once rather than on every check.
	escalated bool
}

// servedCert is a loaded pair and what a log line or the gauge needs from it.
type servedCert struct {
	pair *tls.Certificate
	leaf *x509.Certificate
	// content is the digest of the two files it was loaded from.
	content [sha256.Size]byte
}

// errorShare is how much of the served certificate's validity may be left
// before a failing reload is reported at Error rather than Warn. A share of
// the lifetime, not a number of days, so it scales with the certificate:
// about 18 days of a 90-day one and under 5 hours of a 24-hour one. A fixed
// number of days would put a short-lived certificate in the Error band from
// the moment it is issued (Story 5.33, AC4).
const errorShare = 0.2

// newCertReloader loads the pair the listener will serve. Every way the pair
// can be unusable fails here, before anything listens, naming the setting
// and the path (Story 5.33, AC1).
func newCertReloader(certPath, keyPath string, logger *slog.Logger, reg *metrics.Registry,
	now func() time.Time,
) (*certReloader, error) {
	c := &certReloader{logger: logger, metrics: reg, now: now, certPath: certPath, keyPath: keyPath}

	certPEM, keyPEM, sum, err := readPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}

	loaded, err := parsePair(certPEM, keyPEM, sum, certPath, keyPath)
	if err != nil {
		return nil, err
	}

	c.served.Store(loaded)
	c.tried = sum
	c.setExpiry(loaded)

	attrs := certAttrs(loaded)
	if !now().Before(loaded.leaf.NotAfter) {
		// Not refused: the interface is how an operator finds out what the
		// daemon is doing, and scanning does not depend on it. But a client
		// will refuse this certificate, which somebody has to fix.
		logger.Error("the TLS certificate being served has expired; renew it and wsaw will load the new one", attrs...)
	} else {
		logger.Info("tls certificate loaded", attrs...)
	}

	return c, nil
}

// getCertificate is tls.Config.GetCertificate. A handshake gets whichever
// pair is being served when it starts; one already negotiated keeps its own.
func (c *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.served.Load().pair, nil
}

// watch checks the files on every tick until ctx ends. The ticks are handed
// in so that a test drives the loop without a real clock.
func (c *certReloader) watch(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.check(ctx)
		}
	}
}

// check loads the pair again if either file changed since the last attempt.
func (c *certReloader) check(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.attempt(false)
}

// reload loads the pair from the given paths at once, whether or not the
// files changed. It is what SIGHUP does (Story 5.33, AC3, AC6): a renewal
// hook's way of saying the new certificate is there now, and a reloaded
// configuration's way of moving it.
func (c *certReloader) reload(ctx context.Context, certPath, keyPath string) {
	if ctx.Err() != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.certPath, c.keyPath = certPath, keyPath
	c.attempt(true)
}

// attempt is check and reload once mu is held. Forced, it tries again what
// the last attempt already tried; otherwise an unchanged failure is only
// re-examined for whether it has become urgent.
func (c *certReloader) attempt(force bool) {
	certPEM, keyPEM, sum, err := readPair(c.certPath, c.keyPath)
	served := c.served.Load()

	if err == nil && sum == served.content {
		// The files hold what is already served — nothing was renewed, or a
		// half-done renewal was rolled back. Either way nothing is wrong any
		// more, and nothing changed that is worth a log line (AC5).
		c.tried, c.failure, c.escalated = sum, nil, false

		return
	}

	if !force && sum == c.tried {
		c.escalate(served)

		return
	}

	c.tried = sum

	var next *servedCert

	if err == nil {
		next, err = parsePair(certPEM, keyPEM, sum, c.certPath, c.keyPath)
	}

	if err == nil && !c.now().Before(next.leaf.NotAfter) {
		err = fmt.Errorf("the certificate in api.tlsCert %s expired at %s",
			c.certPath, next.leaf.NotAfter.UTC().Format(time.RFC3339))
	}

	if err != nil {
		c.fail(served, err)

		return
	}

	c.served.Store(next)
	c.failure, c.escalated = nil, false
	c.setExpiry(next)
	c.count(metrics.TLSReloadSuccess)
	c.logger.Info("tls certificate reloaded", certAttrs(next)...)
}

// fail records an attempt that left the served pair in place.
func (c *certReloader) fail(served *servedCert, err error) {
	c.failure = err
	c.count(metrics.TLSReloadFailure)

	attrs := []any{
		"cert", c.certPath,
		"key", c.keyPath,
		"served_not_after", served.leaf.NotAfter.UTC(),
		"error", err,
	}

	c.escalated = c.nearExpiry(served)
	if c.escalated {
		c.logger.Error("the renewed TLS certificate could not be loaded and the one being served expires soon; "+
			"still serving it", attrs...)

		return
	}

	c.logger.Warn("the renewed TLS certificate could not be loaded; still serving the current one, "+
		"and trying again when either file changes", attrs...)
}

// escalate reports, once, a failure that has not been fixed by the time the
// certificate still being served enters the last share of its validity.
func (c *certReloader) escalate(served *servedCert) {
	if c.failure == nil || c.escalated || !c.nearExpiry(served) {
		return
	}

	c.escalated = true
	c.logger.Error("the renewed TLS certificate still cannot be loaded and the one being served expires soon; "+
		"still serving it",
		"cert", c.certPath,
		"key", c.keyPath,
		"served_not_after", served.leaf.NotAfter.UTC(),
		"error", c.failure)
}

// nearExpiry reports whether less than errorShare of the certificate's
// validity period is left.
func (c *certReloader) nearExpiry(s *servedCert) bool {
	life := s.leaf.NotAfter.Sub(s.leaf.NotBefore)
	if life <= 0 {
		return true
	}

	left := s.leaf.NotAfter.Sub(c.now())

	return float64(left) < errorShare*float64(life)
}

func (c *certReloader) count(outcome string) {
	if c.metrics != nil {
		c.metrics.TLSCertificateReloaded(outcome)
	}
}

func (c *certReloader) setExpiry(s *servedCert) {
	if c.metrics != nil {
		c.metrics.SetTLSCertificateExpiry(s.leaf.NotAfter)
	}
}

// readPair reads both files and digests what it read. A file that cannot be
// read digests as its error, so a file that stays missing is one failed
// attempt rather than one per check, and the file reappearing is a change.
//
// The paths are the operator's configuration, never anything a scanned page
// chose.
func readPair(certPath, keyPath string) (certPEM, keyPEM []byte, sum [sha256.Size]byte, err error) {
	certPEM, err = os.ReadFile(certPath) // #nosec G304 -- api.tlsCert, from the operator's configuration
	if err != nil {
		err = fmt.Errorf("api.tlsCert: %w", err)

		return nil, nil, sha256.Sum256([]byte(err.Error())), err
	}

	keyPEM, err = os.ReadFile(keyPath) // #nosec G304 -- api.tlsKey, from the operator's configuration
	if err != nil {
		err = fmt.Errorf("api.tlsKey: %w", err)

		return nil, nil, sha256.Sum256([]byte(err.Error())), err
	}

	h := sha256.New()
	// PEM is ASCII, so a NUL cannot occur in either file and the boundary
	// between them is unambiguous.
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(keyPEM)
	h.Sum(sum[:0])

	return certPEM, keyPEM, sum, nil
}

// parsePair turns the two files into a servable pair. tls.X509KeyPair
// rejects a PEM that does not parse and a key that does not belong to the
// certificate; its errors describe the mismatch, never the key.
func parsePair(certPEM, keyPEM []byte, sum [sha256.Size]byte, certPath, keyPath string) (*servedCert, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("api.tlsCert %s and api.tlsKey %s: %w", certPath, keyPath, err)
	}

	// X509KeyPair fills in Leaf, and refuses a file without a certificate.
	// Parsing it again only matters under GODEBUG=x509keypairleaf=0.
	leaf := pair.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("api.tlsCert %s: %w", certPath, err)
		}
	}

	return &servedCert{pair: &pair, leaf: leaf, content: sum}, nil
}

// certAttrs is what identifies a served certificate in a log line: enough to
// tell a renewal took effect, and nothing from the key.
func certAttrs(s *servedCert) []any {
	fingerprint := sha256.Sum256(s.leaf.Raw)

	return []any{
		"subject", s.leaf.Subject.String(),
		"dns_names", s.leaf.DNSNames,
		"not_after", s.leaf.NotAfter.UTC(),
		"sha256", hex.EncodeToString(fingerprint[:]),
	}
}
