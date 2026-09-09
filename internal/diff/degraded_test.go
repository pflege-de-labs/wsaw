package diff_test

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// failed marks a request as lost to a capture failure: it never opened a
// connection, so it says nothing about the site.
func failed(reason string) func(*model.Request) {
	return func(r *model.Request) {
		r.Failed = true
		r.FailureReason = reason
		r.Status = 0
	}
}

func aborted(r *model.Request) {
	r.Failed = true
	r.FailureReason = "net::ERR_ABORTED"
}

// filler pads a result with successful requests, so a fixture can put a known
// failure ratio in front of the differ.
func filler(n int) []model.Request {
	out := make([]model.Request, 0, n)
	for i := range n {
		out = append(out, req(
			"https://example.com/pad-"+string(rune('a'+i%26))+string(rune('a'+i/26))+".js",
			"example.com", model.FirstParty))
	}

	return out
}

// TestRemovalsAreNotReportedFromADegradedScan is the asymmetry that matters:
// an asset missing from a scan that lost requests to capture failure is
// unprovable, because one failed loader silences everything it would have
// requested. An asset that appeared is still reported — a false positive
// there costs a look, a false negative hides a tracker.
func TestRemovalsAreNotReportedFromADegradedScan(t *testing.T) {
	t.Parallel()

	baseline := result(model.ConsentReject, append(filler(8),
		req("https://cdn.test/widget.js", "cdn.test", model.ThirdParty),
	)...)

	// One of ten requests lost — a tenth, over the default five percent.
	current := result(model.ConsentReject, append(filler(8),
		req("https://loader.test/boot.js", "loader.test", model.ThirdParty,
			failed("net::ERR_INSUFFICIENT_RESOURCES")),
		req("https://tracker.test/px.gif", "tracker.test", model.ThirdParty),
	)...)

	rep := diff.Compare(baseline, current, diff.Options{})

	if !rep.Comparable {
		t.Fatalf("not comparable: %s", rep.Reason)
	}

	for _, c := range rep.Changes {
		if c.Type == diff.AssetRemoved || c.Type == diff.HostRemoved {
			t.Errorf("a degraded scan reported %s for %q; removals are unprovable here", c.Type, c.Subject)
		}
	}

	if rep.Suppressed == 0 {
		t.Error("removals were dropped without being counted as suppressed")
	}

	// The addition survives, and so does the reason the scan is distrusted.
	find(t, rep, diff.HostAdded, "tracker.test")
	change := find(t, rep, diff.ScanDegraded, "net::ERR_INSUFFICIENT_RESOURCES")

	if !strings.Contains(change.Detail, "removals are not reported") {
		t.Errorf("degradation detail does not say removals were withheld: %q", change.Detail)
	}
}

// TestOccasionalFailureDoesNotDegradeAScan keeps the gate from swallowing
// every comparison: one unlucky request in a large page is not a broken
// capture environment.
func TestOccasionalFailureDoesNotDegradeAScan(t *testing.T) {
	t.Parallel()

	baseline := result(model.ConsentReject, append(filler(60),
		req("https://cdn.test/widget.js", "cdn.test", model.ThirdParty),
	)...)

	// One of sixty-one requests lost, well under the default five percent.
	current := result(model.ConsentReject, append(filler(60),
		req("https://loader.test/boot.js", "loader.test", model.ThirdParty,
			failed("net::ERR_INSUFFICIENT_RESOURCES")),
	)...)

	rep := diff.Compare(baseline, current, diff.Options{})

	find(t, rep, diff.HostRemoved, "cdn.test")

	for _, c := range rep.Changes {
		if c.Type == diff.ScanDegraded {
			t.Errorf("one failure in sixty requests degraded the scan: %q", c.Detail)
		}
	}
}

