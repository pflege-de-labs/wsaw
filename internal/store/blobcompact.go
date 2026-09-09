package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// This file is how a series directory stops growing without bound (Story 8.10,
// AC11): a compaction folds the older loose entries of one target's history
// into a single checkpoint object, and a later run deletes the loose keys that
// checkpoint has taken over.
//
// Six rules hold it up, and each of them is a rule because the alternative is a
// history that loses a scan.
//
//  1. **A checkpoint is a new immutable key and nothing is edited.** The key
//     carries the digest of the body, so re-running an interrupted compaction
//     over the same visible set writes the identical bytes to the identical
//     key and changes nothing (I4, AC10). There is no object anywhere in this
//     design that a second compaction has to update.
//
//  2. **Checkpoints are unioned from a full prefix listing, never chained.**
//     The reader (unionCheckpoints) reads every checkpoint one listing shows
//     and takes the union. A chain is a graph, two concurrent compactors fork
//     it, and a fork strands every entry in the orphaned branch the moment its
//     loose keys are deleted. A union cannot fork: the worst two compactors do
//     is write two checkpoints that overlap, and the reader deduplicates by
//     scan ID.
//
//  3. **A loose key is deleted only by membership in a re-observed
//     checkpoint** — never by key range, and never before that checkpoint has
//     been durably visible for Options.CheckpointGrace. Invariant I3, in full,
//     is implemented by collectCovered and each half of it is argued there.
//
//  4. **A partial run leaves a readable series and the next run finishes the
//     job.** Every step is a create of a key derived from its content or a
//     delete of a key some checkpoint already holds, so stopping anywhere —
//     between the checkpoint and the deletes, or half way through the deletes
//     — leaves a state the fold answers correctly, from which running again
//     completes the work.
//
//  5. **Two compactors racing produce a correct index rather than a corrupt
//     one.** They see the same set and write one object, or they see different
//     sets and write two that overlap. Neither can delete a key the other
//     still needs, because deletion asks the bucket what it can see now rather
//     than remembering what this process wrote.
//
//  6. **A reader gets the same answer from the checkpoint plus what follows it
//     as it would have got from the entries alone.** That is the property the
//     other five exist to protect, and it is asserted directly rather than
//     inferred (see the equivalence test in blob_lag_test.go).
//
// **What is not here, and why.** §7.2 of the design has compaction fold the
// audit prefix as well, and this build does not. Three reasons, and the third
// is the one that decides it. An audit key is <inv>.<stamp>.<did> where <did>
// hashes the body *and a nonce*, so unlike an entry key it is not recomputable
// from the body it names: an audit checkpoint would have to carry key/body
// pairs, which is a different object shape from the one §7.2 specifies rather
// than an implementation of it. freeAuditInstant decides where a new entry is
// filed by listing the loose keys of one nanosecond, so deleting loose audit
// keys silently breaks the ordering the shared suite requires of two entries
// recorded at one instant. And the volume is not there to justify either
// change: an audit object is written per baseline decision and per action
// recorded from outside the store, which is the same human-generated volume
// §7.2 gives as its own reason for leaving baseline/ alone, and Audit() stops
// paging as soon as it has the entries the caller asked for — so a long log
// costs one listing whether it is compacted or not. noAuditCheckpoints still
// refuses a read that would be short, so the day an audit compaction lands it
// cannot land half way. Recorded in the design's §8.5.

