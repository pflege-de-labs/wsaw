package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file rebuilds an index from the documents the bucket holds (Story 8.11).
//
// Once every scan's document is an object, the index stops being the record and
// becomes a derived view of it: an ordered list of pointers plus the summaries
// computed from the documents they point at. Derived data is disposable and raw
// capture is not (Tenet 4) — but only where the derivation can actually be
// re-run, and that is what this is. Without it a dropped database, a
// half-finished migration, an entry that never became visible under eventual
// consistency (Story 8.10, AC6) or a bucket restored without its database each
// leave evidence that exists and cannot be found.
//
// It is also the only supported way between store kinds. Story 8.10, AC16
// refuses a migration from SQL to the bucket index and back; all four stores
// keep their documents in the same layout, so pointing the new store at the
// same bucket and rebuilding is the conversion, and there is no second format
// to keep correct.
//
// **The strategy is merge, and nothing is ever deleted.** AC4 asks that the
// existing index survive a rebuild that is interrupted at any point, and offers
// two ways to get there: build the new index beside the old one and swap, or
// merge into it. Merge is what this does, for one reason that is decisive and
// one that is merely true.
//
// The decisive one is that the bucket-index store cannot swap. A swap needs
// either a rename of a whole key space, which no provider offers, or a second
// prefix plus one atomic pointer flip, which is a compare-and-swap that
// Story 8.10 established does not exist portably — and a store that keeps its
// index in the bucket is exactly the store this command exists for. Half a swap
// against object storage is two indexes and no way to say which is live, which
// is worse than the state it was recovering from.
//
// The merely-true one is that a merge needs no swap. Every index record this
// command writes is derived from a content-addressed document and keyed by the
// scan it records, so writing it twice writes the same bytes to the same place:
// an interrupted run leaves a partially rebuilt index, which is a readable
// store, and running it again finishes the job without redoing a single write.
//
// What follows from merge is that the command **removes nothing**. An index
// entry whose document is not in the bucket is reported and left exactly where
// it is, because the two things that produce it are a bucket that lost an
// object and a listing that has not caught up, and only one of those is a
// reason to delete a compliance record. Discarding history in order to rebuild
// it is the one failure AC4 says this command must not have.

// ErrIndexDrift reports that verify found the index and the bucket disagreeing,
// or found an object under the result prefix that cannot be read back as a scan.
//
// It is a sentinel so that the command can exit non-zero on drift and zero on
// none, which is what lets verify run on a schedule or in CI rather than being
// remembered after an incident (AC10). It is deliberately not returned by a
// rebuild: a rebuild that added entries has repaired the disagreement it found,
// and failing afterwards would report a success as a failure.
//
// A damaged document is in it as well as a drifted entry, and the reason is
// what a scheduled verify is for. A `result/` object that no longer hashes to
// its key is evidence this store can no longer produce — a truncated upload, a
// tampered object, a provider that lost bytes — and a verify that exited zero
// over one would be a green cron job standing over unreadable evidence, which
// is the failure Tenet 5 names. AC7's "never fatal" governs whether the run
// aborts, which it does not; it does not govern what the run exits with.
var ErrIndexDrift = errors.New("the index and the artifact bucket do not agree")

// RebuildMode is which of the three things `wsaw store rebuild-index` does.
//
// One method with a mode rather than the Plan-and-apply pair the prune and the
// sweep use, because there are three of them and the third is not a plan of the
// first: verify answers a different question (does the index agree with the
// bucket) from the one a dry run answers (what would a rebuild change). Three
// methods on the store seam for one operation would be the alternative, and
// every implementation would dispatch them into one function anyway.
type RebuildMode int

const (
	// RebuildApply merges what the bucket holds into the index.
	RebuildApply RebuildMode = iota
	// RebuildPlan reports what a rebuild would change and writes nothing.
	RebuildPlan
	// RebuildVerify compares the index against the bucket, changes nothing,
	// and reports drift in both directions.
	RebuildVerify
)

// String names the mode the way a report should.
func (m RebuildMode) String() string {
	switch m {
	case RebuildPlan:
		return "dry run"
	case RebuildVerify:
		return "verify"
	case RebuildApply:
		return "rebuild"
	default:
		return "rebuild"
	}
}

// writes reports whether this mode may change the index. Only one of the three
// does, and every write in this file is behind it.
func (m RebuildMode) writes() bool { return m == RebuildApply }

const (
	// rebuildBatchSize is how many stored documents one batch reads.
	//
	// It bounds the work in progress rather than the memory in the way the
	// document migration's own batch does (documentBatchSize): a batch is a
	// page of keys, and each document is read, decoded, recorded and released
	// before the next batch is listed, so a bucket with a hundred thousand
	// results costs no more memory than one with ten (AC3).
	rebuildBatchSize = 256

	// rebuildConcurrency is how many objects are read at once by default.
	//
	// Reading a stored document is an independent round trip, and against
	// object storage the round trip is the whole cost, so eight of them in
	// flight is roughly eight times the throughput of one. It is a default
	// rather than a constant because what the right number is depends on the
	// provider, the link and how much of it an operator is willing to spend on
	// a maintenance task (AC11).
	rebuildConcurrency = 8

	// rebuildMaxConcurrency is as far as --concurrency may be raised. Beyond a
	// couple of hundred requests in flight the limit is the provider's rate
	// limiter rather than wsaw, and a rebuild that trips it is slower than one
	// that did not.
	rebuildMaxConcurrency = 256

	// rebuildProgressEvery is how many objects pass between progress lines. A
	// rebuild of a large bucket runs for a long time, and an operator watching
	// it needs to see that it is advancing rather than hung (AC3).
	rebuildProgressEvery = 1000
)

// RebuildOptions is what an operator asked for.
type RebuildOptions struct {
	// Mode is rebuild, dry run or verify.
	Mode RebuildMode

	// Concurrency is how many objects are read at once. Zero takes the
	// default; the value is clamped rather than refused, because a rebuild
	// that would not start over an out-of-range number helps nobody.
	Concurrency int

	// Limit is how many keys the report names. Zero names all of them.
	Limit int

	// Now is the instant the orphan survey is measured against, so a test can
	// place an artifact outside the grace period without waiting a day. The
	// zero value means the real clock.
	Now time.Time
}

// workers is the read fan-out this run may use.
func (o RebuildOptions) workers() int {
	if o.Concurrency <= 0 {
		return rebuildConcurrency
	}

	return min(o.Concurrency, rebuildMaxConcurrency)
}

// at is the instant this run measures against.
func (o RebuildOptions) at() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}

	return o.Now
}

