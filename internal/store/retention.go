package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is retention: what wsaw stops keeping, and what it removes when it
// stops keeping it (Story 8.5).
//
// Pruning used to delete rows and leave every artifact behind. That was
// survivable while the bucket held opt-in screenshots and bodies; it is not
// now that it holds every scan's document as well, and nothing else in wsaw
// removes anything from it. So a prune deletes the artifacts the results it
// removed alone referenced, keeps the ones a surviving result or baseline
// still names (AC1), and a sweep collects what earlier runs and interrupted
// writes left behind (AC3).
//
// Three rules run through all of it. Deletion is decided by reference and
// never by the age of a key, because artifacts are content-addressed and two
// scans that captured identical bytes share one object. A deletion that fails
// is counted and left for next time rather than allowed to abort retention
// (AC4): the reference rows are the work list, so a key that could not be
// deleted is met again rather than forgotten. And nothing here infers absence
// from a gap in what wsaw knows (Tenet 5) — a bucket that has gone away, an
// index that holds nothing, a result whose references could not be derived and
// an object a running scan has taken are each a reason to keep rather than a
// licence to delete, because a deletion cannot be taken back and a refusal
// costs one more run.

// unreferencedArtifactGrace is how long an artifact that nothing references is
// left alone before a sweep will collect it (AC3).
//
// It exists because a sweep looks at a bucket from outside and cannot see
// intent. An object written moments ago with no reference to it is far more
// likely to be a scan in progress than garbage: artifacts are written to the
// bucket first and referenced from the database second, deliberately (Story
// 8.2, AC4), so every screenshot and every body spends the rest of its scan
// unreferenced, and the document spends the instant between its upload and the
// row's commit that way. Collecting one of those would destroy a scan's
// evidence while the scan was still running.
//
// A day is far longer than that window — a scan is bounded in minutes, retries
// included — and the margin is deliberate. What the excess costs is that
// genuine garbage lives one more day before it is collected, which is a
// rounding error on a bucket's bill. What too short a grace period costs is
// deleted evidence, which is not recoverable at any price. A day also absorbs
// the two things that would otherwise have to be reasoned about: a provider
// whose clock differs from wsaw's, since one of the two timestamps compared
// against it is the bucket's own, and a wsaw that was restarted or paused
// mid-scan.
//
// It is measured against two timestamps, not one, because neither answers
// alone. The object's own write time catches an interrupted write — an object
// that exists and is young. The claim wsaw records when it takes an artifact
// (see claims.go) catches the case the write time cannot: an unchanged asset
// content-addresses to a key that is already there, so nothing is written and
// the object keeps the date of the scan that first stored those bytes, while a
// scan running now is about to reference it.
const unreferencedArtifactGrace = 24 * time.Hour

// artifactRefPageSize is how many artifacts one page of the reference check
// covers. Retention walks its work rather than loading it, so a store with a
// hundred thousand results costs no more memory than one with ten.
const artifactRefPageSize = 256

// Retention bounds how much history is kept.
//
// It is unchanged by Story 8.5 (AC7): this story changes what pruning removes,
// not how an operator asks for it.
type Retention struct {
	// MaxAge drops results older than this. Zero means no age limit.
	MaxAge time.Duration
	// MaxPerSeries keeps at most this many results per target and mode. Zero
	// means no count limit.
	MaxPerSeries int
}

// PrunedResult names one stored result a prune removed, or would remove.
type PrunedResult struct {
	Target      string            `json:"target"`
	ConsentMode model.ConsentMode `json:"consentMode"`
	ScanID      string            `json:"scanId"`
	StartedAt   time.Time         `json:"startedAt"`
}

// PruneStats reports what a prune removed, so retention is observable rather
// than silent. A plan fills the same fields with what a prune would remove.
type PruneStats struct {
	// ResultsDeleted counts rows dropped from the history.
	ResultsDeleted int `json:"resultsDeleted"`

	// ArtifactsDeleted and BytesFreed count what left the bucket: documents,
	// screenshots and stored bodies that no surviving result or baseline
	// referenced any more (AC5).
	ArtifactsDeleted int   `json:"artifactsDeleted"`
	BytesFreed       int64 `json:"bytesFreed"`

	// ArtifactsFailed counts artifacts the bucket would not delete. Their
	// references are kept, so the next sweep meets the same keys rather than
	// leaking them (AC4).
	ArtifactsFailed int `json:"artifactsFailed"`

	// IndexKeysFailed counts index keys a prune could not remove, which is a
	// state only a store whose index is objects in the bucket can be in: a
	// SQL prune deletes its rows in one transaction that either commits or
	// does not.
	//
	// It is its own number rather than part of ArtifactsFailed because the two
	// mean different things to an operator. ArtifactsFailed says some bytes
	// were not reclaimed; this says a result retention has removed from every
	// listing is still fetchable by its scan ID, so a share link to it still
	// resolves. It is normally zero, and a sweep collects what it counts
	// (Story 8.10, §7.3).
	IndexKeysFailed int `json:"indexKeysFailed,omitempty"`

	// ArtifactsProtected counts artifacts left alone on purpose: too recently
	// written to be sure they are garbage, or of a kind that cannot be
	// declared unreferenced while some result's references are unknown.
	ArtifactsProtected int `json:"artifactsProtected"`

	// UnknownReferences counts stored results whose artifacts this store could
	// not work out, which is what protects those kinds. It is normally zero;
	// a non-zero value means some result's document is missing from the bucket
	// or no longer decodes.
	UnknownReferences int `json:"unknownReferences"`

	// Results and Artifacts name what would be removed. They are filled by a
	// plan, which exists so that an operator can see the consequence of a
	// retention setting before it is applied and cannot be undone (AC6). A
	// prune that is actually removing things leaves them empty, because the
	// list would be as long as the work and nothing reads it.
	Results   []PrunedResult `json:"results,omitempty"`
	Artifacts []string       `json:"artifacts,omitempty"`
}

