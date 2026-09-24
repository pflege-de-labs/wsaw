package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/metrics"
)

// The certificate reload of Story 5.33, driven without a real clock and
// without a listener where one is not the point. Every certificate is made
// here; nothing leaves the machine.

// epoch is the fake clock's starting point. The generated certificates are
// placed around it, so the tests do not depend on today's date.
var epoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = t
}

// logRecord is what a test asserts on: the level and the message.
type logRecord struct {
	level slog.Level
	msg   string
	text  string
}

// logRecorder is a slog.Handler that keeps every record.
type logRecorder struct {
	mu      sync.Mutex
	records []logRecord
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder

	b.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.String())

		return true
	})

	r.mu.Lock()
	defer r.mu.Unlock()

	r.records = append(r.records, logRecord{level: rec.Level, msg: rec.Message, text: b.String()})

	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// take returns the records since the last call and forgets them.
func (r *logRecorder) take() []logRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := r.records
	r.records = nil

	return out
}

// certFixture is a certificate and key on disk, rewritten as a test rotates
// them.
type certFixture struct {
	dir      string
	certPath string
	keyPath  string
}

func newCertFixture(t *testing.T) *certFixture {
	t.Helper()

	dir := t.TempDir()

	return &certFixture{dir: dir, certPath: filepath.Join(dir, "cert.pem"), keyPath: filepath.Join(dir, "key.pem")}
}

// issuedPair is one generated certificate and its key, as PEM.
type issuedPair struct {
	cert []byte
	key  []byte
}

// issue makes a self-signed certificate for 127.0.0.1 with the given serial
// and validity.
func issue(t *testing.T, serial int64, notBefore time.Time, life time.Duration) issuedPair {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "wsaw-test"},
		DNSNames:              []string{"wsaw.test"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(life),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return issuedPair{
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *certFixture) write(t *testing.T, p issuedPair) {
	t.Helper()

	writeFile(t, f.certPath, p.cert)
	writeFile(t, f.keyPath, p.key)
}

// harness is a reloader over a fixture, with the clock, the log and the
// metrics it reports to.
type harness struct {
	files   *certFixture
	clock   *fakeClock
	log     *logRecorder
	metrics *metrics.Registry
	certs   *certReloader
}

// newHarness loads a pair issued at the epoch and valid for life.
func newHarness(t *testing.T, life time.Duration) *harness {
	t.Helper()

	h := &harness{
		files:   newCertFixture(t),
		clock:   &fakeClock{now: epoch},
		log:     &logRecorder{},
		metrics: metrics.New("test"),
	}

	h.files.write(t, issue(t, 1, epoch, life))

	certs, err := newCertReloader(h.files.certPath, h.files.keyPath, slog.New(h.log), h.metrics, h.clock.Now)
	if err != nil {
		t.Fatalf("loading the first pair: %v", err)
	}

	h.certs = certs
	h.log.take()

	return h
}

func (h *harness) servedSerial() int64 {
	return h.certs.served.Load().leaf.SerialNumber.Int64()
}

func (h *harness) scrape(t *testing.T) string {
	t.Helper()

	var b strings.Builder

	if err := h.metrics.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}

	return b.String()
}

func levels(records []logRecord) []slog.Level {
	out := make([]slog.Level, 0, len(records))
	for _, r := range records {
		out = append(out, r.level)
	}

	return out
}

