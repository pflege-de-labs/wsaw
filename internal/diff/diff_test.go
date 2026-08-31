package diff_test

import (
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/diff"
	"github.com/martint17r/wsaw/internal/model"
)

func result(mode model.ConsentMode, reqs ...model.Request) *model.Result {
	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		Target:        "site",
		URL:           "https://example.com/",
		ConsentMode:   mode,
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied},
		Requests:      reqs,
	}
}

func req(url, domain string, party model.Party, opts ...func(*model.Request)) model.Request {
	r := model.Request{
		URL:           url,
		NormalizedURL: url,
		Domain:        domain,
		Host:          domain,
		Party:         party,
		ResourceType:  "script",
		Phase:         model.PhasePost,
		Status:        200,
	}

	for _, o := range opts {
		o(&r)
	}

	return r
}

func pre(r *model.Request)                 { r.Phase = model.PhasePre }
func typ(t string) func(*model.Request)    { return func(r *model.Request) { r.ResourceType = t } }
func digest(d string) func(*model.Request) { return func(r *model.Request) { r.BodySHA256 = d } }
func status(s int) func(*model.Request)    { return func(r *model.Request) { r.Status = s } }

func find(t *testing.T, rep *diff.Report, typ diff.ChangeType, subject string) diff.Change {
	t.Helper()

	for _, c := range rep.Changes {
		if c.Type == typ && c.Subject == subject {
			return c
		}
	}

	t.Fatalf("no %s change for %q; got %+v", typ, subject, rep.Changes)

	return diff.Change{}
}

func TestNoChangesWhenNothingChanged(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentReject, req("https://example.com/a.js", "example.com", model.FirstParty))

	rep := diff.Compare(a, b, diff.Options{})

	if !rep.Comparable {
		t.Fatalf("not comparable: %s", rep.Reason)
	}

	if len(rep.Changes) != 0 {
		t.Errorf("got %d changes on an unchanged site, want 0: %+v", len(rep.Changes), rep.Changes)
	}
}

// TestNewThirdPartyInRejectModeIsCritical is the product's headline finding:
// a tracker that fires despite the user rejecting consent.
func TestNewThirdPartyInRejectModeIsCritical(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentReject,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://tracker.test/px.js", "tracker.test", model.ThirdParty),
	)

	rep := diff.Compare(a, b, diff.Options{})

	c := find(t, rep, diff.HostAdded, "tracker.test")
	if c.Severity != diff.SeverityCritical {
		t.Errorf("severity = %q, want critical", c.Severity)
	}

	if rep.MaxSeverity() != diff.SeverityCritical {
		t.Errorf("MaxSeverity = %q", rep.MaxSeverity())
	}
}

func TestNewThirdPartyPreConsentIsHigh(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentAccept,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://tracker.test/px.js", "tracker.test", model.ThirdParty, pre),
	)

	c := find(t, diff.Compare(a, b, diff.Options{}), diff.HostAdded, "tracker.test")
	if c.Severity != diff.SeverityHigh {
		t.Errorf("severity = %q, want high", c.Severity)
	}

	if c.Phase != model.PhasePre {
		t.Errorf("phase = %q, want pre-interaction", c.Phase)
	}
}

func TestNewFirstPartyImageIsLowNoise(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentAccept,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://example.com/hero.png", "example.com", model.FirstParty, typ("image")),
	)

	rep := diff.Compare(a, b, diff.Options{})

	c := find(t, rep, diff.AssetAdded, "https://example.com/hero.png")
	if c.Severity != diff.SeverityInfo {
		t.Errorf("severity = %q, want info: a new first-party image is not a finding", c.Severity)
	}
}

// TestThirdPartyScriptContentChange is the supply-chain check: same URL,
// different content.
func TestThirdPartyScriptContentChange(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://cdn.test/w.js", "cdn.test", model.ThirdParty, digest("aaa")))
	b := result(model.ConsentAccept, req("https://cdn.test/w.js", "cdn.test", model.ThirdParty, digest("bbb")))

	rep := diff.Compare(a, b, diff.Options{})

	c := find(t, rep, diff.ScriptChanged, "https://cdn.test/w.js")
	if c.Severity != diff.SeverityHigh {
		t.Errorf("severity = %q, want high", c.Severity)
	}

	if c.Before != "aaa" || c.After != "bbb" {
		t.Errorf("before/after = %q/%q", c.Before, c.After)
	}
}

// TestMissingDigestIsNotReportedAsUnchanged guards against a false negative:
// if a digest could not be taken, wsaw must not imply the script is the same.
func TestMissingDigestIsNotReportedAsUnchanged(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://cdn.test/w.js", "cdn.test", model.ThirdParty, digest("aaa")))
	b := result(model.ConsentAccept, req("https://cdn.test/w.js", "cdn.test", model.ThirdParty))

	rep := diff.Compare(a, b, diff.Options{})

	for _, c := range rep.Changes {
		if c.Type == diff.ScriptChanged {
			t.Errorf("reported a script change from a missing digest: %+v", c)
		}
	}
}

