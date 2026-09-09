package store

import (
	"testing"

	"gocloud.dev/blob"
)

// The two halves of Story 8.8, AC4, from inside the package.
//
// `make verify-variants` proves the same thing from outside, by reading the
// released binaries, and that is the check a release depends on. It cannot say
// anything about behaviour, though: a binary that links no cloud SDK could
// still refuse an s3:// URL with a message that sends an operator to the source
// to work out why. So the assertion runs at both levels, and this is the level
// where the refusal has to be *useful* — and where the cloudblob build has to
// actually accept what the default one refuses.
//
// The two directions live in cloudschemes_default_test.go and
// cloudschemes_cloud_test.go, one behind each side of the build constraint,
// because a single test that branched on the tag would pass in whichever
// configuration nobody ran. Both are run by CI: the untagged suite on every
// push, and the tagged one by `make test-cloudblob` (Story 8.8, AC3).

// cloudSchemes are the three the tag decides. Each URL is well-formed and
// names a bucket, so a refusal can only be about the scheme — a URL with a
// path or no host would be refused by ValidateArtifactURL in both builds, and
// would prove nothing about either.
var cloudSchemes = map[string]string{
	"s3":     "s3://wsaw-evidence",
	"gs":     "gs://wsaw-evidence",
	"azblob": "azblob://wsaw-evidence",
}

// registeredSchemes reports which schemes this build's URL mux answers, which
// is the predicate openBucket and ValidateArtifactURL both consult before they
// hand a URL to gocloud. Reading it is how a test asks what was linked without
// opening anything.
func registeredSchemes(t *testing.T) map[string]bool {
	t.Helper()

	registered := make(map[string]bool)
	for _, s := range blob.DefaultURLMux().BucketSchemes() {
		registered[s] = true
	}

	// The control probe. Both builds link fileblob, and the test binary also
	// links memblob, so a mux that answers neither is not the mux these
	// assertions think they are reading and every absence below would be
	// passing for the wrong reason.
	if !registered[fileScheme] {
		t.Fatalf("the URL mux does not answer %q, so what it says about the cloud schemes means nothing", fileScheme)
	}

	return registered
}