const (
	// compactAfter is how many loose entry keys one series directory may hold
	// before a compaction folds the older ones away.
	//
	// A thousand is one listing page, which is the number that matters here:
	// below it every fold of the directory is one request however long the
	// history behind it, and above it a fold the first page does not satisfy
	// starts paying a request per further thousand keys. It is a reasoned
	// choice and not a measurement, it is a constant and not configuration
	// because an operator has no basis on which to tune it, and the design
	// records tuning it as work for the soak run of Story 8.9.
	compactAfter = 1000

	// compactKeep is how many of the newest entries are left loose.
	//
	// It is what keeps a checkpoint off the hot path. foldKeys opens a
	// checkpoint only when the loose keys did not satisfy the answer, so while
	// the newest two hundred scans are loose, a page render, a LatestResult
	// and a comparison all cost the listing and the bodies they would have
	// cost with no compaction at all. Two hundred rather than fifty because
	// PreviousResult skips the scans that observed nothing: two hundred
	// consecutive failures in front of the scan a comparison wants is a
	// deployment in trouble, and one hundred and ninety-nine is not.
	compactKeep = 200

	// checkpointMaxEntries and checkpointMaxBytes cap one checkpoint object,
	// so that a series which somehow arrived with an enormous backlog cannot
	// turn one compaction into a single object nothing wants to read.
	//
	// Neither binds under the constants above — a run folds at most
	// compactAfter entries, and a thousand entries of about 450 bytes is
	// roughly 450 KiB — and that is the point of them: they are the guard that
	// keeps it that way if compactAfter is ever raised, checked from the sizes
	// the listing already reported and therefore before a single body is read.
	checkpointMaxEntries = 20000
	checkpointMaxBytes   = 4 << 20

	// checkpointKind is what a checkpoint body says it is, written by the
	// compaction and checked by the reader.
	//
	// It is a byte of self-description on an object whose absence of one would
	// be expensive: a body of some other shape unmarshals into a checkpoint
	// with no entries at all, and a reader that trusted it would report a
	// history that is short rather than a history it cannot read (Tenet 5).
	checkpointKind = "checkpoint"
)

// tombstoneGrace is how long a tombstone must have been in the bucket before a
// compaction may collect it.
//
// A week, and deliberately far longer than the day a checkpoint waits. A
// tombstone is the one object in this index whose job is to contradict another
// one: it hides an entry key a prune deleted, and the state it protects against
// is a bucket that reported a delete it did not perform, or a listing that goes
// on showing a key that is gone (see the prune's write order in §7.3). Deleting
// it early does not lose an entry — it brings a deleted one back, which for a
// store whose purpose is to answer "what did we hold" is the worse direction.
// The object is one zero-byte key, so a week of patience costs nothing.
const tombstoneGrace = 7 * 24 * time.Hour

// --- the schedule ----------------------------------------------------------

// seriesWrites counts the results this process has stored, per series, and is
// the whole of compaction's schedule: there is no ticker, no goroutine and
// nothing to wire into shutdown (AGENTS §4).
//
// A series is due the first time this process writes to it and every
// compactAfter writes after that. The first-write rule is the part that makes
// the counter useful rather than theoretical: at one scan an hour a series
// takes six weeks to reach a thousand results, so a counter that only ever
// counted would mean a daemon restarted every fortnight never compacted
// anything. Making a restart the thing that re-arms compaction, rather than the
// thing that postpones it, costs one listing per series per process.
//
// The map grows with the number of distinct series this process writes to,
// which is the number of targets it scans — the same order as the target list
// itself, and an int per entry.
type seriesWrites struct {
	mu     sync.Mutex
	counts map[seriesID]int
}

// newSeriesWrites starts a process's compaction schedule.
func newSeriesWrites() *seriesWrites {
	return &seriesWrites{counts: make(map[seriesID]int)}
}

// due records one stored result and reports whether its series should be
// considered for compaction now.
func (w *seriesWrites) due(id seriesID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	written, seen := w.counts[id]
	if !seen || written+1 >= compactAfter {
		w.counts[id] = 0

		return true
	}

	w.counts[id] = written + 1

	return false
}