// Prune enforces retention: it drops the results that fall outside it and
// deletes the artifacts they alone referenced.
//
// Baselines are never pruned, and neither is anything a baseline names: a
// baseline holds its own copy of the approved result, so history can expire
// without invalidating the definition of "expected", and deleting the evidence
// that copy points at would undo that (AC1).
//
// It takes a context because it is no longer only a statement or two. Deleting
// what a prune orphaned is one bucket call per artifact, which against object
// storage is a network round trip each, and a daemon shutting down has to be
// able to stop it rather than wait for it.
//
// It needs the bucket to be there, and says so rather than working around it:
// a prune against a bucket that has gone away would delete rows and report
// every artifact as already collected.
func (s *SQL) Prune(ctx context.Context, now time.Time, r Retention) (PruneStats, error) {
	return s.prune(ctx, now, r, false)
}

// PlanPrune reports what Prune would remove, and removes nothing (AC6).
//
// It works by doing the prune inside a transaction and rolling it back. That
// is deliberate rather than clever: the alternative is a second definition of
// "what this would orphan", and a dry run that answers a slightly different
// question from the prune it is describing is worse than no dry run. Nothing
// is written and no artifact is touched — the bucket is only asked how large
// the objects are, and only after the transaction has been rolled back.
func (s *SQL) PlanPrune(ctx context.Context, now time.Time, r Retention) (PruneStats, error) {
	return s.prune(ctx, now, r, true)
}

func (s *SQL) prune(ctx context.Context, now time.Time, r Retention, plan bool) (PruneStats, error) {
	var stats PruneStats

	if r.MaxAge <= 0 && r.MaxPerSeries <= 0 {
		return stats, nil
	}

	// Established before a single row is deleted. The collection half reads
	// "this key is already gone" as "somebody else collected it", which is the
	// right answer for one key and the wrong one for a bucket that has gone
	// away: an unmounted volume answers NotFound for every artifact, and a
	// prune would then report thousands of deletions, forget the reference
	// rows that were the work list, and leave the objects behind for ever once
	// the volume came back. A failed observation must not read as a clean
	// result (Tenet 5), so the bucket is asked whether it is there.
	if err := s.bucket.reachable(ctx); err != nil {
		return stats, fmt.Errorf("pruning needs the artifact bucket: %w", err)
	}

	unknown, err := s.resultsWithUnknownRefs(ctx)
	if err != nil {
		return stats, err
	}

	stats.UnknownReferences = unknown

	if plan {
		return s.planPrune(ctx, now, r, stats)
	}

	if err := s.retry(ctx, "pruning results", func(ctx context.Context) error {
		deleted, err := s.deleteExpiredTx(ctx, now, r)
		// Assigned inside the retry rather than accumulated across attempts: a
		// replayed transaction starts again, and a count that added up every
		// attempt would report rows that were only deleted once.
		stats.ResultsDeleted = deleted

		return err
	}); err != nil {
		return stats, err
	}

	// Outside the transaction, on the same argument that keeps a bucket write
	// out of one (Story 8.2, AC7): a delete against object storage is a network
	// round trip, and holding a database lock across it would make retention a
	// source of contention. The rows the transaction left behind are what say
	// which artifacts to consider, so nothing is lost by committing first.
	if err := s.collectDangling(ctx, s.db, now, &stats); err != nil {
		return stats, err
	}

	// Last, and only once the collection above has had its answer: a claim
	// that is too old to protect anything is a row nothing will read again
	// (see claims.go). Its failure is not the prune's — the rows and the
	// artifacts are already gone — so it is logged rather than returned.
	if err := s.forgetStaleClaims(ctx, now.Add(-unreferencedArtifactGrace)); err != nil {
		s.log.Warn("the artifact claims of scans that never finished could not be cleared",
			"error", err)
	}

	return stats, nil
}

