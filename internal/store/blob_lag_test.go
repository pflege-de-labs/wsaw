package store_test

import (
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

// What a bucket that lags does to a reader (Story 8.10, §8.3).
//
// Everything here runs against lagbucket_test.go's fake, because what it
// asserts is what a real object store does and a local directory never does: a
// listing that has not caught up with a write, a listing served from a pinned
// view, a response lost after its write landed, two writers of one fact at
// once. blob_test.go is the same store against a bucket that behaves; this file
// is the same store against one that does not, and it is a file of its own
// rather than a section of that one because the two need different fixtures and
// blob_test.go is long enough.
//
// Every fault is scheduled explicitly, so these read as deterministically as
// the tests that need no fake at all, and none of them waits for anything
// (AGENTS §5).

// The two directories these tests hide keys in, spelled out for the reason §4.5
// gives, as the prefixes in blob_test.go are.
const (
	siteEntryDir = "_wsaw/index/v1/series/" + siteSeries + "/reject/"
	siteByIDDir  = "_wsaw/index/v1/byid/" + siteSeries + "/reject/"
)

// lagStore opens a bucket-index store on a bucket that lags.
func lagStore(t *testing.T, fake *lagBucket) store.Store {
	t.Helper()

	return lagStoreAt(t, fake, nil)
}

// lagStoreAt is lagStore with the clock pinned, for the tests that put two
// decisions on one instant on purpose.
func lagStoreAt(t *testing.T, fake *lagBucket, now func() time.Time) store.Store {
	t.Helper()

	opts := blobOptions(fake.url())
	opts.Now = now

	// These tests drive the store's retry deliberately, and what they are about
	// is the retry rather than the wait it does first: at the default backoff a
	// handful of them would be most of this package's wall clock (AGENTS §5).
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

// lagEntryKeyOf is the series-directory key one scan's entry was written to,
// which is the key these tests hide to make a stored scan invisible.
func lagEntryKeyOf(t *testing.T, fake *lagBucket, scanID string) string {
	t.Helper()

	var found []string

	for _, key := range fake.keysUnder(siteEntryDir) {
		if strings.Contains(key, "."+scanID+".") {
			found = append(found, key)
		}
	}

	if len(found) != 1 {
		t.Fatalf("the series holds %d entries for %s: %v", len(found), scanID, found)
	}

	return found[0]
}

// lagAnswer is every read of one series encoded as one value, so that two of
// them can be compared for being the same answer rather than for agreeing field
// by field.
//
// The errors are part of the answer and not a reason to stop. What AC7 asks is
// that one visible key set produces one answer, and "there is nothing before
// this scan" is an answer: a comparison that skipped it would pass over exactly
// the disagreements a lagging listing produces.
func lagAnswer(t *testing.T, s store.Store, anchor string) string {
	t.Helper()

	summaries, listErr := s.ListResults("site", model.ConsentReject, 0)
	latest, latestErr := s.LatestResult("site", model.ConsentReject)
	previous, previousErr := s.PreviousResult("site", model.ConsentReject, anchor)

	encoded, err := json.Marshal(map[string]any{
		"listing":       summaries,
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

// errorText is an error as a comparable value, with nil as the empty string.
func errorText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// scanOf names a result that may not be there.
func scanOf(res *model.Result) string {
	if res == nil {
		return ""
	}

	return res.ScanID
}

// scanOfBaseline names a baseline that may not be there, for a failure message.
func scanOfBaseline(b *store.Baseline) string {
	if b == nil {
		return ""
	}

	return b.ScanID
}

// listedScans is the scan IDs a listing returned, in the order it returned them.
func listedScans(summaries []store.Summary) []string {
	out := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, summary.ScanID)
	}

	return out
}

// TestAScanTheListingCannotShowYetIsNotAScanThatWasDeleted is AC6 from the
// reading side, against the provider behaviour that produces it.
//
// A scan is stored and its entry key is not in the listings yet. What a reader
// must see is a history that is one scan short, with nothing reported as an
// error and nothing reported as removed — the scan is not there *yet*, which is
// a different fact from a scan that was pruned, and the store may not turn one
// into the other (Tenet 5).
//
// The second handle is what §7.6 #6 buys by having no recent-write overlay: the
// process that wrote the scan holds no memory of it, so both handles fold the
// same visible key set and must produce the same bytes. That is a stronger
// property than the per-process view the design originally specified, and it is
// what this test asserts in its place.
func TestAScanTheListingCannotShowYetIsNotAScanThatWasDeleted(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	writer := lagStore(t, fake)

	for i, id := range []string{"scan-older", "scan-newer"} {
		at := siteStart().Add(time.Duration(i) * time.Minute)
		if err := writer.PutResult(result(id, at, model.ConsentReject)); err != nil {
			t.Fatalf("storing %s: %v", id, err)
		}
	}

	hidden := lagEntryKeyOf(t, fake, "scan-newer")
	fake.hideFromListings(hidden, lagEveryListing)

	reader := lagStore(t, fake)

	summaries, err := writer.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("a listing that is behind failed instead of being short: %v", err)
	}

	if got := listedScans(summaries); !slices.Equal(got, []string{"scan-older"}) {
		t.Errorf("the listing shows %v, want only scan-older", got)
	}

	latest, err := writer.LatestResult("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("LatestResult over a listing that is behind: %v", err)
	}

	if latest.ScanID != "scan-older" {
		t.Errorf("LatestResult = %s, want the newest scan the bucket will show", latest.ScanID)
	}

	// Reachable by identity throughout, because that read is a GET of a named
	// key rather than a listing, and named keys do not lag.
	if got, err := writer.GetResult("site", model.ConsentReject, "scan-newer"); err != nil {
		t.Errorf("a scan whose entry is not listed yet is not reachable by ID: %v", err)
	} else if got.ScanID != "scan-newer" {
		t.Errorf("GetResult returned %s", got.ScanID)
	}

	if ok, err := writer.HasResult("site", model.ConsentReject, "scan-newer"); err != nil || !ok {
		t.Errorf("HasResult = %v, %v, want true", ok, err)
	}

	// Nowhere does a caller read "deleted": the scan that is not visible
	// produces no error at all, and the scan before it is still found.
	if _, err := writer.PreviousResult("site", model.ConsentReject, "scan-newer"); err != nil {
		t.Errorf("the scan before one whose entry is not listed yet: %v", err)
	}

	if first, second := lagAnswer(t, writer, "scan-newer"), lagAnswer(t, reader, "scan-newer"); first != second {
		t.Errorf("the handle that wrote the scan and another handle disagreed:\n %s\n %s", first, second)
	}

	// And once the listing catches up the scan is in every fold, which is the
	// other half of AC6: what was missing was the listing, not the scan.
	fake.reveal(hidden)

	summaries, err = writer.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := listedScans(summaries); !slices.Equal(got, []string{"scan-newer", "scan-older"}) {
		t.Errorf("once the key is visible the listing shows %v, want both, newest first", got)
	}

	if latest, err := writer.LatestResult("site", model.ConsentReject); err != nil || latest.ScanID != "scan-newer" {
		t.Errorf("LatestResult = %v, %v, want scan-newer", scanOf(latest), err)
	}
}

// TestTwoReadersOfOneStaleViewGiveOneAnswer is AC7 against the provider
// behaviour it was written for (§8.3 #2).
//
// Both handles read through a listing pinned to an older view of the bucket, so
// both see the same key set and neither sees the scan written since. What they
// must produce is not merely two plausible answers but the same one, including
// the same latest and the same previous — the two reads a comparison is made
// from, where two processes disagreeing would mean two operators reading a
// different history out of one bucket.
func TestTwoReadersOfOneStaleViewGiveOneAnswer(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	writer := lagStore(t, fake)

	for i := range 3 {
		at := siteStart().Add(time.Duration(i) * time.Minute)
		if err := writer.PutResult(result(fmt.Sprintf("scan-%d", i), at, model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	reader := lagStore(t, fake)

	fake.serveStaleListings(lagEveryListing)

	// Written after the view was pinned, so no listing in this test can show
	// it — which is what makes both answers answers about one key set.
	if err := writer.PutResult(result("scan-3", siteStart().Add(3*time.Minute), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	first, second := lagAnswer(t, writer, "scan-2"), lagAnswer(t, reader, "scan-2")
	if first != second {
		t.Errorf("two handles over one pinned view disagreed:\n %s\n %s", first, second)
	}

	summaries, err := writer.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := listedScans(summaries); !slices.Equal(got, []string{"scan-2", "scan-1", "scan-0"}) {
		t.Errorf("the pinned view shows %v, want the three scans it was pinned over", got)
	}
}

// TestAStaleListingCannotUnsayAScanALaterOneStillShows is the direction AC7
// leaves open: a read that is not repeatable because the visible set grew.
//
// The change between the two reads has to be an entry appearing and never a
// different answer to the same question. A store that folded differently over a
// superset — dropping a scan the stale view had shown, or reordering what was
// already there — would make "the history" a function of which replica answered.
func TestAStaleListingCannotUnsayAScanALaterOneStillShows(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStore(t, fake)

	for i := range 3 {
		at := siteStart().Add(time.Duration(i) * time.Minute)
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), at, model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	fake.serveStaleListings(lagEveryListing)

	if err := s.PutResult(result("scan-3", siteStart().Add(3*time.Minute), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	stale, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	fake.catchUpListings()

	fresh, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	staleScans, freshScans := listedScans(stale), listedScans(fresh)

	if !slices.Equal(freshScans, []string{"scan-3", "scan-2", "scan-1", "scan-0"}) {
		t.Fatalf("the listing that sees everything shows %v", freshScans)
	}

	// Every scan the stale view showed is still there and still in the same
	// order relative to the others: the difference is one entry appearing at
	// the front, and nothing else.
	kept := slices.DeleteFunc(slices.Clone(freshScans), func(id string) bool {
		return !slices.Contains(staleScans, id)
	})

	if !slices.Equal(kept, staleScans) {
		t.Errorf("the stale view showed %v and the later one keeps %v of them", staleScans, kept)
	}
}

// TestRevealingEntriesInAnyOrderFoldsTheSame is AC7 as a property rather than
// as a case (§8.3 #3).
//
// Providers make keys visible in whatever order they like, and the order two of
// them arrived in is not a fact this store may answer differently for. So the
// same visible set is reached by four different reveal orders and the answers
// are required to be identical — every listing, every latest, every previous. A
// fold that carried anything over from the order it saw keys in would fail
// here, and could not be found any other way.
func TestRevealingEntriesInAnyOrderFoldsTheSame(t *testing.T) {
	t.Parallel()

	const scans = 5

	fake := newLagBucket(t)
	s := lagStore(t, fake)

	ids := make([]string, 0, scans)
	keys := make([]string, 0, scans)

	for i := range scans {
		id := fmt.Sprintf("scan-%d", i)
		res := result(id, siteStart().Add(time.Duration(i)*time.Minute), model.ConsentReject)

		if i == 3 {
			// One failed scan, because "the scan before this one" skips those,
			// so the order they become visible in has one more way to matter.
			res.Termination = model.TermError
			res.Error = "the browser crashed"
		}

		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}

		ids = append(ids, id)
		keys = append(keys, lagEntryKeyOf(t, fake, id))
	}

	// Four orders rather than all hundred and twenty: what is being pinned is
	// that the answer is a function of the visible set, and each of these
	// reaches sets the others reach by a different route.
	orders := [][]int{{0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {2, 0, 4, 1, 3}, {3, 1, 0, 4, 2}}

	answers := map[string]string{}

	for _, order := range orders {
		for _, key := range keys {
			fake.hideFromListings(key, lagEveryListing)
		}

		var visible []string

		for _, next := range order {
			fake.reveal(keys[next])

			visible = append(visible, ids[next])
			slices.Sort(visible)

			set := strings.Join(visible, ",")
			answer := lagAnswer(t, s, "scan-4")

			if seen, ok := answers[set]; ok && seen != answer {
				t.Errorf("the visible set {%s} folded two ways:\n %s\n %s", set, seen, answer)
			}

			answers[set] = answer
		}
	}
}

// TestAResultWriteThatLostItsResponseLeavesOneEntry is AC10 against the failure
// it was written for: the object landed and the caller was told it had not.
//
// The retry is the store's own — the failure carries a code bucket.go marks as
// worth another attempt — and what it must produce is the same key with the
// same bytes rather than a second entry. Here it cannot produce anything else:
// the key is derived from the scan and the write is a conditional create, so
// the second attempt finds its own bytes already there.
func TestAResultWriteThatLostItsResponseLeavesOneEntry(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStore(t, fake)

	// The entry is the commit point of PutResult, which makes it the write
	// whose lost response would matter most.
	fake.loseResponse(lagPut, siteEntryDir, 1)
	fake.forgetRequests()

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatalf("a write whose response was lost was not retried into success: %v", err)
	}

	if got := fake.keysUnder(siteEntryDir); len(got) != 1 {
		t.Errorf("the retried write left %d entries: %v", len(got), got)
	}

	if got := fake.keysUnder(siteByIDDir); len(got) != 1 {
		t.Errorf("the retried write left %d scan-ID objects: %v", len(got), got)
	}

	// Six writes rather than the five one result costs: the entry was written
	// twice, which is what makes this a test of the retry rather than of a
	// bucket that quietly did nothing.
	if got := fake.requests().Put; got != 6 {
		t.Errorf("the store made %d writes, want the five one result costs plus the retried entry", got)
	}

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := listedScans(summaries); !slices.Equal(got, []string{"scan-1"}) {
		t.Errorf("the history reads %v after one interrupted write", got)
	}
}

// TestAnAuditEntryWriteThatLostItsResponseLeavesOneEntry is the same failure at
// the one write in this store whose key is not a pure function of what it
// records — and so the one place a retry could genuinely duplicate.
//
// An audit entry's key carries a nonce, because two identical actions recorded
// in one clock tick must stay two entries. Drawn inside the write, that nonce
// would be redrawn on the retry and one interrupted RecordAudit would become
// two entries in a compliance log. It is drawn once, above the retry, and this
// is the test that says so: the response is lost after the object landed, the
// store tries again, and the log holds one entry.
func TestAnAuditEntryWriteThatLostItsResponseLeavesOneEntry(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	fake.loseResponse(lagPut, auditPrefix, 1)
	fake.forgetRequests()

	if err := s.RecordAudit(store.AuditEntry{
		At: siteStart(), Actor: "eva", Action: "allowlist-added",
		Target: "site", Mode: model.ConsentReject, Subject: "tracker.test",
	}); err != nil {
		t.Fatalf("an audit entry whose response was lost was not retried into success: %v", err)
	}

	if got := fake.keysUnder(auditPrefix); len(got) != 1 {
		t.Errorf("one retried RecordAudit left %d entries in the log: %v", len(got), got)
	}

	if got := fake.requests().Put; got != 2 {
		t.Errorf("the store made %d writes, want the entry and its retry", got)
	}

	entries, err := s.Audit(0)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Subject != "tracker.test" {
		t.Errorf("the log reads %+v, want the one entry that was recorded", entries)
	}
}

// TestTwoWritersOfOneApprovalConvergeOnOneDecision is §8.3 #4a, and it is about
// what happens when the condition that is supposed to prevent a duplicate is
// not there.
//
// Both writers see a decision listing that shows nothing — which is what a
// lagging provider gives two processes racing — so both derive the same
// decision; and the bucket is told to accept every conditional write, which is
// what a provider without IfNotExist does. One key and one audit pointer are
// left anyway, because I1 is a property of the key derivation and not of the
// condition: two writers of one fact write the same bytes to the same key, and
// which of them lands does not matter.
func TestTwoWritersOfOneApprovalConvergeOnOneDecision(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	fake.hideFromListings(siteDecisionDir, lagEveryListing)
	fake.acceptEveryConditionalWrite()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		approvals []string
	)

	for range 2 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			b, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "agreed")
			encoded, marshalErr := json.Marshal(b)

			mu.Lock()
			defer mu.Unlock()

			approvals = append(approvals, errorText(err)+errorText(marshalErr)+string(encoded))
		}()
	}

	wg.Wait()

	if approvals[0] != approvals[1] {
		t.Errorf("two writers of one approval got two answers:\n %s\n %s", approvals[0], approvals[1])
	}

	if got := fake.keysUnder(siteDecisionDir); len(got) != 1 {
		t.Errorf("two writers of one approval left %d decision objects: %v", len(got), got)
	}

	if got := fake.keysUnder(auditPrefix); len(got) != 1 {
		t.Errorf("two writers of one approval left %d audit entries: %v", len(got), got)
	}

	fake.reveal(siteDecisionDir)

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("GetBaseline after two writers: %v", err)
	}

	if b.ScanID != "scan-1" {
		t.Errorf("the baseline in force approves %s", b.ScanID)
	}
}

// TestTwoWritersOfTwoApprovalsKeepBoth is §8.3 #4b, the other half.
//
// Two different facts must produce two keys, both durable, and a fold that
// picks between them the same way every time. The loser of a race is still a
// compliance decision somebody took: it may not be in force and it may not go
// missing, which is exactly what an edited object would have done to it (AC8).
func TestTwoWritersOfTwoApprovalsKeepBoth(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	for _, id := range []string{"scan-1", "scan-2"} {
		if err := s.PutResult(result(id, siteStart(), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	// Neither writer sees the other's decision, so both file at the same
	// instant and the tie-break decides rather than the monotonic bump.
	fake.hideFromListings(siteDecisionDir, lagEveryListing)

	var wg sync.WaitGroup

	for _, id := range []string{"scan-1", "scan-2"} {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := s.SetBaseline("site", model.ConsentReject, id, "eva", "agreed"); err != nil {
				t.Errorf("approving %s: %v", id, err)
			}
		}()
	}

	wg.Wait()

	decisions := fake.keysUnder(siteDecisionDir)
	if len(decisions) != 2 {
		t.Fatalf("two approvals left %d decision objects: %v", len(decisions), decisions)
	}

	fake.reveal(siteDecisionDir)

	// The winner is the first key of the listing, which is what the fold
	// selects: the same instant, the same operation, the lower digest.
	winner, ok := fake.bodyOf(decisions[0])
	if !ok {
		t.Fatalf("the decision the fold should select is not in the bucket: %s", decisions[0])
	}

	var decided struct {
		Baseline *store.Baseline `json:"baseline"`
	}

	if err := json.Unmarshal(winner, &decided); err != nil {
		t.Fatalf("the decision object does not decode: %v", err)
	}

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("GetBaseline after two approvals: %v", err)
	}

	if decided.Baseline == nil || b.ScanID != decided.Baseline.ScanID {
		t.Errorf("the baseline in force approves %s, want the decision the key order selects", b.ScanID)
	}

	// And the one that lost is still a decision that was taken.
	entries, err := s.Audit(0)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Errorf("the log holds %d of the two approvals: %+v", len(entries), entries)
	}
}

// TestApproveRevokeAndApproveAgainFoldTheSameHoweverTheyBecomeVisible is §8.3
// #5, and it is the case the monotonic bump in decisionInstant exists for.
//
// Under a pinned clock all three decisions ask to be recorded at one instant,
// and the fold would then be choosing between "approved" and "withdrawn" by a
// body digest — deterministic and meaningless. With the bump they are strictly
// ordered, and that order is what every visible subset here folds by: whichever
// of them a lagging listing shows, the answer is the newest one it can see and
// never the one that happened to arrive first.
func TestApproveRevokeAndApproveAgainFoldTheSameHoweverTheyBecomeVisible(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	for _, id := range []string{"scan-1", "scan-2"} {
		if err := s.PutResult(result(id, siteStart(), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "first"); err != nil {
		t.Fatalf("approving: %v", err)
	}

	if err := s.DeleteBaseline("site", model.ConsentReject, "eva"); err != nil {
		t.Fatalf("withdrawing: %v", err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-2", "eva", "second"); err != nil {
		t.Fatalf("re-approving: %v", err)
	}

	// Three decisions of one series at one pinned instant still carry three
	// distinct ordering fields, which is the whole of what the bump buys.
	decisions := fake.keysUnder(siteDecisionDir)
	if len(decisions) != 3 {
		t.Fatalf("three decisions left %d objects: %v", len(decisions), decisions)
	}

	invs := make([]string, 0, len(decisions))

	for _, key := range decisions {
		invs = append(invs, strings.SplitN(strings.TrimPrefix(key, siteDecisionDir), ".", 2)[0])
	}

	if len(slices.Compact(slices.Clone(invs))) != 3 {
		t.Errorf("the three decisions carry ordering fields %v, want three distinct ones", invs)
	}

	// The ordering field is inverted, so the newest decision sorts first and
	// the revocation is next.
	reApproval, revocation := decisions[0], decisions[1]

	for _, tc := range []struct {
		name    string
		hide    string
		approve bool
		scanID  string
	}{
		{name: "everything visible", approve: true, scanID: "scan-2"},
		{name: "the re-approval has not arrived", hide: reApproval},
		{name: "the revocation has not arrived", hide: revocation, approve: true, scanID: "scan-2"},
	} {
		if tc.hide != "" {
			fake.hideFromListings(tc.hide, lagEveryListing)
		}

		b, err := s.GetBaseline("site", model.ConsentReject)

		switch {
		case !tc.approve && !errors.Is(err, store.ErrNotFound):
			t.Errorf("%s: the baseline reads as %v, %v, want ErrNotFound", tc.name, scanOfBaseline(b), err)
		case tc.approve && err != nil:
			t.Errorf("%s: GetBaseline: %v", tc.name, err)
		case tc.approve && b.ScanID != tc.scanID:
			t.Errorf("%s: the baseline approves %s, want %s", tc.name, b.ScanID, tc.scanID)
		}

		fake.reveal(tc.hide)
	}

	// The other direction of AC9's pairing, which holds by construction rather
	// than by timing: an audit pointer is derived from a decision and written
	// after it, so no visible pointer can name a decision that is not there.
	// Asserted over all three, because a pointer whose decision had gone would
	// be a log entry nobody could produce the record for.
	for _, pointer := range fake.keysUnder(auditPrefix) {
		fields := strings.Split(strings.TrimPrefix(pointer, auditPrefix), ".")
		if len(fields) != 3 {
			t.Fatalf("the audit key %q does not split into three fields", pointer)
		}

		if !slices.ContainsFunc(decisions, func(key string) bool {
			return strings.HasSuffix(key, "."+fields[2])
		}) {
			t.Errorf("the audit log holds %q and no decision object hashes to %s", pointer, fields[2])
		}
	}
}

// TestAnApprovalIsNeverVisibleWithoutTheAuditEntryThatExplainsIt is AC9 where a
// bucket has no transaction to give it (§8.3 #5's other half).
//
// The approval and its audit entry are one object under one key, so no
// interruption and no lag can produce one without the other. What can lag is
// the derived pointer the log is read through — and when it does, the store
// must still answer with the approval, because the entry is not missing: it is
// inside the decision, where nothing can lose it separately.
func TestAnApprovalIsNeverVisibleWithoutTheAuditEntryThatExplainsIt(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "agreed"); err != nil {
		t.Fatalf("approving: %v", err)
	}

	fake.hideFromListings(auditPrefix, lagEveryListing)

	b, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("an approval whose audit pointer is not listed yet: %v", err)
	}

	if b.ScanID != "scan-1" {
		t.Errorf("the baseline approves %s", b.ScanID)
	}

	entries, err := s.Audit(0)
	if err != nil {
		t.Fatalf("reading a log whose newest pointer is not listed yet: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("the log shows %d entries through a listing that shows none", len(entries))
	}

	// The record itself was never at risk, because it is in the decision.
	decisions := fake.keysUnder(siteDecisionDir)
	if len(decisions) != 1 {
		t.Fatalf("one approval left %d decisions: %v", len(decisions), decisions)
	}

	body, ok := fake.bodyOf(decisions[0])
	if !ok {
		t.Fatal("the decision object is not in the bucket")
	}

	var decided struct {
		Audit    store.AuditEntry `json:"audit"`
		Baseline *store.Baseline  `json:"baseline"`
	}

	if err := json.Unmarshal(body, &decided); err != nil {
		t.Fatalf("the decision object does not decode: %v", err)
	}

	if decided.Baseline == nil || decided.Audit.Action != "baseline-approved" {
		t.Errorf("the decision carries %+v and %+v, want both halves of one write", decided.Baseline, decided.Audit)
	}

	// And once the pointer is visible the log shows it, with no repair needed.
	fake.reveal(auditPrefix)

	if entries, err = s.Audit(0); err != nil || len(entries) != 1 {
		t.Errorf("the log reads %+v, %v, want the one approval", entries, err)
	}
}

// TestADecisionThatWasListedAndIsGoneIsCorruption is §8.3 #13's second half,
// and it needs a bucket whose listings lag: on a local directory an object that
// has gone leaves the listing with it, so the state cannot be produced at all.
//
// Nothing in this store deletes a decision object. A key that a listing showed
// and the bucket cannot produce therefore means the bucket lost bytes or
// somebody removed them, and the one answer that is not allowed is "there is no
// baseline" — which would silence exactly the findings nobody approved.
func TestADecisionThatWasListedAndIsGoneIsCorruption(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "agreed"); err != nil {
		t.Fatalf("approving: %v", err)
	}

	decisions := fake.keysUnder(siteDecisionDir)
	if len(decisions) != 1 {
		t.Fatalf("one approval left %d decisions: %v", len(decisions), decisions)
	}

	// The listing goes on showing what the bucket no longer holds.
	fake.serveStaleListings(lagEveryListing)
	fake.forget(decisions[0])

	_, err := s.GetBaseline("site", model.ConsentReject)

	switch {
	case errors.Is(err, store.ErrNotFound):
		t.Errorf("a decision that was listed and is gone reads as no baseline: %v", err)
	case !errors.Is(err, store.ErrCorrupt):
		t.Errorf("GetBaseline = %v, want ErrCorrupt", err)
	}
}

// TestACheckpointThatWasListedAndIsGoneIsAnIncompleteIndex is §8.3 #16, and it
// is the third answer this store gives to one question.
//
// "The listing showed a key and the bucket cannot produce it" means something
// different for each of the three kinds of object, and the differences are
// deliberate. An entry was deleted by a concurrent prune, which is ordinary. A
// decision is never deleted by anything, so its absence is corruption. A
// checkpoint is never deleted either, and its absence takes a stretch of
// history with it — so the answer is that the index could not be read, and
// never a history that is quietly short.
//
// Only a bucket whose listings lag can produce the state at all: on a local
// directory an object that has gone leaves the listing with it.
func TestACheckpointThatWasListedAndIsGoneIsAnIncompleteIndex(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	// A compaction summary, keyed the way blobcompact.go writes one: the tag,
	// the instant of the newest entry it covers, and the digest of its body.
	// Its body is never reached — the object is taken away before the read —
	// so what this pins is the answer to the object being gone and not the
	// answer to what is in it (see
	// TestACheckpointThatDoesNotSayWhatItIsIsCorruption for that one).
	checkpoint := siteEntryDir + "k." + siteInverted + "." + strings.Repeat("a", 32)
	lagWrite(t, lagHandle(t, fake), checkpoint, "{}")

	fake.serveStaleListings(lagEveryListing)
	fake.forget(checkpoint)

	_, err := s.ListResults("site", model.ConsentReject, 0)

	switch {
	case errors.Is(err, store.ErrNotFound):
		t.Errorf("a checkpoint that was listed and is gone reads as nothing found: %v", err)
	case !errors.Is(err, store.ErrIndexIncomplete):
		t.Errorf("ListResults = %v, want ErrIndexIncomplete", err)
	}
}

// TestAnEntryThatWasListedAndIsGoneIsOneScanShort is the ordinary case the two
// above are deliberately not.
//
// An entry key and the object under it are written and deleted by the same
// operation, so a key that listed and then could not be read was deleted
// between the two — a concurrent prune, which is a thing that happens. The
// answer is the history as it now is, with the rest of it readable: failing the
// whole read would hide every other scan because one was pruned while it was
// being fetched.
func TestAnEntryThatWasListedAndIsGoneIsOneScanShort(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStore(t, fake)

	for i := range 3 {
		at := siteStart().Add(time.Duration(i) * time.Minute)
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), at, model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	fake.serveStaleListings(lagEveryListing)
	fake.forget(lagEntryKeyOf(t, fake, "scan-1"))

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("one entry deleted mid-read failed the whole listing: %v", err)
	}

	if got := listedScans(summaries); !slices.Equal(got, []string{"scan-2", "scan-0"}) {
		t.Errorf("the listing shows %v, want the two scans still there", got)
	}
}

// TestAScanTheGetCannotSeeYetIsNotFoundAndNotCorruption is AC6 at the other
// read, the one a listing has nothing to do with.
//
// A named key is read-after-write consistent on every provider gocloud reaches,
// which is why this store addresses a scan by ID rather than searching for it —
// but "on every provider it reaches" is not "always", and what a reader must do
// with a GET that misses is treat it as a scan that is not there. Not as a
// damaged index, and not as evidence that has been collected: both of those are
// permanent, and this is a key that will be there on the next attempt.
func TestAScanTheGetCannotSeeYetIsNotFoundAndNotCorruption(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStore(t, fake)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	byID := fake.keysUnder(siteByIDDir)
	if len(byID) != 1 {
		t.Fatalf("one scan left %d objects addressed by ID: %v", len(byID), byID)
	}

	fake.missNext(byID[0], 1)

	_, err := s.GetResult("site", model.ConsentReject, "scan-1")

	switch {
	case errors.Is(err, store.ErrCorrupt), errors.Is(err, store.ErrEvidenceGone):
		t.Errorf("a GET that has not caught up reads as a permanent fault: %v", err)
	case !errors.Is(err, store.ErrNotFound):
		t.Errorf("GetResult = %v, want ErrNotFound", err)
	}

	// And it is not a state the store remembers: the next read is the read the
	// bucket can now answer.
	got, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil || got.ScanID != "scan-1" {
		t.Errorf("GetResult = %v, %v, want scan-1", scanOf(got), err)
	}
}

// TestADeleteTheBucketDidNotDoDoesNotBringAScanBack is why a prune appends a
// tombstone before it deletes anything (AC12).
//
// A delete reported as done and not done is a thing object storage does, and
// without the tombstone the consequence would be a scan reappearing in a
// history an operator had pruned — evidence coming back from the dead, which
// for a store that exists to answer "what did we hold" is the wrong direction
// to fail in. The tombstone is written first and is not conditional on any
// delete succeeding, so the entry that came back is suppressed by the key that
// says it was removed.
func TestADeleteTheBucketDidNotDoDoesNotBringAScanBack(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, pinned(siteStart()))

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	entry := lagEntryKeyOf(t, fake, "scan-1")
	fake.ignoreDeletes(entry, 1)

	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	// The bucket reported the delete as done, so the prune has nothing to
	// report: what it was told and what happened are different, which is the
	// whole point.
	if stats.ResultsDeleted != 1 || stats.IndexKeysFailed != 0 {
		t.Errorf("the prune reported %+v, want one result deleted and nothing failed", stats)
	}

	if !slices.Contains(fake.keysUnder(siteEntryDir), entry) {
		t.Fatal("the entry the bucket refused to delete is not there; this test has nothing to prove")
	}

	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != 0 {
		t.Errorf("the pruned scan came back into the listing: %v", listedScans(summaries))
	}

	if _, err := s.LatestResult("site", model.ConsentReject); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestResult = %v, want ErrNotFound over a history that was pruned", err)
	}
}

// --- what each path costs (AC11, §7.1 as corrected by §7.6) ----------------

// pathCost is one call, what it may cost, and what it must answer.
//
// The call returns a count so that a case can be written without a branch in
// it: how many summaries, how many audit entries, one for a yes. That keeps the
// tables below tables rather than seventeen small functions.
type pathCost struct {
	name      string
	want      lagRequests
	wantCount int
	wantErr   error
	call      func() (int, error)
}

// oneIf turns a yes into a count, so a case that asks a yes-or-no question can
// be written like the ones that ask for a list.
func oneIf(yes bool) int {
	if yes {
		return 1
	}

	return 0
}

// runPathCosts measures each call on its own, in the order they are given.
//
// In order, and not as parallel subtests, because the counters are one bucket's
// and because a write case changes what the cases after it would cost.
func runPathCosts(t *testing.T, fake *lagBucket, cases []pathCost) {
	t.Helper()

	for _, tc := range cases {
		fake.forgetRequests()

		count, err := tc.call()

		switch {
		case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
			t.Fatalf("%s: %v, want %v", tc.name, err, tc.wantErr)
		case tc.wantErr == nil && err != nil:
			t.Fatalf("%s: %v", tc.name, err)
		case count != tc.wantCount:
			t.Fatalf("%s answered with %d, want %d", tc.name, count, tc.wantCount)
		}

		if got := fake.requests(); got != tc.want {
			t.Errorf("%s cost %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// costFixture is the small deployment the two cost tests measure against:
// three scans of one target, an approved baseline, and one recorded action.
func costFixture(t *testing.T) (*lagBucket, store.Store) {
	t.Helper()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	for i := range 3 {
		at := siteStart().Add(time.Duration(i) * time.Minute)
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), at, model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-2", "eva", "agreed"); err != nil {
		t.Fatal(err)
	}

	// Recorded an hour away from the approval's own entry, so the probe for a
	// free nanosecond finds one first time. Two entries filed at one instant
	// would each cost a second listing, which is a property of the log rather
	// than of RecordAudit and is asserted where it belongs
	// (TestARecordedAuditEntryKeepsTheInstantItWasGiven).
	if err := s.RecordAudit(store.AuditEntry{
		At: siteStart().Add(time.Hour), Actor: "eva", Action: "allowlist-added",
		Target: "site", Mode: model.ConsentReject, Subject: "tracker.test",
	}); err != nil {
		t.Fatal(err)
	}

	return fake, s
}

// TestWhatEachReadPathCostsInRequests is AC11: the number of requests per read
// path is documented and asserted by a test, because this is the store where a
// careless read path becomes a line on an invoice rather than a slow query.
//
// Every number here is the design's, from §7.1 as corrected by §7.6, and not
// one softened to whatever the code turned out to do — which is the one failure
// mode a request-count test has. Where the two disagree the disagreement is
// written out beside the case, because a cost table nobody can trust is worse
// than no cost table.
//
// Four properties are worth naming on their own, because they are the ones a
// plausible change would break silently: LatestResult reads no checkpoint,
// ListResults reads no result document (three entries are three reads, not
// three entries and three documents), HasResult and HasBaseline read no body at
// all, and PreviousResult folds the prefix twice on purpose.
func TestWhatEachReadPathCostsInRequests(t *testing.T) {
	t.Parallel()

	fake, s := costFixture(t)

	runPathCosts(t, fake, []pathCost{
		{
			// One listing of the series directory and one read per summary. No
			// result document is opened, which is what keeps a listing cheap
			// however large the scans behind it are.
			name: "ListResults, all of them", want: lagRequests{List: 1, Get: 3}, wantCount: 3,
			call: func() (int, error) {
				got, err := s.ListResults("site", model.ConsentReject, 0)

				return len(got), err
			},
		},
		{
			// A limit bounds the reads as well as the answer: the fold stops
			// as soon as the run it is in has been read out.
			name: "ListResults, two of them", want: lagRequests{List: 1, Get: 2}, wantCount: 2,
			call: func() (int, error) {
				got, err := s.ListResults("site", model.ConsentReject, 2)

				return len(got), err
			},
		},
		{
			// The entry and the document it names, and no checkpoint: the
			// newest entry is at the front of the listing, so the hot path
			// never opens one.
			name: "LatestResult", want: lagRequests{List: 1, Get: 2},
			call: func() (int, error) {
				_, err := s.LatestResult("site", model.ConsentReject)

				return 0, err
			},
		},
		{
			// No listing at all. A scan ID addresses a key.
			name: "GetResult", want: lagRequests{Get: 2},
			call: func() (int, error) {
				_, err := s.GetResult("site", model.ConsentReject, "scan-1")

				return 0, err
			},
		},
		{
			// Attributes and not a body, which is the difference between
			// deciding whether to offer a link and fetching what is behind it.
			name: "HasResult", want: lagRequests{Attributes: 1}, wantCount: 1,
			call: func() (int, error) {
				ok, err := s.HasResult("site", model.ConsentReject, "scan-1")

				return oneIf(ok), err
			},
		},
		{
			// Two listings, and not the ⌈k/1000⌉ + 1 of §7.1: the confirmation
			// re-folds the whole prefix rather than the range between the
			// candidate and the anchor, which is §7.6 #8 and is deliberate —
			// the narrow form cannot see the case that matters, a stale listing
			// omitting a scan newer than the candidate. Three reads: the anchor
			// by ID, the chosen entry by ID, and its document.
			name: "PreviousResult", want: lagRequests{List: 2, Get: 3},
			call: func() (int, error) {
				_, err := s.PreviousResult("site", model.ConsentReject, "scan-2")

				return 0, err
			},
		},
		{
			// One listing of the target markers and one read of each, because
			// the key carries a hashed target and the caller needs the target.
			name: "Series", want: lagRequests{List: 1, Get: 1}, wantCount: 1,
			call: func() (int, error) {
				got, err := s.Series()

				return len(got), err
			},
		},
		{
			// One listing of the decisions and one read of the one in force.
			name: "GetBaseline", want: lagRequests{List: 1, Get: 1}, wantCount: 1,
			call: func() (int, error) {
				b, err := s.GetBaseline("site", model.ConsentReject)

				return oneIf(b != nil), err
			},
		},
		{
			// No read at all: the operation is in the key, so the fold knows
			// whether anything is approved before it fetches anything. This is
			// the question the targets page asks once per series per render.
			name: "HasBaseline", want: lagRequests{List: 1}, wantCount: 1,
			call: func() (int, error) {
				ok, err := s.HasBaseline("site", model.ConsentReject)

				return oneIf(ok), err
			},
		},
		{
			// The loose keys filled the answer, so there is no second listing.
			name: "Audit, as many as there are", want: lagRequests{List: 1, Get: 2}, wantCount: 2,
			call: func() (int, error) {
				got, err := s.Audit(2)

				return len(got), err
			},
		},
		{
			// They did not fill it, so the second listing runs: it is what
			// turns "the log has nothing more" into ErrIndexIncomplete when
			// what it actually has is a compaction this build cannot read
			// (§7.6 #7). Every call against a log shorter than the limit pays
			// it, which is every call in a small deployment.
			name: "Audit, everything", want: lagRequests{List: 2, Get: 2}, wantCount: 2,
			call: func() (int, error) {
				got, err := s.Audit(0)

				return len(got), err
			},
		},
	})
}

// TestWhatEachWritePathCostsInRequests is the other half of AC11, and it is
// where §7.1's table is most wrong: three of its five write rows understate
// what the implementation does, and each correction is recorded beside the case
// rather than quietly assimilated.
//
// The cases run in order and each changes what the ones after it cost, which is
// why they are one table and not five tests.
func TestWhatEachWritePathCostsInRequests(t *testing.T) {
	t.Parallel()

	fake, s := costFixture(t)

	runPathCosts(t, fake, []pathCost{
		{
			// One attribute read to establish the bytes are new, one write for
			// them, and one for the take marker that keeps retention off them
			// until a result names them (§7.6 #1). §7.1 has no row for this at
			// all.
			name: "PutArtifact", want: lagRequests{Attributes: 1, Put: 2},
			call: func() (int, error) {
				_, err := s.PutArtifact("screenshot-before-consent", []byte("fresh bytes"))

				return 0, err
			},
		},
		{
			// Five writes and one attribute read, where §7.1 says "3 + one per
			// artifact (+1 the first time in a series)". Two corrections. The
			// document is content-addressed, so storing it costs the attribute
			// read that establishes it is not already there. And the series
			// marker is written on **every** result rather than only the first:
			// the write is conditional, so it changes nothing after the first
			// time, but the request is made — the cache that would save it
			// cannot be correct on its own, because retention deletes the
			// marker when a series expires entirely (see recordSeries).
			name: "PutResult", want: lagRequests{Attributes: 1, Put: 5},
			call: func() (int, error) {
				return 0, s.PutResult(result("scan-3", siteStart().Add(3*time.Minute), model.ConsentReject))
			},
		},
		{
			// One listing and one write, where §7.1 says no listing at all: the
			// probe for a free nanosecond is what gives two entries of one
			// instant an order a bucket can produce, and it costs a listing
			// (§7.6 #7).
			name: "RecordAudit", want: lagRequests{List: 1, Put: 1},
			call: func() (int, error) {
				return 0, s.RecordAudit(store.AuditEntry{
					At: siteStart().Add(2 * time.Hour), Actor: "eva", Action: "allowlist-added",
					Target: "site", Mode: model.ConsentReject, Subject: "other.test",
				})
			},
		},
		{
			// The scan by ID and its document, the decision listing, then the
			// pin, the decision and the audit pointer. The attribute read is
			// the self-heal of §7.6 #7 probing the one earlier decision of this
			// series for its pointer; it is bounded by auditHealProbes = 8 and
			// never by the size of the series.
			name: "SetBaseline", want: lagRequests{List: 1, Get: 2, Attributes: 1, Put: 3},
			call: func() (int, error) {
				_, err := s.SetBaseline("site", model.ConsentReject, "scan-3", "eva", "second")

				return 0, err
			},
		},
		{
			// No read of the scan, because a revocation approves nothing, and
			// no pin for the same reason. Two heal probes now, one for each
			// earlier decision of this series.
			//
			// Two listings and not one: the second is verifyDecision reading
			// the log back, which is what keeps a withdrawal that lost its own
			// fold from being reported as a withdrawal that took effect. It is
			// paid by the withdrawal alone — an approval that loses fails safe
			// and reads nothing back — and a baseline decision is a human
			// action a few times a year.
			name: "DeleteBaseline", want: lagRequests{List: 2, Attributes: 2, Put: 2},
			call: func() (int, error) {
				return 0, s.DeleteBaseline("site", model.ConsentReject, "eva")
			},
		},
		{
			// §8.3 #13: a withdrawn baseline costs no read at all, because the
			// operation is in the key and the fold knows there is nothing to
			// fetch before it fetches anything.
			name: "GetBaseline, after the withdrawal", want: lagRequests{List: 1}, wantErr: store.ErrNotFound,
			call: func() (int, error) {
				_, err := s.GetBaseline("site", model.ConsentReject)

				return 0, err
			},
		},
	})
}

// TestRetentionDeletesOnlyKeysItHasSeenInAListing is invariant I3's
// precondition, checked against the one pass that does any deleting.
//
// Every delete this store makes has to follow an observation: prune's work list
// is the listing it took of the series directory and of the pins, and a key
// deleted without having been listed would be a deletion decided from something
// other than what the bucket currently shows — which, against a provider whose
// listings lag, is how an index loses an object it still needs.
//
// The assertion is the fake's, and it is on for every test in this file: the
// bucket records each key it reported in a listing, and reports at the end of
// the test any index key that was deleted without having appeared in one. This
// test is where that rule has something to catch, and it is also what keeps the
// rule honest — a check that fired on an ordinary prune would have to be
// switched off, and would then be checking nothing.
func TestRetentionDeletesOnlyKeysItHasSeenInAListing(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, pinned(siteStart()))

	res, screenshot, body := resultWithEvidence(t, s, "scan-1", siteStart())
	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	// A year on, so the scan is past the age limit and every take is long past
	// the grace that made it mean anything.
	stats, err := s.Prune(t.Context(), siteStart().Add(365*24*time.Hour), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if stats.ResultsDeleted != 1 || stats.ArtifactsDeleted != 3 || stats.IndexKeysFailed != 0 {
		t.Errorf("the prune reported %+v, want one result and its three artifacts collected", stats)
	}

	want := []string{
		"_wsaw/index/layout/00000001.json",
		siteEntryDir + "d." + siteInverted + ".scan-1",
	}

	if got := fake.keysUnder(""); !slices.Equal(got, want) {
		t.Errorf("after the prune the bucket holds\n  %s\nwant\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	for _, ref := range []string{screenshot, body} {
		if _, err := s.StatArtifact(t.Context(), ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("artifact %s: %v, want it collected", ref, err)
		}
	}
}

// --- a decision that must not be lost quietly -------------------------------

// pastTheHorizon is a clock reading a year the ordering field cannot express.
//
// A mis-set NTP, a dead RTC and a container started with the wrong epoch all
// produce one, and what makes it worth a test is what clampNano does with it:
// every instant beyond indexHorizon is pinned to one ordering field, so two
// decisions taken a second apart on such a host are two decisions at one
// position in the log.
func pastTheHorizon() time.Time { return time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) }

// tieBreakActors are eight spellings of one person.
//
// Nothing else about the decisions below differs between the cases, so the only
// thing that varies is the digest of the decision body — which is exactly what
// a tie-break on the digest would decide the outcome by. One actor would pass
// or fail by luck; eight is the assertion that luck is not what decides it.
var tieBreakActors = []string{"eva", "ada", "grace", "alan", "lin", "hopper", "turing", "noether"}

// decisionInv is the ordering field of one decision key, which is what two
// decisions have to share for the fold's tie-break to be what answers.
func decisionInv(t *testing.T, key string) string {
	t.Helper()

	fields := strings.SplitN(strings.TrimPrefix(key, siteDecisionDir), ".", 2)
	if len(fields) != 2 {
		t.Fatalf("the decision key %q does not carry an ordering field", key)
	}

	return fields[0]
}

// TestAWithdrawalThatSharesItsApprovalsInstantStillWithdraws is AC8 in the one
// direction a compliance store may not get wrong.
//
// Two decisions of one series can land on one ordering field, by two mechanisms
// that have nothing to do with each other: a listing that had not caught up
// with the approval, so the monotonic bump in decisionInstant had nothing to
// bump against; and a host whose clock is past indexHorizon, where every
// instant clamps to the same field and the bump has nowhere left to go. Both
// are reachable, so both are here.
//
// When one happens the fold has to choose, and the choice may not be the body
// digest: a digest is a coin flip between "approved" and "withdrawn", so a
// withdrawal an operator was told had succeeded would go on silencing findings
// about half the time. Revoke wins at a tie, which is host-independent,
// deterministic, and the only direction a store that silences findings may
// lean.
func TestAWithdrawalThatSharesItsApprovalsInstantStillWithdraws(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		now  func() time.Time
		lag  bool
	}{
		{name: "the withdrawal's listing has not caught up", now: siteStart, lag: true},
		{name: "the clock is past the end of the ordering field", now: pastTheHorizon},
	} {
		for _, actor := range tieBreakActors {
			t.Run(tc.name+"/"+actor, func(t *testing.T) {
				t.Parallel()
				assertAWithdrawalAtOneInstantHolds(t, tc.now, tc.lag, actor)
			})
		}
	}
}

// assertAWithdrawalAtOneInstantHolds approves and withdraws one baseline under
// a clock and a listing the caller chooses, and asserts the withdrawal is what
// stands.
func assertAWithdrawalAtOneInstantHolds(t *testing.T, now func() time.Time, lag bool, actor string) {
	t.Helper()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, now)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", actor, "agreed"); err != nil {
		t.Fatalf("approving: %v", err)
	}

	if lag {
		fake.hideFromListings(siteDecisionDir, lagEveryListing)
	}

	if err := s.DeleteBaseline("site", model.ConsentReject, actor); err != nil {
		t.Fatalf("withdrawing: %v", err)
	}

	fake.reveal(siteDecisionDir)

	decisions := fake.keysUnder(siteDecisionDir)
	if len(decisions) != 2 {
		t.Fatalf("the approval and the withdrawal left %d objects: %v", len(decisions), decisions)
	}

	// The premise, asserted rather than assumed: a change that separated the
	// two instants would leave this test passing for a reason that has nothing
	// to do with the tie-break it is about.
	if a, b := decisionInv(t, decisions[0]), decisionInv(t, decisions[1]); a != b {
		t.Fatalf("the two decisions carry ordering fields %s and %s, so the tie did not happen", a, b)
	}

	has, err := s.HasBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("HasBaseline: %v", err)
	}

	if has {
		t.Errorf("the baseline still stands after a withdrawal that reported success")
	}

	b, err := s.GetBaseline("site", model.ConsentReject)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetBaseline after a withdrawal returned %q, %v, want ErrNotFound", scanOfBaseline(b), err)
	}
}

