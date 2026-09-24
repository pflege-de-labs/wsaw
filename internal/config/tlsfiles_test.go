package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/certtest"
	"github.com/pflege-de-labs/wsaw/internal/config"
)

// CheckTLSFiles is Story 5.33, AC11: a configuration naming a certificate the
// server could not load is reported when it is checked, not first as a
// failed reload in the daemon's log.

func withTLSFiles(certPath, keyPath string) *config.Config {
	cfg := config.New()
	cfg.API.Enabled = true
	cfg.API.TLSCert, cfg.API.TLSKey = certPath, keyPath

	return cfg
}

func TestAUsablePairPassesTheCheck(t *testing.T) {
	t.Parallel()

	certPath, keyPath := certtest.Write(t)

	if err := withTLSFiles(certPath, keyPath).CheckTLSFiles(time.Now()); err != nil {
		t.Errorf("a usable pair was refused: %v", err)
	}
}

func TestThereIsNothingToCheckWithoutTLS(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent.pem")

	plain := config.New()
	plain.API.Enabled = true

	// The API off: the paths are never served, so they are never read.
	off := withTLSFiles(missing, missing)
	off.API.Enabled = false

	for name, cfg := range map[string]*config.Config{"plain HTTP": plain, "API off": off} {
		if err := cfg.CheckTLSFiles(time.Now()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAnUnusablePairFailsTheCheckNamingTheSetting(t *testing.T) {
	t.Parallel()

	now := time.Now()
	good := certtest.Issue(t, now.Add(-time.Minute), time.Hour)
	other := certtest.Issue(t, now.Add(-time.Minute), time.Hour)
	expired := certtest.Issue(t, now.Add(-48*time.Hour), 24*time.Hour)

	for name, tc := range map[string]struct {
		pair    certtest.Pair
		missing string // "cert" or "key": that file is removed after writing
		want    []string
	}{
		"missing certificate": {pair: good, missing: "cert", want: []string{"api.tlsCert:", "no such file"}},
		"missing key":         {pair: good, missing: "key", want: []string{"api.tlsKey:", "no such file"}},
		"not a PEM": {
			pair: certtest.Pair{Cert: []byte("not a certificate"), Key: good.Key},
			want: []string{"api.tlsCert:", "not a usable pair"},
		},
		"mismatched pair": {
			pair: certtest.Pair{Cert: good.Cert, Key: other.Key},
			want: []string{"api.tlsCert:", "api.tlsKey", "does not match"},
		},
		"expired": {pair: expired, want: []string{"api.tlsCert:", "expired at"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			certPath, keyPath := certtest.WritePair(t, tc.pair)

			switch tc.missing {
			case "cert":
				mustRemove(t, certPath)
			case "key":
				mustRemove(t, keyPath)
			}

			err := withTLSFiles(certPath, keyPath).CheckTLSFiles(now)
			if err == nil {
				t.Fatal("an unusable pair passed the check")
			}

			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
