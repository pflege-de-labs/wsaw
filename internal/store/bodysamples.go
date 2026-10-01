package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The body sampling ledger (Story 1.11, AC4–AC7).
//
// A ratio such as "one scan in twenty keeps its bodies" is honoured over a
// trailing window, and the window has to be counted from something that
// survives a restart: a daemon that restarts more often than every twentieth
// scan would otherwise never sample, or sample on every start. The schedule
// already reads its last run from the store for the same reason, and this is
// the same answer.
//
// The ledger counts observations, not attempts. A retry is the same
// observation tried again (Story 3.8), so it adds no row of its own: it
// appends its scan ID to the row its first attempt wrote. Otherwise a target
// that flaps would inflate its own scan count and dilute its ratio, and a
// retry that succeeded would count one sampled observation twice.

const bodySamplesTable = "body_samples"

// schemaBodySamples is the version that adds the ledger.
const schemaBodySamples = 10

// Body sample outcomes. A row is written pending, and the last attempt of its
// observation settles it.
const (
	// BodySamplePending is an observation whose attempts are not finished: a
	// retry may still follow. A sampled pending row holds its slot, so a
	// retry cannot lose the sample to the next scheduled scan.
	BodySamplePending = "pending"
	// BodySampleCompleted is an observation that produced a result.
	BodySampleCompleted = "completed"
	// BodySampleFailed is an observation every attempt of which failed. It is
	// not counted, so the next scan of the series takes its place.
	BodySampleFailed = "failed"
)

// BodySampleRequest asks the ledger to decide one observation.
type BodySampleRequest struct {
	Target string
	Mode   model.ConsentMode
	// ScanID is the first attempt's scan ID, which names the row.
	ScanID string
	Now    time.Time

	Ratio  float64
	Window time.Duration
	// StaleAfter is how long a pending row may wait for its retry before it
	// is read as failed. Retries cannot outlast a target's interval
	// (Story 3.8, AC9), so a row still pending after that has lost its retry
	// — to a restart, or to a reload that removed the target — and holding
	// its slot would leave the series unsampled for the rest of the window.
	// Zero means pending rows are never stale.
	StaleAfter time.Duration
	// Forced samples the observation whatever the ratio says. It is still
	// recorded, and counts towards the window like any other sampled scan.
	Forced bool
}

// BodySampleDecision is what the ledger decided, with the counts it decided
// from: the series' observations in the window, this one included, and how
// many of them are sampled, this one included if it is.
type BodySampleDecision struct {
	Sampled       bool
	WindowScans   int
	WindowSampled int
}

// DecideBodySample decides whether one observation is sampled, and records the
// decision, in one transaction. Reading the window and claiming a slot are one
// step, so two scans of a series that start together — or two daemons sharing
// a server database — cannot both claim the last sampled slot (AC5).
func (s *SQL) DecideBodySample(ctx context.Context, req BodySampleRequest) (BodySampleDecision, error) {
	if req.ScanID == "" {
		return BodySampleDecision{}, errors.New("deciding a body sample: no scan ID")
	}

	var decision BodySampleDecision

	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()

	err := s.retry(ctx, "deciding a body sample", func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, s.d.ledgerTxOptions())
		if err != nil {
			return err
		}

		defer func() { _ = tx.Rollback() }()

		d, err := s.decideBodySampleTx(ctx, tx, req)
		if err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}

		decision = d

		return nil
	})
	if err != nil {
		return BodySampleDecision{}, fmt.Errorf("deciding a body sample for %s/%s: %w", req.Target, req.Mode, err)
	}

	return decision, nil
}