func TestConsentModesAreNeverCompared(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://tracker.test/a.js", "tracker.test", model.ThirdParty))
	b := result(model.ConsentReject)

	rep := diff.Compare(a, b, diff.Options{})

	if rep.Comparable {
		t.Fatal("compared results from different consent modes")
	}

	if rep.Reason == "" {
		t.Error("no reason given for an incomparable pair")
	}
}

// TestDegradedScanIsReportedNotSilentlyEmpty is Tenet 5: a broken scan must
// never read as a clean site.
func TestDegradedScanIsReportedNotSilentlyEmpty(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject, req("https://tracker.test/a.js", "tracker.test", model.ThirdParty))

	b := result(model.ConsentReject)
	b.Termination = model.TermError
	b.Error = "navigate: net::ERR_CONNECTION_REFUSED"

	rep := diff.Compare(a, b, diff.Options{})

	if rep.Comparable {
		t.Error("a failed scan was treated as comparable")
	}

	if len(rep.Changes) == 0 {
		t.Fatal("a failed scan produced no changes at all; it must report its own failure")
	}

	if rep.Changes[0].Type != diff.ScanDegraded {
		t.Errorf("change type = %q, want scan-degraded", rep.Changes[0].Type)
	}
}

func TestTruncatedScanIsFlagged(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty))

	b := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty))
	b.Termination = model.TermRequestCap

	rep := diff.Compare(a, b, diff.Options{})

	find(t, rep, diff.ScanDegraded, string(model.TermRequestCap))
}

func TestAllowListSuppressesExpectedThirdParties(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentAccept,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://cdn.jsdelivr.test/lib.js", "jsdelivr.test", model.ThirdParty),
	)

	opts := diff.Options{Allow: diff.NewHostList([]string{"jsdelivr.test"})}

	rep := diff.Compare(a, b, opts)

	for _, c := range rep.Changes {
		if c.Domain == "jsdelivr.test" {
			t.Errorf("allow-listed domain still reported: %+v", c)
		}
	}

	if rep.Suppressed == 0 {
		t.Error("Suppressed = 0; a quiet report must be distinguishable from an over-configured one")
	}
}

func TestAllowListDoesNotHideConsentRegressions(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject)

	b := result(model.ConsentReject)
	b.Consent = model.Consent{Outcome: model.OutcomeFailed, Reason: "banner not found"}

	// A wildcard allow list must not silence wsaw's own inability to observe.
	opts := diff.Options{Allow: diff.NewHostList([]string{"*.", "example.com"})}

	rep := diff.Compare(a, b, opts)

	find(t, rep, diff.ConsentChanged, string(model.OutcomeFailed))
}

// TestDenyListFiresRegardlessOfBaseline: a known tracker is a finding every
// time, not only the first time.
func TestDenyListFiresRegardlessOfBaseline(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject, req("https://ads.test/t.js", "ads.test", model.ThirdParty))
	b := result(model.ConsentReject, req("https://ads.test/t.js", "ads.test", model.ThirdParty))

	opts := diff.Options{Deny: diff.NewHostList([]string{"ads.test"})}

	rep := diff.Compare(a, b, opts)

	c := find(t, rep, diff.DeniedHost, "ads.test")
	if c.Severity != diff.SeverityCritical {
		t.Errorf("severity = %q, want critical", c.Severity)
	}
}

func TestHostListMatching(t *testing.T) {
	t.Parallel()

	l := diff.NewHostList([]string{"example.com", "*.cdn.test"})

	tests := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"www.example.com", true},
		{"a.cdn.test", true},
		{"cdn.test", true},
		{"evil-example.com", false},
		{"example.org", false},
		{"", false},
	}

	for _, tc := range tests {
		if got := l.Matches(tc.host); got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestStatusBecomingErrorOutranksOtherStatusChanges(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty, status(200)))
	b := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty, status(503)))

	c := find(t, diff.Compare(a, b, diff.Options{}), diff.StatusChanged, "https://example.com/a.js")
	if c.Severity != diff.SeverityMedium {
		t.Errorf("severity = %q, want medium", c.Severity)
	}
}

func TestChangeOrderIsDeterministicAndWorstFirst(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentReject,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://example.com/b.png", "example.com", model.FirstParty, typ("image")),
		req("https://tracker.test/px.js", "tracker.test", model.ThirdParty),
		req("https://other.test/y.js", "other.test", model.ThirdParty),
	)

	first := diff.Compare(a, b, diff.Options{})
	second := diff.Compare(a, b, diff.Options{})

	if len(first.Changes) != len(second.Changes) {
		t.Fatal("comparison is not deterministic in length")
	}

	for i := range first.Changes {
		if first.Changes[i] != second.Changes[i] {
			t.Fatalf("comparison is not deterministic at %d: %+v vs %+v", i, first.Changes[i], second.Changes[i])
		}
	}

	if first.Changes[0].Severity != diff.SeverityCritical {
		t.Errorf("first change severity = %q, want the worst first", first.Changes[0].Severity)
	}
}

