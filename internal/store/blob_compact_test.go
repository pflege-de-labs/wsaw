package store_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// What compaction does to a series, and what it must never do (Story 8.10,
// §7.2 and §8.3 #7 to #10).
//
// These tests run against lagbucket_test.go's fake for two reasons. Deletion is
// decided against the bucket's own modification times and against what a
// listing shows, and the fake is the only bucket here whose clock and whose
// listings a test can place deliberately — the alternative is a test that
// sleeps for a day (AGENTS §5). And the states worth checking are the ones a
// local directory cannot produce: a checkpoint a listing does not show yet, a
// delete the bucket refused, a write whose response was lost.
//
// **They cross the real threshold rather than a lowered one.** compactAfter is
// a constant and not configuration, and a test hook that made it smaller would
// be testing a compaction no deployment runs. A thousand results into the fake
// costs about forty milliseconds, so there is nothing to buy by faking it.

// compactedEntries and compactedLoose are what one pass of the fixtures below
// leaves behind, worked out once here rather than at each use.
//
// A series of compactSeed scans crosses the threshold by one, so the pass at
// the last of them folds everything older than the newest compactKeep: 1001
// entries less the 200 that stay loose is 801.
const (
	compactSeed   = 1001
	compactFolded = 801

	// blobRetryAttempts is how many times the store tries one bucket request
	// before giving up, which is what a fault has to outlast for a test about
	// an interrupted pass to be about the interruption rather than about the
	// retry that hid it.
	blobRetryAttempts = 3
)

// The two prefixes these tests age, hide and count. Spelled out rather than
// asked of the code under test, for the reason §4.5 gives: the key layout is a
// promise, and a test that derived it from the constants would agree with a
// change that broke it.
const (
	siteCheckpointPrefix = siteEntryDir + "k."
	siteLoosePrefix      = siteEntryDir + "r."
	siteTombstonePrefix  = siteEntryDir + "d."
)

// compactStore opens a bucket-index store on a lagging bucket, on the real
// clock.
//
// The clock is deliberately not pinned, unlike most of blob_lag_test.go's
// fixtures: every deletion compaction makes compares the bucket's own
// modification time for an object against the store's now, and pinning one half
// of that comparison to a date in the past would make every object in the
// bucket look as though it were written in the future. What these tests move
// instead is the bucket's clock, through setModTime, which is the half a
// deployment's provider owns.
func compactStore(t *testing.T, fake *lagBucket) store.Store {
	t.Helper()

	opts := blobOptions(fake.url())
	opts.RetryBackoff = time.Nanosecond

	s, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("opening a bucket-index store on a lagging bucket: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s
}

// writeSeries stores n scans of the site series through one handle, one minute
// apart, newest last.
func writeSeries(t *testing.T, s store.Store, n int) {
	t.Helper()

	for i := range n {
		at := siteStart().Add(time.Duration(i) * time.Minute)
		if err := s.PutResult(result(fmt.Sprintf("scan-%05d", i), at, model.ConsentReject)); err != nil {
			t.Fatalf("storing scan %d of %d: %v", i, n, err)
		}
	}
}

// compactionPass runs exactly one compaction pass over the site series.
//
// Compaction has no entry point a test can call, and that is the design rather
// than an oversight: its schedule is an in-memory counter that makes a series
// due on the first result a process stores for it and every compactAfter
// results after that (see seriesWrites). So one fresh handle plus one stored
// result is one pass, which is also exactly what a restarted daemon does.
//
// The scan it stores is dated before every scan writeSeries writes, so it sits
// at the far end of the history and cannot change which scans the newest
// thousand are — which is what lets a test compare the answer before a pass
// with the answer after one and expect them to be equal.
func compactionPass(t *testing.T, fake *lagBucket, pass int) {
	t.Helper()

	if err := runCompaction(compactStore(t, fake), pass); err != nil {
		t.Fatalf("running compaction pass %d: %v", pass, err)
	}
}

// runCompaction is the half of a pass that a goroutine may run: it takes a
// handle somebody else opened and reports its failure rather than ending the
// test from a goroutine that is not the test's.
func runCompaction(s store.Store, pass int) error {
	at := siteStart().Add(-time.Duration(pass+1) * time.Hour)

	if err := s.PutResult(result(fmt.Sprintf("trigger-%02d", pass), at, model.ConsentReject)); err != nil {
		return fmt.Errorf("compaction pass %d: %w", pass, err)
	}

	return nil
}

// ageCheckpoints puts every checkpoint of the site series an hour into the
// bucket's past, which is how a test reaches the deletion half of a compaction
// without waiting out Options.CheckpointGrace.
//
// An hour and not a day, with the grace left at its default: what the rule
// compares is the bucket's modification time against now less the grace, so
// backdating past the grace is the same fact as waiting through it and is the
// one a test can produce. The default grace is what a deployment runs with, so
// leaving it alone keeps the rule under test the shipped one.
func ageCheckpoints(fake *lagBucket, grace time.Duration) {
	fake.setModTime(siteCheckpointPrefix, time.Now().Add(-grace-time.Hour))
}

// countUnder is how many keys the bucket really holds under a prefix.
func countUnder(fake *lagBucket, prefix string) int {
	return len(fake.keysUnder(prefix))
}

// compactAnswer is every read of the site series reduced to one comparable
// value, so that "the same answer" is one assertion rather than a field-by-field
// walk of a thousand summaries.
//
// The listing is reduced to its scan IDs and the errors are part of the value,
// exactly as lagAnswer does and for the same reason: "there is nothing before
// this scan" is an answer, and a comparison that skipped it would pass over the
// disagreements a compaction could introduce.
func compactAnswer(t *testing.T, s store.Store, anchor string) string {
	t.Helper()

	summaries, listErr := s.ListResults("site", model.ConsentReject, 0)
	latest, latestErr := s.LatestResult("site", model.ConsentReject)
	previous, previousErr := s.PreviousResult("site", model.ConsentReject, anchor)

	encoded, err := json.Marshal(map[string]any{
		"listing":       listedScans(summaries),
		"listingError":  errorText(listErr),
		"latest":        scanOf(latest),
		"latestError":   errorText(latestErr),
		"previous":      scanOf(previous),
		"previousError": errorText(previousErr),
	})
	if err != nil {
		t.Fatal(err)
	}

	return string(encoded)
}

// assertNoDuplicates fails when one scan appears twice in a listing, which is
// the shape a fold that unioned a checkpoint badly would produce.
func assertNoDuplicates(t *testing.T, s store.Store, when string) {
	t.Helper()

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("listing %s: %v", when, err)
	}

	seen := make(map[string]struct{}, len(summaries))

	for _, summary := range summaries {
		if _, twice := seen[summary.ScanID]; twice {
			t.Fatalf("%s the listing returned %s twice", when, summary.ScanID)
		}

		seen[summary.ScanID] = struct{}{}
	}
}

