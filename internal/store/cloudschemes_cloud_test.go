//go:build cloudblob

package store

import (
	"strings"
	"testing"
)

// TestTheCloudBuildAcceptsCloudSchemes is the other direction of Story 8.8,
// AC4, and the reason the tag exists at all: the same three URLs the default
// build refuses are opened by this one.
//
// Opening is asserted, not merely the registry entry, because "the scheme is
// registered" and "wsaw can open a bucket with it" are two claims and only the
// second is the promise a release makes. Opening a bucket sends no request —
// gocloud does not verify that a bucket exists until something is read or
// written — so this stays inside the rule that a test makes no network call
// (AGENTS §3.7). What it does have to do is stop each provider's credential
// chain from reaching for an instance metadata service on its way to building
// a client, which is what the fixture environment below is for: static
// credentials where the SDK will take them, and a metadata endpoint pointed at
// a closed port on loopback where it will not.
//
// The endpoint in the s3 URL is a closed loopback port for the same reason. A
// request would be a bug in this test, and if one is ever made it fails
// immediately and locally instead of leaving the runner to find out.
func TestTheCloudBuildAcceptsCloudSchemes(t *testing.T) {
	registered := registeredSchemes(t)

	// The credentials are obvious fixtures and never leave the process. The
	// Azure key has to be valid base64 because the SDK decodes it while
	// constructing the client; nothing signs anything with it.
	cases := map[string]struct {
		url string
		env map[string]string
	}{
		"s3": {
			url: "s3://wsaw-evidence?region=us-east-1&endpoint=http://127.0.0.1:1" +
				"&use_path_style=true&disable_https=true",
			env: map[string]string{
				"AWS_EC2_METADATA_DISABLED": "true",
				"AWS_REGION":                "us-east-1",
				"AWS_ACCESS_KEY_ID":         "wsaw-test-access-key",
				"AWS_SECRET_ACCESS_KEY":     "wsaw-test-secret-key",
			},
		},
		"gs": {
			url: "gs://wsaw-evidence",
			env: map[string]string{"GCE_METADATA_HOST": "127.0.0.1:1"},
		},
		"azblob": {
			url: "azblob://wsaw-evidence",
			env: map[string]string{
				"AZURE_STORAGE_ACCOUNT": "wsawtest",
				"AZURE_STORAGE_KEY":     "d3Nhdy10ZXN0LWtleQ==",
			},
		},
	}

	// Not parallel, at any level: t.Setenv is process-wide and refuses to run
	// in a parallel test for exactly that reason.
	for scheme, tc := range cases {
		t.Run(scheme, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			if !registered[scheme] {
				t.Fatalf("the %q scheme is not registered in a build made with -tags cloudblob", scheme)
			}

			if err := ValidateArtifactURL(cloudSchemes[scheme]); err != nil {
				t.Errorf("configuration refused %s: %v", cloudSchemes[scheme], err)
			}

			b, err := openBucket(t.Context(), tc.url)
			if err != nil {
				t.Fatalf("openBucket(%s): %v", tc.url, err)
			}

			t.Cleanup(func() {
				if err := b.close(); err != nil {
					t.Errorf("close: %v", err)
				}
			})
		})
	}
}

// TestTheCloudBuildNamesItself checks the half of the refusal message that is
// about the binary rather than the URL.
//
// It matters in this direction too: the default build's message tells an
// operator to install the cloudblob build, and an operator already running it
// who has mistyped "s3a://" must not be sent to install what they have. The
// description is derived from the registered schemes rather than from the tag,
// so this is the assertion that the derivation reads the right way round.
func TestTheCloudBuildNamesItself(t *testing.T) {
	t.Parallel()

	if !strings.Contains(buildDescription(), "made with -tags cloudblob") {
		t.Errorf("this build describes itself as %q", buildDescription())
	}

	if hint := cloudBuildHint(); hint != "" {
		t.Errorf("this build tells an operator to go and get the cloud drivers: %q", hint)
	}

	err := ValidateArtifactURL("s3a://wsaw-evidence")
	if err == nil {
		t.Fatal("an unknown scheme was accepted")
	}

	if strings.Contains(err.Error(), "need a build made with -tags cloudblob") {
		t.Errorf("the refusal sends an operator to install the build they are running: %v", err)
	}
}