// TestAnUnusablePairFailsAtStartupNamingTheFile is AC1: every way the pair can
// be unusable is found before anything listens, with the setting and the
// path, not on the first handshake.
func TestAnUnusablePairFailsAtStartupNamingTheFile(t *testing.T) {
	t.Parallel()

	good := issue(t, 1, epoch, 24*time.Hour)
	other := issue(t, 2, epoch, 24*time.Hour)

	for name, tc := range map[string]struct {
		cert, key []byte
		missing   string // "cert" or "key": that file is not written
		want      []string
	}{
		"missing certificate": {key: good.key, missing: "cert", want: []string{"api.tlsCert", "cert.pem"}},
		"missing key":         {cert: good.cert, missing: "key", want: []string{"api.tlsKey", "key.pem"}},
		"not a PEM":           {cert: []byte("hello"), key: good.key, want: []string{"api.tlsCert", "cert.pem"}},
		"mismatched pair": {
			cert: good.cert, key: other.key,
			want: []string{"api.tlsCert", "api.tlsKey", "does not match"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newCertFixture(t)
			if tc.missing != "cert" {
				writeFile(t, f.certPath, tc.cert)
			}

			if tc.missing != "key" {
				writeFile(t, f.keyPath, tc.key)
			}

			_, err := newCertReloader(f.certPath, f.keyPath, slog.New(&logRecorder{}), nil, time.Now)
			if err == nil {
				t.Fatal("an unusable pair was accepted")
			}

			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestARenewedPairIsServedOnTheNextCheck is AC2 and AC5, over a real
// handshake: the listener is never reopened, and the next connection is
// handed the new certificate.
func TestARenewedPairIsServedOnTheNextCheck(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 90*24*time.Hour)

	addr := serveCertificates(t, h.certs)

	if got := handshakeSerial(t, addr); got != 1 {
		t.Fatalf("first handshake presented serial %d, want 1", got)
	}

	// A check with nothing renewed is silent and counts nothing.
	h.certs.check(t.Context())

	if got := h.log.take(); len(got) != 0 {
		t.Errorf("a check with nothing changed logged %v", got)
	}

	h.files.write(t, issue(t, 2, epoch, 90*24*time.Hour))
	h.certs.check(t.Context())

	if got := handshakeSerial(t, addr); got != 2 {
		t.Errorf("after the renewal the handshake presented serial %d, want 2", got)
	}

	logged := h.log.take()
	if len(logged) != 1 || logged[0].level != slog.LevelInfo || !strings.Contains(logged[0].text, "sha256=") ||
		!strings.Contains(logged[0].text, "wsaw.test") {
		t.Errorf("the reload logged %v, want one Info line with the fingerprint and DNS names", logged)
	}

	out := h.scrape(t)
	if !strings.Contains(out, `wsaw_tls_certificate_reloads_total{outcome="success"} 1`) {
		t.Errorf("the reload was not counted:\n%s", out)
	}
}

// serveCertificates serves TLS from the reloader on a loopback port, the way
// Server.Serve does: the certificate comes from GetCertificate alone.
// httptest's TLS server is not used because it installs a certificate of its
// own, which a handshake without SNI would be handed instead.
func serveCertificates(t *testing.T, certs *certReloader) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{
		Handler:           http.NotFoundHandler(),
		ReadHeaderTimeout: time.Second,
		TLSConfig:         &tls.Config{GetCertificate: certs.getCertificate, MinVersion: tls.VersionTLS12},
		// A client that closes straight after the handshake is what these
		// tests do on purpose; the server's complaint about it is noise.
		ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = srv.ServeTLS(ln, "", "") // returns ErrServerClosed on Close below
	}()

	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})

	return ln.Addr().String()
}

// handshakeSerial connects and reports the serial of the certificate the
// server presented. Verification is off because what is under test is which
// certificate was presented, not whether it is trusted.
func handshakeSerial(t *testing.T, addr string) int64 {
	t.Helper()

	dialer := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} // #nosec G402

	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = conn.Close() }()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("dialled a %T, not a TLS connection", conn)
	}

	return tlsConn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// TestASecretMountSwapIsNoticed is the Kubernetes case in AC2: the files are
