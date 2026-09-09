package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The rebuild of an index from the documents in the bucket (Story 8.11).
//
// Every test here runs against whichever store the suite is being run against,
// for the reason the rest of the suite does: what a store promises is the same
// whether its index is rows in SQLite, PostgreSQL or MySQL, or objects in the
// bucket, and a recovery procedure that only worked for one of them would be a
// recovery procedure nobody could rely on (AC1, AC13).
//
// What differs per store is only how an index is lost, which is lostIndex
// below.

// rebuiltScan is one scan a rebuild test writes, and what has to come back.
type rebuiltScan struct {
	id   string
	mode model.ConsentMode
	// document is the bytes PutResult stored, which GetResult has to produce
	// again after the index has been thrown away and rebuilt (AC13).
	document []byte
}

// lostIndex names an empty index over the same artifact bucket.
//
// It is the state this whole command exists for, and each kind of store reaches
// it differently: a database that was dropped, restored without its bucket, or
// never existed is a fresh database beside the same evidence, while an index
// that is objects in the bucket is lost by the objects being gone. Both leave
// exactly what a rebuild has to work from — the documents — and nothing else.
//
// It is also, deliberately, how a history is moved between store kinds: point
// the new store at the same bucket, and rebuild (Story 8.10, AC16).
func lostIndex(t *testing.T, opts store.Options) store.Options {
	t.Helper()

	switch opts.Driver {
	case store.DriverBlob:
		// The whole index tree, layout object included. Reopening writes the
		// layout again, which is what a bucket whose index was deleted looks
		// like from the next start.
		evidence(t, opts).RemoveAll("_wsaw/index/")

	case store.DriverPostgres, store.DriverMySQL:
		opts.DSN = secret.Literal(scratchDatabase(t, opts.Driver))

	default:
		opts.Path = filepath.Join(t.TempDir(), "rebuilt.db")
	}

	return opts
}

// writeHistory stores a handful of scans across two consent modes, with a
// screenshot and a stored body on one of them so that a rebuild has more than
// documents to account for.
func writeHistory(t *testing.T, s store.Store, at time.Time) []rebuiltScan {
	t.Helper()

	withEvidence, _, _ := resultWithEvidence(t, s, "scan-1", at)

	results := []*model.Result{
		withEvidence,
		result("scan-2", at.Add(time.Minute), model.ConsentReject),
		result("scan-3", at.Add(2*time.Minute), model.ConsentAccept),
	}
	results[2].ConsentMode = model.ConsentAccept

	scans := make([]rebuiltScan, 0, len(results))

	for _, res := range results {
		document, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("encoding a result: %v", err)
		}

		if err := s.PutResult(res); err != nil {
			t.Fatalf("PutResult %s: %v", res.ScanID, err)
		}

		scans = append(scans, rebuiltScan{id: res.ScanID, mode: res.ConsentMode, document: document})
	}

	return scans
}

// historySnapshot is every listing this store can produce, as one value.
//
// Comparing two of them is how a test says "the history is what it was" without
// naming each field: a summary column that a rebuild derived differently, a
// series that did not come back, or an ordering that changed all show up as a
// difference in one string.
func historySnapshot(t *testing.T, s store.Store) string {
	t.Helper()

	series, err := s.Series()
	if err != nil {
		t.Fatalf("Series: %v", err)
	}

	snapshot := map[string][]store.Summary{}

	for _, se := range series {
		summaries, err := s.ListResults(se.Target, se.Mode, 0)
		if err != nil {
			t.Fatalf("ListResults %s/%s: %v", se.Target, se.Mode, err)
		}

		snapshot[se.Target+"/"+string(se.Mode)] = summaries
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encoding a history: %v", err)
	}

	return string(encoded)
}

// assertReadsBackIdentically checks that every scan's document comes out of the
// store byte for byte as it went in (AC13).
func assertReadsBackIdentically(t *testing.T, s store.Store, scans []rebuiltScan) {
	t.Helper()

	for _, scan := range scans {
		got, err := s.GetResult("site", scan.mode, scan.id)
		if err != nil {
			t.Errorf("GetResult %s after a rebuild: %v", scan.id, err)

			continue
		}

		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("encoding %s: %v", scan.id, err)
		}

		if string(encoded) != string(scan.document) {
			t.Errorf("%s did not read back byte-identically:\n got %s\nwant %s",
				scan.id, encoded, scan.document)
		}
	}
}

