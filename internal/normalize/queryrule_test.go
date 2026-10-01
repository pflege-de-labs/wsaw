package normalize_test

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

func TestQueryRuleReplacesGlobalQueryHandlingForMatchingURLs(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		DropQueryParams: []string{"cb"},
		QueryRules: []normalize.QueryRule{
			{URLPattern: `^https://px\.example/hit`, KeepQueryParams: []string{"id"}},
		},
	})

	tests := []struct{ name, in, want string }{
		{"matching URL keeps only identity", "https://px.example/hit?id=7&dl=/a&uaa=arm", "https://px.example/hit?id=7"},
		{"global drop list no longer applies there", "https://px.example/hit?cb=1", "https://px.example/hit"},
		{"other URLs keep the global handling", "https://example.com/a.js?cb=1&page=2", "https://example.com/a.js?page=2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := n.Key(tc.in, model.ThirdParty); got != tc.want {
				t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestQueryRuleScopedByParty(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		QueryRules: []normalize.QueryRule{{Party: model.ThirdParty, DropAllQuery: true}},
	})

	const in = "https://example.com/search?q=shoes"

	if got := n.Key(in, model.ThirdParty); got != "https://example.com/search" {
		t.Errorf("third party: Key = %q, want the query dropped", got)
	}

	if got := n.Key(in, model.FirstParty); got != in {
		t.Errorf("first party: Key = %q, want the query kept", got)
	}

	// A caller that does not know the party must not get a party-scoped
	// rule applied by accident.
	if got := n.Key(in, ""); got != in {
		t.Errorf("unknown party: Key = %q, want the query kept", got)
	}
}

func TestQueryRulePatternAndPartyMustBothMatch(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		QueryRules: []normalize.QueryRule{
			{URLPattern: `/collect`, Party: model.ThirdParty, DropAllQuery: true},
		},
	})

	if got := n.Key("https://tm.example.com/collect?x=1", model.FirstParty); got != "https://tm.example.com/collect?x=1" {
		t.Errorf("Key = %q; a first-party request matched a third-party rule", got)
	}
}

func TestFirstMatchingQueryRuleWins(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		QueryRules: []normalize.QueryRule{
			{URLPattern: `px\.example/hit`, KeepQueryParams: []string{"id", "tid"}},
			{URLPattern: `px\.example`, DropAllQuery: true},
		},
	})

	if got := n.Key("https://px.example/hit?tid=G-1&z=9", ""); got != "https://px.example/hit?tid=G-1" {
		t.Errorf("Key = %q, want the first rule applied", got)
	}

	if got := n.Key("https://px.example/other?tid=G-1", ""); got != "https://px.example/other" {
		t.Errorf("Key = %q, want the second rule applied", got)
	}
}

func TestInvalidQueryRulePatternFailsAtCompileTime(t *testing.T) {
	t.Parallel()

	_, err := normalize.New(normalize.Rules{
		QueryRules: []normalize.QueryRule{{URLPattern: "([unclosed", DropAllQuery: true}},
	})
	if err == nil {
		t.Fatal("New accepted an invalid pattern")
	}
}

// The shipped rules are tested against the URLs that produced the churn they
// exist for, with the per-visit parameters varied the way two real visits
// vary them.
func TestDefaultQueryRulesCollapsePerVisitParameters(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{QueryRules: normalize.DefaultQueryRules})

	tests := []struct{ name, a, b string }{
		{
			"gtag loader rollout state",
			"https://www.googletagmanager.com/gtag/js?id=G-13819ZLYSX&cx=c&gtm=4e69t1",
			"https://www.googletagmanager.com/gtag/js?id=G-13819ZLYSX&cx=c&gtm=4e6a01",
		},
		{
			"GA4 hit",
			"https://region1.google-analytics.com/g/collect?v=2&tid=G-1&gtm=45je1&_p=111&dl=https%3A%2F%2Fa.example%2F",
			"https://region1.google-analytics.com/g/collect?v=2&tid=G-1&gtm=45je2&_p=222&dl=https%3A%2F%2Fa.example%2Fb",
		},
		{
			"server-side tagging host",
			"https://tm.example.com/g/collect?tid=G-1&_s=1",
			"https://tm.example.com/g/collect?tid=G-1&_s=2",
		},
		{
			"Google Ads remarketing",
			"https://googleads.g.doubleclick.net/pagead/viewthroughconversion/825411646/?gtm=45be1&uaa=arm&dma=1",
			"https://googleads.g.doubleclick.net/pagead/viewthroughconversion/825411646/?gtm=45be2&uaa=x86&dma=0",
		},
		{
			"consent-mode ping",
			"https://www.google.com/ccm/collect?en=page_view&gcs=G111&dt=A",
			"https://www.google.com/ccm/collect?en=page_view&gcs=G100&dt=B",
		},
		{
			"audience list on a country domain",
			"https://www.google.de/pagead/1p-user-list/967278100/?random=1&gtm=a",
			"https://www.google.de/pagead/1p-user-list/967278100/?random=2&gtm=b",
		},
		{
			"Microsoft UET",
			"https://bat.bing.com/action/0?ti=5000&mid=a&rn=1",
			"https://bat.bing.com/action/0?ti=5000&mid=b&rn=2",
		},
		{
			"Meta pixel",
			"https://www.facebook.com/tr/?id=123&ev=PageView&ts=1",
			"https://www.facebook.com/tr/?id=123&ev=PageView&ts=2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if ka, kb := n.Key(tc.a, model.ThirdParty), n.Key(tc.b, model.ThirdParty); ka != kb {
				t.Errorf("two visits keyed apart:\n  %s\n  %s", ka, kb)
			}
		})
	}
}

