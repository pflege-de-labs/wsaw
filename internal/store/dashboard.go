package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is Story 5.32's own reading of the index: per-series storage
// totals and a month-by-month figure for what is still stored, both answered
// from the index alone — results, result_artifacts and the stored size of
// each object (artifactsizes.go) — never from a bucket operation (AC10).
// Neither method is added to the Store seam: the storage dashboard is the one
// consumer, and it declares the narrow interface it needs, the pattern Story
// 4.11's maintenance-run readers already set.
//
// Two kinds of figure come out of it. DocumentBytes and ArtifactBytes are what
// the results record about themselves — the document's size and each
// screenshot's and body's size before packing, every result charged for
// everything it references (Story 5.31, AC2) — which is what the API has
// always reported under those names. The Stored figures are what the bucket
// occupies: each object counted once however many scans name it, at its
// stored size after packing. Only the second can be held against `du`.

// SeriesStorage is what one series holds: how many results, what they record
// about their own size, what the bucket stores for it, and the span it
// covers.
type SeriesStorage struct {
	Series Series

	Count int

	// DocumentBytes is the series' documents at their recorded size, each
	// result's once; ArtifactBytes its screenshots and bodies at their
	// recorded size, charged to every result that references them.
	DocumentBytes int64
	ArtifactBytes int64

	// StoredDocumentBytes and StoredArtifactBytes are the documents, and the
	// screenshots and bodies, that no other series references, at what they
	// occupy in the bucket: what deleting this series would give back.
	StoredDocumentBytes int64
	StoredArtifactBytes int64

	// StoredSharedBytes is what this series references that another series
	// references too, at its stored size. It is not in StoredTotalBytes,
	// because charging it here would charge it again in every other series
	// that names it.
	StoredSharedBytes int64

	// Unmeasured counts the objects this series references whose stored size
	// the index has not recorded yet; they are in none of the Stored figures.
	Unmeasured int

	Oldest, Newest time.Time
}

// TotalBytes is what this series' results record about their own size.
func (s SeriesStorage) TotalBytes() int64 { return s.DocumentBytes + s.ArtifactBytes }

// StoredTotalBytes is what the bucket stores for this series alone.
func (s SeriesStorage) StoredTotalBytes() int64 { return s.StoredDocumentBytes + s.StoredArtifactBytes }

// StorageReport is what the stored results hold: one row per series, and the
// totals across all of them.
type StorageReport struct {
	Series []SeriesStorage

	// TotalBytes is the sum of every row's TotalBytes.
	TotalBytes int64

	// StoredTotalBytes is the stored size of every object a stored result
	// references, each counted once: the sum of every row's StoredTotalBytes,
	// plus StoredSharedBytes.
	StoredTotalBytes int64

	// StoredSharedBytes is the part of StoredTotalBytes that more than one
	// series references, each object counted once.
	StoredSharedBytes int64

	// Unmeasured counts the referenced objects whose stored size the index
	// has not recorded yet, each counted once. A store written before sizes
	// were recorded has many until its next sweep measures them.
	Unmeasured int
}

// storedObjectsQuery lists every object a stored result references, once per
// series: each result's own document, and every screenshot and body
// result_artifacts records for it. The reference rows are joined to their
// results so that the leftovers of an interrupted prune, which no stored
// result names, are not charged to a series that no longer holds them.
const storedObjectsQuery = `select target, consent_mode, ` + artifactRefColumn + `
		  from ` + resultsTable + ` where ` + artifactRefColumn + ` <> ''
		union
		select ra.target, ra.consent_mode, ra.artifact_ref
		  from ` + resultArtifactsTable + ` ra
		  join ` + resultsTable + ` r
		    on r.target = ra.target and r.consent_mode = ra.consent_mode and r.scan_id = ra.scan_id`