// TestRebuildingALostIndexBringsEveryResultBack is AC1, AC2 and AC13 together,
// for whichever store the suite is running against.
//
// It is also the supported way between store kinds, written out: a store opened
// against a bucket it has no index for is exactly a store of another kind
// pointed at somebody else's bucket, and what it takes to make the history
// readable is this command and nothing else.
func TestRebuildingALostIndexBringsEveryResultBack(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)
	before := historySnapshot(t, first)

	second := openAt(t, lostIndex(t, opts))

	if _, err := second.LatestResult("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a store whose index is gone answered %v, want ErrNotFound before the rebuild", err)
	}

	stats, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if stats.ObjectsFound != len(scans) || stats.ResultsDecoded != len(scans) {
		t.Errorf("the rebuild found %d objects and decoded %d, want %d of each",
			stats.ObjectsFound, stats.ResultsDecoded, len(scans))
	}

	if stats.EntriesAdded != len(scans) {
		t.Errorf("the rebuild added %d entries, want %d", stats.EntriesAdded, len(scans))
	}

	if stats.Damaged != 0 || stats.EvidenceMissing != 0 {
		t.Errorf("a rebuild of an intact bucket reported %d damaged objects and %d missing artifacts, want none",
			stats.Damaged, stats.EvidenceMissing)
	}

	if got := historySnapshot(t, second); got != before {
		t.Errorf("the rebuilt history is not the one that was stored:\n got %s\nwant %s", got, before)
	}

	assertReadsBackIdentically(t, second, scans)
}

// TestARebuildReportsWhatItCannotRestore is AC6.
//
// A document records a scan; an approval is a decision somebody took about one.
// A store whose index was thrown away therefore comes back without its
// baselines and without its audit log however complete the bucket is, and the
// only honest thing a rebuild can do is say so — rather than present a history
// in which every target simply has no baseline, which is what an operator would
// otherwise conclude they were looking at.
func TestARebuildReportsWhatItCannotRestore(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	writeHistory(t, first, at)

	if _, err := first.SetBaseline("site", model.ConsentReject, "scan-2", "eva", "agreed"); err != nil {
		t.Fatalf("approving a baseline: %v", err)
	}

	// The rebuild preserves what is there, so the same store rebuilt still has
	// its decisions and says how many.
	kept, err := first.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("RebuildIndex over an intact index: %v", err)
	}

	if !kept.Unrecoverable.Counted || kept.Unrecoverable.Baselines == 0 || kept.Unrecoverable.AuditEntries == 0 {
		t.Errorf("a rebuild over a store with a baseline reported %+v, want it counted and non-zero",
			kept.Unrecoverable)
	}

	if _, err := first.GetBaseline("site", model.ConsentReject); err != nil {
		t.Errorf("the baseline did not survive a rebuild of the index around it: %v", err)
	}

	// And a rebuild into an index that is gone reports the loss rather than the
	// zero.
	second := openAt(t, lostIndex(t, opts))

	lost, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if !lost.Unrecoverable.Empty() {
		t.Errorf("a rebuild into an empty index reported %+v, want it to say that nothing survived",
			lost.Unrecoverable)
	}

	if _, err := second.GetBaseline("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBaseline after a rebuild = %v, want ErrNotFound: an approval is not in any document", err)
	}
}

// TestRunningARebuildTwiceChangesNothing is AC3's idempotence, and it is
// asserted in writes rather than in outcome: a second run that produced the
// same history by rewriting every entry would pass a behavioural test and would
// be exactly what Story 8.10, AC3 forbids.
func TestRunningARebuildTwiceChangesNothing(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)
	second := openAt(t, lostIndex(t, opts))

	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{}); err != nil {
		t.Fatalf("the first rebuild: %v", err)
	}

	before := historySnapshot(t, second)

	again, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("the second rebuild: %v", err)
	}

	if again.EntriesAdded != 0 || again.EntriesRepaired != 0 || again.EntriesRefreshed != 0 {
		t.Errorf("the second rebuild added %d, repaired %d and refreshed %d entries, want none of each",
			again.EntriesAdded, again.EntriesRepaired, again.EntriesRefreshed)
	}

	if again.EntriesUnchanged != len(scans) {
		t.Errorf("the second rebuild found %d entries unchanged, want %d", again.EntriesUnchanged, len(scans))
	}

	if again.Writes != 0 {
		t.Errorf("the second rebuild issued %d writes, want none", again.Writes)
	}

	if got := historySnapshot(t, second); got != before {
		t.Errorf("the second rebuild changed the history:\n got %s\nwant %s", got, before)
	}
}