// planPrune answers what a prune would do, from inside a transaction it throws
// away.
func (s *SQL) planPrune(ctx context.Context, now time.Time, r Retention, stats PruneStats) (PruneStats, error) {
	// The transaction takes the caller's context, not a bounded one: it spans
	// as many statements as the plan has pages, the way the document migration
	// spans as many as it has rows. Each statement inside it still gets the
	// deadline every store operation gets, so nothing waits for ever — but a
	// deadline on the whole plan would abort a large one halfway through.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("planning a prune: %w", err)
	}

	// Rolled back on every path, including the one where everything worked:
	// this transaction exists to be discarded.
	defer func() { _ = tx.Rollback() }()

	if err := boundedStep(ctx, func(ctx context.Context) error {
		stats.Results, err = s.expiredResults(ctx, tx, now, r)

		return err
	}); err != nil {
		return stats, err
	}

	if err := boundedStep(ctx, func(ctx context.Context) error {
		stats.ResultsDeleted, err = s.deleteExpired(ctx, tx, now, r)

		return err
	}); err != nil {
		return stats, err
	}

	if stats.Artifacts, stats.ArtifactsProtected, err = s.orphanedRefs(ctx, tx, now, stats.UnknownReferences); err != nil {
		return stats, err
	}

	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return stats, fmt.Errorf("discarding a prune plan: %w", err)
	}

	// Measured only now, with the transaction gone: asking the bucket how big
	// an object is means a request per artifact against object storage, and
	// doing that with a write transaction open would hold the database's locks
	// for the length of a network conversation.
	if stats.ArtifactsDeleted, stats.BytesFreed, err = s.measureArtifacts(ctx, stats.Artifacts); err != nil {
		return stats, err
	}

	return stats, nil
}

// boundedStep runs one statement of a longer operation under the deadline
// every store operation gets, inside a transaction that outlives it.
//
// It exists because a plan holds one transaction across several statements and
// several pages: bounding the whole thing would cut a large plan short, and
// bounding nothing would let a database that has stopped answering hang the
// command. This is the same split the document migration makes.
func boundedStep(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return fn(ctx)
}

// deleteExpiredTx deletes the results retention no longer keeps, in one
// transaction so that a series is never left half pruned.
func (s *SQL) deleteExpiredTx(ctx context.Context, now time.Time, r Retention) (int, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("pruning results: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	deleted, err := s.deleteExpired(ctx, tx, now, r)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing prune: %w", err)
	}

	return deleted, nil
}

// deleteExpired applies both retention limits and reports how many rows went.
//
// The reference rows those results own are deliberately left in place. They are
// what the collection step reads to work out which artifacts have just lost
// their last owner, and leaving them means an interrupted prune resumes rather
// than forgetting what it was about to delete (AC3, AC4).
func (s *SQL) deleteExpired(ctx context.Context, h querier, now time.Time, r Retention) (int, error) {
	var deleted int

	if r.MaxAge > 0 {
		res, err := h.ExecContext(ctx,
			s.q(`delete from `+resultsTable+expiredByAgePredicate), now.Add(-r.MaxAge).UnixNano())
		if err != nil {
			return 0, fmt.Errorf("pruning results by age: %w", err)
		}

		if n, err := res.RowsAffected(); err == nil {
			deleted += int(n)
		}
	}

	if r.MaxPerSeries > 0 {
		// Ranked within each series by the same ordering every listing uses,
		// so "the newest N" means the same thing here as it does there. The
		// statement itself is the dialect's: MySQL refuses to select from the
		// table a delete targets, so it needs a different shape.
		res, err := h.ExecContext(ctx, s.q(s.d.pruneByCount()), r.MaxPerSeries)
		if err != nil {
			return 0, fmt.Errorf("pruning results by count: %w", err)
		}

		if n, err := res.RowsAffected(); err == nil {
			deleted += int(n)
		}
	}

	return deleted, nil
}

