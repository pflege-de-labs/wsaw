package capture

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

// TestRecorderStoresBodyIdentityAlongsideTheDigest keeps both: the digest
// records what was fetched, the identity records what the script says it is,
// and a reader has to be able to see either.
func TestRecorderStoresBodyIdentityAlongsideTheDigest(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://www.googletagmanager.com/gtm.js?id=GTM-X",
		"GET", network.ResourceTypeScript))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", Timestamp: mono(0)})
	r.setBodyDigest("1", "deadbeef", 7, "ref-1", "")
	r.setBodyIdentity("1", "GTM container version", "231")

	got := r.requests()[0]

	if got.BodyIdentity != "231" || got.BodyIdentityLabel != "GTM container version" {
		t.Errorf("identity = %q (%q), want 231 (GTM container version)",
			got.BodyIdentity, got.BodyIdentityLabel)
	}

	if got.BodySHA256 != "deadbeef" {
		t.Errorf("the digest was lost when an identity was recorded: %q", got.BodySHA256)
	}
}

// TestRecorderIgnoresAnEmptyBodyIdentity so a rule that matched the URL but
// found nothing in the body leaves no trace. A half-recorded identity would
// read as "one side could not be compared" forever after.
func TestRecorderIgnoresAnEmptyBodyIdentity(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://example.com/a.js", "GET", network.ResourceTypeScript))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", Timestamp: mono(0)})
	r.setBodyIdentity("1", "GTM container version", "")
	r.setBodyIdentity("1", "", "231")

	if got := r.requests()[0]; got.BodyIdentity != "" || got.BodyIdentityLabel != "" {
		t.Errorf("a partial identity was recorded: %q = %q", got.BodyIdentityLabel, got.BodyIdentity)
	}
}

func TestRecorderRequestURLIsReadableForIdentityMatching(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	const url = "https://www.googletagmanager.com/gtm.js?id=GTM-X&cb=1"

	r.requestWillBeSent(willBeSent("1", url, "GET", network.ResourceTypeScript))

	// The raw URL, not the normalized key: a rule may need to match on a
	// parameter that normalization drops.
	if got := r.requestURL("1"); got != url {
		t.Errorf("requestURL() = %q, want the raw %q", got, url)
	}

	if got := r.requestURL("nope"); got != "" {
		t.Errorf("requestURL() = %q for an unknown request, want empty", got)
	}
}
