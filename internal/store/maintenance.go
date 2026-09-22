package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// This file gives Prune and Sweep a receipt (Story 4.11).
//
// Story 8.5 already made both operations observable in the moment: their
// stats report what changed and App.pruneOnce already sends them to metrics
// before checking the error. What neither has is memory — a Prometheus
// counter resets on restart and answers "how many, ever", never "what did
// last Tuesday's run do", and a sweep is deliberately not on a timer
// (see Sweep's own doc comment), so its history exists nowhere at all unless
// an operator happened to be watching the log when it ran.
//
// maintenanceRuns records every real run — never a plan, which changed
// nothing and already printed its own answer — in the shape the audit table
// already uses for a self-describing record nothing needs to query by
// column: an id, a timestamp, and the run's own stats as JSON. kind and
// trigger are the two columns a listing needs without decoding it.

// schemaMaintenanceRuns is the schema version that adds the receipt log.
const schemaMaintenanceRuns = 6

// maintenanceRunsTable records every completed Prune and Sweep.
const maintenanceRunsTable = "maintenance_runs"

// Maintenance kinds, as MaintenanceRun.Kind and the read methods take them.
const (
	MaintenanceKindPrune = "prune"
	MaintenanceKindSweep = "sweep"
)

// Maintenance triggers, as the record methods take them.
const (
	TriggerSchedule = "schedule"
	TriggerCLI      = "cli"
)

// maintenanceRunsKept bounds the receipt log itself: the newest N runs of
// each kind survive a write, older ones are trimmed in the same statement. A
// log growing without bound, inside the feature whose purpose is bounding
// growth, would be self-refuting.
const maintenanceRunsKept = 200

