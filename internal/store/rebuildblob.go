package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// This file is the half of Story 8.11's rebuild that knows about objects.
//
// It is the store the command exists for. A SQL index can be restored from a
// database backup; an index that is objects in the same bucket as the evidence
// has no second copy to restore from, and Story 8.10, AC16 refuses a converter
// in either direction on the record that rebuilding is the way across. This is
// that way.
//
// What it rebuilds is exactly what PutResult writes, derived by exactly the
// code PutResult derives it with: describeResult produces the keys and the one
// body from summarize(), and indexResult writes them in the order whose every
// interruption point PutResult's comment argues for. So a rebuilt entry and a
// written one are the same bytes at the same key, and there is no second
// derivation to keep in step (AC2).
//
// What it does not do is rewrite anything. Story 8.10, AC3 is that no index
// object is ever overwritten, and a recovery command is the last place to make
// an exception: every write here is the conditional create putIndex already
// performs, so a rebuild over an index that is already complete issues the same
// requests and changes nothing (AC3). Where that means a disagreement cannot be
// corrected — a summary in an entry body that a later summarize() would produce
// differently — it is reported as drift rather than papered over, because the
// body is at a key that is already taken and taking it again is the one thing
// this store does not do.

// rebuildBucket is where this store's index and its evidence both are.
func (s *Blob) rebuildBucket() *bucket { return s.bucket }

// rebuildLog is where a rebuild's progress goes.
func (s *Blob) rebuildLog() *slog.Logger { return s.log }

// beginRebuild writes the marker §7.4 of the Story 8.10 design already honours:
// while it is there, a sweep of this bucket collects nothing at all (AC12).
//
// That is the whole of the safety this command needs against a running daemon.
// A rebuild only ever creates index objects that a scan would have created
// itself, so a scan stored while it runs is either seen by the listing and
// written identically, or not seen and already written by the scan — never
// lost. The one thing that would be lost is a document whose entry the rebuild
// has not reached yet, if a sweep decided it was garbage in the meantime, and
// the marker is what stops that.
func (s *Blob) beginRebuild(ctx context.Context, run *rebuildRun) (func(), error) {
	return markRebuild(ctx, s.bucket, run)
}

// mergeRebuilt records one document in the index, or reports what recording it
// would do.
func (s *Blob) mergeRebuilt(
	ctx context.Context, run *rebuildRun, doc rebuiltDocument,
) (rebuildOutcome, error) {
	indexed, err := s.describeResult(doc.result, doc.ref)
	if err != nil {
		return rebuildUnchanged, err
	}

	outcome, err := s.rebuiltState(ctx, run, indexed)
	if err != nil {
		return rebuildUnchanged, err
	}

	if outcome != rebuildAdded && outcome != rebuildRepaired || !run.opts.Mode.writes() {
		return outcome, nil
	}

	// writeIndexObjects rather than indexResult: a rebuild does not schedule a
	// compaction, because compaction deletes and this command does not (AC4).
	if err := s.writeIndexObjects(ctx, indexed); err != nil {
		return outcome, fmt.Errorf("rebuilding the index entry for scan %s: %w", doc.result.ScanID, err)
	}

	// The pins, the object addressed by scan ID, the entry and the series
	// marker. Every one is a conditional create, so a repair that only needed
	// one of them still issues them all and this is what it cost.
	run.wrote(len(indexed.refs) + 3)

	// The series' directory has just gained this entry and this marker, so the
	// facts read for it are brought up to date rather than dropped. Dropping
	// them would be correct and ruinous: the next document of the same series
	// would list the whole directory again, which for a rebuild of a history of
	// a hundred thousand scans in one series is a hundred thousand listings of a
	// directory that is growing as it goes (AC11). What was written is known
	// exactly, so there is nothing to go back and ask.
	if facts, seen := run.series[indexed.series.id]; seen {
		facts.loose[indexed.entry.String()] = struct{}{}
		facts.marker = true
	}

	return outcome, nil
}