// movingClock is a clock a test steps, for the cases where the two decisions of
// one series are taken on hosts whose clocks disagree.
//
// It is a type rather than a captured variable because the store reads it from
// its own goroutines, and the race detector is part of this package's gate.
type movingClock struct {
	mu sync.Mutex
	at time.Time
}

// now is what the store under test reads.
func (c *movingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

// set moves the clock, which is the second host taking over.
func (c *movingClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = at
}

// TestAWithdrawalThatCouldNotOutrankItsApprovalIsReported is the failure the
// monotonic bump cannot cover, and the reason a withdrawal reads the log back
// after it has written to it.
//
// The bump orders a new decision past the newest one the caller could *see*.
// Under the one provider behaviour this whole store is designed around — a key
// written a moment ago missing from the next listing — the withdrawal's listing
// does not show the approval, so nothing is bumped; and if the host taking the
// withdrawal has a clock behind the one that took the approval (two machines a
// second apart, or one machine after an NTP step back), the withdrawal is filed
// *before* the approval it answers and loses the fold.
//
// Losing the fold is survivable. Reporting success while losing it is not: the
// operator is told the baseline is withdrawn, findings stay silenced against it,
// and nothing anywhere says so. So the write is read back, and a withdrawal that
// did not take effect is an error — which a retry, against a listing that has by
// then caught up, resolves.
func TestAWithdrawalThatCouldNotOutrankItsApprovalIsReported(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	clock := &movingClock{at: siteStart()}
	s := lagStoreAt(t, fake, clock.now)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "agreed"); err != nil {
		t.Fatalf("approving: %v", err)
	}

	// One listing that has not caught up — the one the withdrawal reads before
	// it writes — and a clock a second behind the one that took the approval.
	fake.hideFromListings(siteDecisionDir, 1)
	clock.set(siteStart().Add(-time.Second))

	err := s.DeleteBaseline("site", model.ConsentReject, "eva")
	if err == nil {
		t.Fatal("withdrawing a baseline the withdrawal could not outrank reported success")
	}

	if !errors.Is(err, store.ErrIndexIncomplete) {
		t.Errorf("withdrawing reported %v, want it to wrap ErrIndexIncomplete", err)
	}

	// The withdrawal is recorded even though it did not take effect: it is a
	// compliance decision somebody took, and nothing here deletes one.
	if got := fake.keysUnder(siteDecisionDir); len(got) != 2 {
		t.Errorf("the bucket holds %d decisions, want the approval and the withdrawal that lost: %v",
			len(got), got)
	}

	// And the store goes on saying what is true rather than what was asked for.
	has, err := s.HasBaseline("site", model.ConsentReject)
	if err != nil {
		t.Fatalf("HasBaseline: %v", err)
	}

	if !has {
		t.Errorf("the withdrawal was reported as ineffective and the baseline is gone anyway")
	}

	// The retry is what the error is for: a listing that has caught up bumps
	// past the approval, and the second withdrawal takes effect.
	if err := s.DeleteBaseline("site", model.ConsentReject, "eva"); err != nil {
		t.Fatalf("withdrawing again: %v", err)
	}

	if has, err = s.HasBaseline("site", model.ConsentReject); err != nil || has {
		t.Errorf("after the retry HasBaseline = %v, %v, want false", has, err)
	}
}

