package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Story 8.7: serving evidence that lives in a bucket. The routes are the same
// ones Story 5.17 and Story 5.19 built; what these tests are about is what the
// bucket added — a stream instead of a buffer, a cacheable response, a
// redirect where the operator asked for one, and pruned evidence that still
// reads as pruned.

// signingScheme is a bucket that signs URLs, which is otherwise something only
// a cloud provider does.
//
// A signed redirect cannot be exercised against the local directory: fileblob
// signs only when it is given a signer, and wsaw deliberately opens the
// artifact directory without one, because a URL pointing into a directory is a
// URL nothing serves. Registering a scheme that does have one is what lets the
// redirect path be tested without an S3 account.
const signingScheme = "signedfile"

// signingBaseURL is where the signed URLs of that bucket claim to live.
const signingBaseURL = "https://bucket.example/evidence"

func init() {
	blob.DefaultURLMux().RegisterBucket(signingScheme, signingOpener{})
}

type signingOpener struct{}

func (signingOpener) OpenBucketURL(_ context.Context, u *url.URL) (*blob.Bucket, error) {
	base, err := url.Parse(signingBaseURL)
	if err != nil {
		return nil, err
	}

	return fileblob.OpenBucket(u.Path, &fileblob.Options{
		CreateDir: true,
		Metadata:  fileblob.MetadataDontWrite,
		URLSigner: fileblob.NewURLSignerHMAC(base, []byte("a signing key for the tests")),
	})
}

// signingFixture is a server whose evidence lives in a bucket that can sign.
func signingFixture(t *testing.T, opts httpapi.Options) *fixture {
	t.Helper()

	dir := t.TempDir()

	f := newFixtureIn(t, opts, nil, nil, signingScheme+"://localhost"+dir)
	// The bucket is rooted at the directory its URL names, so a test can still
	// remove an object behind the store's back.
	f.artifactDir = dir

	return f
}

// signedExpiry reads when the signed URL in a redirect stops working.
func signedExpiry(t *testing.T, location string) time.Time {
	t.Helper()

	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("the redirect location %q is not a URL: %v", location, err)
	}

	raw := u.Query().Get("expiry")
	if raw == "" {
		t.Fatalf("the redirect location %q carries no expiry", location)
	}

	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("the redirect's expiry %q is not a timestamp: %v", raw, err)
	}

	return time.Unix(seconds, 0)
}

// AC1: the object is streamed, not read into memory and then written out.
//
// Asserted by measuring rather than by reading the code: a body that is
// buffered whole allocates at least its own size, and TotalAlloc counts every
// allocation whether or not the collector has been round since. The margin is
// four times over, so this fails on a regression and not on noise. It does not
// run in parallel, because the figure it reads is process-wide.
func TestALargeArtifactIsStreamedRatherThanBuffered(t *testing.T) {
	f := newFixture(t, httpapi.Options{}, nil)

	// Large enough that buffering it would be unmistakable, small enough to
	// keep the test quick.
	data := bytes.Repeat([]byte("a result document, in miniature. "), 800_000)
	want := sha256.Sum256(data)

	ref, err := f.store.PutArtifact("body", data)
	if err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	resp := f.get("/api/v1/artifacts/" + ref)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("serving a large artifact = %d, want 200", resp.StatusCode)
	}

	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(data)) {
		t.Errorf("Content-Length = %q, want %d", got, len(data))
	}

	// Hashed as it arrives, so the client does not buffer the whole body
	// either and the measurement stays about the server.
	sum := sha256.New()

	copied, err := io.Copy(sum, resp.Body)
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}

	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the body: %v", err)
	}

	runtime.ReadMemStats(&after)

	if copied != int64(len(data)) {
		t.Errorf("served %d bytes, want %d", copied, len(data))
	}

	if !bytes.Equal(sum.Sum(nil), want[:]) {
		t.Error("the bytes served are not the bytes stored")
	}

	if grew := after.TotalAlloc - before.TotalAlloc; grew > uint64(len(data)/4) {
		t.Errorf("serving a %d-byte artifact allocated %d bytes; it is being buffered rather than streamed",
			len(data), grew)
	}
}