// TestARebuildStoppedBeforeItStartsLeavesAReadableStore is the easy half of
// AC3's interruption, and the one that says a rebuild never empties an index in
// order to fill it.
//
// The run is cancelled before it begins, so it writes nothing at all. What has
// to hold is that the store is still readable and that a later run finishes the
// job. The interruption that produces a *partly* rebuilt index — which is where
// the merge strategy's real risk is — is
// TestARebuildInterruptedPartWayThroughKeepsWhatItWrote below.
func TestARebuildStoppedBeforeItStartsLeavesAReadableStore(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)
	before := historySnapshot(t, first)

	second := openAt(t, lostIndex(t, opts))

	// Cancelled before the run starts: the interruption a rebuild has to
	// survive is one it cannot choose the moment of, and the state it leaves
	// has to be readable whichever moment that was.
	stopped, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := second.RebuildIndex(stopped, store.RebuildOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("an interrupted rebuild reported %v, want the cancellation", err)
	}

	// Readable, whatever it managed to record. An index that had been emptied
	// in order to be rebuilt would fail here, and that is the failure AC4 is
	// about.
	if _, err := second.ListResults("site", model.ConsentReject, 0); err != nil {
		t.Fatalf("a store whose rebuild was interrupted could not be listed: %v", err)
	}

	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{}); err != nil {
		t.Fatalf("the rebuild after the interruption: %v", err)
	}

	if got := historySnapshot(t, second); got != before {
		t.Errorf("the resumed rebuild produced a different history:\n got %s\nwant %s", got, before)
	}

	assertReadsBackIdentically(t, second, scans)
}

// TestADryRunOfARebuildWritesNothing is AC5.
func TestADryRunOfARebuildWritesNothing(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)
	second := openAt(t, lostIndex(t, opts))

	stats, err := second.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildPlan})
	if err != nil {
		t.Fatalf("the dry run: %v", err)
	}

	if stats.EntriesAdded != len(scans) {
		t.Errorf("the dry run said %d entries would be added, want %d", stats.EntriesAdded, len(scans))
	}

	if stats.EntriesRemoved != 0 || stats.Writes != 0 {
		t.Errorf("the dry run reported %d removals and issued %d writes, want none of either",
			stats.EntriesRemoved, stats.Writes)
	}

	if _, err := second.LatestResult("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the store answered %v after a dry run, want the index still empty", err)
	}
}

// TestACorruptDocumentIsReportedAndSkipped is AC7.
//
// The object is under the result prefix, is addressed as a document and does not
// hash to the address it is stored under. Neither the run nor the other scans
// may be lost to it, and it must not be quietly missing from the summary either.
func TestACorruptDocumentIsReportedAndSkipped(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)

	// Written at a key that is a content address of something else, which is
	// what a truncated upload or a bucket that lost bytes leaves behind.
	bucket := evidence(t, opts)
	planted := writeArtifact(t, bucket, "result", []byte("not a document at all"))

	bucket.Write(planted, []byte("not a document at all — and now not its digest"))

	second := openAt(t, lostIndex(t, opts))

	stats, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("a rebuild over a damaged object reported %v, want it to carry on", err)
	}

	if stats.Damaged != 1 {
		t.Errorf("the rebuild reported %d damaged objects, want 1", stats.Damaged)
	}

	if len(stats.DamagedKeys) != 1 || stats.DamagedKeys[0].Key != planted {
		t.Errorf("the damaged object was reported as %+v, want it named by the key %s",
			stats.DamagedKeys, planted)
	}

	if stats.EntriesAdded != len(scans) {
		t.Errorf("the rebuild added %d entries, want the %d readable scans", stats.EntriesAdded, len(scans))
	}

	assertReadsBackIdentically(t, second, scans)

	// And a verify over the same bucket exits non-zero, which is the half of
	// AC10 a damaged object used to slip through. The index and the bucket now
	// agree about every scan — the rebuild above saw to that — so a run that
	// only counted drift would report success over an object whose bytes no
	// longer hash to the key they are stored under. That is evidence this store
	// can no longer produce, and a scheduled verify that stayed green over it
	// would be worse than no verify.
	verified, err := second.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if !errors.Is(err, store.ErrIndexDrift) {
		t.Fatalf("a verify over a damaged document reported %v, want it to exit non-zero", err)
	}

	if verified.Drift != 0 {
		t.Errorf("the verify found %d disagreements as well, want the damage to be the whole of it: %+v",
			verified.Drift, verified.DriftList)
	}

	if verified.Damaged != 1 || verified.Unjudged() != 1 {
		t.Errorf("the verify reported %d damaged objects and %d it could not judge, want 1 of each",
			verified.Damaged, verified.Unjudged())
	}
}

