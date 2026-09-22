package capture

import (
	"testing"
)

func TestBeaconsMatch(t *testing.T) {
	t.Parallel()

	rules := []Beacon{
		{Host: "trc-events.taboola.com"},
		{Host: "clarity.ms", URLPattern: `/collect(\?|$)`},
		{URLPattern: `^https?://mc\.yandex\.(ru|com)/watch/`},
		{Host: "*.example-telemetry.test"},
	}

	beacons, err := CompileBeacons(rules)
	if err != nil {
		t.Fatalf("CompileBeacons: %v", err)
	}

	cases := []struct {
		name string
		url  string
		host string
		want bool
	}{
		{
			name: "the heartbeat this story exists for",
			url:  "https://trc-events.taboola.com/1419468/log/3/unip?en=pre_d_eng_tb&tos=30",
			host: "trc-events.taboola.com",
			want: true,
		},
		{
			name: "a host rule covers a subdomain",
			url:  "https://sub.trc-events.taboola.com/log",
			host: "sub.trc-events.taboola.com",
			want: true,
		},
		{
			name: "a sibling host of the same vendor is not a beacon",
			url:  "https://trc.taboola.com/1419468/trc/3/json",
			host: "trc.taboola.com",
			want: false,
		},
		{
			name: "host and pattern together match the ingest path",
			url:  "https://e.clarity.ms/collect",
			host: "e.clarity.ms",
			want: true,
		},
		{
			name: "the same vendor's script is still waited for",
			url:  "https://scripts.clarity.ms/0.8.70/clarity.js",
			host: "scripts.clarity.ms",
			want: false,
		},
		{
			name: "a pattern-only rule needs no host",
			url:  "https://mc.yandex.ru/watch/12345?page-url=x",
			host: "mc.yandex.ru",
			want: true,
		},
		{
			name: "the same host's tag is not a beacon",
			url:  "https://mc.yandex.ru/metrika/tag.js",
			host: "mc.yandex.ru",
			want: false,
		},
		{
			name: "a wildcard matches a subdomain",
			url:  "https://ping.example-telemetry.test/x",
			host: "ping.example-telemetry.test",
			want: true,
		},
		{
			name: "an unrelated host matches nothing",
			url:  "https://example.com/app.js",
			host: "example.com",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := beacons.Matches(tc.url, tc.host); got != tc.want {
				t.Errorf("Matches(%q, %q) = %v, want %v", tc.url, tc.host, got, tc.want)
			}
		})
	}
}

func TestEmptyBeaconsMatchNothing(t *testing.T) {
	t.Parallel()

	var beacons Beacons

	if !beacons.Empty() {
		t.Error("the zero value must be empty")
	}

	if beacons.Matches("https://trc-events.taboola.com/log", "trc-events.taboola.com") {
		t.Error("an empty rule set must match nothing")
	}
}

// TestCompileBeaconsRejectsUnusableRules covers the two mistakes that would
// otherwise be discovered mid-scan: a rule that matches everything, and a
// pattern that never compiles.
func TestCompileBeaconsRejectsUnusableRules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		rules []Beacon
	}{
		{name: "neither host nor pattern", rules: []Beacon{{}}},
		{name: "blank host and pattern", rules: []Beacon{{Host: "  ", URLPattern: " "}}},
		{name: "invalid pattern", rules: []Beacon{{URLPattern: "collect("}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := CompileBeacons(tc.rules); err == nil {
				t.Fatal("CompileBeacons returned no error for an unusable rule")
			}
		})
	}
}

// TestDefaultBeaconsAreUsable guards the shipped list: every rule compiles,
// the beacons that were observed holding real scans open are matched, and no
// rule swallows a script — excluding a script from idle accounting would end
// a scan before the assets that script loads were ever requested.
func TestDefaultBeaconsAreUsable(t *testing.T) {
	t.Parallel()

	beacons, err := CompileBeacons(DefaultBeacons)
	if err != nil {
		t.Fatalf("the shipped beacon list does not compile: %v", err)
	}

	matched := []struct{ url, host string }{
		{"https://trc-events.taboola.com/1419468/log/3/unip?en=pre_d_eng_tb&tos=40", "trc-events.taboola.com"},
		{"https://tr.outbrain.com/unifiedPixel?au=false", "tr.outbrain.com"},
		{"https://e.clarity.ms/collect", "e.clarity.ms"},
		{"https://k.clarity.ms/collect", "k.clarity.ms"},
		{"https://www.google-analytics.com/g/collect?v=2&tid=G-X", "www.google-analytics.com"},
		{"https://region1.google-analytics.com/g/collect?v=2", "region1.google-analytics.com"},
		{"https://bam.nr-data.net/events/1/abc", "bam.nr-data.net"},
		{"https://rs.fullstory.com/rec/bundle", "rs.fullstory.com"},
		{"https://example.com/matomo.php?idsite=1&rec=1", "example.com"},
		{"https://example.com/cdn-cgi/rum?", "example.com"},
	}

	for _, m := range matched {
		if !beacons.Matches(m.url, m.host) {
			t.Errorf("Matches(%q) = false, want the shipped list to cover it", m.url)
		}
	}

	notMatched := []struct{ url, host string }{
		{"https://scripts.clarity.ms/0.8.70/clarity.js", "scripts.clarity.ms"},
		{"https://www.clarity.ms/tag/4016272", "www.clarity.ms"},
		{"https://www.google-analytics.com/analytics.js", "www.google-analytics.com"},
		{"https://cdn.segment.com/analytics.js/v1/key/analytics.min.js", "cdn.segment.com"},
		{"https://edge.fullstory.com/s/fs.js", "edge.fullstory.com"},
		{"https://static.hotjar.com/c/hotjar-123.js", "static.hotjar.com"},
		{"https://www.example.com/", "www.example.com"},
	}

	for _, m := range notMatched {
		if beacons.Matches(m.url, m.host) {
			t.Errorf("Matches(%q) = true, want a script or page to be waited for", m.url)
		}
	}
}