// TestAbortedRequestsDoNotDegradeAScan guards the classification. A beacon
// aborted as the page is torn down is ordinary — it usually carries a status,
// because the server answered — and treating it as a capture defect would
// mark almost every accept-mode scan degraded.
func TestAbortedRequestsDoNotDegradeAScan(t *testing.T) {
	t.Parallel()

	baseline := result(model.ConsentAccept, append(filler(10),
		req("https://cdn.test/widget.js", "cdn.test", model.ThirdParty),
	)...)

	current := result(model.ConsentAccept, append(filler(10),
		req("https://beacon.test/e", "beacon.test", model.ThirdParty, aborted),
		req("https://pixel.test/e", "pixel.test", model.ThirdParty, aborted),
		req("https://stats.test/e", "stats.test", model.ThirdParty, aborted),
	)...)

	rep := diff.Compare(baseline, current, diff.Options{})

	for _, c := range rep.Changes {
		if c.Type == diff.ScanDegraded {
			t.Errorf("aborted beacons degraded the scan: %q", c.Detail)
		}
	}

	// Removals are still reported, because nothing was actually lost.
	find(t, rep, diff.HostRemoved, "cdn.test")
}

// TestADegradedBaselineIsReportedButAdditionsSurvive covers the other side of
// the comparison: when the baseline lost requests, an asset reported as new
// may only have been missed last time. The addition is still raised, and the
// reader is told why it may be an artefact.
func TestADegradedBaselineIsReportedButAdditionsSurvive(t *testing.T) {
	t.Parallel()

	baseline := result(model.ConsentReject, append(filler(18),
		req("https://loader.test/boot.js", "loader.test", model.ThirdParty,
			failed("net::ERR_INSUFFICIENT_RESOURCES")),
		req("https://loader.test/boot2.js", "loader.test", model.ThirdParty,
			failed("net::ERR_INSUFFICIENT_RESOURCES")),
	)...)

	current := result(model.ConsentReject, append(filler(18),
		req("https://loader.test/boot.js", "loader.test", model.ThirdParty),
		req("https://loader.test/boot2.js", "loader.test", model.ThirdParty),
		req("https://tracker.test/px.gif", "tracker.test", model.ThirdParty),
	)...)

	rep := diff.Compare(baseline, current, diff.Options{})

	find(t, rep, diff.HostAdded, "tracker.test")

	change := find(t, rep, diff.ScanDegraded, "net::ERR_INSUFFICIENT_RESOURCES")
	if !strings.Contains(change.Detail, "baseline") {
		t.Errorf("a degraded baseline was not attributed to the baseline: %q", change.Detail)
	}
}

// TestDegradedFailureRatioIsConfigurable lets an operator who would rather
// see the raw comparison switch the gate off.
func TestDegradedFailureRatioIsConfigurable(t *testing.T) {
	t.Parallel()

	baseline := result(model.ConsentReject,
		req("https://cdn.test/widget.js", "cdn.test", model.ThirdParty),
		req("https://example.com/a.js", "example.com", model.FirstParty),
	)

	current := result(model.ConsentReject,
		req("https://loader.test/boot.js", "loader.test", model.ThirdParty,
			failed("net::ERR_INSUFFICIENT_RESOURCES")),
		req("https://example.com/a.js", "example.com", model.FirstParty),
	)

	// Half the requests failed, so the default gate closes.
	if rep := diff.Compare(baseline, current, diff.Options{}); len(rep.Changes) > 0 {
		for _, c := range rep.Changes {
			if c.Type == diff.HostRemoved {
				t.Fatal("the default gate did not suppress a removal at a 50% failure ratio")
			}
		}
	}

	// A ratio above 1 can never be reached, which disables the check.
	rep := diff.Compare(baseline, current, diff.Options{DegradedFailureRatio: 1.5})

	find(t, rep, diff.HostRemoved, "cdn.test")

	for _, c := range rep.Changes {
		if c.Type == diff.ScanDegraded {
			t.Errorf("the check was disabled but still reported degradation: %q", c.Detail)
		}
	}
}