// MaintenanceRun is one recorded Prune or Sweep, read back exactly as it was
// written — a receipt that changed when you read it would not be one.
type MaintenanceRun struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Trigger    string    `json:"trigger"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// Error is the run's own error text, empty for a run that succeeded. A
	// run that failed part way is still recorded, with whatever it had
	// already removed: a prune that deleted a thousand results and then lost
	// the database deleted them, and a receipt that said nothing would be
	// the quiet lie Tenet 5 forbids.
	Error string `json:"error,omitempty"`
	// Stats is the operation's own stats struct, encoded as JSON and decoded
	// back into PruneStats or SweepStats by the matching accessor below —
	// never a second, hand-picked set of columns that can drift from what the
	// struct already reports.
	Stats json.RawMessage `json:"stats"`
}

// Duration is how long the run took.
func (r MaintenanceRun) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }

// PruneStats decodes a prune run's own stats. It is meaningless called on a
// sweep run's receipt, which is why the kind is always read alongside it.
func (r MaintenanceRun) PruneStats() (PruneStats, error) {
	var s PruneStats

	err := json.Unmarshal(r.Stats, &s)

	return s, err
}

// SweepStats decodes a sweep run's own stats.
func (r MaintenanceRun) SweepStats() (SweepStats, error) {
	var s SweepStats

	err := json.Unmarshal(r.Stats, &s)

	return s, err
}

// prunedStatsForRecord clears the fields a plan fills and a real run leaves
// empty before a run is recorded: a receipt states what happened, not the
// same list of scan IDs and series plans the caller already has in hand from
// the value Prune returned. Keeping them would make the receipt log grow
// with the size of the history being pruned rather than with the number of
// runs, which is exactly the unbounded growth AC5 exists to prevent.
func prunedStatsForRecord(stats PruneStats) PruneStats {
	stats.Results = nil
	stats.Artifacts = nil
	stats.Plans = nil

	return stats
}

func sweptStatsForRecord(stats SweepStats) SweepStats {
	stats.Artifacts = nil
	stats.RebuildMarkers = nil

	return stats
}

// RecordPruneRun records a completed Prune. It is never called for a plan:
// PlanPrune reports what a prune would do and changes nothing, and a receipt
// for it would be a receipt for something that did not happen.
func (s *SQL) RecordPruneRun(
	ctx context.Context, trigger string, started time.Time, stats PruneStats, runErr error,
) error {
	return s.recordMaintenanceRun(ctx, MaintenanceKindPrune, trigger, started, prunedStatsForRecord(stats), runErr)
}

// RecordSweepRun records a completed Sweep. It is never called for a plan,
// for the same reason RecordPruneRun is not.
func (s *SQL) RecordSweepRun(
	ctx context.Context, trigger string, started time.Time, stats SweepStats, runErr error,
) error {
	return s.recordMaintenanceRun(ctx, MaintenanceKindSweep, trigger, started, sweptStatsForRecord(stats), runErr)
}

// recordMaintenanceRun writes one receipt and trims the log for that kind to
// maintenanceRunsKept, in one transaction: a run and its own effect on the
// log's size are one fact, and a crash between the two must not shrink the
// log without ever having grown it.
func (s *SQL) recordMaintenanceRun(
	ctx context.Context, kind, trigger string, started time.Time, stats any, runErr error,
) error {
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("encoding %s run: %w", kind, err)
	}

	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}

	finished := time.Now()

	return s.retry(ctx, "recording a "+kind+" run", func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		defer func() { _ = tx.Rollback() }()

		const insert = `insert into ` + maintenanceRunsTable +
			` (kind, trigger, started_at, finished_at, error, stats) values (?, ?, ?, ?, ?, ?)`

		if _, err := tx.ExecContext(ctx, s.q(insert),
			kind, trigger, started.UnixNano(), finished.UnixNano(), errText, string(statsJSON)); err != nil {
			return err
		}

		if err := s.trimMaintenanceRunsTx(ctx, tx, kind); err != nil {
			return err
		}

		return tx.Commit()
	})
}

// trimMaintenanceRunsTx deletes every row of kind older than the newest
// maintenanceRunsKept, so the log a dashboard reads back stays bounded
// whatever runs on whatever schedule.
func (s *SQL) trimMaintenanceRunsTx(ctx context.Context, tx *sql.Tx, kind string) error {
	const del = `delete from ` + maintenanceRunsTable + ` where kind = ? and id not in (
		select id from ` + maintenanceRunsTable + ` where kind = ? order by id desc limit ?)`

	_, err := tx.ExecContext(ctx, s.q(del), kind, kind, maintenanceRunsKept)

	return err
}

// maintenanceRunColumns are the columns scanMaintenanceRun reads, in order.
const maintenanceRunColumns = `id, kind, trigger, started_at, finished_at, error, stats`

func scanMaintenanceRun(rows *sql.Rows) (MaintenanceRun, error) {
	var (
		r                 MaintenanceRun
		started, finished int64
		stats             string
	)

	if err := rows.Scan(&r.ID, &r.Kind, &r.Trigger, &started, &finished, &r.Error, &stats); err != nil {
		return MaintenanceRun{}, err
	}

	r.StartedAt = time.Unix(0, started).UTC()
	r.FinishedAt = time.Unix(0, finished).UTC()
	r.Stats = json.RawMessage(stats)

	return r, nil
}

// LastMaintenanceRun reads the newest recorded run of kind, and reports
// whether one exists at all — a store nothing has pruned or swept yet
// reports that as its own fact rather than a zero-valued run that would read
// as one that ran and did nothing (Tenet 5).
func (s *SQL) LastMaintenanceRun(ctx context.Context, kind string) (MaintenanceRun, bool, error) {
	runs, err := s.MaintenanceRuns(ctx, kind, 1)
	if err != nil {
		return MaintenanceRun{}, false, err
	}

	if len(runs) == 0 {
		return MaintenanceRun{}, false, nil
	}

	return runs[0], true, nil
}

// MaintenanceRuns reads runs of kind, newest first, at most limit. A limit of
// zero means every recorded run of that kind (at most maintenanceRunsKept,
// since the log is already trimmed to that on write).
func (s *SQL) MaintenanceRuns(ctx context.Context, kind string, limit int) ([]MaintenanceRun, error) {
	q := `select ` + maintenanceRunColumns + ` from ` + maintenanceRunsTable +
		` where kind = ? order by id desc`

	args := []any{kind}

	if limit > 0 {
		q += limitClause
		args = append(args, limit)
	}

	var out []MaintenanceRun

	err := s.retry(ctx, "listing "+kind+" runs", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			run, err := scanMaintenanceRun(rows)
			if err != nil {
				return err
			}

			out = append(out, run)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s runs: %w", kind, err)
	}

	return out, nil
}
