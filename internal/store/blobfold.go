package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is where the bucket index answers questions about results: the
// write that records a scan, and the six reads a page render and a comparison
// make of a series (Story 8.10, AC4).
//
// Every write here goes to a key derived from the fact it records, through
// putIndex, which creates and never rewrites. That is AC3, and it is what makes
// the rest of the design possible: because no object is ever edited, a reader
// never sees a torn one, a retry of an interrupted write lands the same bytes
// at the same key rather than a second version of the same scan (AC10), and two
// writers racing produce two keys rather than a lost update.
//
// Every read is a fold, and the fold is split in two because everything a read
// needs in order to *select* is already in the key. foldKeys lists one
// directory and returns the entries the answer is made of, having read no
// bodies at all; hydrate then reads exactly the bodies the answer returns. That
// split is why a listing costs no result document (Story 8.3, AC2), why
// PreviousResult can skip two hundred consecutive failures for one listing, and
// why retention by age will be able to decide from the keys alone.
//
// The one shape to keep in mind is the series directory. Tombstones, checkpoints
// and entries share it, their tags sort "d" < "k" < "r", and the ordering field
// is inverted — so a single listing returns every tombstone, then every
// checkpoint newest first, then every entry newest first. A fold that wants the
// newest few reads one page and stops, having already seen everything that
// could suppress or replace what it found. That is what makes AC7's determinism
// a property of one visible key set rather than of three listings that may
// disagree with one another.

const (
	// blobMaxHydrate caps how many entry bodies one read may fetch.
	//
	// A thousand, which is the same number the HTTP API clamps its own ?limit=
	// to, so the cap can only bind on a caller that has asked for more than the
	// interface will ever render. It is a cap on requests and therefore on an
	// invoice (AC11): the store interface spells "all of them" as a limit of
	// zero, and a series with sixty thousand scans in it would otherwise turn
	// one page render into sixty thousand GETs.
	blobMaxHydrate = 1000

	// blobHydrateConcurrency is how many entry bodies are read at once.
	//
	// Sixteen, because the reads are independent and each is a round trip: at
	// one at a time a hundred-row page would be a hundred sequential round
	// trips, and the wall clock of a page render is the number this decides.
	// It is a fixed bound rather than one goroutine per entry, which AGENTS §4
	// forbids and which would open a thousand connections to answer one page.
	blobHydrateConcurrency = 16

	// maxTieRun is how many entries may share one nanosecond before the fold
	// stops trying to order them and reports the directory as damaged.
	//
	// Entries at one instant form a contiguous run that the fold has to read to
	// completion in order to break the tie, so an unbounded run is an unbounded
	// read on the hot path. A thousand scans of one target starting in the same
	// nanosecond is not a history; it is a key collision or an object somebody
	// else wrote, and saying so beats folding it.
	maxTieRun = 1000

	// unreadableEntry explains an index entry that is present and unusable. It
	// is the bucket index's counterpart to underivedSummary, and it exists for
	// the same reason: presenting a summary of zeroes would read as a scan that
	// saw nothing, and dropping the entry would hide a scan that happened
	// (Tenet 5).
	unreadableEntry = "this scan's index entry could not be read, so its summary could not be recovered"
)

// The two terminations that are not observations of the site, encoded once.
//
// PreviousResult skips them: a failed or skipped scan happened and stays in the
// history, but diffing against it would report the whole site as new or as
// gone. They are package variables rather than a call per entry because the
// fold tests every key it walks against them, and because deriving them from
// the model here is what keeps this rule and SQL's `termination not in (?, ?)`
// from becoming two lists.
var (
	termNotObserved  = tm(model.TermError)
	termNotAttempted = tm(model.TermSkipped)
)