// DamagedObject is one object under the result prefix that could not be turned
// back into a scan: corrupt bytes, a digest that does not match the key it is
// stored under, or JSON this build cannot read.
//
// It is named by key and counted, never fatal and never left out of the summary
// (AC7). One unreadable object out of a hundred thousand must not stop the
// other ninety-nine thousand from being indexed, and it must not vanish from
// the report either — a rebuild that quietly skipped it would present a store
// that merely looks intact.
type DamagedObject struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// EvidenceGone is one screenshot or stored body that a result names and the
// bucket no longer holds (AC8).
//
// It is named and not merely counted, because the count alone tells an operator
// that evidence has gone and not which scan lost what — and a lifecycle rule
// that expired more than its author meant to is diagnosed from the keys.
type EvidenceGone struct {
	// Scan is the scan that names it, spelled as the report spells a subject.
	Scan string `json:"scan"`
	// Artifact is the reference that no longer resolves.
	Artifact string `json:"artifact"`
}

// IndexDrift is one disagreement between the index and the bucket.
type IndexDrift struct {
	// Kind is which of the disagreements this is, in the words the report
	// prints. The set is the constants below.
	Kind string `json:"kind"`
	// Subject names what disagrees: a scan, a decision, or an object key.
	Subject string `json:"subject"`
	// Detail is what the check actually found, where the kind alone does not
	// say it.
	Detail string `json:"detail,omitempty"`
}

// The kinds of drift a verify reports. They are constants because the report
// groups by them and because a phrase repeated at three call sites is a phrase
// that comes to be spelled three ways.
const (
	driftDocumentGone    = "an index entry names a document the bucket does not hold"
	driftEntryMissing    = "a stored document has no index entry"
	driftEntryPartial    = "a stored document is only half indexed"
	driftSummaryStale    = "an index summary no longer matches the document it describes"
	driftScanIDReused    = "one scan ID names two different documents"
	driftAuditMissing    = "a baseline decision has no entry in the audit log"
	driftPinMissing      = "a stored result names an artifact that nothing pins"
	driftIndexUnreadable = "an index object does not decode"
)

// Evidence a result names and the bucket no longer holds is deliberately not in
// that list.
//
// AC8 defines it as the recorded outcome rather than as a fault: the reference
// stays, the result reads as evidence that is no longer stored, and the run
// counts it. AC10 enumerates drift as entries pointing at gone documents,
// documents with no entry, and stale summaries — a lifecycle rule that expires
// screenshots at ninety days (Story 8.6, AC7) is none of those, and counting it
// as drift would make the scheduled verify permanently red on a deployment
// behaving exactly as its operator configured it. It is reported by count and
// by key, in its own section, and it does not change the exit code.

// Unrecoverable is what a rebuild preserves where it still exists and cannot
// recreate where it does not (AC6).
//
// A rebuild derives the index from the documents, and a document records a
// scan. Baselines and their approvals, the audit log and the change history a
// review reads are decisions and observations about scans, not properties of
// them, and no document contains one. So this is a count of what the index
// still holds, printed prominently rather than as a footnote, and a zero here
// after a lost index is a loss to be reported and not a store to be presented
// as intact.
//
// Share tokens are absent from it on purpose: wsaw's share links are signed
// with api.share.key and are not stored anywhere, so there is nothing for a
// rebuild to lose (Story 5.19).
type Unrecoverable struct {
	// Baselines is how many baseline records the index holds. For a SQL store
	// that is one row per approved target and mode; for the bucket index it is
	// one object per decision, approvals and withdrawals alike, because that
	// store keeps the log rather than the current value.
	Baselines int `json:"baselines"`

	// AuditEntries is how many entries the audit log holds.
	AuditEntries int `json:"auditEntries"`

	// Counted reports whether the two numbers above were established at all. A
	// store that could not answer must not be reported as a store that answered
	// zero.
	Counted bool `json:"counted"`
}

// Empty reports whether the index holds none of what a rebuild cannot restore.
func (u Unrecoverable) Empty() bool { return u.Counted && u.Baselines == 0 && u.AuditEntries == 0 }