// TestARebuiltResultStillNamesEvidenceThatIsGone is AC8 and Tenet 5.
//
// A screenshot the bucket no longer holds does not make the scan a scan that
// took no screenshot. The reference stays where it is, the result reads as
// evidence that is no longer stored, and the rebuild says how much of that it
// found — because repairing it by dropping the reference would turn a deletion
// into a clean result, which is the one thing this product must never do.
func TestARebuiltResultStillNamesEvidenceThatIsGone(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	res, screenshotRef, _ := resultWithEvidence(t, first, "scan-1", at)
	if err := first.PutResult(res); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	// Removed behind the store's back, which is what a bucket lifecycle rule
	// does (Story 8.6, AC7).
	evidence(t, opts).Remove(screenshotRef)

	second := openAt(t, lostIndex(t, opts))

	stats, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if stats.EvidenceMissing != 1 || stats.ResultsMissingEvidence != 1 {
		t.Errorf("the rebuild reported %d missing artifacts over %d results, want 1 over 1",
			stats.EvidenceMissing, stats.ResultsMissingEvidence)
	}

	got, err := second.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("GetResult after a rebuild: %v", err)
	}

	if len(got.Screenshots) != 1 || got.Screenshots[0].Ref != screenshotRef {
		t.Fatalf("the rebuilt result names %+v, want it still naming %s", got.Screenshots, screenshotRef)
	}

	if _, err := second.GetArtifact(screenshotRef); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the deleted screenshot gave %v, want it reported as no longer stored", err)
	}

	if len(stats.MissingEvidence) != 1 || stats.MissingEvidence[0].Artifact != screenshotRef {
		t.Errorf("the rebuild named %+v as the evidence that is gone, want %s", stats.MissingEvidence, screenshotRef)
	}

	// And a verify exits zero over it. AC8 defines this as the recorded outcome
	// and not as a fault, and AC10's drift is entries pointing at gone
	// documents, documents with no entry and stale summaries — none of which
	// this is. A bucket lifecycle rule that expires screenshots at ninety days
	// is a configuration, and a scheduled verify that went red on the first
	// expiry would be a cron job nobody reads again.
	verified, err := second.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if err != nil {
		t.Fatalf("a verify over evidence a lifecycle rule removed reported %v, want it to exit zero: %+v",
			err, verified.DriftList)
	}

	if verified.EvidenceMissing != 1 {
		t.Errorf("the verify reported %d artifacts that are gone, want 1", verified.EvidenceMissing)
	}
}

// TestVerifyFindsDriftInBothDirections is AC10.
//
// Both directions matter and neither is enough on its own: walking the
// documents finds a scan the index never recorded, and walking the index finds
// an entry whose document is gone. A verify that only did one of them would
// call a half-empty store healthy.
func TestVerifyFindsDriftInBothDirections(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, s, at)

	clean, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if err != nil {
		t.Fatalf("verifying a store that agrees with its bucket reported %v, want no drift", err)
	}

	if clean.Drift != 0 {
		t.Fatalf("a verify of an intact store found %d disagreements: %+v", clean.Drift, clean.DriftList)
	}

	if clean.EntriesFound != len(scans) {
		t.Errorf("the verify surveyed %d entries, want %d", clean.EntriesFound, len(scans))
	}

	// One document removed behind the store's back: an index entry that now
	// names nothing.
	document := storedDocumentRef(t, s, scans[1])

	evidence(t, opts).Remove(document)

	drifted, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if !errors.Is(err, store.ErrIndexDrift) {
		t.Fatalf("a verify over a missing document reported %v, want ErrIndexDrift", err)
	}

	if drifted.EntriesStale != 1 {
		t.Errorf("the verify found %d entries naming an absent document, want 1: %+v",
			drifted.EntriesStale, drifted.DriftList)
	}

	// And the other direction: an index that has lost the scans the bucket can
	// still produce.
	second := openAt(t, lostIndex(t, opts))

	missing, err := second.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if !errors.Is(err, store.ErrIndexDrift) {
		t.Fatalf("a verify over an empty index reported %v, want ErrIndexDrift", err)
	}

	if missing.Drift == 0 || missing.EntriesAdded == 0 {
		t.Errorf("the verify found %d disagreements over %d unrecorded documents, want both non-zero",
			missing.Drift, missing.EntriesAdded)
	}

	// Verify changes nothing: the index is still empty afterwards.
	if _, err := second.LatestResult("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the store answered %v after a verify, want the index untouched", err)
	}
}

// storedDocumentRef is the artifact reference of one scan's stored document.
//
// It is read from the bucket rather than from the store, because the tests that
// use it are about to delete it behind the store's back and need the key the
// bucket knows it by.
func storedDocumentRef(t *testing.T, s store.Store, scan rebuiltScan) string {
	t.Helper()

	res, err := s.GetResult("site", scan.mode, scan.id)
	if err != nil {
		t.Fatalf("reading %s: %v", scan.id, err)
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("encoding %s: %v", scan.id, err)
	}

	return "result/" + digestOf(encoded)
}

