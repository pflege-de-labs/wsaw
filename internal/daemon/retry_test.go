package daemon_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/retry"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// Story 3.8, from the scheduler's side. No browser needed: what is under test
// is what the scheduler does with an outcome, not how the outcome was reached.

// failingScanner produces a given termination for the first n calls and then
// succeeds, so a test can watch a retry recover.
type failingScanner struct {
	mu sync.Mutex

	failFor     int
	termination model.TerminationReason
	errText     string

	// hold, when non-nil, keeps the first call in flight until it is closed,
	// so a test can change the configuration underneath a running scan.
	hold chan struct{}

	calls []scanner.Outcome
}

func (f *failingScanner) Scan(ctx context.Context, target config.Resolved, mode model.ConsentMode) (scanner.Outcome, error) {
	f.mu.Lock()

	res := &model.Result{
		Target:      target.Name,
		ConsentMode: mode,
		Termination: model.TermIdle,
	}

	if len(f.calls) < f.failFor {
		res.Termination = f.termination
		res.Error = f.errText
	}

	out := scanner.Outcome{Result: res}
	first := len(f.calls) == 0
	f.calls = append(f.calls, out)
	hold := f.hold
	f.mu.Unlock()

	if first && hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return scanner.Outcome{}, ctx.Err()
		}
	}

	return out, nil
}

func (f *failingScanner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

func (f *failingScanner) countFor(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, out := range f.calls {
		if out.Result.Target == target {
			n++
		}
	}

	return n
}

// collectingSink records what the daemon published, which is the observable
// form of "who gets told".
type collectingSink struct {
	mu        sync.Mutex
	published []*model.Result
}

func (s *collectingSink) Publish(_ context.Context, out scanner.Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.published = append(s.published, out.Result)
}

func (s *collectingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.published)
}

func (s *collectingSink) results() []*model.Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*model.Result(nil), s.published...)
}

// retryTarget is a target with a fast schedule and a fast retry, so a test
// can watch several attempts without waiting.
func retryTarget(name string, policy retry.Policy) config.Resolved {
	return config.Resolved{
		Name:         name,
		URL:          "https://example.com/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		// Long enough that the scheduled runs do not muddle the retries under
		// test; the retries themselves are what drive the extra attempts.
		Interval: time.Hour,
		Retry:    policy,
	}
}

func TestAFailedScanIsRetried(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 1, termination: model.TermError, errText: "the browser crashed"}
	sink := &collectingSink{}

	d, err := daemon.New(fake, sink,
		[]config.Resolved{retryTarget("site", retry.Policy{
			Attempts: 3, Backoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		})},
		daemon.Options{Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return fake.count() >= 2 }) {
		t.Fatalf("a failed scan was not retried: %d attempts", fake.count())
	}

	if !waitFor(t, 5*time.Second, func() bool { return sink.count() >= 1 }) {
		t.Fatal("the successful retry was never published")
	}

	// AC7: only the final outcome is published. The failure is in the store —
	// the scanner put it there — but nobody is paged for a failure that a
	// retry fixed.
	for _, res := range sink.results() {
		if res.Termination != model.TermIdle {
			t.Errorf("a failed attempt was published: %s (%s)", res.Termination, res.Error)
		}
	}
}

// AC2: a robots skip is a decision, not a failure. Retrying it would reach
// the same decision and pester the site for nothing.
func TestASkippedScanIsNotRetried(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 5, termination: model.TermSkipped, errText: "disallowed by robots.txt"}
	sink := &collectingSink{}

	d, err := daemon.New(fake, sink,
		[]config.Resolved{retryTarget("site", retry.Policy{
			Attempts: 4, Backoff: 2 * time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		})},
		daemon.Options{Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return sink.count() >= 1 }) {
		t.Fatal("the skipped scan was never published")
	}

	// Give the scheduler room to make a second attempt if it were going to.
	time.Sleep(80 * time.Millisecond)

	if got := fake.count(); got != 1 {
		t.Errorf("a skipped scan was attempted %d times, want 1", got)
	}
}

