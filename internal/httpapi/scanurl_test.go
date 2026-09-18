package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/adhoc"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Story 5.27: a URL typed into the web interface. These cover that the page
// does not exist unless a deployment turned it on, that nothing is fetched
// that the scanner's admission checks refused, that the budget is real, and
// that the API reaches the same seam the page does (Tenet 16).

// fakeURLScanner stands in for the app. It accepts every URL whose host is
// "example.com" and refuses the rest, which is enough to see which path a
// request took without importing the real admission checks.
type fakeURLScanner struct {
	mu      sync.Mutex
	scanned []string

	acceptErr error
	scanErr   error

	// started reports each scan as it begins, so a test can wait for the
	// detached goroutine rather than sleep.
	started chan string
}

func newFakeURLScanner() *fakeURLScanner {
	return &fakeURLScanner{started: make(chan string, 8)}
}

func (f *fakeURLScanner) AcceptURL(_ context.Context, rawURL string, _ model.ConsentMode) (string, error) {
	if f.acceptErr != nil {
		return "", f.acceptErr
	}

	name := adhoc.Name(rawURL)
	if !strings.Contains(rawURL, "example.com") {
		return "", errors.New("that address is not one this wsaw will fetch")
	}

	return name, nil
}

func (f *fakeURLScanner) ScanURL(_ context.Context, rawURL string, mode model.ConsentMode) (scanner.Outcome, error) {
	f.mu.Lock()
	f.scanned = append(f.scanned, rawURL+" "+string(mode))
	f.mu.Unlock()

	f.started <- rawURL

	if f.scanErr != nil {
		return scanner.Outcome{}, f.scanErr
	}

	return scanner.Outcome{Result: &model.Result{
		ScanID: "typed-1", Target: adhoc.Name(rawURL), URL: rawURL,
		ConsentMode: mode, Termination: model.TermIdle,
	}}, nil
}

func (f *fakeURLScanner) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.scanned...)
}

// urlScanFixture builds a server with the typed-URL page on, unless opts say
// otherwise.
func urlScanFixture(t *testing.T, opts httpapi.Options, urls *fakeURLScanner) *fixture {
	t.Helper()

	if len(opts.URLScanModes) == 0 {
		opts.URLScanModes = []model.ConsentMode{model.ConsentReject, model.ConsentAccept}
	}

	return newFixtureWith(t, opts, nil, nil, func(d *httpapi.Deps) {
		d.URLScanner = urls
	})
}