// expiredResults lists the results retention would remove, for a plan.
//
// Both queries are built from the same predicates the deletes use, so what a
// dry run names and what a prune removes cannot describe different sets. A
// result can match both limits, so the two lists are merged on the row's
// identity rather than concatenated.
func (s *SQL) expiredResults(ctx context.Context, h querier, now time.Time, r Retention) ([]PrunedResult, error) {
	seen := make(map[resultRowKey]struct{})

	var out []PrunedResult

	collect := func(query string, arg any) error {
		rows, err := h.QueryContext(ctx, s.q(query), arg)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				pr        PrunedResult
				mode      string
				startedAt int64
			)

			if err := rows.Scan(&pr.Target, &mode, &pr.ScanID, &startedAt); err != nil {
				return err
			}

			key := resultRowKey{target: pr.Target, mode: mode, scanID: pr.ScanID}
			if _, dup := seen[key]; dup {
				continue
			}

			seen[key] = struct{}{}

			pr.ConsentMode = model.ConsentMode(mode)
			pr.StartedAt = time.Unix(0, startedAt).UTC()
			out = append(out, pr)
		}

		return rows.Err()
	}

	if r.MaxAge > 0 {
		if err := collect(expiredByAge, now.Add(-r.MaxAge).UnixNano()); err != nil {
			return nil, fmt.Errorf("listing the results an age limit would remove: %w", err)
		}
	}

	if r.MaxPerSeries > 0 {
		if err := collect(expiredByCount, r.MaxPerSeries); err != nil {
			return nil, fmt.Errorf("listing the results a count limit would remove: %w", err)
		}
	}

	return out, nil
}

// collectDangling deletes the artifacts whose last owner has gone, and forgets
// the references of those that are still named by something alive.
//
// It is the shared engine of a prune's second half and a sweep's first half.
// A prune reaches it having just deleted rows; a sweep reaches it to finish
// whatever an earlier prune could not (AC3), which is why the same walk serves
// both and why a failed delete simply waits here for the next run.
func (s *SQL) collectDangling(ctx context.Context, h querier, now time.Time, stats *PruneStats) error {
	after := ""

	for {
		page, err := s.danglingRefs(ctx, h, after, artifactRefPageSize)
		if err != nil {
			return err
		}

		if len(page) == 0 {
			return nil
		}

		// One query for the whole page, before anything is deleted: which of
		// these keys a scan that is still running has taken (AC3).
		claimed, err := s.claimedSince(ctx, h, danglingRefKeys(page), now.Add(-unreferencedArtifactGrace))
		if err != nil {
			return err
		}

		for _, d := range page {
			after = d.ref

			if err := ctx.Err(); err != nil {
				return fmt.Errorf("collecting unreferenced artifacts: %w", err)
			}

			if err := s.collectOne(ctx, h, d, claimed, stats); err != nil {
				return err
			}
		}
	}
}

// danglingRefKeys is one page's references, for the queries that take a page
// at a time.
func danglingRefKeys(page []danglingRef) []string {
	refs := make([]string, 0, len(page))
	for _, d := range page {
		refs = append(refs, d.ref)
	}

	return refs
}

// collectOne decides the fate of one artifact whose references have outlived
// their scans.
func (s *SQL) collectOne(
	ctx context.Context, h querier, d danglingRef, claimed map[string]struct{}, stats *PruneStats,
) error {
	if d.stillNamed {
		// A surviving result or baseline names the same bytes — the shared
		// artifact AC1 is about. Only the stale references go.
		return s.forgetRef(ctx, h, d.ref)
	}

	if _, taken := claimed[d.ref]; taken {
		// A scan that is running has stored these bytes and has not committed
		// the row that names them. That is the case content addressing makes
		// invisible from the bucket: an unchanged asset stores nothing, so the
		// object still carries the date of the scan that first saw it while a
		// live scan depends on it (AC3). The reference rows are left in place,
		// so the next prune considers the key again.
		stats.ArtifactsProtected++

		return nil
	}

	if !collectable(d.ref, stats.UnknownReferences) {
		stats.ArtifactsProtected++

		return nil
	}

	size, deleted := s.removeArtifact(ctx, d.ref)
	if !deleted {
		// Left exactly as it is, references included, so the next sweep finds
		// it again (AC4). Retention has still done its job: the rows are gone.
		stats.ArtifactsFailed++

		return nil
	}

	stats.ArtifactsDeleted++
	stats.BytesFreed += size

	return s.forgetRef(ctx, h, d.ref)
}

// removeArtifact deletes one artifact and reports how many bytes that
// reclaimed.
//
// The size is read before the delete because the store does not record how
// large a screenshot or a stored body is — only the document's size is on a row
// — and "bytes freed" that were never measured would be a number an operator
// cannot use (AC5). It costs one extra request per artifact actually deleted,
// which is small beside the delete it accompanies and is only paid for
// artifacts that are going.
//
// A key that is already gone counts as done rather than failed: something else
// collected it, which is the outcome this method wanted.
func (s *SQL) removeArtifact(ctx context.Context, ref string) (bytes int64, deleted bool) {
	return reclaimArtifact(ctx, s.bucket, s.log, ref)
}

