package scanner

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Body sampling (Story 1.11).
//
// Whether a scan stores its bodies is decided here, before capture starts, so
// capture itself never knows about ratios: an unsampled scan is simply a scan
// with body storage off, and costs what a scan always cost.

// BodyLedger is the sampling ledger a scan decides against. It is the store's,
// and a seam because the scanner is tested without one (Tenet 13).
type BodyLedger interface {
	DecideBodySample(ctx context.Context, req store.BodySampleRequest) (store.BodySampleDecision, error)
	SettleBodySample(ctx context.Context, decidedBy, scanID, outcome string) error
}

// inheritedBodiesKey carries a decision a retry inherits.
type inheritedBodiesKey struct{}

// forcedBodiesKey carries a body storage mode an operator forced.
type forcedBodiesKey struct{}

// WithInheritedBodies hands a retry the sampling decision its first attempt
// took (Story 1.11, AC7). A retry is the same observation tried again, so it
// never takes a decision of its own: a sampled observation stays sampled on
// every attempt, and an unsampled one stays unsampled.
//
// It travels in the context for the reason the attempt does (WithAttempt).
func WithInheritedBodies(ctx context.Context, decision *model.BodyCapture) context.Context {
	if decision == nil {
		return ctx
	}

	return context.WithValue(ctx, inheritedBodiesKey{}, decision)
}

// WithForcedBodies samples a scan whatever its ratio says, storing bodies in
// the given mode (Story 1.11, AC8). It is what `wsaw scan --bodies` sets.
func WithForcedBodies(ctx context.Context, mode model.BodyStore) context.Context {
	if mode == "" || mode == model.BodyStoreNone {
		return ctx
	}

	return context.WithValue(ctx, forcedBodiesKey{}, mode)
}

// InheritedBodies reports the decision a retry inherited, if any. It exists
// for other implementations of the scanning seam — the scheduler's tests among
// them — to see what WithInheritedBodies handed them.
func InheritedBodies(ctx context.Context) *model.BodyCapture { return inheritedBodiesOf(ctx) }

func inheritedBodiesOf(ctx context.Context) *model.BodyCapture {
	d, _ := ctx.Value(inheritedBodiesKey{}).(*model.BodyCapture)

	return d
}

func forcedBodiesOf(ctx context.Context) model.BodyStore {
	m, _ := ctx.Value(forcedBodiesKey{}).(model.BodyStore)

	return m
}

// decideBodies takes, or inherits, the scan's sampling decision.
func (s *Scanner) decideBodies(
	ctx context.Context, scanID string, target config.Resolved, mode model.ConsentMode, log *slog.Logger,
) *model.BodyCapture {
	p := target.Bodies

	bc := &model.BodyCapture{
		Store:         p.Store,
		Ratio:         p.Ratio,
		RatioWindow:   p.RatioWindow,
		RequestBodies: p.RequestBodies,
		MaxBodyBytes:  p.MaxBodyBytes,
		MaxScanBytes:  p.MaxScanBytes,
		DecidedBy:     scanID,
	}

	if inherited := inheritedBodiesOf(ctx); inherited != nil {
		return inheritBodies(bc, inherited, p)
	}

	if forced := forcedBodiesOf(ctx); forced != "" {
		bc.Store = forced
		bc.Sampled, bc.Forced, bc.Decision = true, true, model.BodyDecisionForced

		// Recorded so the forced sample counts towards the window, but the
		// ledger is not needed for it to take effect: an operator who asked
		// for this scan's bodies gets them.
		if d, err := s.decideInLedger(ctx, scanID, target, mode, true); err == nil {
			bc.WindowScans, bc.WindowSampled = d.WindowScans, d.WindowSampled
		} else if s.deps.Ledger != nil {
			log.Warn("recording a forced body sample failed", "error", err)
		}

		return bc
	}

	if !p.Enabled() {
		bc.Decision = model.BodyDecisionDisabled

		return bc
	}

	d, err := s.decideInLedger(ctx, scanID, target, mode, false)
	if err != nil {
		if s.deps.Ledger != nil {
			log.Warn("the body sampling ledger is unavailable", "error", err)
		}

		// Without the ledger only the two ratios that need no history can
		// still be honoured. Anything between is recorded as a gap rather
		// than passed off as an ordinary unsampled scan (Tenet 5).
		switch {
		case p.Ratio >= 1:
			bc.Sampled, bc.Decision = true, model.BodyDecisionSampled
		case p.Ratio <= 0:
			bc.Decision = model.BodyDecisionNotSampled
		default:
			bc.Decision = model.BodyDecisionLedgerUnavailable
		}

		return bc
	}

	bc.Sampled = d.Sampled
	bc.WindowScans, bc.WindowSampled = d.WindowScans, d.WindowSampled

	bc.Decision = model.BodyDecisionNotSampled
	if d.Sampled {
		bc.Decision = model.BodyDecisionSampled
	}

	return bc
}

