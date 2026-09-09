package model_test

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// TestCaptureFailureSeparatesOurFaultFromTheSites is the distinction the whole
// degradation gate rests on. A request that never opened a connection told us
// nothing about the site; a beacon aborted as the page was torn down is
// ordinary, and usually carries a status because the server answered.
func TestCaptureFailureSeparatesOurFaultFromTheSites(t *testing.T) {
	t.Parallel()

	ours := []string{
		"net::ERR_INSUFFICIENT_RESOURCES",
		"net::ERR_NAME_NOT_RESOLVED",
		"net::ERR_NETWORK_CHANGED",
		"net::ERR_CONNECTION_RESET",
		"net::ERR_TIMED_OUT",
	}

	for _, reason := range ours {
		if !model.IsCaptureFailure(reason) {
			t.Errorf("%s is not treated as a capture failure, but it means we saw nothing", reason)
		}
	}

	theirs := []string{
		"net::ERR_ABORTED",
		"net::ERR_BLOCKED_BY_ORB",
		"net::ERR_BLOCKED_BY_CLIENT",
		"net::ERR_BLOCKED_BY_RESPONSE",
		"",
	}

	for _, reason := range theirs {
		if model.IsCaptureFailure(reason) {
			t.Errorf("%q is treated as a capture failure; it is an observation, not a defect", reason)
		}
	}
}

func TestCaptureFailureNeedsTheRequestToHaveActuallyFailed(t *testing.T) {
	t.Parallel()

	// A reason recorded on a request that did not fail is not a failure.
	stale := model.Request{FailureReason: "net::ERR_INSUFFICIENT_RESOURCES"}
	if stale.CaptureFailure() {
		t.Error("a request that did not fail was counted as a capture failure")
	}

	// A data: URL never left the browser, so it cannot have failed in transit.
	inline := model.Request{
		NonNetwork:    true,
		Failed:        true,
		FailureReason: "net::ERR_INSUFFICIENT_RESOURCES",
	}
	if inline.CaptureFailure() {
		t.Error("a non-network request was counted as a capture failure")
	}
}

// TestNeverCompletedIsAnIncompleteObservationToo covers the other way a
// request tells us nothing. Idle detection stops waiting for a request that
// stalls, because a cross-origin iframe's completion events never arrive —
// but a script that stalls early leaves the page half-built while the scan
// still ends on schedule, and Chrome reports no error at all.
func TestNeverCompletedIsAnIncompleteObservationToo(t *testing.T) {
	t.Parallel()

	stalled := model.Request{URL: "https://example.com/gtm.js"}
	if !stalled.NeverCompleted() || !stalled.Incomplete() {
		t.Error("a request with no status, no end offset and no error is not counted as incomplete")
	}

	// It is not a capture *failure*, though: nothing errored.
	if stalled.CaptureFailure() {
		t.Error("a stalled request was classified as a transport failure")
	}

	finished := model.Request{
		URL:    "https://example.com/a.js",
		Status: 200,
		Timing: model.Timing{EndOffset: time.Millisecond},
	}
	if finished.NeverCompleted() || finished.Incomplete() {
		t.Error("a completed request was counted as incomplete")
	}

	// A 204 carries no body and no bytes, but it did complete.
	empty := model.Request{
		URL:    "https://example.com/beacon",
		Status: 204,
		Timing: model.Timing{EndOffset: time.Millisecond},
	}
	if empty.Incomplete() {
		t.Error("a 204 was counted as incomplete")
	}

	inline := model.Request{URL: "data:text/css,body{}", NonNetwork: true}
	if inline.Incomplete() {
		t.Error("an inline resource was counted as incomplete; it never left the browser")
	}
}

func TestIncompleteRatioIsOverNetworkRequestsOnly(t *testing.T) {
	t.Parallel()

	done := func(url string) model.Request {
		return model.Request{URL: url, Status: 200, Timing: model.Timing{EndOffset: time.Millisecond}}
	}

	res := &model.Result{Requests: []model.Request{
		done("https://example.com/a.js"),
		done("https://example.com/b.js"),
		{URL: "https://example.com/c.js", Failed: true, FailureReason: "net::ERR_INSUFFICIENT_RESOURCES"},
		// Aborted, so not a capture failure. It carries a status, because the
		// server answered before the page was torn down.
		{
			URL: "https://example.com/d.js", Failed: true, FailureReason: "net::ERR_ABORTED",
			Status: 200, Timing: model.Timing{EndOffset: time.Millisecond},
		},
		// Inline, so counted in neither half of the ratio.
		{URL: "data:text/css,body{}", NonNetwork: true},
	}}

	if got, want := res.IncompleteObservations(), 1; got != want {
		t.Errorf("IncompleteObservations() = %d, want %d", got, want)
	}

	if got, want := res.IncompleteRatio(), 0.25; got != want {
		t.Errorf("IncompleteRatio() = %v, want %v (1 of 4 network requests)", got, want)
	}
}

func TestIncompleteRatioOfAnEmptyResultIsZero(t *testing.T) {
	t.Parallel()

	if got := (&model.Result{}).IncompleteRatio(); got != 0 {
		t.Errorf("IncompleteRatio() = %v on a result with no requests, want 0", got)
	}
}

// TestTopIncompleteReasonNamesTheFaultDeterministically matters twice over: a
// degradation report has to say what actually went wrong, and the same result
// has to produce the same report every time (Tenet 6).
func TestTopIncompleteReasonNamesTheFaultDeterministically(t *testing.T) {
	t.Parallel()

	fail := func(reason string) model.Request {
		return model.Request{Failed: true, FailureReason: reason}
	}

	res := &model.Result{Requests: []model.Request{
		fail("net::ERR_NAME_NOT_RESOLVED"),
		fail("net::ERR_INSUFFICIENT_RESOURCES"),
		fail("net::ERR_INSUFFICIENT_RESOURCES"),
		fail("net::ERR_ABORTED"),
	}}

	reason, count := res.TopIncompleteReason()
	if reason != "net::ERR_INSUFFICIENT_RESOURCES" || count != 2 {
		t.Errorf("TopIncompleteReason() = %q, %d; want net::ERR_INSUFFICIENT_RESOURCES, 2", reason, count)
	}

	// A stalled request has no error text of its own, so it reports under a
	// stand-in reason rather than an empty string.
	stalledOnly := &model.Result{Requests: []model.Request{
		{URL: "https://example.com/gtm.js"},
		{URL: "https://example.com/a.js"},
	}}

	if got, n := stalledOnly.TopIncompleteReason(); got != model.ReasonNeverCompleted || n != 2 {
		t.Errorf("TopIncompleteReason() = %q, %d; want %q, 2", got, n, model.ReasonNeverCompleted)
	}

	// A tie breaks on the reason string, so the answer never depends on map
	// iteration order.
	tied := &model.Result{Requests: []model.Request{
		fail("net::ERR_TIMED_OUT"),
		fail("net::ERR_CONNECTION_RESET"),
	}}

	for range 20 {
		if got, _ := tied.TopIncompleteReason(); got != "net::ERR_CONNECTION_RESET" {
			t.Fatalf("TopIncompleteReason() = %q on a tie, want the lexically first reason", got)
		}
	}
}