// considerCompaction runs a compaction pass over one series if it is due.
//
// It is called at the end of PutResult, after the scan is recorded and durable,
// and its failure is never the write's: a compaction that could not finish has
// changed no answer, and reporting a stored scan as unstored because the
// housekeeping after it failed would be the lie Tenet 5 is about.
//
// **Deletion happens inside the call that stored a result, and AC3 says no
// index object is deleted "as part of an ordinary write".** The tension is
// real and is worth naming rather than glossing. What AC3 buys is that two
// writers racing cannot lose an update, and that comes from the key derivation
// — nothing here rewrites or edits anything. Compaction is a separate pass that
// this call happens to schedule: PutResult's own writes are complete before it
// starts, every key it deletes is one a durably visible checkpoint already
// holds the body of, and an interrupted compaction leaves the history exactly
// as readable as an uninterrupted one. The alternative the design weighed and
// rejected (§7.2) was hanging compaction off Prune, which never runs at all in
// a deployment with retention switched off — and an index that grows without
// bound because nobody set a retention policy is the failure AC11 exists to
// prevent.
func (s *Blob) considerCompaction(ctx context.Context, series blobSeries) {
	if !s.writes.due(series.id) {
		return
	}

	stats, err := s.compactSeries(ctx, series, s.now())
	if err != nil {
		s.log.Warn("compacting a target's history did not finish, and will be attempted again",
			"target", series.target, "consent_mode", string(series.mode), "error", err)

		return
	}

	if stats.quiet() {
		return
	}

	// Info because it is rare — once per thousand scans of one target — and
	// because it is keys moving underneath a history nobody asked to have
	// moved, which is exactly what an operator watching this store has to be
	// able to see (Tenet 8).
	s.log.Info("compacted a target's history in the bucket index",
		"target", series.target, "consent_mode", string(series.mode),
		"entries_folded", stats.folded, "entry_keys_deleted", stats.entries,
		"tombstones_deleted", stats.tombstones)
}

// --- one pass --------------------------------------------------------------

// compacted is what one pass did.
type compacted struct {
	folded     int
	entries    int
	tombstones int
}

// quiet reports that the pass found nothing to do, which is the ordinary case
// and the one that must not produce a log line.
func (c compacted) quiet() bool { return c.folded == 0 && c.entries == 0 && c.tombstones == 0 }

