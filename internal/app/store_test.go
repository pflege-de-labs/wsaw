package app_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Where the evidence goes is resolved here rather than in the store, because
// this is the layer that knows where the state directory is (Story 8.6, AC1).

// TestArtifactsDefaultToTheDirectoryBesideTheStore is the criterion the
// single-binary deployment depends on: nothing configured has to keep meaning
// the artifacts directory beside the database, so an upgrade to the bucket
// world needs no configuration edit at all (Tenet 14).
func TestArtifactsDefaultToTheDirectoryBesideTheStore(t *testing.T) {
	t.Parallel()

	cases := map[string]*config.Config{
		// The state directory is deliberately not asserted by name: it differs
		// per platform, and what this test is about is the artifacts directory
		// sitting beside whichever database file was resolved.
		"an empty configuration": config.New(),
		"a configured database file": func() *config.Config {
			cfg := config.New()
			cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")

			return cfg
		}(),
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			opts, err := app.StoreOptions(cfg, nil)
			if err != nil {
				t.Fatalf("StoreOptions: %v", err)
			}

			want := filepath.Join(filepath.Dir(opts.Path), "artifacts")
			if opts.ArtifactDir != want {
				t.Errorf("artifacts resolve to %q, want %q", opts.ArtifactDir, want)
			}
		})
	}
}

func TestArtifactURLIsUsedAsConfigured(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")
	cfg.Store.ArtifactURL = "s3://wsaw-evidence?region=eu-central-1"

	opts, err := app.StoreOptions(cfg, nil)
	if err != nil {
		t.Fatalf("StoreOptions: %v", err)
	}

	if opts.ArtifactDir != cfg.Store.ArtifactURL {
		t.Errorf("artifacts resolve to %q, want the configured URL %q", opts.ArtifactDir, cfg.Store.ArtifactURL)
	}
}

// TestACredentialInTheBucketURLIsProtected is AC3. A query string is where an
// S3-compatible endpoint's keys go, so the printable form of the location must
// not have one however the URL was configured.
func TestACredentialInTheBucketURLIsProtected(t *testing.T) {
	t.Parallel()

	const url = "s3://wsaw-evidence?endpoint=minio.example.com&access_key_id=AKIAEXAMPLE&secret_access_key=s3cr3t-do-not-log"

	cfg := config.New()
	cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")
	cfg.Store.ArtifactURL = url

	secrets := &secret.Registry{}

	opts, err := app.StoreOptions(cfg, secrets)
	if err != nil {
		t.Fatalf("StoreOptions: %v", err)
	}

	// The bucket is opened with the credential intact: it is the URL the
	// provider needs.
	if opts.ArtifactDir != url {
		t.Errorf("the store is given %q, want the configured URL", opts.ArtifactDir)
	}

	printed := opts.ArtifactLocation()

	for _, leaked := range []string{"AKIAEXAMPLE", "s3cr3t-do-not-log", "access_key_id"} {
		if strings.Contains(printed, leaked) {
			t.Errorf("the printable location %q leaks %q", printed, leaked)
		}
	}

	if !strings.Contains(printed, secret.Redacted) {
		t.Errorf("the printable location %q does not say that something was removed", printed)
	}
}

// TestAPlainBucketURLStaysReadableInLogs is the other half of the same
// decision. Registering a URL that carries no credential would scrub the
// bucket's own name out of every line that names it, which protects nothing
// and costs an operator the one detail those lines exist for.
func TestAPlainBucketURLStaysReadableInLogs(t *testing.T) {
	t.Parallel()

	const url = "s3://wsaw-evidence?region=eu-central-1"

	cfg := config.New()
	cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")
	cfg.Store.ArtifactURL = url

	secrets := &secret.Registry{}

	if _, err := app.StoreOptions(cfg, secrets); err != nil {
		t.Fatalf("StoreOptions: %v", err)
	}

	if got := secrets.Scrub("artifacts=" + url); got != "artifacts="+url {
		t.Errorf("a bucket URL with no credential was scrubbed to %q", got)
	}
}