// AC4: a browser that already holds the image asks, and is told to keep it.
func TestAConditionalRequestIsAnsweredWithNotModified(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	before, _ := seedWithScreenshots(t, f)

	first := f.get("/api/v1/artifacts/" + before)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first fetch = %d, want 200", first.StatusCode)
	}

	etag := first.Header.Get("ETag")
	modified := first.Header.Get("Last-Modified")

	if etag == "" || modified == "" {
		t.Fatalf("the response carries no validators to cache by: ETag %q, Last-Modified %q", etag, modified)
	}

	if got := first.Header.Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("Cache-Control = %q, want evidence kept out of shared caches", got)
	}

	_ = body(t, first)

	for name, tc := range map[string]struct {
		header, value string
		want          int
	}{
		"the tag it was given": {
			header: "If-None-Match", value: etag, want: http.StatusNotModified,
		},
		"the tag, weakened by a proxy": {
			header: "If-None-Match", value: "W/" + etag, want: http.StatusNotModified,
		},
		"any tag at all": {
			header: "If-None-Match", value: "*", want: http.StatusNotModified,
		},
		"one of several tags": {
			header: "If-None-Match", value: `"sha256:whatever", ` + etag, want: http.StatusNotModified,
		},
		"a tag from another artifact": {
			header: "If-None-Match", value: `"sha256:0000"`, want: http.StatusOK,
		},
		"the date it was given": {
			header: "If-Modified-Since", value: modified, want: http.StatusNotModified,
		},
		"a date before it was written": {
			header: "If-Modified-Since",
			value:  time.Now().Add(-24 * time.Hour).UTC().Format(http.TimeFormat),
			want:   http.StatusOK,
		},
		"a date that is not a date": {
			header: "If-Modified-Since", value: "the day before yesterday", want: http.StatusOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resp := f.get("/api/v1/artifacts/"+before, tc.header, tc.value)

			if resp.StatusCode != tc.want {
				t.Errorf("%s: %s = %d, want %d", name, tc.header, resp.StatusCode, tc.want)
			}

			got := body(t, resp)

			if tc.want == http.StatusNotModified {
				if got != "" {
					t.Errorf("a 304 carried %d bytes of body", len(got))
				}

				if resp.Header.Get("ETag") != etag {
					t.Error("a 304 does not repeat the entity tag, so the client cannot revalidate again")
				}
			}
		})
	}
}

// AC4: the validator is the content address, which is what makes it strong.
func TestTheETagIsTheStoredDigest(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	ref, err := f.store.PutArtifact("body", []byte("stored evidence"))
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte("stored evidence"))
	want := `"sha256:` + hex.EncodeToString(sum[:]) + `"`

	resp := f.get("/api/v1/artifacts/" + ref)
	_ = body(t, resp)

	if got := resp.Header.Get("ETag"); got != want {
		t.Errorf("ETag = %q, want %q: the key is the digest, so the tag is exact", got, want)
	}
}

// AC5: evidence the bucket no longer holds is reported as gone, on the page
// and on the route, rather than as a fault or a stale cache hit.
func TestEvidenceRemovedFromTheBucketIsReportedAsNoLongerStored(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	before, _ := seedWithScreenshots(t, f)

	// Removed behind the store's back, which is what a lifecycle rule or a
	// finished prune leaves.
	if err := os.Remove(filepath.Join(f.artifactDir, filepath.FromSlash(before))); err != nil {
		t.Fatal(err)
	}

	// AC6's other half: the failed fetch is confined to itself. The page that
	// links to the evidence still renders, with the rest of the scan on it.
	page := f.get("/results/site/reject/scan-1", "Accept", "text/html")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("the result page = %d, want 200: one missing object must not take the page with it", page.StatusCode)
	}

	html := body(t, page)

	if !strings.Contains(html, "no longer stored") {
		t.Error("a screenshot the bucket has lost is not reported as missing on the result page")
	}

	if !strings.Contains(html, "tracker.test") {
		t.Error("the rest of the scan is not on the page any more")
	}

	resp := f.get("/api/v1/artifacts/" + before)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("fetching pruned evidence = %d, want 404", resp.StatusCode)
	}

	_ = body(t, resp)

	// A client holding a cached copy is told the evidence is gone, not that
	// its copy is still current: absence is answered, never inferred (Tenet 5).
	conditional := f.get("/api/v1/artifacts/"+before, "If-None-Match", "*")
	if conditional.StatusCode != http.StatusNotFound {
		t.Errorf("a conditional request for pruned evidence = %d, want 404", conditional.StatusCode)
	}

	_ = body(t, conditional)
}

