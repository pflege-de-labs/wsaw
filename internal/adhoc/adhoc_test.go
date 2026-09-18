package adhoc_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/adhoc"
)

// Story 5.27: a URL typed into the web interface is chosen by whoever is
// looking at the page, not by the operator who wrote the configuration file.
// These cover what wsaw will and will not be talked into fetching, and that
// the name a result lands under is the one `wsaw scan --url` uses.

// fixedResolver answers from a table, so no test needs a network (Tenet 13).
type fixedResolver map[string][]string

func (f fixedResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}

	out := make([]net.IPAddr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}

	return out, nil
}

func publicResolver() fixedResolver {
	return fixedResolver{
		"example.com":   {"93.184.216.34"},
		"internal.test": {"10.0.0.5"},
		"split.test":    {"93.184.216.34", "127.0.0.1"},
		"metadata.test": {"169.254.169.254"},
		"v6.test":       {"2606:2800:220:1:248:1893:25c8:1946"},
		"ula.test":      {"fd00::1"},
	}
}

func TestAcceptTakesAnOrdinaryPublicURL(t *testing.T) {
	t.Parallel()

	name, err := adhoc.Accept(t.Context(), "https://example.com/pricing",
		adhoc.Policy{Resolver: publicResolver()})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if name != "example.com-pricing" {
		t.Errorf("name = %q, want example.com-pricing", name)
	}
}

func TestAcceptRefusesWhatWsawWillNotFetch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"empty", "", "no URL was given"},
		{"no scheme", "example.com", "needs a scheme"},
		{"file", "file:///etc/passwd", "is not supported"},
		{"javascript", "javascript:alert(1)", "is not supported"},
		{"no host", "https:///nowhere", "no host"},
		// Credentials would be stored in every result and logged on every
		// scan; a site that needs them needs a configured target.
		{"credentials", "https://user:pw@example.com/", "contains credentials"},
		{"too long", "https://example.com/" + strings.Repeat("a", 4096), "the limit is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := adhoc.Accept(t.Context(), tc.url, adhoc.Policy{Resolver: publicResolver()})
			if err == nil {
				t.Fatalf("Accept(%q) was accepted", tc.url)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Accept(%q) said %q, want it to mention %q", tc.url, err, tc.want)
			}
		})
	}
}

// The whole point of the host check: wsaw must not be usable as a way to
// reach whatever network it happens to run in.
func TestAcceptRefusesAddressesOffThePublicInternet(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, url string }{
		{"loopback literal", "http://127.0.0.1:8080/"},
		{"loopback name", "http://localhost:8080/"},
		{"unspecified", "http://0.0.0.0/"},
		{"private literal", "http://192.168.1.1/"},
		{"cgnat", "http://100.64.3.4/"},
		{"this network", "http://0.1.2.3/"},
		{"benchmark range", "http://198.18.0.1/"},
		{"ipv6 loopback", "http://[::1]/"},
		{"unique local", "http://[fd00::1]/"},
		{"resolves privately", "https://internal.test/"},
		{"cloud metadata", "https://metadata.test/latest/meta-data/"},
		// One public answer and one internal one: the name would otherwise
		// decide for itself which address the browser connects to.
		{"one private answer of several", "https://split.test/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := adhoc.Accept(t.Context(), tc.url, adhoc.Policy{Resolver: publicResolver()})
			if err == nil {
				t.Fatalf("Accept(%q) was accepted", tc.url)
			}

			if !strings.Contains(err.Error(), "not a public internet address") {
				t.Errorf("Accept(%q) refused with %q, which does not say why", tc.url, err)
			}
		})
	}
}

func TestAcceptTakesPublicAddressesInEitherFamily(t *testing.T) {
	t.Parallel()

	for _, u := range []string{
		"https://example.com/",
		"https://v6.test/",
		"http://93.184.216.34/",
		"http://[2606:2800:220:1:248:1893:25c8:1946]/",
	} {
		if _, err := adhoc.Accept(t.Context(), u, adhoc.Policy{Resolver: publicResolver()}); err != nil {
			t.Errorf("Accept(%q) = %v, want it accepted", u, err)
		}
	}
}

// A deployment that says so may scan its own network; nothing else changes.
func TestAllowPrivateHostsOpensTheDoorDeliberately(t *testing.T) {
	t.Parallel()

	p := adhoc.Policy{AllowPrivateHosts: true, Resolver: publicResolver()}

	if _, err := adhoc.Accept(t.Context(), "http://127.0.0.1:8080/", p); err != nil {
		t.Errorf("with allowPrivateHosts, loopback was still refused: %v", err)
	}

	// The scheme and credential rules are not part of that door.
	if _, err := adhoc.Accept(t.Context(), "file:///etc/passwd", p); err == nil {
		t.Error("allowPrivateHosts also accepted a file: URL")
	}
}

func TestAcceptRefusesAHostThatDoesNotResolve(t *testing.T) {
	t.Parallel()

	_, err := adhoc.Accept(t.Context(), "https://nowhere.test/", adhoc.Policy{Resolver: publicResolver()})
	if err == nil || !strings.Contains(err.Error(), "could not be resolved") {
		t.Errorf("err = %v, want it to say the host could not be resolved", err)
	}
}

// The name is the identity a result is stored under, so it has to be stable
// and has to match what the CLI derives for the same address.
func TestName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"https://example.com/", "example.com"},
		{"http://example.com/", "example.com"},
		{"https://example.com", "example.com"},
		{"  https://example.com/  ", "example.com"},
		{"https://example.com/a/b?c=d", "example.com-a-b-c-d"},
		{"https://sub.example.com:8443/", "sub.example.com-8443"},
		{"https://", ""},
		{"", ""},
	} {
		if got := adhoc.Name(tc.in); got != tc.want {
			t.Errorf("Name(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNameIsBoundedAndStable(t *testing.T) {
	t.Parallel()

	long := adhoc.Name("https://example.com/" + strings.Repeat("path/", 100))

	if len(long) > 80 {
		t.Errorf("name is %d characters long: %q", len(long), long)
	}

	if again := adhoc.Name("https://example.com/" + strings.Repeat("path/", 100)); again != long {
		t.Errorf("the same URL produced two names: %q and %q", long, again)
	}
}