func (s *SQL) decideBodySampleTx(ctx context.Context, tx *sql.Tx, req BodySampleRequest) (BodySampleDecision, error) {
	now := req.Now.UnixNano()
	from := req.Now.Add(-req.Window).UnixNano()

	staleBefore := int64(0)
	if req.StaleAfter > 0 {
		staleBefore = req.Now.Add(-req.StaleAfter).UnixNano()
	}

	const window = `select id, sampled, outcome, decided_at from ` + bodySamplesTable +
		` where target = ? and consent_mode = ? and decided_at >= ?`

	rows, err := tx.QueryContext(ctx, s.q(window), req.Target, string(req.Mode), from)
	if err != nil {
		return BodySampleDecision{}, err
	}

	var (
		decision BodySampleDecision
		stale    []int64
	)

	for rows.Next() {
		var (
			id, decidedAt int64
			sampled       int
			outcome       string
		)

		if err := rows.Scan(&id, &sampled, &outcome, &decidedAt); err != nil {
			_ = rows.Close()

			return BodySampleDecision{}, err
		}

		if outcome == BodySamplePending && staleBefore > 0 && decidedAt < staleBefore {
			stale = append(stale, id)

			continue
		}

		if outcome == BodySampleFailed {
			continue
		}

		decision.WindowScans++

		if sampled != 0 {
			decision.WindowSampled++
		}
	}

	if err := rows.Close(); err != nil {
		return BodySampleDecision{}, err
	}

	if err := rows.Err(); err != nil {
		return BodySampleDecision{}, err
	}

	// This observation counts towards its own window.
	decision.WindowScans++
	decision.Sampled = req.Forced || model.SampleWanted(req.Ratio, decision.WindowScans, decision.WindowSampled)

	if decision.Sampled {
		decision.WindowSampled++
	}

	const insert = `insert into ` + bodySamplesTable +
		` (target, consent_mode, scan_id, decided_at, sampled, forced, outcome, retry_scan_ids, settled_at)` +
		` values (?, ?, ?, ?, ?, ?, ?, '', 0)`

	if _, err := tx.ExecContext(ctx, s.q(insert),
		req.Target, string(req.Mode), req.ScanID, now,
		boolInt(decision.Sampled), boolInt(req.Forced), BodySamplePending); err != nil {
		return BodySampleDecision{}, err
	}

	// A pending row whose retry never came is settled as failed here, lazily,
	// rather than by a sweep at startup: a sweep could not tell a retry lost
	// to this process's restart from one another daemon still has queued.
	for _, id := range stale {
		const fail = `update ` + bodySamplesTable + ` set outcome = ?, settled_at = ? where id = ? and outcome = ?`
		if _, err := tx.ExecContext(ctx, s.q(fail), BodySampleFailed, now, id, BodySamplePending); err != nil {
			return BodySampleDecision{}, err
		}
	}

	if err := s.trimBodySamplesTx(ctx, tx, req); err != nil {
		return BodySampleDecision{}, err
	}

	return decision, nil
}

// trimBodySamplesTx keeps the ledger bounded. A series keeps two of its
// windows: the one being counted, and one more as margin, so that widening
// ratioWindow does not start the new window empty. Anything older can no
// longer influence a decision.
func (s *SQL) trimBodySamplesTx(ctx context.Context, tx *sql.Tx, req BodySampleRequest) error {
	keep := 2 * req.Window
	if req.StaleAfter > keep {
		keep = req.StaleAfter
	}

	const del = `delete from ` + bodySamplesTable + ` where target = ? and consent_mode = ? and decided_at < ?`

	_, err := tx.ExecContext(ctx, s.q(del), req.Target, string(req.Mode), req.Now.Add(-keep).UnixNano())

	return err
}

// SettleBodySample records what an attempt of an observation produced.
//
// decidedBy names the observation's row: the scan ID of its first attempt.
// scanID is the attempt being settled; when it is a retry, it is appended to
// the row, so the ledger shows which scans one decision covered. outcome is
// pending when a retry is expected to follow.
func (s *SQL) SettleBodySample(ctx context.Context, decidedBy, scanID, outcome string) error {
	switch outcome {
	case BodySamplePending, BodySampleCompleted, BodySampleFailed:
	default:
		return fmt.Errorf("settling a body sample: unknown outcome %q", outcome)
	}

	err := s.retry(ctx, "settling a body sample", func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		defer func() { _ = tx.Rollback() }()

		var retries string

		const read = `select retry_scan_ids from ` + bodySamplesTable + ` where scan_id = ?`

		switch err := tx.QueryRowContext(ctx, s.q(read), decidedBy).Scan(&retries); {
		case errors.Is(err, sql.ErrNoRows):
			// Trimmed, or never written because the ledger was unavailable
			// when the decision was taken. Nothing to settle.
			return nil
		case err != nil:
			return err
		}

		if scanID != "" && scanID != decidedBy && !containsField(retries, scanID) {
			retries = strings.TrimSpace(retries + " " + scanID)
		}

		const update = `update ` + bodySamplesTable +
			` set outcome = ?, retry_scan_ids = ?, settled_at = ? where scan_id = ?`

		if _, err := tx.ExecContext(ctx, s.q(update), outcome, retries, time.Now().UnixNano(), decidedBy); err != nil {
			return err
		}

		return tx.Commit()
	})
	if err != nil {
		return fmt.Errorf("settling the body sample decided by %s: %w", decidedBy, err)
	}

	return nil
}