// TestAHistoryFoldsTheSameOnceItsOlderEntriesAreInACheckpoint is the property
// the whole of compaction exists to preserve, and the one the other tests in
// this file protect: a reader gets the same answer from the newest visible
// checkpoint plus the entries after it as it would have got from the entries
// alone (AC11).
//
// It is asserted directly rather than inferred from the parts, because every
// step of a compaction is individually plausible and the failure worth catching
// is the one where they compose into a history that is short by eight hundred
// scans.
func TestAHistoryFoldsTheSameOnceItsOlderEntriesAreInACheckpoint(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed)

	// The pass at the last of those scans has written the checkpoint and
	// deleted nothing: what it covers is a moment old and the grace has not
	// passed, so every entry is still loose and the fold does not need the
	// checkpoint to answer anything.
	if got := countUnder(fake, siteCheckpointPrefix); got != 1 {
		t.Fatalf("the series holds %d checkpoints after crossing the threshold, want exactly one", got)
	}

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed {
		t.Fatalf("%d loose entries remain, want all %d: nothing may be deleted inside the grace",
			got, compactSeed)
	}

	anchor := "scan-00900"
	before := compactAnswer(t, s, anchor)

	ageCheckpoints(fake, 24*time.Hour)
	compactionPass(t, fake, 1)

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed+1-compactFolded {
		t.Errorf("%d loose entries remain, want %d: the pass collects exactly what the checkpoint holds",
			got, compactSeed+1-compactFolded)
	}

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("compacting the series changed the answer:\n before %s\n after  %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	assertNoDuplicates(t, s, "after compaction")

	// And the scan whose loose key is gone is still the scan it was, read
	// through the fold rather than by ID: the checkpoint carries the body, so
	// nothing about it is recovered from the key.
	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	oldest := summaries[len(summaries)-1]
	if oldest.Target != "site" || oldest.Requests != 1 || oldest.Termination != model.TermIdle {
		t.Errorf("the oldest summary came back as %+v, want the scan that was stored", oldest)
	}
}

// truncateAnswer keeps a failure message readable when the answer it is about
// is a thousand scan IDs long.
func truncateAnswer(answer string) string {
	const keep = 400

	if len(answer) <= keep {
		return answer
	}

	return answer[:keep] + "…"
}

// checkpointObject is the shape of the object a compaction writes, decoded here
// rather than reached through the store.
//
// It is spelled out in the test as well as in the code for the reason every
// on-disk shape in this package is: a bucket somebody else has to read in five
// years is a promise, and a test that asked the code what it wrote could not
// fail when the promise changed.
type checkpointObject struct {
	Layout     int    `json:"layout"`
	Kind       string `json:"kind"`
	CoversFrom string `json:"coversFrom"`
	CoversTo   string `json:"coversTo"`
	Count      int    `json:"count"`

	Entries []struct {
		Layout  int `json:"layout"`
		Summary struct {
			ScanID      string `json:"scanId"`
			Target      string `json:"target"`
			Termination string `json:"termination"`
		} `json:"summary"`
		Document struct {
			Ref string `json:"ref"`
		} `json:"document"`
	} `json:"entries"`

	Tombstoned []string `json:"tombstoned"`
}

