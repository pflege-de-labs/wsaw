package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is the upgrade of an existing store: every result document that
// was written into a row before Story 8.2 is moved into the artifact bucket,
// referenced from its row, summarised into the columns a listing reads, and
// only then is the column it lived in dropped (Story 8.4).
//
// It is the one part of opening a store that can take minutes. A store that
// has been running for months holds months of evidence, baselines pinned to
// particular scans and share links pointing at results, so the alternative —
// starting again with an empty store — would destroy exactly the compliance
// history the product exists to keep.

// schemaDocumentsInBucket is the schema version that drops the document
// column. The move that has to happen first is not something SQL can express,
// so it runs from prepareMigration; naming the version here keeps the two
// halves of one migration next to each other.
const schemaDocumentsInBucket = 3

// documentBatchSize is how many rows one query of the migration claims.
//
// It bounds the work in progress, not the memory: a batch is a page of row
// keys, and each document is read, written to the bucket and released before
// the next one is touched, so a store with a hundred thousand results costs no
// more memory than a store with ten (AC3). What the number trades is round
// trips against the size of one page — 256 keys is a few tens of kilobytes and
// turns a hundred thousand rows into four hundred queries rather than a
// hundred thousand.
const documentBatchSize = 256

// maxReportedFailures bounds how many scan IDs a failure report names. An
// operator needs identifiers they can act on, and a bucket that has stopped
// accepting writes would otherwise put every scan ID in the store into one
// error message.
const maxReportedFailures = 10

// maxConsecutiveBucketFailures is how many rows in a row may fail to reach the
// bucket before the migration concludes the bucket is gone rather than the rows
// are bad.
//
// A handful is enough, because probeWritable has already established that a
// healthy bucket was the precondition: credentials that expired, a network
// partition, or an endpoint that started returning 5xx do not fail one row and
// then recover for the next. Counting rows rather than time keeps the decision
// independent of how big the documents are.
const maxConsecutiveBucketFailures = 5

// undecodableDocument is what a moved row records when its document is not
// JSON this build can read. The bytes are preserved either way — they are the
// evidence — but the summary cannot be derived from them, and saying so is the
// only honest thing to put in columns that would otherwise read as a scan that
// observed nothing (Tenet 5).
const undecodableDocument = "this result's stored document could not be decoded, so its summary could not be derived"

// DocumentMigration reports the move of stored documents into the artifact
// bucket: what PlanDocumentMigration found, or what an upgrade actually did.
//
// It exists so that an operator does not have to reconstruct a migration from
// log lines. Moving evidence is one-way — an older wsaw cannot read a migrated
// store — so how much moved, where it went, and what was left behind are
// answers the command that ran it has to be able to print (AC4, AC6).
type DocumentMigration struct {
	// Bucket is where the documents go, named as an operator configured it
	// and with any credentials its URL carries removed.
	Bucket string `json:"bucket"`

	// Rows and Bytes are what was found in rows: how many documents are still
	// in the database, and how many bytes they hold. For a plan this is the
	// whole answer — the objects and bytes the bucket is about to receive.
	Rows  int   `json:"rows"`
	Bytes int64 `json:"bytes"`

	// Moved and MovedBytes are what was written. Both stay zero for a plan,
	// which writes nothing.
	//
	// MovedBytes can be larger than what the bucket gains, because artifacts
	// are content-addressed: two identical documents are one object.
	Moved      int   `json:"moved"`
	MovedBytes int64 `json:"movedBytes"`

	// Unsummarised counts documents that were moved but would not decode.
	// Their bytes are safe in the bucket and their rows say why the summary is
	// missing, rather than carrying zeroes that would read as an empty scan.
	Unsummarised int `json:"unsummarised"`

	// Failed counts rows whose document could not be moved at all, and
	// FailedScans names the first few of them (AC4). A failed row keeps its
	// document: the column is not dropped while one is outstanding, so
	// nothing is lost by trying again.
	Failed      int      `json:"failed"`
	FailedScans []string `json:"failedScans,omitempty"`
}

// Pending reports whether there is anything to move.
func (m *DocumentMigration) Pending() bool { return m != nil && m.Rows > 0 }

// note records a row that could not be moved, keeping the first few scan IDs
// so the report can name them.
func (m *DocumentMigration) note(scanID string) {
	m.Failed++

	if len(m.FailedScans) < maxReportedFailures {
		m.FailedScans = append(m.FailedScans, scanID)
	}
}