// reclaimArtifact is removeArtifact for whichever kind of store is pruning.
//
// It is a function of the bucket and a logger rather than a method, because
// both stores reach this point with the same question and must answer it the
// same way: the bucket-index store's prune decides which artifacts have lost
// their last owner from index objects instead of from rows, and everything
// after that decision — measure, delete, count the bytes, treat an absent key
// as done — is the policy of Story 8.5 and not of a store kind. Two copies
// would be two policies, and the one that drifted would be the one nobody was
// reading.
func reclaimArtifact(ctx context.Context, b *bucket, log *slog.Logger, ref string) (bytes int64, deleted bool) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	obj, err := b.stat(ctx, ref)

	switch {
	case errors.Is(err, ErrNotFound):
		return 0, true

	case err != nil:
		log.Warn("an unreferenced artifact could not be measured, so it was left for the next sweep",
			"artifact", ref, "bucket", b.String(), "error", err)

		return 0, false
	}

	if !dropArtifact(ctx, b, log, ref) {
		return 0, false
	}

	return obj.size, true
}

// deleteArtifact removes one object and reports whether it is gone, without
// asking how large it was.
//
// It is separate because the sweep's walk of the bucket already knows: a
// listing reports every key's size, so measuring one again would be a request
// spent on a number already in hand.
//
// A failure is logged rather than returned. A bucket that refuses one delete
// must not abort retention: the rows are already gone, the rest of the
// artifacts are still worth reclaiming, and the key stays referenced so the
// next sweep meets it again (AC4).
func (s *SQL) deleteArtifact(ctx context.Context, ref string) bool {
	return dropArtifact(ctx, s.bucket, s.log, ref)
}

// dropArtifact is deleteArtifact for whichever kind of store is collecting; see
// reclaimArtifact for why it is shared.
func dropArtifact(ctx context.Context, b *bucket, log *slog.Logger, ref string) bool {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	switch err := b.remove(ctx, ref); {
	case err == nil, errors.Is(err, ErrNotFound):
		return true

	default:
		log.Warn("an unreferenced artifact could not be deleted and was left for the next sweep",
			"artifact", ref, "bucket", b.String(), "error", err)

		return false
	}
}

// orphanedRefs lists the artifacts a plan would delete, without touching the
// bucket, and reports how many were protected instead.
func (s *SQL) orphanedRefs(
	ctx context.Context, h querier, now time.Time, unknownRefs int,
) (refs []string, protected int, err error) {
	after := ""

	for {
		page, err := s.danglingRefs(ctx, h, after, artifactRefPageSize)
		if err != nil {
			return nil, 0, err
		}

		if len(page) == 0 {
			return refs, protected, nil
		}

		// The same question the prune asks, so a dry run cannot promise a
		// deletion the prune would refuse.
		claimed, err := s.claimedSince(ctx, h, danglingRefKeys(page), now.Add(-unreferencedArtifactGrace))
		if err != nil {
			return nil, 0, err
		}

		for _, d := range page {
			after = d.ref

			_, taken := claimed[d.ref]

			switch {
			case d.stillNamed:
			case taken, !collectable(d.ref, unknownRefs):
				protected++
			default:
				refs = append(refs, d.ref)
			}
		}
	}
}

// measureArtifacts asks the bucket how large each artifact is, so a plan can
// report the bytes a prune would reclaim.
//
// An artifact whose size cannot be read is still counted, at zero bytes, and
// the reason is logged: a plan that dropped it from the list would understate
// what is about to be deleted, which is the one direction a dry run must not
// err in.
//
// A cancelled walk is reported as the failure it is, for the same reason. The
// list of keys is complete by the time this runs, so returning the bytes
// measured so far would print a reclaim figure an order of magnitude below the
// listing beside it — a half-measured plan presented as a plan.
func (s *SQL) measureArtifacts(ctx context.Context, refs []string) (count int, bytes int64, err error) {
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return count, bytes, fmt.Errorf("measuring what a prune would reclaim: %w", err)
		}

		size, err := s.statArtifact(ctx, ref)
		if err != nil {
			s.log.Debug("an artifact a prune would delete could not be measured",
				"artifact", ref, "bucket", s.bucket.String(), "error", err)
		}

		count++
		bytes += size
	}

	return count, bytes, nil
}

// statArtifact reads one artifact's size under its own deadline.
func (s *SQL) statArtifact(ctx context.Context, ref string) (int64, error) {
	return measureArtifact(ctx, s.bucket, ref)
}

// measureArtifact reads one artifact's size under its own deadline, for
// whichever kind of store is planning a deletion.
func measureArtifact(ctx context.Context, b *bucket, ref string) (int64, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	obj, err := b.stat(ctx, ref)

	return obj.size, err
}

// collectable reports whether an artifact may be deleted at all, given what
// this store knows about which results name what.
//
// While any stored result's references are unknown, a screenshot or a stored
// body that no row names might still belong to it, and deleting it would be
// inferring absence from a gap in wsaw's own index — the failure Tenet 5 exists
// to prevent.
//
// Result documents stay collectable regardless, because a row's own document
// reference is never unknown. That is what schema version 5 established: it
// records every existing row's artifact_ref as a reference in SQL, before any
// document is read, so a row waiting for the backfill — or one the backfill
// gave up on — still names its own document. Without that statement this
// exemption would be a hole rather than a distinction, and a sweep run while
// the backfill was incomplete would collect the documents of live results.
func collectable(ref string, unknownRefs int) bool {
	if unknownRefs == 0 {
		return true
	}

	return strings.HasPrefix(ref, artifactKindResult+refSeparator)
}