// blobSeriesFacts is what one series directory says, read once per series.
//
// It replaces the pair of existence checks the completeness test used to make
// per scan, and it exists because one of those checks was wrong. Compaction
// folds a series' older entries into a checkpoint and then deletes the loose
// entry keys (Story 8.10, AC11), so on any series past compactAfter scans the
// loose key is legitimately gone for hundreds of scans — and stat'ing it made a
// verify report one drift per compacted scan, for ever, while a rebuild
// re-created every one of them and undid the compaction. The question a rebuild
// has to ask is whether the scan is in the index at all, which is "loose, or
// inside a visible checkpoint".
//
// The tombstones are here for the same reason and are read from the same
// listing: a prune appends one and deletes everything else it can (AC12), so a
// tombstone is how this index says "this scan was removed" — and it is the only
// thing standing between a rebuild and the resurrection of a scan retention
// deliberately took out.
type blobSeriesFacts struct {
	// loose is every entry key the directory listing showed.
	loose map[string]struct{}

	// covered is every scan a visible checkpoint holds an entry for, and
	// tombstoned is every scan a tombstone or an absorbed tombstone names.
	covered    map[encodedScan]struct{}
	tombstoned map[encodedScan]struct{}

	// marker records whether the object that puts this target in Series() is
	// there. One stat per series rather than one per scan.
	marker bool
}

// indexes reports whether this series' index records the scan.
func (f *blobSeriesFacts) indexes(entry seriesRecord, scan encodedScan) bool {
	if _, loose := f.loose[entry.String()]; loose {
		return true
	}

	_, covered := f.covered[scan]

	return covered
}

// pruned reports whether this series' index records the scan as removed.
func (f *blobSeriesFacts) pruned(scan encodedScan) bool {
	_, gone := f.tombstoned[scan]

	return gone
}

// seriesFacts reads one series directory, or returns what a previous document
// of the same series already established.
//
// Once per series and not once per scan: a full listing of one directory plus
// one read per checkpoint answers the question for every scan in it, where the
// per-scan form was two existence checks each. For the rebuild of a history of
// a thousand scans in one series that is a handful of requests instead of two
// thousand, which is the direction AC11 asks this command to move in.
func (s *Blob) seriesFacts(
	ctx context.Context, run *rebuildRun, series blobSeries,
) (*blobSeriesFacts, error) {
	if facts, seen := run.series[series.id]; seen {
		return facts, nil
	}

	dir, err := s.readSeriesDirectory(ctx, series)
	if err != nil {
		return nil, err
	}

	listed := len(dir.tombstones) + len(dir.checkpoints) + len(dir.entries)

	run.listed(max(1, (listed+indexListPageSize-1)/indexListPageSize))

	cover, err := s.coverOf(ctx, series, dir, run.opts.at())
	if err != nil {
		return nil, err
	}

	// coverOf reads every visible checkpoint. They are the largest objects this
	// index writes, so leaving their bytes out of the cost report would
	// understate what a rebuild of a compacted history costs.
	for _, checkpoint := range dir.checkpoints {
		run.read(checkpoint.size)
	}

	facts := &blobSeriesFacts{
		loose:      make(map[string]struct{}, len(dir.entries)),
		covered:    cover.scans,
		tombstoned: make(map[encodedScan]struct{}, len(dir.tombstones)+len(cover.tombstoned)),
	}

	for _, entry := range dir.entries {
		facts.loose[entry.key()] = struct{}{}
	}

	for _, tombstone := range dir.tombstones {
		facts.tombstoned[tombstone.record.scan] = struct{}{}
	}

	// A tombstone object is collected once a checkpoint has absorbed it, and
	// from then on the checkpoint is the only record that the scan was pruned.
	// Reading it from both is what stops a rebuild resurrecting the oldest
	// pruned scans of a compacted series and leaving the rest alone.
	for scan := range cover.tombstoned {
		facts.tombstoned[scan] = struct{}{}
	}

	_, err = s.statIndex(ctx, series.id.markerKey())

	run.checked(1)

	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return nil, err
	default:
		facts.marker = true
	}

	if run.series == nil {
		run.series = make(map[seriesID]*blobSeriesFacts)
	}

	run.series[series.id] = facts

	return facts, nil
}

// prunedScan reports whether this index records that the scan was removed.
//
// The record is the tombstone a prune appends before it deletes anything
// (Story 8.10, AC12), which is exactly what makes this answerable: the
// tombstone is written first and its failure fails the prune, so a scan this
// store removed has one whatever else the prune managed to finish.
//
// The document is still in the bucket because something else names it — a
// baseline holds a copy of the scan it approved, and retention keeps every
// artifact a baseline names (Story 8.5, AC1) — so without this question a
// rebuild would read "here is a document with no entry" and put back a result
// that was deleted to satisfy a retention obligation.
func (s *Blob) prunedScan(ctx context.Context, run *rebuildRun, doc rebuiltDocument) (bool, error) {
	series := blobSeriesFor(doc.result.Target, doc.result.ConsentMode)

	facts, err := s.seriesFacts(ctx, run, series)
	if err != nil {
		return false, err
	}

	return facts.pruned(sk(doc.result.ScanID)), nil
}