// DocumentMigration reports what opening this store moved into the artifact
// bucket, or nil where there was nothing to move. It is how the command an
// operator runs an upgrade from prints the outcome rather than asking them to
// read the log.
func (s *Store) DocumentMigration() *DocumentMigration { return s.documents }

// PlanDocumentMigration reports what upgrading this store would move, without
// moving it (AC6).
//
// Deleting nothing and writing nothing is the point: an operator sizing a
// bucket and a maintenance window needs the number of objects and bytes before
// committing to a migration that cannot be undone. It opens the database and
// the bucket exactly as a store does, so a configuration that will not work is
// reported here too — but it applies no schema change, not even the one it is
// reporting on.
func PlanDocumentMigration(ctx context.Context, opts Options) (*DocumentMigration, error) {
	s, err := connect(ctx, opts)
	if err != nil {
		return nil, err
	}

	defer func() {
		// Nothing has been written, so a failure to close cleanly changes
		// nothing about the answer; the caller is being handed a plan, not a
		// store.
		_ = s.bucket.close()
		_ = s.db.Close()
	}()

	return s.planDocuments(ctx)
}

// planDocuments counts the documents still held in rows.
//
// It asks the catalogue whether the column is there rather than reading the
// schema version, because that question is read-only on every dialect and is
// also the true one: a store whose results table has no document column has
// nothing to move, whatever its version record says.
//
// The deadline is applied here rather than left to the caller, because both
// callers reach it from a context that has none: a dry run holds only the
// process's, and the migration holds one that deliberately spans minutes. A
// count against a database that has stopped answering must still fail rather
// than hang the start (Story 8.1, AC6).
func (s *Store) planDocuments(ctx context.Context) (*DocumentMigration, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	m := &DocumentMigration{Bucket: s.bucket.String()}

	present, err := s.d.hasColumn(ctx, s.db, resultsTable, documentColumn)
	if err != nil {
		return nil, fmt.Errorf("looking for the %s column of the %s table: %w",
			documentColumn, resultsTable, err)
	}

	if !present {
		return m, nil
	}

	q := `select count(*), coalesce(sum(` + s.d.documentByteLength() + `), 0)
		from ` + resultsTable + ` where ` + documentColumn + ` <> ''`

	err = s.retry(ctx, "counting stored documents", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(q)).Scan(&m.Rows, &m.Bytes)
	})
	if err != nil {
		return nil, fmt.Errorf("counting the documents still stored in rows: %w", err)
	}

	return m, nil
}

// moveDocumentsToBucket is the half of migration 3 that SQL cannot express.
func (s *Store) moveDocumentsToBucket(ctx context.Context) error {
	return s.moveDocuments(ctx, documentBatchSize)
}

// moveDocuments moves every document still held in a row into the bucket,
// batch by batch.
//
// The batch size is a parameter rather than the constant so that the batching
// itself is testable: crossing a boundary is where a paged migration skips or
// repeats a row, and a test that needs a store of 257 results to reach that
// boundary is a test nobody runs.
func (s *Store) moveDocuments(ctx context.Context, batch int) error {
	m, err := s.planDocuments(ctx)
	if err != nil {
		return err
	}

	if !m.Pending() {
		// Cleared rather than recorded, which is what DocumentMigration()
		// promises when nothing moved. A zero report here would have every
		// freshly created store tell its caller that a migration ran.
		s.documents = nil

		return nil
	}

	s.documents = m

	// Before a single row is touched (AC5). A bucket that is unreachable or
	// read-only fails the upgrade here, while every document is still in the
	// database and nothing has been half-moved.
	if err := s.bucket.probeWritable(ctx); err != nil {
		return fmt.Errorf(
			"the artifact bucket %s must be writable before %d stored result documents can be moved into it: %w",
			s.bucket, m.Rows, err,
		)
	}

	s.log.Info("moving stored result documents into the artifact bucket",
		"bucket", s.bucket.String(), "rows", m.Rows, "bytes", m.Bytes)

	if err := s.moveEveryBatch(ctx, m, batch); err != nil {
		return err
	}

	s.log.Info("stored result documents moved into the artifact bucket",
		"bucket", s.bucket.String(), "moved", m.Moved, "bytes", m.MovedBytes,
		"unsummarised", m.Unsummarised, "failed", m.Failed)

	if m.Failed > 0 {
		// The column is not dropped while a document is still only in it. A
		// migration that carried on here would destroy the evidence it had
		// just failed to copy, which is the one outcome worse than not
		// upgrading (AC4).
		return fmt.Errorf(
			"moving stored result documents into the artifact bucket %s: %d of %d rows could not be moved and were left as they are (%s); "+
				"the %s column is kept until every row has moved, so nothing has been lost: fix the bucket and start wsaw again",
			s.bucket, m.Failed, m.Rows, strings.Join(m.FailedScans, ", "), documentColumn,
		)
	}

	return nil
}