// waitForLog blocks until the server has logged something matching, so a test
// never sleeps to catch a detached goroutine.
func waitForLog(t *testing.T, f *fixture, want string) {
	t.Helper()

	for range 200 {
		if strings.Contains(f.logs(), want) {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("the server never logged %q", want)
}

func scanURLForm(rawURL, mode string) url.Values {
	v := url.Values{}
	v.Set("url", rawURL)

	if mode != "" {
		v.Set("mode", mode)
	}

	return v
}

// Off unless a deployment turned it on. The page is not merely hidden: the
// route answers as though it were not there, and the write action refuses.
func TestTypedURLScanningIsOffByDefault(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowAdHocScan: true}, urls)

	if resp := f.get("/scan"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /scan with the feature off = %d, want 404", resp.StatusCode)
	}

	if resp := f.postForm("/scan", scanURLForm("https://example.com/", "reject")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /scan with the feature off = %d, want 404", resp.StatusCode)
	}

	if resp := f.do(http.MethodPost, "/api/v1/scan-url", `{"url":"https://example.com/"}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /api/v1/scan-url with the feature off = %d, want 403", resp.StatusCode)
	}

	if got := urls.calls(); len(got) != 0 {
		t.Errorf("the disabled feature still scanned %v", got)
	}

	// And the interface does not offer a page that is not there.
	if body := body(t, f.get("/")); strings.Contains(body, `href="/scan"`) {
		t.Error("the dashboard links to a page this deployment does not serve")
	}
}

func TestTypedURLFormRendersWhenEnabled(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowURLScan: true, URLScanPerHour: 7}, urls)

	page := body(t, f.get("/scan"))

	for _, want := range []string{`action="/scan"`, `name="url"`, `name="mode"`, "7 scans left"} {
		if !strings.Contains(page, want) {
			t.Errorf("the form page does not contain %q", want)
		}
	}

	// Both offered modes, and nothing else.
	if !strings.Contains(page, `value="reject"`) || !strings.Contains(page, `value="accept"`) {
		t.Error("the form does not offer the configured consent modes")
	}

	if strings.Contains(page, `value="none"`) {
		t.Error("the form offers a consent mode this deployment did not configure")
	}

	if !strings.Contains(body(t, f.get("/")), `href="/scan"`) {
		t.Error("the dashboard does not link to the form")
	}
}

func TestTypedURLScanStartsAndSendsTheReaderToTheHistory(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowURLScan: true}, urls)

	dest := location(t, f.postForm("/scan", scanURLForm("https://example.com/pricing", "accept")))

	if dest.Path != "/targets/example.com-pricing/accept" {
		t.Errorf("redirected to %q, want the typed address's history page", dest.Path)
	}

	if msg := dest.Query().Get("ok"); !strings.Contains(msg, "example.com-pricing") {
		t.Errorf("the flash message %q does not name what was started", msg)
	}

	<-urls.started

	if got := urls.calls(); len(got) != 1 || got[0] != "https://example.com/pricing accept" {
		t.Errorf("scans = %v, want one scan of the typed URL in accept mode", got)
	}
}

// A refusal belongs on the page the URL was typed on, with the typing intact.
func TestTypedURLRefusalReturnsToTheFormWithTheAddress(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowURLScan: true}, urls)

	dest := location(t, f.postForm("/scan", scanURLForm("http://192.168.0.1/", "reject")))

	if dest.Path != "/scan" {
		t.Errorf("a refusal went to %q, want back to the form", dest.Path)
	}

	if got := dest.Query().Get("url"); got != "http://192.168.0.1/" {
		t.Errorf("the form lost what was typed: %q", got)
	}

	if msg := dest.Query().Get("err"); !strings.Contains(msg, "will fetch") {
		t.Errorf("the refusal %q does not say why", msg)
	}

	if got := urls.calls(); len(got) != 0 {
		t.Errorf("a refused address was still scanned: %v", got)
	}
}

func TestTypedURLRefusesAConsentModeThatIsNotOffered(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{
		WebUI: true, AllowURLScan: true,
		URLScanModes: []model.ConsentMode{model.ConsentReject},
	}, urls)

	dest := location(t, f.postForm("/scan", scanURLForm("https://example.com/", "accept")))

	if msg := dest.Query().Get("err"); !strings.Contains(msg, "not offered") {
		t.Errorf("err = %q, want it to say the mode is not offered", msg)
	}

	if got := urls.calls(); len(got) != 0 {
		t.Errorf("a mode this deployment does not offer was still scanned: %v", got)
	}
}

// Read-only means read-only, whatever else is configured.
func TestTypedURLScanningIsRefusedInReadOnlyMode(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowURLScan: true, ReadOnly: true}, urls)

	if resp := f.get("/scan"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /scan in read-only mode = %d, want 404", resp.StatusCode)
	}

	dest := location(t, f.postForm("/scan", scanURLForm("https://example.com/", "reject")))
	if msg := dest.Query().Get("err"); !strings.Contains(msg, "read-only") {
		t.Errorf("err = %q, want it to name read-only mode", msg)
	}

	if got := urls.calls(); len(got) != 0 {
		t.Errorf("a read-only deployment still scanned %v", got)
	}
}

// The budget protects the sites being scanned, so it is one budget for the
// whole deployment rather than one per reader (Tenet 17).
func TestTypedURLScansAreBudgeted(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowURLScan: true, URLScanPerHour: 1}, urls)

	if dest := location(t, f.postForm("/scan", scanURLForm("https://example.com/a", "reject"))); dest.Query().Get("err") != "" {
		t.Fatalf("the first scan was refused: %q", dest.Query().Get("err"))
	}

	<-urls.started

	dest := location(t, f.postForm("/scan", scanURLForm("https://example.com/b", "reject")))

	msg := dest.Query().Get("err")
	if !strings.Contains(msg, "per hour") {
		t.Errorf("err = %q, want it to explain the budget", msg)
	}

	if got := urls.calls(); len(got) != 1 {
		t.Errorf("the budget did not hold: %v", got)
	}
}

func TestTypedURLScanRefusesWhenTheSameSeriesIsRunning(t *testing.T) {
	t.Parallel()

	running := func() []scanner.Running {
		return []scanner.Running{{
			ScanID: "live-1", Target: "example.com", ConsentMode: model.ConsentReject,
			Source: scanner.SourceURL,
		}}
	}

	urls := newFakeURLScanner()
	f := newFixtureWith(t,
		httpapi.Options{
			WebUI: true, AllowURLScan: true,
			URLScanModes: []model.ConsentMode{model.ConsentReject},
		},
		nil, running,
		func(d *httpapi.Deps) { d.URLScanner = urls })

	dest := location(t, f.postForm("/scan", scanURLForm("https://example.com/", "reject")))

	if dest.Path != "/targets/example.com/reject" {
		t.Errorf("redirected to %q, want the running series' page", dest.Path)
	}

	if msg := dest.Query().Get("err"); !strings.Contains(msg, "already running") {
		t.Errorf("err = %q, want it to say a scan is already running", msg)
	}

	if got := urls.calls(); len(got) != 0 {
		t.Errorf("a second scan of a running series was started: %v", got)
	}
}

// A failed scan is logged by the series it belongs to, not by echoing back
// the address somebody typed into a form.
func TestTypedURLScanLogsTheSeriesRatherThanTheAddress(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	urls.scanErr = errors.New("capture failed")

	f := urlScanFixture(t, httpapi.Options{WebUI: true, AllowURLScan: true}, urls)

	f.postForm("/scan", scanURLForm("https://example.com/secret-path?token=abc", "reject"))

	<-urls.started

	// The goroutine logs after Scan returns; wait for the line rather than
	// racing it.
	waitForLog(t, f, "typed URL reported an error")

	logs := f.logs()

	if strings.Contains(logs, "https://example.com/secret-path?token=abc") {
		t.Error("the typed address was echoed into the log")
	}

	if !strings.Contains(logs, "example.com-secret-path") {
		t.Error("the log does not name the series the failed scan belongs to")
	}
}

func TestAPIScansATypedURLSynchronously(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{AllowURLScan: true}, urls)

	resp := f.do(http.MethodPost, "/api/v1/scan-url", `{"url":"https://example.com/","consentMode":"reject"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Target string        `json:"target"`
		Result *model.Result `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	if body.Target != "example.com" || body.Result == nil {
		t.Errorf("body = %+v, want the target name and the result", body)
	}

	<-urls.started
}

func TestAPIRefusesWhatThePageRefuses(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{
		AllowURLScan: true, URLScanPerHour: 1,
		URLScanModes: []model.ConsentMode{model.ConsentReject},
	}, urls)

	if resp := f.do(http.MethodPost, "/api/v1/scan-url", `{"url":"http://10.0.0.1/"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a refused address = %d, want 400", resp.StatusCode)
	}

	if resp := f.do(http.MethodPost, "/api/v1/scan-url",
		`{"url":"https://example.com/","consentMode":"accept"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a mode that is not offered = %d, want 400", resp.StatusCode)
	}

	if resp := f.do(http.MethodPost, "/api/v1/scan-url", `{"url":"https://example.com/"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("the first scan = %d, want 200", resp.StatusCode)
	}

	<-urls.started

	resp := f.do(http.MethodPost, "/api/v1/scan-url", `{"url":"https://example.com/other"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the budgeted scan = %d, want 429", resp.StatusCode)
	}

	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After leaves a client guessing")
	}
}

// The form is a write action, so it carries the same CSRF protection every
// other one does (Story 5.11).
func TestTypedURLFormRequiresTheCSRFToken(t *testing.T) {
	t.Parallel()

	urls := newFakeURLScanner()
	f := urlScanFixture(t, httpapi.Options{
		WebUI: true, AllowURLScan: true, Token: secret.Literal("s3cret"),
	}, urls)

	resp := f.postForm("/scan", scanURLForm("https://example.com/", "reject"),
		"Authorization", "Bearer s3cret")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a post without the CSRF token = %d, want 403", resp.StatusCode)
	}

	form := scanURLForm("https://example.com/", "reject")
	form.Set("csrf", "s3cret")

	if resp := f.postForm("/scan", form, "Authorization", "Bearer s3cret"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("a post with the CSRF token = %d, want 303", resp.StatusCode)
	}

	<-urls.started
}
