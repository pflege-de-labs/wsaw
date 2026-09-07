package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A stored body used to be write-only: capture wrote it, the result named it,
// and nothing could read it back. These tests are about it being reachable,
// and about it being served in a way a hostile page cannot exploit.

const storedScript = "(function(){window.tracker='on';})();\n"

// seedWithBody stores a result whose script request has a real stored body.
func seedWithBody(t *testing.T, f *fixture) string {
	t.Helper()

	ref, err := f.store.PutArtifact("body", []byte(storedScript))
	if err != nil {
		t.Fatal(err)
	}

	f.seed("scan-1", model.ConsentReject, time.Now(), func(res *model.Result) {
		for i := range res.Requests {
			res.Requests[i].BodyRef = ref
			res.Requests[i].BodySHA256 = "digest"
			res.Requests[i].DecodedSize = int64(len(storedScript))

			return
		}
	})

	return ref
}

func TestAStoredBodyCanBeDownloaded(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ref := seedWithBody(t, f)

	resp := f.get("/api/v1/artifacts/" + ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the stored body = %d, want 200", resp.StatusCode)
	}

	if got := body(t, resp); got != storedScript {
		t.Errorf("the served body does not match what was stored: %q", got)
	}
}

// Story 5.11, AC5: a captured body is page-controlled, so it must never come
// back as something a browser will render on wsaw's own origin.
func TestAStoredBodyIsServedAsAnInertDownload(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ref := seedWithBody(t, f)

	resp := f.get("/api/v1/artifacts/" + ref)

	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}

	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}

	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want a sandbox", got)
	}
}

// The reference reaches the store from the request, so it is untrusted like
// any other path (Tenet 9).
func TestAnArtifactReferenceCannotEscapeItsDirectory(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	seedWithBody(t, f)

	for _, ref := range []string{
		"../wsaw.db",
		"body/../../wsaw.db",
		"..%2f..%2fwsaw.db",
	} {
		resp := f.get("/api/v1/artifacts/" + ref)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("reference %q was served", ref)
		}
	}
}

func TestAMissingArtifactIsNotFound(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	if resp := f.get("/api/v1/artifacts/body/deadbeef"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a missing artifact = %d, want 404", resp.StatusCode)
	}
}

// The result endpoint carries the body, because storing it was already an
// explicit opt-in: a client that asked for bodies to be kept should not have
// to ask again to see them.
func TestTheResultEndpointCarriesStoredBodies(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	seedWithBody(t, f)

	got := body(t, f.get("/api/v1/results/site/reject/scan-1"))

	if !strings.Contains(got, `"body":`) {
		t.Error("the result carries no body")
	}

	// ...and a client polling for metadata can turn it off.
	got = body(t, f.get("/api/v1/results/site/reject/scan-1?bodies=false"))

	if strings.Contains(got, `"body":`) {
		t.Error("bodies=false still returned bodies")
	}

	if !strings.Contains(got, `"bodyRef":`) {
		t.Error("the reference went missing; without a body it is the only way to reach one")
	}
}

// The downloadable HAR carries them too, which is most of the reason to
// export a HAR at all.
func TestTheDownloadableHARCarriesStoredBodies(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	seedWithBody(t, f)

	got := body(t, f.get("/api/v1/results/site/reject/scan-1/har"))

	if !strings.Contains(got, "window.tracker") {
		t.Errorf("the HAR carries no response body:\n%s", got)
	}

	if !strings.Contains(got, `"text":`) {
		t.Error("the HAR has no content.text, so a viewer would show nothing")
	}
}

// And the result page links the body, so it is discoverable rather than
// something only an API client would find.
func TestTheResultPageLinksTheStoredBody(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	ref := seedWithBody(t, f)

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if !strings.Contains(html, "/api/v1/artifacts/"+ref) {
		t.Error("the result page does not link the stored body")
	}
}
