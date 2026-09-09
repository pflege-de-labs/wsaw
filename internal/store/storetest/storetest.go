// Package storetest gives other packages' tests a bucket the store itself
// cannot offer them, without letting a provider type out of the store's tree.
//
// Story 8.1, AC1 is that no provider-specific type appears outside the store
// package — the rule Story 4.6, AC1 set for the database driver, applied to
// storage — and that rule holds in tests too: modernc.org/sqlite, pgx and
// go-sql-driver/mysql appear only inside internal/store, its test files
// included. A gocloud.dev/blob import in internal/httpapi's tests would be a
// second place that knows what evidence is stored with, which is exactly the
// knowledge the seam exists to contain.
//
// So the one thing another package's tests genuinely need and cannot build for
// themselves — a bucket that can sign a URL, which Story 8.7, AC3's redirect
// path cannot otherwise be exercised against without a cloud account — lives
// here, beside the store, and everything gocloud stays under internal/store/.
//
// Nothing in wsaw imports this package outside a test, and nothing should: it
// registers a URL scheme on the process-wide mux, which is a reasonable thing
// for a test binary to do and not a reasonable thing for a daemon to do.
package storetest

import (
	"context"
	"net/url"
	"sync"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
)

// signingScheme is the URL scheme a signing bucket is reached by.
//
// A signed redirect cannot be exercised against the local directory as wsaw
// opens it: fileblob signs only when it is given a signer, and wsaw
// deliberately opens the artifact directory without one, because a URL pointing
// into a directory is a URL nothing serves. A scheme that does have one is what
// lets the redirect path be tested with no S3 account and no network.
const signingScheme = "wsawsignedfile"

// SigningBaseURL is where the signed URLs of a SigningBucketURL bucket claim to
// live. It is exported because the tests assert on the location a redirect
// carries.
const SigningBaseURL = "https://bucket.example/evidence"

// registered guards the mux registration, which panics on a second call for one
// scheme and would otherwise depend on how many fixtures a test file builds.
var registered sync.Once

// SigningBucketURL is a bucket URL rooted at dir whose objects can be signed.
//
// It takes *testing.T rather than being a plain function so that misuse from
// non-test code does not compile, which is the whole of what keeps a signer
// with a hard-coded key out of a deployment.
func SigningBucketURL(t *testing.T, dir string) string {
	t.Helper()

	registered.Do(func() {
		blob.DefaultURLMux().RegisterBucket(signingScheme, signingOpener{})
	})

	return signingScheme + "://localhost" + dir
}

// signingOpener opens a local directory as a bucket that signs.
type signingOpener struct{}

// OpenBucketURL opens the directory the URL names, with an HMAC signer.
func (signingOpener) OpenBucketURL(_ context.Context, u *url.URL) (*blob.Bucket, error) {
	base, err := url.Parse(SigningBaseURL)
	if err != nil {
		return nil, err
	}

	// The same options the store's own openBucket gives a file bucket, plus the
	// signer it deliberately does not: metadata is not written beside an
	// object, and the directory is created if it is not there.
	return fileblob.OpenBucket(u.Path, &fileblob.Options{
		CreateDir: true,
		Metadata:  fileblob.MetadataDontWrite,
		URLSigner: fileblob.NewURLSignerHMAC(base, []byte("a signing key for the tests")),
	})
}
