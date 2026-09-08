package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file records which artifacts each stored result names.
//
// It exists because retention had nothing to go on. Deleting a result used to
// delete a row and leave every object it referenced in the bucket for ever,
// which was a tolerable leak while artifacts were opt-in screenshots and
// bodies and is not one now that every scan's document is out there too
// (Story 8.5).
//
// What makes the deletion decidable is that the references are written down at
// the moment a result is stored, when the decoded result is already in hand.
// The alternative — reading every surviving document back out of the bucket to
// find out what it names — would turn one prune into one object-storage
// request per stored scan, which is the same mistake Story 8.3 refused to make
// for listings (AC2).

// artifactRefsOf lists every artifact a result names: its document, its
// screenshots, and every stored response body.
//
// The document's reference is included even though the results row carries it
// in artifact_ref as well. One table that answers "what does this result name"
// completely is what lets the sweep ask its question once per page of bucket
// keys instead of once per table, and both copies are written from this list
// inside one transaction, so they cannot come to disagree.
//
// References that are not the shape this store writes are dropped rather than
// recorded. A body reference reaches a document through wsaw's own writer, but
// the document is JSON that has been out of the database and back, so treating
// what comes out of it as trusted is the assumption Tenet 9 exists to refuse —
// and a reference that names no object this store could have written can only
// add a row that matches nothing.
//
// The result is sorted and deduplicated, so the same result produces the same
// rows every time it is stored: a scan is content-addressed evidence, and two
// requests that returned identical bytes share one body artifact.
func artifactRefsOf(res *model.Result, documentRef string) []string {
	seen := make(map[string]struct{}, 1+len(res.Screenshots)+len(res.Requests))

	add := func(ref string) {
		if ref == "" || validateRef(ref) != nil {
			return
		}

		seen[ref] = struct{}{}
	}

	add(documentRef)

	for i := range res.Screenshots {
		add(res.Screenshots[i].Ref)
	}

	for i := range res.Requests {
		add(res.Requests[i].BodyRef)
	}

	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}

	slices.Sort(refs)

	return refs
}

// putResultTx writes a result's row and its artifact references together.
//
// One transaction, because the two halves are one fact. A row without its
// references would be a result whose evidence a sweep could not account for,
// and references without their row would keep artifacts alive for a scan that
// is not in the history. The bucket write has already happened outside it
// (Story 8.2, AC7): what is in a bucket cannot be rolled back, and a
// transaction spanning it would hold database locks across a network round
// trip to object storage.
//
// The whole thing is idempotent, which is what lets Store.retry replay it: the
// row is an upsert and the references are replaced wholesale, so storing the
// same result twice leaves exactly one row and one set of references.
func (s *Store) putResultTx(ctx context.Context, key resultRowKey, args []any, refs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storing a result: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		s.q(resultInsert()+s.d.upsert(resultKey, resultUpdate)), args...); err != nil {
		return err
	}

	if err := s.replaceArtifactRefsTx(ctx, tx, key, refs); err != nil {
		return err
	}

	// The claims the scan took while it was running are released here, in the
	// same transaction: from this commit onwards the reference rows are what
	// keep those objects alive, and a claim outliving its purpose would hold
	// an artifact past the retention meant to reclaim it (see claims.go).
	if err := s.releaseClaimsTx(ctx, tx, refs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing a stored result: %w", err)
	}

	return nil
}

// artifactRefInsertChunk is how many references one insert statement carries.
//
// A scan with body storage on can name hundreds of artifacts, and a statement
// per reference would make storing one result hundreds of round trips. The
// chunk keeps the statement — and the placeholder count every driver has a
// ceiling on — bounded whatever the scan captured.
const artifactRefInsertChunk = 64

