package httpapi_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// A wsaw whose store has gone away must say so. With a server database the
// store is a network dependency that can fail while everything else is fine,
// and the failure mode that matters is the quiet one: a page or a payload
// that reads as "nothing to report" when the truth is "cannot tell" (Tenet
// 5).
//
// Deps.Store is a concrete *store.Store rather than an interface, so closing
// it is the injection point available. Every read then fails with "sql:
// database is closed", which is exactly the shape of a store that is down.

// closedStoreFixture seeds a result and a baseline, then takes the store
// away, so the handlers under test have something to fail to read.
func closedStoreFixture(t *testing.T, opts httpapi.Options) *fixture {
	t.Helper()

	f := newFixture(t, opts, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)
	approve(t, f, model.ConsentReject, "scan-1")

	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	return f
}

func TestTheAPIReportsAStoreThatHasGoneAway(t *testing.T) {
	t.Parallel()

	f := closedStoreFixture(t, httpapi.Options{})

	// Every one of these is a read the store can no longer serve. None of
	// them may answer 200, and none may answer with an empty result set.
	for _, path := range []string{
		"/api/v1/results/site/reject",
		"/api/v1/results/site/reject/scan-1",
		"/api/v1/results/site/reject/latest",
		"/api/v1/results/site/reject/scan-1/har",
		"/api/v1/results/site/reject/scan-1/csv",
		"/api/v1/results/site/reject/scan-1/report",
		"/api/v1/diff/site/reject/scan-1",
		"/api/v1/baseline/site/reject",
		"/api/v1/audit",
	} {
		resp := f.get(path)

		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("GET %s with a closed store = %d, want 500", path, resp.StatusCode)

			continue
		}

		if got := body(t, resp); !strings.Contains(got, "database is closed") {
			t.Errorf("GET %s did not report why it could not answer: %s", path, got)
		}
	}
}

// Readiness with a reachable store still has two verdicts left to give, and
// the distinction between them matters to whoever is acting on it: a
// deployment that does not track readiness must not be reported as ready,
// and one that tracks it and says no must be a 503.
func TestReadinessWithoutAMetricsRegistrySaysSoRatherThanClaimingReady(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{}, nil, nil, func(deps *httpapi.Deps) {
		deps.Metrics = nil
	})

	resp := f.get("/api/v1/ready")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready = %d, want 200", resp.StatusCode)
	}

	// "Not tracked" is a different claim from "ready", and the payload has to
	// carry it (Tenet 5).
	if got := body(t, resp); !strings.Contains(got, "readiness is not tracked") {
		t.Errorf("ready body = %s, want it to say readiness is not tracked", got)
	}
}

func TestReadinessFailsWhenTheRegistrySaysNotReady(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{}, nil, nil, func(deps *httpapi.Deps) {
		// Configuration never loaded: the store answers, and wsaw still has
		// nothing it could scan.
		deps.Metrics.SetReady(true, false)
	})

	resp := f.get("/api/v1/ready")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready = %d, want 503", resp.StatusCode)
	}

	got := body(t, resp)

	if !strings.Contains(got, `"ready": false`) {
		t.Errorf("ready body does not say unready: %s", got)
	}

	if !strings.Contains(got, "configuration is not loaded") {
		t.Errorf("ready body does not carry the reason: %s", got)
	}
}

// A write whose store is gone must fail, not appear to succeed. An approval
// that silently did nothing would leave an operator believing a state had
// been signed off.
func TestWritesFailWhenTheStoreIsUnreachable(t *testing.T) {
	t.Parallel()

	f := closedStoreFixture(t, httpapi.Options{})

	resp := f.do(http.MethodPost, "/api/v1/baseline/site/reject", `{"scanId":"scan-1"}`)
	if resp.StatusCode == http.StatusOK {
		t.Error("approving a baseline succeeded against a closed store")
	}

	resp = f.do(http.MethodDelete, "/api/v1/baseline/site/reject", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("deleting a baseline against a closed store = %d, want 500", resp.StatusCode)
	}
}

func TestTheWebInterfaceReportsAStoreThatHasGoneAway(t *testing.T) {
	t.Parallel()

	f := closedStoreFixture(t, httpapi.Options{WebUI: true})

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/targets/site/reject", http.StatusInternalServerError},
		{"/audit", http.StatusInternalServerError},
		// The result page reports a store failure as "not found", because
		// loadResultUI cannot tell the two apart from the error alone. It is
		// pinned here as the behaviour that exists: what matters for a
		// reviewer is that the page is an error page and not an empty result.
		{"/results/site/reject/scan-1", http.StatusNotFound},
	} {
		resp := f.get(tc.path, "Accept", "text/html")

		if resp.StatusCode != tc.want {
			t.Errorf("GET %s with a closed store = %d, want %d", tc.path, resp.StatusCode, tc.want)

			continue
		}

		// The error page, not a page that renders as though the scan simply
		// had nothing in it.
		if got := body(t, resp); !strings.Contains(got, "database is closed") {
			t.Errorf("GET %s did not render the reason: %s", tc.path, got)
		}
	}
}

// A UI write against a dead store redirects with the reason in the flash
// rather than reporting success.
func TestApprovingThroughTheWebInterfaceFailsWhenTheStoreIsUnreachable(t *testing.T) {
	t.Parallel()

	f := closedStoreFixture(t, httpapi.Options{WebUI: true})

	resp := f.postForm("/approve/site/reject", url.Values{
		"scanId": {"scan-1"},
		"actor":  {"martin"},
	})

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve = %d, want a redirect", resp.StatusCode)
	}

	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	if got := loc.Query().Get("err"); !strings.Contains(got, "database is closed") {
		t.Errorf("redirect carries err=%q, want the store failure", got)
	}

	if loc.Query().Get("ok") != "" {
		t.Error("a failed approval redirected with a success message")
	}
}

// A share link is a promise that one result stays readable. When the store
// cannot serve it the reader has to be told, not shown an empty page.
func TestASharedResultFailsWhenTheStoreIsUnreachable(t *testing.T) {
	t.Parallel()

	signer := shareSigner(t)

	f := newFixture(t, httpapi.Options{WebUI: true, Share: signer, Token: secret.Literal("s3cret")}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	token, _, err := signer.Mint("site", "reject", "scan-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	suffix := "?t=" + url.QueryEscape(token)

	for _, path := range []string{
		"/shared/site/reject/scan-1",
		"/shared/site/reject/scan-1/json",
		"/shared/site/reject/scan-1/har",
		"/shared/site/reject/scan-1/csv",
		"/shared/site/reject/scan-1/artifacts/whatever",
	} {
		if resp := f.get(path + suffix); resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s served a result from a closed store", path)
		}
	}

	// Minting is a read of the result too: a link to something wsaw cannot
	// currently read would fail in front of whoever it was sent to.
	resp := f.do(http.MethodPost, "/api/v1/share/site/reject/scan-1", "")
	if resp.StatusCode == http.StatusOK {
		t.Error("a share link was minted for a result the store cannot read")
	}
}
