package store

import (
	"context"
	"fmt"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// schemaConfidence is the version that adds the results row's confidence
// columns (Story 5.35).
const schemaConfidence = 9

// confidenceNotComputed is what confidence_score holds for a result whose
// document carries no score, which is every document written before schema
// 2.1. A sentinel rather than NULL because every other results column is NOT
// NULL with a default, in all three dialects; -1 cannot be mistaken for a
// score, which runs from 0 to 100, so it can never render as one.
const confidenceNotComputed = -1

// confidenceArgs binds the columns of resultConfidence.
func confidenceArgs(sum Summary) []any {
	score := confidenceNotComputed
	if sum.ConfidenceScore != nil {
		score = *sum.ConfidenceScore
	}

	return []any{score, string(sum.ConfidenceBand)}
}

// readConfidence sets a summary's confidence from the two columns, leaving it
// unset where the row records none.
func readConfidence(sm *Summary, score int, band string) {
	if score == confidenceNotComputed {
		return
	}

	sm.ConfidenceScore = &score
	sm.ConfidenceBand = model.ConfidenceBand(band)
}

// RecentCleanDurations returns how long the series' last scans before an
// instant took, newest first, at most limit of them — counting only scans that
// ended idle without an error.
//
// Only clean scans count because this is the reference a new scan's duration
// is judged against (Story 5.35, AC5): a run of timeouts or failures folded
// into it would make the next broken scan look normal. It reads one column of
// the index and no document, so a scan pays one small query for it.
func (s *SQL) RecentCleanDurations(
	target string, mode model.ConsentMode, before time.Time, limit int,
) ([]time.Duration, error) {
	ctx, cancel := opCtx()
	defer cancel()

	q := `select duration_ns from results
		where target = ? and consent_mode = ?
		  and termination = ? and scan_error = ''
		  and started_at < ?
		order by started_at desc, scan_id desc` + limitClause

	var out []time.Duration

	err := s.retry(ctx, "reading earlier scan durations", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(q),
			target, string(mode), string(model.TermIdle), before.UnixNano(), limit)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var ns int64
			if err := rows.Scan(&ns); err != nil {
				return err
			}

			out = append(out, time.Duration(ns))
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("reading earlier durations of %s/%s: %w", target, mode, err)
	}

	return out, nil
}