// SweepStats reports what a sweep looked at and what it reclaimed.
type SweepStats struct {
	// ArtifactsScanned and BytesScanned are what the bucket holds under
	// wsaw's prefixes — the number an operator wants next to what was freed,
	// because "we deleted 12 objects" means nothing without it.
	ArtifactsScanned int   `json:"artifactsScanned"`
	BytesScanned     int64 `json:"bytesScanned"`

	ArtifactsDeleted int   `json:"artifactsDeleted"`
	BytesFreed       int64 `json:"bytesFreed"`

	// ArtifactsProtected counts objects that were left alone: written or
	// claimed too recently to be called garbage (AC3), or of a kind no sweep
	// may collect while some result's references are unknown.
	ArtifactsProtected int `json:"artifactsProtected"`

	// ForeignObjects counts keys in the bucket that are not the shape this
	// store writes, which a sweep neither deletes nor counts against itself.
	//
	// They are their own number because the alternative was worse in both
	// directions. Attempting to delete them fails — the bucket seam refuses a
	// reference it did not write — so every stray object would be reported as
	// a delete the bucket refused, on every run, and ArtifactsFailed would
	// stop meaning what it says (AC4). Deleting them would be worse still: a
	// sweep would be reaching outside what wsaw wrote.
	ForeignObjects int `json:"foreignObjects"`

	ArtifactsFailed   int `json:"artifactsFailed"`
	UnknownReferences int `json:"unknownReferences"`

	// ResultsWithoutEntry counts result documents the bucket holds that no
	// index entry points at.
	//
	// They are left in place rather than collected, and they are counted
	// rather than passed over in silence, because a result document is
	// self-describing: it decodes to the scan it records, so it is a
	// candidate for a rebuild of the index (Story 8.11) and not garbage. A
	// non-zero value means an interrupted write or an index that is behind
	// the bucket, which is Story 8.10, AC6's "a document visible without its
	// index entry is not silently lost". Screenshots and stored bodies are
	// not self-describing and keep the ordinary grace-based collection.
	ResultsWithoutEntry int `json:"resultsWithoutEntry,omitempty"`

	// RebuildInProgress counts the markers a rebuild of the index leaves while
	// it runs, and is non-zero only for a store whose index is objects in the
	// bucket (Story 8.10, §7.4).
	//
	// It is its own number rather than part of UnknownReferences, which is
	// where an earlier version of the blob sweep put it. The two say opposite
	// things about the evidence: UnknownReferences says a stored result's
	// document is missing or no longer decodes, which is an integrity problem
	// an operator has to go and look at, while this says the index is half
	// built and the sweep therefore judged nothing — everything is where it
	// was, and the answer is to let the rebuild finish. Reporting the second
	// as the first tells an operator their evidence is damaged when nothing at
	// all is wrong.
	RebuildInProgress int `json:"rebuildInProgress,omitempty"`

	// Artifacts names what a plan would delete. A sweep that is deleting
	// leaves it empty.
	Artifacts []string `json:"artifacts,omitempty"`
}

// SweepOptions is what an operator has to say out loud before a sweep will do
// something irreversible on a store that cannot prove what its bucket holds.
type SweepOptions struct {
	// AllowEmptyIndex lets a sweep walk the bucket for a store whose index
	// holds no results and no baselines at all.
	//
	// It is off by default because "no result references this object" and "the
	// index that would say so is gone" look identical from inside a sweep, and
	// the second one is not hypothetical: a restored database without its
	// bucket, a store pointed at the wrong bucket, or a fresh store opened
	// against an existing one produce exactly it — and a sweep would then
	// delete every document, screenshot and body in the bucket and report
	// success. Rebuilding an index from the bucket is Story 8.11 and does not
	// exist yet, so there is no way back (Tenet 5).
	//
	// An operator who really does have a bucket full of garbage and an empty
	// history says so, and the sweep proceeds.
	AllowEmptyIndex bool
}

// Sweep collects artifacts that nothing references (AC3).
//
// It exists because two things leak past a prune. An interrupted write leaves
// an object whose row was never committed — the deliberate consequence of
// writing the bucket first (Story 8.2, AC4) — and a delete the bucket refused
// leaves a key a prune counted and moved on from. Neither is visible from the
// database alone, so this is the one operation that walks the bucket.
//
// It is safe to run while scans are running. An object nothing references but
// that was written, or claimed by a running scan, within the grace period is
// left alone rather than collected, because that is exactly what a scan in
// progress looks like from out here.
//
// It refuses to walk the bucket for a store that holds no scans at all, unless
// asked to in as many words: see SweepOptions.
//
// It is not on a timer. Walking a bucket is a listing of every key wsaw owns,
// which against object storage is a request per page and a line on an invoice,
// so it is something an operator asks for.
func (s *SQL) Sweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error) {
	return s.sweep(ctx, now, opts, false)
}

