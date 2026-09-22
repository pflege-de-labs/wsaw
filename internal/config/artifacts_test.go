package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

// Where evidence goes is configured by one of two settings, and this file is
// the specification of which values each accepts (Story 8.6, AC1, AC2, AC4).

// TestArtifactLocationDefaultsToNothingConfigured is AC1's half that has to
// keep working: a configuration that says nothing about storage is valid, and
// says nothing. What it resolves to — the artifacts directory beside the
// database — is the app package's decision and is asserted there.
func TestArtifactLocationDefaultsToNothingConfigured(t *testing.T) {
	t.Parallel()

	cfg := parse(t, `
targets:
  - name: example
    url: https://example.com/
`)

	if got := cfg.Store.ArtifactLocation(); got != "" {
		t.Errorf("an unconfigured store names %q as its artifact location, want nothing", got)
	}
}

func TestArtifactLocationPrefersTheURL(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body string
		want string
	}{
		"a directory": {
			body: "store:\n  artifactDir: /var/lib/wsaw/artifacts\n",
			want: "/var/lib/wsaw/artifacts",
		},
		"a bucket URL": {
			body: "store:\n  artifactURL: \"file:///var/lib/wsaw/artifacts\"\n",
			want: "file:///var/lib/wsaw/artifacts",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := parse(t, tc.body)

			if got := cfg.Store.ArtifactLocation(); got != tc.want {
				t.Errorf("artifact location = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestArtifactURLIsValidated covers the values an operator can write and what
// each of them is told. A rejection has to name the line and say what to do,
// because a scheme this build cannot open is not a mistake anybody can debug
// from "invalid configuration" (Tenet 15).
func TestArtifactURLIsValidated(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body     string
		accepted bool
		// contains is asserted against the rejection message.
		contains []string
	}{
		"a local directory": {
			body:     "store:\n  artifactDir: /var/lib/wsaw/artifacts\n",
			accepted: true,
		},
		"a file URL": {
			body:     "store:\n  artifactURL: \"file:///var/lib/wsaw/artifacts\"\n",
			accepted: true,
		},
		"a file URL naming localhost": {
			body:     "store:\n  artifactURL: \"file://localhost/var/lib/wsaw/artifacts\"\n",
			accepted: true,
		},
		"a secret reference, which is not a URL yet": {
			// Resolved when the store is opened, and validated there. Reading
			// the environment here would make `wsaw config --check` answer
			// differently depending on which shell ran it.
			body:     "store:\n  artifactURL: \"${env:WSAW_ARTIFACT_URL}\"\n",
			accepted: true,
		},
		"a scheme no build supports": {
			body:     "store:\n  artifactURL: \"ftp://evidence/bucket\"\n",
			contains: []string{"line 2", "store.artifactURL", "ftp", "this build"},
		},
		"a malformed URL": {
			body:     "store:\n  artifactURL: \"s3://bucket/%zz\"\n",
			contains: []string{"line 2", "store.artifactURL", "not a valid URL"},
		},
		"a bucket URL with no bucket": {
			body:     "store:\n  artifactURL: \"file://\"\n",
			contains: []string{"store.artifactURL", "names no directory"},
		},
		"a file URL naming another host": {
			body:     "store:\n  artifactURL: \"file://elsewhere/artifacts\"\n",
			contains: []string{"store.artifactURL", "elsewhere"},
		},
		"a directory setting holding a URL": {
			body:     "store:\n  artifactDir: \"file:///var/lib/wsaw/artifacts\"\n",
			contains: []string{"line 2", "store.artifactDir", "store.artifactURL"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tc.accepted {
				parse(t, tc.body)

				return
			}

			err := parseErr(t, tc.body)

			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the rejection %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestBothArtifactSettingsAreRefused is AC2's second half. Preferring one
// would leave the other looking as though it were in force, which is how
// evidence ends up somewhere nobody goes looking for it.
func TestBothArtifactSettingsAreRefused(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
store:
  artifactDir: /var/lib/wsaw/artifacts
  artifactURL: "file:///srv/evidence"
`)

	// Both lines are named: either one could be the one to delete, and the
	// operator is the only one who knows which.
	for _, want := range []string{"line 4", "store.artifactURL", "artifactDir (line 3)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection %q does not mention %q", err, want)
		}
	}
}

// TestArtifactURLKeepsCredentialsOutOfTheMessage is AC3 at the point it is
// easiest to get wrong: a configuration error prints the value that caused it,
// and a bucket URL's query string is where an S3-compatible endpoint's keys
// live.
func TestArtifactURLKeepsCredentialsOutOfTheMessage(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
store:
  artifactURL: "ftp://minio.example.com/evidence?access_key_id=AKIAEXAMPLE&secret_access_key=s3cr3t-do-not-log"
`)

	for _, leaked := range []string{"AKIAEXAMPLE", "s3cr3t-do-not-log", "access_key_id"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("the rejection %q leaks %q from the URL", err, leaked)
		}
	}

	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("the rejection %q does not show the URL as redacted", err)
	}
}

// TestUnknownStoreSettingIsRejected guards the strict decoding that the store
// section's own unmarshaller could quietly have dropped: it decodes the
// section itself in order to record where each setting was written, and a
// misspelled key that does nothing is indistinguishable from a setting that
// does not work.
func TestUnknownStoreSettingIsRejected(t *testing.T) {
	t.Parallel()

	err := parseErr(t, `
store:
  artifactUrl: "file:///srv/evidence"
`)

	if !strings.Contains(err.Error(), "artifactUrl") {
		t.Errorf("the rejection %q does not name the misspelled setting", err)
	}
}

// TestSignedArtifactURLsAreValidatedWithTheBucket is Story 8.7, AC3: the
// redirect setting is configured where the bucket is and refused at load, with
// the line to change, when it says something that cannot happen.
func TestSignedArtifactURLsAreValidatedWithTheBucket(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body     string
		accepted bool
		contains []string
	}{
		"off, which is the default": {
			body:     "store:\n  artifactURL: \"file:///srv/evidence\"\n",
			accepted: true,
		},
		"on, with a bucket and the default lifetime": {
			body:     "store:\n  artifactURL: \"file:///srv/evidence\"\n  artifactSignedURLs: true\n",
			accepted: true,
		},
		"on, with a lifetime of its own": {
			body: "store:\n  artifactURL: \"file:///srv/evidence\"\n" +
				"  artifactSignedURLs: true\n  artifactSignedURLTTL: 30m\n",
			accepted: true,
		},
		"a lifetime without the switch": {
			body:     "store:\n  artifactURL: \"file:///srv/evidence\"\n  artifactSignedURLTTL: 2m\n",
			contains: []string{"line 3", "store.artifactSignedURLTTL", "artifactSignedURLs: true"},
		},
		"on, against a local directory": {
			body:     "store:\n  artifactDir: /var/lib/wsaw/artifacts\n  artifactSignedURLs: true\n",
			contains: []string{"line 3", "store.artifactSignedURLs", "store.artifactURL"},
		},
		"on, against the default location": {
			// Nothing configured is the artifacts directory beside the
			// database, which is local disk and cannot sign either.
			body:     "store:\n  artifactSignedURLs: true\n",
			contains: []string{"store.artifactSignedURLs", "local disk"},
		},
		"a lifetime beyond the ceiling": {
			body: "store:\n  artifactURL: \"file:///srv/evidence\"\n" +
				"  artifactSignedURLs: true\n  artifactSignedURLTTL: 8h\n",
			contains: []string{"line 4", "store.artifactSignedURLTTL", "1h0m0s", "cannot be withdrawn"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tc.accepted {
				parse(t, tc.body)

				return
			}

			err := parseErr(t, tc.body)

			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the rejection %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestSignedArtifactURLLifetimeDefaults keeps the default short without
// requiring anyone to write it down.
func TestSignedArtifactURLLifetimeDefaults(t *testing.T) {
	t.Parallel()

	cfg := parse(t, "store:\n  artifactURL: \"file:///srv/evidence\"\n  artifactSignedURLs: true\n")

	if got := cfg.Store.SignedURLTTL(); got != config.DefaultArtifactSignedURLTTL {
		t.Errorf("the default signed-URL lifetime is %s, want %s", got, config.DefaultArtifactSignedURLTTL)
	}

	cfg = parse(t, "store:\n  artifactURL: \"file:///srv/evidence\"\n"+
		"  artifactSignedURLs: true\n  artifactSignedURLTTL: 45s\n")

	if got := cfg.Store.SignedURLTTL(); got != 45*time.Second {
		t.Errorf("the configured signed-URL lifetime is %s, want 45s", got)
	}
}
