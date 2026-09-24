// Package certtest writes TLS certificates for tests, so that a test which
// needs a certificate on disk makes one rather than reaching for a real host
// (AGENTS.md §3.7).
//
// Nothing in wsaw imports this package outside a test, and nothing should.
package certtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pair is a certificate and its key, as PEM.
type Pair struct {
	Cert []byte
	Key  []byte
}

// Issue makes a self-signed certificate for 127.0.0.1, valid from notBefore
// for life.
func Issue(tb testing.TB, notBefore time.Time, life time.Duration) Pair {
	tb.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		tb.Fatal(err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "wsaw-test"},
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
		tb.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		tb.Fatal(err)
	}

	return Pair{
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

// Write issues a certificate valid for the next hour into a new temporary
// directory and returns the paths of the certificate and the key.
func Write(tb testing.TB) (certPath, keyPath string) {
	tb.Helper()

	return WritePair(tb, Issue(tb, time.Now().Add(-time.Minute), time.Hour))
}

// WritePair writes p into a new temporary directory and returns the paths.
func WritePair(tb testing.TB, p Pair) (certPath, keyPath string) {
	tb.Helper()

	dir := tb.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	for path, data := range map[string][]byte{certPath: p.Cert, keyPath: p.Key} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			tb.Fatal(err)
		}
	}

	return certPath, keyPath
}