// PlanSweep reports what Sweep would collect, and collects nothing (AC6).
func (s *SQL) PlanSweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error) {
	return s.sweep(ctx, now, opts, true)
}

// ErrEmptyIndex refuses a sweep of a bucket that the store holds no index for.
//
// A sweep decides by reference, and a store that holds no scan and has never
// recorded a reference or a claim references nothing — so every object in the
// bucket looks like garbage, including a full history whose database was
// restored without it. The sentinel is exported so the command that offers the
// override can name it.
var ErrEmptyIndex = errors.New(
	"this store's index holds nothing at all, so every artifact in the bucket looks unreferenced",
)

func (s *SQL) sweep(ctx context.Context, now time.Time, opts SweepOptions, plan bool) (SweepStats, error) {
	var stats SweepStats

	// The same reason the prune establishes it: the collection path reads a
	// missing key as a key somebody else collected, which is true of one
	// object and false of a bucket that has gone away (Tenet 5).
	if err := s.bucket.reachable(ctx); err != nil {
		return stats, fmt.Errorf("sweeping needs the artifact bucket: %w", err)
	}

	unknown, err := s.resultsWithUnknownRefs(ctx)
	if err != nil {
		return stats, err
	}

	stats.UnknownReferences = unknown

	// Asked before anything is collected, because collecting changes the
	// answer: the reference half below empties the very rows that establish
	// that this store and this bucket belong together.
	if err := s.indexCanJudgeTheBucket(ctx, opts); err != nil {
		return stats, err
	}

	// First the references that outlived their results: the leftovers of a
	// prune that was interrupted or whose deletes the bucket refused. They are
	// answered from the database, so they cost nothing to check and they make
	// the walk that follows see fewer keys as unreferenced.
	if err := s.sweepDangling(ctx, now, &stats, plan); err != nil {
		return stats, err
	}

	if err := s.sweepBucket(ctx, now, &stats, plan); err != nil {
		return stats, err
	}

	return stats, nil
}

// indexCanJudgeTheBucket refuses a sweep when the index has nothing to judge
// the bucket with.
//
// What is at stake is only the walk of the bucket — the reference half works
// from rows that exist and cannot mistake an absent index for absent evidence
// — but an index that holds nothing holds no such rows either, so refusing the
// whole sweep costs nothing and refusing it before any collection happens is
// what makes the answer stable.
func (s *SQL) indexCanJudgeTheBucket(ctx context.Context, opts SweepOptions) error {
	if opts.AllowEmptyIndex {
		return nil
	}

	empty, err := s.indexIsEmpty(ctx)
	if err != nil {
		return err
	}

	if empty {
		return fmt.Errorf("%w: if the bucket really does hold nothing but garbage, sweep it with the override; "+
			"otherwise the index this store should hold is missing, and deleting on that basis is not recoverable",
			ErrEmptyIndex)
	}

	return nil
}

// indexIsEmpty reports whether this store's index says anything at all about
// any artifact.
//
// Four tables, because each is on its own enough to establish that this store
// and this bucket belong together. A result or a baseline names evidence
// directly. A reference row without its result is what a prune that could not
// finish leaves behind, and it is precisely a record of an object in this
// bucket. A claim is a note that this store wrote an artifact there. Only a
// store where all four are empty knows nothing — and that is the state a
// restored database, a wrong bucket or a fresh store opened against somebody
// else's evidence is in.
func (s *SQL) indexIsEmpty(ctx context.Context) (bool, error) {
	for _, table := range []string{resultsTable, "baselines", resultArtifactsTable, artifactClaimsTable} {
		present, err := s.hasRows(ctx, table)
		if err != nil {
			return false, err
		}

		if present {
			return false, nil
		}
	}

	return true, nil
}