// inheritBodies applies a first attempt's decision to a retry, under the
// configuration in force now. A changed ratio or window applies from the next
// observation; a reload that turned storage off applies at once, because
// collecting possibly personal data must not wait for a retry (Tenet 19).
func inheritBodies(bc, inherited *model.BodyCapture, p config.BodyPolicy) *model.BodyCapture {
	bc.Sampled, bc.Forced, bc.Decision = inherited.Sampled, inherited.Forced, inherited.Decision
	bc.DecidedBy = inherited.DecidedBy
	bc.WindowScans, bc.WindowSampled = inherited.WindowScans, inherited.WindowSampled

	switch {
	case inherited.Forced:
		// A forced sample ignored the configuration when it was taken, so a
		// reload of that configuration does not undo it: the operator asked
		// for this observation's bodies, in this mode.
		bc.Store = inherited.Store

	case inherited.Sampled && !p.Enabled():
		bc.Sampled, bc.Decision = false, model.BodyDecisionDisabledByReload
	}

	return bc
}

// decideInLedger asks the ledger. The pending row a retry may still need is
// read as failed once it is older than everything the retry policy could
// spend, so a retry lost to a restart releases its slot.
func (s *Scanner) decideInLedger(
	ctx context.Context, scanID string, target config.Resolved, mode model.ConsentMode, forced bool,
) (store.BodySampleDecision, error) {
	if s.deps.Ledger == nil {
		return store.BodySampleDecision{}, errNoLedger
	}

	return s.deps.Ledger.DecideBodySample(ctx, store.BodySampleRequest{
		Target:     target.Name,
		Mode:       mode,
		ScanID:     scanID,
		Now:        time.Now(),
		Ratio:      target.Bodies.Ratio,
		Window:     target.Bodies.RatioWindow,
		StaleAfter: retryHorizon(target, mode),
		Forced:     forced,
	})
}

// retryHorizon is how long an observation's retries can take at most: every
// backoff, and every attempt running to its hard timeout, with a minute of
// margin for the scheduler's tick and the browser pool.
func retryHorizon(target config.Resolved, mode model.ConsentMode) time.Duration {
	attempts := time.Duration(target.Retry.MaxAttempts())

	return target.Retry.TotalDelay(target.Name+"/"+string(mode)) + attempts*target.HardTimeout + time.Minute
}

// settleBodies records what this attempt of the observation produced.
//
// It is settled as pending while another attempt may follow, so a sampled
// observation keeps its slot while its retry is due (Story 1.11, AC7), and as
// failed only when every attempt failed — releasing the slot to the next
// scheduled scan.
func (s *Scanner) settleBodies(ctx context.Context, res *model.Result, target config.Resolved, log *slog.Logger) {
	bc := res.BodyCapture
	if s.deps.Ledger == nil || bc == nil {
		return
	}

	switch bc.Decision {
	case model.BodyDecisionSampled, model.BodyDecisionNotSampled, model.BodyDecisionForced,
		model.BodyDecisionDisabledByReload:
	default:
		return
	}

	outcome := store.BodySampleCompleted

	if target.Retry.Retryable(res) {
		outcome = store.BodySampleFailed
		if attemptOf(ctx).attempt < target.Retry.MaxAttempts() {
			outcome = store.BodySamplePending
		}
	}

	// Settled under its own deadline: a cancelled scan still settles, or its
	// slot would be held until the row goes stale.
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := s.deps.Ledger.SettleBodySample(settleCtx, bc.DecidedBy, res.ScanID, outcome); err != nil {
		log.Warn("settling the body sample failed", "error", err)
	}
}

// mergeBodyCapture adds what capture stored to the decision taken before it.
func mergeBodyCapture(decision, captured *model.BodyCapture) *model.BodyCapture {
	if decision == nil {
		return captured
	}

	if captured != nil {
		decision.ResponseBodiesStored = captured.ResponseBodiesStored
		decision.ResponseBodyBytes = captured.ResponseBodyBytes
		decision.RequestBodiesStored = captured.RequestBodiesStored
		decision.RequestBodyBytes = captured.RequestBodyBytes
		decision.BodiesUnavailable = captured.BodiesUnavailable
		decision.BudgetExhausted = captured.BudgetExhausted
	}

	return decision
}

// errNoLedger is what deciding without a ledger reports.
var errNoLedger = errors.New("no body sampling ledger is configured")
