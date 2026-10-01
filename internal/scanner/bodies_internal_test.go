package scanner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/retry"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 1.11: the sampling decision, taken and settled without a browser.

type fakeLedger struct {
	mu sync.Mutex

	sampled bool
	err     error

	decided []store.BodySampleRequest
	settled []string // "decidedBy scanID outcome"
}

func (f *fakeLedger) DecideBodySample(_ context.Context, req store.BodySampleRequest) (store.BodySampleDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.decided = append(f.decided, req)

	if f.err != nil {
		return store.BodySampleDecision{}, f.err
	}

	sampled := f.sampled || req.Forced

	d := store.BodySampleDecision{Sampled: sampled, WindowScans: 5}
	if sampled {
		d.WindowSampled = 1
	}

	return d, nil
}

func (f *fakeLedger) SettleBodySample(_ context.Context, decidedBy, scanID, outcome string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.settled = append(f.settled, decidedBy+" "+scanID+" "+outcome)

	return nil
}

func bodyTarget(p config.BodyPolicy) config.Resolved {
	return config.Resolved{
		Name: "site", Bodies: p, HardTimeout: time.Minute,
		Retry: retry.Policy{Attempts: 2, Backoff: time.Minute},
	}
}

var sampling = config.BodyPolicy{
	Store: model.BodyStoreAll, Ratio: 0.05, RatioWindow: 24 * time.Hour,
	RequestBodies: true, MaxBodyBytes: 100, MaxScanBytes: 1000,
}

func scannerWith(l BodyLedger) *Scanner {
	return &Scanner{deps: Deps{Ledger: l, Logger: slog.New(slog.DiscardHandler)}}
}

func TestDecideBodies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		policy   config.BodyPolicy
		ledger   *fakeLedger
		ctx      func(context.Context) context.Context
		sampled  bool
		decision model.BodyDecision
		store    model.BodyStore
	}{
		{"off", config.BodyPolicy{Store: model.BodyStoreNone}, &fakeLedger{}, nil,
			false, model.BodyDecisionDisabled, model.BodyStoreNone},
		{"the ledger samples", sampling, &fakeLedger{sampled: true}, nil,
			true, model.BodyDecisionSampled, model.BodyStoreAll},
		{"the ledger does not", sampling, &fakeLedger{}, nil,
			false, model.BodyDecisionNotSampled, model.BodyStoreAll},
		{"ledger down, ratio between", sampling, &fakeLedger{err: errors.New("db down")}, nil,
			false, model.BodyDecisionLedgerUnavailable, model.BodyStoreAll},
		{"ledger down, ratio one", config.BodyPolicy{Store: model.BodyStoreHashed, Ratio: 1}, &fakeLedger{err: errors.New("db down")}, nil,
			true, model.BodyDecisionSampled, model.BodyStoreHashed},
		{"forced on a target with storage off", config.BodyPolicy{Store: model.BodyStoreNone}, &fakeLedger{},
			func(ctx context.Context) context.Context { return WithForcedBodies(ctx, model.BodyStoreAll) },
			true, model.BodyDecisionForced, model.BodyStoreAll},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			if c.ctx != nil {
				ctx = c.ctx(ctx)
			}

			s := scannerWith(c.ledger)
			bc := s.decideBodies(ctx, "scan-1", bodyTarget(c.policy), model.ConsentReject, s.deps.Logger)

			if bc.Sampled != c.sampled || bc.Decision != c.decision || bc.Store != c.store {
				t.Errorf("decision = sampled %v %s store %s; want sampled %v %s store %s",
					bc.Sampled, bc.Decision, bc.Store, c.sampled, c.decision, c.store)
			}

			if bc.DecidedBy != "scan-1" {
				t.Errorf("DecidedBy = %q, want the deciding scan", bc.DecidedBy)
			}
		})
	}
}

// A forced decision is recorded in the ledger, so it counts towards the window.
func TestAForcedDecisionIsRecorded(t *testing.T) {
	t.Parallel()

	l := &fakeLedger{}
	s := scannerWith(l)

	s.decideBodies(WithForcedBodies(t.Context(), model.BodyStoreHashed), "scan-1", bodyTarget(sampling),
		model.ConsentReject, s.deps.Logger)

	if len(l.decided) != 1 || !l.decided[0].Forced {
		t.Errorf("ledger saw %+v, want one forced decision", l.decided)
	}
}