// TestAnApprovalWhoseAuditPointerFailedIsStillAnApproval is AC9 read from the
// caller's side.
//
// The decision object carries the approval and the audit entry that explains
// it, in one body, under one key: once it is written the approval is committed
// and GetBaseline answers from it. The copy under audit/ is a derived pointer
// at that object, written afterwards, and it heals.
//
// So a failure to write the pointer may not be reported as a failure to
// approve. An operator told the approval did not happen approves again, and one
// human decision becomes two entries in a compliance log — while the first was
// in force the whole time.
func TestAnApprovalWhoseAuditPointerFailedIsStillAnApproval(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	fake.failNext(lagPut, auditPrefix, blobRetryAttempts)

	b, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "agreed")
	if err != nil {
		t.Fatalf("an approval whose audit pointer could not be written reported %v, "+
			"want the approval it had already committed", err)
	}

	if b == nil || b.ScanID != "scan-1" {
		t.Fatalf("SetBaseline returned %q, want the approval of scan-1", scanOfBaseline(b))
	}

	got, err := s.GetBaseline("site", model.ConsentReject)
	if err != nil || got.ScanID != "scan-1" {
		t.Errorf("the approval reads back as %q, %v, want scan-1", scanOfBaseline(got), err)
	}

	// The log being briefly behind is the only thing that went wrong.
	if pointers := fake.keysUnder(auditPrefix); len(pointers) != 0 {
		t.Errorf("the audit prefix holds %v, want the pointer that failed to be missing", pointers)
	}

	// And it heals on the next decision of the series, which is where AC9's
	// self-heal lives.
	if err := s.DeleteBaseline("site", model.ConsentReject, "eva"); err != nil {
		t.Fatalf("withdrawing: %v", err)
	}

	if pointers := fake.keysUnder(auditPrefix); len(pointers) != 2 {
		t.Errorf("the audit prefix holds %d entries, want the healed approval and the withdrawal: %v",
			len(pointers), pointers)
	}
}

