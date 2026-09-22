package consent

// Story 2.9: what a page with no CMP product behind its banner is recorded
// as. These are the decisions that happen without a browser — what kind of
// CMP was found, what counts as evidence that a choice was written down, and
// what a banner nobody handled leaves behind for the next rule author.

import (
	"slices"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

func TestRuleKindSeparatesAVendorFromASitesOwnBanner(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rule Rule
		want model.CMPKind
	}{
		{"vendor rule", Rule{Name: "cookiebot", Vendor: "Cookiebot"}, model.CMPKindVendor},
		{"host rule", Rule{Name: "example-banner", Hosts: []string{"example.com"}}, model.CMPKindBespoke},
		{"heuristic", Rule{Name: "generic-label-match", Heuristic: true}, model.CMPKindBespoke},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ruleKind(tc.rule); got != tc.want {
				t.Errorf("ruleKind(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestKindForBannerNeverReportsAVisibleBannerAsNoCMP(t *testing.T) {
	t.Parallel()

	if got := kindForBanner(true); got != model.CMPKindBespoke {
		t.Errorf("a page with a banner was classified %q, want %q", got, model.CMPKindBespoke)
	}

	if got := kindForBanner(false); got != model.CMPKindNone {
		t.Errorf("a page with no banner was classified %q, want %q", got, model.CMPKindNone)
	}
}

func TestDiffStorageReportsOnlyWhatThisInteractionWrote(t *testing.T) {
	t.Parallel()

	// A key the page already had, with an unchanged value, is not evidence
	// that the interaction recorded anything: the same trap __wsawCmpPrior
	// exists to avoid on the CMP's own consent state.
	h := &handler{storageBefore: map[string]int{
		"local:visitor-id":      12,
		"local:cookie-accepted": 4,
		"session:cart":          3,
	}}

	got := h.diffStorage(map[string]int{
		"local:visitor-id":      12,
		"local:cookie-accepted": 5,
		"session:cart":          3,
		"local:consent-shown":   1,
	})

	want := []string{"local:consent-shown", "local:cookie-accepted"}

	if !slices.Equal(got, want) {
		t.Errorf("storage writes = %v, want %v", got, want)
	}
}

func TestDiffStorageWithNothingStoredReportsNothing(t *testing.T) {
	t.Parallel()

	h := &handler{storageBefore: map[string]int{"local:x": 1}}

	if got := h.diffStorage(nil); got != nil {
		t.Errorf("storage writes = %v, want none: an unreadable storage is not a write", got)
	}
}

func TestDiagnosticRecordsWhatTheUnhandledBannerOffered(t *testing.T) {
	t.Parallel()

	h := &handler{banner: bannerSummary{
		Found:    true,
		Element:  "div#cookie-banner",
		Heading:  "Wir verwenden Cookies",
		Text:     "Diese Website nutzt Cookies.",
		Controls: []string{"Ablehnen", "Akzeptieren"},
	}}

	d := h.diagnostic()
	if d == nil {
		t.Fatal("no diagnostic recorded for a banner that was found")
	}

	if d.Element != "div#cookie-banner" {
		t.Errorf("element = %q, want the matched container", d.Element)
	}

	if !slices.Contains(d.Controls, "Ablehnen") {
		t.Errorf("controls = %v, want the labels a rule author would bind to", d.Controls)
	}

	if (&handler{}).diagnostic() != nil {
		t.Error("a diagnostic was recorded for a page with no banner")
	}
}

func TestBannerWaitDefaultsAreBounded(t *testing.T) {
	t.Parallel()

	opts := (&Options{}).withDefaults()

	if opts.BannerWait != DefaultBannerWait {
		t.Errorf("BannerWait = %s, want the default %s", opts.BannerWait, DefaultBannerWait)
	}

	if opts.BannerWait > opts.TotalTimeout {
		t.Errorf("BannerWait %s exceeds the whole interaction budget %s", opts.BannerWait, opts.TotalTimeout)
	}
}