// AC7: a retry inherits its first attempt's decision and asks the ledger
// nothing; a ratio changed by a reload does not re-decide, and a reload that
// turned storage off applies at once.
func TestARetryInheritsTheDecision(t *testing.T) {
	t.Parallel()

	first := &model.BodyCapture{
		Store: model.BodyStoreAll, Sampled: true, Decision: model.BodyDecisionSampled,
		DecidedBy: "attempt-1", WindowScans: 3, WindowSampled: 1,
	}

	l := &fakeLedger{}
	s := scannerWith(l)
	ctx := WithInheritedBodies(t.Context(), first)

	reloaded := sampling
	reloaded.Ratio = 0.5

	bc := s.decideBodies(ctx, "attempt-2", bodyTarget(reloaded), model.ConsentReject, s.deps.Logger)

	if !bc.Sampled || bc.DecidedBy != "attempt-1" || bc.Decision != model.BodyDecisionSampled {
		t.Errorf("retry decision = %+v, want the first attempt's", bc)
	}

	if !bc.Inherited("attempt-2") {
		t.Error("the retry's decision does not read as inherited")
	}

	if len(l.decided) != 0 {
		t.Error("a retry took a decision of its own")
	}

	off := s.decideBodies(ctx, "attempt-2", bodyTarget(config.BodyPolicy{Store: model.BodyStoreNone}),
		model.ConsentReject, s.deps.Logger)

	if off.Sampled || off.Decision != model.BodyDecisionDisabledByReload {
		t.Errorf("after a reload turned storage off the retry is %+v, want storage-disabled-by-reload", off)
	}

	var opts capture.Options
	applyBodyDecision(&opts, off)

	if opts.Bodies != model.BodyStoreNone {
		t.Errorf("capture was asked to store %s after storage was turned off", opts.Bodies)
	}
}

// AC7: an attempt is settled pending while a retry may follow, failed when it
// was the last, completed when it produced a result.
func TestSettleBodies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		attempt     int
		termination model.TerminationReason
		want        string
	}{
		{"a usable result", 1, model.TermIdle, "attempt-1 scan-x completed"},
		{"a failure with a retry to come", 1, model.TermError, "attempt-1 scan-x pending"},
		{"the last attempt failed", 2, model.TermError, "attempt-1 scan-x failed"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			l := &fakeLedger{}
			s := scannerWith(l)
			ctx := WithAttempt(t.Context(), c.attempt, 2, "")

			res := &model.Result{
				ScanID: "scan-x", Termination: c.termination,
				BodyCapture: &model.BodyCapture{Decision: model.BodyDecisionSampled, DecidedBy: "attempt-1"},
			}

			s.settleBodies(ctx, res, bodyTarget(sampling), s.deps.Logger)

			if len(l.settled) != 1 || l.settled[0] != c.want {
				t.Errorf("settled %v, want %q", l.settled, c.want)
			}
		})
	}
}

// Nothing is settled for a decision no ledger row stands for.
func TestSettleSkipsDecisionsWithNoRow(t *testing.T) {
	t.Parallel()

	for _, d := range []model.BodyDecision{model.BodyDecisionDisabled, model.BodyDecisionLedgerUnavailable} {
		l := &fakeLedger{}
		s := scannerWith(l)

		s.settleBodies(t.Context(), &model.Result{BodyCapture: &model.BodyCapture{Decision: d}},
			bodyTarget(sampling), s.deps.Logger)

		if len(l.settled) != 0 {
			t.Errorf("%s was settled", d)
		}
	}
}

// Only a sampled scan stores, and only a sampled scan runs under the body caps.
func TestApplyBodyDecision(t *testing.T) {
	t.Parallel()

	var opts capture.Options

	applyBodyDecision(&opts, &model.BodyCapture{Store: model.BodyStoreAll, Sampled: false, MaxBodyBytes: 5})

	if opts.Bodies != model.BodyStoreNone || opts.MaxBodyBytes != 0 {
		t.Errorf("an unsampled scan runs with %+v", opts)
	}

	applyBodyDecision(&opts, &model.BodyCapture{
		Store: model.BodyStoreAll, Sampled: true, RequestBodies: true, MaxBodyBytes: 5, MaxScanBytes: 50,
	})

	if opts.Bodies != model.BodyStoreAll || !opts.RequestBodies || opts.MaxBodyBytes != 5 || opts.MaxScanBodyBytes != 50 {
		t.Errorf("a sampled scan runs with %+v", opts)
	}
}

// The horizon covers every backoff and every attempt's hard timeout.
func TestRetryHorizon(t *testing.T) {
	t.Parallel()

	target := bodyTarget(sampling)

	if got := retryHorizon(target, model.ConsentReject); got < time.Minute+2*time.Minute {
		t.Errorf("horizon = %s, want at least the backoff plus two hard timeouts", got)
	}
}
