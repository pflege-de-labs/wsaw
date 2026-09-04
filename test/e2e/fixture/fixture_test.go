package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the fixture itself, without a container and without a
// browser, so the thing the end-to-end suite depends on cannot rot silently
// between the rare runs of that suite.
//
// What a scan makes of this fixture is Story 7.4's and 7.5's business. What is
// checked here is that the fixture serves what those tests will assume.

func newSite(t *testing.T, extraBase string) http.Handler {
	t.Helper()

	// Stand-in for the pinned klaro.js, which is fetched when the image is
	// built and is not in the repository.
	dir := t.TempDir()
	klaro := filepath.Join(dir, "klaro.js")

	if err := os.WriteFile(klaro, []byte("/* klaro stand-in */\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := siteHandler(klaro, "http://tracker.example:8081", extraBase)
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	return rec.Code, string(body)
}

func setVariant(t *testing.T, h http.Handler, variant string) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/__fixture/variant",
		strings.NewReader(variant))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	return rec.Code
}

// AC2: the page must load from a second origin, or the test suite cannot tell
// a working first/third-party classifier from a broken one.
func TestThePageLoadsFromASecondOrigin(t *testing.T) {
	t.Parallel()

	h := newSite(t, "")

	code, body := get(t, h, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}

	if !strings.Contains(body, "http://tracker.example:8081/pixel.gif") {
		t.Error("the page does not load anything from the third-party origin")
	}

	if !strings.Contains(body, `src="/assets/app.js"`) {
		t.Error("the page loads no first-party script, so attribution has nothing to distinguish")
	}
}

// AC2 again, from the other side: a fixture with only one origin must be
// refused rather than started, because it would silently prove nothing.
func TestTheSiteRefusesToRunWithoutAThirdPartyOrigin(t *testing.T) {
	t.Parallel()

	_, err := siteHandler("/nonexistent/klaro.js", "", "")
	if err == nil {
		t.Fatal("the site role started without a third-party origin")
	}
}

