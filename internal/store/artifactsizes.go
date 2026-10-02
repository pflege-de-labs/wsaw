package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file keeps the index's record of how many bytes each object occupies
// in the bucket: one row per object, keyed by reference (schema version 11).
//
// Nothing else in the index can answer that. results.document_size and
// result_artifacts.bytes are what a result holds before the bucket packs it,
// recorded once per result that names the object, so a sum of them charges a
// body twenty scans share twenty times and a compressed document at its
// inflated size. The storage dashboard is the reader; it must still never
// touch the bucket (Story 5.32, AC10), so the size is recorded where it is
// already known for free: when the store writes an object, and when a sweep
// lists one.
//
// The record is derived data (Tenet 4). A missing row means "not measured
// yet", and the dashboard says so rather than counting it as zero bytes
// (Tenet 5); the next sweep measures it.

// artifactSizesTable is that record.
const artifactSizesTable = "artifact_sizes"

// schemaArtifactSizes is the version that adds it.
const schemaArtifactSizes = 11

// documentRefPrefix is how every document's reference begins, which is how a
// query over objects tells documents from screenshots and bodies.
const documentRefPrefix = artifactKindResult + refSeparator

// artifactSizeColumns are the columns an upsert into artifactSizesTable
// overwrites.
var artifactSizeColumns = []string{"stored_bytes", "measured_at"}

// recordArtifactSize notes the stored size of an object the store has just
// written.
//
// A failure is logged and not returned: the object and the result that names
// it are the evidence, and refusing them because a figure about them could not
// be written would trade evidence for bookkeeping. The object then reads as
// unmeasured until the next sweep measures it.
func (s *SQL) recordArtifactSize(ctx context.Context, ref string, stored int64, at time.Time) {
	if stored < 0 {
		return
	}

	if err := s.upsertArtifactSizes(ctx, []artifactObject{{ref: ref, size: stored}}, at); err != nil {
		s.log.Warn("the stored size of an artifact could not be recorded; the next sweep measures it",
			"artifact", ref, "error", err)
	}
}

// upsertArtifactSizes records the stored size of each object, in one
// statement.
func (s *SQL) upsertArtifactSizes(ctx context.Context, objs []artifactObject, at time.Time) error {
	if len(objs) == 0 {
		return nil
	}

	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `insert into ` + artifactSizesTable + ` (` + artifactRefColumn + `, stored_bytes, measured_at) values ` +
		strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(objs)), ", ") +
		s.d.upsert([]string{artifactRefColumn}, artifactSizeColumns)

	args := make([]any, 0, 3*len(objs))
	for _, obj := range objs {
		args = append(args, obj.ref, obj.size, at.UnixNano())
	}

	err := s.retry(ctx, "recording artifact sizes", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(q), args...)

		return err
	})
	if err != nil {
		return fmt.Errorf("recording the stored size of %d artifact(s): %w", len(objs), err)
	}

	return nil
}

// forgetArtifactSize drops the record of an object the store has deleted.
//
// A failure is logged and not returned, for the same reason as
// recordArtifactSize: the delete has happened. The row it leaves names an
// object no result references, which no figure reads, and the next complete
// sweep drops it (forgetUnlistedSizes).
func (s *SQL) forgetArtifactSize(ctx context.Context, ref string) {
	ctx, cancel := opCtxFrom(context.WithoutCancel(ctx))
	defer cancel()

	q := `delete from ` + artifactSizesTable + ` where ` + artifactRefColumn + ` = ?`

	err := s.retry(ctx, "forgetting an artifact's size", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(q), ref)

		return err
	})
	if err != nil {
		s.log.Warn("the size record of a deleted artifact could not be removed; the next sweep removes it",
			"artifact", ref, "error", err)
	}
}

// forgetUnlistedSizes drops every record no measurement has touched since
// before: after a complete walk of the bucket, those are objects the bucket no
// longer holds. Only a walk that reached every key may call it — a cancelled
// one has not seen the keys it did not reach.
func (s *SQL) forgetUnlistedSizes(ctx context.Context, before time.Time) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `delete from ` + artifactSizesTable + ` where measured_at < ?`

	err := s.retry(ctx, "forgetting the sizes of artifacts the bucket no longer holds", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(q), before.UnixNano())

		return err
	})
	if err != nil {
		return fmt.Errorf("forgetting the sizes of artifacts the bucket no longer holds: %w", err)
	}

	return nil
}

// scanSize is what one stored result occupies in the bucket, as the index
// records it.
type scanSize struct {
	document, artifacts, added int64
	unmeasured                 int
}

// scanSizes reads, for every stored result of a series, what its document and
// its screenshots and bodies occupy in the bucket, what it added that no
// earlier result of the series references, and how many of its objects have
// no recorded size yet.
//
// An object is new in the oldest stored scan that references it. That is
// measured over the whole series rather than over a listed page, because a
// page of the newest fifty scans would otherwise call every unchanged script
// new in the oldest scan it happens to show.
func (s *SQL) scanSizes(ctx context.Context, target string, mode model.ConsentMode) (map[string]scanSize, error) {
	const q = `with o as (
			select ` + scanIDColumn + `, ` + artifactRefColumn + `, started_at
			  from ` + resultsTable + `
			 where target = ? and consent_mode = ? and ` + artifactRefColumn + ` <> ''
			union
			select ra.scan_id, ra.artifact_ref, r.started_at
			  from ` + resultArtifactsTable + ` ra
			  join ` + resultsTable + ` r
			    on r.target = ra.target and r.consent_mode = ra.consent_mode and r.scan_id = ra.scan_id
			 where ra.target = ? and ra.consent_mode = ?
		), f as (
			select ` + artifactRefColumn + `, min(started_at) as first_at from o group by ` + artifactRefColumn + `
		)
		select o.scan_id,
			coalesce(sum(case when o.artifact_ref like '` + documentRefPrefix + `%' then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when o.artifact_ref not like '` + documentRefPrefix + `%' then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when o.started_at = f.first_at then z.stored_bytes else 0 end), 0),
			coalesce(sum(case when z.artifact_ref is null then 1 else 0 end), 0)
		  from o
		  join f on f.artifact_ref = o.artifact_ref
		  left join ` + artifactSizesTable + ` z on z.artifact_ref = o.artifact_ref
		 group by o.scan_id`

	rows, err := s.db.QueryContext(ctx, s.q(q), target, string(mode), target, string(mode))
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	out := make(map[string]scanSize)

	for rows.Next() {
		var (
			scanID string
			sz     scanSize
		)

		if err := rows.Scan(&scanID, &sz.document, &sz.artifacts, &sz.added, &sz.unmeasured); err != nil {
			return nil, err
		}

		out[scanID] = sz
	}

	return out, rows.Err()
}
