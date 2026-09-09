package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is the half of Story 8.11's rebuild that knows about rows.
//
// Everything about the bucket — listing the documents, reading them, checking
// them against the content address they are stored under, deriving from them
// and reporting what was found — is in rebuild.go and is the same for every
// store. What is here is the four questions a SQL index answers differently
// from an index made of objects: what it already records, whether one scan is
// in it, how to put one in, and what it holds that no document could produce.
//
// The derivation is deliberately not re-implemented. A row is built by
// resultArgs from summarize(), and its references by artifactRefsOf, which are
// the same two functions PutResult calls — so a rebuilt row and a written one
// are the same row, column for column, and a summary field added later cannot
// be written on a scan and skipped on a rebuild (AC2, Story 8.3 AC4).

// rebuildBucket is where this store's documents and evidence are.
func (s *SQL) rebuildBucket() *bucket { return s.bucket }

// rebuildLog is where a rebuild's progress goes.
func (s *SQL) rebuildLog() *slog.Logger { return s.log }

// beginRebuild marks the bucket as having a rebuild in progress, so that a
// sweep run against a half-rebuilt index collects nothing (AC12).
//
// A SQL store writes the same marker the bucket-index store does, and honours
// it in the same place, because the risk is identical and lives in the bucket
// rather than in either index: between a rebuild starting and finishing, the
// documents of the scans it has not reached yet are referenced by nothing, and
// a sweep would read that as garbage and delete the evidence the rebuild exists
// to recover.
func (s *SQL) beginRebuild(ctx context.Context, run *rebuildRun) (func(), error) {
	return markRebuild(ctx, s.bucket, run)
}

// rebuiltRowColumns are the columns the merge compares a document against, in
// the order rebuiltRow scans them.
//
// They are the summary a listing reads plus where the document is, which is
// exactly the set resultDerivedArgs writes — so "does the index already say
// this" is asked of every column the rebuild would set, and of no column it
// would not.
const rebuiltRowColumns = `started_at, duration_ns, termination, scan_error,
	consent_outcome, consent_cmp, requests, third_party_domains,
	pre_consent_domains, artifact_ref, document_size, document_digest, ` + refsIndexedColumn

// rebuiltRow is what a results row says about one scan, as the merge needs it.
type rebuiltRow struct {
	summary Summary
	ref     resultRef
	// refsIndexed is the refsIndexedColumn state. A row whose references were
	// never worked out is half a record: retention cannot tell what it names,
	// so re-deriving it from the document is a repair rather than a rewrite.
	refsIndexed int
}

