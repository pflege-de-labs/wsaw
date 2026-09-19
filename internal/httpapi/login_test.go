package httpapi_test

// The one-time browser sign-in (Story 5.21), driven the way "wsaw ui" drives
// it: mint with the standing bearer token, then redeem the link a browser
// would open next.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

func (f *fixture) postNoBody(path string, headers ...string) *http.Response {
	f.t.Helper()

	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost, f.server.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}

	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}

	return resp
}

func mintLoginToken(t *testing.T, f *fixture, bearer string) (string, int) {
	t.Helper()

	resp := f.postNoBody("/api/v1/ui/login-token", "Authorization", "Bearer "+bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint status = %d, want 200: %s", resp.StatusCode, body(t, resp))
	}

	var out struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expiresIn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding mint response: %v", err)
	}

	if out.Token == "" {
		t.Fatal("mint returned an empty token")
	}

	return out.Token, out.ExpiresIn
}

func TestMintRequiresTheStandingToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	unauthed := f.postNoBody("/api/v1/ui/login-token")
	if unauthed.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without the standing token", unauthed.StatusCode)
	}

	wrong := f.postNoBody("/api/v1/ui/login-token", "Authorization", "Bearer wrong")
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 with the wrong token", wrong.StatusCode)
	}
}

func TestMintWithoutATokenConfiguredIsRefused(t *testing.T) {
	t.Parallel()

	// No token configured at all: nothing to authenticate with, and the
	// route is reachable unauthenticated (withAuth passes everything
	// through), but minting a session for a deployment with no session
	// concept is refused rather than silently issuing a token nobody needs.
	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	resp := f.postNoBody("/api/v1/ui/login-token")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRedeemedLinkSignsInAndCannotBeReplayed(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	token, expiresIn := mintLoginToken(t, f, "s3cret")
	if expiresIn <= 0 {
		t.Errorf("expiresIn = %d, want positive", expiresIn)
	}

	first := f.get("/login/otp/" + token)
	if first.StatusCode != http.StatusSeeOther {
		t.Fatalf("first redemption status = %d, want a redirect", first.StatusCode)
	}

	if got := first.Header.Get("Location"); got != "/" {
		t.Errorf("Location = %q, want the dashboard", got)
	}

	cookies := first.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}

	if cookies[0].Value != "s3cret" {
		t.Error("session cookie does not carry the standing token")
	}

	// The link is spent: the same URL a second time must not sign in again.
	second := f.get("/login/otp/" + token)
	if len(second.Cookies()) != 0 {
		t.Error("a second redemption of the same link issued another cookie")
	}

	if got := second.Header.Get("Location"); got == "/" {
		t.Error("a replayed link should not land back on the dashboard as if it worked")
	}
}

func TestUnknownOrExpiredLinkFailsExplicitly(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.get("/login/otp/not-a-real-token")
	if len(resp.Cookies()) != 0 {
		t.Error("an invalid token must never set a session cookie")
	}

	if resp.Header.Get("Location") == "/" {
		t.Error("an invalid token should not redirect to the dashboard as if it had worked")
	}
}

func TestMintedTokenNeverAuthenticatesTheAPIDirectly(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	token, _ := mintLoginToken(t, f, "s3cret")

	resp := f.get("/api/v1/targets", "Authorization", "Bearer "+token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: a one-time login token must not work as a Bearer token", resp.StatusCode)
	}
}

func TestRedeemWithNoTokenConfiguredJustGoesHome(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	resp := f.get("/login/otp/anything")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("status = %d, location = %q; want a redirect to / when no token is configured",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestMintRouteAbsentWithoutWebUI(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret")}, nil)

	resp := f.postNoBody("/api/v1/ui/login-token", "Authorization", "Bearer s3cret")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the web interface is off", resp.StatusCode)
	}
}

// The login form is the one endpoint reachable without any prior
// credential, so wrong-token submissions are bounded per source address —
// the same reasoning that rate-limits the one-time link (Story 5.21, AC9).
// The window's exact size is the internal limiter test's business; this one
// asserts the behaviour an attacker or an operator would see.
func TestLoginFormRateLimitsWrongTokens(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	// Wrong guesses until the limit fires. Each in-budget guess is answered
	// with the ordinary redirect, so the refusal that ends the loop is the
	// limit's and not the token check's.
	const attemptCeiling = 100

	limited := false

	for range attemptCeiling {
		resp := f.postForm("/login", url.Values{"token": {"wrong"}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("a wrong guess = %d, want a redirect", resp.StatusCode)
		}

		if strings.Contains(resp.Header.Get("Location"), "too+many+sign-in+attempts") {
			limited = true

			break
		}
	}

	if !limited {
		t.Fatalf("%d wrong guesses were never rate-limited", attemptCeiling)
	}

	// The window is exhausted for this address, so a further attempt is
	// refused even when it guesses correctly: the limit bounds attempts,
	// not responses, and the wrong guesses must not have been free.
	refused := f.postForm("/login", url.Values{"token": {"s3cret"}})
	if loc := refused.Header.Get("Location"); !strings.Contains(loc, "too+many+sign-in+attempts") {
		t.Errorf("a correct token after the limit redirected to %q, want the rate-limit refusal", loc)
	}

	if len(refused.Cookies()) != 0 {
		t.Error("a refused sign-in set a session cookie anyway")
	}
}

// A successful sign-in must not count towards the failure window, or a
// browser that signs in normally could exhaust its own budget and find
// itself locked out one guess later.
func TestSuccessfulLoginsAreNotRateLimited(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	// Well past any plausible failure budget, so a counted success would
	// exhaust the window partway through the loop.
	for range 100 {
		resp := f.postForm("/login", url.Values{"token": {"s3cret"}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("correct login = %d, want the usual redirect", resp.StatusCode)
		}

		if len(resp.Cookies()) != 1 {
			t.Fatal("a correct login did not set the session cookie")
		}
	}
}