// replaceArtifactRefsTx rewrites one result's references inside a caller's
// transaction.
//
// Delete then insert, rather than an upsert, because a re-stored result can
// name fewer artifacts than it did before — a retried scan that captured no
// screenshot, say — and references left behind would keep an object alive that
// nothing names any more.
func (s *Store) replaceArtifactRefsTx(ctx context.Context, tx *sql.Tx, key resultRowKey, refs []string) error {
	if _, err := tx.ExecContext(ctx, s.q(`delete from `+resultArtifactsTable+
		` where target = ? and consent_mode = ? and scan_id = ?`), key.args()...); err != nil {
		return fmt.Errorf("clearing the artifact references of scan %s: %w", key.scanID, err)
	}

	for chunk := range slices.Chunk(refs, artifactRefInsertChunk) {
		values := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*4)

		for _, ref := range chunk {
			values = append(values, "(?, ?, ?, ?)")
			args = append(args, key.target, key.mode, key.scanID, ref)
		}

		if _, err := tx.ExecContext(ctx, s.q(`insert into `+resultArtifactsTable+
			` (target, consent_mode, scan_id, artifact_ref) values `+
			strings.Join(values, ", ")), args...); err != nil {
			return fmt.Errorf("recording the artifact references of scan %s: %w", key.scanID, err)
		}
	}

	return nil
}

// querier is the part of *sql.DB and *sql.Tx the reference queries need.
//
// It exists for the dry run, which has to see the world as it would be after a
// prune: it deletes the rows inside a transaction, asks these same queries what
// that orphaned, and then rolls the transaction back. Sharing the queries
// rather than writing a second set is what stops a dry run from reporting
// something other than what the real prune would do (AC6).
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// liveOwners is the set of scans that still have a claim on an artifact.
//
// A baseline is in it as well as a result, because a baseline holds a copy of
// the scan it approved and that copy names the same screenshots and bodies.
// Retention deliberately never prunes a baseline (it is what "expected" means,
// and it must survive the expiry of the history around it), so deleting the
// evidence it names because the result row beside it had aged out would be
// retention destroying exactly what it is meant to preserve. A baseline names
// its scan by the same three columns a result does, so it costs one union and
// no second write path.
const liveOwners = `(select target, consent_mode, scan_id from ` + resultsTable + `
		union
		select target, consent_mode, scan_id from baselines)`

// danglingRefsQuery finds artifacts whose references have outlived the results
// that made them, a page at a time.
//
// A row of result_artifacts is "dangling" when no result and no baseline names
// its scan any more, which is what retention leaves behind when it deletes a
// row. Grouping by the reference answers both questions at once: whether this
// artifact has dangling rows at all, and whether anything still alive names it.
// An artifact with a live owner is kept however many dangling rows point at it
// — that is AC1's shared artifact — and only one with none is deleted.
//
// It is a group-by over the reference table joined to the owners, so it costs a
// pass over the references rather than a bucket listing. That is the trade this
// story is about: a query against an index wsaw already maintains, never a walk
// of somebody else's object store (AC2).
// Counted rather than summed over a CASE, because count() returns an integer
// on all three databases where a sum over a CASE does not: MySQL would answer
// in DECIMAL, which is one more conversion for the driver to get right for no
// gain. count(live.scan_id) counts the rows that matched an owner, count(*)
// counts the group, and their difference is how many references are dangling.
const danglingRefsQuery = `
	select ra.artifact_ref, count(live.scan_id) as still_named
	  from ` + resultArtifactsTable + ` ra
	  left join ` + liveOwners + ` live
	    on live.target       = ra.target
	   and live.consent_mode = ra.consent_mode
	   and live.scan_id      = ra.scan_id
	 where ra.artifact_ref > ?
	 group by ra.artifact_ref
	having count(*) > count(live.scan_id)
	 order by ra.artifact_ref
	 limit ?`

// dropDanglingRefRows removes the reference rows of one artifact whose scans
// are gone, leaving any row whose scan is still stored.
//
// Row-value NOT IN rather than a correlated NOT EXISTS: the same form the
// count-based prune already uses, understood by all three databases, and one
// that does not depend on how each of them treats the table a DELETE targets.
const dropDanglingRefRows = `
	delete from ` + resultArtifactsTable + `
	 where artifact_ref = ?
	   and (target, consent_mode, scan_id) not in (select target, consent_mode, scan_id from ` + resultsTable + `)
	   and (target, consent_mode, scan_id) not in (select target, consent_mode, scan_id from baselines)`

// danglingRef is one artifact whose references have outlived their scans.
type danglingRef struct {
	ref string
	// stillNamed is true when a surviving result or baseline names this
	// artifact too, which is exactly when it must be kept (AC1).
	stillNamed bool
}