// documentPointer is where an index object says a result's document is, how big
// it should be, and what it must hash to.
//
// It is the body's copy of resultRef, spelled as JSON. The three fields travel
// together because an index that recorded fewer of them could not tell
// truncated evidence from intact evidence.
type documentPointer struct {
	Ref    string `json:"ref"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// resultRef is what the document reader takes.
func (d documentPointer) resultRef() resultRef {
	return resultRef{ref: d.Ref, size: d.Size, digest: d.Digest}
}

// entryBody is the body of both index objects that record one scan: the entry
// in the series directory, and the object addressed by scan ID.
//
// The two hold byte-identical bodies, and the duplication is deliberate. Each
// object is then self-describing, which is what makes this bucket debuggable
// and what lets Story 8.11's rebuild derive both from one document without
// keeping two derivations in step. The entry's own key is recomputable from
// this body — the summary carries the start time, the scan ID and the
// termination, which is every field of it — so no back-pointer is written, and
// a body that does not agree with the key it was found under is caught rather
// than trusted.
//
// Refs is the index's answer to "which artifacts does this result name" (Story
// 8.5, AC2), costing retention no request at all. It includes the document
// itself, because the document is an artifact like any other and the pin that
// keeps it has to be written like any other.
type entryBody struct {
	Layout   int             `json:"layout"`
	Summary  Summary         `json:"summary"`
	Document documentPointer `json:"document"`
	Refs     []string        `json:"refs"`
}

// record rebuilds the series key this body belongs under, in the series named.
func (e entryBody) record(id seriesID) seriesRecord {
	return id.entry(e.Summary.StartedAt, sk(e.Summary.ScanID), tm(e.Summary.Termination))
}

// seriesOf is the series this body's own summary says it belongs to.
func (e entryBody) seriesOf() seriesID {
	return seriesFor(e.Summary.Target, e.Summary.ConsentMode)
}

// describes reports whether this body is the scan the key it was found under
// names — including which series that key is in.
//
// The check is free, because the whole key is a pure function of the body, and
// it is the one that catches an object written to the wrong key by a bug here
// or by something else writing into the index tree. The series is derived from
// the body rather than taken from the key on purpose: taking it from the key
// would make the comparison true by construction for exactly the case that
// matters most, a scan filed under another target's history.
//
// It compares the encoded forms rather than the raw values because the key
// holds only the encoded ones, so that is the comparison a key can support.
func (e entryBody) describes(rec seriesRecord) bool {
	return e.record(e.seriesOf()) == rec
}

// checkpointBody is what a compaction leaves behind: the entries it folded away
// and the tombstones it absorbed, under one immutable key.
//
// It is read here and written by compaction, and it is defined here because the
// fold is the only thing that has to be right about it. There is deliberately no
// wall-clock field and no pointer to a previous checkpoint. No clock, so the
// bytes — and therefore the digest in the key — are a pure function of what was
// folded, which makes an interrupted compaction that re-runs a no-op (AC10). No
// chain, because completeness comes from unioning every checkpoint one listing
// shows: a chain is a graph two concurrent compactors can fork, and a fork
// strands every entry in the orphaned branch.
//
// CoversFrom and CoversTo are documentation and are never a predicate. Deletion
// of what a checkpoint covers is by membership of Entries, never by key range,
// because a range includes keys written after the compactor's listing was taken.
type checkpointBody struct {
	Layout     int    `json:"layout"`
	Kind       string `json:"kind"`
	CoversFrom string `json:"coversFrom"`
	CoversTo   string `json:"coversTo"`
	Count      int    `json:"count"`

	Entries []entryBody `json:"entries"`

	// Tombstoned holds encoded scan IDs, because a compaction reads them off
	// tombstone keys and a key is all it has. They are carried in the
	// checkpoint so that the tombstone objects themselves can eventually be
	// collected without the scans they hide coming back.
	Tombstoned []encodedScan `json:"tombstoned"`
}

// seriesMarker is the body of the object that records that a series exists.
//
// It carries the literal target and consent mode because the key cannot: <tk>
// is a hash with a decorative slug after it, and Series() has to return the
// target a caller could ask about again. It is also why Series() sorts in
// memory — the keys order by digest and SQL orders by target.
type seriesMarker struct {
	Layout int               `json:"layout"`
	Target string            `json:"target"`
	Mode   model.ConsentMode `json:"consentMode"`
}

// blobSeries is one target-and-mode history, carrying both the identity a
// caller named it by and the one its keys are spelled with.
//
// The pair travels together because nearly everything in this file needs both:
// the encoded half builds the keys, and the literal half is what an error
// message, a log line and a returned Summary have to say. Deriving the encoded
// half at each use would be a second place for a target to be encoded, which is
// the mistake seriesID exists to prevent.
type blobSeries struct {
	target string
	mode   model.ConsentMode
	id     seriesID
}

// blobSeriesFor names one series from the values a caller asked with.
func blobSeriesFor(target string, mode model.ConsentMode) blobSeries {
	return blobSeries{target: target, mode: mode, id: seriesFor(target, mode)}
}

// String names the series the way the SQL stores' error messages do, so a
// failure reads the same whichever store produced it.
func (b blobSeries) String() string { return b.target + "/" + string(b.mode) }

// foldEntry is one entry a fold selected: the key that selected it and, where
// the fold already had to read the object carrying it, its body.
//
// The body is on the entry rather than fetched later because an entry a
// checkpoint covers has no loose object left to fetch: compaction wrote its
// body into the checkpoint and deleted the key. Carrying it here is what lets
// hydrate spend a request only on the entries whose bodies it does not already
// hold.
type foldEntry struct {
	record seriesRecord
	body   *entryBody
}

// --- the write ------------------------------------------------------------

// PutResult stores a scan: its document in the bucket, and the index objects
// that make it findable by series and by scan ID.
//
// The document goes to the bucket first (Story 8.2, AC4) and the index objects
// after it, in an order chosen so that every point it can be interrupted at
// leaves a state a reader survives:
//
//   - the document alone is invisible to every query. That is a result that
//     does not exist yet, never one that was deleted (AC6), and the sweep is
//     forbidden from collecting it (§7.4) precisely so a rebuild can re-derive
//     its entry.
//   - the pins next, so a live result never names an artifact nothing has
//     claimed. Pins whose owner never arrived are the sweep's dangling-owner
//     case, which is the analogue of what a SQL store's sweep already cleans up.
//   - the object addressed by scan ID before the entry in the series listing,
//     so that a row appearing in an interface always resolves when it is
//     clicked. The reverse order is the one visibly wrong state.
//   - the entry is the commit point: after it the scan is in the history.
//   - the series marker last. Without it Series() is one write behind and heals
//     on the next scan of the same target or on a rebuild, which is a listing
//     that is briefly short a target rather than a history that is wrong.
//
// It is idempotent in every half. The document is content-addressed, and each
// index object is created with IfNotExist at a key derived from the scan, so
// re-running an interrupted call writes the same bytes to the same keys and
// changes nothing (AC10).
func (s *Blob) PutResult(res *model.Result) error {
	if res.Target == "" {
		return errors.New("store: result has no target")
	}

	if res.ScanID == "" {
		return errors.New("store: result has no scan ID")
	}

	document, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encoding result %s: %w", res.ScanID, err)
	}

	ctx, cancel := s.opBudget()
	defer cancel()

	ref, err := s.putDocument(ctx, document)
	if err != nil {
		return fmt.Errorf("storing result %s: %w", res.ScanID, err)
	}

	indexed, err := s.describeResult(res, ref)
	if err != nil {
		return fmt.Errorf("storing result %s: %w", res.ScanID, err)
	}

	if err := s.indexResult(ctx, indexed); err != nil {
		return fmt.Errorf("storing result %s: %w", res.ScanID, err)
	}

	return nil
}

// putDocument writes the document to the bucket and describes it for the index.
//
// The digest is computed here rather than taken from the reference the bucket
// returns. They agree today, because artifacts are content-addressed, but the
// key layout is the bucket's business: the index records what the document must
// hash to, independently of how it is addressed.
func (s *Blob) putDocument(ctx context.Context, document []byte) (resultRef, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	ref, err := s.bucket.put(ctx, artifactKindResult, document)
	if err != nil {
		return resultRef{}, err
	}

	return resultRef{ref: ref, size: int64(len(document)), digest: documentDigest(document)}, nil
}

// indexedResult is one stored document reduced to the keys and the bytes that
// will record it.
//
// It is computed once, before the first write, because that is what makes the
// write replayable: the body is marshalled once and every object that carries
// it gets the identical bytes, so an interrupted call and its retry cannot
// produce two spellings of one fact (AC10, and see describeResult on why the
// start time is normalised).
type indexedResult struct {
	series   blobSeries
	scan     encodedScan
	entry    seriesRecord
	document resultRef
	refs     []string
	body     []byte
}

// describeResult derives every key and the one body a stored result needs.
func (s *Blob) describeResult(res *model.Result, ref resultRef) (indexedResult, error) {
	// The same summarize() the interface renders a listing with, so a listed
	// summary and a computed one cannot disagree (Story 8.3, AC4).
	sum := summarize(res)

	// UTC, because the index object records an instant and not the offset the
	// scanning host happened to be in. It is also what makes the bytes a pure
	// function of the scan: two hosts in different zones storing the same
	// result would otherwise write different bodies to the same key, and the
	// conditional create would keep whichever landed first.
	sum.StartedAt = sum.StartedAt.UTC()

	if _, clamped := clampNano(sum.StartedAt); clamped {
		// Clamped and never dropped: a scan with a wrong clock is still a scan.
		// Reported here because this is the only place that holds the scan ID,
		// target and consent mode a warning about it has to carry.
		s.log.Warn("a scan's start time is outside the range the bucket index can order, and was clamped to the edge",
			"scan_id", res.ScanID, "target", res.Target,
			"consent_mode", string(res.ConsentMode), "started_at", sum.StartedAt)
	}

	series := blobSeriesFor(res.Target, res.ConsentMode)
	scan := sk(res.ScanID)

	// Which artifacts this result names, worked out here where the decoded
	// result is in hand, so retention never has to read a document back out of
	// the bucket to find out (Story 8.5, AC2).
	refs := artifactRefsOf(res, ref.ref)

	body, err := json.Marshal(entryBody{
		Layout:   indexLayoutVersion,
		Summary:  sum,
		Document: documentPointer{Ref: ref.ref, Size: ref.size, Digest: ref.digest},
		Refs:     refs,
	})
	if err != nil {
		return indexedResult{}, fmt.Errorf("building the index entry for %s: %w", res.ScanID, err)
	}

	return indexedResult{
		series:   series,
		scan:     scan,
		entry:    series.id.entry(sum.StartedAt, scan, tm(sum.Termination)),
		document: ref,
		refs:     refs,
		body:     body,
	}, nil
}

// indexResult writes the index objects that make one stored document findable,
// in the order PutResult's comment argues for.
func (s *Blob) indexResult(ctx context.Context, r indexedResult) error {
	if err := s.pinArtifacts(ctx, r); err != nil {
		return err
	}

	byID := r.series.id.byIDKey(r.scan)

	outcome, err := s.putIndex(ctx, byID, r.body)
	if err != nil {
		return err
	}

	if outcome == putExisted {
		if err := s.sameScanAlreadyStored(ctx, r); err != nil {
			return err
		}
	}

	// The commit point.
	if _, err := s.putIndex(ctx, r.entry.String(), r.body); err != nil {
		return err
	}

	return s.recordSeries(ctx, r.series)
}

// pinArtifacts writes the reverse index this result's evidence is kept alive by.
//
// One zero-byte object per artifact, named for the artifact and the owner, so
// that "does anything still reference this" is a listing of one short directory
// and no read at all — which is what makes the retention sweep affordable
// against a bucket that charges per request (AC12). The document is pinned with
// the rest: it is an artifact, and nothing else would keep it.
//
// They are written before the result becomes visible, so a live result never
// names an artifact that retention has no reason to keep.
func (s *Blob) pinArtifacts(ctx context.Context, r indexedResult) error {
	owner := resultRefOwner(r.series.id, r.scan)

	for _, artifact := range r.refs {
		marker, err := newRefMarker(artifact, owner)
		if err != nil {
			// artifactRefsOf already dropped every reference that is not one
			// this store wrote, so this is unreachable from a stored document.
			// It is checked anyway because the alternative is splicing an
			// unvalidated reference into a key (Tenet 9).
			return err
		}

		if _, err := s.putIndex(ctx, marker.String(), nil); err != nil {
			return err
		}
	}

	return nil
}

// sameScanAlreadyStored establishes that a scan-ID key which was already there
// records the scan this call is storing, and refuses when it does not.
//
// This is the one deliberate behaviour divergence from the SQL stores, which
// upsert. Here a scan ID is a key, and rewriting a key is the thing this index
// does not do (AC3): two histories under one scan ID would be the alternative,
// and neither of them could be shown to a reviewer as the scan. Production
// cannot produce it — newScanID mints a fresh identifier per scan, retries
// included — and a retry of the same scan cannot either, because a document is
// content-addressed and so an identical reference is an identical scan.
func (s *Blob) sameScanAlreadyStored(ctx context.Context, r indexedResult) error {
	existing, err := s.entryAt(ctx, r.series, r.scan)
	if err != nil {
		return fmt.Errorf("reading the index object already recorded for this scan ID: %w", err)
	}

	if existing.Document.Ref == r.document.ref {
		return nil
	}

	return fmt.Errorf(
		"scan %s of %s is already recorded against document %s and this call stores %s; "+
			"a scan ID names one scan and this index does not rewrite a key it has written",
		truncateForMessage(existing.Summary.ScanID), r.series, existing.Document.Ref, r.document.ref,
	)
}

// recordSeries writes the marker that puts a target and consent mode in
// Series().
//
// It is written on every store rather than once per series per process. The
// write is conditional and so costs nothing after the first, and the cache that
// would save the request cannot be correct on its own: retention deletes this
// marker when a series expires entirely, and a process holding "already written"
// in memory would then never re-create it and would drop a live target out of
// the dashboard. The cache belongs with the prune that invalidates it.
func (s *Blob) recordSeries(ctx context.Context, series blobSeries) error {
	body, err := json.Marshal(seriesMarker{
		Layout: indexLayoutVersion,
		Target: series.target,
		Mode:   series.mode,
	})
	if err != nil {
		return fmt.Errorf("building the series marker for %s: %w", series, err)
	}

	if _, err := s.putIndex(ctx, series.id.markerKey(), body); err != nil {
		return err
	}

	return nil
}

// --- the fold -------------------------------------------------------------

// seriesFold is the state of one walk over a series directory.
//
// It is a type rather than a handful of locals because the selection rules are
// what this store is, and having them as methods on the thing they decide about
// is what lets each of them be read — and argued with — on its own.
type seriesFold struct {
	// want is how many entries the answer holds. It is always at least one:
	// the interface's "all of them" is turned into a bounded number by the
	// caller, because an unbounded read is an unbounded invoice (AC11).
	want int

	// skipFailed drops the terminations that are not observations of the site.
	skipFailed bool

	// before, when set, restricts the answer to the entries that precede one
	// anchor in the order SQL's (started_at, scan_id) comparison defines.
	before *seriesRecord

	// tombstoned is every scan the listing said has been pruned. It is complete
	// before the first entry is examined, because tombstone keys sort ahead of
	// entry keys in the one directory both live in.
	tombstoned map[encodedScan]struct{}

	// checkpoints is every compaction summary the listing showed, unread. They
	// are read only when the loose entries did not satisfy the answer, which is
	// what keeps the hot path off them.
	checkpoints []seriesRecord

	selected []foldEntry

	// runInv and runLen track the entries sharing one instant. A run has to be
	// read to completion before it can be ordered, so it is also the one place
	// a fold's cost is not bounded by want.
	runInv string
	runLen int

	// full records that the answer cannot change: enough entries have been
	// selected and the run the last of them belongs to has been read out.
	full bool
}

// newSeriesFold starts a walk.
func newSeriesFold(want int, skipFailed bool, before *seriesRecord) *seriesFold {
	return &seriesFold{
		want:       want,
		skipFailed: skipFailed,
		before:     before,
		tombstoned: make(map[encodedScan]struct{}),
	}
}

// satisfied reports whether the walk has as many entries as were asked for.
func (f *seriesFold) satisfied() bool { return len(f.selected) >= f.want }

// observe classifies one key the listing returned.
func (f *seriesFold) observe(rec seriesRecord) error {
	switch rec.kind {
	case seriesTombstone:
		f.tombstoned[rec.scan] = struct{}{}
	case seriesCheckpoint:
		f.checkpoints = append(f.checkpoints, rec)
	case seriesEntry:
		return f.observeEntry(rec)
	}

	return nil
}

// observeEntry decides what one entry key does to the answer.
func (f *seriesFold) observeEntry(rec seriesRecord) error {
	if rec.inv != f.runInv {
		if f.satisfied() {
			// The answer is full and this key begins a new instant, so the run
			// its last entry belongs to has been read out and nothing further
			// down the listing can reorder it. Stopping here rather than one
			// entry earlier is the whole of the tie rule: the listing gives a
			// run in ascending scan ID and the answer wants descending, so a
			// run has to be seen whole before any of it can be placed.
			f.full = true

			return nil
		}

		f.runInv, f.runLen = rec.inv, 0
	}

	f.runLen++

	if f.runLen > maxTieRun {
		return fmt.Errorf(
			"more than %d entries of this series claim to have started at the same nanosecond: %w",
			maxTieRun, ErrCorrupt,
		)
	}

	if !f.admits(rec) {
		return nil
	}

	f.selected = append(f.selected, foldEntry{record: rec})

	return nil
}

// admits applies the three selection rules to one entry key.
//
// All three are answered from the key, which is the reason the key carries what
// it carries: a series with two hundred consecutive failures in front of the
// scan a comparison wants costs one listing and no reads at all.
func (f *seriesFold) admits(rec seriesRecord) bool {
	if _, pruned := f.tombstoned[rec.scan]; pruned {
		return false
	}

	if f.skipFailed && (rec.term == termNotObserved || rec.term == termNotAttempted) {
		return false
	}

	if f.before != nil && !olderThan(rec, *f.before) {
		return false
	}

	return true
}

// answer orders, deduplicates and truncates what the walk selected.
//
// The tombstone filter is applied again here because a checkpoint carries the
// scans whose own tombstone objects have been collected, and those are learned
// after the loose entries were selected. It cannot change an answer the walk
// stopped early on: a tombstone object is only collected once no entry key for
// its scan is visible, so a loose entry can never be suppressed by a checkpoint
// alone.
func (f *seriesFold) answer() []foldEntry {
	slices.SortFunc(f.selected, func(a, b foldEntry) int {
		return compareEntries(a.record, b.record)
	})

	out := make([]foldEntry, 0, min(len(f.selected), f.want))
	seen := make(map[encodedScan]struct{}, len(f.selected))

	for _, entry := range f.selected {
		if _, pruned := f.tombstoned[entry.record.scan]; pruned {
			continue
		}

		// A scan can appear twice: once as a loose entry and once inside a
		// checkpoint that covers it but whose deletes have not run yet. Both
		// carry the same key, because both were derived from the same body, so
		// keeping the first is keeping the one the sort put first.
		if _, duplicate := seen[entry.record.scan]; duplicate {
			continue
		}

		seen[entry.record.scan] = struct{}{}
		out = append(out, entry)

		if len(out) == f.want {
			break
		}
	}

	return out
}

// compareEntries is the order the answer is returned in: newest first, and a
// tie at one instant broken by scan ID descending.
//
// The ordering field is inverted, so ascending byte order over it is descending
// time. The tie-break is not in the key and is applied here, for two reasons the
// design records: a scan ID from the entropy-failure fallback is not
// fixed-width, so a key-encoded descending tie-break would not be sound, and a
// byte-complemented scan ID would destroy the one property that makes a bucket
// index worth debugging.
//
// It compares the encoded scan ID rather than the raw one, because that is what
// a key carries and the fold reads no bodies. The two orders agree for every
// scan ID wsaw mints, all of which pass through the encoder unchanged; a scan ID
// that had to be hashed orders by its hash, which is deterministic and is the
// most a key can offer.
func compareEntries(a, b seriesRecord) int {
	if order := strings.Compare(a.inv, b.inv); order != 0 {
		return order
	}

	return -strings.Compare(string(a.scan), string(b.scan))
}

// olderThan reports whether rec precedes anchor, in the order SQL expresses as
// a row-value comparison on (started_at, scan_id).
//
// Older is a larger ordering field, because the field is inverted. At one
// instant it is a smaller scan ID, which is the same tie-break compareEntries
// applies, read the other way round.
func olderThan(rec, anchor seriesRecord) bool {
	if rec.inv != anchor.inv {
		return rec.inv > anchor.inv
	}

	return rec.scan < anchor.scan
}

// foldKeys returns the entries one query selects from a series, newest first,
// reading no bodies unless a checkpoint has to be opened.
//
// It is one listing of the series directory, which yields the tombstones, the
// checkpoints and the loose entries together, so that the answer is a function
// of one visible key set (AC7). Paging continues only while entries are still
// wanted: a page render of a target with ten thousand scans behind it costs one
// listing, not ten.
func (s *Blob) foldKeys(
	ctx context.Context,
	series blobSeries,
	want int,
	skipFailed bool,
	before *seriesRecord,
) ([]foldEntry, error) {
	fold := newSeriesFold(want, skipFailed, before)
	prefix := series.id.dirPrefix()

	var cursor indexCursor

	for cursor.more() && !fold.full {
		page, err := s.listIndex(ctx, prefix, cursor, indexListPageSize)
		if err != nil {
			return nil, fmt.Errorf("reading the history of %s: %w", series, err)
		}

		cursor = page.next

		if err := s.observePage(series, fold, page); err != nil {
			return nil, fmt.Errorf("reading the history of %s: %w", series, err)
		}
	}

	if !fold.satisfied() {
		if err := s.unionCheckpoints(ctx, series, fold); err != nil {
			return nil, fmt.Errorf("reading the history of %s: %w", series, err)
		}
	}

	return fold.answer(), nil
}

// observePage folds one page of a series listing into the walk.
func (s *Blob) observePage(series blobSeries, fold *seriesFold, page indexPage) error {
	for _, object := range page.objects {
		record, err := parseSeriesRecord(object.key)
		if err != nil {
			// A key inside this store's own tree that this grammar does not
			// produce. It is reported and stepped over rather than failing the
			// read: something else has written into the index, and refusing to
			// show a target's history because of it would turn one stray object
			// into an outage.
			s.log.Warn("an object in the index is not a key this store writes, and was ignored",
				"key", object.key, "target", series.target, "consent_mode", string(series.mode))

			continue
		}

		if err := fold.observe(record); err != nil {
			return err
		}

		if fold.full {
			return nil
		}
	}

	return nil
}

// unionCheckpoints adds what the compaction summaries hold to a walk the loose
// entries did not satisfy.
//
// Every visible checkpoint is read and unioned, never chained. Completeness
// then follows from one listing rather than from a graph that two concurrent
// compactors can fork, and two overlapping checkpoints just produce duplicates
// the answer deduplicates by scan ID.
//
// A checkpoint the listing showed and the bucket cannot produce is
// ErrIndexIncomplete and never a short answer. Nothing in this store deletes a
// checkpoint, so its absence means the bucket lost an object or a person removed
// one, and answering "that is all the history there is" would be the inference
// Tenet 5 forbids.
func (s *Blob) unionCheckpoints(ctx context.Context, series blobSeries, fold *seriesFold) error {
	for _, record := range fold.checkpoints {
		key := record.String()

		raw, err := s.getIndex(ctx, key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("checkpoint %s is listed and its object is gone: %w", key, ErrIndexIncomplete)
			}

			return err
		}

		var checkpoint checkpointBody

		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return fmt.Errorf("checkpoint %s does not decode: %w: %w", key, ErrCorrupt, err)
		}

		for _, scan := range checkpoint.Tombstoned {
			fold.tombstoned[scan] = struct{}{}
		}

		for i := range checkpoint.Entries {
			entry := checkpoint.Entries[i].record(series.id)

			if !checkpoint.Entries[i].describes(entry) {
				// A checkpoint carrying a scan of some other target is a
				// compaction that folded the wrong directory, and adding it
				// here would put one target's scan in another's history.
				s.log.Warn("a checkpoint holds an entry that is not a scan of the series it summarises, and it was ignored",
					"key", key, "target", series.target, "consent_mode", string(series.mode))

				continue
			}

			if !fold.admits(entry) {
				continue
			}

			fold.selected = append(fold.selected, foldEntry{record: entry, body: &checkpoint.Entries[i]})
		}
	}

	return nil
}

// eachBounded runs fn over the indices 0 to n-1, at most limit of them at once,
// and stops at the first failure.
//
// It is the one worker pool this store has, and it is shared by every read that
// turns a listing into a set of small object reads — the entry bodies of a
// series, the entries of the audit log. The reads are independent round trips
// and the wall clock of a page render is how many of them run at once, so doing
// them one at a time would make a fifty-row listing fifty sequential round
// trips. It is a fixed bound rather than one goroutine per item, which AGENTS §4
// forbids and which would open a thousand connections to answer one page.
//
// The failure it reports is the first by time rather than by position:
// cancelling the rest turns their errors into "context canceled", and reporting
// one of those instead of the cause would hide what actually went wrong. The
// workers keep draining after a failure rather than returning, so the send loop
// below can never block on a pool that has gone away.
func eachBounded(ctx context.Context, n, limit int, fn func(context.Context, int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		once     sync.Once
		firstErr error
		wg       sync.WaitGroup
	)

	work := make(chan int)

	for range min(limit, n) {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range work {
				if err := fn(ctx, i); err != nil {
					once.Do(func() { firstErr = err; cancel() })
				}
			}
		}()
	}

	for i := range n {
		work <- i
	}

	close(work)
	wg.Wait()

	return firstErr
}

// hydrate reads the bodies of exactly the entries an answer is made of.
//
// Bounded concurrency, because the reads are independent round trips and a page
// render's wall clock is how many of them run at once. Entries a checkpoint
// already supplied a body for cost no request at all.
//
// An entry whose object is gone by the time it is read is dropped, counted and
// logged at Debug. The entry key and the object that carries it are the same
// object, written and deleted by the same operation, so a key that listed and
// then could not be read was deleted between the two — the ordinary consequence
// of a concurrent prune. That rule is deliberately not the one a missing
// checkpoint gets, and the reason is written on both: a checkpoint is never
// deleted, so its absence really is a fault.
func (s *Blob) hydrate(ctx context.Context, series blobSeries, entries []foldEntry) ([]entryBody, error) {
	read := make([]*entryBody, len(entries))

	err := eachBounded(ctx, len(entries), blobHydrateConcurrency, func(ctx context.Context, i int) error {
		body, err := s.readEntry(ctx, series, entries[i])
		if err != nil {
			return err
		}

		read[i] = body

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]entryBody, 0, len(entries))

	for _, body := range read {
		if body == nil {
			continue
		}

		out = append(out, *body)
	}

	if missing := len(entries) - len(out); missing > 0 {
		s.log.Debug("index entries were deleted while a series was being read",
			"entries", missing, "target", series.target, "consent_mode", string(series.mode))
	}

	return out, nil
}

// readEntry produces one selected entry's body, reading it only if the fold did
// not already hold it. A nil body with no error is an entry that has gone.
func (s *Blob) readEntry(ctx context.Context, series blobSeries, entry foldEntry) (*entryBody, error) {
	if entry.body != nil {
		return entry.body, nil
	}

	key := entry.record.String()

	raw, err := s.getIndex(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil //nolint:nilnil // an entry deleted mid-read is neither a body nor a failure; see the doc comment
		}

		return nil, err
	}

	return s.decodeEntry(series, entry.record, raw), nil
}

// decodeEntry turns one entry object's bytes into the scan it records, and
// never fails.
//
// An object that is present and unusable is reported as a scan whose summary
// could not be recovered, carrying what its own key still says: when it started,
// which is exact, and its scan ID and termination where those were not hashed
// into the key. Dropping it would hide a scan that happened and failing the
// whole read would hide the rest of the history, and the SQL stores already
// answer the same question the same way (see underivedSummary).
func (s *Blob) decodeEntry(series blobSeries, record seriesRecord, raw []byte) *entryBody {
	var body entryBody

	if err := json.Unmarshal(raw, &body); err != nil {
		s.log.Warn("an index entry does not decode and is reported without its summary",
			"key", record.String(), "error", err)

		return damagedEntry(series, record)
	}

	if !body.describes(record) {
		s.log.Warn("an index entry does not describe the scan its own key names",
			"key", record.String(), "scan_id", truncateForMessage(body.Summary.ScanID))

		return damagedEntry(series, record)
	}

	return &body
}

// damagedEntry is what a listing shows for an entry object it could not read.
func damagedEntry(series blobSeries, record seriesRecord) *entryBody {
	return &entryBody{
		Layout: indexLayoutVersion,
		Summary: Summary{
			ScanID:      literalOrEmpty(string(record.scan)),
			Target:      series.target,
			ConsentMode: series.mode,
			StartedAt:   instantAt(record.inv),
			Termination: model.TerminationReason(literalOrEmpty(string(record.term))),
			Error:       unreadableEntry,
		},
	}
}

// --- the reads ------------------------------------------------------------

// ListResults returns the newest summaries for one target and consent mode.
//
// One listing of the series directory, and one read per summary returned. It
// reads no result document at all, which is Story 8.3, AC2 for a store whose
// index is itself the bucket: the strict form of that criterion — zero bucket
// operations — cannot hold where the index is objects, and the property that
// matters is that a listing never fetches the megabytes a scan's document runs
// to. TestListingNeedsNoDocumentAtAll asserts exactly that by deleting every
// document and requiring the summaries to come back.
//
// A limit of zero means all, as it does for every store, bounded by
// blobMaxHydrate. The bound is real and is stated rather than hidden: a series
// with more scans than that is listed one page deep. Nothing in wsaw asks for
// more — the HTTP API clamps its own limit to the same number — and an unbounded
// read here is an unbounded number of requests (AC11).
func (s *Blob) ListResults(target string, mode model.ConsentMode, limit int) ([]Summary, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	entries, err := s.foldKeys(ctx, series, hydrateLimit(limit), false, nil)
	if err != nil {
		return nil, fmt.Errorf("listing results for %s: %w", series, err)
	}

	bodies, err := s.hydrate(ctx, series, entries)
	if err != nil {
		return nil, fmt.Errorf("listing results for %s: %w", series, err)
	}

	var out []Summary

	for i := range bodies {
		out = append(out, bodies[i].Summary)
	}

	return out, nil
}

// hydrateLimit turns the interface's "zero means all" into the bounded number
// of bodies one call may read.
func hydrateLimit(limit int) int {
	if limit <= 0 || limit > blobMaxHydrate {
		return blobMaxHydrate
	}

	return limit
}

// HasResult reports whether one scan is in the history, without reading it.
//
// It is attributes on one key: no listing, and not the body either. The
// interface asks this to decide whether it may offer a link, and a store that
// fetched a body to answer would be reading a scan's index entry to find out
// whether to show an anchor (Story 8.3, AC3).
func (s *Blob) HasResult(target string, mode model.ConsentMode, scanID string) (bool, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	if _, err := s.statIndex(ctx, series.id.byIDKey(sk(scanID))); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Not there is not there. Whether that is "not yet" or "never" is a
			// question this store cannot answer and does not pretend to: what
			// it must not do is report it as a result that was deleted (AC6).
			return false, nil
		}

		return false, fmt.Errorf("checking for result %s in %s: %w", scanID, series, err)
	}

	return true, nil
}

// GetResult reads one scan by its identity.
//
// Two reads and no listing: the object addressed by scan ID, then the document
// it names. It is deliberately not a fold — a scan ID addresses a key directly,
// and a lookup that had to list a series would get slower as the history grew
// and would be answerable only from a listing that may lag.
func (s *Blob) GetResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	_, res, err := s.resultByID(ctx, blobSeriesFor(target, mode), scanID)

	return res, err
}

// resultByID reads one scan by its identity, and returns its index entry beside
// the document.
//
// Both halves are returned because the baseline path needs both: the document is
// the copy an approval embeds, and the entry is where the reference to that
// document is recorded, which is what the approval's pins are derived from. It
// is one function rather than two so that the errors a caller sees for a scan
// that is not there read the same whether it was asked for by a reader or by an
// approval — and the same as the SQL stores' resultByID, whose messages these
// are.
func (s *Blob) resultByID(
	ctx context.Context,
	series blobSeries,
	scanID string,
) (entryBody, *model.Result, error) {
	entry, err := s.entryAt(ctx, series, sk(scanID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return entryBody{}, nil, fmt.Errorf("result %s for %s: %w", scanID, series, ErrNotFound)
		}

		return entryBody{}, nil, fmt.Errorf("reading result %s: %w", scanID, err)
	}

	res, err := s.document(ctx, series, entry)
	if err != nil {
		return entryBody{}, nil, err
	}

	return entry, res, nil
}

// entryAt reads the object one scan is addressed by, and checks that what it
// holds is the scan its key names.
//
// It returns a bare ErrNotFound: what an absent key means depends on which
// question was asked — no such scan, or no scan before the one that was named —
// and the caller is the only one that knows which.
func (s *Blob) entryAt(ctx context.Context, series blobSeries, scan encodedScan) (entryBody, error) {
	key := series.id.byIDKey(scan)

	raw, err := s.getIndex(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return entryBody{}, ErrNotFound
		}

		return entryBody{}, err
	}

	var body entryBody

	if err := json.Unmarshal(raw, &body); err != nil {
		// A read that names one scan has nowhere to put a partial answer, so
		// this is the path §6.7 calls "cannot skip": the object is present and
		// wrong, which is corruption and is never absence.
		return entryBody{}, fmt.Errorf("the index object %s does not decode: %w: %w", key, ErrCorrupt, err)
	}

	if sk(body.Summary.ScanID) != scan ||
		tk(body.Summary.Target) != series.id.target ||
		mk(body.Summary.ConsentMode) != series.id.mode {
		return entryBody{}, fmt.Errorf("the index object %s records scan %q of %q instead: %w",
			key, truncateForMessage(body.Summary.ScanID), truncateForMessage(body.Summary.Target), ErrCorrupt)
	}

	return body, nil
}

// document reads the stored document one index entry names.
func (s *Blob) document(ctx context.Context, series blobSeries, entry entryBody) (*model.Result, error) {
	if entry.Document.Ref == "" {
		// Every entry this store writes names a document, and one that does not
		// is an object that has been damaged or written by something else. It
		// is not the SQL stores' "not migrated yet" case, which has no
		// counterpart here.
		return nil, fmt.Errorf("the index entry for scan %s of %s names no stored document: %w",
			truncateForMessage(entry.Summary.ScanID), series, ErrCorrupt)
	}

	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return s.bucket.resultDocument(ctx, entry.Document.resultRef())
}

// LatestResult reads the newest scan of one target and consent mode.
func (s *Blob) LatestResult(target string, mode model.ConsentMode) (*model.Result, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	entry, err := s.foldOne(ctx, series, false, nil)
	if err != nil {
		return nil, fmt.Errorf("reading the latest result for %s: %w", series, err)
	}

	return s.document(ctx, series, entry)
}

// foldOne selects the newest entry a query admits and reads its body.
//
// It folds again when the entry it chose turned out to have been deleted
// between the listing and the read, because the answer in that case is not "the
// series is empty" — it is whatever the series looks like now that the prune has
// happened. Once, not in a loop: a second disagreement is a bucket losing
// objects faster than they can be read, and reporting that as no history would
// be worse than reporting it as nothing found.
func (s *Blob) foldOne(
	ctx context.Context,
	series blobSeries,
	skipFailed bool,
	before *seriesRecord,
) (entryBody, error) {
	for range 2 {
		entries, err := s.foldKeys(ctx, series, 1, skipFailed, before)
		if err != nil {
			return entryBody{}, err
		}

		if len(entries) == 0 {
			break
		}

		bodies, err := s.hydrate(ctx, series, entries)
		if err != nil {
			return entryBody{}, err
		}

		if len(bodies) == 1 {
			return bodies[0], nil
		}
	}

	return entryBody{}, ErrNotFound
}

// PreviousResult reads the scan a given one should be compared against,
// skipping the failed ones that are not a baseline for anything.
//
// A failed or skipped scan stays in the history — it happened, and hiding it
// would be the quiet lie Tenet 5 forbids — but it is not an observation of the
// site, so diffing against it would report the whole site as new or as gone.
// That matters most for a retried scan: without it, a successful retry would be
// compared against the failure it replaced and manufacture a finding out of its
// own recovery (Story 3.8, AC6).
//
// The anchor is read by scan ID rather than searched for, and the skip is
// decided from the key, so a target with two hundred consecutive failures behind
// it still costs one listing.
func (s *Blob) PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	anchor, err := s.entryAt(ctx, series, sk(scanID))
	if err != nil {
		return nil, previousError(scanID, err)
	}

	// The anchor's place in the ordering, recomputed from its own body. It is a
	// pure function of the summary, which is why no entry carries a pointer
	// back to its key.
	at := anchor.record(series.id)

	chosen, err := s.previousOf(ctx, series, &at)
	if err != nil {
		return nil, previousError(scanID, err)
	}

	// Read through the scan-ID key rather than through the entry the fold
	// selected. The two carry identical bytes, and this one is the object a
	// prune deletes last, so a candidate that a concurrent prune is part way
	// through removing is still readable here.
	entry, err := s.entryAt(ctx, series, chosen)
	if err != nil {
		return nil, previousError(scanID, err)
	}

	return s.document(ctx, series, entry)
}

// previousError gives every failure of PreviousResult the shape SQL gives it,
// so that "there is nothing to compare against" reads the same from both.
func previousError(scanID string, err error) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("result before %s: %w", scanID, ErrNotFound)
	}

	return fmt.Errorf("reading result before %s: %w", scanID, err)
}

// previousOf selects the newest entry before an anchor that is an observation
// of the site, and confirms its choice against a second listing.
//
// The confirmation is one extra listing and it is worth it. A listing served
// from a stale view can omit a scan another process wrote, and the answer to
// "what came before this one" is what a comparison is made against — so a
// silently skipped scan becomes a diff against the wrong baseline. Re-reading
// cannot make that impossible, and the design says so; what it does is turn the
// case from invisible into a Warn line naming the target, most of the time.
//
// **The confirmation re-folds the whole prefix, not the range between the
// candidate and the anchor**, which is a deviation from §6.4 and doubles the
// listings this path costs: an anchor k scans deep is 2⌈k/1000⌉ listings rather
// than ⌈k/1000⌉ + 1. It is deliberate. The narrow form would only see a scan
// that appeared between the candidate and the anchor, and the case that matters
// is the opposite one — a stale listing view omitting a scan *newer* than the
// candidate, which is the entry that would have been the answer. Confirming
// with the same fold, from the same starting key, is what makes the two answers
// comparable at all. It is strictly safer than the specified form and strictly
// more expensive, and the corrected figure is recorded in the design's §7.6 so
// that the request-count assertions are written against what this does.
func (s *Blob) previousOf(ctx context.Context, series blobSeries, anchor *seriesRecord) (encodedScan, error) {
	chosen, found, err := s.foldPrevious(ctx, series, anchor)
	if err != nil {
		return "", err
	}

	confirmed, alsoFound, err := s.foldPrevious(ctx, series, anchor)
	if err != nil {
		return "", err
	}

	if chosen != confirmed || found != alsoFound {
		s.log.Warn("the history of a target changed while the scan before another was being chosen; reading it again",
			"target", series.target, "consent_mode", string(series.mode),
			"first", string(chosen), "second", string(confirmed))

		chosen, found, err = s.foldPrevious(ctx, series, anchor)
		if err != nil {
			return "", err
		}
	}

	if !found {
		return "", ErrNotFound
	}

	return chosen, nil
}

// foldPrevious is one fold for the entry before an anchor, reporting separately
// whether there was one so that an appearing entry is a change the confirmation
// can see.
func (s *Blob) foldPrevious(
	ctx context.Context,
	series blobSeries,
	anchor *seriesRecord,
) (encodedScan, bool, error) {
	entries, err := s.foldKeys(ctx, series, 1, true, anchor)
	if err != nil {
		return "", false, err
	}

	if len(entries) == 0 {
		return "", false, nil
	}

	return entries[0].record.scan, true, nil
}

// Series lists every target and consent mode the store holds a history for.
//
// One listing of the target markers, and one read of each, because the key
// carries a hashed target and the caller needs the target itself. The order is
// produced in memory: the keys sort by digest and the SQL stores sort by target,
// and the interface's promise is one order rather than one implementation's.
func (s *Blob) Series() ([]Series, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	return s.seriesList(ctx)
}

// seriesList is Series under a context the caller owns.
//
// Retention needs the same listing and already holds a context — a prune is
// bounded by the operator's patience rather than by blobOpBudget, which is why
// Prune takes one — so the walk is a function of the context rather than of
// which method called it.
func (s *Blob) seriesList(ctx context.Context) ([]Series, error) {
	var (
		out    []Series
		cursor indexCursor
	)

	for cursor.more() {
		page, err := s.listIndex(ctx, indexTargetsPrefix, cursor, indexListPageSize)
		if err != nil {
			return nil, fmt.Errorf("listing the series in %s: %w", s.bucket, err)
		}

		cursor = page.next

		found, err := s.readSeriesPage(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("listing the series in %s: %w", s.bucket, err)
		}

		out = append(out, found...)
	}

	slices.SortFunc(out, func(a, b Series) int {
		if order := strings.Compare(a.Target, b.Target); order != 0 {
			return order
		}

		return strings.Compare(string(a.Mode), string(b.Mode))
	})

	return out, nil
}

// readSeriesPage reads the markers one page of the target listing named.
func (s *Blob) readSeriesPage(ctx context.Context, page indexPage) ([]Series, error) {
	out := make([]Series, 0, len(page.objects))

	for _, object := range page.objects {
		id, err := parseTargetMarkerKey(object.key)
		if err != nil {
			s.log.Warn("an object among the series markers is not a key this store writes, and was ignored",
				"key", object.key)

			continue
		}

		raw, err := s.getIndex(ctx, object.key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Deleted between the listing and the read, which is what a
				// prune does to a series whose whole history has expired.
				s.log.Debug("a series marker was deleted while the series were being listed", "key", object.key)

				continue
			}

			return nil, err
		}

		var marker seriesMarker

		if err := json.Unmarshal(raw, &marker); err != nil {
			// Reported rather than skipped, and this is the one listing in the
			// store where that is the right way round: the target a marker
			// names is not recoverable from its key, which is a hash, so a
			// marker that cannot be read is a whole target that would silently
			// disappear from every dashboard rather than one row that would look
			// odd on one.
			return nil, fmt.Errorf("the series marker %s does not decode: %w: %w", object.key, ErrCorrupt, err)
		}

		if seriesFor(marker.Target, marker.Mode) != id {
			return nil, fmt.Errorf("the series marker %s names %q, which is not the series its key spells: %w",
				object.key, truncateForMessage(marker.Target), ErrCorrupt)
		}

		out = append(out, Series{Target: marker.Target, Mode: marker.Mode})
	}

	return out, nil
}
