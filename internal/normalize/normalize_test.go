package normalize_test

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

func mustNew(t *testing.T, r normalize.Rules) *normalize.Normalizer {
	t.Helper()

	n, err := normalize.New(r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return n
}

func TestStructuralNormalizationAlwaysApplies(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{})

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"host lowercased", "https://Example.COM/a.js", "https://example.com/a.js"},
		{"default https port dropped", "https://example.com:443/a.js", "https://example.com/a.js"},
		{"default http port dropped", "http://example.com:80/a.js", "http://example.com/a.js"},
		{"non-default port kept", "https://example.com:8443/a.js", "https://example.com:8443/a.js"},
		{"fragment dropped", "https://example.com/a.js#frag", "https://example.com/a.js"},
		{"wss default port dropped", "wss://example.com:443/socket", "wss://example.com/socket"},
		{"query order made stable", "https://example.com/a?b=2&a=1", "https://example.com/a?a=1&b=2"},
		{"path case preserved", "https://example.com/App/Main.js", "https://example.com/App/Main.js"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := n.Key(tc.in); got != tc.want {
				t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDropQueryParams(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{DropQueryParams: []string{"cb", "_"}})

	tests := []struct{ in, want string }{
		{"https://example.com/a.js?cb=12345", "https://example.com/a.js"},
		{"https://example.com/a.js?cb=1&keep=2", "https://example.com/a.js?keep=2"},
		{"https://example.com/a.js?_=99&keep=2", "https://example.com/a.js?keep=2"},
		// Parameter names are matched case-insensitively.
		{"https://example.com/a.js?CB=1&keep=2", "https://example.com/a.js?keep=2"},
	}

	for _, tc := range tests {
		if got := n.Key(tc.in); got != tc.want {
			t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestKeepQueryParamsTakesPrecedence(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		KeepQueryParams: []string{"id"},
		DropQueryParams: []string{"id"}, // ignored while KeepQueryParams is set
	})

	got := n.Key("https://example.com/a?id=7&cb=1&other=2")
	if want := "https://example.com/a?id=7"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

func TestDropAllQuery(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{DropAllQuery: true})

	got := n.Key("https://example.com/a.js?sig=abc&exp=123")
	if want := "https://example.com/a.js"; got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

func TestPathReplacements(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		PathReplacements: []normalize.Replacement{
			{Pattern: `\.[0-9a-f]{8,}\.(js|css)$`, With: ".{hash}.$1"},
			{Pattern: `/v[0-9]+/`, With: "/v{n}/"},
		},
	})

	tests := []struct{ in, want string }{
		{
			in:   "https://example.com/static/main.4f3a9c8b1d.js",
			want: "https://example.com/static/main.{hash}.js",
		},
		{
			in:   "https://example.com/v42/app.css",
			want: "https://example.com/v{n}/app.css",
		},
		{
			in:   "https://example.com/static/main.js",
			want: "https://example.com/static/main.js",
		},
	}

	for _, tc := range tests {
		if got := n.Key(tc.in); got != tc.want {
			t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPathReplacementCollapsesBuildChurn is the property that actually
// matters: two deploys of the same asset must produce one key, not two.
func TestPathReplacementCollapsesBuildChurn(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		PathReplacements: []normalize.Replacement{
			{Pattern: `\.[0-9a-f]{8,}\.js$`, With: ".{hash}.js"},
		},
	})

	before := n.Key("https://example.com/app.aaaaaaaa11.js")
	after := n.Key("https://example.com/app.bbbbbbbb22.js")

	if before != after {
		t.Errorf("build hashes not collapsed: %q != %q", before, after)
	}
}

func TestDropTrailingSlash(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{DropTrailingSlash: true})

	tests := []struct{ in, want string }{
		{"https://example.com/a/", "https://example.com/a"},
		{"https://example.com/", "https://example.com/"},
		{"https://example.com/a/b/", "https://example.com/a/b"},
	}

	for _, tc := range tests {
		if got := n.Key(tc.in); got != tc.want {
			t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpaqueSchemesLoseTheirPayload(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{})

	tests := []struct{ in, want string }{
		{"data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///", "data:image/gif;base64,…"},
		{"blob:https://example.com/2b6c1f3e-0d1a-4c1e-9c5b-8f0a1b2c3d4e", "blob:https://example.com/{uuid}"},
		{"about:blank", "about:blank"},
	}

	for _, tc := range tests {
		if got := n.Key(tc.in); got != tc.want {
			t.Errorf("Key(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOpaqueDataURLsCollapse checks that two different inline images do not
// look like two different assets forever.
func TestOpaqueDataURLsCollapse(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{})

	a := n.Key("data:image/png;base64,AAAA")
	b := n.Key("data:image/png;base64,BBBB")

	if a != b {
		t.Errorf("data URL payloads not collapsed: %q != %q", a, b)
	}
}

func TestKeyIsIdempotent(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{
		DropQueryParams:   normalize.DefaultDropQueryParams,
		DropTrailingSlash: true,
		PathReplacements: []normalize.Replacement{
			{Pattern: `\.[0-9a-f]{8,}\.js$`, With: ".{hash}.js"},
		},
	})

	inputs := []string{
		"https://Example.com:443/a/?cb=1&keep=2#x",
		"https://example.com/app.deadbeef99.js",
		"data:text/css,body{}",
		"not a url at all",
	}

	for _, in := range inputs {
		once := n.Key(in)
		if twice := n.Key(once); twice != once {
			t.Errorf("Key not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestUnparseableURLIsPreservedNotDiscarded(t *testing.T) {
	t.Parallel()

	n := mustNew(t, normalize.Rules{})

	const in = "http://[::1]:namedport/x"
	if got := n.Key(in); got != in {
		t.Errorf("Key(%q) = %q, want the input preserved verbatim", in, got)
	}
}

func TestEmptyInput(t *testing.T) {
	t.Parallel()

	if got := mustNew(t, normalize.Rules{}).Key(""); got != "" {
		t.Errorf("Key(\"\") = %q, want empty", got)
	}
}

func TestInvalidPathPatternFailsAtCompileTime(t *testing.T) {
	t.Parallel()

	_, err := normalize.New(normalize.Rules{
		PathReplacements: []normalize.Replacement{{Pattern: "([unclosed", With: "x"}},
	})
	if err == nil {
		t.Fatal("New accepted an invalid regular expression")
	}
}