// assertCheckpointObject checks that the object under key is the checkpoint
// §7.2 specifies, and that its key names it the way the grammar says.
//
// The digest in the key is checked against the body, because that identity is
// what makes an interrupted compaction that re-runs a no-op: two runs over one
// visible set produce one key only if the key is a function of the bytes and
// the bytes are a function of nothing else — no clock, no host, no run
// identifier.
func assertCheckpointObject(t *testing.T, fake *lagBucket, key string, entries int) {
	t.Helper()

	raw, ok := fake.bodyOf(key)
	if !ok {
		t.Fatalf("the checkpoint at %s is listed and the bucket does not hold it", key)
	}

	var body checkpointObject

	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the checkpoint at %s does not decode: %v", key, err)
	}

	if body.Layout != 1 || body.Kind != "checkpoint" {
		t.Errorf("the checkpoint says it is a %q of layout %d, want a \"checkpoint\" of layout 1",
			body.Kind, body.Layout)
	}

	if body.Count != entries || len(body.Entries) != entries {
		t.Errorf("the checkpoint says it holds %d entries and holds %d, want %d",
			body.Count, len(body.Entries), entries)
	}

	// Every entry carries the whole of what the loose object carried, which is
	// what lets the fold answer from the checkpoint alone.
	for i, entry := range body.Entries {
		if entry.Layout != 1 || entry.Summary.ScanID == "" ||
			entry.Summary.Target != "site" || entry.Document.Ref == "" {
			t.Fatalf("entry %d of the checkpoint is not a whole index entry: %+v", i, entry)
		}
	}

	// The span is documentation and never a predicate, so all that is asked of
	// it is that it names the two ends of what the entries actually are.
	first, last := body.Entries[0], body.Entries[len(body.Entries)-1]
	if !strings.Contains(body.CoversTo, "."+first.Summary.ScanID+".") {
		t.Errorf("the checkpoint says it covers up to %s and its newest entry is %s",
			body.CoversTo, first.Summary.ScanID)
	}

	if !strings.Contains(body.CoversFrom, "."+last.Summary.ScanID+".") {
		t.Errorf("the checkpoint says it covers from %s and its oldest entry is %s",
			body.CoversFrom, last.Summary.ScanID)
	}

	gen := key[strings.LastIndex(key, ".")+1:]
	if want := fmt.Sprintf("%x", sha256.Sum256(raw))[:32]; gen != want {
		t.Errorf("the checkpoint key names generation %s and its body hashes to %s", gen, want)
	}
}

