package retry

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The whole story turns on one distinction: a failure is a missing
// observation, a finding is something wsaw saw. Retrying the first is the
// point; retrying the second would only confirm it, at the scanned site's
// expense.
func TestRetryableSeparatesFailuresFromFindings(t *testing.T) {
	t.Parallel()

	p := Policy{Attempts: 3}

	cases := map[string]struct {
		res  *model.Result
		want bool
		why  string
	}{
		"no result at all": {
			res: nil, want: true,
			why: "nothing was observed",
		},
		"failed": {
			res:  &model.Result{Termination: model.TermError, Error: "the browser crashed"},
			want: true,
			why:  "the browser or the navigation failed, not the page",
		},
		"skipped by robots": {
			res:  &model.Result{Termination: model.TermSkipped, Error: "disallowed by robots.txt"},
			want: false,
			why:  "a policy decision reached again is the same decision",
		},
		"loaded, consent failed": {
			res: &model.Result{
				Termination: model.TermIdle,
				Consent:     model.Consent{Outcome: model.OutcomeFailed, Reason: "no banner found"},
			},
			want: false,
			why:  "the page loaded; a failed banner interaction is the finding",
		},
		"loaded, nothing on it": {
			res:  &model.Result{Termination: model.TermIdle},
			want: false,
			why:  "an empty page is an observation",
		},
		"timed out": {
			res:  &model.Result{Termination: model.TermTimeout},
			want: false,
			why:  "truncation is retried only when configured",
		},
		"request cap": {
			res:  &model.Result{Termination: model.TermRequestCap},
			want: false,
			why:  "as above",
		},
	}

	for name, tc := range cases {
		if got := p.Retryable(tc.res); got != tc.want {
			t.Errorf("Retryable(%s) = %t, want %t: %s", name, got, tc.want, tc.why)
		}
	}
}

// AC3: truncation is retryable, but only on purpose. Retrying every slow site
// doubles the load wsaw puts on it and usually reproduces the same result.
func TestTruncationIsRetriedOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	truncated := []model.TerminationReason{model.TermTimeout, model.TermRequestCap, model.TermByteCap}

	off := Policy{Attempts: 3}
	on := Policy{Attempts: 3, Truncated: true}

	for _, term := range truncated {
		res := &model.Result{Termination: term}

		if off.Retryable(res) {
			t.Errorf("%s is retried by default", term)
		}

		if !on.Retryable(res) {
			t.Errorf("%s is not retried even when configured", term)
		}
	}

	// Turning it on must not make a robots skip retryable.
	if on.Retryable(&model.Result{Termination: model.TermSkipped}) {
		t.Error("enabling truncation retries also retried a skipped scan")
	}
}

func TestEnabledAndMaxAttempts(t *testing.T) {
	t.Parallel()

	for _, attempts := range []int{0, 1} {
		p := Policy{Attempts: attempts}

		if p.Enabled() {
			t.Errorf("Attempts=%d reports retrying as enabled", attempts)
		}

		if p.MaxAttempts() != 1 {
			t.Errorf("Attempts=%d gives MaxAttempts %d, want 1", attempts, p.MaxAttempts())
		}
	}

	p := Policy{Attempts: 3}

	if !p.Enabled() || p.MaxAttempts() != 3 {
		t.Errorf("Attempts=3: enabled=%t max=%d", p.Enabled(), p.MaxAttempts())
	}
}

// The backoff doubles, is capped, and never applies to the first attempt.
func TestDelayGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	p := Policy{Attempts: 6, Backoff: time.Second, MaxBackoff: 4 * time.Second}

	if d := p.Delay(1, "site/reject"); d != 0 {
		t.Errorf("the first attempt waits %s, want none", d)
	}

	second := p.Delay(2, "site/reject")
	third := p.Delay(3, "site/reject")

	if third <= second {
		t.Errorf("the backoff did not grow: %s then %s", second, third)
	}

	// The cap holds, jitter included: jitter is a tenth of the base, so the
	// ceiling is the cap plus that.
	for attempt := 2; attempt <= 6; attempt++ {
		if d := p.Delay(attempt, "site/reject"); d > p.MaxBackoff+p.MaxBackoff/10 {
			t.Errorf("attempt %d waits %s, above the %s cap", attempt, d, p.MaxBackoff)
		}
	}
}

// AC4: jitter, so a network blip that fails many targets at once does not
// have them all retry in the same instant — and deterministic, so a restart
// reproduces the same spread rather than reshuffling every target.
func TestDelayIsJitteredPerJobAndDeterministic(t *testing.T) {
	t.Parallel()

	p := Policy{Attempts: 3, Backoff: 10 * time.Second, MaxBackoff: time.Minute}

	first := p.Delay(2, "one/reject")
	second := p.Delay(2, "two/reject")

	if first == second {
		t.Error("two different jobs wait exactly the same time; a blip would make them retry in lockstep")
	}

	for range 5 {
		if again := p.Delay(2, "one/reject"); again != first {
			t.Errorf("the same job got %s then %s; the spread is not reproducible", first, again)
		}
	}

	// Jitter must not swamp the backoff: it is a spread, not a policy.
	if first < 10*time.Second || first > 11*time.Second {
		t.Errorf("delay %s is not the configured 10s plus a small jitter", first)
	}
}

// TotalDelay is what configuration validation uses to refuse a retry schedule
// that would still be running when the next scheduled scan starts.
func TestTotalDelayCoversEveryRetry(t *testing.T) {
	t.Parallel()

	p := Policy{Attempts: 3, Backoff: time.Minute, MaxBackoff: time.Hour}

	total := p.TotalDelay("site/reject")
	sum := p.Delay(2, "site/reject") + p.Delay(3, "site/reject")

	if total != sum {
		t.Errorf("TotalDelay = %s, want the sum of every retry's delay (%s)", total, sum)
	}

	// With retrying off there is nothing to wait for.
	if got := (Policy{Attempts: 1}).TotalDelay("site/reject"); got != 0 {
		t.Errorf("a disabled policy reports %s of delay", got)
	}
}

// A policy with no backoff configured must still wait: retrying instantly
// would hit a site that just failed with no pause at all.
func TestAZeroBackoffStillWaits(t *testing.T) {
	t.Parallel()

	if d := (Policy{Attempts: 2}).Delay(2, "site/reject"); d <= 0 {
		t.Errorf("an unconfigured backoff waits %s", d)
	}
}