// compactSeries is one compaction pass over one series directory.
//
// One listing, then two halves that are deliberately never the same run's work:
// the write half folds loose entries into a new checkpoint, and the delete half
// collects the loose keys that some *earlier* checkpoint has already carried
// for longer than the grace. The checkpoint this run writes is not consulted by
// this run's deletes, which is the grace period expressed as control flow
// rather than as an arithmetic that could be got wrong — a checkpoint written a
// moment ago cannot be assumed visible to any other reader, so nothing it
// covers may go yet.
//
// now is passed in rather than read from the store so that the one
// time-dependent decision in this design has a single value for the whole pass.
// The ages it is compared against are the bucket's own ModTimes: there is one
// clock deciding how old an object is, the provider's, and a ModTime this
// process cannot make sense of disables a deletion rather than enabling one.
func (s *Blob) compactSeries(ctx context.Context, series blobSeries, now time.Time) (compacted, error) {
	var stats compacted

	dir, err := s.readSeriesDirectory(ctx, series)
	if err != nil {
		return stats, err
	}

	cover, err := s.coverOf(ctx, series, dir, now)
	if err != nil {
		return stats, err
	}

	folded, err := s.foldEntries(ctx, series, dir, cover)
	if err != nil {
		return stats, err
	}

	stats.folded = folded

	stats.entries, stats.tombstones, err = s.collectCovered(ctx, series, dir, cover, now)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

// --- what the bucket currently shows ---------------------------------------

// seriesObject is one key a compaction's listing reported, with the two facts
// the bucket adds to it.
//
// modTime is the clock every deletion in this file is decided against, and size
// is what lets the byte cap on a checkpoint be applied before any body is read.
type seriesObject struct {
	record  seriesRecord
	modTime time.Time
	size    int64
}

// key is where this object lives.
func (o seriesObject) key() string { return o.record.String() }

// seriesDirectory is one full listing of one series directory, split by kind.
//
// The whole directory and not one page, because both halves of a compaction
// need to be sure of what is there: the delete half must not act on a partial
// view of the checkpoints, and the tombstone rule asks whether any loose entry
// for a scan is visible, which a truncated listing cannot answer. It is the
// same unbounded read the retention walk already makes of the same directory,
// and it is bounded in practice by the compaction this function performs.
type seriesDirectory struct {
	tombstones  []seriesObject
	checkpoints []seriesObject

	// entries are the loose entry keys, in the order the listing produced
	// them, which is newest first.
	entries []seriesObject
}

// readSeriesDirectory lists one series directory whole.
func (s *Blob) readSeriesDirectory(ctx context.Context, series blobSeries) (seriesDirectory, error) {
	var (
		dir    seriesDirectory
		cursor indexCursor
	)

	for cursor.more() {
		page, err := s.listIndex(ctx, series.id.dirPrefix(), cursor, indexListPageSize)
		if err != nil {
			return seriesDirectory{}, fmt.Errorf("listing the history of %s to compact it: %w", series, err)
		}

		cursor = page.next

		for _, object := range page.objects {
			record, ok := s.seriesRecordOf(series, object)
			if !ok {
				continue
			}

			listed := seriesObject{record: record, modTime: object.modTime, size: object.size}

			switch record.kind {
			case seriesTombstone:
				dir.tombstones = append(dir.tombstones, listed)
			case seriesCheckpoint:
				dir.checkpoints = append(dir.checkpoints, listed)
			case seriesEntry:
				dir.entries = append(dir.entries, listed)
			}
		}
	}

	return dir, nil
}

// looseScans is every scan this listing still shows a loose entry key for.
//
// It is the tombstone rule's first question, and it is asked of the listing
// this run took rather than of the state the run has produced: a tombstone
// whose entry key this very pass deleted keeps its tombstone for one more
// round, which is a round of patience and not a leak.
func (d seriesDirectory) looseScans() map[encodedScan]struct{} {
	out := make(map[encodedScan]struct{}, len(d.entries))

	for _, entry := range d.entries {
		out[entry.record.scan] = struct{}{}
	}

	return out
}

// --- what the visible checkpoints already hold ------------------------------

// checkpointCover is what the checkpoints of one listing say, reduced to the
// three questions a compaction asks of them.
//
// Every time in it is the earliest ModTime the bucket reported for a checkpoint
// carrying that fact, because the deletion rule is satisfied by *any* covering
// checkpoint being old enough and the earliest is the one most likely to be.
// The zero time means covered by a checkpoint whose ModTime this process cannot
// use — absent, or in the future — which is coverage a fold benefits from and a
// deleter must not: a clock the store cannot make sense of has to disable a
// deletion, never enable one.
type checkpointCover struct {
	// entries maps an entry key some checkpoint holds the body of to that
	// moment. Membership is by the whole key and not by the scan ID: a
	// checkpoint entry whose recomputed key differs from the loose key is not
	// cover for that loose key, and deleting it on a scan-ID match would drop
	// a spelling of the entry the checkpoint does not hold.
	entries map[string]time.Time

	// scans is every scan some checkpoint holds an entry for, by ID. Here the
	// scan ID is the right granularity and the whole key would be the wrong
	// one: the question it answers is the tombstone's, which is whether
	// anything at all can still bring that scan back.
	scans map[encodedScan]struct{}

	// tombstoned maps a scan whose tombstone some checkpoint has absorbed into
	// its own Tombstoned array to that same moment.
	tombstoned map[encodedScan]time.Time
}

// newCheckpointCover starts an empty cover.
func newCheckpointCover() checkpointCover {
	return checkpointCover{
		entries:    make(map[string]time.Time),
		scans:      make(map[encodedScan]struct{}),
		tombstoned: make(map[encodedScan]time.Time),
	}
}

// earliest records at against key, keeping the earliest of the two and letting
// an unusable time win, because an unusable time is what stops a deletion.
func earliest[K comparable](into map[K]time.Time, key K, at time.Time) {
	previous, seen := into[key]
	if !seen || at.IsZero() || (!previous.IsZero() && at.Before(previous)) {
		into[key] = at
	}
}

// coverOf reads every checkpoint the listing showed and reduces them to what
// the two halves of a compaction need.
//
// Every visible checkpoint is read, never a chain from the newest: completeness
// has to come from the listing, for the reason rule 2 at the top of this file
// gives. A checkpoint whose object the bucket cannot produce fails the pass
// rather than shortening it — nothing in this store deletes a checkpoint, so
// its absence means the bucket lost an object, and carrying on would delete
// loose keys on the strength of a cover this run could not actually read.
func (s *Blob) coverOf(
	ctx context.Context, series blobSeries, dir seriesDirectory, now time.Time,
) (checkpointCover, error) {
	cover := newCheckpointCover()

	for _, listed := range dir.checkpoints {
		key := listed.key()

		body, err := s.readCheckpoint(ctx, key)
		if err != nil {
			return checkpointCover{}, fmt.Errorf("compacting the history of %s: %w", series, err)
		}

		at := s.usableModTime(key, listed.modTime, now)

		for i := range body.Entries {
			record := body.Entries[i].record(series.id)

			if !body.Entries[i].describes(record) {
				// The same rule unionCheckpoints applies to the same object: a
				// checkpoint holding a scan of some other target is a
				// compaction that folded the wrong directory, and treating it
				// as cover here would delete one target's entry on the
				// strength of another target's checkpoint.
				s.log.Warn("a checkpoint holds an entry that is not a scan of the series it summarises, and it was ignored",
					"key", key, "target", series.target, "consent_mode", string(series.mode))

				continue
			}

			earliest(cover.entries, record.String(), at)
			cover.scans[record.scan] = struct{}{}
		}

		for _, scan := range body.Tombstoned {
			earliest(cover.tombstoned, scan, at)
		}
	}

	return cover, nil
}

// usableModTime is the moment a deletion may be timed against, or the zero
// time when there is no such moment.
//
// A ModTime the bucket did not report, and one it reports as being in the
// future, both mean the same thing to this pass: the age of the object cannot
// be established, so nothing it covers may be collected. Reported at Warn
// because it is a bucket behaving in a way that switches a maintenance pass
// off, which is precisely the degradation nobody notices (Tenet 8).
func (s *Blob) usableModTime(key string, at, now time.Time) time.Time {
	if at.IsZero() {
		s.log.Warn("the bucket reported no modification time for a checkpoint, so nothing it covers was collected",
			"key", key)

		return time.Time{}
	}

	if at.After(now) {
		s.log.Warn("the bucket reported a modification time in the future for a checkpoint, so nothing it covers was collected",
			"key", key, "mod_time", at, "now", now)

		return time.Time{}
	}

	return at
}

// durablyVisible reports whether an object the bucket timestamped at has been
// there long enough for what it says to be acted on.
//
// The zero time is never long enough, which is how usableModTime disables a
// deletion, and it is the same reason a ModTime in the future would fail this
// test even if it reached here: the comparison is against a moment in the past,
// so a clock that is ahead can only ever make an object look younger.
func durablyVisible(at, now time.Time, grace time.Duration) bool {
	return !at.IsZero() && at.Before(now.Add(-grace))
}

// readCheckpoint reads and validates one checkpoint object.
//
// It is shared with the fold (unionCheckpoints) so that there is one answer to
// what a checkpoint object has to be: the same layout this build writes, saying
// it is a checkpoint. A body of some other shape unmarshals into a checkpoint
// holding nothing, and a reader that accepted it would report a history that is
// short rather than one it could not read.
//
// A listed checkpoint whose object is gone is ErrIndexIncomplete and never a
// short answer, because nothing in this store deletes a checkpoint — see AC11's
// grace period, which exists so that the one thing that could is prevented from
// doing it too early.
func (s *Blob) readCheckpoint(ctx context.Context, key string) (checkpointBody, error) {
	raw, err := s.getIndex(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return checkpointBody{}, fmt.Errorf("checkpoint %s is listed and its object is gone: %w",
				key, ErrIndexIncomplete)
		}

		return checkpointBody{}, err
	}

	var body checkpointBody

	if err := json.Unmarshal(raw, &body); err != nil {
		return checkpointBody{}, fmt.Errorf("checkpoint %s does not decode: %w: %w", key, ErrCorrupt, err)
	}

	if body.Layout != indexLayoutVersion || body.Kind != checkpointKind {
		return checkpointBody{}, fmt.Errorf(
			"the object at %s says it is a %q of layout %d and this build writes %q of layout %d: %w",
			key, truncateForMessage(body.Kind), body.Layout, checkpointKind, indexLayoutVersion, ErrCorrupt,
		)
	}

	return body, nil
}

