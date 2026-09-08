package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The server is tested through an in-process handler rather than a socket, so
// none of this needs a browser or a network.

type fixture struct {
	t      *testing.T
	server *httptest.Server
	store  *store.Store
	client *http.Client

	// artifactDir is where the store's bucket keeps its evidence, so a test
	// can delete an object behind the store's back the way a bucket lifecycle
	// rule would.
	artifactDir string

	// logged captures what the server wrote, for the assertions about what
	// must never reach a log — a share token, for one (Story 5.19, AC11).
	logged *lockedBuffer
}

// logs returns everything the server has logged so far.
func (f *fixture) logs() string { return f.logged.String() }

// lockedBuffer is a log sink safe to read while the server is writing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

type fakeTrigger struct {
	called int
}

func (f *fakeTrigger) Trigger(_ context.Context, target string, mode model.ConsentMode) (scanner.Outcome, error) {
	f.called++

	return scanner.Outcome{Result: &model.Result{
		ScanID: "adhoc-1", Target: target, ConsentMode: mode, Termination: model.TermIdle,
	}}, nil
}

func newFixture(t *testing.T, opts httpapi.Options, trigger httpapi.ScanTrigger) *fixture {
	t.Helper()

	return newFixtureLive(t, opts, trigger, nil)
}

// newFixtureLive additionally supplies the in-flight scan registry, which the
// server treats as absent when it is nil.
func newFixtureLive(
	t *testing.T,
	opts httpapi.Options,
	trigger httpapi.ScanTrigger,
	running func() []scanner.Running,
) *fixture {
	t.Helper()

	dir := t.TempDir()
	artifacts := filepath.Join(dir, "artifacts")

	st, err := store.Open(t.Context(), store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: artifacts,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	targets := []config.Resolved{{
		Name:         "site",
		URL:          "https://example.com/",
		ConsentModes: []model.ConsentMode{model.ConsentNone, model.ConsentReject, model.ConsentAccept},
	}}

	reg := metrics.New("test")
	reg.SetReady(true, true)

	if opts.Version == "" {
		opts.Version = "test"
	}

	logged := &lockedBuffer{}

	srv, err := httpapi.New(opts, httpapi.Deps{
		Store: st,
		// Debug level, because the request log — where a leaked token would
		// show up — is written at debug.
		Logger:  slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics: reg,
		Trigger: trigger,
		Targets: func() []config.Resolved { return targets },
		Running: running,
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// A client that does not follow redirects, so redirect behaviour is
	// observable.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           nil,
	}

	return &fixture{
		t: t, server: ts, store: st, client: client,
		artifactDir: artifacts, logged: logged,
	}
}

func (f *fixture) seed(scanID string, mode model.ConsentMode, at time.Time, mutate func(*model.Result)) *model.Result {
	f.t.Helper()

	res := &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        scanID,
		Target:        "site",
		URL:           "https://example.com/",
		ConsentMode:   mode,
		StartedAt:     at,
		FinishedAt:    at.Add(time.Second),
		Duration:      time.Second,
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied},
		Requests: []model.Request{
			{
				URL: "https://example.com/", NormalizedURL: "https://example.com/", Method: "GET",
				ResourceType: "document", Host: "example.com", Domain: "example.com",
				Party: model.FirstParty, Phase: model.PhasePre, Status: 200,
			},
			{
				URL: "https://tracker.test/px", NormalizedURL: "https://tracker.test/px", Method: "GET",
				ResourceType: "image", Host: "tracker.test", Domain: "tracker.test",
				Party: model.ThirdParty, Phase: model.PhasePre, Status: 200,
			},
		},
	}

	if mutate != nil {
		mutate(res)
	}

	if err := f.store.PutResult(res); err != nil {
		f.t.Fatal(err)
	}

	return res
}

func (f *fixture) get(path string, headers ...string) *http.Response {
	f.t.Helper()

	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.server.URL+path, nil)
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

func (f *fixture) postForm(path string, form url.Values, headers ...string) *http.Response {
	f.t.Helper()

	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodPost, f.server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		f.t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}

	return resp
}

// mustRequest builds a request bound to the test's context, so a hung call
// cannot outlive the test.
func mustRequest(t *testing.T, f *fixture, method, path, body string) *http.Request {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}

	return req
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()

	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestHealthAndReady(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	if resp := f.get("/api/v1/health"); resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d", resp.StatusCode)
	}

	resp := f.get("/api/v1/ready")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("ready status = %d", resp.StatusCode)
	}

	var payload struct {
		Ready bool `json:"ready"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &payload); err != nil {
		t.Fatal(err)
	}

	if !payload.Ready {
		t.Error("ready = false with Chrome usable and config loaded")
	}
}

func TestTargetsIncludeStalenessForNeverScanned(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	var payload struct {
		Targets []httpapi.TargetView `json:"targets"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/targets"))), &payload); err != nil {
		t.Fatal(err)
	}

	if len(payload.Targets) != 1 {
		t.Fatalf("got %d targets", len(payload.Targets))
	}

	for _, s := range payload.Targets[0].Series {
		if !s.Stale {
			t.Errorf("series %s is not flagged stale despite never being scanned", s.Mode)
		}

		if s.StaleReason == "" {
			t.Errorf("series %s has no stale reason", s.Mode)
		}
	}
}