// moveEveryBatch walks the rows still holding a document, a page of keys at a
// time.
//
// Paging is by key rather than by offset because the predicate changes as the
// migration works: a moved row stops matching, so an offset would step over
// rows that had shifted underneath it. Advancing past the last key seen also
// guarantees progress when a row cannot be moved — it is reported, left alone,
// and not met again until the next run.
//
// A run of consecutive bucket failures stops the walk rather than grinding
// through the rest of the table. The probe established that the bucket was
// writable moments earlier, so a bucket that now refuses row after row has
// stopped being reachable — and retrying every remaining row against it would
// spend the store's whole retry policy per row, turning a dead bucket into
// hours of work, a log line per scan, and a failure count the size of the
// store (AC3, AC4).
func (s *Store) moveEveryBatch(ctx context.Context, m *DocumentMigration, batch int) error {
	var (
		after     *resultRowKey
		inARow    int
		lastError error
	)

	for {
		keys, err := s.documentKeys(ctx, after, batch)
		if err != nil {
			return err
		}

		if len(keys) == 0 {
			return nil
		}

		for _, key := range keys {
			failure, err := s.moveDocument(ctx, key, m)
			if err != nil {
				return err
			}

			after = &key

			if failure == nil {
				// Only the bucket's own failures count towards the run. A
				// document that will not decode says nothing about whether the
				// bucket is alive, and one bad row in the middle of a healthy
				// migration must not stop it.
				inARow = 0

				continue
			}

			inARow++
			lastError = failure

			if inARow >= maxConsecutiveBucketFailures {
				return fmt.Errorf(
					"moving stored result documents into the artifact bucket %s: %d rows in a row could not be written, "+
						"so the bucket is treated as unreachable and the migration stopped after %d of %d rows (%s); "+
						"the %s column is kept until every row has moved, so nothing has been lost: fix the bucket and start wsaw again: %w",
					s.bucket, inARow, m.Moved, m.Rows, strings.Join(m.FailedScans, ", "),
					documentColumn, lastError,
				)
			}
		}

		s.log.Info("moving stored result documents",
			"moved", m.Moved, "rows", m.Rows, "bytes", m.MovedBytes, "failed", m.Failed)
	}
}

// resultRowKey identifies one result row: the primary key, and nothing else.
// The migration works from keys rather than from rows so that a document is in
// memory only while it is being moved.
type resultRowKey struct {
	target string
	mode   string
	scanID string
}

func (k resultRowKey) args() []any { return []any{k.target, k.mode, k.scanID} }

