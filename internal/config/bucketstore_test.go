package config_test

import (
	"strings"
	"testing"
)

// Configuring the store that keeps its index in the bucket (Story 8.10, AC1).
//
// The point of these is that the choice is one line and that everything which
// would only be true of a database is refused rather than quietly ignored. An
// operator switching driver leaves settings behind, and a dsn that sits there
// looking effective is worse than one that fails the load (Tenet 15).

const bucketStoreTargets = `
targets:
  - name: example
    url: https://example.com/
`

// TestTheBucketIndexStoreIsSelectedByOneLine is AC1's first half.
func TestTheBucketIndexStoreIsSelectedByOneLine(t *testing.T) {
	t.Parallel()

	cfg := parse(t, bucketStoreTargets+`
store:
  driver: blob
  artifactURL: "file:///srv/evidence"
`)

	if got := cfg.Store.StoreDriver(); got != "blob" {
		t.Errorf("driver = %q, want blob", got)
	}

	if !cfg.Store.IsBucketStore() {
		t.Error("a blob store is not recognised as one that keeps its index in the bucket")
	}

	// The question that decides whether a DSN is required. A bucket store is
	// not a server store, and asking it for a connection string would refuse a
	// configuration that is correct.
	if cfg.Store.IsServerStore() {
		t.Error("a blob store is treated as a database with a server")
	}
}

// TestTheDefaultStoreIsStillSQLite is AC1's second half, and the one worth
// asserting explicitly: a deployment that does not ask for the bucket index
// must not get it (Story 4.7, AC2).
func TestTheDefaultStoreIsStillSQLite(t *testing.T) {
	t.Parallel()

	cfg := parse(t, bucketStoreTargets)

	if got := cfg.Store.StoreDriver(); got != "sqlite" {
		t.Errorf("driver = %q, want sqlite", got)
	}

	if cfg.Store.IsBucketStore() {
		t.Error("a configuration that says nothing about its store gets the bucket index")
	}
}

// TestABucketStoreNeedsSomewhereToKeepItsIndex: there is no default to fall
// back on. For the other drivers the artifact location can be derived — beside
// the database file, or in the state directory — but here the bucket is the
// store, and deriving one would start a history somewhere the operator never
// named.
func TestABucketStoreNeedsSomewhereToKeepItsIndex(t *testing.T) {
	t.Parallel()

	err := parseErr(t, bucketStoreTargets+`
store:
  driver: blob
`)

	for _, want := range []string{"store.artifactURL", "artifactDir", "line 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
}

// TestABucketStoreRefusesWhatOnlyADatabaseHas covers the settings an operator
// leaves behind when switching driver. Each names the line it was written on,
// because "invalid configuration" is not something anyone can act on.
func TestABucketStoreRefusesWhatOnlyADatabaseHas(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body     string
		contains []string
	}{
		"a connection string": {
			body:     "  dsn: postgres://wsaw@db/wsaw\n",
			contains: []string{"store.dsn", "line 9", "no database"},
		},
		"a database file": {
			body:     "  path: /var/lib/wsaw/wsaw.db\n",
			contains: []string{"store.path", "line 9", "keeps its index in the bucket"},
		},
		"a connection pool": {
			body:     "  maxOpenConns: 8\n",
			contains: []string{"store.maxOpenConns", "line 9", "opens no connections"},
		},
		"an idle pool bound alone": {
			// The message points at what was actually written rather than at
			// the first of the three names it could have been.
			body:     "  maxIdleConns: 2\n",
			contains: []string{"store.maxIdleConns", "line 9"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := parseErr(t, bucketStoreTargets+`
store:
  driver: blob
  artifactURL: "file:///srv/evidence"
`+tc.body)

			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not contain %q: %v", want, err)
				}
			}
		})
	}
}

// TestAnUnknownDriverStillOffersTheBucketIndex: the list an operator is shown
// after a typo has to include the driver they may have been reaching for.
func TestAnUnknownDriverStillOffersTheBucketIndex(t *testing.T) {
	t.Parallel()

	err := parseErr(t, bucketStoreTargets+`
store:
  driver: bucket
`)

	if !strings.Contains(err.Error(), "blob") {
		t.Errorf("the refusal does not offer the blob driver: %v", err)
	}
}

// TestABucketStoreKeepsItsEvidenceWhereItIsTold is the ordinary case, and the
// one that says the two settings mean the same thing here as everywhere else:
// a directory on local disk is as valid as a bucket URL, which is what makes
// the store testable and what makes a single-machine deployment possible
// without object storage at all.
func TestABucketStoreKeepsItsEvidenceWhereItIsTold(t *testing.T) {
	t.Parallel()

	cfg := parse(t, bucketStoreTargets+`
store:
  driver: blob
  artifactDir: /var/lib/wsaw/artifacts
`)

	if got := cfg.Store.ArtifactLocation(); got != "/var/lib/wsaw/artifacts" {
		t.Errorf("artifact location = %q", got)
	}
}