// rebuiltState decides what one document does to the index, reading only.
//
// One cheap read per scan — the object addressed by its scan ID — plus one
// listing per *series* for everything else it has to know: which entry keys are
// loose, which scans a checkpoint covers, and whether the series marker is
// there (see blobSeriesFacts). That is the price of "running it twice changes
// nothing the second time" being cheap in writes rather than merely idempotent
// (AC3), and it is a per-series price rather than a per-scan one because a
// rebuild of a long history would otherwise pay for the same directory
// thousands of times over (AC11).
func (s *Blob) rebuiltState(
	ctx context.Context, run *rebuildRun, indexed indexedResult,
) (rebuildOutcome, error) {
	existing, err := s.entryAt(ctx, indexed.series, indexed.scan)

	run.checked(1)

	switch {
	case errors.Is(err, ErrNotFound):
		return rebuildAdded, nil

	case errors.Is(err, ErrCorrupt):
		// The object addressed by this scan ID is present and is not this scan.
		// There is no key to write it to that is not already taken, and taking
		// it again is the rewrite AC3 forbids, so the answer is to report it and
		// leave both where they are. Counted apart from the entries that were
		// left alone because they already agreed: "unchanged: N entries already
		// agreed with the document" is the one thing this entry did not do.
		run.drifted(driftIndexUnreadable, indexed.series.String()+" "+string(indexed.scan), err.Error())

		return rebuildUnreadable, nil

	case err != nil:
		return rebuildUnchanged, err
	}

	if existing.Document.Ref != indexed.document.ref {
		return rebuildConflict, nil
	}

	if !summariesAgree(existing.Summary, indexed.summary) {
		// This store cannot correct it: the entry's key is a function of the
		// scan and the key is already there, so a new body has nowhere to go
		// (Story 8.10, AC3). Reported in every mode, because a rebuild that
		// counted it as work done would be claiming a repair it did not make.
		//
		// It is also the one thing Story 8.3, AC5 promises that this store
		// cannot do. Re-deriving every summary from the documents repairs a bad
		// derivation for a SQL index (see sqlMergeOutcome) and cannot for this
		// one, because the repair is a rewrite. The README says so where an
		// operator chooses between the stores, rather than leaving it to be
		// discovered: the documents are intact, every read of a result derives
		// from them afresh, and what is fixed at write time is the listing's
		// columns. Putting a new derivation into the listing means re-storing
		// those scans.
		return rebuildStale, nil
	}

	complete, err := s.entryIsComplete(ctx, run, indexed)
	if err != nil {
		return rebuildUnchanged, err
	}

	if !complete {
		return rebuildRepaired, nil
	}

	if run.opts.Mode == RebuildVerify {
		if err := s.checkPins(ctx, run, indexed); err != nil {
			return rebuildUnchanged, err
		}
	}

	return rebuildUnchanged, nil
}

// entryIsComplete reports whether the rest of what PutResult writes for this
// scan is there: its entry in the series listing, and the marker that puts its
// target in Series().
//
// "In the series listing" means loose or inside a visible checkpoint, which is
// the whole of the fix this function used to need. Compaction deletes the loose
// key once a checkpoint covering it has been durably visible, so on a compacted
// series a check that only stat'ed the loose key called every folded scan half
// indexed — one drift per scan on every verify, and a rebuild that wrote the
// keys back and undid the compaction, which the next compaction then undid
// again. See blobSeriesFacts.
func (s *Blob) entryIsComplete(ctx context.Context, run *rebuildRun, indexed indexedResult) (bool, error) {
	facts, err := s.seriesFacts(ctx, run, indexed.series)
	if err != nil {
		return false, err
	}

	return facts.marker && facts.indexes(indexed.entry, indexed.scan), nil
}