// AC3: off by default. A bucket that can sign is not reason enough to hand the
// reader a URL instead of the bytes.
func TestSignedRedirectsAreOffByDefault(t *testing.T) {
	t.Parallel()

	f := signingFixture(t, httpapi.Options{})
	before, _ := seedWithScreenshots(t, f)

	resp := f.get("/api/v1/artifacts/" + before)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch = %d, want 200: nothing was configured to redirect", resp.StatusCode)
	}

	if got := body(t, resp); got != string(onePixelPNG) {
		t.Error("the artifact was not served by wsaw itself")
	}
}

// AC3: with the opt-in, the reader is sent to the bucket — and for no longer
// than the lifetime that was configured.
//
// Two lifetimes, because one would not pin anything: a redirect signed for a
// hardcoded five minutes, or for a default plus a fixed slack, would satisfy a
// single case while ignoring the setting. The tolerance is a second, which is
// the resolution the expiry is expressed in.
func TestASignedRedirectIsIssuedWhenAskedFor(t *testing.T) {
	t.Parallel()

	for name, ttl := range map[string]time.Duration{
		"five minutes":  5 * time.Minute,
		"half a minute": 30 * time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := signingFixture(t, httpapi.Options{
				SignedArtifactURLs:   true,
				SignedArtifactURLTTL: ttl,
			})
			before, _ := seedWithScreenshots(t, f)

			resp := f.get("/api/v1/artifacts/" + before)
			_ = body(t, resp)

			if resp.StatusCode != http.StatusFound {
				t.Fatalf("fetch = %d, want 302", resp.StatusCode)
			}

			location := resp.Header.Get("Location")

			if !strings.HasPrefix(location, signingBaseURL) {
				t.Errorf("Location = %q, want a URL at the bucket", location)
			}

			if !strings.Contains(location, "signature=") {
				t.Errorf("Location = %q, want a signed URL", location)
			}

			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store: the redirect stops working when it expires", got)
			}

			if lifetime := time.Until(signedExpiry(t, location)); lifetime > ttl+time.Second {
				t.Errorf("the signed URL lives for %s, longer than the configured %s", lifetime, ttl)
			}
		})
	}
}

// AC5 again, on the redirect path: pruned evidence must not be answered with a
// redirect to a bucket that would report it in its own words.
func TestNoRedirectIsIssuedForEvidenceThatIsGone(t *testing.T) {
	t.Parallel()

	f := signingFixture(t, httpapi.Options{
		SignedArtifactURLs:   true,
		SignedArtifactURLTTL: 5 * time.Minute,
	})
	before, _ := seedWithScreenshots(t, f)

	if err := os.Remove(filepath.Join(f.artifactDir, filepath.FromSlash(before))); err != nil {
		t.Fatal(err)
	}

	resp := f.get("/api/v1/artifacts/" + before)
	_ = body(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a redirect for pruned evidence = %d, want 404", resp.StatusCode)
	}
}

// A crafted reference must not become a redirect either. Signing is arithmetic
// over the key, so a provider would sign anything it was handed; what stops it
// is the store refusing every reference it did not write (Tenet 9).
func TestACraftedReferenceIsNotSigned(t *testing.T) {
	t.Parallel()

	f := signingFixture(t, httpapi.Options{
		SignedArtifactURLs:   true,
		SignedArtifactURLTTL: 5 * time.Minute,
	})
	seedWithScreenshots(t, f)

	for _, ref := range []string{
		"../wsaw.db",
		"body/../../wsaw.db",
		"..%2f..%2fwsaw.db",
		"screenshot-before-consent/not-a-digest",
	} {
		resp := f.get("/api/v1/artifacts/" + ref)
		_ = body(t, resp)

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusFound {
			t.Errorf("reference %q was answered with %d", ref, resp.StatusCode)
		}
	}
}