// TestFailedScanKeepsSeriesStale is Tenet 5 at the API boundary: a target
// whose last scan failed must not look healthy.
func TestFailedScanKeepsSeriesStale(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	f.seed("scan-bad", model.ConsentReject, time.Now(), func(r *model.Result) {
		r.Termination = model.TermError
		r.Error = "navigate failed"
	})

	var payload struct {
		Targets []httpapi.TargetView `json:"targets"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/targets"))), &payload); err != nil {
		t.Fatal(err)
	}

	for _, s := range payload.Targets[0].Series {
		if s.Mode != model.ConsentReject {
			continue
		}

		if !s.Stale {
			t.Error("a series whose last scan failed is not flagged")
		}

		if !strings.Contains(s.StaleReason, "failed") {
			t.Errorf("stale reason = %q, want it to say the scan failed", s.StaleReason)
		}
	}
}

func TestResultEndpoints(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	for _, path := range []string{
		"/api/v1/results/site/reject",
		"/api/v1/results/site/reject/scan-1",
		"/api/v1/results/site/reject/latest",
		"/api/v1/results/site/reject/scan-1/har",
		"/api/v1/results/site/reject/scan-1/csv",
		"/api/v1/results/site/reject/scan-1/report",
		"/api/v1/diff/site/reject/scan-1",
	} {
		if resp := f.get(path); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, resp.StatusCode)
		}
	}
}

// TestAPreviousResultWhoseDocumentIsGoneIsNotSilence is the regression for the
// worst thing this epic could have shipped (Story 8.2, AC5; Tenet 5).
//
// While a document lived in its row, "the previous result is not there" could
// only mean there was no previous scan. Now the row and the document can part
// company — a bucket lifecycle rule, a restore without its matching database —
// and a comparison that treated the two the same would render the page a
// first-ever scan renders: not comparable, nothing changed, "no baseline to
// compare against". A tracker added between the two scans would go unreported,
// and the only trace would be a reason that is untrue.
func TestAPreviousResultWhoseDocumentIsGoneIsNotSilence(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	base := time.Now().Add(-time.Hour)
	older := f.seed("scan-1", model.ConsentReject, base, nil)
	f.seed("scan-2", model.ConsentReject, base.Add(time.Minute), nil)

	f.removeStoredDocument(older)

	// The diff of the newer scan has to say that its comparison anchor exists
	// and its evidence does not.
	var report struct {
		Comparable bool   `json:"comparable"`
		Reason     string `json:"reason"`
	}

	resp := f.get("/api/v1/diff/site/reject/scan-2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the diff = %d", resp.StatusCode)
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &report); err != nil {
		t.Fatal(err)
	}

	if report.Comparable {
		t.Error("a scan was compared against a result whose document is gone")
	}

	if report.Reason != diff.ReasonEvidenceGone {
		t.Errorf("the diff reports %q, want it to say the stored evidence is gone rather than that there is nothing to compare", report.Reason)
	}

	// And reading the older result itself is Gone, not Not Found: the scan is
	// in the index, so claiming it never happened would be untrue.
	if resp := f.get("/api/v1/results/site/reject/scan-1"); resp.StatusCode != http.StatusGone {
		t.Errorf("GET the result whose document is gone = %d, want %d", resp.StatusCode, http.StatusGone)
	}
}

// removeStoredDocument deletes a result's document from the bucket without
// telling the store, which is what a lifecycle rule or a restore of the bucket
// alone does.
func (f *fixture) removeStoredDocument(res *model.Result) {
	f.t.Helper()

	document, err := json.Marshal(res)
	if err != nil {
		f.t.Fatal(err)
	}

	sum := sha256.Sum256(document)
	path := filepath.Join(f.artifactDir, "result", hex.EncodeToString(sum[:]))

	if err := os.Remove(path); err != nil {
		f.t.Fatalf("removing the stored document: %v", err)
	}
}

func TestInvalidConsentModeIsRejected(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	resp := f.get("/api/v1/results/site/maybe")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMissingResultIsNotFound(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	if resp := f.get("/api/v1/results/site/reject/absent"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	for _, path := range []string{"/", "/api/v1/health", "/static/wsaw.css"} {
		resp := f.get(path, "Accept", "text/html")

		csp := resp.Header.Get("Content-Security-Policy")
		if csp == "" {
			t.Errorf("%s has no Content-Security-Policy", path)
		}

		// The point of serving CSS as a file is that inline is forbidden.
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s CSP allows unsafe-inline: %s", path, csp)
		}

		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s CSP is not default-deny: %s", path, csp)
		}

		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s X-Content-Type-Options = %q", path, got)
		}

		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s X-Frame-Options = %q", path, got)
		}

		if got := resp.Header.Get("Referrer-Policy"); got == "" {
			t.Errorf("%s has no Referrer-Policy", path)
		}
	}
}

// TestCapturedURLIsEscapedInHTML is the XSS case a web interface introduces:
// every URL rendered comes from a hostile page.
func TestCapturedURLIsEscapedInHTML(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	const payload = `<script>alert("xss")</script>`

	f.seed("scan-1", model.ConsentReject, time.Now(), func(r *model.Result) {
		r.Requests = append(r.Requests, model.Request{
			URL:           "https://evil.test/" + payload,
			NormalizedURL: "https://evil.test/" + payload,
			Method:        "GET",
			ResourceType:  "script",
			Host:          "evil.test",
			Domain:        "evil.test",
			Party:         model.ThirdParty,
			Phase:         model.PhasePre,
			Status:        200,
			Initiator:     model.Initiator{Type: "script", URL: "https://evil.test/" + payload},
		})
		r.Warnings = append(r.Warnings, "warning containing "+payload)
	})

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))

	if strings.Contains(html, "<script>alert") {
		t.Error("a captured URL was rendered as markup")
	}

	// It must still be visible, escaped, rather than dropped.
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("the captured URL was not rendered at all; it must appear as escaped text")
	}
}

func TestReadOnlyBlocksWrites(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{ReadOnly: true, WebUI: true, AllowAdHocScan: true}, &fakeTrigger{})
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	req := mustRequest(t, f, http.MethodPost, "/api/v1/baseline/site/reject", `{"scanId":"scan-1"}`)

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("baseline write in read-only mode = %d, want 403", resp.StatusCode)
	}

	if resp := f.postForm("/rescan/site/reject", url.Values{}); resp.StatusCode == http.StatusOK {
		t.Error("ad-hoc scan succeeded in read-only mode")
	}
}

func TestAdHocScanDisabledByDefault(t *testing.T) {
	t.Parallel()

	trigger := &fakeTrigger{}
	f := newFixture(t, httpapi.Options{}, trigger)

	req := mustRequest(t, f, http.MethodPost, "/api/v1/scan/site/reject", "")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when ad-hoc scanning is not enabled", resp.StatusCode)
	}

	if trigger.called != 0 {
		t.Error("the trigger ran despite ad-hoc scanning being disabled")
	}
}

// TestAdHocScanOnlyForConfiguredTargets: accepting an arbitrary target would
// turn the API into a request-forgery primitive.
func TestAdHocScanOnlyForConfiguredTargets(t *testing.T) {
	t.Parallel()

	trigger := &fakeTrigger{}
	f := newFixture(t, httpapi.Options{AllowAdHocScan: true}, trigger)

	req := mustRequest(t, f, http.MethodPost, "/api/v1/scan/unknown-target/reject", "")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unconfigured target", resp.StatusCode)
	}

	if trigger.called != 0 {
		t.Error("the trigger ran for an unconfigured target")
	}
}

func TestAdHocScanForConfiguredTarget(t *testing.T) {
	t.Parallel()

	trigger := &fakeTrigger{}
	f := newFixture(t, httpapi.Options{AllowAdHocScan: true}, trigger)

	req := mustRequest(t, f, http.MethodPost, "/api/v1/scan/site/reject", "")

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body(t, resp))
	}

	if trigger.called != 1 {
		t.Errorf("trigger called %d times, want 1", trigger.called)
	}
}

func TestBaselineApprovalAndAudit(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	req := mustRequest(t, f, http.MethodPost, "/api/v1/baseline/site/reject", `{"scanId":"scan-1","actor":"martin","note":"reviewed"}`)

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body(t, resp))
	}

	audit := body(t, f.get("/api/v1/audit"))
	if !strings.Contains(audit, "baseline-approved") || !strings.Contains(audit, "martin") {
		t.Errorf("approval was not audited: %s", audit)
	}
}

func TestApprovingAFailedScanIsRejected(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	f.seed("scan-bad", model.ConsentReject, time.Now(), func(r *model.Result) {
		r.Termination = model.TermError
		r.Error = "boom"
	})

	req := mustRequest(t, f, http.MethodPost, "/api/v1/baseline/site/reject", `{"scanId":"scan-bad"}`)

	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode == http.StatusOK {
		t.Error("a failed scan was accepted as a baseline")
	}
}

func TestAuthenticationRequiredWhenTokenSet(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret")}, nil)

	resp := f.get("/api/v1/targets")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated request = %d, want 401", resp.StatusCode)
	}

	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}

	authed := f.get("/api/v1/targets", "Authorization", "Bearer s3cret")
	if authed.StatusCode != http.StatusOK {
		t.Errorf("authenticated request = %d", authed.StatusCode)
	}

	wrong := f.get("/api/v1/targets", "Authorization", "Bearer wrong")
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", wrong.StatusCode)
	}
}

func TestHTMLClientIsRedirectedToLogin(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.get("/", "Accept", "text/html")
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect to the login page", resp.StatusCode)
	}

	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("Location = %q", got)
	}

	// The login page itself must be reachable.
	if resp := f.get("/login", "Accept", "text/html"); resp.StatusCode != http.StatusOK {
		t.Errorf("login page = %d", resp.StatusCode)
	}
}

func TestLoginSetsHardenedCookie(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.postForm("/login", url.Values{"token": {"s3cret"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}

	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}

	c := cookies[0]

	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}

	if c.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie is not SameSite=Strict")
	}
}

func TestWrongTokenDoesNotAuthenticate(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil)

	resp := f.postForm("/login", url.Values{"token": {"wrong"}})
	if len(resp.Cookies()) != 0 {
		t.Error("a session cookie was issued for a wrong token")
	}
}

// TestCSRFRequiredForFormWrites: without it, any page a reviewer visits could
// approve a baseline in their name.
func TestCSRFRequiredForFormWrites(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{
		Token: secret.Literal("s3cret"), WebUI: true, AllowAdHocScan: true,
	}, &fakeTrigger{})

	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	// Authenticated via the bearer header but with no CSRF token in the form.
	resp := f.postForm("/approve/site/reject",
		url.Values{"scanId": {"scan-1"}},
		"Authorization", "Bearer s3cret")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without a CSRF token", resp.StatusCode)
	}

	withToken := f.postForm("/approve/site/reject",
		url.Values{"scanId": {"scan-1"}, "csrf": {"s3cret"}},
		"Authorization", "Bearer s3cret")

	if withToken.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d with a valid CSRF token: %s", withToken.StatusCode, body(t, withToken))
	}
}

func TestWebUIPagesRender(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	f.seed("scan-1", model.ConsentReject, time.Now(), nil)
	f.seed("scan-2", model.ConsentAccept, time.Now(), nil)

	for _, path := range []string{
		"/",
		"/targets/site/reject",
		"/results/site/reject/scan-1",
		"/compare/site",
		"/audit",
	} {
		resp := f.get(path, "Accept", "text/html")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, resp.StatusCode)

			continue
		}

		html := body(t, resp)

		if !strings.Contains(html, "<!DOCTYPE html>") {
			t.Errorf("%s did not render a document", path)
		}

		if strings.Contains(html, "<no value>") {
			t.Errorf("%s has an unresolved template value", path)
		}
	}
}

// TestCompareShowsPreConsentAcrossModes covers the view that makes the
// headline finding visible.
func TestCompareShowsPreConsentAcrossModes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/compare/site", "Accept", "text/html"))

	if !strings.Contains(html, "tracker.test") {
		t.Error("the third-party domain is missing from the comparison")
	}

	if !strings.Contains(html, "pre-consent") {
		t.Error("the pre-consent marker is missing from the comparison")
	}
}

func TestWebUIDisabledLeavesAPIWorking(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: false}, nil)

	if resp := f.get("/api/v1/health"); resp.StatusCode != http.StatusOK {
		t.Errorf("API health = %d with the UI disabled", resp.StatusCode)
	}

	if resp := f.get("/", "Accept", "text/html"); resp.StatusCode == http.StatusOK {
		t.Error("the dashboard is served despite the UI being disabled")
	}
}

func TestMetricsEndpointIsOptional(t *testing.T) {
	t.Parallel()

	off := newFixture(t, httpapi.Options{}, nil)
	if resp := off.get("/metrics"); resp.StatusCode == http.StatusOK {
		t.Error("metrics are served without being enabled")
	}

	on := newFixture(t, httpapi.Options{MetricsEnabled: true, MetricsPath: "/metrics"}, nil)

	resp := on.get("/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}

	if !strings.Contains(body(t, resp), "wsaw_last_successful_scan_timestamp_seconds") {
		t.Error("metrics output is missing the liveness metric")
	}
}

func TestDownloadFilenameCannotBreakTheHeader(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed(`scan"; drop`, model.ConsentReject, time.Now(), nil)

	resp := f.get(`/api/v1/results/site/reject/latest/har`)

	cd := resp.Header.Get("Content-Disposition")
	if strings.Count(cd, `"`) != 2 {
		t.Errorf("Content-Disposition is malformed: %q", cd)
	}
}