// A missing klaro.js has to fail at startup. A page that quietly has no
// consent banner would make every consent assertion below it meaningless
// (Tenet 5).
func TestAMissingKlaroFailsLoudly(t *testing.T) {
	t.Parallel()

	_, err := siteHandler(filepath.Join(t.TempDir(), "absent.js"), "http://tracker.example:8081", "")
	if err == nil {
		t.Fatal("the site role started without klaro.js")
	}

	if !strings.Contains(err.Error(), "klaro") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// AC3: one third-party asset unconditional, one held back by Klaro. The
// unconditional one is the point — it is what "contacted before any consent
// decision" looks like.
func TestOneThirdPartyAssetIsUnconditionalAndOneIsConsentGated(t *testing.T) {
	t.Parallel()

	_, body := get(t, newSite(t, ""), "/")

	// The pixel is a plain img: nothing can hold it back.
	if !strings.Contains(body, `<img src="http://tracker.example:8081/pixel.gif"`) {
		t.Error("the unconditional third-party asset is missing")
	}

	// The script is marked the way Klaro requires in order to block it.
	for _, want := range []string{
		`type="text/plain"`,
		`data-type="application/javascript"`,
		`data-name="analytics"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the consent-gated script is missing %s, so Klaro would not hold it back", want)
		}
	}
}

// The Klaro configuration must offer both buttons wsaw's shipped rule clicks.
// Without a decline button the reject mode of the whole product would be
// untested against a real CMP.
func TestKlaroIsConfiguredWithSomethingToRejectWith(t *testing.T) {
	t.Parallel()

	code, body := get(t, newSite(t, ""), "/assets/klaro-config.js")
	if code != http.StatusOK {
		t.Fatalf("GET the klaro config = %d", code)
	}

	if !strings.Contains(body, "acceptAll: true") {
		t.Error("the notice has no accept-all button")
	}

	if !strings.Contains(body, "hideDeclineAll: false") {
		t.Error("the notice has no decline button, so reject mode cannot be exercised")
	}

	if !strings.Contains(body, "name: 'analytics'") {
		t.Error("the consent-gated service is not declared, so Klaro would not block its script")
	}
}

// AC4: determinism. Repeated requests must be byte-identical, or two scans of
// an unchanged fixture would differ and a non-empty diff would mean nothing.
func TestTheFixtureIsByteIdenticalOnRepeatedRequests(t *testing.T) {
	t.Parallel()

	h := newSite(t, "")

	for _, path := range []string{"/", "/assets/app.js", "/assets/site.css", "/assets/klaro-config.js"} {
		_, first := get(t, h, path)

		for range 3 {
			_, again := get(t, h, path)
			if again != first {
				t.Errorf("%s changed between requests, so no scan of it can be reproducible", path)

				break
			}
		}
	}
}

// AC5: the change the test makes must be exactly the change it intends — one
// new third-party host and one changed script body, nothing else.
func TestTheChangedVariantAddsAHostAndChangesAScript(t *testing.T) {
	t.Parallel()

	h := newSite(t, "http://extra-tracker.example:8081")

	_, basePage := get(t, h, "/")
	_, baseScript := get(t, h, "/assets/app.js")

	if strings.Contains(basePage, "extra-tracker.example") {
		t.Fatal("the base variant already contacts the extra host")
	}

	if code := setVariant(t, h, variantChanged); code != http.StatusOK {
		t.Fatalf("switching to the changed variant = %d", code)
	}

	_, changedPage := get(t, h, "/")
	_, changedScript := get(t, h, "/assets/app.js")

	if !strings.Contains(changedPage, "http://extra-tracker.example:8081/extra.gif") {
		t.Error("the changed variant does not contact the extra third-party host")
	}

	if changedScript == baseScript {
		t.Error("the changed variant serves the same script body, so no digest change would be reported")
	}

	// And nothing else about the page moved. Every line that differs has to
	// be one of the two intended differences, or a scan's diff would report
	// changes the test cannot account for.
	for _, line := range addedLines(basePage, changedPage) {
		switch {
		case strings.Contains(line, "extra-tracker.example"),
			strings.Contains(line, "Only in the changed variant"),
			strings.Contains(line, "<p>"), strings.Contains(line, "</p>"),
			strings.Contains(line, ">changed<"):
		default:
			t.Errorf("the changed variant altered the page beyond the extra host: %q", line)
		}
	}

	// Reversible, so a suite can run more than once.
	if code := setVariant(t, h, variantBase); code != http.StatusOK {
		t.Fatalf("switching back = %d", code)
	}

	if _, page := get(t, h, "/"); strings.Contains(page, "extra-tracker.example") {
		t.Error("the fixture did not return to its base variant")
	}
}

// addedLines returns the non-blank lines present in b but not in a.
func addedLines(a, b string) []string {
	was := make(map[string]int)
	for _, line := range strings.Split(a, "\n") {
		was[strings.TrimSpace(line)]++
	}

	var out []string

	for _, line := range strings.Split(b, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if was[line] > 0 {
			was[line]--

			continue
		}

		out = append(out, line)
	}

	return out
}

// A variant switch that cannot do what it claims must be refused, not
// silently ignored: a test that thinks it added a host and did not would
// report a passing scan as proof of nothing.
func TestTheChangedVariantIsRefusedWithoutAnExtraOrigin(t *testing.T) {
	t.Parallel()

	h := newSite(t, "")

	if code := setVariant(t, h, variantChanged); code != http.StatusPreconditionFailed {
		t.Errorf("switching to changed without an extra origin = %d, want 412", code)
	}

	if _, page := get(t, h, "/"); strings.Contains(page, "extra") {
		t.Error("the refused switch changed the page anyway")
	}
}

func TestUnknownVariantIsRefused(t *testing.T) {
	t.Parallel()

	if code := setVariant(t, newSite(t, ""), "sideways"); code != http.StatusBadRequest {
		t.Errorf("an unknown variant = %d, want 400", code)
	}
}

// The scanned page must never 404: a failed request in the fixture's baseline
// is something every later assertion has to explain away.
func TestNothingTheScannedPageAsksForIsMissing(t *testing.T) {
	t.Parallel()

	h := newSite(t, "")

	for _, path := range []string{
		"/", "/assets/site.css", "/assets/app.js",
		"/assets/klaro.js", "/assets/klaro-config.js",
		// Chrome asks for this whether the page mentions it or not.
		"/favicon.ico",
	} {
		if code, _ := get(t, h, path); code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
	}
}

// The third-party role's script must beacon to its own origin. A relative URL
// would resolve against the page's origin, so wsaw would record a first-party
// request and the initiator chain would prove nothing.
func TestTheThirdPartyScriptBeaconsToItsOwnOrigin(t *testing.T) {
	t.Parallel()

	h, err := thirdPartyHandler("http://tracker.example:8081")
	if err != nil {
		t.Fatal(err)
	}

	code, body := get(t, h, "/analytics.js")
	if code != http.StatusOK {
		t.Fatalf("GET /analytics.js = %d", code)
	}

	if !strings.Contains(body, "http://tracker.example:8081/collect") {
		t.Errorf("the beacon is not absolute to the third-party origin: %s", body)
	}

	if strings.Contains(body, "'{{selfBase}}") {
		t.Error("the origin was not substituted into the script")
	}
}

func TestTheThirdPartyRoleNeedsItsOwnBase(t *testing.T) {
	t.Parallel()

	if _, err := thirdPartyHandler(""); err == nil {
		t.Fatal("the third-party role started without knowing its own base URL")
	}
}

func TestTheThirdPartyServesEverythingThePageAsksFor(t *testing.T) {
	t.Parallel()

	h, err := thirdPartyHandler("http://tracker.example:8081")
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/pixel.gif", "/analytics.js", "/collect?e=pageview", "/extra.gif"} {
		if code, _ := get(t, h, path); code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
	}
}

// A request path arrives percent-decoded, so it can contain a newline. Logged
// raw, one request could be made to look like two — the log-forging problem
// Tenet 9 rules out for wsaw, and the fixture is no exception.
func TestARequestPathCannotForgeALogLine(t *testing.T) {
	t.Parallel()

	got := safeLogPath("/assets/app.js\nfixture site listening on :9999")

	if strings.Contains(got, "\n") {
		t.Errorf("a newline survived into the log line: %q", got)
	}

	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("the path is not quoted: %s", got)
	}

	// Still legible: sanitising must not throw away what makes a log useful.
	if !strings.Contains(got, "/assets/app.js") {
		t.Errorf("the path itself was lost: %s", got)
	}

	// And bounded, so one request cannot fill a log.
	long := safeLogPath("/" + strings.Repeat("a", 5000))
	if len(long) > 260 {
		t.Errorf("an over-long path was logged in full: %d bytes", len(long))
	}
}