func TestSeverityThreshold(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentAccept,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://example.com/hero.png", "example.com", model.FirstParty, typ("image")),
	)

	rep := diff.Compare(a, b, diff.Options{})

	if rep.HasFindingsAtLeast(diff.SeverityHigh) {
		t.Error("a new first-party image counted as a high finding")
	}

	if !rep.HasFindingsAtLeast(diff.SeverityInfo) {
		t.Error("no findings at all at info level")
	}
}

func TestParseSeverity(t *testing.T) {
	t.Parallel()

	if _, err := diff.ParseSeverity("high"); err != nil {
		t.Errorf("ParseSeverity(high): %v", err)
	}

	if _, err := diff.ParseSeverity("catastrophic"); err == nil {
		t.Error("ParseSeverity accepted an unknown severity")
	}
}

func TestCustomSeverityRulesOverrideDefaults(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentReject, req("https://example.com/a.js", "example.com", model.FirstParty))
	b := result(model.ConsentReject,
		req("https://example.com/a.js", "example.com", model.FirstParty),
		req("https://tracker.test/px.js", "tracker.test", model.ThirdParty),
	)

	opts := diff.Options{Severity: diff.SeverityRules{ThirdPartyHostRejectMode: diff.SeverityLow}}

	c := find(t, diff.Compare(a, b, opts), diff.HostAdded, "tracker.test")
	if c.Severity != diff.SeverityLow {
		t.Errorf("severity = %q, want the configured low", c.Severity)
	}
}

// TestPartialSeverityConfigKeepsOtherDefaults: configuring one rule must not
// blank out the rest, which would silently downgrade every other finding.
func TestPartialSeverityConfigKeepsOtherDefaults(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, req("https://cdn.test/w.js", "cdn.test", model.ThirdParty, digest("aaa")))
	b := result(model.ConsentAccept, req("https://cdn.test/w.js", "cdn.test", model.ThirdParty, digest("bbb")))

	opts := diff.Options{Severity: diff.SeverityRules{FirstPartyHostAdded: diff.SeverityCritical}}

	c := find(t, diff.Compare(a, b, opts), diff.ScriptChanged, "https://cdn.test/w.js")
	if c.Severity != diff.SeverityHigh {
		t.Errorf("severity = %q, want the default high", c.Severity)
	}
}

func TestFlapSuppression(t *testing.T) {
	t.Parallel()

	f := diff.NewFlapSuppressor(time.Hour)

	change := diff.Change{Type: diff.HostAdded, Subject: "rotating.test", Target: "site", ConsentMode: model.ConsentReject}

	now := time.Now()

	emit, suppressed := f.Filter(now, []diff.Change{change})
	if len(emit) != 1 || suppressed != 0 {
		t.Fatalf("first occurrence: emit=%d suppressed=%d, want 1 and 0", len(emit), suppressed)
	}

	// The same change again inside the window is noise.
	emit, suppressed = f.Filter(now.Add(time.Minute), []diff.Change{change})
	if len(emit) != 0 || suppressed != 1 {
		t.Errorf("repeat inside window: emit=%d suppressed=%d, want 0 and 1", len(emit), suppressed)
	}

	// After the window it is news again.
	emit, _ = f.Filter(now.Add(2*time.Hour), []diff.Change{change})
	if len(emit) != 1 {
		t.Errorf("after window: emit=%d, want 1", len(emit))
	}
}

// TestFlapSuppressionCollapsesInverses covers the actual flapping case: a
// host that appears and disappears on alternate scans.
func TestFlapSuppressionCollapsesInverses(t *testing.T) {
	t.Parallel()

	f := diff.NewFlapSuppressor(time.Hour)
	now := time.Now()

	added := diff.Change{Type: diff.HostAdded, Subject: "rotating.test", Target: "site"}
	removed := diff.Change{Type: diff.HostRemoved, Subject: "rotating.test", Target: "site"}

	f.Filter(now, []diff.Change{added})

	emit, suppressed := f.Filter(now.Add(time.Minute), []diff.Change{removed})
	if len(emit) != 0 || suppressed != 1 {
		t.Errorf("the inverse change was not collapsed: emit=%d suppressed=%d", len(emit), suppressed)
	}
}

func TestFlapSuppressorPrunesSoItDoesNotGrowForever(t *testing.T) {
	t.Parallel()

	f := diff.NewFlapSuppressor(time.Hour)
	now := time.Now()

	f.Filter(now, []diff.Change{{Type: diff.HostAdded, Subject: "a.test"}})

	if f.Len() != 1 {
		t.Fatalf("Len = %d, want 1", f.Len())
	}

	f.Prune(now.Add(2 * time.Hour))

	if f.Len() != 0 {
		t.Errorf("Len = %d after pruning, want 0", f.Len())
	}
}

func TestDisabledFlapSuppressorPassesEverything(t *testing.T) {
	t.Parallel()

	f := diff.NewFlapSuppressor(0)
	change := diff.Change{Type: diff.HostAdded, Subject: "x.test"}

	for range 3 {
		emit, suppressed := f.Filter(time.Now(), []diff.Change{change})
		if len(emit) != 1 || suppressed != 0 {
			t.Fatalf("emit=%d suppressed=%d, want everything through", len(emit), suppressed)
		}
	}
}