// RebuildStats is the account of one run, and it is the whole of what the
// command prints.
//
// Every number is here rather than in the log, because a rebuild is a recovery
// procedure: what it found, what it changed, what it could not read and what it
// could not restore are the answer an operator needs in one place, at the
// moment they are deciding whether the store is now trustworthy.
type RebuildStats struct {
	Driver string `json:"driver"`
	Bucket string `json:"bucket"`
	Mode   string `json:"mode"`

	// ObjectsFound and ObjectBytes are what the result prefix holds.
	ObjectsFound int   `json:"objectsFound"`
	ObjectBytes  int64 `json:"objectBytes"`

	// ResultsDecoded is how many of those turned back into a scan.
	ResultsDecoded int `json:"resultsDecoded"`

	// Damaged counts the ones that did not, and DamagedKeys names the first
	// few (AC7).
	Damaged     int             `json:"damaged"`
	DamagedKeys []DamagedObject `json:"damagedKeys,omitempty"`

	// EntriesFound is how many scans the index recorded before this run.
	EntriesFound int `json:"entriesFound"`

	// EntriesAdded, EntriesRepaired and EntriesRefreshed are the three ways a
	// document changes an index: it was not there, it was there in part, or it
	// was there with a summary that no longer matches the document. In a dry
	// run and in a verify they are what would happen (AC5).
	EntriesAdded     int `json:"entriesAdded"`
	EntriesRepaired  int `json:"entriesRepaired"`
	EntriesRefreshed int `json:"entriesRefreshed"`

	// EntriesUnchanged is the second run's answer, and the whole of it: a
	// rebuild over an index that is already complete changes nothing (AC3).
	EntriesUnchanged int `json:"entriesUnchanged"`

	// DocumentsPruned counts stored documents whose scan this index records as
	// deliberately removed, and PrunedScans names the first few.
	//
	// They are the one category that must never be added back. Retention
	// deletes a result and keeps the artifacts something else still names
	// (Story 8.5, AC1), so a scan a baseline pinned leaves its document in the
	// bucket for ever — and a rebuild that read "there is a document, so there
	// should be an entry" would undo a deletion made to satisfy a retention
	// obligation, silently, and report it as work done. They are named as their
	// own category in the report (AC5) rather than folded into the entries a
	// run would add, and a verify does not call them drift: an index that has
	// correctly forgotten a pruned scan is not an index that disagrees with its
	// bucket.
	DocumentsPruned int      `json:"documentsPruned"`
	PrunedScans     []string `json:"prunedScans,omitempty"`

	// Conflicts counts scans whose index record names a different document,
	// and IndexUnreadable counts index objects that are present and do not
	// decode.
	//
	// Both are outcomes no rebuild can repair — resolving a conflict means
	// choosing which of two scans to discard, and an unreadable index object
	// sits at a key that is already taken — so they are counted apart from the
	// entries that were left alone because they already agreed. Without them
	// the report's own numbers do not add up, and its closing sentence would
	// claim a completeness the run did not reach.
	Conflicts       int `json:"conflicts"`
	IndexUnreadable int `json:"indexUnreadable"`

	// DocumentsNewer counts stored documents whose schemaVersion this build
	// does not understand, and NewerDocuments names the first few.
	//
	// Nothing else gates a rebuild on the document format, and without this it
	// would be the one operation in wsaw that lets an older binary rewrite what
	// a newer one recorded. readDocument decodes with a plain unmarshal, which
	// drops every field this build does not know; the summary derived from that
	// decode is lossy by exactly those fields; and the merge would then call the
	// row stale and rewrite it downwards, reporting it as a repair. A store
	// refuses a newer schema (Story 4.6, AC5) and the bucket index refuses a
	// newer layout (Story 8.10, AC14) for the same reason, and neither of those
	// gates covers a change to what a document holds.
	//
	// The document itself is untouched and is still readable by the build that
	// wrote it, so nothing is lost — what is refused is the derivation
	// (Tenet 4, Tenet 16).
	DocumentsNewer int      `json:"documentsNewer"`
	NewerDocuments []string `json:"newerDocuments,omitempty"`

	// EntriesRemoved is always zero, and it is reported anyway. AC5 asks a dry
	// run to say how many entries would be removed, and the honest answer for
	// a merge is none: see EntriesStale for what a destructive rebuild would
	// have deleted and this one reports instead.
	EntriesRemoved int `json:"entriesRemoved"`

	// SummariesStale counts index entries whose summary no longer matches the
	// document they describe and whose store cannot correct it.
	//
	// It exists because the bucket index cannot: an entry's key is a function
	// of the scan it records, the key is already written, and no index object
	// is ever overwritten (Story 8.10, AC3). A SQL index re-derives the columns
	// instead and counts it as EntriesRefreshed. Reporting the two apart is
	// what stops a rebuild claiming a repair it could not make.
	SummariesStale int `json:"summariesStale"`

	// EntriesStale counts index entries naming a document the bucket does not
	// hold. They are kept, because a lifecycle rule that removed evidence and
	// a listing that has not caught up look identical from here, and only one
	// of them is a reason to delete a record of a scan.
	EntriesStale int `json:"entriesStale"`

	// Drift and DriftList are what a verify found, in both directions (AC10).
	Drift     int          `json:"drift"`
	DriftList []IndexDrift `json:"driftList,omitempty"`

	// EvidenceMissing counts screenshots and stored bodies that a decoded
	// result names and the bucket no longer holds; ResultsMissingEvidence is
	// how many results that is spread over.
	//
	// The references are kept and the result reads as evidence that is no
	// longer stored, never repaired by dropping the reference (AC8, Tenet 5).
	EvidenceMissing        int            `json:"evidenceMissing"`
	ResultsMissingEvidence int            `json:"resultsMissingEvidence"`
	MissingEvidence        []EvidenceGone `json:"missingEvidence,omitempty"`

	// The survey of the whole bucket (AC9). A rebuild is the one operation
	// that sees every key, so it is the cheapest place to learn what is in it.
	BucketObjects     int   `json:"bucketObjects"`
	BucketBytes       int64 `json:"bucketBytes"`
	Unreferenced      int   `json:"unreferenced"`
	UnreferencedBytes int64 `json:"unreferencedBytes"`
	ForeignObjects    int   `json:"foreignObjects"`

	// OrphanNote says why the survey above is missing, where it is. It is
	// never fatal: an orphan count is information, and a rebuild that refused
	// to finish because it could not gather it would be trading the recovery
	// for the report.
	OrphanNote string `json:"orphanNote,omitempty"`

	// What the run cost, in the units the invoice is in (AC11). Retries are
	// not counted: these are the requests the rebuild asked for, and the store
	// logs and counts the ones it had to repeat.
	Reads     int   `json:"reads"`
	Checks    int   `json:"checks"`
	Writes    int   `json:"writes"`
	Listings  int   `json:"listings"`
	BytesRead int64 `json:"bytesRead"`

	// Unrecoverable is what no document can produce (AC6).
	Unrecoverable Unrecoverable `json:"unrecoverable"`
}

// Requests is what the run asked the bucket and the index for, all told.
func (s RebuildStats) Requests() int { return s.Reads + s.Checks + s.Writes + s.Listings }

// Changed reports whether the index gained anything.
func (s RebuildStats) Changed() bool {
	return s.EntriesAdded+s.EntriesRepaired+s.EntriesRefreshed > 0
}

// Complete reports whether every scan the bucket could produce an entry for now
// has one.
//
// It is what the closing sentence is allowed to claim. A run that skipped a
// damaged object, refused a scan ID naming two documents, or met an index object
// it could not decode has recovered part of a store, and saying "the index now
// records every scan the bucket could produce one for" over that would be a
// completion claim over a partial recovery — with the drift list, which --limit
// trims, as the only contradiction.
func (s RebuildStats) Complete() bool {
	return s.Damaged == 0 && s.Conflicts == 0 && s.IndexUnreadable == 0 && s.DocumentsNewer == 0
}

// Unjudged is what a verify could not form an opinion about.
//
// It is what makes verify exit non-zero over something other than drift: an
// object under the result prefix that does not decode, and a document this
// build's result schema cannot read, are both evidence the run could not judge
// — and a scheduled verify that exited zero over either would be a green cron
// job standing over a store nobody has actually checked (Tenet 5).
func (s RebuildStats) Unjudged() int { return s.Damaged + s.DocumentsNewer }

// rebuildOutcome is what recording one document did, or would do.
type rebuildOutcome int

const (
	// rebuildAdded: the index had no record of this scan at all.
	rebuildAdded rebuildOutcome = iota
	// rebuildRepaired: part of the record was there and part was not, which is
	// what an interrupted write leaves.
	rebuildRepaired
	// rebuildRefreshed: the record is there and its summary no longer matches
	// what the document says, which is what a change to summarize() leaves.
	rebuildRefreshed
	// rebuildStale: the index disagrees with the document and this store
	// cannot rewrite the record that disagrees.
	rebuildStale
	// rebuildUnchanged: the index already says exactly this.
	rebuildUnchanged
	// rebuildConflict: the index records another document under this scan ID.
	// Never resolved by overwriting one of them; reported, and both kept.
	rebuildConflict
	// rebuildUnreadable: the index has a record for this scan and it does not
	// decode. Neither unchanged nor repairable — the key is taken.
	rebuildUnreadable
	// rebuildPruned: the index records that this scan was removed, and the
	// document survived because something else still names it. Never added.
	rebuildPruned
	// rebuildNewer: the document was written by a build whose result schema
	// this one does not understand, so no derivation from it is trustworthy.
	rebuildNewer
)