// checkPins reports every artifact this result names that has no pin keeping it
// alive.
//
// It runs in verify and in no other mode, and the reason is an ordering
// argument rather than a saving. PutResult writes the pins before the object
// addressed by the scan ID and the entry that makes the scan visible, so an
// entry that exists is an entry whose pins were written — a rebuild that found
// the entry has nothing to add. Verify's job is to check that what was written
// is still there, which is a different question and worth one listing per
// artifact to answer.
//
// A missing pin is drift and not damage: the evidence is in the bucket, and
// what is missing is the record that keeps a sweep from collecting it.
func (s *Blob) checkPins(ctx context.Context, run *rebuildRun, indexed indexedResult) error {
	owner := resultRefOwner(indexed.series.id, indexed.scan)

	for _, artifact := range indexed.refs {
		marker, err := newRefMarker(artifact, owner)
		if err != nil {
			return err
		}

		_, err = s.statIndex(ctx, marker.String())

		run.checked(1)

		switch {
		case errors.Is(err, ErrNotFound):
			run.drifted(driftPinMissing, indexed.series.String()+" "+string(indexed.scan), artifact)
		case err != nil:
			return err
		}
	}

	return nil
}

// surveyIndex streams the scans this index records and checks that the document
// each one names is still in the bucket.
//
// It walks byid/ rather than the series directories, because byid/ is the one
// prefix that holds every scan: compaction folds a series' older entries into a
// checkpoint and deletes the loose keys, while the object addressed by a scan
// ID is never folded and never deleted except by a prune. Walking the series
// directories would report every compacted scan as missing.
func (s *Blob) surveyIndex(ctx context.Context, run *rebuildRun) error {
	var cursor indexCursor

	for cursor.more() {
		page, err := s.listIndex(ctx, indexByIDPrefix, cursor, indexListPageSize)
		if err != nil {
			return fmt.Errorf("listing the scans this index records: %w", err)
		}

		cursor = page.next

		run.listed(1)

		scans, err := s.readIndexedScans(ctx, run, page)
		if err != nil {
			return err
		}

		if err := run.checkIndexed(ctx, scans); err != nil {
			return err
		}
	}

	return nil
}