// TestARebuildReportsWhatNothingReferences is AC9: a rebuild is the one
// operation that sees the whole bucket, so it is the cheapest place to learn
// what is in it.
func TestARebuildReportsWhatNothingReferences(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	writeHistory(t, s, at)

	orphan := []byte("a screenshot of a scan that was never stored")
	writeArtifact(t, evidence(t, opts), "screenshot-before-consent", orphan)

	// A day later, so the orphan is past the grace period that protects an
	// artifact a scan in progress has just written.
	stats, err := s.RebuildIndex(t.Context(), store.RebuildOptions{
		Mode: store.RebuildVerify,
		Now:  time.Now().Add(48 * time.Hour),
	})
	if err != nil && !errors.Is(err, store.ErrIndexDrift) {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if stats.Unreferenced != 1 || stats.UnreferencedBytes != int64(len(orphan)) {
		t.Errorf("the rebuild reported %d unreferenced artifacts holding %d bytes, want 1 holding %d",
			stats.Unreferenced, stats.UnreferencedBytes, len(orphan))
	}

	if stats.BucketObjects == 0 || stats.BucketBytes == 0 {
		t.Errorf("the rebuild reported a bucket holding %d objects and %d bytes, want both non-zero",
			stats.BucketObjects, stats.BucketBytes)
	}
}

// TestARebuildReportsWhatItCost is AC11. The numbers are what an operator sizes
// a maintenance window and an invoice from, so a run that reported none of them
// would be a run whose cost is a surprise.
func TestARebuildReportsWhatItCost(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)
	second := openAt(t, lostIndex(t, opts))

	stats, err := second.RebuildIndex(t.Context(), store.RebuildOptions{Concurrency: 4})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if stats.Reads != len(scans) {
		t.Errorf("the rebuild read %d objects, want the %d documents", stats.Reads, len(scans))
	}

	if stats.BytesRead == 0 || stats.Requests() <= stats.Reads {
		t.Errorf("the rebuild reported %d bytes over %d requests, want the bytes read and the "+
			"listings and writes counted too", stats.BytesRead, stats.Requests())
	}
}

// TestASweepCollectsNothingWhileARebuildIsInProgress is half of AC12.
//
// The other half — that a rebuild runs concurrently with a daemon rather than
// locking it out — is what makes this necessary. A rebuild half way through has
// documents nothing references yet, and a sweep started in that window would
// read them as garbage and delete the evidence the rebuild exists to recover.
// So the rebuild leaves a marker in the bucket and every store's sweep stands
// down while it is there.
func TestASweepCollectsNothingWhileARebuildIsInProgress(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	writeHistory(t, s, at)
	writeArtifact(t, evidence(t, opts), "screenshot-before-consent", []byte("an orphan nothing names"))

	markRebuildInProgress(t, evidence(t, opts))

	stats, err := s.Sweep(t.Context(), time.Now().Add(48*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if stats.RebuildInProgress != 1 {
		t.Errorf("the sweep saw %d rebuilds in progress, want 1", stats.RebuildInProgress)
	}

	if stats.ArtifactsDeleted != 0 || stats.ArtifactsScanned != 0 {
		t.Errorf("the sweep scanned %d artifacts and deleted %d while a rebuild was running, want none of either",
			stats.ArtifactsScanned, stats.ArtifactsDeleted)
	}
}

// markRebuildInProgress plants the marker a running rebuild leaves, without
// running one.
//
// Written into the bucket directly, because what the sweep honours is an object
// in the bucket and not a fact one store told another: a rebuild of a
// bucket-index store's index protects the documents a SQL store keeps in the
// same bucket, and the other way round.
func markRebuildInProgress(t *testing.T, bucket *store.TestBucket) {
	t.Helper()

	bucket.Write("_wsaw/index/v1/rebuild/rebuild-0123456789abcdef", []byte(`{"layout":1,"mode":"rebuild"}`))
}

// TestAResultStoredDuringARebuildIsNotLost is the other half of AC12.
//
// The decision is that a rebuild runs concurrently rather than taking exclusive
// access, and this is what that has to mean: a scan stored while the rebuild is
// walking the bucket is either seen by the walk and written identically — every
// index record is derived from a content-addressed document and keyed by the
// scan, so writing it twice writes the same thing — or not seen and already
// written by the scan itself. Neither way is it lost.
//
// It is run under -race as the rest of the suite is, which is the other thing
// this concurrency has to be.
func TestAResultStoredDuringARebuildIsNotLost(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	writeHistory(t, first, at)

	second := openAt(t, lostIndex(t, opts))

	var (
		wg         sync.WaitGroup
		rebuilt    error
		concurrent = []string{"scan-during-1", "scan-during-2", "scan-during-3"}
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		_, rebuilt = second.RebuildIndex(t.Context(), store.RebuildOptions{})
	}()

	for i, id := range concurrent {
		if err := second.PutResult(result(id, at.Add(time.Duration(i+10)*time.Minute), model.ConsentReject)); err != nil {
			t.Errorf("storing %s during a rebuild: %v", id, err)
		}
	}

	wg.Wait()

	if rebuilt != nil {
		t.Fatalf("a rebuild alongside a writer reported %v", rebuilt)
	}

	// A second rebuild picks up whatever the first did not see, which is the
	// "or arrive afterwards" half of AC12.
	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{}); err != nil {
		t.Fatalf("the rebuild after the writes: %v", err)
	}

	for _, id := range concurrent {
		if _, err := second.GetResult("site", model.ConsentReject, id); err != nil {
			t.Errorf("%s, stored during a rebuild, reads back as %v", id, err)
		}
	}

	listed := listedScanIDs(t, second)

	for _, id := range concurrent {
		if !slices.Contains(listed, id) {
			t.Errorf("%s is missing from the listing after the rebuild: %v", id, listed)
		}
	}
}

// listedScanIDs is every scan the reject series lists, newest first.
func listedScanIDs(t *testing.T, s store.Store) []string {
	t.Helper()

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ScanID)
	}

	return ids
}