// AC3: the redirect never outlives the share link that produced it.
func TestASignedRedirectDoesNotOutliveItsShareLink(t *testing.T) {
	t.Parallel()

	signer := shareSigner(t)

	f := signingFixture(t, httpapi.Options{
		WebUI:        true,
		Token:        secret.Literal("s3cret"),
		Share:        signer,
		ShareBaseURL: "https://wsaw.example.com",
		// Ten minutes of configured lifetime against a link with one minute
		// left: the link has to win.
		SignedArtifactURLs:   true,
		SignedArtifactURLTTL: 10 * time.Minute,
	})

	ref, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-shared", model.ConsentReject, time.Now(), func(res *model.Result) {
		res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: ref}}
	})

	token, claims, err := signer.Mint("site", "reject", "scan-shared", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	resp := f.get("/shared/site/reject/scan-shared/artifacts/" + ref + "?t=" + url.QueryEscape(token))
	_ = body(t, resp)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a shared artifact = %d, want 302", resp.StatusCode)
	}

	expiry := signedExpiry(t, resp.Header.Get("Location"))

	// One second of slack for the rounding a Unix timestamp does.
	if expiry.After(claims.Expiry().Add(time.Second)) {
		t.Errorf("the signed URL expires at %s, after the share link's %s",
			expiry.Format(time.RFC3339), claims.Expiry().Format(time.RFC3339))
	}
}

// AC3: a provider that cannot sign is served through wsaw, and said once.
func TestABucketThatCannotSignFallsBackToProxying(t *testing.T) {
	t.Parallel()

	// The plain artifact directory, which is exactly the provider that cannot
	// sign: a URL into a directory is a URL nothing serves.
	f := newFixture(t, httpapi.Options{
		SignedArtifactURLs:   true,
		SignedArtifactURLTTL: 5 * time.Minute,
	}, nil)
	before, _ := seedWithScreenshots(t, f)

	for i := range 3 {
		resp := f.get("/api/v1/artifacts/" + before)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("fetch %d = %d, want 200: a bucket that cannot sign must not fail the request",
				i+1, resp.StatusCode)
		}

		if got := body(t, resp); got != string(onePixelPNG) {
			t.Errorf("fetch %d did not return the image", i+1)
		}
	}

	// Counted on the sentence only the log line carries; the wrapped error it
	// quotes says the same thing in its own words.
	if got := strings.Count(f.logs(), "artifacts are served through wsaw instead"); got != 1 {
		t.Errorf("the bucket's inability to sign was logged %d times, want exactly 1", got)
	}
}

// AC2: content addressing plus one bucket for every scan makes the scope check
// the only thing between a link to a harmless scan and all the evidence there
// is. So it is asserted against the bucket-backed path, with the other scan's
// artifact demonstrably readable by an operator at the same moment.
func TestAShareLinkCannotFetchAnotherResultsEvidence(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	mine, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	// A second scan's screenshot, in the same bucket, under a key that is
	// nothing but its digest.
	othersBytes := append([]byte(nil), onePixelPNG...)
	othersBytes[len(othersBytes)-6] ^= 0xff

	theirs, err := f.store.PutArtifact("screenshot-before-consent", othersBytes)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-shared", model.ConsentReject, time.Now(), func(res *model.Result) {
		res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: mine}}
	})

	f.seed("scan-private", model.ConsentAccept, time.Now(), func(res *model.Result) {
		res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: theirs}}
	})

	token, _, err := signer.Mint("site", "reject", "scan-shared", 0)
	if err != nil {
		t.Fatal(err)
	}

	base := "/shared/site/reject/scan-shared/artifacts/"

	if resp := f.get(base + mine + "?t=" + url.QueryEscape(token)); resp.StatusCode != http.StatusOK {
		t.Errorf("the shared result's own screenshot = %d, want 200", resp.StatusCode)
	}

	// In the bucket and readable — by an operator, through the authenticated
	// route. Without this the 403 below could be absence rather than scope.
	authed := f.get("/api/v1/artifacts/"+theirs, "Authorization", "Bearer s3cret")
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("the other scan's screenshot = %d for an operator, want 200", authed.StatusCode)
	}

	_ = body(t, authed)

	resp := f.get(base + theirs + "?t=" + url.QueryEscape(token))

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another scan's evidence = %d through a share link, want 403", resp.StatusCode)
	}

	// Not even with a cached copy to validate: the scope check runs before
	// anything asks the bucket a question.
	conditional := f.get(base+theirs+"?t="+url.QueryEscape(token), "If-None-Match", "*")
	if conditional.StatusCode != http.StatusForbidden {
		t.Errorf("a conditional request for another scan's evidence = %d, want 403", conditional.StatusCode)
	}
}