// TestACompactionInterruptedBeforeItsDeletesIsFinishedByTheNextOne is §8.3 #7,
// and it is the property that makes compaction safe to run at all: there is no
// point at which stopping leaves a history that reads wrong, and no point at
// which starting again does the work twice.
//
// Three interruptions in one series, in the order they can happen. The
// checkpoint's write lands and its response is lost, which a retry must turn
// into the same key rather than a second summary of the same entries (AC10).
// The pass ends before its deletes, which the next pass must finish rather than
// re-fold. And the bucket refuses a delete, which must leave the entry exactly
// where it was and the next pass must collect.
func TestACompactionInterruptedBeforeItsDeletesIsFinishedByTheNextOne(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	// One short of the threshold, so that no pass has folded anything yet and
	// the answer below is the uncompacted one. Every compactionPass below adds
	// one scan of its own, which is what makes the loose counts arithmetic.
	seed := compactSeed - 1

	writeSeries(t, s, seed)

	if got := countUnder(fake, siteCheckpointPrefix); got != 0 {
		t.Fatalf("a series of %d entries already holds %d checkpoints", seed, got)
	}

	anchor := "scan-00900"
	before := compactAnswer(t, s, anchor)

	// The checkpoint lands and the bucket says it did not. The store's retry
	// re-issues the identical bytes at the identical key, which the conditional
	// create refuses — one object, not two.
	fake.loseResponse(lagPut, siteCheckpointPrefix, 1)
	fake.forgetRequests()
	compactionPass(t, fake, 1)

	// Seven writes: the five a stored result costs, and two attempts at the one
	// checkpoint. Asserted because "one checkpoint object" would be true of a
	// compaction that gave up after the lost response as well, and the point
	// here is that it retried and found its own bytes already there.
	if got := fake.requests().Put; got != 7 {
		t.Errorf("a pass whose checkpoint write lost its response made %d writes, want 7", got)
	}

	checkpoints := fake.keysUnder(siteCheckpointPrefix)
	if len(checkpoints) != 1 {
		t.Fatalf("a compaction whose write lost its response left %d checkpoints: %v",
			len(checkpoints), checkpoints)
	}

	assertCheckpointObject(t, fake, checkpoints[0], compactFolded)

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("writing a checkpoint changed the answer before anything was deleted:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	// Running again over the same set is a no-op. Nothing is re-folded, because
	// the entries are already in a checkpoint this pass can see, and nothing is
	// deleted, because that checkpoint is a moment old.
	compactionPass(t, fake, 2)

	if got := fake.keysUnder(siteCheckpointPrefix); !slices.Equal(got, checkpoints) {
		t.Errorf("a second pass over the same set wrote %v, want the one checkpoint %v", got, checkpoints)
	}

	if got := countUnder(fake, siteLoosePrefix); got != seed+2 {
		t.Errorf("%d loose entries remain, want %d: nothing may be collected inside the grace",
			got, seed+2)
	}

	// Now past the grace, with the bucket refusing a delete through every
	// attempt the store will make. The pass stops at the first one rather than
	// pressing a thousand more into a bucket that has just refused, so the
	// entries are all still there and all still in the history.
	ageCheckpoints(fake, 24*time.Hour)
	fake.failNext(lagDelete, siteLoosePrefix, blobRetryAttempts)
	compactionPass(t, fake, 3)

	if got := countUnder(fake, siteLoosePrefix); got != seed+3 {
		t.Errorf("%d loose entries remain after a refused delete, want the pass to have stopped at %d",
			got, seed+3)
	}

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("a compaction that stopped half way changed the answer:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	// The next pass finishes the job.
	compactionPass(t, fake, 4)

	if got := countUnder(fake, siteLoosePrefix); got != seed+4-compactFolded {
		t.Errorf("%d loose entries remain, want %d: the pass after an interrupted one finishes it",
			got, seed+4-compactFolded)
	}

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("finishing the compaction changed the answer:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	assertNoDuplicates(t, s, "after the interrupted compaction was finished")
}

// TestNothingIsCollectedOnTheStrengthOfACheckpointThisRunCannotSee is one half
// of invariant I3: a loose key is removed only by membership in a checkpoint
// re-observed **now, in this run**.
//
// The hazard it excludes is the one a bucket produces on its own. A compactor
// that remembered writing a checkpoint, or that trusted an earlier listing,
// would delete the entries it covers while a listing that does not yet show the
// checkpoint is being served to somebody else — and that reader would get a
// history eight hundred scans short with no error to say so.
func TestNothingIsCollectedOnTheStrengthOfACheckpointThisRunCannotSee(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed)
	ageCheckpoints(fake, 24*time.Hour)

	// Old enough to collect in every respect but one: no listing shows it.
	fake.hideFromListings(siteCheckpointPrefix, lagEveryListing)
	compactionPass(t, fake, 1)

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed+1 {
		t.Errorf("%d loose entries remain, want all %d: a checkpoint no listing shows authorises nothing",
			got, compactSeed+1)
	}

	// And a reader that cannot see the checkpoint either still reads the whole
	// history, because nothing was taken away from it.
	if got := len(fake.keysUnder(siteEntryDir)); got < compactSeed {
		t.Fatalf("the series directory holds %d keys; this test has nothing to prove", got)
	}

	fake.reveal(siteCheckpointPrefix)
	compactionPass(t, fake, 2)

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed+2-compactFolded {
		t.Errorf("%d loose entries remain once the checkpoint is visible, want %d",
			got, compactSeed+2-compactFolded)
	}
}

// TestALooseKeyOutlivesItsCheckpointsGracePeriod is the other half of I3, and
// it is three refusals rather than one because the rule has three ways to be
// got wrong.
//
// A checkpoint that is visible and young collects nothing: the grace is what
// buys every other reader time to see the checkpoint before the only other copy
// of those entries goes away, and AC11 asks for it by name.
//
// A checkpoint the bucket dates in the future collects nothing either. There is
// one clock in this decision, the provider's, and a timestamp this process
// cannot make sense of has to switch a deletion off rather than on — the
// opposite convention would make a bucket with a fast clock delete entries
// early and silently.
//
// And a key that merely *sorts* inside what a checkpoint covers is never
// collected, however old the checkpoint is. Deletion is by membership of the
// entries the checkpoint actually carries; a range predicate would take in
// every key written after the compactor's listing, which is a scan lost with
// one process and no concurrency at all.
func TestALooseKeyOutlivesItsCheckpointsGracePeriod(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed)

	compactionPass(t, fake, 1)

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed+1 {
		t.Errorf("%d loose entries remain inside the grace, want all %d", got, compactSeed+1)
	}

	fake.setModTime(siteCheckpointPrefix, time.Now().Add(time.Hour))
	compactionPass(t, fake, 2)

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed+2 {
		t.Errorf("%d loose entries remain under a checkpoint dated in the future, want all %d",
			got, compactSeed+2)
	}

	// A scan whose start time falls between two entries the checkpoint covers,
	// stored after the checkpoint was written and therefore in no checkpoint at
	// all. Its key sorts inside the covered span and its membership is what
	// decides.
	interloper := "scan-between"
	between := siteStart().Add(400*time.Minute + 30*time.Second)

	if err := s.PutResult(result(interloper, between, model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	ageCheckpoints(fake, 24*time.Hour)
	compactionPass(t, fake, 3)

	remaining := fake.keysUnder(siteLoosePrefix)
	if !slices.ContainsFunc(remaining, func(key string) bool {
		return strings.Contains(key, "."+interloper+".")
	}) {
		t.Errorf("the entry that only sorts inside the checkpoint's range was collected with the ones it holds")
	}

	if _, err := s.GetResult("site", model.ConsentReject, interloper); err != nil {
		t.Errorf("the scan written into the middle of a covered range: %v", err)
	}

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(listedScans(summaries), interloper) {
		t.Errorf("the scan written into the middle of a covered range is not in the history any more")
	}
}

// TestTwoCompactionsOfOneSeriesThatSawDifferentListingsStrandNoEntry is §8.3 #8
// as the design frames it, and it is why the checkpoints of a series are
// unioned from a listing rather than chained from the newest.
//
// Two compactors that saw different visible sets write two checkpoints whose
// entries overlap. A union deduplicates them by scan ID and every entry is in
// at least one, so the answer is right and no key becomes undeletable. A chain
// would have forked here, and after both compactors deleted what they covered
// every entry in the orphaned branch would have been gone for good.
//
// The divergence is produced deliberately, by hiding the first checkpoint from
// the second compactor's listing, rather than by racing two goroutines and
// hoping — a test that only sometimes constructs the state it is about is a
// test that only sometimes checks anything.
func TestTwoCompactionsOfOneSeriesThatSawDifferentListingsStrandNoEntry(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed-1)

	anchor := "scan-00900"
	before := compactAnswer(t, s, anchor)

	compactionPass(t, fake, 1)

	// The second compactor's listing does not show the first's checkpoint, so
	// it folds a set that overlaps it instead of skipping it.
	fake.hideFromListings(siteCheckpointPrefix, lagEveryListing)
	compactionPass(t, fake, 2)
	fake.reveal(siteCheckpointPrefix)

	checkpoints := fake.keysUnder(siteCheckpointPrefix)
	if len(checkpoints) != 2 {
		t.Fatalf("two compactors over different listings wrote %d checkpoints: %v",
			len(checkpoints), checkpoints)
	}

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("two overlapping checkpoints changed the answer:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	assertNoDuplicates(t, s, "with two overlapping checkpoints")

	// Both are past the grace, so the third pass collects the union of what
	// they hold. Nothing may be stranded: every entry either has a loose key
	// still or is inside a checkpoint that is still there.
	ageCheckpoints(fake, 24*time.Hour)
	compactionPass(t, fake, 3)

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("collecting what two overlapping checkpoints hold changed the answer:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	assertNoDuplicates(t, s, "after both checkpoints were collected against")
}

// TestTwoCompactionsOfOneSeriesAtOnceLeaveOneCorrectIndex is the same property
// under a real race, which is the half the deterministic test above cannot
// reach: two processes compacting one series at the same moment, sharing
// nothing.
//
// It is here for -race and for the assertion that whatever interleaving
// happens, the history is unchanged. What each pass folds depends on which
// listing it took, so this test asserts the invariant and never the object
// count — a concurrency test that pinned the number of checkpoints would be
// asserting a scheduling accident.
func TestTwoCompactionsOfOneSeriesAtOnceLeaveOneCorrectIndex(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed-1)

	anchor := "scan-00900"
	before := compactAnswer(t, s, anchor)

	// Both handles are opened here rather than in the goroutines, so that the
	// only thing running concurrently is the pass itself and a failure to open
	// one is reported by the test's own goroutine.
	handles := []store.Store{compactStore(t, fake), compactStore(t, fake)}
	failures := make([]error, len(handles))

	var wg sync.WaitGroup

	for pass, handle := range handles {
		wg.Add(1)

		go func() {
			defer wg.Done()

			failures[pass] = runCompaction(handle, pass+1)
		}()
	}

	wg.Wait()

	for _, err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := countUnder(fake, siteCheckpointPrefix); got == 0 {
		t.Fatal("neither of two concurrent compactions wrote a checkpoint")
	}

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("two compactions at once changed the answer:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	ageCheckpoints(fake, 24*time.Hour)
	compactionPass(t, fake, 3)

	if after := compactAnswer(t, s, anchor); after != before {
		t.Errorf("collecting after two concurrent compactions changed the answer:\n %s\n %s",
			truncateAnswer(before), truncateAnswer(after))
	}

	assertNoDuplicates(t, s, "after two concurrent compactions")
}

// TestAPrunedScanInsideACheckpointDoesNotComeBack is §8.3 #9, and it is the one
// interaction between compaction and retention that can lose a compliance
// answer in the wrong direction: not a scan that disappears, but a scan an
// operator removed that reappears.
//
// A prune deletes a loose entry key and writes a tombstone. When that entry is
// also inside a checkpoint, the checkpoint goes on carrying it — checkpoints are
// never deleted — so the tombstone is the only thing keeping the scan out of the
// history and it must outlive every pass until a checkpoint has absorbed it.
// Only then, when the fold suppresses the scan from the checkpoints alone, may
// the tombstone object go.
func TestAPrunedScanInsideACheckpointDoesNotComeBack(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed)

	const keep = 500

	// The prune's deletes of the scan-ID objects are reported as done and are
	// not done, so that the leftovers §7.4 #5 collects are still there at the
	// end of this test — by which point the tombstones that say those scans
	// were pruned are inside a checkpoint and nowhere else.
	fake.ignoreDeletes(siteByIDDir, compactSeed-keep)

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxPerSeries: keep})
	if err != nil {
		t.Fatalf("pruning: %v", err)
	}

	if stats.ResultsDeleted != compactSeed-keep {
		t.Fatalf("the prune removed %d results, want %d", stats.ResultsDeleted, compactSeed-keep)
	}

	pruned := "scan-00010"
	if _, err := s.GetResult("site", model.ConsentReject, pruned); err == nil {
		t.Fatalf("%s survived a prune that was supposed to remove it", pruned)
	}

	if got := countUnder(fake, siteTombstonePrefix); got != compactSeed-keep {
		t.Fatalf("the prune left %d tombstones, want %d", got, compactSeed-keep)
	}

	assertPrunedStaysGone(t, s, pruned, "after the prune")

	// A pass over what is left. The tombstoned scans are still inside the
	// checkpoint, so their tombstones stay — collecting one now would put every
	// pruned scan back into the history the moment the fold opened the
	// checkpoint.
	ageCheckpoints(fake, 24*time.Hour)
	fake.setModTime(siteTombstonePrefix, time.Now().Add(-8*24*time.Hour))
	compactionPass(t, fake, 1)

	if got := countUnder(fake, siteTombstonePrefix); got != compactSeed-keep {
		t.Errorf("%d tombstones remain, want all %d: a checkpoint still holds the scans they hide",
			got, compactSeed-keep)
	}

	assertPrunedStaysGone(t, s, pruned, "after a pass over a pruned series")

	// Enough new scans that the next pass writes a checkpoint, which is where
	// the tombstones are absorbed.
	writeSeries2(t, s, compactSeed)
	compactionPass(t, fake, 2)

	if got := countUnder(fake, siteCheckpointPrefix); got < 2 {
		t.Fatalf("the second checkpoint that absorbs the tombstones was not written (%d checkpoints)", got)
	}

	assertPrunedStaysGone(t, s, pruned, "once a second checkpoint exists")

	// And now the tombstone objects themselves may go: the fold suppresses
	// those scans from the checkpoints alone.
	ageCheckpoints(fake, 24*time.Hour)
	fake.setModTime(siteTombstonePrefix, time.Now().Add(-8*24*time.Hour))
	compactionPass(t, fake, 3)

	if got := countUnder(fake, siteTombstonePrefix); got != 0 {
		t.Errorf("%d tombstones remain once a checkpoint has absorbed them, want none", got)
	}

	assertPrunedStaysGone(t, s, pruned, "once the tombstones were collected")

	// And the sweep can still tell that those scans were pruned, from the
	// checkpoint that absorbed their tombstones and from nothing else — which
	// is the reason it opens one at all (see scanKeySweep.loadCheckpoint).
	before := countUnder(fake, siteByIDDir)

	if _, err := s.Sweep(t.Context(), time.Now(), store.SweepOptions{}); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if got := countUnder(fake, siteByIDDir); got != before-(compactSeed-keep) {
		t.Errorf("%d scan-ID objects remain after the sweep, want %d: the %d the prune could not delete "+
			"are named by no tombstone any more and only the checkpoint says they were pruned",
			got, before-(compactSeed-keep), compactSeed-keep)
	}

	if _, err := s.GetResult("site", model.ConsentReject, pruned); err == nil {
		t.Error("a pruned scan is still addressable by ID after the sweep")
	}
}

// writeSeries2 stores a second batch of scans of the site series, newer than
// every scan writeSeries wrote and under names that cannot collide with them.
func writeSeries2(t *testing.T, s store.Store, n int) {
	t.Helper()

	base := siteStart().Add(24 * time.Hour)

	for i := range n {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := s.PutResult(result(fmt.Sprintf("later-%05d", i), at, model.ConsentReject)); err != nil {
			t.Fatalf("storing later scan %d of %d: %v", i, n, err)
		}
	}
}

// assertPrunedStaysGone checks that one pruned scan is in no listing and in no
// fold, whichever object is currently doing the suppressing.
func assertPrunedStaysGone(t *testing.T, s store.Store, scanID, when string) {
	t.Helper()

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("listing %s: %v", when, err)
	}

	if slices.Contains(listedScans(summaries), scanID) {
		t.Errorf("%s the pruned scan %s is back in the history", when, scanID)
	}
}

