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
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Request shapes the API accepts but nothing sent: the limit parameter on
// both listing endpoints, and a shared page that actually has evidence
// attached to it.

// listLimit reads the result count from a listing response.
func listLimit(t *testing.T, resp *http.Response) int {
	t.Helper()

	var payload struct {
		Results []store.Summary `json:"results"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &payload); err != nil {
		t.Fatal(err)
	}

	return len(payload.Results)
}

func TestResultsLimit(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	at := time.Now().Add(-10 * time.Hour)
	for i := range 5 {
		f.seed("scan-"+string(rune('a'+i)), model.ConsentReject, at.Add(time.Duration(i)*time.Hour), nil)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"no limit returns everything under the default", "", 5},
		{"an honoured limit", "?limit=2", 2},
		{"a limit above what exists", "?limit=100", 5},
		// Out-of-range and unparseable limits fall back to the default
		// rather than erroring: the useful answer is the results, and a
		// listing that refused would hide them over a typo.
		{"zero falls back", "?limit=0", 5},
		{"negative falls back", "?limit=-3", 5},
		{"above the ceiling falls back", "?limit=5000", 5},
		{"not a number falls back", "?limit=lots", 5},
	} {
		resp := f.get("/api/v1/results/site/reject" + tc.query)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", tc.name, resp.StatusCode)

			continue
		}

		if got := listLimit(t, resp); got != tc.want {
			t.Errorf("%s: %d results, want %d", tc.name, got, tc.want)
		}
	}
}

func TestAuditLimit(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	// Three audited actions, so a limit of one is distinguishable from the
	// default.
	for i := range 3 {
		id := "scan-" + string(rune('a'+i))
		f.seed(id, model.ConsentReject, time.Now().Add(time.Duration(i)*time.Second), nil)
		approve(t, f, model.ConsentReject, id)
	}

	count := func(query string) int {
		t.Helper()

		var payload struct {
			Entries []store.AuditEntry `json:"entries"`
		}

		resp := f.get("/api/v1/audit" + query)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("audit%s = %d", query, resp.StatusCode)
		}

		if err := json.Unmarshal([]byte(body(t, resp)), &payload); err != nil {
			t.Fatal(err)
		}

		return len(payload.Entries)
	}

	if got := count(""); got != 3 {
		t.Errorf("audit entries = %d, want 3", got)
	}

	if got := count("?limit=1"); got != 1 {
		t.Errorf("audit?limit=1 = %d entries, want 1", got)
	}

	for _, query := range []string{"?limit=0", "?limit=-1", "?limit=99999", "?limit=some"} {
		if got := count(query); got != 3 {
			t.Errorf("audit%s = %d entries, want the default 3", query, got)
		}
	}
}

// A shared page carries its own links, and the ones for evidence are built
// per artifact. Nothing rendered a shared result that had screenshots, so
// the page's image links went unexercised — and a link without the token
// would be a broken image for whoever the link was sent to.
func TestASharedPageLinksItsScreenshotsWithTheToken(t *testing.T) {
	t.Parallel()

	signer := shareSigner(t)

	f := newFixture(t, httpapi.Options{
		WebUI:        true,
		Token:        secret.Literal("s3cret"),
		Share:        signer,
		ShareBaseURL: "https://wsaw.example.com",
	}, nil)

	before, after := seedWithScreenshots(t, f)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	resp := f.get("/shared/site/reject/scan-1?t=" + url.QueryEscape(token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared page = %d", resp.StatusCode)
	}

	html := body(t, resp)

	for _, ref := range []string{before, after} {
		want := "/shared/site/reject/scan-1/artifacts/" + ref + "?t=" + url.QueryEscape(token)

		if !strings.Contains(html, want) {
			t.Errorf("the shared page does not link artifact %s with its token", ref)
		}
	}

	// The reader must not be pointed at the authenticated artifact route,
	// which their link is not a credential for.
	if strings.Contains(html, "/api/v1/artifacts/") {
		t.Error("the shared page links the authenticated artifact route")
	}
}

// AC10 again, for the other kind of artifact. A share link covers one
// result's screenshots and one result's bodies; a stored body belonging to
// another scan is as much somebody else's data as a screenshot is.
func TestASharedLinkCoversItsOwnBodiesAndNothingElse(t *testing.T) {
	t.Parallel()

	signer := shareSigner(t)

	f := newFixture(t, httpapi.Options{
		WebUI: true,
		Token: secret.Literal("s3cret"),
		Share: signer,
	}, nil)

	// The shared scan's own body.
	mine := seedWithBody(t, f)

	// A body belonging to a scan the link does not cover.
	theirs, err := f.store.PutArtifact("body", []byte("someone else's response"))
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-2", model.ConsentAccept, time.Now(), func(res *model.Result) {
		res.Requests[0].BodyRef = theirs
	})

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	base := "/shared/site/reject/scan-1/artifacts/"
	suffix := "?t=" + url.QueryEscape(token)

	if resp := f.get(base + mine + suffix); resp.StatusCode != http.StatusOK {
		t.Errorf("the link does not serve its own result's body: %d", resp.StatusCode)
	}

	resp := f.get(base + theirs + suffix)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a body from another scan = %d, want 403", resp.StatusCode)
	}

	// An empty reference must not be treated as covered either.
	if resp := f.get(base + suffix); resp.StatusCode == http.StatusOK {
		t.Error("an empty artifact reference was served")
	}
}
