package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

// How often the certificate files are checked for a renewal (Story 5.33,
// AC2).

const tlsAPI = "api:\n  enabled: true\n  tlsCert: cert.pem\n  tlsKey: key.pem\n"

func TestTheCertificateIsCheckedEveryMinuteByDefault(t *testing.T) {
	t.Parallel()

	cfg := parse(t, tlsAPI)

	if !cfg.API.TLSEnabled() {
		t.Error("a certificate and a key did not turn TLS on")
	}

	if got := cfg.API.TLSReloadEvery(); got != time.Minute {
		t.Errorf("the default check interval is %s, want 1m", got)
	}

	if got := parse(t, tlsAPI+"  tlsReloadInterval: 5m\n").API.TLSReloadEvery(); got != 5*time.Minute {
		t.Errorf("tlsReloadInterval: 5m reads as %s", got)
	}

	if config.New().API.TLSEnabled() {
		t.Error("the zero configuration serves TLS")
	}
}

func TestACertificateCheckIntervalThatCannotWorkIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ body, want string }{
		"under the floor": {tlsAPI + "  tlsReloadInterval: 5s\n", "shorter than the 10s minimum"},
		"not a duration":  {tlsAPI + "  tlsReloadInterval: often\n", "not a valid duration"},
		"without TLS": {
			"api:\n  enabled: true\n  tlsReloadInterval: 1m\n",
			"api.tlsReloadInterval: is set but TLS is off",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := parseErr(t, tc.body); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