// TestASweepReadsTheCheckpointsBeforeItJudgesAScanIDObject is the interaction
// compaction creates with the sweep, and it fails in the worst possible
// direction if it is got wrong.
//
// §7.4 #5 has the sweep collect a byid key with no entry, no tombstone and no
// checkpoint membership, because that is what a prune whose delete failed
// leaves behind. Once a compaction has run, a live scan has no entry key at
// all — its body is in the checkpoint and its loose key is gone — so a sweep
// that judged the series directory by its keys would find eight hundred live
// scans absent and delete the object every share link to them resolves through.
//
// Both directions are asserted, because only checking the leftovers is
// collected would pass on a sweep that collected everything.
func TestASweepReadsTheCheckpointsBeforeItJudgesAScanIDObject(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed)

	const keep = 900

	// The prune's deletes of the scan-ID objects are reported as done and are
	// not done, which is precisely the leftover §7.4 #5 exists for. Exactly as
	// many as the prune will make, so that the sweep's own deletes later on are
	// performed rather than swallowed by a schedule that never runs out.
	fake.ignoreDeletes(siteByIDDir, compactSeed-keep)

	if _, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxPerSeries: keep}); err != nil {
		t.Fatalf("pruning: %v", err)
	}

	if got := countUnder(fake, siteByIDDir); got != compactSeed {
		t.Fatalf("%d scan-ID objects remain after a prune whose deletes the bucket ignored, want all %d",
			got, compactSeed)
	}

	// The compaction takes the loose keys of the live scans away, which is the
	// state the sweep has to judge correctly.
	ageCheckpoints(fake, 24*time.Hour)
	compactionPass(t, fake, 1)

	if got := countUnder(fake, siteLoosePrefix); got >= keep {
		t.Fatalf("%d loose entries remain; the sweep would not have to open a checkpoint to be right", got)
	}

	if _, err := s.Sweep(t.Context(), time.Now(), store.SweepOptions{}); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	// The leftovers are gone and every live scan is still addressable, one
	// extra for the scan the compaction pass itself stored.
	if got := countUnder(fake, siteByIDDir); got != keep+1 {
		t.Errorf("%d scan-ID objects remain after the sweep, want %d", got, keep+1)
	}

	for _, live := range []string{"scan-00101", "scan-00500", "scan-01000"} {
		if _, err := s.GetResult("site", model.ConsentReject, live); err != nil {
			t.Errorf("the sweep took away the identity of a scan that is still in the history: %s: %v",
				live, err)
		}
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-00010"); err == nil {
		t.Error("a pruned scan is still addressable by ID after the sweep")
	}
}