// rebuiltDocument is one stored document, read back and verified against the
// content address it was found under.
type rebuiltDocument struct {
	result *model.Result
	ref    resultRef
}

// subject names this scan the way a report and a log line should.
func (d rebuiltDocument) subject() string {
	return d.result.Target + "/" + string(d.result.ConsentMode) + " " + d.result.ScanID
}

// indexedScan is one scan the index already records, reduced to what the survey
// of an existing index needs: who it is, and where it says its document is.
type indexedScan struct {
	target   string
	mode     model.ConsentMode
	scanID   string
	document string
}

// subject names this scan the way a report should.
func (s indexedScan) subject() string {
	return s.target + "/" + string(s.mode) + " " + s.scanID
}

// indexRebuild is what a store has to be able to do for its index to be
// rebuildable from the bucket it shares with the evidence.
//
// It is unexported and declared here, beside the one function that consumes it,
// because it is a seam inside the package rather than a promise to the rest of
// wsaw: what a caller asks for is Store.RebuildIndex, and this is how the two
// implementations of it share the half that is the same. That half is
// everything about the bucket — listing the documents, reading them, verifying
// them, deriving from them, surveying the orphans and printing the account —
// and it is the larger half.
type indexRebuild interface {
	// Driver names the store kind for the report.
	Driver() string

	// rebuildBucket is where the documents and the evidence are.
	rebuildBucket() *bucket

	// rebuildLog is where progress goes.
	rebuildLog() *slog.Logger

	// beginRebuild records that a rebuild is running, so that a sweep started
	// against a half-rebuilt index collects nothing (AC12), and returns the
	// function that clears the record.
	beginRebuild(ctx context.Context, run *rebuildRun) (func(), error)

	// surveyIndex streams what the index already records and checks that each
	// entry's document is still in the bucket. It is the half of the drift
	// question the documents cannot answer.
	surveyIndex(ctx context.Context, run *rebuildRun) error

	// prunedScan reports whether this index records that the scan was
	// deliberately removed, so that a rebuild puts back the scans whose entry
	// never landed and not the ones retention took out.
	//
	// It is on the seam because the two indexes record it differently and
	// neither can be derived from the other: the bucket index appends a
	// tombstone in the series directory (Story 8.10, AC12), while a SQL index
	// records it in the artifact references a pruned result leaves behind. What
	// they have in common is that both answers come from the index, so a store
	// whose index is genuinely gone answers "no" to all of them and a rebuild
	// recovers the whole bucket — which is the recovery this command is for.
	prunedScan(ctx context.Context, run *rebuildRun, doc rebuiltDocument) (bool, error)

	// mergeRebuilt records one decoded document, or reports what recording it
	// would do. It writes only where the mode says it may.
	mergeRebuilt(ctx context.Context, run *rebuildRun, doc rebuiltDocument) (rebuildOutcome, error)

	// indexOnlyRecords counts what the index holds that no document can
	// produce (AC6).
	indexOnlyRecords(ctx context.Context, run *rebuildRun) (Unrecoverable, error)

	// indexOnlyDrift reports the disagreements only this kind of index can
	// have — for the bucket index, a decision whose audit pointer never landed.
	indexOnlyDrift(ctx context.Context, run *rebuildRun) error

	// PlanSweep answers "what does this bucket hold that nothing references",
	// deleting nothing (AC9).
	PlanSweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error)
}

// The compile-time proof that both stores can be rebuilt, next to the contract
// that says what that takes.
var (
	_ indexRebuild = (*SQL)(nil)
	_ indexRebuild = (*Blob)(nil)
)

// rebuildRun is one run: what was asked for, what has been found, and the two
// handles every step needs.
//
// The statistics are mutated from one goroutine only. Reads fan out (AC11's
// configurable concurrency) and their results are folded in afterwards, in
// order, by the goroutine that started them — which is what makes the counts
// exact and the report reproducible without a mutex in front of every counter.
type rebuildRun struct {
	opts   RebuildOptions
	log    *slog.Logger
	bucket *bucket
	stats  RebuildStats

	// logged is how many objects had passed at the last progress line.
	logged int

	// series is the bucket-index store's scratch: what each series directory
	// says about which scans are tombstoned and which a checkpoint covers,
	// worked out once per series instead of once per document. It is nil for a
	// SQL index, which answers both questions with a query. See
	// blobSeriesFacts in rebuildblob.go.
	series map[seriesID]*blobSeriesFacts
}

// read records one object read in full.
func (r *rebuildRun) read(bytes int64) {
	r.stats.Reads++
	r.stats.BytesRead += bytes
}

// checked records existence checks, which are the cheap requests.
func (r *rebuildRun) checked(n int) { r.stats.Checks += n }

// wrote records index records written. It is called by the store-specific
// halves, which are the only things in this file's world that write.
//
// The rebuild marker's own write and delete are deliberately not in it. What
// the cost report is for is sizing a rebuild of a large bucket, which is a
// question about what scales with the history; two fixed requests per run are
// noise in that number, and leaving them out is also what lets "a second run
// writes nothing" be asserted as the zero it is (AC3).
func (r *rebuildRun) wrote(n int) { r.stats.Writes += n }

// listed records listing pages.
func (r *rebuildRun) listed(n int) { r.stats.Listings += n }

// damaged records an object that could not be turned back into a scan (AC7).
func (r *rebuildRun) damaged(key, reason string) {
	r.stats.Damaged++

	if r.opts.Limit <= 0 || len(r.stats.DamagedKeys) < r.opts.Limit {
		r.stats.DamagedKeys = append(r.stats.DamagedKeys, DamagedObject{Key: key, Reason: reason})
	}

	r.log.Warn("an object under the result prefix could not be read back as a scan, so it was skipped",
		"key", truncateForMessage(key), "reason", reason, "bucket", r.bucket.String())
}

// drifted records one disagreement between the index and the bucket.
func (r *rebuildRun) drifted(kind, subject, detail string) {
	r.stats.Drift++

	if r.opts.Limit <= 0 || len(r.stats.DriftList) < r.opts.Limit {
		r.stats.DriftList = append(r.stats.DriftList, IndexDrift{Kind: kind, Subject: subject, Detail: detail})
	}
}

// progress reports that the run is advancing, which over a bucket that takes
// an hour to read is the difference between a long job and one that looks hung
// (AC3).
func (r *rebuildRun) progress(force bool) {
	if !force && r.stats.ObjectsFound-r.logged < rebuildProgressEvery {
		return
	}

	r.logged = r.stats.ObjectsFound

	r.log.Info("rebuilding an index from the documents in the bucket",
		"mode", r.stats.Mode, "driver", r.stats.Driver, "bucket", r.bucket.String(),
		"objects", r.stats.ObjectsFound, "decoded", r.stats.ResultsDecoded,
		"added", r.stats.EntriesAdded, "unchanged", r.stats.EntriesUnchanged,
		"damaged", r.stats.Damaged, "requests", r.stats.Requests())
}