// BodySample is one ledger row, for tests and for the rebuild.
type BodySample struct {
	Target       string
	Mode         model.ConsentMode
	ScanID       string
	DecidedAt    time.Time
	Sampled      bool
	Forced       bool
	Outcome      string
	RetryScanIDs []string
}

// BodySamples lists a series' ledger rows, oldest first.
func (s *SQL) BodySamples(ctx context.Context, target string, mode model.ConsentMode) ([]BodySample, error) {
	const query = `select scan_id, decided_at, sampled, forced, outcome, retry_scan_ids from ` + bodySamplesTable +
		` where target = ? and consent_mode = ? order by decided_at, id`

	var out []BodySample

	err := s.retry(ctx, "listing body samples", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(query), target, string(mode))
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				b                BodySample
				decidedAt        int64
				sampled, forced  int
				outcome, retries string
			)

			if err := rows.Scan(&b.ScanID, &decidedAt, &sampled, &forced, &outcome, &retries); err != nil {
				return err
			}

			b.Target, b.Mode = target, mode
			b.DecidedAt = time.Unix(0, decidedAt)
			b.Sampled, b.Forced = sampled != 0, forced != 0
			b.Outcome = outcome
			b.RetryScanIDs = strings.Fields(retries)

			out = append(out, b)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing body samples for %s/%s: %w", target, mode, err)
	}

	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

func containsField(list, field string) bool {
	for _, f := range strings.Fields(list) {
		if f == field {
			return true
		}
	}

	return false
}

// rebuildBodySample restores the ledger row a result's recorded decision
// stands for, when the index is rebuilt from the bucket (Story 8.10, and
// Story 1.11, AC5). Every result records the decision it ran under, so the
// ledger is recoverable from the documents alone.
//
// Documents arrive in no particular order, so the merge must not depend on
// one: a retry's scan ID is appended whichever attempt is read first, and a
// completed attempt wins over a failed one, because one success settles the
// observation however many attempts failed before it.
func (s *SQL) rebuildBodySample(ctx context.Context, res *model.Result) error {
	bc := res.BodyCapture
	if bc == nil || bc.DecidedBy == "" {
		return nil
	}

	switch bc.Decision {
	case model.BodyDecisionSampled, model.BodyDecisionNotSampled, model.BodyDecisionForced:
	default:
		// No ledger row was ever written for these: storage was off, or the
		// ledger could not be reached when the decision was taken.
		return nil
	}

	outcome := BodySampleFailed
	if res.OK() {
		outcome = BodySampleCompleted
	}

	retry := ""
	if res.ScanID != bc.DecidedBy {
		retry = res.ScanID
	}

	// A rebuild reads documents on several workers, and two attempts of one
	// observation merge into one row.
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()

	return s.retry(ctx, "rebuilding a body sample", func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, s.d.ledgerTxOptions())
		if err != nil {
			return err
		}

		defer func() { _ = tx.Rollback() }()

		var existing, retries string

		const read = `select outcome, retry_scan_ids from ` + bodySamplesTable + ` where scan_id = ?`

		err = tx.QueryRowContext(ctx, s.q(read), bc.DecidedBy).Scan(&existing, &retries)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			const insert = `insert into ` + bodySamplesTable +
				` (target, consent_mode, scan_id, decided_at, sampled, forced, outcome, retry_scan_ids, settled_at)` +
				` values (?, ?, ?, ?, ?, ?, ?, ?, ?)`

			// The decision was taken as the first attempt started, which a
			// retry's own start time only approximates; the window is hours
			// or days long, so the difference does not move a decision.
			if _, err := tx.ExecContext(ctx, s.q(insert),
				res.Target, string(res.ConsentMode), bc.DecidedBy, res.StartedAt.UnixNano(),
				boolInt(bc.Sampled), boolInt(bc.Forced), outcome, retry, res.FinishedAt.UnixNano()); err != nil {
				return err
			}

		case err != nil:
			return err

		default:
			if retry != "" && !containsField(retries, retry) {
				retries = strings.TrimSpace(retries + " " + retry)
			}

			if existing == BodySampleCompleted {
				outcome = BodySampleCompleted
			}

			const update = `update ` + bodySamplesTable + ` set outcome = ?, retry_scan_ids = ? where scan_id = ?`

			if _, err := tx.ExecContext(ctx, s.q(update), outcome, retries, bc.DecidedBy); err != nil {
				return err
			}
		}

		return tx.Commit()
	})
}