// indexedRow reads what this store already records about one scan.
//
// It returns found=false for a scan the index has never heard of, which is the
// ordinary case for the rebuild this command exists for and is not an error.
func (s *SQL) indexedRow(
	ctx context.Context, target string, mode model.ConsentMode, scanID string,
) (rebuiltRow, bool, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select ` + rebuiltRowColumns + ` from ` + resultsTable +
		` where target = ? and consent_mode = ? and scan_id = ?`

	var (
		row   rebuiltRow
		found bool
	)

	err := s.retry(ctx, "reading what the index records about a scan", func(ctx context.Context) error {
		row, found = rebuiltRow{}, false

		var (
			startedAt, durationNS int64
			termination, outcome  string
		)

		err := s.db.QueryRowContext(ctx, s.q(q), target, string(mode), scanID).Scan(
			&startedAt, &durationNS, &termination, &row.summary.Error,
			&outcome, &row.summary.ConsentCMP, &row.summary.Requests,
			&row.summary.ThirdPartyDomains, &row.summary.PreConsentDomains,
			&row.ref.ref, &row.ref.size, &row.ref.digest, &row.refsIndexed,
		)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}

		row.summary.Target = target
		row.summary.ConsentMode = mode
		row.summary.ScanID = scanID
		// UTC and nanoseconds, exactly as scanSummary reads the same two
		// columns: a row records an instant, not the offset the scanning host
		// happened to be in.
		row.summary.StartedAt = time.Unix(0, startedAt).UTC()
		row.summary.Duration = time.Duration(durationNS)
		row.summary.Termination = model.TerminationReason(termination)
		row.summary.ConsentOutcome = model.ConsentOutcome(outcome)

		found = true

		return nil
	})
	if err != nil {
		return rebuiltRow{}, false, fmt.Errorf("reading the index entry for scan %s of %s/%s: %w",
			scanID, target, mode, err)
	}

	return row, found, nil
}

// prunedScan reports whether this index records that the scan was removed.
//
// A SQL index has no tombstone, and it does not need one: what a prune leaves
// behind is the answer. `deleteExpired` deletes the results row and
// **deliberately leaves the result's artifact reference rows in place**, and
// `collectDangling` then drops them one artifact at a time — but only for an
// artifact it actually collected. An artifact a baseline still names is kept
// with its references (liveOwners is that union), a delete the bucket refused
// keeps them too (Story 8.5, AC4), and a prune that was killed between the
// commit and the collection has not reached them yet. So:
//
//	references for this scan, and no row for it  ⇒  this store had the scan
//	                                                and removed it.
//
// Nothing else produces that state. PutResult writes the row and its references
// in one transaction, so an interrupted store leaves neither — which is exactly
// the case a rebuild exists to repair, and it is correctly not this one.
//
// Without the question, a rebuild reads "here is a document and there is no row
// for it" and puts back a result retention deleted, permanently and reported as
// work done; and a verify calls a correctly pruned store drifted for ever,
// which is the scheduled check nobody looks at twice.
func (s *SQL) prunedScan(ctx context.Context, _ *rebuildRun, doc rebuiltDocument) (bool, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select 1 from ` + resultArtifactsTable +
		` where target = ? and consent_mode = ? and scan_id = ? limit 1`

	var pruned bool

	err := s.retry(ctx, "asking whether a scan was pruned", func(ctx context.Context) error {
		pruned = false

		var one int

		err := s.db.QueryRowContext(ctx, s.q(q),
			doc.result.Target, string(doc.result.ConsentMode), doc.result.ScanID).Scan(&one)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}

		pruned = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("asking whether scan %s of %s/%s was pruned: %w",
			doc.result.ScanID, doc.result.Target, doc.result.ConsentMode, err)
	}

	if !pruned {
		return false, nil
	}

	// The references are there. The scan is only pruned if the row is not —
	// the ordinary case is a result that is stored, indexed and intact, and
	// that has references for exactly that reason.
	_, found, err := s.indexedRow(ctx, doc.result.Target, doc.result.ConsentMode, doc.result.ScanID)
	if err != nil {
		return false, err
	}

	return !found, nil
}

// mergeRebuilt records one document in the index, or reports what recording it
// would do.
//
// The write is PutResult's own: the same resultArgs over the same summarize(),
// the same artifactRefsOf, and the same putResultTx that keeps a row and its
// references in one transaction. What is not PutResult's is the bucket write,
// because the document is already in the bucket — that is where this one came
// from.
//
// A scan ID that names a different document is refused rather than resolved.
// Overwriting one of the two would discard a scan, and choosing which is not a
// decision a recovery command may make on its own (AC4).
func (s *SQL) mergeRebuilt(
	ctx context.Context, run *rebuildRun, doc rebuiltDocument,
) (rebuildOutcome, error) {
	sum := summarize(doc.result)

	row, found, err := s.indexedRow(ctx, sum.Target, sum.ConsentMode, sum.ScanID)
	if err != nil {
		return rebuildUnchanged, err
	}

	outcome := sqlMergeOutcome(row, found, sum, doc.ref)
	if outcome == rebuildUnchanged || outcome == rebuildConflict || !run.opts.Mode.writes() {
		return outcome, nil
	}

	if err := s.writeRebuilt(ctx, doc, sum); err != nil {
		return outcome, err
	}

	run.wrote(1)

	return outcome, nil
}

// sqlMergeOutcome decides what one document does to the row that describes it.
func sqlMergeOutcome(row rebuiltRow, found bool, sum Summary, ref resultRef) rebuildOutcome {
	switch {
	case !found:
		return rebuildAdded

	case row.ref.ref != ref.ref:
		return rebuildConflict

	case !summariesAgree(row.summary, sum), row.ref.size != ref.size, row.ref.digest != ref.digest:
		// The document is the record and the columns are a view of it, so a
		// view that no longer matches is re-derived rather than believed
		// (Tenet 4). This is also the repair Story 8.3, AC5 reserved for this
		// command: a change to summarize() is applied to history by running it.
		return rebuildRefreshed

	case row.refsIndexed != refsRecorded:
		// The row is there and what it names is not, which is what an upgrade
		// that could not read a document leaves behind. The document reads now,
		// so the references can be worked out and retention can see them again.
		return rebuildRepaired

	default:
		return rebuildUnchanged
	}
}