// RebuildIndex rebuilds this store's index from the documents in its bucket,
// reports what doing so would change, or verifies the two against each other.
func (s *SQL) RebuildIndex(ctx context.Context, opts RebuildOptions) (RebuildStats, error) {
	return runRebuild(ctx, s, opts)
}

// RebuildIndex rebuilds this store's index from the documents in its bucket,
// reports what doing so would change, or verifies the two against each other.
func (s *Blob) RebuildIndex(ctx context.Context, opts RebuildOptions) (RebuildStats, error) {
	return runRebuild(ctx, s, opts)
}

// runRebuild is the whole operation, for every store kind.
//
// The order is deliberate and each step's reason is on it. What it adds up to
// is that the report describes the index as it was found and the bucket as it
// is, and that nothing irreversible happens at any point — because nothing
// irreversible happens at all.
func runRebuild(ctx context.Context, s indexRebuild, opts RebuildOptions) (RebuildStats, error) {
	run := &rebuildRun{
		opts:   opts,
		log:    s.rebuildLog(),
		bucket: s.rebuildBucket(),
	}

	run.stats.Driver = s.Driver()
	run.stats.Bucket = run.bucket.String()
	run.stats.Mode = opts.Mode.String()

	// The bucket first, and for the reason a prune establishes it: a bucket
	// that has gone away answers "no such key" for everything, and a rebuild
	// over one would report an index whose every entry is stale and a history
	// with nothing left in it (Tenet 5).
	if err := run.bucket.reachable(ctx); err != nil {
		return run.stats, fmt.Errorf("rebuilding an index needs the artifact bucket: %w", err)
	}

	if err := rebuildIndexFromBucket(ctx, s, run); err != nil {
		return run.stats, err
	}

	// After the marker is cleared, because the sweep this borrows refuses to
	// judge a bucket while a rebuild is running — which is exactly the
	// protection the marker exists for (AC12).
	run.surveyOrphans(ctx, s)

	run.progress(true)

	if opts.Mode == RebuildVerify && run.stats.Drift+run.stats.Unjudged() > 0 {
		return run.stats, fmt.Errorf("%w: %d disagreements and %d documents this build could not judge",
			ErrIndexDrift, run.stats.Drift, run.stats.Unjudged())
	}

	return run.stats, nil
}

// rebuildIndexFromBucket is the part that runs under the rebuild marker.
//
// It is its own function so that the marker is cleared by a defer on the way
// out of it, before the orphan survey, rather than by unwinding the whole run.
func rebuildIndexFromBucket(ctx context.Context, s indexRebuild, run *rebuildRun) error {
	// Counted before anything else, so that a run which fails half way still
	// reports what the index held that a rebuild could never put back (AC6).
	kept, err := s.indexOnlyRecords(ctx, run)
	if err != nil {
		return err
	}

	run.stats.Unrecoverable = kept

	end, err := s.beginRebuild(ctx, run)
	if err != nil {
		return err
	}

	defer end()

	// The index as found, before the merge changes it: how many scans it
	// records, and which of them name a document the bucket no longer holds.
	if err := s.surveyIndex(ctx, run); err != nil {
		return err
	}

	if err := s.indexOnlyDrift(ctx, run); err != nil {
		return err
	}

	return run.walkDocuments(ctx, s)
}

// walkDocuments reads every object under the result prefix, a batch at a time.
//
// Every object, in every mode, because the documents are the source of truth
// and a rebuild that trusted the index about which of them it had already seen
// would be deriving from the thing it is repairing (AC2). What makes running it
// twice cheap in writes rather than in requests is that the second run finds
// every entry already there and writes nothing.
func (r *rebuildRun) walkDocuments(ctx context.Context, s indexRebuild) error {
	batch := make([]artifactObject, 0, rebuildBatchSize)

	// Every key the listing returned, documents and everything else. The pages
	// are counted from this rather than from the documents because a page is a
	// request and the provider fills it with whatever is under the prefix: a
	// bucket with a temporary object beside every document would otherwise be
	// reported as costing half the listings it actually cost.
	keys := 0

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		err := r.readAndRecord(ctx, s, batch)
		batch = batch[:0]

		return err
	}

	prefix := artifactKindResult + refSeparator

	err := r.bucket.list(ctx, prefix, func(obj artifactObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		keys++

		if !isArtifactRef(obj.ref) {
			// Under the result prefix and not a content address, so not a
			// document. The local file bucket leaves a temporary object beside
			// a key while it writes it, so a scan stored while this runs
			// produces one — and calling that damaged evidence would be a
			// rebuild reporting a fault because somebody scanned a site. It is
			// counted where the sweep already counts such keys, in the survey
			// of the whole bucket below.
			r.log.Debug("an object under the result prefix is not a key this store writes, and was skipped",
				"key", truncateForMessage(obj.ref), "bucket", r.bucket.String())

			return nil
		}

		r.stats.ObjectsFound++
		r.stats.ObjectBytes += obj.size

		batch = append(batch, obj)

		if len(batch) < rebuildBatchSize {
			return nil
		}

		return flush()
	})
	if err != nil {
		return fmt.Errorf("listing the stored documents in bucket %s: %w", r.bucket, err)
	}

	// One listing request per page of keys. The listing helper pages
	// internally and reports objects rather than pages, so this is derived
	// from the count rather than counted — the only figure in the cost report
	// that is, and it is the smallest one. A prefix with nothing under it still
	// costs the one request that established that.
	r.listed(max(1, (keys+artifactListPageSize-1)/artifactListPageSize))

	return flush()
}

// readObject is one worker's answer about one key: the document, or why there
// is not one, or a failure that ends the run.
//
// The three are separate fields rather than a result and an error because only
// the third is an error. An object that does not decode is a finding, and the
// run's whole purpose is to report findings rather than to stop at the first
// one (AC7).
type readObject struct {
	doc     *rebuiltDocument
	bytes   int64
	key     string
	damaged string
	err     error
}