// TestARebuildNeedsTheBucket refuses to run against a bucket that has gone away,
// for the reason a prune refuses: every key reads as absent, so a rebuild would
// report an index whose every entry is stale over a history with nothing left
// in it (Tenet 5).
func TestARebuildNeedsTheBucket(t *testing.T) {
	t.Parallel()

	// A bucket that has gone away, rather than one that has been emptied: the
	// two are different facts and only a directory can be made to report the
	// first. What a rebuild does against a bucket that answers with failures is
	// TestAVerifySurvivesABucketThatFailsAListing.
	skipUnlessLocalBucket(t, "the bucket goes away by removing its directory")

	opts := storeOptions(t)
	s := openAt(t, opts)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(opts.ArtifactDir); err != nil {
		t.Fatalf("removing the bucket: %v", err)
	}

	_, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if err == nil || !strings.Contains(err.Error(), "artifact bucket") {
		t.Errorf("a rebuild over a bucket that has gone away reported %v, want it to name the bucket", err)
	}
}

// TestARebuildDoesNotPutBackAResultRetentionRemoved is the loss this command
// can cause in the other direction, and it is the one that matters most.
//
// Retention deletes a result and keeps the artifacts something else still names
// (Story 8.5, AC1). A baseline holds a copy of the scan it approved, so
// approving scan-old and then expiring it leaves the result gone from the
// history and its document in the bucket for ever. A rebuild that read "here is
// a document and there is no entry for it" would put the scan back — undoing a
// deletion made to satisfy a retention obligation, silently, and reporting it as
// work done. And a verify that called the same state drift would exit non-zero
// for ever on a store that is exactly right, which is how a scheduled check
// stops being read.
//
// So all three modes are asserted: the verify is clean, the dry run names the
// documents as their own category rather than as entries it would add, and the
// rebuild leaves the scan gone.
func TestARebuildDoesNotPutBackAResultRetentionRemoved(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)
	now := time.Now()

	if err := s.PutResult(result("scan-old", now.Add(-90*24*time.Hour), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult scan-old: %v", err)
	}

	if err := s.PutResult(result("scan-new", now.Add(-time.Hour), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult scan-new: %v", err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-old", "eva", "the approved state"); err != nil {
		t.Fatalf("approving scan-old as the baseline: %v", err)
	}

	pruned, err := s.Prune(t.Context(), now, store.Retention{MaxAge: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if pruned.ResultsDeleted != 1 {
		t.Fatalf("the prune removed %d results, want the one that expired", pruned.ResultsDeleted)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetResult after the prune = %v, want ErrNotFound", err)
	}

	// The whole premise: the document outlived the result, because the baseline
	// still names it. Without that there would be nothing for a rebuild to
	// resurrect and nothing here to test.
	document := "result/" + digestOf(baselineDocument(t, s))
	if !evidence(t, opts).Has(document) {
		t.Fatalf("the pruned scan's document at %s is not in the bucket, so this test proves nothing", document)
	}

	verified, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if err != nil {
		t.Fatalf("verifying a correctly pruned store reported %v, want no drift: %+v", err, verified.DriftList)
	}

	if verified.Drift != 0 {
		t.Errorf("the verify found %d disagreements over a correctly pruned store: %+v",
			verified.Drift, verified.DriftList)
	}

	if verified.DocumentsPruned != 1 {
		t.Errorf("the verify reported %d documents of pruned scans, want 1", verified.DocumentsPruned)
	}

	planned, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildPlan})
	if err != nil {
		t.Fatalf("the dry run: %v", err)
	}

	if planned.EntriesAdded != 0 || planned.DocumentsPruned != 1 {
		t.Errorf("the dry run said %d entries would be added and %d documents belong to pruned scans, want 0 and 1",
			planned.EntriesAdded, planned.DocumentsPruned)
	}

	if len(planned.PrunedScans) != 1 || !strings.Contains(planned.PrunedScans[0], "scan-old") {
		t.Errorf("the dry run named %v as the pruned scans, want scan-old", planned.PrunedScans)
	}

	rebuilt, err := s.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if rebuilt.EntriesAdded != 0 || rebuilt.DocumentsPruned != 1 {
		t.Errorf("the rebuild added %d entries and skipped %d pruned documents, want 0 and 1",
			rebuilt.EntriesAdded, rebuilt.DocumentsPruned)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetResult after the rebuild = %v, want the pruned scan to stay gone", err)
	}

	if got := listedScanIDs(t, s); len(got) != 1 || got[0] != "scan-new" {
		t.Errorf("the history after the rebuild is %v, want only scan-new", got)
	}

	// And the baseline is untouched, which is why the document was kept.
	if _, err := s.GetBaseline("site", model.ConsentReject); err != nil {
		t.Errorf("the baseline did not survive the rebuild: %v", err)
	}
}

// baselineDocument is the bytes of the result a target's baseline holds.
//
// The baseline's own copy is the pruned scan, so its document is the object the
// rebuild meets in the bucket with no index entry beside it.
func baselineDocument(t *testing.T, s store.Store) []byte {
	t.Helper()

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("GetBaseline: %v", err)
	}

	encoded, err := json.Marshal(b.Result)
	if err != nil {
		t.Fatalf("encoding the baseline's result: %v", err)
	}

	return encoded
}