// readIndexedScans reads one page of the objects addressed by scan ID.
//
// The bodies are read in parallel and folded in afterwards, in order, for the
// reason the document walk does the same: the reads are independent round trips
// and the folding touches the run's counters.
func (s *Blob) readIndexedScans(ctx context.Context, run *rebuildRun, page indexPage) ([]indexedScan, error) {
	// Only the keys this grammar spells. A listing of a directory that is being
	// written to shows more than the objects in it — the local file bucket
	// leaves a temporary file beside a key while it writes it, and something
	// else may have written into the index tree — and every other walk in this
	// store steps over what it cannot parse rather than failing the read
	// (seriesRecordOf, observeDecisions). A survey must do the same, or a scan
	// stored while it runs would fail it.
	keys := make([]indexObject, 0, len(page.objects))

	for _, object := range page.objects {
		if _, _, err := parseByIDKey(object.key); err != nil {
			s.log.Debug("an object among the scan-ID keys is not a key this store writes, and was ignored",
				"key", object.key, "bucket", s.bucket.String())

			continue
		}

		keys = append(keys, object)
	}

	read := make([]*entryBody, len(keys))
	broken := make([]string, len(keys))

	err := eachBounded(ctx, len(keys), run.opts.workers(), func(ctx context.Context, i int) error {
		raw, err := s.getIndex(ctx, keys[i].key)

		// Listed and gone. A prune deletes these, so a listing one step behind
		// a prune produces it and it is not a fault — the entry is left out of
		// the survey rather than reported as damage.
		gone := errors.Is(err, ErrNotFound)

		if err != nil && !gone {
			return err
		}

		if gone {
			return nil
		}

		var body entryBody

		// An object that is present and does not decode is drift rather than a
		// failure of the survey, so it is recorded here and reported by the
		// fold below. Neither branch returns: one damaged index object must not
		// stop the other thousand from being checked.
		if decodeErr := json.Unmarshal(raw, &body); decodeErr != nil {
			broken[i] = decodeErr.Error()
		} else {
			read[i] = &body
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the scans this index records: %w", err)
	}

	run.checked(len(keys))

	scans := make([]indexedScan, 0, len(keys))

	for i, object := range keys {
		switch {
		case broken[i] != "":
			run.drifted(driftIndexUnreadable, object.key, broken[i])
		case read[i] != nil:
			scans = append(scans, indexedScan{
				target:   read[i].Summary.Target,
				mode:     read[i].Summary.ConsentMode,
				scanID:   read[i].Summary.ScanID,
				document: read[i].Document.Ref,
			})
		}
	}

	return scans, nil
}

// indexOnlyRecords counts what this index holds that no document could produce
// (AC6): the baseline decisions and the audit log.
//
// For this store they are objects under _wsaw/index/v1/baseline/ and
// _wsaw/index/v1/audit/, and they are the reason the README tells an operator
// to turn on bucket versioning or to back that one prefix up. A document
// decodes to the scan it records, so the result index is derivable; an approval
// is a human decision about a scan and is derivable from nothing.
//
// Decisions are counted rather than standing baselines, because the log is what
// this store keeps: an approval and the withdrawal that replaced it are two
// objects and both are records of a decision somebody took.
func (s *Blob) indexOnlyRecords(ctx context.Context, run *rebuildRun) (Unrecoverable, error) {
	decisions, err := s.countIndexObjects(ctx, run, indexBaselinePrefix)
	if err != nil {
		return Unrecoverable{}, err
	}

	entries, err := s.countIndexObjects(ctx, run, indexAuditPrefix)
	if err != nil {
		return Unrecoverable{}, err
	}

	return Unrecoverable{Baselines: decisions, AuditEntries: entries, Counted: true}, nil
}

// countIndexObjects counts the keys under one index prefix.
func (s *Blob) countIndexObjects(ctx context.Context, run *rebuildRun, prefix string) (int, error) {
	var (
		found  int
		cursor indexCursor
	)

	for cursor.more() {
		page, err := s.listIndex(ctx, prefix, cursor, indexListPageSize)
		if err != nil {
			return 0, fmt.Errorf("counting what only the index holds under %s: %w", prefix, err)
		}

		cursor = page.next
		found += len(page.objects)

		run.listed(1)
	}

	return found, nil
}

// indexOnlyDrift reports the one disagreement this index can have that no
// document is involved in: a baseline decision whose entry in the audit log is
// not there.
//
// It is the gap Story 8.10 documented and deferred to here. An approval and the
// audit entry that explains it are one object under one key, so no interruption
// records one without the other — but the copy filed under audit/, which is
// what the log view is read through, is a second write, and a process that dies
// between them leaves the log one entry short. It heals on the next decision
// for that target, and where a target takes no further decision it stays until
// something looks for it. This is that something (AC10).
//
// A pointer that has been folded into an audit checkpoint is legitimately
// absent, so the check stands down where one exists rather than reporting every
// old decision as drift. Nothing in this build writes a checkpoint, which is
// why the branch is a guard and not a code path with tests of its own.
func (s *Blob) indexOnlyDrift(ctx context.Context, run *rebuildRun) error {
	if err := s.noAuditCheckpoints(ctx); err != nil {
		if errors.Is(err, ErrIndexIncomplete) {
			s.log.Info("the audit log has been compacted, so a rebuild did not check it against the decisions",
				"bucket", s.bucket.String())

			return nil
		}

		return err
	}

	run.listed(1)

	var cursor indexCursor

	for cursor.more() {
		page, err := s.listIndex(ctx, indexBaselinePrefix, cursor, indexListPageSize)
		if err != nil {
			return fmt.Errorf("listing the baseline decisions this index holds: %w", err)
		}

		cursor = page.next

		run.listed(1)

		if err := s.checkAuditPointers(ctx, run, page); err != nil {
			return err
		}
	}

	return nil
}

// checkAuditPointers reports the decisions in one page whose audit entry is
// missing.
func (s *Blob) checkAuditPointers(ctx context.Context, run *rebuildRun, page indexPage) error {
	for _, object := range page.objects {
		key, err := parseDecisionKey(object.key)
		if err != nil {
			// Stepped over rather than reported, exactly as observeDecisions
			// steps over it: a decision being written right now leaves a
			// temporary object beside its key in a local file bucket, and
			// calling that drift would make a verify fail because somebody
			// approved a baseline while it ran.
			s.log.Debug("an object among the baseline decisions is not a key this store writes, and was ignored",
				"key", object.key, "bucket", s.bucket.String())

			continue
		}

		_, err = s.statIndex(ctx, key.auditPointer().String())

		run.checked(1)

		switch {
		case errors.Is(err, ErrNotFound):
			run.drifted(driftAuditMissing, object.key, "the audit log has no entry at "+
				key.auditPointer().String())
		case err != nil:
			return err
		}
	}

	return nil
}

// rebuildsInProgress names the markers a rebuild leaves while it runs.
//
// It is the sweep's question and the marker is a fact about the bucket rather
// than about this store, so the answer lives on the bucket and both store kinds
// ask it there (see bucket.rebuildMarkers).
func (s *Blob) rebuildsInProgress(ctx context.Context, now time.Time) ([]string, error) {
	return s.bucket.rebuildMarkers(ctx, now, s.log)
}