// readAndRecord reads one batch of objects in parallel and folds the answers in
// afterwards, in order.
//
// The split is what makes AC11's configurable concurrency safe to have: the
// reads are independent round trips and are the whole cost of the run, while
// the folding touches the statistics and the index and is therefore done by one
// goroutine. It also makes the report reproducible — the same bucket produces
// the same summary in the same order however many workers read it.
func (r *rebuildRun) readAndRecord(ctx context.Context, s indexRebuild, batch []artifactObject) error {
	answers := make([]readObject, len(batch))

	// Each worker writes exactly one element and reads none, so the slice needs
	// no synchronisation of its own.
	err := eachBounded(ctx, len(batch), r.opts.workers(), func(ctx context.Context, i int) error {
		answers[i] = r.readDocument(ctx, batch[i])

		return answers[i].err
	})
	if err != nil {
		return fmt.Errorf("reading the stored documents in bucket %s: %w", r.bucket, err)
	}

	for _, answer := range answers {
		r.read(answer.bytes)

		if answer.damaged != "" {
			r.damaged(answer.key, answer.damaged)

			continue
		}

		r.stats.ResultsDecoded++

		if err := r.record(ctx, s, *answer.doc); err != nil {
			return err
		}
	}

	r.progress(false)

	return nil
}

// readDocument reads one object and turns it back into the scan it records.
//
// The digest it is checked against is the one in its own key, because these
// objects are content-addressed: the key is the sha256 of the bytes, so an
// object that does not hash to its key is damaged whatever any index says
// about it. That is the check a rebuild has and a read through the index does
// not need, and it is what makes "the documents are the source of truth" a
// verifiable claim rather than an assumption (AC7).
func (r *rebuildRun) readDocument(ctx context.Context, obj artifactObject) readObject {
	out := readObject{key: obj.ref}

	// The walk has already established that this is a key this store writes, so
	// the digest is there. Checked anyway rather than assumed, because what
	// follows compares bytes against it and a rebuild that verified a document
	// against an empty digest would accept anything.
	digest := artifactDigest(obj.ref)
	if digest == "" {
		out.damaged = "the key is not one this store writes, so it is not a stored document"

		return out
	}

	body, err := r.bucket.get(ctx, obj.ref)

	switch {
	case errors.Is(err, ErrNotFound):
		// Listed and then gone: a concurrent prune, or a lifecycle rule. Not a
		// failure of the run, and not silence either.
		out.damaged = "the object was listed and is no longer in the bucket"

		return out

	case err != nil:
		out.err = err

		return out
	}

	out.bytes = int64(len(body))

	// The size comes from the listing rather than from the read, so that a
	// truncated read is caught rather than described.
	res, err := decodeDocument(resultRef{ref: obj.ref, size: obj.size, digest: digest}, body)
	if err != nil {
		out.damaged = err.Error()

		return out
	}

	if res.Target == "" || res.ScanID == "" {
		out.damaged = "the document names no target or no scan ID, so there is no entry to derive"

		return out
	}

	out.doc = &rebuiltDocument{
		result: res,
		ref:    resultRef{ref: obj.ref, size: int64(len(body)), digest: digest},
	}

	return out
}

// record merges one decoded document into the index and accounts for what that
// did.
//
// The pruned-scan question comes first and short-circuits everything after it.
// A document whose scan the index deliberately removed is not a scan waiting to
// be recovered, and nothing may be written for it — not the entry, not the
// pins, not a drift record. Asking before the merge rather than after is also
// what keeps a dry run and a verify from describing an addition they would not
// make.
func (r *rebuildRun) record(ctx context.Context, s indexRebuild, doc rebuiltDocument) error {
	pruned, err := s.prunedScan(ctx, r, doc)
	if err != nil {
		return err
	}

	if pruned {
		r.account(rebuildPruned, doc)

		return nil
	}

	if documentIsNewer(doc.result.SchemaVersion) {
		r.account(rebuildNewer, doc)

		return nil
	}

	if err := r.checkEvidence(ctx, doc); err != nil {
		return err
	}

	outcome, err := s.mergeRebuilt(ctx, r, doc)
	if err != nil {
		return err
	}

	r.account(outcome, doc)

	return nil
}

// account folds one outcome into the statistics and, where the mode is verify,
// into the drift.
//
// Which of them counts as drift is the difference between the modes. A rebuild
// that adds an entry has repaired a disagreement and reports it as work; a
// verify that finds the same thing reports it as drift, because verify's whole
// job is to say that the two do not agree (AC10). A scan ID naming two
// documents is drift in every mode: no rebuild can resolve it, because
// resolving it would mean choosing which of two scans to discard.
func (r *rebuildRun) account(outcome rebuildOutcome, doc rebuiltDocument) {
	verify := r.opts.Mode == RebuildVerify

	switch outcome {
	case rebuildAdded:
		r.stats.EntriesAdded++

		if verify {
			r.drifted(driftEntryMissing, doc.subject(), "document "+doc.ref.ref)
		}

	case rebuildRepaired:
		r.stats.EntriesRepaired++

		if verify {
			r.drifted(driftEntryPartial, doc.subject(), "document "+doc.ref.ref)
		}

	case rebuildRefreshed:
		r.stats.EntriesRefreshed++

		if verify {
			r.drifted(driftSummaryStale, doc.subject(), "document "+doc.ref.ref)
		}

	case rebuildStale:
		// Drift in every mode, because no mode repairs it.
		r.stats.SummariesStale++

		r.drifted(driftSummaryStale, doc.subject(),
			"this store does not rewrite an index object, so the summary was left as it is")

	case rebuildUnchanged:
		r.stats.EntriesUnchanged++

	case rebuildConflict:
		r.stats.Conflicts++

		r.drifted(driftScanIDReused, doc.subject(), "this document is "+doc.ref.ref)

	case rebuildUnreadable:
		// The drift was recorded where the object was read, which is where the
		// decode error is. What is counted here is that the scan was not
		// indexed by this run, so that it is not reported as one that already
		// agreed.
		r.stats.IndexUnreadable++

	case rebuildPruned:
		// Not drift in any mode, and never written. See DocumentsPruned.
		r.stats.DocumentsPruned++

		if r.opts.Limit <= 0 || len(r.stats.PrunedScans) < r.opts.Limit {
			r.stats.PrunedScans = append(r.stats.PrunedScans, doc.subject())
		}

	case rebuildNewer:
		r.stats.DocumentsNewer++

		if r.opts.Limit <= 0 || len(r.stats.NewerDocuments) < r.opts.Limit {
			r.stats.NewerDocuments = append(r.stats.NewerDocuments,
				doc.subject()+" (schema "+doc.result.SchemaVersion+")")
		}

		r.log.Warn("a stored document was written by a newer wsaw, so this build did not derive an index entry from it",
			"scan_id", doc.result.ScanID, "target", doc.result.Target,
			"document_schema", doc.result.SchemaVersion, "this_build", model.SchemaVersion)
	}
}

