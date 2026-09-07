package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Story 5.17: the screenshots a scan captured, on the scan's own page. The
// pair is the point — the banner as the site presented it, and what wsaw's
// interaction did to it.

// onePixelPNG is a valid 1x1 PNG, so the magic-byte check and the browser
// both see a real image.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
	0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// seedWithScreenshots stores a result with a before and an after image, in
// the wrong order on purpose: the page has to present them the right way
// round regardless of how they were recorded.
func seedWithScreenshots(t *testing.T, f *fixture) (before, after string) {
	t.Helper()

	beforeRef, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	// A different byte, so the two artifacts are distinct files.
	afterBytes := append([]byte(nil), onePixelPNG...)
	afterBytes[len(afterBytes)-5] ^= 0xff

	afterRef, err := f.store.PutArtifact("screenshot-after-consent", afterBytes)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-1", model.ConsentReject, time.Now(), func(res *model.Result) {
		res.Screenshots = []model.Artifact{
			{Kind: "screenshot-after-consent", Ref: afterRef, SHA256: "afterdigest", Bytes: int64(len(afterBytes))},
			{Kind: "screenshot-before-consent", Ref: beforeRef, SHA256: "beforedigest", Bytes: int64(len(onePixelPNG))},
		}
	})

	return beforeRef, afterRef
}

// AC1: both images on the page, as a pair, in the order that makes them mean
// what they mean.
func TestTheResultPageShowsBothScreenshots(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	before, after := seedWithScreenshots(t, f)

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	for _, ref := range []string{before, after} {
		if !strings.Contains(html, "/api/v1/artifacts/"+ref) {
			t.Errorf("the page does not show the screenshot %s", ref)
		}
	}

	beforeAt := strings.Index(html, "/api/v1/artifacts/"+before)
	afterAt := strings.Index(html, "/api/v1/artifacts/"+after)

	if beforeAt > afterAt {
		t.Error("after-consent is shown before before-consent; read that way round the pair says the opposite")
	}

	if !strings.Contains(html, "Before the consent interaction") ||
		!strings.Contains(html, "After the consent interaction") {
		t.Error("the images are not labelled with the moment they show")
	}
}

// AC2: a scan with none says so, and says how to get them.
func TestAScanWithoutScreenshotsSaysSo(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "No screenshots were captured") {
		t.Error("the page does not say that no screenshots were captured")
	}

	if !strings.Contains(html, "screenshots: true") {
		t.Error("the page does not say how to enable them")
	}

	if strings.Contains(html, "<img src=\"/api/v1/artifacts/") {
		t.Error("an image was rendered for a scan that captured none")
	}
}

// AC3: evidence that has been pruned must not look like evidence that never
// existed, and must not render as a broken image.
func TestAPrunedScreenshotIsReportedAsMissing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	f.seed("scan-1", model.ConsentReject, time.Now(), func(res *model.Result) {
		res.Screenshots = []model.Artifact{{
			Kind:   "screenshot-before-consent",
			Ref:    "screenshot-before-consent/gone",
			SHA256: "digest",
			Bytes:  1234,
		}}
	})

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "no longer stored") {
		t.Error("a screenshot whose file is gone is not reported as missing")
	}

	if strings.Contains(html, `img src="/api/v1/artifacts/screenshot-before-consent/gone"`) {
		t.Error("a missing screenshot was rendered as an image, which shows a broken frame")
	}
}

// AC4: the digest and the size are on the page, and the original is
// downloadable — a screenshot is worth what a reader can verify.
func TestAScreenshotCarriesItsDigestAndADownload(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	before, _ := seedWithScreenshots(t, f)

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "beforedigest") {
		t.Error("the recorded digest is not shown, so the image cannot be checked against it")
	}

	if !strings.Contains(html, "/api/v1/artifacts/"+before+"?download=1") {
		t.Error("the original file cannot be downloaded")
	}
}

// AC5: a screenshot is served as an image — and only as an image.
func TestAScreenshotIsServedAsAnImage(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	before, _ := seedWithScreenshots(t, f)

	resp := f.get("/api/v1/artifacts/" + before)

	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png — an <img> cannot render an octet-stream", got)
	}

	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}

	csp := resp.Header.Get("Content-Security-Policy")

	if !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q, want an image-only policy", csp)
	}

	if strings.Contains(csp, "script-src") && !strings.Contains(csp, "'none'") {
		t.Errorf("the policy allows script: %q", csp)
	}
}

// The same file, asked for as a download, comes back as one — so a
// compliance pack gets the original bytes rather than a rendering.
func TestAScreenshotCanStillBeDownloaded(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	before, _ := seedWithScreenshots(t, f)

	resp := f.get("/api/v1/artifacts/" + before + "?download=1")

	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}

	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
}

// The narrowing of Story 5.11's AC5 must go no further than screenshots: a
// response body is the page's own bytes and stays inert, even if something
// hands the endpoint a reference that claims otherwise.
func TestOnlyRealScreenshotsAreServedAsImages(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	// HTML stored under a screenshot kind: the name says image, the bytes do
	// not, and the bytes decide.
	ref, err := f.store.PutArtifact("screenshot-before-consent", []byte("<script>alert(1)</script>"))
	if err != nil {
		t.Fatal(err)
	}

	resp := f.get("/api/v1/artifacts/" + ref)

	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q: non-PNG bytes were served as an image because of their name", got)
	}

	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}

	// And a body, whose kind says nothing about images, is never inline —
	// even when its bytes happen to be a PNG.
	bodyRef, err := f.store.PutArtifact("body", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	resp = f.get("/api/v1/artifacts/" + bodyRef)

	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("a stored body was served as %q; a body is the page's bytes and stays a download", got)
	}
}

