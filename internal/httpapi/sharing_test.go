package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/share"
)

// Story 5.19. A share link is a credential that travels in a URL, so these
// tests are mostly about what it does *not* open.

const shareKey = "a-share-signing-key-of-32-plus-characters"

func shareSigner(t *testing.T) *share.Signer {
	t.Helper()

	s, err := share.New(share.Options{
		Key:         shareKey,
		Validity:    24 * time.Hour,
		MaxValidity: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// sharedFixture is a server with sharing on, a token configured for the
// authenticated surface, and one stored result.
func sharedFixture(t *testing.T) (*fixture, *share.Signer) {
	t.Helper()

	signer := shareSigner(t)

	f := newFixture(t, httpapi.Options{
		WebUI:        true,
		Token:        secret.Literal("s3cret"),
		Share:        signer,
		ShareBaseURL: "https://wsaw.example.com",
	}, nil)

	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	return f, signer
}

func linkFor(t *testing.T, signer *share.Signer, target, mode, scan string) string {
	t.Helper()

	token, _, err := signer.Mint(target, mode, scan, 0)
	if err != nil {
		t.Fatal(err)
	}

	return "/shared/" + target + "/" + mode + "/" + scan + "?t=" + url.QueryEscape(token)
}

// AC1 and AC9: an operator mints a link, and it says when it expires.
func TestMintingAShareLink(t *testing.T) {
	t.Parallel()

	f, _ := sharedFixture(t)

	req := mustRequest(t, f, http.MethodPost, "/api/v1/share/site/reject/scan-1", "")
	req.Header.Set("Authorization", "Bearer s3cret")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("minting = %d, want 200", resp.StatusCode)
	}

	var out struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expiresAt"`
		Note      string    `json:"note"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &out); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(out.URL, "https://wsaw.example.com/shared/site/reject/scan-1?t=") {
		t.Errorf("url = %q, want an absolute shared link", out.URL)
	}

	if !out.ExpiresAt.After(time.Now()) {
		t.Errorf("expiresAt = %s, want a time in the future", out.ExpiresAt)
	}

	// The trade-off is told to whoever mints the link, not buried in a
	// document they have not read.
	if !strings.Contains(out.Note, "cannot be revoked") {
		t.Errorf("the response does not state that the link cannot be revoked: %q", out.Note)
	}
}

// Minting is an authenticated action: otherwise anyone could turn no access
// into a link.
func TestMintingRequiresAuthentication(t *testing.T) {
	t.Parallel()

	f, _ := sharedFixture(t)

	resp := f.postForm("/api/v1/share/site/reject/scan-1", nil)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated minting = %d, want 401", resp.StatusCode)
	}
}

// The link itself needs no token — that is the point of the story.
func TestASharedResultOpensWithoutTheAPIToken(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	resp := f.get(linkFor(t, signer, "site", "reject", "scan-1"), "Accept", "text/html")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a shared link = %d, want 200", resp.StatusCode)
	}

	html := body(t, resp)

	if !strings.Contains(html, "scan-1") {
		t.Error("the shared page does not show the scan it is for")
	}

	// AC9: the reader is told it is shared and when it stops working.
	if !strings.Contains(html, "read only") || !strings.Contains(html, "expires") {
		t.Error("the page does not say that it is a shared, expiring view")
	}
}

// AC8: the result and nothing that would navigate out of it.
func TestASharedPageOffersNoWayIntoTheRestOfTheInterface(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	html := body(t, f.get(linkFor(t, signer, "site", "reject", "scan-1"), "Accept", "text/html"))

	for _, forbidden := range []string{
		`href="/"`,      // the dashboard
		`href="/audit"`, // the audit log
		`/targets/`,     // the target's history
		"Scan now",      // a write action
		"Approve as baseline",
		"Configuration:", // the config path in the footer
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("the shared page exposes %q", forbidden)
		}
	}

	// And no forms at all: every write path in this interface is a form.
	if strings.Contains(html, "<form") {
		t.Error("the shared page carries a form; a shared reader has nothing to submit")
	}
}

// AC2: a genuine token for one result opens that result and no other.
func TestAShareLinkDoesNotOpenAnotherResult(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)
	f.seed("scan-2", model.ConsentReject, time.Now(), nil)
	f.seed("scan-3", model.ConsentAccept, time.Now(), nil)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/shared/site/reject/scan-2",
		"/shared/site/accept/scan-3",
		"/shared/other/reject/scan-1",
	} {
		resp := f.get(path+"?t="+url.QueryEscape(token), "Accept", "text/html")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with a token for scan-1 = %d, want 403", path, resp.StatusCode)
		}
	}
}

// AC2 again: a share token is not a credential for the authenticated API.
func TestAShareTokenDoesNotOpenTheRestOfTheAPI(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/api/v1/targets",
		"/api/v1/audit",
		"/api/v1/schedule",
		"/api/v1/results/site/reject/scan-1",
		"/",
	} {
		resp := f.get(path+"?t="+url.QueryEscape(token), "Accept", "text/html")

		// Either unauthorised, or redirected to the login form — never served.
		if resp.StatusCode == http.StatusOK {
			t.Errorf("a share token opened %s", path)
		}
	}
}

// AC3: read-only without exception. A shared reader must not be able to reach
// a write path even by knowing its URL.
func TestAShareTokenCannotWrite(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/api/v1/baseline/site/reject",
		"/api/v1/scan/site/reject",
		"/approve/site/reject",
		"/rescan/site/reject",
		"/shared/site/reject/scan-1",
	} {
		resp := f.postForm(path+"?t="+url.QueryEscape(token), url.Values{"scanId": {"scan-1"}})
		if resp.StatusCode == http.StatusOK {
			t.Errorf("POST %s with a share token = 200; a share link is read-only", path)
		}
	}
}

// AC7: expired says expired, invalid says invalid.
func TestAnExpiredLinkSaysSoAndAnAlteredOneDoesNot(t *testing.T) {
	t.Parallel()

	expiring, err := share.New(share.Options{
		Key:         shareKey,
		Validity:    time.Millisecond,
		MaxValidity: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFixture(t, httpapi.Options{WebUI: true, Share: shareSigner(t)}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	token, _, err := expiring.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	resp := f.get("/shared/site/reject/scan-1?t="+url.QueryEscape(token), "Accept", "text/html")

	if resp.StatusCode != http.StatusGone {
		t.Errorf("an expired link = %d, want 410", resp.StatusCode)
	}

	if got := body(t, resp); !strings.Contains(got, "expired") {
		t.Errorf("the page does not say the link expired: %q", got)
	}

	// A forged one is refused differently, and says so.
	resp = f.get("/shared/site/reject/scan-1?t=not-a-token", "Accept", "text/html")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("an invalid link = %d, want 403", resp.StatusCode)
	}

	if got := body(t, resp); !strings.Contains(got, "not valid") {
		t.Errorf("the page does not say the link is invalid: %q", got)
	}
}

// A link with no token at all is refused rather than treated as anonymous
// access.
func TestASharedPathWithoutATokenIsRefused(t *testing.T) {
	t.Parallel()

	f, _ := sharedFixture(t)

	if resp := f.get("/shared/site/reject/scan-1", "Accept", "text/html"); resp.StatusCode == http.StatusOK {
		t.Error("a shared path with no token was served")
	}
}

// AC12: off until configured. With sharing disabled the prefix must not exist.
func TestSharingIsOffUntilConfigured(t *testing.T) {
	t.Parallel()

	signer := shareSigner(t)

	// A server with no signer at all: the tokens exist, the routes do not.
	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	resp := f.get("/shared/site/reject/scan-1?t="+url.QueryEscape(token), "Accept", "text/html")

	if resp.StatusCode == http.StatusOK {
		t.Error("a shared link worked on a wsaw where sharing is not enabled")
	}

	if resp := f.postForm("/api/v1/share/site/reject/scan-1", nil); resp.StatusCode == http.StatusOK {
		t.Error("a link was minted on a wsaw where sharing is not enabled")
	}
}

// AC10: the artifacts of its own result, and no others. This is the check
// that stops a link to one harmless scan being a key to every stored file.
func TestAShareLinkReachesOnlyItsOwnArtifacts(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	// An artifact belonging to the shared scan...
	mine, err := f.store.PutArtifact("screenshot-before-consent", onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}

	// ...and one belonging to a different scan entirely.
	theirs, err := f.store.PutArtifact("screenshot-before-consent", []byte("another scan's evidence"))
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-shared", model.ConsentReject, time.Now(), func(res *model.Result) {
		res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: mine}}
	})

	token, _, err := signer.Mint("site", "reject", "scan-shared", 0)
	if err != nil {
		t.Fatal(err)
	}

	base := "/shared/site/reject/scan-shared/artifacts/"

	if resp := f.get(base + mine + "?t=" + url.QueryEscape(token)); resp.StatusCode != http.StatusOK {
		t.Errorf("the shared result's own screenshot = %d, want 200", resp.StatusCode)
	}

	resp := f.get(base + theirs + "?t=" + url.QueryEscape(token))

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another scan's artifact = %d, want 403: a link to one scan is not a key to every file",
			resp.StatusCode)
	}
}

// The downloads a reader needs come with the link, since evidence they cannot
// take away is evidence they cannot check.
func TestASharedResultCanBeDownloaded(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"json", "har", "csv"} {
		resp := f.get("/shared/site/reject/scan-1/" + suffix + "?t=" + url.QueryEscape(token))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("the shared %s download = %d, want 200", suffix, resp.StatusCode)
		}
	}
}

// AC11: the token must not reach wsaw's own logs. The request logger records
// the path, and the path is where the token is not.
func TestTheTokenIsNotInTheRequestLog(t *testing.T) {
	t.Parallel()

	f, signer := sharedFixture(t)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	f.get("/shared/site/reject/scan-1?t=" + url.QueryEscape(token))

	if strings.Contains(f.logs(), token) {
		t.Error("the share token was written to the log; a credential in a log is a credential")
	}
}

// AC1: the button is on the result page, and only when sharing is on — the
// interface must not offer an action that would fail.
func TestTheResultPageOffersSharingOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	f, _ := sharedFixture(t)

	req := mustRequest(t, f, http.MethodGet, "/results/site/reject/scan-1", "")
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Accept", "text/html")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if html := body(t, resp); !strings.Contains(html, `action="/share/site/reject/scan-1"`) {
		t.Error("the result page does not offer a share button")
	}

	// With sharing off there is no button.
	off := newFixture(t, httpapi.Options{WebUI: true}, nil)
	off.seed("scan-1", model.ConsentReject, time.Now(), nil)

	if html := body(t, off.get("/results/site/reject/scan-1", "Accept", "text/html")); strings.Contains(html, "/share/") {
		t.Error("the result page offers sharing on a wsaw where it is not enabled")
	}
}

// The button mints a link and shows it, rather than redirecting with the
// token in the URL — that would put a credential in the browser's history.
func TestTheShareButtonShowsTheLinkWithoutRedirecting(t *testing.T) {
	t.Parallel()

	f, _ := sharedFixture(t)

	req := mustRequest(t, f, http.MethodPost, "/share/site/reject/scan-1", "csrf=s3cret")
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the share button = %d, want 200 with the link on the page", resp.StatusCode)
	}

	if got := resp.Header.Get("Location"); got != "" {
		t.Errorf("the share button redirected to %q, putting the token in browser history", got)
	}

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a page holding a credential", got)
	}

	html := body(t, resp)

	if !strings.Contains(html, "/shared/site/reject/scan-1?t=") {
		t.Error("the page does not show the minted link")
	}

	if !strings.Contains(html, "cannot be withdrawn") {
		t.Error("the page does not say the link cannot be revoked before it expires")
	}
}
