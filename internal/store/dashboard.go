package store

import (
	"context"
	"fmt"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is Story 5.32's own reading of the index: per-series storage
// totals and a month-by-month figure for what is still stored, both answered
// from the results and result_artifacts tables alone — the same tables
// ListResults' own join already reads (Story 5.31, AC2) — never from a bucket
// operation (AC10). Neither method is added to the Store seam: the storage
// dashboard is the one consumer, and it declares the narrow interface it
// needs, the pattern Story 4.11's maintenance-run readers already set.

// SeriesStorage is what one series holds: how many results, what they total,
// and the span they cover.
type SeriesStorage struct {
	Series Series

	Count         int
	DocumentBytes int64
	ArtifactBytes int64

	Oldest, Newest time.Time
}

// TotalBytes is what this series holds together.
func (s SeriesStorage) TotalBytes() int64 { return s.DocumentBytes + s.ArtifactBytes }

// SeriesStorage totals every series' stored bytes in one query, grouped the
// way Series already groups a listing: by target and consent mode.
//
// A pre-Story-5.31 result_artifacts row's bytes default to 0 (see
// references.go and sqlite.go's migration 7), so a series holding only
// results written before that migration reports less than it truly holds
// rather than more — the same direction Tenet 5 already prefers a wrong
// number in, and one AC5's per-scan "size not recorded" figure exists to
// make precise at the history-page level (Story 5.31). This aggregate view
// does not repeat that distinction per series; it is the total the index
// currently knows, stated as exactly that (AC3).
func (s *SQL) SeriesStorage(ctx context.Context) ([]SeriesStorage, error) {
	const q = `select r.target, r.consent_mode, count(*), min(r.started_at), max(r.started_at),
			coalesce(sum(r.document_size), 0), coalesce(sum(ra.bytes), 0)
		  from results r
		  left join ` + resultArtifactsTable + ` ra
		    on ra.target = r.target and ra.consent_mode = r.consent_mode
		   and ra.scan_id = r.scan_id and ra.artifact_ref <> r.artifact_ref
		 group by r.target, r.consent_mode
		 order by r.target, r.consent_mode`

	var out []SeriesStorage

	err := s.retry(ctx, "reading storage by series", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(q))
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				target            string
				mode              string
				count             int
				started, finished int64
				documentBytes     int64
				artifactBytes     int64
			)

			if err := rows.Scan(&target, &mode, &count, &started, &finished,
				&documentBytes, &artifactBytes); err != nil {
				return err
			}

			out = append(out, SeriesStorage{
				Series:        Series{Target: target, Mode: model.ConsentMode(mode)},
				Count:         count,
				DocumentBytes: documentBytes,
				ArtifactBytes: artifactBytes,
				Oldest:        time.Unix(0, started).UTC(),
				Newest:        time.Unix(0, finished).UTC(),
			})
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("reading storage by series: %w", err)
	}

	return out, nil
}

// MonthlyBytes is what the store held in one calendar month, from the scans
// that are still stored — never from a bucket read, and never counting a
// scan retention has since pruned (AC5).
type MonthlyBytes struct {
	// Month is the first instant of the month, UTC.
	Month time.Time
	Bytes int64
}

// MonthlyStorage totals stored bytes by calendar month (UTC) for every scan
// started at or after since, across every series, filling every month in the
// range with zero when nothing currently stored falls in it — a thinned
// month must read as small, not be silently absent from the chart (AC5).
func (s *SQL) MonthlyStorage(ctx context.Context, since time.Time) ([]MonthlyBytes, error) {
	const q = `select r.started_at, r.document_size, coalesce(sum(ra.bytes), 0)
		  from results r
		  left join ` + resultArtifactsTable + ` ra
		    on ra.target = r.target and ra.consent_mode = r.consent_mode
		   and ra.scan_id = r.scan_id and ra.artifact_ref <> r.artifact_ref
		 where r.started_at >= ?
		 group by r.target, r.consent_mode, r.scan_id, r.started_at, r.document_size`

	type monthKey struct{ year, month int }

	buckets := make(map[monthKey]int64)

	err := s.retry(ctx, "reading storage by month", func(ctx context.Context) error {
		for k := range buckets {
			delete(buckets, k)
		}

		rows, err := s.db.QueryContext(ctx, s.q(q), since.UnixNano())
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var startedAt, documentBytes, artifactBytes int64

			if err := rows.Scan(&startedAt, &documentBytes, &artifactBytes); err != nil {
				return err
			}

			t := time.Unix(0, startedAt).UTC()
			buckets[monthKey{t.Year(), int(t.Month())}] += documentBytes + artifactBytes
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("reading storage by month: %w", err)
	}

	var out []MonthlyBytes

	cursor := time.Date(since.Year(), since.Month(), 1, 0, 0, 0, 0, time.UTC)

	for end := time.Now().UTC(); !cursor.After(end); cursor = cursor.AddDate(0, 1, 0) {
		out = append(out, MonthlyBytes{
			Month: cursor,
			Bytes: buckets[monthKey{cursor.Year(), int(cursor.Month())}],
		})
	}

	return out, nil
}
