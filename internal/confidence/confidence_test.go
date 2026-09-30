package confidence_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/confidence"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

const degradedRatio = 0.05

var opts = confidence.Options{DegradedFailureRatio: degradedRatio}

// clean is a scan with nothing wrong: idle, every request observed, consent
// applied, run for as long as its series usually does.
func clean(requests int) *model.Result {
	res := &model.Result{
		Target:      "site",
		ConsentMode: model.ConsentReject,
		Termination: model.TermIdle,
		Duration:    10 * time.Second,
		Consent:     model.Consent{Outcome: model.OutcomeApplied},
	}

	for i := range requests {
		res.Requests = append(res.Requests, model.Request{
			URL:    fmt.Sprintf("https://example.com/%d", i),
			Status: 200,
		})
	}

	return res
}

// steady is a series history of n earlier scans of exactly d each.
func steady(n int, d time.Duration) confidence.History {
	h := confidence.History{}
	for range n {
		h.Durations = append(h.Durations, d)
	}

	return h
}

func lose(res *model.Result, n int, reason string) {
	for i := range n {
		res.Requests[i].Failed = true
		res.Requests[i].FailureReason = reason
	}
}

func reason(t *testing.T, c model.Confidence, signal model.ConfidenceSignal) model.ConfidenceReason {
	t.Helper()

	for _, r := range c.Reasons {
		if r.Signal == signal {
			return r
		}
	}

	t.Fatalf("no %s reason in %+v", signal, c.Reasons)

	return model.ConfidenceReason{}
}

func TestACleanScanScoresExactlyOneHundredWithEverySignalChecked(t *testing.T) {
	t.Parallel()

	c := confidence.Score(clean(100), steady(10, 10*time.Second), opts)

	if c.Score != 100 || c.Band != model.ConfidenceHigh {
		t.Fatalf("score = %d %s, want 100 high", c.Score, c.Band)
	}

	want := []model.ConfidenceSignal{
		model.SignalTermination, model.SignalFetches, model.SignalDuration, model.SignalConsent,
	}

	got := make([]model.ConfidenceSignal, 0, len(c.Reasons))

	for _, r := range c.Reasons {
		got = append(got, r.Signal)

		if r.NotAssessed != "" {
			t.Errorf("%s was not assessed (%s); 100 must mean every signal was checked", r.Signal, r.NotAssessed)
		}
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("reasons = %v, want %v", got, want)
	}

	if c.DurationReference == nil || c.DurationReference.Median != 10*time.Second || c.DurationReference.Scans != 10 {
		t.Errorf("duration reference = %+v, want the 10s median over 10 scans", c.DurationReference)
	}
}

func TestTermination(t *testing.T) {
	t.Parallel()

	cases := []struct {
		term   model.TerminationReason
		err    string
		points int
		score  int
		band   model.ConfidenceBand
	}{
		{model.TermIdle, "", 0, 100, model.ConfidenceHigh},
		{model.TermTimeout, "", 30, 70, model.ConfidenceMedium},
		{model.TermRequestCap, "", 25, 75, model.ConfidenceMedium},
		{model.TermByteCap, "", 25, 75, model.ConfidenceMedium},
		{model.TermError, "navigation failed", 100, 0, model.ConfidenceNone},
		{model.TermSkipped, "robots.txt disallows", 100, 0, model.ConfidenceNone},
		// An error on a scan that otherwise ended idle is still no asset list.
		{model.TermIdle, "browser crashed", 100, 0, model.ConfidenceNone},
	}

	for _, tc := range cases {
		t.Run(string(tc.term)+tc.err, func(t *testing.T) {
			t.Parallel()

			res := clean(20)
			res.Termination = tc.term
			res.Error = tc.err

			c := confidence.Score(res, steady(10, 10*time.Second), opts)

			if c.Score != tc.score || c.Band != tc.band {
				t.Errorf("score = %d %s, want %d %s", c.Score, c.Band, tc.score, tc.band)
			}

			if got := reason(t, c, model.SignalTermination); got.Points != tc.points {
				t.Errorf("termination points = %d, want %d", got.Points, tc.points)
			}

			if tc.err != "" && !strings.Contains(reason(t, c, model.SignalTermination).Observed, tc.err) {
				t.Errorf("a failed scan's reason does not carry its error %q", tc.err)
			}
		})
	}
}