// checkEvidence establishes whether the screenshots and stored bodies a result
// names are still in the bucket (AC8).
//
// What it does about a missing one is nothing, and that is the point. The
// reference stays in the document and in the index, so the result goes on
// naming evidence that is no longer stored and reads as exactly that; dropping
// the reference would turn a scan whose screenshot was deleted into a scan that
// never took one, which is the inference Tenet 5 forbids. The count is here so
// that an operator learns it from the rebuild rather than from a broken link
// months later.
//
// It is counted and named and it is not drift. See the note beside the drift
// kinds for why a verify must stay green over it.
//
// The document itself is not checked: it was just read.
func (r *rebuildRun) checkEvidence(ctx context.Context, doc rebuiltDocument) error {
	// The empty document reference is what excludes it: artifactRefsOf drops
	// an empty one, so what comes back is the screenshots and the stored
	// bodies and nothing else.
	refs := artifactRefsOf(doc.result, "")
	if len(refs) == 0 {
		return nil
	}

	present := make([]bool, len(refs))

	err := eachBounded(ctx, len(refs), r.opts.workers(), func(ctx context.Context, i int) error {
		found, err := r.bucket.exists(ctx, refs[i])
		if err != nil {
			return err
		}

		present[i] = found

		return nil
	})
	if err != nil {
		return fmt.Errorf("checking the evidence scan %s names: %w", doc.result.ScanID, err)
	}

	r.checked(len(refs))

	missing := 0

	for i, found := range present {
		if found {
			continue
		}

		missing++

		if r.opts.Limit <= 0 || len(r.stats.MissingEvidence) < r.opts.Limit {
			r.stats.MissingEvidence = append(r.stats.MissingEvidence,
				EvidenceGone{Scan: doc.subject(), Artifact: refs[i]})
		}
	}

	if missing == 0 {
		return nil
	}

	r.stats.EvidenceMissing += missing
	r.stats.ResultsMissingEvidence++

	return nil
}

// checkIndexed establishes, for one page of the index, that each entry's
// document is still in the bucket.
//
// It is the direction the documents cannot answer: walking the result prefix
// finds documents with no entry, and only walking the index finds entries with
// no document (AC10). Nothing is deleted on the strength of it — see the merge
// argument at the top of this file.
func (r *rebuildRun) checkIndexed(ctx context.Context, page []indexedScan) error {
	present := make([]bool, len(page))
	checks := 0

	for _, scan := range page {
		if scan.document != "" {
			checks++
		}
	}

	err := eachBounded(ctx, len(page), r.opts.workers(), func(ctx context.Context, i int) error {
		if page[i].document == "" {
			return nil
		}

		found, err := r.bucket.exists(ctx, page[i].document)
		if err != nil {
			if errors.Is(err, errInvalidRef) {
				// A reference the index holds that this store could never have
				// written. Not an absent object: a damaged entry.
				return nil
			}

			return err
		}

		present[i] = found

		return nil
	})
	if err != nil {
		return fmt.Errorf("checking the documents this index names: %w", err)
	}

	r.checked(checks)
	r.stats.EntriesFound += len(page)

	for i, scan := range page {
		if present[i] {
			continue
		}

		r.stats.EntriesStale++

		detail := "the index names no document for it"
		if scan.document != "" {
			detail = scan.document
		}

		r.drifted(driftDocumentGone, scan.subject(), detail)
	}

	return nil
}

// surveyOrphans reports what the bucket holds that no result references (AC9).
//
// It borrows the sweep's plan rather than joining the references a second time.
// A plan deletes nothing, it already knows the grace period that separates
// garbage from a scan still being written, it already counts the objects that
// wsaw did not write, and it is already tested for both store kinds — a second
// implementation of the same join would be a second answer to the same question
// with only one of them under test.
//
// A failure here is reported and never fatal. The orphan count is information
// an operator asked for on the way past; the recovery is what they ran the
// command for, and it has already happened.
func (r *rebuildRun) surveyOrphans(ctx context.Context, s indexRebuild) {
	stats, err := s.PlanSweep(ctx, r.opts.at(), SweepOptions{})

	switch {
	case errors.Is(err, ErrEmptyIndex):
		r.stats.OrphanNote = "the index holds nothing, so nothing could be said about which objects are referenced"

		return

	case err != nil:
		r.stats.OrphanNote = "the survey of the bucket did not finish: " + err.Error()

		return
	}

	r.stats.BucketObjects = stats.ArtifactsScanned
	r.stats.BucketBytes = stats.BytesScanned
	r.stats.Unreferenced = stats.ArtifactsDeleted
	r.stats.UnreferencedBytes = stats.BytesFreed
	r.stats.ForeignObjects = stats.ForeignObjects

	if stats.RebuildInProgress > 0 {
		// Another rebuild is running against this bucket. Its marker made the
		// plan judge nothing, and reporting its zeroes as an empty bucket would
		// be the quiet lie the whole command exists to avoid.
		r.stats.BucketObjects, r.stats.BucketBytes = 0, 0
		r.stats.Unreferenced, r.stats.UnreferencedBytes = 0, 0
		r.stats.OrphanNote = "another rebuild of this bucket's index is in progress, so nothing was surveyed"
	}
}

// documentIsNewer reports whether a stored document names a result schema this
// build does not understand (see RebuildStats.DocumentsNewer).
//
// The comparison is on the two numbers the version is, not on the string: "1.10"
// is newer than "1.9" and a lexical comparison says the opposite. A version this
// function cannot parse is not newer — an empty or unrecognisable one comes from
// a build older than the field or from an object somebody else wrote, and
// neither is a reason to refuse to index a document that decoded.
func documentIsNewer(version string) bool {
	got, ok := parseSchemaVersion(version)
	if !ok {
		return false
	}

	have, ok := parseSchemaVersion(model.SchemaVersion)
	if !ok {
		return false
	}

	if got[0] != have[0] {
		return got[0] > have[0]
	}

	return got[1] > have[1]
}

// parseSchemaVersion splits a "major.minor" result schema version.
func parseSchemaVersion(version string) ([2]int, bool) {
	major, minor, found := strings.Cut(version, ".")
	if !found {
		return [2]int{}, false
	}

	first, err := strconv.Atoi(major)
	if err != nil {
		return [2]int{}, false
	}

	second, err := strconv.Atoi(minor)
	if err != nil {
		return [2]int{}, false
	}

	return [2]int{first, second}, true
}

// summariesAgree reports whether two summaries describe the same scan in the
// same way.
//
// The start time is compared with Equal and the rest by value, because the two
// sides reach this function by different routes: one has been through JSON and
// one out of a database column, and a wall-clock comparison of two time.Time
// values that name one instant in different locations is false. Everything else
// in a Summary is a string, an int or a duration.
func summariesAgree(a, b Summary) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return false
	}

	a.StartedAt, b.StartedAt = time.Time{}, time.Time{}

	return a == b
}

