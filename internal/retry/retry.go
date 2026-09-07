// Package retry decides whether a scan that produced no usable observation is
// worth another attempt, and when (Story 3.8).
//
// It is its own package because both ends need it: configuration resolves and
// validates the policy, and the scheduler and the one-shot command apply it.
// Configuration cannot import the scanner — the scanner imports configuration
// — and a policy duplicated at both ends would drift.
package retry

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Policy decides whether a scan that did not produce a usable
// observation is worth another attempt, and when (Story 3.8).
//
// The distinction it encodes is between a failure and a finding. A page that
// loads nothing, refuses consent, or answers 500 is a *result*: wsaw observed
// it, and observing it again would only confirm it. A browser that died
// before navigating, a container that would not start, a navigation that
// failed at the network level — those are *missing observations*, and not
// missing them is the whole job.
//
// The policy is data, and the deciding is here, so the scheduler and the
// one-shot command cannot disagree about what "failed" means.
type Policy struct {
	// Attempts is the total number of attempts including the first. Zero or
	// one disables retrying.
	Attempts int

	// Backoff is the wait before the second attempt. It doubles for each
	// attempt after that, up to MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration

	// Truncated also retries a scan that was cut short — a timeout, a request
	// cap, a byte cap. Off by default: retrying every slow site doubles the
	// load wsaw puts on it and usually reproduces the same truncation.
	Truncated bool
}

// Enabled reports whether this policy retries anything at all.
func (p Policy) Enabled() bool { return p.Attempts > 1 }

// MaxAttempts is the total number of attempts, never less than one.
func (p Policy) MaxAttempts() int {
	if p.Attempts < 1 {
		return 1
	}

	return p.Attempts
}

// Retryable reports whether a result represents a missing observation rather
// than something wsaw actually saw.
//
// A nil result means the scan produced nothing at all, which is the clearest
// case of all.
func (p Policy) Retryable(res *model.Result) bool {
	if res == nil {
		return true
	}

	switch res.Termination {
	case model.TermError:
		// The browser, the container, or the navigation failed. Another
		// attempt can plausibly succeed.
		return true

	case model.TermSkipped:
		// A robots policy decided not to scan. That is a decision, not a
		// failure, and repeating it would only reach the same decision.
		return false

	case model.TermTimeout, model.TermRequestCap, model.TermByteCap:
		return p.Truncated

	case model.TermIdle:
		// The page loaded. Whatever else is wrong with the result — a failed
		// consent interaction, an error status, no assets at all — is a
		// finding, and re-scanning would report the same finding.
		return false

	default:
		return false
	}
}

// Delay returns how long to wait before the given attempt, which counts from
// 2 — attempt 1 is the original scan and never waits.
//
// The backoff doubles and carries jitter derived from the job key, so fifty
// targets that fail during one network blip do not retry in lockstep and turn
// the recovery into a load test (Tenet 17). The jitter is deterministic for
// the same reason the scheduler's is: a restart should reproduce the same
// spread rather than reshuffle every target's phase.
func (p Policy) Delay(attempt int, key string) time.Duration {
	if attempt < 2 {
		return 0
	}

	base := p.Backoff
	if base <= 0 {
		base = 30 * time.Second
	}

	for range attempt - 2 {
		base *= 2

		if p.MaxBackoff > 0 && base >= p.MaxBackoff {
			base = p.MaxBackoff

			break
		}
	}

	if p.MaxBackoff > 0 && base > p.MaxBackoff {
		base = p.MaxBackoff
	}

	return base + retryJitter(key, attempt, base)
}

// TotalDelay is the longest a full run of retries can take in waiting alone.
// Configuration validation uses it to refuse a retry schedule that could
// outlast the target's own interval, which would mean overlapping runs of the
// same target.
func (p Policy) TotalDelay(key string) time.Duration {
	var total time.Duration

	for attempt := 2; attempt <= p.MaxAttempts(); attempt++ {
		total += p.Delay(attempt, key)
	}

	return total
}

// retryJitter spreads retries over up to a tenth of the backoff.
func retryJitter(key string, attempt int, base time.Duration) time.Duration {
	window := base / 10
	if window <= 0 {
		return 0
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	_, _ = fmt.Fprintf(h, "#%d", attempt)

	return time.Duration(int64(h.Sum32()) % int64(window))
}