// documentKeys reads the next page of rows that still hold a document.
func (s *Store) documentKeys(ctx context.Context, after *resultRowKey, batch int) ([]resultRowKey, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select target, consent_mode, scan_id from ` + resultsTable +
		` where ` + documentColumn + ` <> ''`

	var args []any

	if after != nil {
		// A row-value comparison, the same form PreviousResult uses to page
		// through history, so the three key columns order as one.
		q += ` and (target, consent_mode, scan_id) > (?, ?, ?)`
		args = append(args, after.args()...)
	}

	q += ` order by target, consent_mode, scan_id limit ?`
	args = append(args, batch)

	var keys []resultRowKey

	err := s.retry(ctx, "listing rows that still hold a document", func(ctx context.Context) error {
		keys = nil

		rows, err := s.db.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var key resultRowKey

			if err := rows.Scan(&key.target, &key.mode, &key.scanID); err != nil {
				return err
			}

			keys = append(keys, key)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing the rows that still hold a document: %w", err)
	}

	return keys, nil
}

// moveDocument moves one row's document into the bucket and rewrites the row
// to reference it.
//
// An error is returned only for a failure that makes the rest of the migration
// pointless — the database itself refusing to answer. A document that will not
// move or will not decode is recorded on the report and the walk continues,
// because one unreadable row out of a hundred thousand must not stop the other
// ninety-nine thousand from being migrated (AC4).
//
// A bucket write that failed comes back as the first return value rather than
// as an error, because it is the caller's business how many of them in a row
// mean the bucket itself has gone: one is a bad row, five is a dead bucket.
func (s *Store) moveDocument(
	ctx context.Context,
	key resultRowKey,
	m *DocumentMigration,
) (bucketFailure, err error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	document, err := s.readStoredDocument(ctx, key)
	if err != nil {
		return nil, err
	}

	if document == "" {
		// Nothing to move. The query that selected this key excluded empty
		// documents, so this is a row that emptied underneath the walk; the
		// listing reports such a row as one whose summary could not be
		// derived, which is what it is.
		return nil, nil
	}

	data := []byte(document)

	ref, err := s.putDocument(ctx, data)
	if err != nil {
		m.note(key.scanID)

		s.log.Error("a stored result document could not be moved into the artifact bucket",
			"scan_id", key.scanID, "target", key.target, "consent_mode", key.mode,
			"bucket", s.bucket.String(), "error", err)

		return err, nil
	}

	sum, decoded := summaryFromDocument(data)
	if !decoded {
		m.Unsummarised++

		s.log.Warn("a stored result document was moved but could not be decoded, so no summary was derived from it",
			"scan_id", key.scanID, "target", key.target, "consent_mode", key.mode,
			"artifact", ref.ref)
	}

	if err := s.recordMovedDocument(ctx, key, sum, ref); err != nil {
		return nil, err
	}

	m.Moved++
	m.MovedBytes += ref.size

	return nil, nil
}

// readStoredDocument reads one row's document, and nothing else on the row.
func (s *Store) readStoredDocument(ctx context.Context, key resultRowKey) (string, error) {
	q := `select ` + documentColumn + ` from ` + resultsTable +
		` where target = ? and consent_mode = ? and scan_id = ?`

	var document string

	err := s.retry(ctx, "reading a stored document", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(q), key.args()...).Scan(&document)
	})
	if err != nil {
		return "", fmt.Errorf("reading the stored document of scan %s: %w", key.scanID, err)
	}

	return document, nil
}

// summaryFromDocument derives a row's summary from the document that moved,
// reporting whether the document decoded at all.
//
// The summary comes from summarize(), the same function a scan's own write
// uses, so a migrated row and a freshly written one cannot disagree about what
// a summary is (Story 8.3, AC4).
func summaryFromDocument(document []byte) (Summary, bool) {
	var res model.Result

	if err := json.Unmarshal(document, &res); err != nil {
		// Not a hard failure: the bytes are evidence and they are already in
		// the bucket. What cannot be produced is the derivation, and the row
		// says so instead of carrying zeroes.
		return Summary{Error: undecodableDocument}, false
	}

	return summarize(&res), true
}

// recordMovedDocument points a row at the document that has moved and empties
// the column it came out of.
//
// One statement, so a row can never both name an artifact and hold a copy of
// it: two sources of truth for one document is the failure this epic exists to
// avoid. It is also what makes the migration resumable to the row — an
// interrupted run leaves every row either wholly moved or wholly not (AC2).
//
// The row's start time and termination are deliberately not rewritten. They
// were recorded when the scan was, they are what every listing has been
// ordered by since, and a migration that recomputed them from a document that
// disagreed would silently reorder a target's history. Moving evidence must
// not change the dataset.
func (s *Store) recordMovedDocument(ctx context.Context, key resultRowKey, sum Summary, ref resultRef) error {
	args := append(resultDerivedArgs(sum, ref), documentMovedToBucket)
	args = append(args, key.args()...)

	err := s.retry(ctx, "recording a moved document", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(documentMigrationUpdate()), args...)

		return err
	})
	if err != nil {
		return fmt.Errorf("recording the moved document of scan %s: %w", key.scanID, err)
	}

	return nil
}

// documentMigrationUpdate is built from resultDerived, the same list the
// insert of a new result is built from, so a summary column added later is
// migrated as well as written.
func documentMigrationUpdate() string {
	sets := make([]string, 0, len(resultDerived)+1)

	for _, col := range resultDerived {
		sets = append(sets, col+" = ?")
	}

	sets = append(sets, documentColumn+" = ?")

	return `update ` + resultsTable + ` set ` + strings.Join(sets, ", ") +
		` where target = ? and consent_mode = ? and scan_id = ?`
}
