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

// decidingScanner fails its first attempt, recording a sampling decision on
// the result the way the real scanner does, and notes what each attempt
// inherited.
type decidingScanner struct {
	mu        sync.Mutex
	calls     int
	inherited []*model.BodyCapture
}

func (f *decidingScanner) Scan(ctx context.Context, target config.Resolved, mode model.ConsentMode) (scanner.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	inherited := scanner.InheritedBodies(ctx)
	f.inherited = append(f.inherited, inherited)

	res := &model.Result{Target: target.Name, ConsentMode: mode, Termination: model.TermIdle}

	switch {
	case inherited != nil:
		res.BodyCapture = inherited
	default:
		res.BodyCapture = &model.BodyCapture{
			Store: model.BodyStoreAll, Sampled: true, Decision: model.BodyDecisionSampled,
			DecidedBy: "first-attempt",
		}
	}

	if f.calls == 1 {
		res.Termination = model.TermError
		res.Error = "the browser crashed"
	}

	return scanner.Outcome{Result: res}, nil
}

func (f *decidingScanner) seen() []*model.BodyCapture {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]*model.BodyCapture(nil), f.inherited...)
}

// Story 1.11, AC7: a retry inherits the decision its first attempt took, and
// the next scheduled scan takes its own again.
func TestARetryInheritsTheBodySamplingDecision(t *testing.T) {
	t.Parallel()

	fake := &decidingScanner{}
	sink := &collectingSink{}

	d, err := daemon.New(fake, sink,
		[]config.Resolved{retryTarget("site", retry.Policy{
			Attempts: 3, Backoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		})},
		daemon.Options{Concurrency: 1, PerOriginConcurrency: 1, CatchUp: true, Tick: 2 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	run(t, d)

	if !waitFor(t, 5*time.Second, func() bool { return sink.count() >= 1 }) {
		t.Fatal("the retry never produced a published result")
	}

	seen := fake.seen()
	if len(seen) < 2 {
		t.Fatalf("the scanner ran %d times, want a first attempt and a retry", len(seen))
	}

	if seen[0] != nil {
		t.Errorf("the first attempt inherited %+v; a first attempt takes its own decision", seen[0])
	}

	if seen[1] == nil || seen[1].DecidedBy != "first-attempt" || !seen[1].Sampled {
		t.Errorf("the retry inherited %+v, want the first attempt's sampled decision", seen[1])
	}
}