// TestACheckpointThatDoesNotSayWhatItIsIsCorruption is the other way a
// checkpoint can fail a read, and it is separated from the object being gone
// because the two mean opposite things.
//
// An object that is not there took a stretch of history with it and the index
// is incomplete. An object that is there and is not a checkpoint is damaged,
// and the distinction is worth a field in the body because of how it fails
// otherwise: a body of almost any shape unmarshals into a checkpoint holding no
// entries at all, so a reader without the check would report a history that is
// quietly eight hundred scans short rather than one it could not read
// (Tenet 5).
func TestACheckpointThatDoesNotSayWhatItIsIsCorruption(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "a body that says nothing at all", body: `{}`},
		{name: "the right layout and no kind", body: `{"layout":1,"entries":[]}`},
		{name: "a kind this store does not write", body: `{"layout":1,"kind":"snapshot"}`},
		{name: "a layout this build does not understand", body: `{"layout":2,"kind":"checkpoint"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newLagBucket(t)
			s := lagStoreAt(t, fake, siteStart)

			if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
				t.Fatal(err)
			}

			// One loose entry does not fill a listing of "all of them", so the
			// fold has to open the checkpoint to find out whether there is more
			// history behind it — which is exactly when a checkpoint is read.
			lagWrite(t, lagHandle(t, fake),
				siteCheckpointPrefix+siteInverted+"."+strings.Repeat("b", 32), tc.body)

			_, err := s.ListResults("site", model.ConsentReject, 0)

			switch {
			case errors.Is(err, store.ErrIndexIncomplete):
				t.Errorf("a checkpoint that is present and unreadable reads as an incomplete index: %v", err)
			case !errors.Is(err, store.ErrCorrupt):
				t.Errorf("ListResults = %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestWhatACompactedHistoryCostsToRead is AC11's other half applied to
// compaction: the point of writing a checkpoint at all is the request count,
// and a compaction that did not reduce it would be storage churn with a
// justification attached.
//
// The numbers are measured rather than asserted from the design, because the
// design has none for this case, and they are written down exactly rather than
// as "fewer than before" — an inequality would go on passing after a change
// that halved the saving.
func TestWhatACompactedHistoryCostsToRead(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	writeSeries(t, s, compactSeed)

	// Before: every entry is loose. The directory is one key past a listing
	// page, so the fold pays a second listing for one key, and every summary in
	// the answer is a read of its own object.
	fake.forgetRequests()

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	uncompacted := fake.requests()

	if want := (lagRequests{List: 2, Get: 1000}); uncompacted != want {
		t.Errorf("reading the whole of an uncompacted history cost %+v, want %+v", uncompacted, want)
	}

	if len(summaries) != 1000 {
		t.Fatalf("the listing returned %d summaries, want the 1000 blobMaxHydrate caps it at", len(summaries))
	}

	ageCheckpoints(fake, 24*time.Hour)
	compactionPass(t, fake, 1)

	// After: 201 loose keys and one checkpoint fit in one listing page, the
	// checkpoint is one read, and the 800 summaries it carries cost nothing at
	// all — a body the fold already holds is a body hydrate does not fetch.
	fake.forgetRequests()

	summaries, err = s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	compactedCost := fake.requests()

	if want := (lagRequests{List: 1, Get: 201}); compactedCost != want {
		t.Errorf("reading the whole of a compacted history cost %+v, want %+v", compactedCost, want)
	}

	if len(summaries) != 1000 {
		t.Fatalf("the listing returned %d summaries after compaction, want 1000", len(summaries))
	}

	// The hot path is untouched, which is what compactKeep buys: the newest
	// entries stay loose, so a page render and a comparison never open a
	// checkpoint.
	fake.forgetRequests()

	if _, err := s.LatestResult("site", model.ConsentReject); err != nil {
		t.Fatal(err)
	}

	if want := (lagRequests{List: 1, Get: 2}); fake.requests() != want {
		t.Errorf("LatestResult over a compacted history cost %+v, want %+v", fake.requests(), want)
	}

	fake.forgetRequests()

	if _, err := s.ListResults("site", model.ConsentReject, 50); err != nil {
		t.Fatal(err)
	}

	if want := (lagRequests{List: 1, Get: 50}); fake.requests() != want {
		t.Errorf("a page of fifty over a compacted history cost %+v, want %+v", fake.requests(), want)
	}
}

// checkpointScans is every scan one checkpoint holds the body of.
func checkpointScans(t *testing.T, fake *lagBucket, key string) map[string]struct{} {
	t.Helper()

	raw, ok := fake.bodyOf(key)
	if !ok {
		t.Fatalf("the checkpoint at %s is listed and the bucket does not hold it", key)
	}

	var body checkpointObject

	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the checkpoint at %s does not decode: %v", key, err)
	}

	out := make(map[string]struct{}, len(body.Entries))
	for _, entry := range body.Entries {
		out[entry.Summary.ScanID] = struct{}{}
	}

	return out
}

// looseScanOf is the scan a loose entry key names. The key is
// <inv>.<stamp>.<scan>.<term> under the loose prefix, spelled out here rather
// than parsed by the code under test.
func looseScanOf(t *testing.T, key string) string {
	t.Helper()

	fields := strings.Split(strings.TrimPrefix(key, siteLoosePrefix), ".")
	if len(fields) != 4 {
		t.Fatalf("the loose entry key %q does not split into four fields", key)
	}

	return fields[2]
}

// TestACoveredEntryIsReadFromTheCheckpointAndNotFromTheKeyItReplaces is the
// state between a compaction's two halves, which is where the fold is most
// exposed and where it spends least.
//
// A pass writes its checkpoint and deletes nothing: collectCovered waits out
// Options.CheckpointGrace, deliberately, so that every other reader has had a
// day to see the checkpoint before the only other copy of those entries goes.
// For that whole day a covered scan is selected twice by any fold that opens
// the checkpoints — once as a loose key, once out of the checkpoint that
// already carries its body — and the fold has to keep one of them.
//
// It must keep the copy that has the body. Keeping the key instead spends a
// request the fold did not need, and — the moment the deletes of a later pass
// or of another process run, which is legal precisely because the checkpoint
// holds those bodies — reads a key that is gone and drops the scan from the
// answer. Which of the two an arbitrary order happened to put first is not
// something a reader can predict or an operator can be told.
//
// A prune is the read that asks for every entry, so it is the one that always
// unions the checkpoints and the one this is asserted through.
func TestACoveredEntryIsReadFromTheCheckpointAndNotFromTheKeyItReplaces(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := compactStore(t, fake)

	// The pass at the last of these has written the checkpoint and deleted
	// nothing, so every covered scan is now in the directory twice.
	writeSeries(t, s, compactSeed)

	checkpoints := fake.keysUnder(siteCheckpointPrefix)
	if len(checkpoints) != 1 {
		t.Fatalf("the series holds %d checkpoints, want exactly one: %v", len(checkpoints), checkpoints)
	}

	covered := checkpointScans(t, fake, checkpoints[0])
	if len(covered) != compactFolded {
		t.Fatalf("the checkpoint covers %d scans, want %d", len(covered), compactFolded)
	}

	if got := countUnder(fake, siteLoosePrefix); got != compactSeed {
		t.Fatalf("%d loose entries remain, want all %d: %d of them are covered twice over",
			got, compactSeed, compactFolded)
	}

	// The delete half, run by another process between this fold's listing and
	// its reads: the key is still in the listing this fold took and the object
	// behind it has gone. It is legal precisely because the checkpoint holds
	// those bodies — so a fold holding the checkpoint cannot notice, and a fold
	// that goes back to the key loses the scan.
	for _, key := range fake.keysUnder(siteLoosePrefix) {
		if _, isCovered := covered[looseScanOf(t, key)]; isCovered {
			fake.missNext(key, 1)
		}
	}

	stats, err := s.PlanPrune(t.Context(), siteStart().Add(365*24*time.Hour),
		store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != compactSeed {
		t.Errorf("the plan accounted for %d of the %d results in the history, "+
			"so %d covered entries were read from the keys the checkpoint replaces",
			stats.ResultsDeleted, compactSeed, compactSeed-stats.ResultsDeleted)
	}
}