func TestFetches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		lost   int
		reason string
		points int
		band   model.ConfidenceBand
	}{
		{"none lost", 0, "", 0, model.ConfidenceHigh},
		{"one percent", 1, "net::ERR_CONNECTION_RESET", 4, model.ConfidenceHigh},
		// Just under the differ's degraded threshold: points only.
		{"four percent", 4, "net::ERR_NAME_NOT_RESOLVED", 16, model.ConfidenceMedium},
		// At the threshold the band is capped at low whatever the points say,
		// so the board agrees with the differ's scan-degraded.
		{"at the degraded threshold", 5, "net::ERR_NAME_NOT_RESOLVED", 20, model.ConfidenceLow},
		{"capped at sixty", 40, "net::ERR_TIMED_OUT", 60, model.ConfidenceLow},
		// ERR_ABORTED is what a torn-down beacon looks like, not a lost fetch.
		{"aborted", 10, "net::ERR_ABORTED", 0, model.ConfidenceHigh},
		// Blocked by the client is a decision, and an accurate observation.
		{"blocked", 10, "net::ERR_BLOCKED_BY_CLIENT", 0, model.ConfidenceHigh},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := clean(100)
			lose(res, tc.lost, tc.reason)

			c := confidence.Score(res, steady(10, 10*time.Second), opts)
			got := reason(t, c, model.SignalFetches)

			if got.Points != tc.points {
				t.Errorf("points = %d, want %d (%s)", got.Points, tc.points, got.Observed)
			}

			if c.Band != tc.band {
				t.Errorf("band = %s (score %d), want %s", c.Band, c.Score, tc.band)
			}

			if tc.points > 0 && !strings.Contains(got.Observed, tc.reason) {
				t.Errorf("the reason %q does not name the failure %s", got.Observed, tc.reason)
			}
		})
	}
}

func TestAnHTTPErrorIsAnObservationNotALostFetch(t *testing.T) {
	t.Parallel()

	res := clean(10)
	res.Requests[0].Status = 404
	res.Requests[1].Status = 503

	if got := reason(t, confidence.Score(res, steady(10, 10*time.Second), opts), model.SignalFetches); got.Points != 0 {
		t.Errorf("HTTP error statuses cost %d points; the server answered, which is an observation", got.Points)
	}
}

func TestDuration(t *testing.T) {
	t.Parallel()

	const median = 10 * time.Second

	cases := []struct {
		name   string
		d      time.Duration
		points int
	}{
		{"at the median", median, 0},
		{"exactly half", median / 2, 0},
		{"just under half", median/2 - time.Millisecond, 15},
		{"exactly twice", 2 * median, 0},
		{"just over twice", 2*median + time.Millisecond, 15},
		{"exactly a quarter", median / 4, 15},
		{"just under a quarter", median/4 - time.Millisecond, 30},
		{"exactly four times", 4 * median, 15},
		{"just over four times", 4*median + time.Millisecond, 30},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := clean(10)
			res.Duration = tc.d

			got := reason(t, confidence.Score(res, steady(10, median), opts), model.SignalDuration)

			if got.Points != tc.points {
				t.Errorf("a %s scan against a %s median cost %d points, want %d", tc.d, median, got.Points, tc.points)
			}

			if !strings.Contains(got.Reference, "10s") || !strings.Contains(got.Reference, "10 earlier") {
				t.Errorf("the reference %q does not state the median and the scans behind it", got.Reference)
			}
		})
	}
}

func TestDurationIsNotAssessedWithTooLittleHistory(t *testing.T) {
	t.Parallel()

	res := clean(10)
	res.Duration = time.Second // far off, but there is nothing to be far from

	c := confidence.Score(res, steady(4, 10*time.Second), opts)
	got := reason(t, c, model.SignalDuration)

	if got.Points != 0 || !strings.Contains(got.NotAssessed, "4 of 5") {
		t.Errorf("duration reason = %+v, want not assessed with 4 of 5 earlier scans", got)
	}

	if c.DurationReference != nil {
		t.Errorf("a reference was formed from 4 scans: %+v", c.DurationReference)
	}
}

