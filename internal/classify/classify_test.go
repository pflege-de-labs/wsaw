package classify_test

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func TestRegistrableDomain(t *testing.T) {
	t.Parallel()

	tests := []struct{ host, want string }{
		{"example.com", "example.com"},
		{"www.example.com", "example.com"},
		{"a.b.c.example.com", "example.com"},
		{"EXAMPLE.COM", "example.com"},
		{"example.com.", "example.com"},
		// Multi-level public suffixes must not collapse to the wrong owner.
		{"example.co.uk", "example.co.uk"},
		{"www.example.co.uk", "example.co.uk"},
		{"shop.example.com.au", "example.com.au"},
		// github.io is a public suffix: two projects are different owners.
		{"alice.github.io", "alice.github.io"},
		{"bob.github.io", "bob.github.io"},
		// Hosts with no registrable domain keep their identity.
		{"localhost", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"192.168.1.10", "192.168.1.10"},
		{"[::1]", "[::1]"},
		{"", ""},
	}

	for _, tc := range tests {
		if got := classify.RegistrableDomain(tc.host); got != tc.want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func mustNew(t *testing.T, target string, additional ...string) *classify.Classifier {
	t.Helper()

	c, err := classify.New(target, additional)
	if err != nil {
		t.Fatalf("New(%q): %v", target, err)
	}

	return c
}

func TestClassifySubdomainsAreFirstParty(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "https://www.example.com/start")

	firstParty := []string{
		"https://www.example.com/a.js",
		"https://example.com/b.css",
		"https://static.example.com/c.png",
		"https://deep.nested.example.com/d.woff2",
		"http://example.com/insecure.js",
	}

	for _, u := range firstParty {
		if got := c.Classify(u); got.Party != model.FirstParty {
			t.Errorf("Classify(%q).Party = %q, want first", u, got.Party)
		}
	}
}

// TestClassifyRejectsLookalikeDomains is the case naive string matching gets
// wrong, and getting it wrong would hide a real third party.
func TestClassifyRejectsLookalikeDomains(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "https://example.com/")

	lookalikes := []string{
		"https://evil-example.com/track.js",
		"https://example.com.evil.test/track.js",
		"https://notexample.com/track.js",
		"https://example.org/track.js",
		"https://example.co.uk/track.js",
	}

	for _, u := range lookalikes {
		got := c.Classify(u)
		if got.Party != model.ThirdParty {
			t.Errorf("Classify(%q).Party = %q, want third", u, got.Party)
		}
	}
}

func TestClassifyAdditionalFirstPartyDomains(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "https://example.com/",
		"examplecdn.net",
		"*.assets.example.io",
		"https://media.example.de/path",
	)

	tests := []struct {
		url   string
		party model.Party
	}{
		{"https://cdn.examplecdn.net/a.js", model.FirstParty},
		{"https://x.assets.example.io/b.js", model.FirstParty},
		{"https://media.example.de/c.js", model.FirstParty},
		{"https://other.example.de/d.js", model.FirstParty}, // same registrable domain
		{"https://tracker.test/e.js", model.ThirdParty},
	}

	for _, tc := range tests {
		if got := c.Classify(tc.url); got.Party != tc.party {
			t.Errorf("Classify(%q).Party = %q, want %q", tc.url, got.Party, tc.party)
		}
	}
}

func TestClassifyReturnsHostAndDomain(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "https://example.com/")

	got := c.Classify("https://Ads.Tracker.CO.UK:8443/px?id=1")
	if got.Host != "ads.tracker.co.uk" {
		t.Errorf("Host = %q, want %q (lowercased, no port)", got.Host, "ads.tracker.co.uk")
	}

	if got.Domain != "tracker.co.uk" {
		t.Errorf("Domain = %q, want %q", got.Domain, "tracker.co.uk")
	}

	if got.Party != model.ThirdParty {
		t.Errorf("Party = %q, want third", got.Party)
	}
}

func TestClassifyHostlessURLsDidNotLeaveTheBrowser(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "https://example.com/")

	for _, u := range []string{"data:image/png;base64,AA", "about:blank", "blob:x"} {
		got := c.Classify(u)
		if got.Party != model.FirstParty {
			t.Errorf("Classify(%q).Party = %q, want first (nothing left the browser)", u, got.Party)
		}

		if got.Host != "" {
			t.Errorf("Classify(%q).Host = %q, want empty", u, got.Host)
		}
	}
}

func TestClassifyCookieDomain(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "https://example.com/")

	tests := []struct {
		domain string
		party  model.Party
	}{
		{".example.com", model.FirstParty},
		{"example.com", model.FirstParty},
		{"www.example.com", model.FirstParty},
		{".doubleclick.test", model.ThirdParty},
		{"", model.FirstParty},
	}

	for _, tc := range tests {
		if got := c.ClassifyCookieDomain(tc.domain); got != tc.party {
			t.Errorf("ClassifyCookieDomain(%q) = %q, want %q", tc.domain, got, tc.party)
		}
	}
}

func TestNewRejectsURLWithoutHost(t *testing.T) {
	t.Parallel()

	if _, err := classify.New("not-a-url", nil); err == nil {
		t.Error("New accepted a URL with no host")
	}
}

func TestIPLiteralTargetClassifiesItself(t *testing.T) {
	t.Parallel()

	c := mustNew(t, "http://127.0.0.1:8080/")

	if got := c.Classify("http://127.0.0.1:8080/a.js"); got.Party != model.FirstParty {
		t.Errorf("same IP = %q, want first", got.Party)
	}

	if got := c.Classify("http://10.0.0.5/a.js"); got.Party != model.ThirdParty {
		t.Errorf("different IP = %q, want third", got.Party)
	}
}