// TestASweepDoesNotCollectTheEvidenceOfAnInterruptedPutResult is AC6 against
// the sweep, and it is the one interruption the take marker cannot cover.
//
// PutResult writes the document, then the pins, then the object addressed by
// scan ID. A process that dies between the pins and that object leaves pins
// whose owner does not exist — a state PutResult's own comment enumerates as
// survivable. From the sweep's side every pin then reads as gone, and the take
// PutArtifact wrote *before* the pin cannot rescue it: a pin written after a
// take is what releases it.
//
// So the age has to come from the pins themselves. A pin seconds old whose
// owner is absent is a write in flight; a pin days old whose owner is absent is
// the leftover this pass exists for. Without that a scan's screenshots go
// within a second of the interruption — and Story 8.11's rebuild, which the
// preserved document exists for, would restore an entry naming evidence the
// sweep had destroyed.
func TestASweepDoesNotCollectTheEvidenceOfAnInterruptedPutResult(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)

	// The real clock, because what decides this is the bucket's own
	// modification times measured against the sweep's now.
	s := lagStore(t, fake)

	screenshot := []byte("the screenshot of a scan that was never stored")

	ref, err := s.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	res := result("scan-1", time.Now(), model.ConsentReject)
	res.Screenshots = []model.Artifact{{
		Kind: "screenshot-before-consent", Ref: ref, Bytes: int64(len(screenshot)),
	}}

	// The write dies after its pins and before the object addressed by scan ID.
	fake.failNext(lagPut, siteByIDDir, blobRetryAttempts)

	if err := s.PutResult(res); err == nil {
		t.Fatal("the interrupted PutResult reported success")
	}

	stats, err := s.Sweep(t.Context(), time.Now().Add(time.Minute), store.SweepOptions{})
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	if stats.ArtifactsDeleted != 0 {
		t.Errorf("a sweep a minute after an interrupted write collected %d artifacts, want 0: %+v",
			stats.ArtifactsDeleted, stats)
	}

	if _, err := s.StatArtifact(t.Context(), ref); err != nil {
		t.Errorf("the interrupted scan's screenshot was collected: %v", err)
	}

	// And the grace is a grace rather than an exemption: two days on the
	// leftover is what it looks like, and it goes. The document stays — it is
	// self-describing, so a rebuild can restore it (AC6).
	stats, err = s.Sweep(t.Context(), time.Now().Add(48*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatalf("sweeping two days later: %v", err)
	}

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("a sweep two days later collected %d artifacts, want the one screenshot: %+v",
			stats.ArtifactsDeleted, stats)
	}

	if _, err := s.StatArtifact(t.Context(), ref); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the leftover screenshot: %v, want it collected", err)
	}
}
