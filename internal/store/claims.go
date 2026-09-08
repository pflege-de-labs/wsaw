package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// This file records the artifacts wsaw has taken for a scan that has not been
// stored yet (Story 8.5, AC3).
//
// It exists because content addressing makes "when was this object written"
// the wrong question. An artifact is stored under the SHA-256 of its bytes, so
// a scan that captures an unchanged asset stores nothing at all — the key is
// already there from the scan that first saw those bytes, and its modification
// time is that scan's, which may be months old. The new scan nevertheless
// depends on the object: it will name it from the row it commits minutes
// later, and until then nothing in the database says so.
//
// A sweep that reasoned from the object's age alone would collect exactly that
// object, and a prune whose deletion emptied the last reference to it would do
// the same. So the take itself is written down. PutArtifact records a claim,
// the result that lands releases the claims of everything it now references,
// and retention treats a claim younger than the grace period as a reference it
// cannot see yet. What is left behind — a claim whose scan died before it
// committed — ages out and is deleted by the next prune, at which point the
// object it protected is genuinely garbage and the sweep may have it.

// schemaArtifactClaims is the schema version that adds the claim table, and
// with it the document references migration 4 could not derive in SQL.
const schemaArtifactClaims = 5

// claimedAtColumn is when a claim was taken, in nanoseconds, on wsaw's own
// clock rather than the bucket's. Every other instant this store records is
// stored the same way.
const claimedAtColumn = "claimed_at"

// claimArtifact records that wsaw has just taken these bytes for a scan whose
// result is not stored yet.
//
// The write is an upsert, so the claim always says when the most recent take
// happened rather than the first: two scans days apart that capture the same
// unchanged asset each need the grace period counted from their own run.
//
// Its failure fails the artifact write that asked for it. That is deliberate:
// an artifact stored without a claim is an artifact a concurrent sweep may
// collect out from under the scan that is about to reference it, and a store
// that cannot write one small row is a store that is about to refuse the
// result as well. Reporting the storage of evidence as having succeeded when
// the record that protects it did not is the failure Tenet 5 exists to
// prevent.
func (s *Store) claimArtifact(ctx context.Context, ref string, at time.Time) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `insert into ` + artifactClaimsTable + ` (artifact_ref, ` + claimedAtColumn + `) values (?, ?)` +
		s.d.upsert([]string{"artifact_ref"}, []string{claimedAtColumn})

	err := s.retry(ctx, "claiming an artifact", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(q), ref, at.UnixNano())

		return err
	})
	if err != nil {
		return fmt.Errorf("recording that artifact %s is in use: %w", ref, err)
	}

	return nil
}

// releaseClaimsTx drops the claims of the artifacts a result now references,
// inside the transaction that records those references.
//
// One transaction with the reference rows, because the two are one fact: from
// the moment the row is committed the reference is what keeps the object
// alive, and a claim released without its reference landing would leave the
// object unprotected. Doing it the other way round — releasing nothing and
// letting claims age out — would hold every artifact of every scan for a day
// beyond the retention that was meant to reclaim it.
//
// A second scan that took the same bytes and has not committed yet loses its
// claim here, and is protected from then on by the reference this result just
// recorded: the object is named by a stored result, which is the strongest
// claim there is.
func (s *Store) releaseClaimsTx(ctx context.Context, tx *sql.Tx, refs []string) error {
	for chunk := range slices.Chunk(refs, artifactRefInsertChunk) {
		args := make([]any, 0, len(chunk))
		for _, ref := range chunk {
			args = append(args, ref)
		}

		q := `delete from ` + artifactClaimsTable + ` where artifact_ref in (` +
			placeholders(len(chunk)) + `)`

		if _, err := tx.ExecContext(ctx, s.q(q), args...); err != nil {
			return fmt.Errorf("releasing the artifact claims of a stored result: %w", err)
		}
	}

	return nil
}

// claimedSince reports which of refs are claimed by a take no older than
// since.
//
// One statement per page of candidates, like the sweep's reference check and
// for the same reason: a bucket holding a hundred thousand objects must not
// become a hundred thousand queries.
//
// It reads through the caller's handle rather than the store's, because a dry
// run asks this question from inside the transaction it has deleted rows in. A
// second connection would be a reader waiting on that transaction's own write
// lock, which on SQLite is a prune plan blocking until its busy timeout
// expires.
func (s *Store) claimedSince(
	ctx context.Context, h querier, refs []string, since time.Time,
) (map[string]struct{}, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	claimed := make(map[string]struct{}, len(refs))

	if len(refs) == 0 {
		return claimed, nil
	}

	args := make([]any, 0, len(refs)+1)
	args = append(args, since.UnixNano())

	for _, ref := range refs {
		args = append(args, ref)
	}

	q := `select artifact_ref from ` + artifactClaimsTable +
		` where ` + claimedAtColumn + ` >= ? and artifact_ref in (` + placeholders(len(refs)) + `)`

	err := s.retry(ctx, "checking which artifacts a running scan has taken", func(ctx context.Context) error {
		clear(claimed)

		rows, err := h.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var ref string

			if err := rows.Scan(&ref); err != nil {
				return err
			}

			claimed[ref] = struct{}{}
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("checking which artifacts a running scan has taken: %w", err)
	}

	return claimed, nil
}

// forgetStaleClaims deletes the claims of scans that never landed.
//
// A claim older than the grace period protects nothing any more — retention
// stopped honouring it at that moment — so keeping the row would grow one
// table by one row per interrupted artifact write for the life of the store.
// It is a single statement against a table that only ever holds what is in
// flight, so the prune it rides along with pays almost nothing for it.
func (s *Store) forgetStaleClaims(ctx context.Context, before time.Time) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `delete from ` + artifactClaimsTable + ` where ` + claimedAtColumn + ` < ?`

	err := s.retry(ctx, "forgetting stale artifact claims", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(q), before.UnixNano())

		return err
	})
	if err != nil {
		return fmt.Errorf("forgetting the artifact claims of scans that never finished: %w", err)
	}

	return nil
}