// --- the write half ---------------------------------------------------------

// foldEntries writes the checkpoint this pass owes, and reports how many
// entries went into it.
//
// The threshold is more than compactAfter loose keys **that no visible
// checkpoint already holds**, which is a shade narrower than §7.2's "more than
// compactAfter loose r. keys in a series directory" and is narrower on purpose.
// A key a checkpoint already covers is a key waiting out its grace period, not
// work: counting it would make every pass between the checkpoint and its
// deletes fold the handful of entries that had arrived since into a checkpoint
// of their own, so a busy series would collect a checkpoint per pass carrying
// one entry each. What the threshold is asking is how much of this directory no
// checkpoint accounts for, and that is what it now measures.
func (s *Blob) foldEntries(
	ctx context.Context, series blobSeries, dir seriesDirectory, cover checkpointCover,
) (int, error) {
	if uncoveredLoose(dir, cover) <= compactAfter {
		return 0, nil
	}

	bodies, err := s.readFoldable(ctx, series, foldable(dir, cover))
	if err != nil {
		return 0, err
	}

	if len(bodies) == 0 {
		return 0, nil
	}

	key, body, err := buildCheckpoint(series, bodies, absorbedTombstones(dir, cover, bodies))
	if err != nil {
		return 0, err
	}

	outcome, err := s.putIndex(ctx, key, body)
	if err != nil {
		return 0, fmt.Errorf("writing a checkpoint of the history of %s: %w", series, err)
	}

	if outcome == putExisted {
		// The key is the digest of the body, so this is the same checkpoint
		// over the same visible set: an earlier pass wrote it and was
		// interrupted before its deletes, and the retry is the no-op AC10 asks
		// for rather than a second summary of the same entries.
		s.log.Debug("a compaction re-folded a set an earlier one had already checkpointed",
			"key", key, "target", series.target, "consent_mode", string(series.mode))
	}

	return len(bodies), nil
}

