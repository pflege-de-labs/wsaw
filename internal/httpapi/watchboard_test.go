package httpapi_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// The target list is a watchboard: one tile per series, grouped by the "env"
// label, with the scan age and the pre-consent count carrying the weight.
// Grouping, the filter and the collapse are display decisions applied to the
// TargetViews the dashboard already built, so these tests assert on the
// rendered page and never on the JSON API, which is unchanged.

// twoEnvs is a fixture with one target in each of two environments, which is
// the smallest list that can be grouped wrongly.
func twoEnvs(t *testing.T) *fixture {
	t.Helper()

	return newFixtureWith(t, httpapi.Options{WebUI: true}, nil, nil, func(d *httpapi.Deps) {
		d.Targets = func() []config.Resolved {
			return []config.Resolved{
				{
					Name:         "site",
					URL:          "https://example.com/",
					Labels:       map[string]string{"env": "prod"},
					ConsentModes: []model.ConsentMode{model.ConsentNone, model.ConsentReject},
				},
				{
					Name:         "partner",
					URL:          "https://partner.example.net/de-de",
					Labels:       map[string]string{"env": "iagfdk"},
					ConsentModes: []model.ConsentMode{model.ConsentReject},
				},
			}
		}
	})
}

func TestTheBoardGroupsTargetsByTheirEnvLabel(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{
		`<span class="watch-env-label">prod</span>`,
		`<span class="watch-env-label">iagfdk</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the board did not render a group for %s", want)
		}
	}

	// Ordered by label value, so the list does not reshuffle between renders.
	if strings.Index(html, ">iagfdk<") > strings.Index(html, ">prod<") {
		t.Error("groups are not in label order")
	}
}

// A target nobody gave an env is a configuration fact, not a rendering edge
// case: it gets a named group rather than being folded into another one.
func TestAnUnlabelledTargetIsGroupedUnderANamedGroup(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, "no env label") {
		t.Error("a target with no env label was not grouped honestly")
	}
}

// The core read paths must work with script off, so the group is a <details>
// the browser collapses on its own — not a scripted toggle.
func TestGroupsAreDetailsElementsAndOpenByDefault(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	html := body(t, f.get("/", "Accept", "text/html"))

	if !strings.Contains(html, `<details class="watch-env" data-env="prod" open>`) {
		t.Errorf("groups are not open <details> elements by default\n%s", html)
	}

	if !strings.Contains(html, "<summary>") {
		t.Error("a group has no summary, so it cannot be collapsed without script")
	}
}

// The collapse is the viewer's preference, remembered per browser exactly as
// the refresh interval is — one viewer collapsing a group must not change
// what anybody else sees.
func TestACollapsedGroupIsRememberedPerBrowser(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	html := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_envs=prod"))

	if !strings.Contains(html, `<details class="watch-env" data-env="prod">`) {
		t.Errorf("a remembered collapsed group was rendered open\n%s", html)
	}

	if !strings.Contains(html, `<details class="watch-env" data-env="iagfdk" open>`) {
		t.Error("collapsing one group collapsed another")
	}
}

// The filter is applied on the server as well as in the script, so the
// remembered preference means the same thing with JavaScript off.
func TestTheFilterIsAppliedServerSideFromTheCookie(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	html := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_filter=partner"))

	if !strings.Contains(html, "partner.example.net") {
		t.Error("the filter hid the target it matches")
	}

	if strings.Contains(html, `href="/compare/site"`) {
		t.Error("a target that does not match the filter was still rendered")
	}

	// A filtered board must say what it is not showing. A watcher that
	// reports "1 target" while two are configured has misrepresented itself.
	if !strings.Contains(html, "showing 1/2") {
		t.Errorf("the filtered board does not state the configured total\n%s", html)
	}

	// And the field shows the filter that is in force, rather than looking
	// empty while quietly filtering.
	if !strings.Contains(html, `value="partner"`) {
		t.Error("the filter field does not echo the filter being applied")
	}
}

// The filter matches what is written on the row — name, url and labels — so
// a reader can always see why something matched.
func TestTheFilterMatchesLabelsAndURLs(t *testing.T) {
	t.Parallel()

	for _, needle := range []string{"env%3Diagfdk", "example.net", "PARTNER"} {
		f := twoEnvs(t)

		html := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_filter="+needle))

		if !strings.Contains(html, "showing 1/2") {
			t.Errorf("filtering by %q did not match exactly the partner target", needle)
		}
	}
}

// Story 5.22, AC8's reasoning: a filter nobody can clear is worse than none.
func TestAFilterThatMatchesNothingSaysSo(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	html := body(t, f.get("/", "Accept", "text/html", "Cookie", "wsaw_filter=nothing-matches-this"))

	if !strings.Contains(html, "No target matches this filter") {
		t.Error("an empty filtered board does not explain itself")
	}

	if !strings.Contains(html, "2 are configured") {
		t.Error("an empty filtered board does not say how many targets exist")
	}
}

// The headline number: third-party hosts contacted before the consent
// interaction, totalled across the series on screen. seed() makes exactly one
// such request per scan.
func TestTheHeaderTotalsPreConsentHosts(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	f.seed("scan-none", model.ConsentNone, time.Now(), nil)
	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	// "none" and "reject" agree here, so they fold into one displayed row and
	// the board counts what it shows.
	if !strings.Contains(html, `class="watch-count watch-pre is-set">1 pre-consent hosts`) {
		t.Errorf("the board does not total the pre-consent hosts it is showing\n%s", html)
	}

	// And the tile that carries it is marked, in words as well as colour.
	if !strings.Contains(html, "has-pre") || !strings.Contains(html, "1 pre") {
		t.Error("a tile with a pre-consent host is not marked as one")
	}
}

// A clean board must not shout. Counts stay muted until they are non-zero,
// because a page that reports "0 stale" in red every day teaches the reader
// to stop looking at it.
func TestZeroCountsAreNotFlagged(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	f.seed("scan-clean", model.ConsentReject, time.Now(), func(r *model.Result) {
		r.Requests = r.Requests[:1]
	})

	html := body(t, f.get("/", "Accept", "text/html"))

	if strings.Contains(html, `watch-pre is-set`) {
		t.Error("a board with no pre-consent hosts flagged the count anyway")
	}
}

// Every link the previous list offered still has to be on the board: a
// redesign that loses the way into a scan has removed the feature it was
// meant to surface.
func TestTheBoardKeepsTheLinksTheListHad(t *testing.T) {
	t.Parallel()

	f := twoEnvs(t)

	f.seed("scan-reject", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/", "Accept", "text/html"))

	for _, want := range []string{
		`href="/results/site/reject/scan-reject"`,
		`href="/targets/site/reject"`,
		`href="/compare/site"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the board is missing the link %s", want)
		}
	}
}