// AC8: nothing here makes evidence easier to reach than the results it
// belongs to.
func TestScreenshotsNeedTheSameAuthenticationAsEverythingElse(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, Token: secret.Literal("s3cret")}, nil)
	before, _ := seedWithScreenshots(t, f)

	resp := f.get("/api/v1/artifacts/" + before)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request for a screenshot = %d, want 401", resp.StatusCode)
	}
}

// A pair whose two frames are the same image asserts an interaction that
// changed nothing. For a scan that never interacted at all — consent mode
// "none", or a page with no banner — that is a lie twice over, and it is what
// every such scan stored before capture stopped shooting the second frame.
func TestAnUnchangedPairIsShownAsOneFrame(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	beforeRef, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	afterRef, err := f.store.PutArtifact("screenshot-after-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-1", model.ConsentNone, time.Now(), func(res *model.Result) {
		res.Consent = model.Consent{
			Outcome: model.OutcomeNotNeeded,
			Reason:  "consent mode is none; the page was not interacted with",
		}
		res.Screenshots = []model.Artifact{
			{Kind: "screenshot-before-consent", Ref: beforeRef, SHA256: "samedigest", Bytes: 1},
			{Kind: "screenshot-after-consent", Ref: afterRef, SHA256: "samedigest", Bytes: 1},
		}
	})

	html := body(t, f.get("/results/site/none/scan-1", "Accept", "text/html"))

	if strings.Contains(html, "/api/v1/artifacts/"+afterRef) {
		t.Error("the duplicate frame is still shown as an image of its own")
	}

	if strings.Contains(html, "After the consent interaction") {
		t.Error("the page still claims a consent interaction happened")
	}

	if !strings.Contains(html, "Page as loaded") {
		t.Error("the remaining frame is not labelled as the page as it loaded")
	}

	if !strings.Contains(html, "never interacted with") {
		t.Error("the page does not say why there is only one frame")
	}

	if !strings.Contains(html, "/api/v1/artifacts/"+beforeRef) {
		t.Error("the surviving frame is not shown")
	}
}

// A scan that captured only the one frame — what capture stores now when
// nothing interacts with the page — must not be captioned as the "before" of
// an interaction that never came.
func TestASoleFrameFromAnUninteractedScanSaysSo(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	ref, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-1", model.ConsentNone, time.Now(), func(res *model.Result) {
		res.Consent = model.Consent{
			Outcome: model.OutcomeNotNeeded,
			Reason:  "consent mode is none; the page was not interacted with",
		}
		res.Screenshots = []model.Artifact{
			{Kind: "screenshot-before-consent", Ref: ref, SHA256: "digest", Bytes: 1},
		}
	})

	html := body(t, f.get("/results/site/none/scan-1", "Accept", "text/html"))

	if strings.Contains(html, "Before the consent interaction") {
		t.Error("the frame is captioned as the before of an interaction that never happened")
	}

	if !strings.Contains(html, "Page as loaded") {
		t.Error("the frame is not labelled as the page as it loaded")
	}

	if !strings.Contains(html, "no consent interaction was performed") {
		t.Error("the page does not say why there is no second frame")
	}
}

// A real interaction that happens to leave the page pixel-identical is a
// different statement, and must read as one.
func TestAnUnchangedPairAfterARealInteractionSaysThat(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	beforeRef, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	afterRef, err := f.store.PutArtifact("screenshot-after-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-1", model.ConsentReject, time.Now(), func(res *model.Result) {
		res.Consent = model.Consent{Outcome: model.OutcomeApplied, CMP: "Consentmanager"}
		res.Screenshots = []model.Artifact{
			{Kind: "screenshot-before-consent", Ref: beforeRef, SHA256: "samedigest", Bytes: 1},
			{Kind: "screenshot-after-consent", Ref: afterRef, SHA256: "samedigest", Bytes: 1},
		}
	})

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "left the page looking") {
		t.Error("the page does not say that the interaction changed nothing visible")
	}

	if strings.Contains(html, "never interacted with") {
		t.Error("the page denies an interaction that did happen")
	}
}

// An interaction that was attempted and failed is a third statement again:
// the page is unchanged because the attempt did not work, which is a finding.
func TestAnUnchangedPairAfterAFailedInteractionSaysThat(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	beforeRef, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	afterRef, err := f.store.PutArtifact("screenshot-after-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-1", model.ConsentAccept, time.Now(), func(res *model.Result) {
		res.Consent = model.Consent{
			Outcome: model.OutcomeFailed,
			Reason:  "no accept control could be found",
		}
		res.Screenshots = []model.Artifact{
			{Kind: "screenshot-before-consent", Ref: beforeRef, SHA256: "samedigest", Bytes: 1},
			{Kind: "screenshot-after-consent", Ref: afterRef, SHA256: "samedigest", Bytes: 1},
		}
	})

	html := body(t, f.get("/results/site/accept/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "the consent interaction failed and left the page unchanged") {
		t.Error("the page does not say that the unchanged frame is the result of a failed interaction")
	}

	if strings.Contains(html, "never interacted with") {
		t.Error("the page denies an interaction that was attempted")
	}
}