// What names the account or container must survive: a site switching its
// measurement ID is a change somebody needs to see (Tenet 5).
func TestDefaultQueryRulesKeepIdentity(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{QueryRules: normalize.DefaultQueryRules})

	tests := []struct{ name, a, b string }{
		{"gtag tag ID", "https://www.googletagmanager.com/gtag/js?id=G-1&gtm=a", "https://www.googletagmanager.com/gtag/js?id=G-2&gtm=a"},
		{"GA4 property", "https://www.google-analytics.com/g/collect?tid=G-1", "https://www.google-analytics.com/g/collect?tid=G-2"},
		{"UET tag", "https://bat.bing.com/action/0?ti=1", "https://bat.bing.com/action/0?ti=2"},
		{"Meta pixel", "https://www.facebook.com/tr/?id=1", "https://www.facebook.com/tr/?id=2"},
		{"Pinterest tag", "https://ct.pinterest.com/v3/?tid=1", "https://ct.pinterest.com/v3/?tid=2"},
		{"Ads conversion ID in the path", "https://www.google.com/pagead/1p-user-list/1/?x=1", "https://www.google.com/pagead/1p-user-list/2/?x=1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if n.Key(tc.a, model.ThirdParty) == n.Key(tc.b, model.ThirdParty) {
				t.Errorf("identity lost: %s and %s share a key", tc.a, tc.b)
			}
		})
	}
}

// A shipped rule must not reach a URL it was not written for: the GTM
// container loader names its container in id and keeps every parameter.
func TestDefaultQueryRulesLeaveOtherURLsAlone(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{QueryRules: normalize.DefaultQueryRules})

	for _, in := range []string{
		"https://www.googletagmanager.com/gtm.js?id=GTM-ABC&l=dataLayer",
		"https://www.google.com/recaptcha/api.js?render=site-key",
		"https://example.com/collect?page=2",
	} {
		if got := n.Key(in, model.ThirdParty); got != in {
			t.Errorf("Key(%q) = %q; a shipped rule matched a URL it was not written for", in, got)
		}
	}
}

func TestRekeyDerivesTheKeyAgainFromTheRawURL(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{QueryRules: normalize.DefaultQueryRules})

	req := model.Request{
		URL:           "https://www.googletagmanager.com/gtag/js?id=G-1&cx=c&gtm=4e69t1",
		NormalizedURL: "https://www.googletagmanager.com/gtag/js?cx=c&gtm=4e69t1&id=G-1",
		Party:         model.ThirdParty,
	}

	if got, want := n.Rekey(&req), "https://www.googletagmanager.com/gtag/js?id=G-1"; got != want {
		t.Errorf("Rekey = %q, want %q", got, want)
	}
}

func TestRekeyKeepsTheStoredKeyWhereTheRawURLCannotRebuildIt(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{})

	tests := []struct {
		name string
		req  model.Request
	}{
		// A data: URL is stored truncated; its key came from the full URL.
		{"data URL", model.Request{URL: "data:image/png;base64,AAA…", NormalizedURL: "data:image/png;base64,…"}},
		{"no raw URL", model.Request{NormalizedURL: "https://example.com/a.js"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := n.Rekey(&tc.req); got != tc.req.NormalizedURL {
				t.Errorf("Rekey = %q, want the stored %q", got, tc.req.NormalizedURL)
			}
		})
	}
}