// Storing a filter is a write, so it carries CSRF protection and answers with
// a redirect — the same treatment the refresh interval gets.
func TestStoringAFilterRequiresCSRFAndRedirects(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{Token: secret.Literal("s3cret"), WebUI: true}, nil, nil, func(d *httpapi.Deps) {
		d.Targets = func() []config.Resolved {
			return []config.Resolved{
				{
					Name:         "site",
					URL:          "https://example.com/",
					Labels:       map[string]string{"env": "prod"},
					ConsentModes: []model.ConsentMode{model.ConsentNone, model.ConsentReject},
				},
				{
					Name:         "partner",
					URL:          "https://partner.example.net/de-de",
					Labels:       map[string]string{"env": "iagfdk"},
					ConsentModes: []model.ConsentMode{model.ConsentReject},
				},
			}
		}
	})

	resp := f.postForm("/filter",
		url.Values{"filter": {"partner"}, "return": {"/"}},
		"Authorization", "Bearer s3cret")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a filter POST without a CSRF token was accepted: %d", resp.StatusCode)
	}

	withToken := f.postForm("/filter",
		url.Values{"filter": {"partner"}, "return": {"/"}, "csrf": {"s3cret"}},
		"Authorization", "Bearer s3cret")
	if withToken.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d with a valid CSRF token: %s", withToken.StatusCode, body(t, withToken))
	}
}