// AC1 and AC7: attempts are bounded, and running out is reported.
func TestRetriesAreBoundedAndExhaustionIsReported(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 99, termination: model.TermError, errText: "the browser crashed"}
	sink := &collectingSink{}

	var (
		mu        sync.Mutex
		retried   int
		exhausted int
	)

	d, err := daemon.New(fake, sink,
		[]config.Resolved{retryTarget("site", retry.Policy{
			Attempts: 3, Backoff: 2 * time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		})},
		daemon.Options{
			Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond,
			OnRetry: func(string, model.ConsentMode, int) {
				mu.Lock()
				retried++
				mu.Unlock()
			},
			OnRetriesExhausted: func(string, model.ConsentMode, int) {
				mu.Lock()
				exhausted++
				mu.Unlock()
			},
		})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return fake.count() >= 3 }) {
		t.Fatalf("the scan was attempted %d times, want 3", fake.count())
	}

	// It must stop at three, not keep going.
	time.Sleep(80 * time.Millisecond)

	if got := fake.count(); got != 3 {
		t.Errorf("the scan was attempted %d times, want exactly the configured 3", got)
	}

	mu.Lock()
	defer mu.Unlock()

	if retried != 2 {
		t.Errorf("reported %d retries, want 2 for 3 attempts", retried)
	}

	if exhausted != 1 {
		t.Errorf("reported exhaustion %d times, want 1", exhausted)
	}

	// The final failure is published: a target that is genuinely broken has
	// to be reported as broken (Tenet 8).
	if sink.count() != 1 {
		t.Fatalf("published %d results, want the one final failure", sink.count())
	}

	if res := sink.results()[0]; res.Termination != model.TermError {
		t.Errorf("published %s, want the final failure", res.Termination)
	}
}

// AC1: retrying can be switched off, and then a failure is published at once.
func TestRetryingCanBeDisabled(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 99, termination: model.TermError, errText: "the browser crashed"}
	sink := &collectingSink{}

	d, err := daemon.New(fake, sink,
		[]config.Resolved{retryTarget("site", retry.Policy{Attempts: 1})},
		daemon.Options{Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return sink.count() >= 1 }) {
		t.Fatal("the failure was never published")
	}

	time.Sleep(60 * time.Millisecond)

	if got := fake.count(); got != 1 {
		t.Errorf("attempted %d times with retrying disabled, want 1", got)
	}
}

// AC8: a pending retry must not hold a worker. With one worker and two
// targets, the target that is waiting to be retried cannot be allowed to stop
// the other one from being scanned at all.
func TestAPendingRetryDoesNotHoldTheWorker(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 1, termination: model.TermError, errText: "the browser crashed"}

	policy := retry.Policy{Attempts: 3, Backoff: 300 * time.Millisecond, MaxBackoff: time.Second}

	first := retryTarget("first", policy)

	second := retryTarget("second", retry.Policy{Attempts: 1})
	second.URL = "https://other.example/"

	d, err := daemon.New(fake, nil, []config.Resolved{first, second},
		daemon.Options{Concurrency: 1, PerOriginConcurrency: 1, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	// The second target has to be scanned well before the first one's retry
	// is due, which can only happen if the waiting retry released its slot.
	scanned := func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()

		for _, out := range fake.calls {
			if out.Result.Target == "second" {
				return true
			}
		}

		return false
	}

	if !waitFor(t, 200*time.Millisecond, scanned) {
		t.Error("a target waiting to be retried blocked the only worker")
	}
}