func TestDurationIsNotAssessedWhenTheHistoryCouldNotBeRead(t *testing.T) {
	t.Parallel()

	c := confidence.Score(clean(10), confidence.History{Unavailable: "the store did not answer"}, opts)

	if got := reason(t, c, model.SignalDuration); got.NotAssessed != "the store did not answer" {
		t.Errorf("duration reason = %+v, want it not assessed with the store's reason", got)
	}
}

func TestTheMedianUsesOnlyTheLatestScans(t *testing.T) {
	t.Parallel()

	// Ten recent 10s scans and five much older 100s ones: only the ten count.
	hist := steady(10, 10*time.Second)
	hist.Durations = append(hist.Durations, steady(5, 100*time.Second).Durations...)

	c := confidence.Score(clean(10), hist, opts)

	if c.DurationReference == nil || c.DurationReference.Median != 10*time.Second || c.DurationReference.Scans != 10 {
		t.Errorf("reference = %+v, want the median of the latest ten", c.DurationReference)
	}
}

func TestConsent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		mode      model.ConsentMode
		outcome   model.ConsentOutcome
		heuristic bool
		points    int
	}{
		{"applied", model.ConsentReject, model.OutcomeApplied, false, 0},
		{"not needed", model.ConsentNone, model.OutcomeNotNeeded, false, 0},
		{"failed", model.ConsentAccept, model.OutcomeFailed, false, 40},
		{"unverified", model.ConsentReject, model.OutcomeUnverified, false, 40},
		{"banner visible", model.ConsentReject, model.OutcomeBannerVisible, false, 40},
		{"necessary-only in reject", model.ConsentReject, model.OutcomeNecessaryOnly, false, 0},
		{"necessary-only in accept", model.ConsentAccept, model.OutcomeNecessaryOnly, false, 20},
		{"heuristic", model.ConsentReject, model.OutcomeApplied, true, 10},
		{"heuristic and unverified", model.ConsentReject, model.OutcomeUnverified, true, 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := clean(10)
			res.ConsentMode = tc.mode
			res.Consent = model.Consent{Outcome: tc.outcome, Heuristic: tc.heuristic}

			if got := reason(t, confidence.Score(res, steady(10, 10*time.Second), opts), model.SignalConsent); got.Points != tc.points {
				t.Errorf("points = %d, want %d (%s)", got.Points, tc.points, got.Observed)
			}
		})
	}
}

func TestTheScoreIsClampedAtZero(t *testing.T) {
	t.Parallel()

	res := clean(10)
	res.Termination = model.TermTimeout
	res.Duration = time.Hour
	res.Consent = model.Consent{Outcome: model.OutcomeFailed, Heuristic: true}
	lose(res, 10, "net::ERR_CONNECTION_REFUSED")

	c := confidence.Score(res, steady(10, 10*time.Second), opts)

	if c.Score != 0 || c.Band != model.ConfidenceLow {
		t.Errorf("score = %d %s, want 0 low: a completed scan that lost everything is a poor observation, not none",
			c.Score, c.Band)
	}
}

func TestTheSameInputsGiveTheSameScore(t *testing.T) {
	t.Parallel()

	res := clean(50)
	res.Termination = model.TermRequestCap
	lose(res, 3, "net::ERR_CONNECTION_RESET")
	res.Requests[3].Failed, res.Requests[3].FailureReason = true, "net::ERR_TIMED_OUT"

	hist := steady(7, 10*time.Second)
	hist.Durations[2] = 3 * time.Second

	first := confidence.Score(res, hist, opts)
	for range 20 {
		if again := confidence.Score(res, hist, opts); !reflect.DeepEqual(again, first) {
			t.Fatalf("two scores of the same inputs differ:\n%+v\n%+v", first, again)
		}
	}
}

func TestConsentCaveatIsEmptyForATrustworthyConsentState(t *testing.T) {
	t.Parallel()

	res := clean(1)
	res.ConsentMode = model.ConsentReject
	res.Consent.Outcome = model.OutcomeNecessaryOnly

	if caveat := confidence.ConsentCaveat(res); caveat != "" {
		t.Errorf("a verified necessary-only in reject mode has caveat %q; Story 2.10 trusts it", caveat)
	}
}