// rebuildMarkerTTL is how long a marker keeps a sweep off a bucket before it is
// treated as the leftover of a run that died.
//
// The marker is cleared by a deferred call, which covers a cancelled run and a
// failed one and does not cover SIGKILL, an out-of-memory kill or a container
// eviction — and those are the conditions an operator is most likely to be
// running a recovery command under. Without a ceiling the consequence is
// permanent: every later sweep of that bucket returns early and collects
// nothing, for ever, while the garbage a sweep exists to reclaim accumulates in
// a store that is billed by the byte (Tenet 8 — a stalled maintenance pass is
// the failure nobody notices).
//
// A day, which is the same grace the sweep already gives an artifact whose scan
// may still be running (unreferencedArtifactGrace). A rebuild reads every
// document in the bucket and a large one is hours, so the ceiling has to be
// well clear of a long run; a day is, and a marker still there a day later is
// not a rebuild in progress by any reading.
const rebuildMarkerTTL = 24 * time.Hour

// rebuildMarkerBody is what a rebuild marker holds.
//
// StartedAt is read back by rebuildMarkers, which is what lets a marker whose
// run died expire; the rest is for whoever opens the bucket with a browser and
// needs to know what left it there.
type rebuildMarkerBody struct {
	Layout    int       `json:"layout"`
	Mode      string    `json:"mode"`
	StartedAt time.Time `json:"startedAt"`
}

// newRebuildMarker mints the id one run's marker is filed under.
//
// Random rather than derived from the host or the clock, because two rebuilds
// against one bucket must leave two markers: an id they could share would let
// the first to finish clear the second's protection, and the protection is what
// stops a sweep deleting the evidence of the part that is not indexed yet.
func newRebuildMarker() (string, error) {
	raw := make([]byte, 16)

	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("naming a rebuild of the index: %w", err)
	}

	return "rebuild-" + hex.EncodeToString(raw), nil
}

// markRebuild writes the marker that says a rebuild is running against this
// bucket, and returns the function that clears it.
//
// The marker is in the bucket rather than in the index, and both store kinds
// write and honour it, because the thing it protects is in the bucket: a sweep
// that walked a bucket while half of it was indexed would delete the evidence
// of the other half, and that is true whether the index being rebuilt is rows
// or objects (AC12). Story 8.10 built the reader for it; this is the writer.
//
// A mode that writes nothing takes no marker. A dry run and a verify change no
// index, so there is no half-built state for a sweep to be confused by, and
// making a read-only command write to the bucket would be a surprise in the one
// command an operator runs when they are not sure what is safe.
func markRebuild(ctx context.Context, b *bucket, run *rebuildRun) (func(), error) {
	if !run.opts.Mode.writes() {
		return func() {}, nil
	}

	id, err := newRebuildMarker()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(rebuildMarkerBody{
		Layout:    indexLayoutVersion,
		Mode:      run.stats.Mode,
		StartedAt: run.opts.at().UTC(),
	})
	if err != nil {
		return nil, fmt.Errorf("describing a rebuild of the index: %w", err)
	}

	key := rebuildKey(sk(id))

	if _, err := b.putIndex(ctx, key, body); err != nil {
		return nil, fmt.Errorf("marking a rebuild of the index as in progress: %w", err)
	}

	return func() {
		// A fresh context: the marker has to be cleared even where the run was
		// cancelled, because a marker left behind stops every later sweep from
		// collecting anything at all. It is logged rather than returned, since
		// the recovery it accompanied has already happened and failing it now
		// would report a success as a failure — and the command prints how to
		// clear one by hand.
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
		defer cancel()

		if err := b.deleteIndex(done, key); err != nil && !errors.Is(err, ErrNotFound) {
			run.log.Error("the marker that says a rebuild of the index is in progress could not be cleared, "+
				"so sweeps of this bucket will collect nothing until it is deleted",
				"key", key, "bucket", b.String(), "error", err)
		}
	}, nil
}

// rebuildMarkers names the markers that say a rebuild of an index is running
// against this bucket right now.
//
// It is on the bucket rather than on either store because the marker is a fact
// about the bucket: whichever store is being rebuilt, the objects a sweep would
// collect are in here, and both stores' sweeps ask this question.
//
// The keys are returned rather than counted so that the sweep's report can name
// them. "Let the rebuild finish, or clear its marker" is not actionable advice
// when the operator has to guess which key to delete, and guessing at a key
// under `_wsaw/` is not something anybody should be doing in a bucket that
// holds evidence.
//
// A marker older than rebuildMarkerTTL is not returned: no rebuild has been
// running for a day, and honouring it for ever would leave the bucket
// permanently unsweepable. It is logged at Error rather than passed over,
// because a marker that outlived its run is a leftover somebody has to delete.
func (b *bucket) rebuildMarkers(ctx context.Context, now time.Time, log *slog.Logger) ([]string, error) {
	var (
		live   []string
		cursor indexCursor
	)

	for cursor.more() {
		page, err := b.listIndexPage(ctx, indexRebuildPrefix, cursor, indexListPageSize)
		if err != nil {
			return nil, fmt.Errorf("checking whether a rebuild of the index is in progress: %w", err)
		}

		cursor = page.next

		for _, object := range page.objects {
			stale, err := b.rebuildMarkerIsStale(ctx, object.key, now)
			if err != nil {
				return nil, err
			}

			if stale {
				log.Error("a marker says a rebuild of an index has been running for more than a day, "+
					"so it is being treated as the leftover of a run that was killed; delete the object to clear it",
					"key", object.key, "bucket", b.String(), "after", rebuildMarkerTTL.String())

				continue
			}

			live = append(live, object.key)
		}
	}

	return live, nil
}

// rebuildMarkerIsStale reports whether one marker is old enough to ignore.
//
// The age comes from the body the run wrote rather than from the object's
// modification time, because a modification time is the provider's opinion and
// this one is wsaw's own record of when the run began. A marker whose body
// cannot be read or does not carry a start is never stale: a marker that cannot
// be aged has to keep protecting the bucket, and the sweep names it so that a
// person can decide.
func (b *bucket) rebuildMarkerIsStale(ctx context.Context, key string, now time.Time) (bool, error) {
	raw, err := b.getIndex(ctx, key)

	switch {
	case errors.Is(err, ErrNotFound):
		// Listed and gone: the run that left it finished between the listing
		// and this read, which is the outcome the sweep wanted anyway.
		return true, nil

	case err != nil:
		return false, fmt.Errorf("reading the marker of a rebuild in progress at %s: %w", key, err)
	}

	var body rebuildMarkerBody

	// A body that will not decode leaves StartedAt at the zero time, which is
	// the "cannot be aged" answer this function's comment describes — so the
	// decode failure needs no branch of its own, and the marker goes on
	// protecting the bucket either way.
	_ = json.Unmarshal(raw, &body)

	if body.StartedAt.IsZero() {
		return false, nil
	}

	return now.Sub(body.StartedAt) > rebuildMarkerTTL, nil
}
