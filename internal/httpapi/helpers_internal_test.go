package httpapi

// The package's pure helpers, tested directly.
//
// Every other test file drives the server from outside, through
// package httpapi_test, which is the right default: it keeps the tests
// honest about what a client can actually observe. But a request cannot
// reach every branch of a filter or a clamp without a combinatorial number
// of requests, and the branches that go untested that way are exactly the
// ones that decide what a reviewer is shown. These are cheap and exhaustive
// instead.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func TestMatchesFilter(t *testing.T) {
	t.Parallel()

	req := &model.Request{
		URL:          "https://cdn.tracker.test/px",
		Host:         "cdn.tracker.test",
		Domain:       "tracker.test",
		ResourceType: "image",
		Party:        model.ThirdParty,
		Phase:        model.PhasePre,
	}

	for _, tc := range []struct {
		name string
		data resultData
		want bool
	}{
		{"no filter matches everything", resultData{}, true},

		// Host is a substring match against both the host and the
		// registrable domain, so a reviewer can type either.
		{"host substring", resultData{FilterHost: "cdn.tracker"}, true},
		{"domain substring", resultData{FilterHost: "tracker.test"}, true},
		{"host miss", resultData{FilterHost: "example.com"}, false},

		{"type match", resultData{FilterType: "image"}, true},
		{"type miss", resultData{FilterType: "script"}, false},

		{"party match", resultData{FilterParty: "third"}, true},
		{"party miss", resultData{FilterParty: "first"}, false},

		{"phase match", resultData{FilterPhase: "pre-interaction"}, true},
		{"phase miss", resultData{FilterPhase: "post-interaction"}, false},

		// Filters combine: every one given has to hold.
		{"all match", resultData{
			FilterHost: "tracker", FilterType: "image",
			FilterParty: "third", FilterPhase: "pre-interaction",
		}, true},
		{"one of several misses", resultData{
			FilterHost: "tracker", FilterType: "script",
		}, false},
	} {
		if got := matchesFilter(req, tc.data); got != tc.want {
			t.Errorf("%s: matchesFilter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClampRefresh(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   time.Duration
		want time.Duration
	}{
		{-time.Second, 0},
		{0, 0},
		// Below the floor is held at the floor rather than refused: a viewer
		// who asks for one second gets five and can see what they got.
		{time.Second, refreshFloor},
		{refreshFloor, refreshFloor},
		{30 * time.Second, 30 * time.Second},
		{refreshCeiling, refreshCeiling},
		{48 * time.Hour, refreshCeiling},
	} {
		if got := clampRefresh(tc.in); got != tc.want {
			t.Errorf("clampRefresh(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestParseRefresh(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		// Three spellings of off, because all three are things somebody
		// types.
		{"off", 0, true},
		{"OFF", 0, true},
		{"0", 0, true},
		{"none", 0, true},
		// Bare seconds and Go durations both, clamped.
		{"30", 30 * time.Second, true},
		{"1", refreshFloor, true},
		{"30s", 30 * time.Second, true},
		{"5m", 5 * time.Minute, true},
		{" 45s ", 45 * time.Second, true},
		{"99h", refreshCeiling, true},
		{"soon", 0, false},
		{"30 seconds", 0, false},
	} {
		got, ok := parseRefresh(tc.in)

		if ok != tc.wantOK {
			t.Errorf("parseRefresh(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)

			continue
		}

		if got != tc.want {
			t.Errorf("parseRefresh(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestHumaniseIntervalAndLabel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, labelOff},
		{-time.Second, labelOff},
		{30 * time.Second, "30s"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{time.Hour, "1h"},
		{2 * time.Hour, "2h"},
		// Neither a whole number of minutes nor of hours: it falls back to
		// the duration's own spelling rather than rounding silently.
		{90 * time.Second, "1m30s"},
	} {
		if got := humaniseInterval(tc.in); got != tc.want {
			t.Errorf("humaniseInterval(%s) = %q, want %q", tc.in, got, tc.want)
		}

		// Label is what the page shows, and it must agree.
		if got := (refreshSetting{Interval: tc.in}).Label(); got != tc.want {
			t.Errorf("refreshSetting{%s}.Label() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSafeLocalKeepsARedirectOnThisOrigin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back", "", "/"},
		{"a path is kept", "/targets/site/reject", "/targets/site/reject"},
		{"an absolute URL is refused", "https://evil.test/", "/"},
		{"a protocol-relative host is refused", "//evil.test/", "/"},
		{"a relative path is refused", "targets/site", "/"},
		// A control character could split the redirect header.
		{"a newline is refused", "/targets\r\nSet-Cookie: a=b", "/"},
		{"a delete character is refused", "/targets\x7f", "/"},
		// The flash parameters are appended by the caller, so anything the
		// destination carried is dropped rather than smuggled through.
		{"a query is stripped", "/targets/site?ok=already", "/targets/site"},
		{"a fragment is stripped", "/targets/site#here", "/targets/site"},
	} {
		if got := safeLocal(tc.in); got != tc.want {
			t.Errorf("%s: safeLocal(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestStaleness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	maxAge := 48 * time.Hour

	fresh := &store.Summary{StartedAt: now.Add(-time.Hour), Termination: model.TermIdle}

	for _, tc := range []struct {
		name       string
		last       *store.Summary
		wantStale  bool
		wantReason string
	}{
		// An empty result set must never read as a clean site (Tenet 5).
		{"never scanned", nil, true, "never scanned"},
		{"last scan failed", &store.Summary{
			StartedAt: now.Add(-time.Hour), Termination: model.TermError, Error: "boom",
		}, true, "last scan failed: boom"},
		{"last scan skipped", &store.Summary{
			StartedAt: now.Add(-time.Hour), Termination: model.TermSkipped, Error: "robots.txt",
		}, true, "last scan was skipped: robots.txt"},
		{"too old", &store.Summary{
			StartedAt: now.Add(-72 * time.Hour), Termination: model.TermIdle,
		}, true, "last scan is older than 48h0m0s"},
		{"fresh", fresh, false, ""},
	} {
		stale, reason := staleness(tc.last, now, maxAge)

		if stale != tc.wantStale {
			t.Errorf("%s: stale = %v, want %v", tc.name, stale, tc.wantStale)
		}

		if reason != tc.wantReason {
			t.Errorf("%s: reason = %q, want %q", tc.name, reason, tc.wantReason)
		}
	}

	// With no maximum age configured, age alone never flags a series.
	if stale, _ := staleness(&store.Summary{
		StartedAt: now.Add(-10000 * time.Hour), Termination: model.TermIdle,
	}, now, 0); stale {
		t.Error("a series was flagged stale with no maximum age configured")
	}
}

func TestSortedDomains(t *testing.T) {
	t.Parallel()

	got := sortedDomains(map[string][]string{
		"tracker.test": {"a"},
		"ads.test":     {"b"},
		"example.com":  {"c"},
	})

	want := []string{"ads.test", "example.com", "tracker.test"}

	if len(got) != len(want) {
		t.Fatalf("sortedDomains = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedDomains = %v, want %v", got, want)
		}
	}

	if len(sortedDomains(nil)) != 0 {
		t.Error("sortedDomains of nothing is not empty")
	}
}

// Both path extractors refuse anything that is not a real consent mode,
// before it can reach the store.
func TestPathAndUITargetModeValidate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		target     string
		mode       string
		wantOK     bool
		wantStatus int
	}{
		{"valid", "site", "reject", true, http.StatusOK},
		{"valid none", "site", "none", true, http.StatusOK},
		{"valid accept", "site", "accept", true, http.StatusOK},
		{"missing target", "", "reject", false, http.StatusBadRequest},
		{"unknown mode", "site", "sideways", false, http.StatusBadRequest},
		{"empty mode", "site", "", false, http.StatusBadRequest},
	} {
		for _, extract := range []struct {
			name string
			fn   func(http.ResponseWriter, *http.Request) (string, model.ConsentMode, bool)
		}{
			{"pathTargetMode", pathTargetMode},
			{"uiTargetMode", uiTargetMode},
		} {
			rec := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.SetPathValue("target", tc.target)
			r.SetPathValue("mode", tc.mode)

			target, mode, ok := extract.fn(rec, r)

			if ok != tc.wantOK {
				t.Errorf("%s/%s: ok = %v, want %v", extract.name, tc.name, ok, tc.wantOK)

				continue
			}

			if !ok {
				if rec.Code != tc.wantStatus {
					t.Errorf("%s/%s: status = %d, want %d", extract.name, tc.name, rec.Code, tc.wantStatus)
				}

				continue
			}

			if target != tc.target || string(mode) != tc.mode {
				t.Errorf("%s/%s: got %s/%s", extract.name, tc.name, target, mode)
			}
		}
	}
}

// A share link covers one result, and an artifact reference alone says
// nothing about who may read it: artifacts are content-addressed, so without
// this check a link to one harmless scan would open every stored artifact
// (Story 5.19, AC10).
func TestResultNamesArtifact(t *testing.T) {
	t.Parallel()

	res := &model.Result{
		Screenshots: []model.Artifact{{Kind: "screenshot-before-consent", Ref: "sha256/aaa"}},
		Requests: []model.Request{
			{URL: "https://example.com/", BodyRef: "sha256/bbb"},
			{URL: "https://example.com/none"},
		},
	}

	for _, tc := range []struct {
		name string
		ref  string
		want bool
	}{
		{"a screenshot it captured", "sha256/aaa", true},
		{"a body it captured", "sha256/bbb", true},
		{"somebody else's artifact", "sha256/ccc", false},
		// An empty reference must not match the requests that have no body.
		{"no reference at all", "", false},
	} {
		if got := resultNamesArtifact(res, tc.ref); got != tc.want {
			t.Errorf("%s: resultNamesArtifact(%q) = %v, want %v", tc.name, tc.ref, got, tc.want)
		}
	}
}

// The shared page builds its own links, and every one of them has to carry
// the token — and escape it, along with the reference, since both end up in
// an attribute.
func TestSharedDataLinks(t *testing.T) {
	t.Parallel()

	d := sharedData{base: "/shared/site/reject/scan-1", token: "tok+en/with spaces"}

	if got, want := d.Link("json"), "/shared/site/reject/scan-1/json?t=tok%2Ben%2Fwith+spaces"; got != want {
		t.Errorf("Link = %q, want %q", got, want)
	}

	// Path segments are escaped per segment, so the slashes that structure a
	// content-addressed reference survive and anything else does not.
	got := d.ArtifactLink("sha256/ab cd")
	want := "/shared/site/reject/scan-1/artifacts/sha256/ab%20cd?t=tok%2Ben%2Fwith+spaces"

	if got != want {
		t.Errorf("ArtifactLink = %q, want %q", got, want)
	}
}

func TestRequestedValidity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		query   string
		want    time.Duration
		wantErr bool
	}{
		// Absent means "the signer's default", not zero-length.
		{"absent", "", 0, false},
		{"a duration", "?validity=24h", 24 * time.Hour, false},
		{"minutes", "?validity=90m", 90 * time.Minute, false},
		{"not a duration", "?validity=7d", 0, true},
		{"nonsense", "?validity=soon", 0, true},
		{"zero", "?validity=0s", 0, true},
		{"negative", "?validity=-1h", 0, true},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/share/site/reject/scan-1"+tc.query, nil)

		got, err := requestedValidity(r)

		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)

			continue
		}

		if got != tc.want {
			t.Errorf("%s: validity = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestShareURLIsAbsoluteOnlyWhenABaseIsConfigured(t *testing.T) {
	t.Parallel()

	res := &model.Result{Target: "site", ConsentMode: model.ConsentReject, ScanID: "scan-1"}

	// Without a base URL the operator gets a path, which is honest: wsaw
	// does not know where it is reachable from.
	bare := &Server{opts: Options{}}
	if got, want := bare.shareURL(res, "tok"), "/shared/site/reject/scan-1?t=tok"; got != want {
		t.Errorf("shareURL without a base = %q, want %q", got, want)
	}

	// A trailing slash on the configured base must not double up.
	withBase := &Server{opts: Options{ShareBaseURL: "https://wsaw.example.com/"}}

	want := "https://wsaw.example.com/shared/site/reject/scan-1?t=tok"
	if got := withBase.shareURL(res, "tok"); got != want {
		t.Errorf("shareURL = %q, want %q", got, want)
	}
}

// The template helpers decide what a reviewer reads. "never" rather than a
// zero timestamp, above all: a scan that never happened must not render as
// one that happened at the beginning of time.
func TestUIFuncs(t *testing.T) {
	t.Parallel()

	funcs := uiFuncs()

	timeFn, ok := funcs["time"].(func(time.Time) string)
	if !ok {
		t.Fatal("uiFuncs has no time function of the expected shape")
	}

	if got := timeFn(time.Time{}); got != "never" {
		t.Errorf("time(zero) = %q, want never", got)
	}

	if got := timeFn(time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)); got != "2026-09-10 12:30:00 UTC" {
		t.Errorf("time = %q", got)
	}

	agoFn, ok := funcs["ago"].(func(time.Time) string)
	if !ok {
		t.Fatal("uiFuncs has no ago function of the expected shape")
	}

	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"zero", time.Time{}, "never"},
		{"seconds", time.Now().Add(-10 * time.Second), "just now"},
		{"minutes", time.Now().Add(-5 * time.Minute), "5m ago"},
		{"hours", time.Now().Add(-3 * time.Hour), "3h ago"},
		{"days", time.Now().Add(-50 * time.Hour), "2d ago"},
	} {
		if got := agoFn(tc.at); got != tc.want {
			t.Errorf("ago(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}

	bytesFn, ok := funcs["bytes"].(func(int64) string)
	if !ok {
		t.Fatal("uiFuncs has no bytes function of the expected shape")
	}

	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{5 << 20, "5.0 MB"},
		{3 << 30, "3.0 GB"},
	} {
		if got := bytesFn(tc.in); got != tc.want {
			t.Errorf("bytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}

	durFn, ok := funcs["dur"].(func(time.Duration) string)
	if !ok {
		t.Fatal("uiFuncs has no dur function of the expected shape")
	}

	if got := durFn(1500*time.Millisecond + 400*time.Microsecond); got != "1.5s" {
		t.Errorf("dur = %q, want 1.5s", got)
	}

	sevFn, ok := funcs["sevclass"].(func(diff.Severity) string)
	if !ok {
		t.Fatal("uiFuncs has no sevclass function of the expected shape")
	}

	if got := sevFn(diff.SeverityHigh); got != "sev-high" {
		t.Errorf("sevclass = %q, want sev-high", got)
	}

	addFn, ok := funcs["add"].(func(int, int) int)
	if !ok {
		t.Fatal("uiFuncs has no add function of the expected shape")
	}

	if got := addFn(2, 3); got != 5 {
		t.Errorf("add(2,3) = %d", got)
	}

	// since is for a scan still running: it reports elapsed time, which is
	// not the same claim as a finished scan's duration.
	sinceFn, ok := funcs["since"].(func(time.Time) string)
	if !ok {
		t.Fatal("uiFuncs has no since function of the expected shape")
	}

	if got := sinceFn(time.Now().Add(-3 * time.Second)); got != "3s" {
		t.Errorf("since = %q, want 3s", got)
	}
}