// TestARebuildInterruptedPartWayThroughKeepsWhatItWrote is the half of AC3 and
// AC13 that is about a rebuild which had already written something when it
// stopped.
//
// A run that fails before it starts writes nothing, and a store that survives
// that has not been tested for much: the merge strategy's risk is a partially
// rebuilt index — some scans in, the rest not, and for the bucket index a set
// of objects written for one scan and not for the next. So the failure is put
// in the second batch of documents, which needs more than one batch to exist at
// all: a batch is read in parallel and merged afterwards, so a read that fails
// inside the first batch has written nothing.
//
// What is asserted is that the index holds a strict subset and is readable,
// that resuming completes it, and that the resumed run did not rewrite what the
// first one had already recorded.
func TestARebuildInterruptedPartWayThroughKeepsWhatItWrote(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)

	opts := storeOptions(t)
	opts.ArtifactDir = fake.url()
	// One attempt, so the scheduled failure is the interruption rather than
	// something the retrier hides.
	opts.MaxAttempts = 1

	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	const scans = store.RebuildBatchSize + 8

	for i := range scans {
		id := fmt.Sprintf("scan-%05d", i)
		if err := first.PutResult(result(id, at.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatalf("storing %s: %v", id, err)
		}
	}

	before := historySnapshot(t, first)

	second := openAt(t, lostIndex(t, opts))

	// The documents are listed in key order, which is the order of their
	// content addresses, so the batch boundary is a position in the sorted key
	// set and this is a key inside the second batch.
	documents := evidence(t, opts).Keys("result/")
	slices.Sort(documents)

	if len(documents) != scans {
		t.Fatalf("the bucket holds %d documents, want %d", len(documents), scans)
	}

	fake.failNext(lagGet, documents[store.RebuildBatchSize+4], 1)

	interrupted, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err == nil {
		t.Fatal("a rebuild whose read failed part way through reported success")
	}

	if interrupted.EntriesAdded != store.RebuildBatchSize {
		t.Fatalf("the interrupted run added %d entries, want the whole first batch of %d",
			interrupted.EntriesAdded, store.RebuildBatchSize)
	}

	// A strict subset, and a readable one: the entries it wrote are entries,
	// not half of one.
	partial := listedScanIDs(t, second)
	if len(partial) == 0 || len(partial) >= scans {
		t.Fatalf("the interrupted rebuild left %d of %d scans listed, want some but not all", len(partial), scans)
	}

	for _, id := range partial {
		if _, err := second.GetResult("site", model.ConsentReject, id); err != nil {
			t.Fatalf("%s was written by the interrupted rebuild and does not read back: %v", id, err)
		}
	}

	resumed, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("the rebuild after the interruption: %v", err)
	}

	// It continues rather than starting over: the scans the first run recorded
	// are found unchanged, and only the rest are added.
	if resumed.EntriesUnchanged != len(partial) {
		t.Errorf("the resumed run found %d entries unchanged, want the %d the first run wrote",
			resumed.EntriesUnchanged, len(partial))
	}

	if resumed.EntriesAdded != scans-len(partial) {
		t.Errorf("the resumed run added %d entries, want the %d that were left",
			resumed.EntriesAdded, scans-len(partial))
	}

	if got := historySnapshot(t, second); got != before {
		t.Errorf("the resumed rebuild produced a different history:\n got %s\nwant %s", got, before)
	}
}

