package robots_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/martint17r/wsaw/internal/robots"
)

func parse(t *testing.T, body string) *robots.Rules {
	t.Helper()

	return robots.Parse(strings.NewReader(body))
}

func TestEmptyRobotsAllowsEverything(t *testing.T) {
	t.Parallel()

	r := parse(t, "")

	if !r.Allowed("wsaw", "/anything") {
		t.Error("empty robots.txt disallowed a path")
	}
}

func TestWildcardGroup(t *testing.T) {
	t.Parallel()

	r := parse(t, `
User-agent: *
Disallow: /private/
Disallow: /admin
`)

	tests := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/public/page", true},
		{"/private/", false},
		{"/private/thing", false},
		{"/admin", false},
		{"/administrator", false}, // prefix match, as the standard specifies
	}

	for _, tc := range tests {
		if got := r.Allowed("wsaw", tc.path); got != tc.want {
			t.Errorf("Allowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestSpecificAgentBeatsWildcard is how a site owner addresses wsaw
// specifically, which is the point of having a named user agent.
func TestSpecificAgentBeatsWildcard(t *testing.T) {
	t.Parallel()

	r := parse(t, `
User-agent: *
Disallow: /

User-agent: wsaw
Disallow: /secret/
`)

	if !r.Allowed("wsaw", "/public") {
		t.Error("wsaw group was ignored in favour of the wildcard")
	}

	if r.Allowed("wsaw", "/secret/x") {
		t.Error("wsaw group's own disallow was not applied")
	}

	// Another crawler still gets the wildcard rules.
	if r.Allowed("othercrawler", "/public") {
		t.Error("wildcard group was not applied to a different agent")
	}
}

func TestAllowOverridesLongerDisallow(t *testing.T) {
	t.Parallel()

	r := parse(t, `
User-agent: *
Disallow: /assets/
Allow: /assets/public/
`)

	if r.Allowed("wsaw", "/assets/private/x") {
		t.Error("disallowed path was allowed")
	}

	if !r.Allowed("wsaw", "/assets/public/x") {
		t.Error("the more specific Allow did not win")
	}
}

func TestEmptyDisallowMeansAllowAll(t *testing.T) {
	t.Parallel()

	r := parse(t, `
User-agent: *
Disallow:
`)

	if !r.Allowed("wsaw", "/anything") {
		t.Error("an empty Disallow must mean allow everything")
	}
}

func TestWildcardAndAnchorPatterns(t *testing.T) {
	t.Parallel()

	r := parse(t, `
User-agent: *
Disallow: /*.pdf$
Disallow: /search?*
`)

	tests := []struct {
		path string
		want bool
	}{
		{"/doc.pdf", false},
		{"/a/b/doc.pdf", false},
		{"/doc.pdf.html", true}, // anchored, so only a real suffix matches
		{"/search?q=x", false},
		{"/searching", true},
	}

	for _, tc := range tests {
		if got := r.Allowed("wsaw", tc.path); got != tc.want {
			t.Errorf("Allowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestCommentsAndCaseAreHandled(t *testing.T) {
	t.Parallel()

	r := parse(t, `
# a comment
USER-AGENT: WSAW
DISALLOW: /nope   # trailing comment
Crawl-delay: 5
`)

	if r.Allowed("wsaw", "/nope") {
		t.Error("field names must be case-insensitive")
	}

	if r.CrawlDelay() == 0 {
		t.Error("crawl-delay was not parsed")
	}
}

func TestMalformedLinesAreSkipped(t *testing.T) {
	t.Parallel()

	// A malformed file must not make a site unscannable, nor silently appear
	// to allow everything when it does carry real rules.
	r := parse(t, `
this line has no colon
User-agent: *
Disallow: /blocked
!!! garbage !!!
`)

	if r.Allowed("wsaw", "/blocked") {
		t.Error("valid rules were lost because of a malformed line")
	}
}

func TestCheckerAllowsWhenRobotsIsMissing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := robots.NewChecker(robots.Options{Client: srv.Client()})

	d := c.Check(context.Background(), srv.URL+"/page", robots.UserAgent)
	if !d.Allowed {
		t.Errorf("a missing robots.txt must mean no restrictions: %s", d.Reason)
	}
}

func TestCheckerDisallow(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
	}))
	defer srv.Close()

	c := robots.NewChecker(robots.Options{Client: srv.Client()})

	if d := c.Check(context.Background(), srv.URL+"/private/x", robots.UserAgent); d.Allowed {
		t.Error("disallowed path was allowed")
	} else if d.Reason == "" {
		t.Error("a skip must carry a reason, never be a silent no-op")
	}

	if d := c.Check(context.Background(), srv.URL+"/public", robots.UserAgent); !d.Allowed {
		t.Errorf("allowed path was refused: %s", d.Reason)
	}
}

func TestCheckerCachesPerOrigin(t *testing.T) {
	t.Parallel()

	var fetches int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fetches++
		}

		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
	}))
	defer srv.Close()

	c := robots.NewChecker(robots.Options{Client: srv.Client()})

	for range 5 {
		c.Check(context.Background(), srv.URL+"/page", robots.UserAgent)
	}

	if fetches != 1 {
		t.Errorf("robots.txt fetched %d times, want 1: it must be cached per origin", fetches)
	}
}

// TestCheckerFallbackIsExplicit: an unreachable robots.txt must produce a
// recorded, configured outcome rather than an arbitrary one.
func TestCheckerFallbackIsExplicit(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	permissive := robots.NewChecker(robots.Options{Client: srv.Client(), FallbackAllow: true})

	d := permissive.Check(context.Background(), srv.URL+"/page", robots.UserAgent)
	if !d.Allowed {
		t.Error("permissive fallback did not allow")
	}

	if !strings.Contains(d.Reason, "fallback") {
		t.Errorf("reason does not explain the fallback: %s", d.Reason)
	}

	strict := robots.NewChecker(robots.Options{Client: srv.Client(), FallbackAllow: false})

	if d := strict.Check(context.Background(), srv.URL+"/page", robots.UserAgent); d.Allowed {
		t.Error("strict fallback allowed a scan despite an unreadable robots.txt")
	}
}