// hasRows reports whether a table holds anything, without counting it. A
// count(*) over a year of history is a scan on some databases, and the
// question here is only whether the table is empty.
func (s *SQL) hasRows(ctx context.Context, table string) (bool, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select count(*) from (select 1 from ` + table + ` limit 1) as sample`

	var n int

	err := s.retry(ctx, "checking whether the store holds any scan", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(q)).Scan(&n)
	})
	if err != nil {
		return false, fmt.Errorf("checking whether %s holds any row: %w", table, err)
	}

	return n > 0, nil
}

// sweepDangling runs the reference-side half of a sweep and folds its counts
// into the sweep's own.
func (s *SQL) sweepDangling(ctx context.Context, now time.Time, stats *SweepStats, plan bool) error {
	prune := PruneStats{UnknownReferences: stats.UnknownReferences}

	if plan {
		refs, protected, err := s.orphanedRefs(ctx, s.db, now, stats.UnknownReferences)
		if err != nil {
			return err
		}

		count, bytes, err := s.measureArtifacts(ctx, refs)
		if err != nil {
			return err
		}

		stats.Artifacts = append(stats.Artifacts, refs...)
		stats.ArtifactsDeleted += count
		stats.BytesFreed += bytes
		stats.ArtifactsProtected += protected

		return nil
	}

	if err := s.collectDangling(ctx, s.db, now, &prune); err != nil {
		return err
	}

	stats.ArtifactsDeleted += prune.ArtifactsDeleted
	stats.BytesFreed += prune.BytesFreed
	stats.ArtifactsFailed += prune.ArtifactsFailed
	stats.ArtifactsProtected += prune.ArtifactsProtected

	return nil
}

// sweepBucket walks every key wsaw owns and collects the ones no result names.
//
// The keys are checked a page at a time rather than one by one: a bucket
// holding every document of every scan has more keys than a query per key
// would be reasonable for, and more than a sweep should hold in memory at once.
//
// The walk starts at the root of the bucket, because that is where wsaw's own
// kinds are, and every key it meets is put through the same shape test the
// bucket seam applies to a reference. A key that fails it was not written by
// this store — a leftover from another tool, a provider's placeholder, another
// deployment's layout — and is counted apart and left alone: a sweep may only
// collect what wsaw wrote (AC3, AC4).
func (s *SQL) sweepBucket(ctx context.Context, now time.Time, stats *SweepStats, plan bool) error {
	cutoff := now.Add(-unreferencedArtifactGrace)
	page := make([]artifactObject, 0, artifactRefPageSize)

	flush := func() error {
		if len(page) == 0 {
			return nil
		}

		err := s.collectUnreferenced(ctx, now, page, stats, plan)
		page = page[:0]

		return err
	}

	err := s.bucket.list(ctx, "", func(obj artifactObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !isArtifactRef(obj.ref) {
			// Not counted as scanned either: ArtifactsScanned is what wsaw
			// holds in the bucket, and this is not wsaw's.
			stats.ForeignObjects++

			s.log.Debug("an object in the artifact bucket was not written by wsaw, so the sweep left it alone",
				"key", truncateForMessage(obj.ref), "bucket", s.bucket.String())

			return nil
		}

		stats.ArtifactsScanned++
		stats.BytesScanned += obj.size

		if obj.modTime.After(cutoff) {
			// Too young to be called garbage: this is what an artifact of a
			// scan that has not finished writing its row looks like (AC3).
			stats.ArtifactsProtected++

			return nil
		}

		page = append(page, obj)

		if len(page) < artifactRefPageSize {
			return nil
		}

		return flush()
	})
	if err != nil {
		return fmt.Errorf("sweeping artifact bucket %s: %w", s.bucket, err)
	}

	return flush()
}

// collectUnreferenced deletes the objects in one page that no stored result
// names.
func (s *SQL) collectUnreferenced(
	ctx context.Context, now time.Time, page []artifactObject, stats *SweepStats, plan bool,
) error {
	refs := make([]string, 0, len(page))
	for _, obj := range page {
		refs = append(refs, obj.ref)
	}

	named, err := s.referencedRefs(ctx, refs)
	if err != nil {
		return err
	}

	// Two questions per page, both answered from the index: which of these
	// keys a stored result names, and which a scan that is still running has
	// taken. The second is what the object's own age cannot answer — a scan
	// that captured an unchanged asset wrote nothing, so the key it depends on
	// carries the date of the scan that first stored those bytes (AC3).
	claimed, err := s.claimedSince(ctx, s.db, refs, now.Add(-unreferencedArtifactGrace))
	if err != nil {
		return err
	}

	for _, obj := range page {
		if _, referenced := named[obj.ref]; referenced {
			continue
		}

		if _, taken := claimed[obj.ref]; taken {
			stats.ArtifactsProtected++

			continue
		}

		if !collectable(obj.ref, stats.UnknownReferences) {
			stats.ArtifactsProtected++

			continue
		}

		if plan {
			stats.Artifacts = append(stats.Artifacts, obj.ref)
			stats.ArtifactsDeleted++
			// The listing already reported the size, so a plan of the bucket
			// half needs no request of its own.
			stats.BytesFreed += obj.size

			continue
		}

		if !s.deleteArtifact(ctx, obj.ref) {
			stats.ArtifactsFailed++

			continue
		}

		stats.ArtifactsDeleted++
		stats.BytesFreed += obj.size
	}

	return nil
}