// TestARebuildRefusesToDeriveFromADocumentANewerWsawWrote is Tenet 4 and
// Tenet 16 applied to the one command that rewrites derived data wholesale.
//
// Nothing else gates a rebuild on the document format. A newer wsaw records a
// field this build does not know; readDocument's plain unmarshal drops it; the
// summary derived from what is left is short by exactly that field; and the
// merge would then call the index stale and rewrite it downwards, printing it
// as a repair. The bucket's raw capture survives either way, so nothing is
// lost — but the history an operator reads would have been rewritten to a
// smaller truth by a binary that should have refused.
func TestARebuildRefusesToDeriveFromADocumentANewerWsawWrote(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	scans := writeHistory(t, first, at)

	// A document of a scan this build has never seen, carrying a schema version
	// it does not understand and a field it cannot decode. It is written at its
	// own content address, so it is intact evidence and not damage: the run has
	// to tell the two apart.
	future := map[string]any{
		"schemaVersion":    "9.9",
		"scanId":           "scan-from-the-future",
		"target":           "site",
		"consentMode":      "reject",
		"startedAt":        at.Add(time.Hour).Format(time.RFC3339Nano),
		"termination":      "complete",
		"somethingUnknown": []string{"a field this build has no name for"},
	}

	encoded, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}

	writeArtifact(t, evidence(t, opts), "result", encoded)

	second := openAt(t, lostIndex(t, opts))

	stats, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("a rebuild over a newer document reported %v, want it to carry on", err)
	}

	if stats.DocumentsNewer != 1 {
		t.Fatalf("the rebuild reported %d documents newer than this build, want 1", stats.DocumentsNewer)
	}

	if stats.Damaged != 0 {
		t.Errorf("the rebuild called %d objects damaged, want none: an intact document this build "+
			"cannot read is not a corrupt one", stats.Damaged)
	}

	if stats.EntriesAdded != len(scans) {
		t.Errorf("the rebuild added %d entries, want the %d scans it understands", stats.EntriesAdded, len(scans))
	}

	if _, err := second.GetResult("site", model.ConsentReject, "scan-from-the-future"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the newer scan was indexed anyway: GetResult = %v", err)
	}

	if len(stats.NewerDocuments) != 1 || !strings.Contains(stats.NewerDocuments[0], "9.9") {
		t.Errorf("the run named %v as the newer documents, want the schema version in it", stats.NewerDocuments)
	}

	// And a verify cannot claim agreement over it either.
	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify}); !errors.Is(err, store.ErrIndexDrift) {
		t.Errorf("a verify over a document it cannot read reported %v, want it to exit non-zero", err)
	}
}

// TestASweepIgnoresARebuildMarkerThatOutlivedItsRun is AC12's other end.
//
// The marker is cleared by a deferred call, which covers cancellation and
// failure and does not cover SIGKILL, an OOM kill or an evicted container —
// which are the conditions somebody runs a recovery command under. Without an
// expiry the consequence is permanent and silent: every later sweep of that
// bucket collects nothing at all, for ever, while the garbage accumulates in a
// store billed by the byte.
func TestASweepIgnoresARebuildMarkerThatOutlivedItsRun(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

	writeHistory(t, s, at)

	orphan := []byte("a screenshot of a scan that was never stored")
	writeArtifact(t, evidence(t, opts), "screenshot-before-consent", orphan)

	now := time.Now().Add(48 * time.Hour)

	// A marker whose run started before the ceiling, which is what a killed
	// rebuild leaves behind.
	markRebuildStartedAt(t, evidence(t, opts), "rebuild-0000000000000001",
		now.Add(-store.RebuildMarkerTTL-time.Hour))

	stats, err := s.Sweep(t.Context(), now, store.SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if stats.RebuildInProgress != 0 {
		t.Fatalf("the sweep honoured %d markers older than %s, want it to carry on",
			stats.RebuildInProgress, store.RebuildMarkerTTL)
	}

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("the sweep collected %d artifacts, want the one orphan", stats.ArtifactsDeleted)
	}

	// And one that has not expired still stops it, and is named so that an
	// operator can clear it.
	markRebuildStartedAt(t, evidence(t, opts), "rebuild-0000000000000002", now.Add(-time.Minute))

	held, err := s.Sweep(t.Context(), now, store.SweepOptions{})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if held.RebuildInProgress != 1 {
		t.Fatalf("the sweep saw %d rebuilds in progress, want the one that has not expired", held.RebuildInProgress)
	}

	if len(held.RebuildMarkers) != 1 || !strings.Contains(held.RebuildMarkers[0], "rebuild-0000000000000002") {
		t.Errorf("the sweep named %v as the markers, want the key an operator has to delete", held.RebuildMarkers)
	}
}

// markRebuildStartedAt plants a rebuild marker whose run began at a chosen
// moment, without running one.
func markRebuildStartedAt(t *testing.T, bucket *store.TestBucket, id string, startedAt time.Time) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"layout":    1,
		"mode":      "rebuild",
		"startedAt": startedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}

	bucket.Write("_wsaw/index/v1/rebuild/"+id, body)
}