// The minimum interval must not swallow a retry. It exists to bound how often
// wsaw asks a site for a new observation; a retry is the same observation, so
// holding it for an hour would make retrying useless.
func TestARetryIsNotHeldBackByTheMinimumInterval(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 1, termination: model.TermError, errText: "the browser crashed"}

	tgt := retryTarget("site", retry.Policy{
		Attempts: 2, Backoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	tgt.MinInterval = time.Hour

	d, err := daemon.New(fake, nil, []config.Resolved{tgt},
		daemon.Options{Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return fake.count() >= 2 }) {
		t.Errorf("the retry was held back by minInterval: %d attempts", fake.count())
	}
}

// publishedIdle reports whether a successful scan of the target was published.
func (s *collectingSink) publishedIdle(target string) bool {
	for _, res := range s.results() {
		if res.Target == target && res.Termination == model.TermIdle {
			return true
		}
	}

	return false
}

// otherTarget is a second target whose only purpose is to make a reload
// visible in Jobs(), so a test can tell the reload has been applied.
func otherTarget() config.Resolved {
	tgt := retryTarget("other", retry.Policy{Attempts: 1})
	tgt.URL = "https://other.example/"

	return tgt
}

// A reload must not drop a retry that is waiting to run. The failed attempt
// is held back from notification because a retry is coming (AC7); if the
// reload then discards the retry, the failure is never published and the
// target goes unobserved until its next scheduled run — the gap Story 3.8
// exists to close.
func TestAPendingRetrySurvivesAReload(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{failFor: 1, termination: model.TermError, errText: "net::ERR_NAME_NOT_RESOLVED"}
	sink := &collectingSink{}
	retried := make(chan struct{}, 1)

	tgt := retryTarget("site", retry.Policy{
		Attempts: 2, Backoff: 500 * time.Millisecond, MaxBackoff: time.Second,
	})
	// Without a pending retry the reloaded job would be held back by this
	// floor, so a second scan can only come from the retry.
	tgt.MinInterval = time.Hour

	d, err := daemon.New(fake, sink, []config.Resolved{tgt},
		daemon.Options{
			Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond,
			OnRetry: func(string, model.ConsentMode, int) {
				select {
				case retried <- struct{}{}:
				default:
				}
			},
		})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	select {
	case <-retried:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed scan was never scheduled for a retry")
	}

	d.Reload([]config.Resolved{tgt, otherTarget()})

	if !waitFor(t, 5*time.Second, func() bool { return len(d.Jobs()) == 2 }) {
		t.Fatal("the reload was never applied")
	}

	if got := fake.countFor("site"); got != 1 {
		t.Fatalf("site was scanned %d times before the reload landed; the test cannot tell whether the retry survived", got)
	}

	if !waitFor(t, 5*time.Second, func() bool { return fake.countFor("site") >= 2 }) {
		t.Fatal("the pending retry was dropped by the reload")
	}

	if !waitFor(t, 5*time.Second, func() bool { return sink.publishedIdle("site") }) {
		t.Error("the successful retry was never published")
	}
}

// A scan that is in flight when a reload lands must still be retried if it
// fails. The retry has to be scheduled on the job the scheduler now runs, not
// on the one the reload replaced, or it is lost along with the failure's
// notification.
func TestAFailureDuringAReloadIsStillRetried(t *testing.T) {
	t.Parallel()

	fake := &failingScanner{
		failFor: 1, termination: model.TermError, errText: "net::ERR_NAME_NOT_RESOLVED",
		hold: make(chan struct{}),
	}
	sink := &collectingSink{}

	tgt := retryTarget("site", retry.Policy{
		Attempts: 2, Backoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	tgt.MinInterval = time.Hour

	d, err := daemon.New(fake, sink, []config.Resolved{tgt},
		daemon.Options{Concurrency: 2, PerOriginConcurrency: 2, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return fake.countFor("site") == 1 }) {
		t.Fatal("the first scan never started")
	}

	d.Reload([]config.Resolved{tgt, otherTarget()})

	if !waitFor(t, 5*time.Second, func() bool { return len(d.Jobs()) == 2 }) {
		t.Fatal("the reload was never applied")
	}

	close(fake.hold)

	if !waitFor(t, 5*time.Second, func() bool { return fake.countFor("site") >= 2 }) {
		t.Fatal("a scan that failed across a reload was never retried")
	}

	if !waitFor(t, 5*time.Second, func() bool { return sink.publishedIdle("site") }) {
		t.Error("the successful retry was never published")
	}
}