// objectSeriesQuery counts, for every referenced object, how many series
// reference it.
const objectSeriesQuery = `select ` + artifactRefColumn + `, count(*) as series_count
		  from (` + storedObjectsQuery + `) as per_series
		 group by ` + artifactRefColumn

// Storage reports what the bucket holds per series and in total, from the
// index alone.
func (s *SQL) Storage(ctx context.Context) (StorageReport, error) {
	var report StorageReport

	err := s.retry(ctx, "reading storage by series", func(ctx context.Context) error {
		report = StorageReport{}

		series, err := s.seriesSpans(ctx)
		if err != nil {
			return err
		}

		if err := s.seriesRecordedArtifactBytes(ctx, series); err != nil {
			return err
		}

		if err := s.seriesObjectBytes(ctx, series); err != nil {
			return err
		}

		for _, sd := range series {
			report.Series = append(report.Series, *sd)
			report.TotalBytes += sd.TotalBytes()
		}

		slices.SortFunc(report.Series, func(a, b SeriesStorage) int {
			return cmp.Or(strings.Compare(a.Series.Target, b.Series.Target),
				strings.Compare(string(a.Series.Mode), string(b.Series.Mode)))
		})

		return s.storageTotals(ctx, &report)
	})
	if err != nil {
		return StorageReport{}, fmt.Errorf("reading storage by series: %w", err)
	}

	return report, nil
}

// seriesSpans reads how many results each series holds, the span they cover,
// and their documents' recorded size — one row each, so each once.
func (s *SQL) seriesSpans(ctx context.Context) (map[Series]*SeriesStorage, error) {
	const q = `select target, consent_mode, count(*), min(started_at), max(started_at),
			coalesce(sum(document_size), 0)
		  from ` + resultsTable + `
		 group by target, consent_mode`

	rows, err := s.db.QueryContext(ctx, s.q(q))
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	out := make(map[Series]*SeriesStorage)

	for rows.Next() {
		var (
			target, mode      string
			count             int
			started, finished int64
			documents         int64
		)

		if err := rows.Scan(&target, &mode, &count, &started, &finished, &documents); err != nil {
			return nil, err
		}

		key := Series{Target: target, Mode: model.ConsentMode(mode)}
		out[key] = &SeriesStorage{
			Series: key, Count: count, DocumentBytes: documents,
			Oldest: time.Unix(0, started).UTC(), Newest: time.Unix(0, finished).UTC(),
		}
	}

	return out, rows.Err()
}

// seriesRecordedArtifactBytes reads each series' screenshots and bodies at
// their recorded size, charged to every stored result that references them —
// the sum of what ListResults reports per result as ArtifactBytes. Summed
// apart from the documents, because a join of the two would count a
// document once for every artifact its result names.
func (s *SQL) seriesRecordedArtifactBytes(ctx context.Context, series map[Series]*SeriesStorage) error {
	const q = `select ra.target, ra.consent_mode, coalesce(sum(ra.bytes), 0)
		  from ` + resultArtifactsTable + ` ra
		  join ` + resultsTable + ` r
		    on r.target = ra.target and r.consent_mode = ra.consent_mode and r.scan_id = ra.scan_id
		   and ra.artifact_ref <> r.artifact_ref
		 group by ra.target, ra.consent_mode`

	rows, err := s.db.QueryContext(ctx, s.q(q))
	if err != nil {
		return err
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			target, mode string
			artifacts    int64
		)

		if err := rows.Scan(&target, &mode, &artifacts); err != nil {
			return err
		}

		if sd, ok := series[Series{Target: target, Mode: model.ConsentMode(mode)}]; ok {
			sd.ArtifactBytes = artifacts
		}
	}

	return rows.Err()
}