// danglingRefs reads the next page of artifacts whose references have outlived
// their scans, after the given reference.
//
// Paging by reference rather than by offset, for the reason the document
// migration pages by key: the predicate changes as the work proceeds — a
// reference whose rows have been dropped stops matching — and an offset would
// step over the rows that shifted underneath it. Advancing past the last
// reference seen also guarantees progress when an artifact cannot be deleted,
// so a bucket that refuses one key cannot turn a sweep into a loop.
func (s *Store) danglingRefs(ctx context.Context, h querier, after string, batch int) ([]danglingRef, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	var out []danglingRef

	err := s.retry(ctx, "listing artifact references without a result", func(ctx context.Context) error {
		out = nil

		rows, err := h.QueryContext(ctx, s.q(danglingRefsQuery), after, batch)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				d          danglingRef
				stillNamed int64
			)

			if err := rows.Scan(&d.ref, &stillNamed); err != nil {
				return err
			}

			d.stillNamed = stillNamed > 0
			out = append(out, d)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing the artifact references whose results are gone: %w", err)
	}

	return out, nil
}

// forgetRef drops the dangling reference rows of one artifact.
//
// It is called only once the artifact itself has been dealt with — deleted, or
// found already absent — so a bucket that refuses a delete leaves its rows in
// place and the next sweep meets the same key again (AC4). The rows are the
// work list, and a work list that is cleared before the work is done is how a
// failed deletion becomes a permanent leak.
func (s *Store) forgetRef(ctx context.Context, h querier, ref string) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	err := s.retry(ctx, "forgetting an artifact reference", func(ctx context.Context) error {
		_, err := h.ExecContext(ctx, s.q(dropDanglingRefRows), ref)

		return err
	})
	if err != nil {
		return fmt.Errorf("forgetting the references to artifact %s: %w", ref, err)
	}

	return nil
}

// referencedRefs reports which of the given references any stored result names.
//
// It is the sweep's question, asked a page of bucket keys at a time so that
// neither the number of keys in the bucket nor the number of references in the
// database has to fit in memory. One statement per page, rather than one per
// key: a bucket holding a hundred thousand objects would otherwise be a hundred
// thousand queries.
func (s *Store) referencedRefs(ctx context.Context, refs []string) (map[string]struct{}, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	named := make(map[string]struct{}, len(refs))

	if len(refs) == 0 {
		return named, nil
	}

	args := make([]any, 0, len(refs))
	for _, ref := range refs {
		args = append(args, ref)
	}

	q := `select distinct artifact_ref from ` + resultArtifactsTable +
		` where artifact_ref in (` + placeholders(len(refs)) + `)`

	err := s.retry(ctx, "checking which artifacts are referenced", func(ctx context.Context) error {
		clear(named)

		rows, err := s.db.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var ref string

			if err := rows.Scan(&ref); err != nil {
				return err
			}

			named[ref] = struct{}{}
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("checking which artifacts are still referenced: %w", err)
	}

	return named, nil
}

// placeholders builds "?, ?, …" for an IN list. Every query in this package is
// written with ? and rewritten per dialect by Store.q, so this one is too.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// resultsWithUnknownRefs counts the stored results whose artifact references
// this store has not been able to work out.
//
// It is the guard on every deletion of a screenshot or a stored body. A row in
// that state came out of a store written by an older wsaw whose document has
// since gone missing or stopped decoding, so what it named cannot be recovered
// — and an artifact that might belong to it must not be collected merely
// because no row says it does. Absence of a reference is not evidence of an
// unreferenced artifact (Tenet 5).
//
// Result documents are exempt from that caution and stay collectable, because a
// row's own document reference is never unknown: the backfill records it from
// the artifact_ref column even for a row whose document it could not read. The
// documents are also the bulk of what the bucket holds, so one unreadable scan
// does not stop retention from reclaiming anything at all.
func (s *Store) resultsWithUnknownRefs(ctx context.Context) (int, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select count(*) from ` + resultsTable + ` where ` + refsIndexedColumn + ` <> ?`

	var n int

	err := s.retry(ctx, "counting results with unknown artifact references", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(q), refsRecorded).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("counting the results whose artifact references are not recorded: %w", err)
	}

	return n, nil
}