// uncoveredLoose counts the loose entry keys no visible checkpoint holds, which
// is what the threshold above is measured against.
func uncoveredLoose(dir seriesDirectory, cover checkpointCover) int {
	uncovered := 0

	for _, entry := range dir.entries {
		if _, covered := cover.entries[entry.key()]; !covered {
			uncovered++
		}
	}

	return uncovered
}

// foldable is which loose entries this pass may fold.
//
// Everything older than the newest compactKeep, minus what a visible checkpoint
// already holds, capped at one threshold's worth of entries and at the size of
// one checkpoint object. Skipping what is already covered is not merely an
// economy: re-folding it would write a second checkpoint carrying the same
// entries, which the reader would deduplicate and the bucket would pay for
// twice.
//
// The caps are applied against the sizes the listing already reported, so a
// checkpoint's bound is decided before a single body is fetched. Neither binds
// under the constants as they stand; they are the guard for when one of them
// changes.
func foldable(dir seriesDirectory, cover checkpointCover) []seriesObject {
	out := make([]seriesObject, 0, min(len(dir.entries)-compactKeep, compactAfter))

	var bytes int64

	for _, entry := range dir.entries[compactKeep:] {
		if _, covered := cover.entries[entry.key()]; covered {
			continue
		}

		if len(out) >= min(compactAfter, checkpointMaxEntries) || bytes+entry.size > checkpointMaxBytes {
			break
		}

		out = append(out, entry)
		bytes += entry.size
	}

	return out
}

// readFoldable reads the bodies of the entries a checkpoint will carry.
//
// Strictly, and unlike every read path in this store: an object that is gone or
// that does not decode, or whose body does not describe the key it was found
// under, is left out of the checkpoint and therefore left loose. The read paths
// substitute a placeholder for a damaged entry so that one bad object does not
// hide a history (see decodeEntry); doing that here would write the placeholder
// into a checkpoint and then delete the only object that still held the real
// bytes, which is how a summary becomes the record.
//
// The order the caller gave is preserved, which is the listing's order, which
// is the key order §7.2 requires a checkpoint's entries to be in.
func (s *Blob) readFoldable(
	ctx context.Context, series blobSeries, foldable []seriesObject,
) ([]entryBody, error) {
	read := make([]*entryBody, len(foldable))

	err := eachBounded(ctx, len(foldable), blobHydrateConcurrency, func(ctx context.Context, i int) error {
		body, err := s.readFoldableEntry(ctx, foldable[i])
		if err != nil {
			return err
		}

		read[i] = body

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the history of %s to compact it: %w", series, err)
	}

	out := make([]entryBody, 0, len(foldable))

	for _, body := range read {
		if body == nil {
			continue
		}

		out = append(out, *body)
	}

	return out, nil
}

// readFoldableEntry reads one entry a checkpoint would carry, reporting an
// object this pass must not fold as a nil body rather than as a failure.
func (s *Blob) readFoldableEntry(ctx context.Context, listed seriesObject) (*entryBody, error) {
	key := listed.key()

	raw, err := s.getIndex(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A concurrent prune deleted the entry between the listing and
			// this read. The entry key and its body are one object, so the
			// scan is gone rather than unreadable, and there is nothing to
			// fold.
			s.log.Debug("an index entry was deleted while a compaction was reading it", "key", key)

			return nil, nil //nolint:nilnil // an entry deleted mid-pass is neither a body nor a failure; see the doc comment
		}

		return nil, err
	}

	var body entryBody

	if err := json.Unmarshal(raw, &body); err != nil {
		s.log.Warn("an index entry does not decode and was left out of a checkpoint",
			"key", key, "error", err)

		return nil, nil //nolint:nilnil // left loose on purpose: see readFoldable on why no placeholder is folded
	}

	if !body.describes(listed.record) {
		s.log.Warn("an index entry does not describe the scan its own key names and was left out of a checkpoint",
			"key", key, "scan_id", truncateForMessage(body.Summary.ScanID))

		return nil, nil //nolint:nilnil // left loose on purpose: see readFoldable on why no placeholder is folded
	}

	return &body, nil
}