// seriesObjectBytes fills in each series' stored bytes: exclusive documents,
// exclusive screenshots and bodies, shared objects, and unmeasured ones.
func (s *SQL) seriesObjectBytes(ctx context.Context, series map[Series]*SeriesStorage) error {
	const q = `select o.target, o.consent_mode,
			coalesce(sum(case when sh.series_count = 1 and o.artifact_ref like '` + documentRefPrefix + `%'
				then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when sh.series_count = 1 and o.artifact_ref not like '` + documentRefPrefix + `%'
				then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when sh.series_count > 1 then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when z.artifact_ref is null then 1 else 0 end), 0)
		  from (` + storedObjectsQuery + `) as o
		  join (` + objectSeriesQuery + `) as sh on sh.artifact_ref = o.artifact_ref
		  left join ` + artifactSizesTable + ` z on z.artifact_ref = o.artifact_ref
		 group by o.target, o.consent_mode`

	rows, err := s.db.QueryContext(ctx, s.q(q))
	if err != nil {
		return err
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			target, mode                 string
			documents, artifacts, shared int64
			unmeasured                   int
		)

		if err := rows.Scan(&target, &mode, &documents, &artifacts, &shared, &unmeasured); err != nil {
			return err
		}

		sd, ok := series[Series{Target: target, Mode: model.ConsentMode(mode)}]
		if !ok {
			// Every object row is joined to a result, so its series has a
			// span; one without is a result stored between the two reads,
			// and the next snapshot counts it.
			continue
		}

		sd.StoredDocumentBytes, sd.StoredArtifactBytes = documents, artifacts
		sd.StoredSharedBytes, sd.Unmeasured = shared, unmeasured
	}

	return rows.Err()
}

// storageTotals fills in the figures across every series, each object counted
// once.
func (s *SQL) storageTotals(ctx context.Context, report *StorageReport) error {
	const q = `select coalesce(sum(z.stored_bytes), 0),
			coalesce(sum(case when sh.series_count > 1 then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when z.artifact_ref is null then 1 else 0 end), 0)
		  from (` + objectSeriesQuery + `) as sh
		  left join ` + artifactSizesTable + ` z on z.artifact_ref = sh.artifact_ref`

	return s.db.QueryRowContext(ctx, s.q(q)).Scan(&report.StoredTotalBytes, &report.StoredSharedBytes, &report.Unmeasured)
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
//
// Each object is counted once, at its stored size, in the month of the oldest
// stored scan that references it: the month it was first written, as far as
// what is still stored can say. An object shared by scans in two months is in
// the earlier one only, so the months add up to what the bucket holds. An
// object whose stored size is not recorded yet is in no month.
func (s *SQL) MonthlyStorage(ctx context.Context, since time.Time) ([]MonthlyBytes, error) {
	const q = `select min(r.started_at), z.stored_bytes
		  from (
			select target, consent_mode, scan_id, ` + artifactRefColumn + `
			  from ` + resultsTable + ` where ` + artifactRefColumn + ` <> ''
			union
			select target, consent_mode, scan_id, artifact_ref from ` + resultArtifactsTable + `
		  ) as o
		  join ` + resultsTable + ` r
		    on r.target = o.target and r.consent_mode = o.consent_mode and r.scan_id = o.scan_id
		  join ` + artifactSizesTable + ` z on z.artifact_ref = o.artifact_ref
		 group by o.artifact_ref, z.stored_bytes`

	type monthKey struct{ year, month int }

	buckets := make(map[monthKey]int64)

	err := s.retry(ctx, "reading storage by month", func(ctx context.Context) error {
		for k := range buckets {
			delete(buckets, k)
		}

		rows, err := s.db.QueryContext(ctx, s.q(q))
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		first := since.UnixNano()

		for rows.Next() {
			var startedAt, storedBytes int64

			if err := rows.Scan(&startedAt, &storedBytes); err != nil {
				return err
			}

			// Filtered here rather than in the query, because the month is
			// the oldest reference's: an object first stored before since is
			// outside the chart even when a newer scan still names it.
			if startedAt < first {
				continue
			}

			t := time.Unix(0, startedAt).UTC()
			buckets[monthKey{t.Year(), int(t.Month())}] += storedBytes
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