// writeRebuilt puts one derived row and its references in, through the write
// path's own transaction.
func (s *SQL) writeRebuilt(ctx context.Context, doc rebuiltDocument, sum Summary) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	key := resultRowKey{target: sum.Target, mode: string(sum.ConsentMode), scanID: sum.ScanID}
	args := resultArgs(sum, doc.ref)
	refs := artifactRefsOf(doc.result, doc.ref.ref)

	err := s.retry(ctx, "rebuilding a result's index entry", func(ctx context.Context) error {
		return s.putResultTx(ctx, key, args, refs)
	})
	if err != nil {
		return fmt.Errorf("rebuilding the index entry for scan %s: %w", sum.ScanID, err)
	}

	return nil
}

// surveyIndex streams the scans this index records and checks that the document
// each one names is still in the bucket.
//
// Keyset paging over the three key columns, not an offset, for the reason the
// document migration pages that way: the rows may be changing underneath, and
// an offset steps over whatever shifted. Here they are changing because the
// daemon may be storing scans while this runs (AC12).
func (s *SQL) surveyIndex(ctx context.Context, run *rebuildRun) error {
	var after *resultRowKey

	for {
		page, err := s.indexedScans(ctx, after, rebuildBatchSize)
		if err != nil {
			return err
		}

		if len(page) == 0 {
			return nil
		}

		run.listed(1)

		if err := run.checkIndexed(ctx, page); err != nil {
			return err
		}

		last := page[len(page)-1]
		after = &resultRowKey{target: last.target, mode: string(last.mode), scanID: last.scanID}
	}
}

// indexedScans reads one page of the scans this index records.
func (s *SQL) indexedScans(ctx context.Context, after *resultRowKey, batch int) ([]indexedScan, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select target, consent_mode, scan_id, artifact_ref from ` + resultsTable

	var args []any

	if after != nil {
		q += ` where (target, consent_mode, scan_id) > (?, ?, ?)`
		args = append(args, after.args()...)
	}

	q += orderByResultKey
	args = append(args, batch)

	var page []indexedScan

	err := s.retry(ctx, "listing the scans this index records", func(ctx context.Context) error {
		page = nil

		rows, err := s.db.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				scan indexedScan
				mode string
			)

			if err := rows.Scan(&scan.target, &mode, &scan.scanID, &scan.document); err != nil {
				return err
			}

			scan.mode = model.ConsentMode(mode)
			page = append(page, scan)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing the scans this index records: %w", err)
	}

	return page, nil
}

// indexOnlyRecords counts what this index holds that no document could produce
// (AC6): the baselines and the audit log.
//
// Both are decisions rather than properties of a scan. A rebuild preserves them
// by never touching them, and where they are gone — a database dropped and
// recreated, a restore of the bucket without its database — it can only say so.
func (s *SQL) indexOnlyRecords(ctx context.Context, _ *rebuildRun) (Unrecoverable, error) {
	baselines, err := s.countRows(ctx, "baselines")
	if err != nil {
		return Unrecoverable{}, err
	}

	entries, err := s.countRows(ctx, "audit")
	if err != nil {
		return Unrecoverable{}, err
	}

	return Unrecoverable{Baselines: baselines, AuditEntries: entries, Counted: true}, nil
}

// countRows counts one table. Unlike hasRows it wants the number, because the
// report says how many decisions survived rather than whether any did.
func (s *SQL) countRows(ctx context.Context, table string) (int, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	var n int

	err := s.retry(ctx, "counting what only the index holds", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(`select count(*) from `+table)).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("counting the rows in %s: %w", table, err)
	}

	return n, nil
}

// indexOnlyDrift has nothing to report for a SQL index.
//
// The drift a store can have on its own is the drift its own shape allows. The
// bucket index can lose a derived audit pointer while keeping the decision it
// points at, because the two are separate objects written one after the other;
// here they are two rows in one transaction, so there is no state where one
// exists without the other for a verify to find (Story 4.6, AC7).
func (s *SQL) indexOnlyDrift(context.Context, *rebuildRun) error { return nil }