// absorbedTombstones is which tombstones this checkpoint takes over.
//
// A tombstone may only be collected once nothing can bring its scan back, and a
// scan inside a checkpoint can always be brought back — the checkpoint is never
// deleted. So a tombstone whose scan any checkpoint holds, or whose scan this
// one is about to hold, is copied into the new checkpoint's own Tombstoned
// array first; from then on the fold suppresses the scan from the checkpoints
// alone and the tombstone object itself is collectable under the ordinary
// membership rule.
//
// Already-absorbed scans are left out, so a checkpoint written when nothing has
// changed carries the same bytes as the one before it.
func absorbedTombstones(dir seriesDirectory, cover checkpointCover, folding []entryBody) []encodedScan {
	held := make(map[encodedScan]struct{}, len(folding))

	for i := range folding {
		held[sk(folding[i].Summary.ScanID)] = struct{}{}
	}

	var out []encodedScan

	for _, tombstone := range dir.tombstones {
		scan := tombstone.record.scan

		if _, absorbed := cover.tombstoned[scan]; absorbed {
			continue
		}

		_, inCheckpoint := cover.scans[scan]
		if _, folded := held[scan]; !inCheckpoint && !folded {
			continue
		}

		out = append(out, scan)
	}

	// Sorted and deduplicated so that the bytes are a function of the set and
	// not of the order a listing happened to produce, which is what makes the
	// digest in the key — and therefore the key — reproducible (I4, AC10).
	slices.Sort(out)

	return slices.Compact(out)
}

// buildCheckpoint turns the folded bodies into the object and the key that
// carries them.
//
// There is no wall-clock field, no host name and no run identifier in the body,
// so the bytes are a pure function of what was folded and the digest that names
// them is reproducible. That is what makes an interrupted compaction which
// re-runs a no-op instead of a second summary of the same entries, and it is
// why the key's ordering field is the start time of the newest entry the
// checkpoint covers rather than the moment the compaction happened to run — a
// checkpoint sorts among the entries it replaces.
//
// CoversFrom and CoversTo are documentation for whoever is reading the bucket
// by hand. **They are never a predicate.** A range includes keys written after
// the compactor's listing was taken and therefore absent from Entries, and
// deleting by range would lose exactly those — with one process and one stale
// listing, and no concurrency required.
func buildCheckpoint(
	series blobSeries, bodies []entryBody, tombstoned []encodedScan,
) (string, []byte, error) {
	newest, oldest := bodies[0], bodies[len(bodies)-1]

	body, err := json.Marshal(checkpointBody{
		Layout:     indexLayoutVersion,
		Kind:       checkpointKind,
		CoversFrom: oldest.record(series.id).String(),
		CoversTo:   newest.record(series.id).String(),
		Count:      len(bodies),
		Entries:    bodies,
		Tombstoned: tombstoned,
	})
	if err != nil {
		return "", nil, fmt.Errorf("building a checkpoint of the history of %s: %w", series, err)
	}

	key := series.id.checkpoint(newest.Summary.StartedAt, shortSum(body, identityHexLen))

	return key.String(), body, nil
}

