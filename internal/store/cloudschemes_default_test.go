//go:build !cloudblob

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestTheDefaultBuildRefusesCloudSchemes is the first direction of Story 8.8,
// AC4: a binary built without the tag reaches the local disk and nothing else,
// and it says so in a way an operator can act on.
//
// Every entry point that takes a bucket location is asserted, not just the
// lowest one. Configuration validation, the bucket opener, and both store
// constructors each consult the URL mux for themselves, so each could
// independently grow a branch that opened an s3:// URL — and the two store
// constructors are the ones an operator actually reaches, since a location that
// got past validation arrives there.
func TestTheDefaultBuildRefusesCloudSchemes(t *testing.T) {
	t.Parallel()

	registered := registeredSchemes(t)

	for scheme, location := range cloudSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()

			if registered[scheme] {
				t.Fatalf("the %q scheme is registered in a build made without -tags cloudblob", scheme)
			}

			// Configuration first, because this is where an operator meets the
			// refusal: `wsaw config --check` and startup both validate before
			// anything is opened (Story 8.6, AC4).
			assertNamesTheWayOut(t, ValidateArtifactURL(location), scheme)

			assertNamesTheWayOut(t, openBucketError(t, location), scheme)

			// The bucket-index store, which is the whole store: its index and
			// its evidence are both in the bucket the URL names.
			_, err := Open(t.Context(), Options{Driver: DriverBlob, ArtifactDir: location})
			assertNamesTheWayOut(t, err, scheme)

			// And a SQL store whose evidence was pointed at a bucket. It is
			// refused before the schema is touched, so the database is left as
			// it was — see connect().
			_, err = Open(t.Context(), Options{
				Driver:      DriverSQLite,
				Path:        filepath.Join(t.TempDir(), "wsaw.db"),
				ArtifactDir: location,
			})
			assertNamesTheWayOut(t, err, scheme)
		})
	}
}

// openBucketError opens a bucket that is expected not to open, and closes it if
// it does, so a failing assertion does not also leak a bucket.
func openBucketError(t *testing.T, location string) error {
	t.Helper()

	b, err := openBucket(t.Context(), location)
	if err == nil {
		t.Cleanup(func() {
			if err := b.close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}

	return err
}

// assertNamesTheWayOut checks that a refusal tells an operator what this build
// can open and how to get a build that opens more.
//
// Naming the scheme alone would leave "s3:// is not supported" as the whole
// message, which reads as a missing feature rather than as the build variant
// it is (Tenet 15). The three things asserted here are the three an operator
// needs: what they asked for, what they have, and what to install instead.
func assertNamesTheWayOut(t *testing.T, err error, scheme string) {
	t.Helper()

	if err == nil {
		t.Fatalf("the %s scheme was accepted by a build made without -tags cloudblob", scheme)
	}

	for _, want := range []string{scheme, fileScheme + schemeSeparator, "-tags cloudblob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal of a %s:// URL does not mention %q: %v", scheme, want, err)
		}
	}
}
