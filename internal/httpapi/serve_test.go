package httpapi_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Serve is the path production actually takes: it binds a socket, chooses
// between plain HTTP and TLS, and shuts down when the process is asked to
// stop. Every other test in this package drives the handler in-process, so
// none of that was exercised anywhere in the repository — the server's only
// caller is cmd/wsaw, which has no test that starts it.

// newServer builds a server on its own store, without binding anything. The
// fixture cannot be reused here because it serves through httptest, which is
// precisely the layer these tests are meant to skip.
func newServer(t *testing.T, opts httpapi.Options) *httpapi.Server {
	t.Helper()

	dir := t.TempDir()

	st, err := store.Open(store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	reg := metrics.New("test")
	reg.SetReady(true, true)

	srv, err := httpapi.New(opts, httpapi.Deps{
		Store:   st,
		Metrics: reg,
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatal(err)
	}

	return srv
}

// freePort reserves an address, then releases it, so a test knows where the
// server will be before it starts. Reading Server.Addr while Serve is
// running would be a data race; this avoids needing to.
func freePort(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	return addr
}

// serveInBackground starts the server and returns a channel carrying whatever
// Serve returns. Receiving from that channel also synchronises with Serve, so
// Addr may be read once it has yielded.
func serveInBackground(t *testing.T, srv *httpapi.Server) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)

	go func() { errCh <- srv.Serve(ctx) }()

	return cancel, errCh
}

// awaitListener waits for the socket to accept. This is waiting on an
// external state — a listener that exists a moment after Serve is called —
// not a sleep standing in for synchronisation: there is no signal to wait on
// until something is listening on the port.
func awaitListener(t *testing.T, addr string) {
	t.Helper()

	dialer := net.Dialer{Timeout: time.Second}

	for {
		conn, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()

			return
		}

		select {
		case <-t.Context().Done():
			t.Fatalf("nothing accepted on %s: %v", addr, err)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestServeBindsASocketAndServesUntilCancelled(t *testing.T) {
	t.Parallel()

	addr := freePort(t)
	srv := newServer(t, httpapi.Options{Listen: addr})

	cancel, errCh := serveInBackground(t, srv)

	awaitListener(t, addr)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health over the socket = %d, want 200", resp.StatusCode)
	}

	_ = resp.Body.Close()

	// Cancellation is a graceful shutdown, not a failure: a restart must not
	// look like a crash in whatever is watching the process.
	cancel()

	if err := <-errCh; err != nil {
		t.Errorf("Serve returned %v on cancellation, want nil", err)
	}

	// Addr reports the bound address, which is the useful thing after a
	// port-zero bind.
	if got := srv.Addr(); got != addr {
		t.Errorf("Addr = %q, want %q", got, addr)
	}
}

// A port-zero listener is what a test or a sidecar deployment uses, and the
// bound port has to be discoverable afterwards or nothing can reach it.
func TestServeRecordsThePortItWasGiven(t *testing.T) {
	t.Parallel()

	srv := newServer(t, httpapi.Options{Listen: "127.0.0.1:0"})

	cancel, errCh := serveInBackground(t, srv)
	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}

	// Receiving from errCh synchronised with Serve, so this read is safe.
	host, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("Addr = %q: %v", srv.Addr(), err)
	}

	if host != "127.0.0.1" {
		t.Errorf("bound host = %q, want 127.0.0.1", host)
	}

	if port == "0" || port == "" {
		t.Errorf("Addr = %q: the resolved port was not recorded", srv.Addr())
	}
}

func TestServeReportsAnAddressItCannotBind(t *testing.T) {
	t.Parallel()

	// A port already in use. The other listener stays open for the duration,
	// so the bind cannot succeed by chance.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	srv := newServer(t, httpapi.Options{Listen: ln.Addr().String()})

	err = srv.Serve(t.Context())
	if err == nil {
		t.Fatal("Serve on an occupied port returned nil")
	}

	// The error has to name the address, or an operator cannot tell which
	// listener the deployment failed on.
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("error = %q, want it to name %s", err, ln.Addr())
	}
}

func TestServeUsesTLSWhenACertificateIsConfigured(t *testing.T) {
	t.Parallel()

	certPath, keyPath := selfSignedCert(t)
	addr := freePort(t)

	srv := newServer(t, httpapi.Options{Listen: addr, TLSCert: certPath, TLSKey: keyPath})

	cancel, errCh := serveInBackground(t, srv)
	defer cancel()

	awaitListener(t, addr)

	// The certificate is self-signed, so the test trusts it explicitly
	// rather than skipping verification: that the handshake completes with
	// this certificate is the assertion.
	pool := x509.NewCertPool()

	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}

	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("the generated certificate could not be parsed back")
	}

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS request failed, so the TLS branch did not serve: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health over TLS = %d, want 200", resp.StatusCode)
	}

	// Serving TLS is also what turns HSTS on, and the header is only
	// meaningful over TLS.
	if got := resp.Header.Get("Strict-Transport-Security"); got == "" {
		t.Error("no Strict-Transport-Security header on a TLS listener")
	}

	cancel()

	if err := <-errCh; err != nil {
		t.Errorf("Serve returned %v on cancellation, want nil", err)
	}
}

// selfSignedCert writes a certificate and key valid for 127.0.0.1 and
// returns their paths.
func selfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wsaw-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
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

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	write := func(path, blockType string, bytes []byte) {
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: bytes}), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(certPath, "CERTIFICATE", der)
	write(keyPath, "EC PRIVATE KEY", keyDER)

	return certPath, keyPath
}