// --- the delete half --------------------------------------------------------

// collectCovered deletes the loose keys a durably visible checkpoint has taken
// over, and the tombstones nothing can still need. It is invariant I3.
//
// Three conditions guard every entry deletion, and each one is there because
// dropping it loses a scan.
//
//   - **Membership, never range.** The key must be one some checkpoint holds
//     the body of. A range predicate covers keys written after the compactor's
//     listing was taken, which are in no checkpoint at all.
//   - **Re-observed now, in this run.** The cover is built from the listing
//     this pass took, not from what an earlier pass wrote or this process
//     remembers. A checkpoint this run cannot see is a checkpoint whose entries
//     this run must not delete.
//   - **Durably visible for the grace period.** The bucket's own ModTime for
//     the checkpoint has to be older than Options.CheckpointGrace, so that
//     every other reader has had a day to see it before the only other copy of
//     those entries goes away.
//
// A failed delete stops the pass rather than being counted and stepped over.
// Every step here is idempotent and the next run repeats the work from a fresh
// listing, so stopping is the cheap expression of "safe to interrupt": there is
// nothing to be gained from pressing a thousand more deletes into a bucket that
// has just refused one.
func (s *Blob) collectCovered(
	ctx context.Context, series blobSeries, dir seriesDirectory, cover checkpointCover, now time.Time,
) (int, int, error) {
	entries := 0

	for _, entry := range dir.entries {
		at, covered := cover.entries[entry.key()]
		if !covered || !durablyVisible(at, now, s.checkpointGrace) {
			continue
		}

		if err := s.dropIndex(ctx, entry.key()); err != nil {
			return entries, 0, fmt.Errorf("collecting an entry of %s a checkpoint holds: %w", series, err)
		}

		entries++
	}

	tombstones, err := s.collectTombstones(ctx, series, dir, cover, now)
	if err != nil {
		return entries, tombstones, err
	}

	return entries, tombstones, nil
}

// collectTombstones deletes the tombstones that suppress nothing any more.
//
// A tombstone hides an entry key a prune removed. It may go when the entry it
// hides can no longer come back, which is three things at once: this listing
// shows no loose entry key for that scan; no visible checkpoint holds the scan
// unless a durably visible one has also absorbed the tombstone into its own
// Tombstoned array; and the tombstone object itself has been in the bucket for
// tombstoneGrace.
//
// The last of those is the one that matters, and the direction it fails in is
// the point. Every other deletion in this file risks losing an entry and is
// guarded against that; this one risks bringing a deleted scan back — a bucket
// that reported a delete it did not perform, or a listing still showing a key
// that is gone, both look exactly like "no loose entry for this scan". A week
// is far beyond any window in which either could still be true, and the object
// being collected is one zero-byte key.
func (s *Blob) collectTombstones(
	ctx context.Context, series blobSeries, dir seriesDirectory, cover checkpointCover, now time.Time,
) (int, error) {
	loose := dir.looseScans()
	collected := 0

	for _, tombstone := range dir.tombstones {
		if !s.tombstoneIsSpent(tombstone, loose, cover, now) {
			continue
		}

		if err := s.dropIndex(ctx, tombstone.key()); err != nil {
			return collected, fmt.Errorf("collecting a tombstone of %s that suppresses nothing: %w", series, err)
		}

		collected++
	}

	return collected, nil
}

// tombstoneIsSpent applies the three conditions collectTombstones argues for.
func (s *Blob) tombstoneIsSpent(
	tombstone seriesObject, loose map[encodedScan]struct{}, cover checkpointCover, now time.Time,
) bool {
	if !durablyVisible(tombstone.modTime, now, tombstoneGrace) {
		return false
	}

	scan := tombstone.record.scan

	if _, visible := loose[scan]; visible {
		return false
	}

	if _, held := cover.scans[scan]; !held {
		// Nothing holds the scan at all, so the tombstone has nothing left to
		// suppress and its absence cannot bring anything back.
		return true
	}

	at, absorbed := cover.tombstoned[scan]

	return absorbed && durablyVisible(at, now, s.checkpointGrace)
}