// symlinks through ..data, and a rotation swaps that one link, so the paths
// wsaw was given never change and neither does anything's mtime but the
// link's.
func TestASecretMountSwapIsNoticed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for i, p := range []issuedPair{issue(t, 1, epoch, time.Hour), issue(t, 2, epoch, time.Hour)} {
		version := filepath.Join(dir, "..v"+string(rune('1'+i)))
		if err := os.Mkdir(version, 0o700); err != nil {
			t.Fatal(err)
		}

		writeFile(t, filepath.Join(version, "tls.crt"), p.cert)
		writeFile(t, filepath.Join(version, "tls.key"), p.key)
	}

	link := func(target string) {
		tmp := filepath.Join(dir, "..data_tmp")
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}

		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}

	link("..v1")

	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	clock := &fakeClock{now: epoch}

	certs, err := newCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"),
		slog.New(&logRecorder{}), nil, clock.Now)
	if err != nil {
		t.Fatal(err)
	}

	link("..v2")
	certs.check(t.Context())

	if got := certs.served.Load().leaf.SerialNumber.Int64(); got != 2 {
		t.Errorf("after the ..data swap the served serial is %d, want 2", got)
	}
}

// TestAHalfWrittenRenewalKeepsTheServedPair is AC4: a renewal tool writes the
// certificate, then the key, and a check between the two sees a pair that
// does not match. That is a failed reload, not a reason to stop serving, and
// it succeeds once the key catches up.
func TestAHalfWrittenRenewalKeepsTheServedPair(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 90*24*time.Hour)
	next := issue(t, 2, epoch, 90*24*time.Hour)

	writeFile(t, h.files.certPath, next.cert)
	h.certs.check(t.Context())

	if got := h.servedSerial(); got != 1 {
		t.Fatalf("a half-written renewal replaced the served pair with serial %d", got)
	}

	logged := h.log.take()
	if len(logged) != 1 || logged[0].level != slog.LevelWarn {
		t.Errorf("the failed reload logged %v, want one Warn line", logged)
	}

	// Nothing changed, so nothing is tried, logged or counted again.
	h.certs.check(t.Context())

	if got := h.log.take(); len(got) != 0 {
		t.Errorf("an unchanged failure was logged again: %v", got)
	}

	writeFile(t, h.files.keyPath, next.key)
	h.certs.check(t.Context())

	if got := h.servedSerial(); got != 2 {
		t.Errorf("once the key caught up the served serial is %d, want 2", got)
	}

	out := h.scrape(t)
	for _, want := range []string{
		`wsaw_tls_certificate_reloads_total{outcome="failure"} 1`,
		`wsaw_tls_certificate_reloads_total{outcome="success"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestAnExpiredReplacementIsRefused is AC4: a certificate that has already
// expired is not an improvement on one that has not, however new the file.
func TestAnExpiredReplacementIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 90*24*time.Hour)

	h.files.write(t, issue(t, 2, epoch.Add(-48*time.Hour), 24*time.Hour))
	h.certs.check(t.Context())

	if got := h.servedSerial(); got != 1 {
		t.Errorf("an expired replacement was served (serial %d)", got)
	}

	logged := h.log.take()
	if len(logged) != 1 || !strings.Contains(logged[0].text, "expired at") {
		t.Errorf("the refusal logged %v, want it to say the replacement expired", logged)
	}
}

// TestAFailedReloadIsAnErrorOnlyInTheLastFifthOfTheCertificate is AC4's
// threshold, for a certificate of the ordinary length and for a short-lived
// one: a failure is Warn while most of the served certificate is left, Error
// once less than 20% of it is, and the escalation of a failure that nobody
// fixed is logged once.
func TestAFailedReloadIsAnErrorOnlyInTheLastFifthOfTheCertificate(t *testing.T) {
	t.Parallel()

	for name, life := range map[string]time.Duration{
		"90-day certificate":  90 * 24 * time.Hour,
		"24-hour certificate": 24 * time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, life)

			// Half the lifetime left.
			h.clock.set(epoch.Add(life / 2))
			writeFile(t, h.files.certPath, []byte("not a certificate"))
			h.certs.check(t.Context())

			if got := levels(h.log.take()); len(got) != 1 || got[0] != slog.LevelWarn {
				t.Errorf("with half the certificate left the failure logged %v, want [WARN]", got)
			}

			// 10% left, and the failure still unfixed.
			h.clock.set(epoch.Add(life * 9 / 10))
			h.certs.check(t.Context())
			h.certs.check(t.Context())

			if got := levels(h.log.take()); len(got) != 1 || got[0] != slog.LevelError {
				t.Errorf("with 10%% left the unfixed failure logged %v, want one [ERROR]", got)
			}

			// A new failure inside the band is an Error from the start.
			writeFile(t, h.files.certPath, []byte("still not a certificate"))
			h.certs.check(t.Context())

			if got := levels(h.log.take()); len(got) != 1 || got[0] != slog.LevelError {
				t.Errorf("a new failure with 10%% left logged %v, want [ERROR]", got)
			}
		})
	}
}

// TestReloadTriesAgainAtOnce is AC3 and AC6: SIGHUP does not wait for the
// next check and does not skip a pair because it failed before, and it may
// move the pair to new paths.
func TestReloadTriesAgainAtOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 90*24*time.Hour)

	writeFile(t, h.files.certPath, []byte("not a certificate"))
	h.certs.check(t.Context())
	h.certs.reload(t.Context(), h.files.certPath, h.files.keyPath)

	if out := h.scrape(t); !strings.Contains(out, `wsaw_tls_certificate_reloads_total{outcome="failure"} 2`) {
		t.Errorf("SIGHUP did not try the failed pair again:\n%s", out)
	}

	moved := newCertFixture(t)
	moved.write(t, issue(t, 3, epoch, 90*24*time.Hour))
	h.log.take()

	h.certs.reload(t.Context(), moved.certPath, moved.keyPath)

	if got := h.servedSerial(); got != 3 {
		t.Errorf("after moving the pair the served serial is %d, want 3", got)
	}

	// And a SIGHUP with nothing to load is silent.
	h.log.take()
	h.certs.reload(t.Context(), moved.certPath, moved.keyPath)

	if got := h.log.take(); len(got) != 0 {
		t.Errorf("a reload that changed nothing logged %v", got)
	}
}

// TestTheExpiryGaugeFollowsTheServedCertificate is AC7: the gauge moves with
// a successful reload and stays put through a failed one.
func TestTheExpiryGaugeFollowsTheServedCertificate(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 24*time.Hour)

	first := epoch.Add(24 * time.Hour).Unix()
	if out := h.scrape(t); !strings.Contains(out, gaugeLine(first)) {
		t.Fatalf("the gauge does not read the first certificate's expiry:\n%s", out)
	}

	writeFile(t, h.files.certPath, []byte("not a certificate"))
	h.certs.check(t.Context())

	if out := h.scrape(t); !strings.Contains(out, gaugeLine(first)) {
		t.Errorf("a failed reload moved the gauge:\n%s", out)
	}

	h.files.write(t, issue(t, 2, epoch, 48*time.Hour))
	h.certs.check(t.Context())

	if out := h.scrape(t); !strings.Contains(out, gaugeLine(epoch.Add(48*time.Hour).Unix())) {
		t.Errorf("a successful reload did not move the gauge:\n%s", out)
	}
}

func gaugeLine(unix int64) string {
	return fmt.Sprintf("wsaw_tls_certificate_expiry_timestamp_seconds %d\n", unix)
}

// TestTheWatchStopsWithItsContext is AC8. The ticks are handed in, and a
// second tick is only taken once the first check has finished, which is what
// synchronises the test with the loop.
func TestTheWatchStopsWithItsContext(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 90*24*time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time)
	done := make(chan struct{})

	go func() {
		defer close(done)

		h.certs.watch(ctx, ticks)
	}()

	h.files.write(t, issue(t, 2, epoch, 90*24*time.Hour))

	ticks <- epoch
	ticks <- epoch

	if got := h.servedSerial(); got != 2 {
		t.Errorf("the watch did not load the renewal (serial %d)", got)
	}

	cancel()
	<-done
}