// TestArtifactURLResolvesASecretReference: a bucket URL that has to carry a
// credential belongs in the environment rather than in a file that gets
// committed, which is what makes it a secret reference like every other
// credential wsaw takes (AC3).
func TestArtifactURLResolvesASecretReference(t *testing.T) {
	const url = "s3://wsaw-evidence?access_key_id=AKIAEXAMPLE&secret_access_key=s3cr3t-do-not-log"

	// No t.Parallel: this sets an environment variable for the process.
	t.Setenv("WSAW_TEST_ARTIFACT_URL", url)

	cfg := config.New()
	cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")
	cfg.Store.ArtifactURL = "${env:WSAW_TEST_ARTIFACT_URL}"

	secrets := &secret.Registry{}

	opts, err := app.StoreOptions(cfg, secrets)
	if err != nil {
		t.Fatalf("StoreOptions: %v", err)
	}

	if opts.ArtifactDir != url {
		t.Errorf("the reference resolved to %q, want the URL from the environment", opts.ArtifactDir)
	}

	// A value its author called secret is scrubbed out of anything wsaw
	// prints, including text it did not write — an error from a provider's
	// SDK that quotes the URL back.
	if scrubbed := secrets.Scrub("opening " + url); strings.Contains(scrubbed, "s3cr3t-do-not-log") {
		t.Errorf("the resolved URL was not registered for scrubbing: %q", scrubbed)
	}
}

// TestAnInlineCredentialIsScrubbedFromWhatOthersPrint is the same protection
// for the URL an operator wrote out in full (AC3).
//
// The printable form of the location drops the query string, but the URL does
// not only travel through wsaw's own formatting: gocloud's openers quote the
// whole URL back in their errors — "open bucket %v: %v" — and wsaw wraps that
// error, logs it and prints it to stderr. So the credential-bearing parameters
// join the scrubber whether or not the operator called the setting a secret,
// while the bucket's own name stays readable.
func TestAnInlineCredentialIsScrubbedFromWhatOthersPrint(t *testing.T) {
	t.Parallel()

	const url = "s3://wsaw-evidence?region=eu-central-1" +
		"&access_key_id=AKIAEXAMPLE&secret_access_key=s3cr3t-do-not-log"

	cfg := config.New()
	cfg.Store.Path = filepath.Join(t.TempDir(), "wsaw.db")
	cfg.Store.ArtifactURL = url

	secrets := &secret.Registry{}

	if _, err := app.StoreOptions(cfg, secrets); err != nil {
		t.Fatalf("StoreOptions: %v", err)
	}

	// The shape of what a provider's own error looks like when it comes back
	// through wsaw's wrapping.
	scrubbed := secrets.Scrub("opening artifact bucket: open bucket " + url + ": invalid parameter")

	for _, leaked := range []string{"s3cr3t-do-not-log", "AKIAEXAMPLE"} {
		if strings.Contains(scrubbed, leaked) {
			t.Errorf("a third-party error quoting the URL leaks %q: %q", leaked, scrubbed)
		}
	}

	// The bucket is still identifiable, which is the whole reason those lines
	// are logged.
	if !strings.Contains(scrubbed, "wsaw-evidence") {
		t.Errorf("the bucket's own name was scrubbed out of %q", scrubbed)
	}

	if !strings.Contains(scrubbed, "eu-central-1") {
		t.Errorf("the region was scrubbed out of %q, which protects nothing", scrubbed)
	}
}

// TestAnUnresolvableArtifactURLFailsAtStartup: a reference to a variable
// nobody set must fail while wsaw is starting, naming the setting, rather than
// leaving the store to open a bucket called "${env:...}".
func TestAnUnresolvableArtifactURLFailsAtStartup(t *testing.T) {
	t.Parallel()

	cfg := config.New()
	cfg.Store.ArtifactURL = "${env:WSAW_TEST_ARTIFACT_URL_THAT_IS_NOT_SET}"

	_, err := app.StoreOptions(cfg, nil)
	if err == nil {
		t.Fatal("StoreOptions accepted a reference that cannot be resolved")
	}

	if !strings.Contains(err.Error(), "store.artifactURL") {
		t.Errorf("the failure %q does not name the setting", err)
	}
}
