package httpapi_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// TestLoginReturnsToTheRequestedPage: a reviewer who follows a link to one
// result and has to sign in first must land on that result, not the
// dashboard.
func TestLoginReturnsToTheRequestedPage(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.get(resultPath, "Accept", "text/html")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the login page", resp.StatusCode)
	}

	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	if loc.Path != "/login" || loc.Query().Get("next") != resultPath {
		t.Fatalf("Location = %q, want /login carrying next=%s", loc, resultPath)
	}

	page := f.get(loc.String(), "Accept", "text/html")

	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), `name="next" value="`+resultPath+`"`) {
		t.Errorf("login form does not carry the destination:\n%s", body)
	}

	signedIn := f.postForm("/login", url.Values{"token": {"s3cret"}, "next": {resultPath}})
	if got := signedIn.Header.Get("Location"); got != resultPath {
		t.Errorf("after sign-in Location = %q, want %q", got, resultPath)
	}
}

// TestFailedLoginKeepsTheDestination: mistyping the token once must not
// forget where the reviewer was going.
func TestFailedLoginKeepsTheDestination(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.postForm("/login", url.Values{"token": {"wrong"}, "next": {resultPath}})

	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	if loc.Path != "/login" || loc.Query().Get("next") != resultPath || loc.Query().Get("err") == "" {
		t.Errorf("Location = %q, want /login with next and err", loc)
	}
}

// TestLoginRefusesAnOffSiteDestination: next arrives from the request, so it
// must never turn the login form into an open redirect.
func TestLoginRefusesAnOffSiteDestination(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	for _, next := range []string{"https://evil.test/", "//evil.test/", `/\evil.test/`} {
		resp := f.postForm("/login", url.Values{"token": {"s3cret"}, "next": {next}})
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("next=%q: Location = %q, want /", next, got)
		}
	}
}

// TestCrossSiteLinkIsRetriedSameSite: the session cookie is SameSite=Strict,
// so a browser following a link from another site — a chat message, a
// ticket — does not send it, and a signed-in reviewer would be shown the
// login form. The first answer re-requests the same page from this origin,
// where the cookie is sent.
func TestCrossSiteLinkIsRetriedSameSite(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.get(resultPath, "Accept", "text/html", "Sec-Fetch-Site", "cross-site")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the same-site retry page", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), `http-equiv="refresh" content="0;url=`+resultPath+`"`) {
		t.Errorf("retry page does not reload %s:\n%s", resultPath, body)
	}

	// The retry is same-origin; it must go on to the login form rather than
	// bounce forever.
	retry := f.get(resultPath, "Accept", "text/html", "Sec-Fetch-Site", "same-origin")
	if retry.StatusCode != http.StatusSeeOther {
		t.Errorf("same-origin retry = %d, want a redirect to the login page", retry.StatusCode)
	}
}
