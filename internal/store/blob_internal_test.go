package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// White-box tests of the bucket-index store's own decisions (Story 8.10).

// TestTheBucketIndexRetriesWhatTheBucketMarkedTransient pins the one judgement
// this store makes about failure, and pins it as a judgement it borrows.
//
// bucket.do classifies a provider failure by its gcerrors code and marks the
// retryable ones; this store's retry policy honours the mark instead of reading
// messages the way the SQL dialects have to. A second classifier here would be
// a second answer to a settled question, and it would disagree first on exactly
// the failures that happen least often and cost most.
func TestTheBucketIndexRetriesWhatTheBucketMarkedTransient(t *testing.T) {
	t.Parallel()

	marked := &transientBucketError{err: errors.New("the service asked for a slower rate")}

	cases := map[string]struct {
		err  error
		want bool
	}{
		"a marked failure":                      {err: marked, want: true},
		"a marked failure a caller has wrapped": {err: fmt.Errorf("writing an index object: %w", marked), want: true},
		"a refusal nothing marked":              {err: errors.New("access denied"), want: false},
		"nothing at all":                        {err: nil, want: false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := isMarkedTransient(tc.err); got != tc.want {
				t.Errorf("isMarkedTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestOnlyAFailureWorthRetryingIsMarked joins the two halves, so that a change
// to either bucket.do's classification or to the predicate that reads it is
// caught here rather than by a deployment retrying a permanent failure three
// times over.
func TestOnlyAFailureWorthRetryingIsMarked(t *testing.T) {
	t.Parallel()

	b := &bucket{location: "mem://", retry: retryOnce}

	// A transport failure, which arrives from every driver with no service code
	// and is recognised by wording. This is the case another attempt is for.
	dropped := b.do(t.Context(), "a bucket call", func(context.Context) error {
		return errors.New("connection reset by peer")
	})

	if !isMarkedTransient(dropped) {
		t.Errorf("a dropped connection was not marked as worth retrying: %v", dropped)
	}

	// An absent key, produced by a real provider rather than fabricated: the
	// code that says "not found" is one only the driver can set.
	missing := realNotFound(t)

	if isMarkedTransient(missing) {
		t.Errorf("an absent key was marked as worth retrying: %v", missing)
	}
}

// realNotFound asks a real local bucket for a key it does not hold, so the
// error under test carries the provider's own code.
func realNotFound(t *testing.T) error {
	t.Helper()

	b, err := openFileBucket(t.TempDir())
	if err != nil {
		t.Fatalf("opening a local bucket: %v", err)
	}

	t.Cleanup(func() { _ = b.close() })

	_, err = b.get(t.Context(), "screenshot-before-consent/"+strings.Repeat("a", digestLength))
	if err == nil {
		t.Fatal("an empty bucket answered with an artifact")
	}

	return err
}

// --- the fold's own decisions ---------------------------------------------

// foldSeries is the series every fold test below folds.
func foldSeries() blobSeries { return blobSeriesFor("site", model.ConsentReject) }

// at is one instant, spelled so that a test reads as a time rather than as a
// nineteen-digit ordering field.
func at(second, nano int) time.Time {
	return time.Date(2026, 9, 8, 10, 45, second, nano, time.UTC)
}

// TestTheAnswerIsNewestFirstAndTiesGoToTheHigherScanID pins the order every read
// of a series is returned in.
//
// It is two rules and they come from different places, which is why they are
// tested together. Newest first is the key: the ordering field is inverted, so
// the bucket's own ascending order is already the answer's. The tie-break is not
// the key — a run of entries at one instant arrives in ascending scan ID and the
// answer wants descending, the same order SQL gets from `order by started_at
// desc, scan_id desc` (AC5, AC7).
func TestTheAnswerIsNewestFirstAndTiesGoToTheHigherScanID(t *testing.T) {
	t.Parallel()

	series := foldSeries()

	cases := map[string]struct {
		a, b seriesRecord
		want string
	}{
		"the newer scan comes first": {
			a:    series.id.entry(at(30, 0), "scan-a", "idle"),
			b:    series.id.entry(at(10, 0), "scan-b", "idle"),
			want: "a",
		},
		"a nanosecond is enough to order two scans": {
			a:    series.id.entry(at(10, 1), "scan-a", "idle"),
			b:    series.id.entry(at(10, 0), "scan-b", "idle"),
			want: "a",
		},
		"one instant is broken by the higher scan ID": {
			a:    series.id.entry(at(10, 0), "scan-b", "idle"),
			b:    series.id.entry(at(10, 0), "scan-a", "idle"),
			want: "a",
		},
		"the termination cannot reorder a tie": {
			a:    series.id.entry(at(10, 0), "scan-a", "error"),
			b:    series.id.entry(at(10, 0), "scan-a", "idle"),
			want: "equal",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := compareEntries(tc.a, tc.b)

			switch tc.want {
			case "a":
				if got >= 0 {
					t.Errorf("compare = %d, want the first to sort ahead", got)
				}

				if back := compareEntries(tc.b, tc.a); back <= 0 {
					t.Errorf("compare is not antisymmetric: %d and %d", got, back)
				}
			case "equal":
				if got != 0 {
					t.Errorf("compare = %d, want 0: only the instant and the scan ID order an entry", got)
				}
			}
		})
	}
}

// TestWhatCountsAsBeforeAnAnchorIsWhatSQLCountsAsBefore holds the blob store's
// "the scan before this one" to the row-value comparison the SQL stores make.
//
// The anchor itself is excluded, an older instant is included, and at one
// instant the lower scan ID is the older one — which is the tie-break of
// TestTheAnswerIsNewestFirstAndTiesGoToTheHigherScanID read the other way round.
// If the two rules ever disagree, a comparison would be made against a scan the
// listing puts on the wrong side of the anchor.
func TestWhatCountsAsBeforeAnAnchorIsWhatSQLCountsAsBefore(t *testing.T) {
	t.Parallel()

	series := foldSeries()
	anchor := series.id.entry(at(20, 0), "scan-m", "idle")

	cases := map[string]struct {
		record seriesRecord
		want   bool
	}{
		"an older instant":                     {record: series.id.entry(at(10, 0), "scan-z", "idle"), want: true},
		"a newer instant":                      {record: series.id.entry(at(30, 0), "scan-a", "idle"), want: false},
		"the anchor itself":                    {record: anchor, want: false},
		"the same instant, a lower scan ID":    {record: series.id.entry(at(20, 0), "scan-a", "idle"), want: true},
		"the same instant, a higher scan ID":   {record: series.id.entry(at(20, 0), "scan-z", "idle"), want: false},
		"a nanosecond older, a higher scan ID": {record: series.id.entry(at(19, 999999999), "scan-z", "idle"), want: true},
		"a nanosecond newer, a lower scan ID":  {record: series.id.entry(at(20, 1), "scan-a", "idle"), want: false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := olderThan(tc.record, anchor); got != tc.want {
				t.Errorf("olderThan = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAFoldReadsAWholeRunBeforeItStops is why a fold cannot stop the moment it
// has enough entries.
//
// A listing hands back the entries of one instant in ascending scan ID and the
// answer wants the highest of them, so a fold that stopped at the first entry
// past its limit would return whichever member of the run happened to sort
// first — a wrong "latest", deterministically, whenever two scans of one target
// start in the same nanosecond.
func TestAFoldReadsAWholeRunBeforeItStops(t *testing.T) {
	t.Parallel()

	series := foldSeries()
	fold := newSeriesFold(1, false, nil)

	// The order one listing of the directory returns them in.
	listed := []seriesRecord{
		series.id.entry(at(20, 0), "scan-a", "idle"),
		series.id.entry(at(20, 0), "scan-b", "idle"),
		series.id.entry(at(20, 0), "scan-c", "idle"),
		series.id.entry(at(10, 0), "scan-older", "idle"),
	}

	for _, record := range listed {
		if err := fold.observe(record); err != nil {
			t.Fatalf("observing %s: %v", record, err)
		}

		if fold.full {
			break
		}
	}

	if !fold.full {
		t.Error("the fold did not stop once the run past its limit began")
	}

	answer := fold.answer()

	if len(answer) != 1 {
		t.Fatalf("the answer holds %d entries, want 1", len(answer))
	}

	if answer[0].record.scan != "scan-c" {
		t.Errorf("the newest scan is %s, want scan-c: the run was not read out before the answer was fixed",
			answer[0].record.scan)
	}
}

// TestAFoldRefusesAnImpossibleRunOfOneInstant is the bound on the one part of a
// fold that is not limited by what was asked for.
//
// Entries sharing an instant have to be read to completion, so an unbounded run
// is an unbounded read on the hot path. A thousand scans of one target starting
// in the same nanosecond is a key collision or somebody else's objects, and
// saying so beats folding it.
func TestAFoldRefusesAnImpossibleRunOfOneInstant(t *testing.T) {
	t.Parallel()

	series := foldSeries()
	fold := newSeriesFold(maxTieRun+10, false, nil)

	var err error

	for i := 0; i <= maxTieRun && err == nil; i++ {
		err = fold.observe(series.id.entry(at(20, 0), encodedScan(fmt.Sprintf("scan-%04d", i)), "idle"))
	}

	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("a run of more than %d entries at one instant = %v, want ErrCorrupt", maxTieRun, err)
	}
}

// TestAFoldSelectsFromTheKeyAlone pins that the three things a read selects on
// are all answered by the key.
//
// This is what the split between foldKeys and hydrate is for: a tombstoned
// scan, a failed scan a comparison must skip, and a scan on the wrong side of
// an anchor are each excluded without any object being read. Without it,
// PreviousResult against a target with two hundred consecutive failures behind
// it would be two hundred reads.
func TestAFoldSelectsFromTheKeyAlone(t *testing.T) {
	t.Parallel()

	series := foldSeries()
	anchor := series.id.entry(at(50, 0), "scan-anchor", "idle")

	fold := newSeriesFold(10, true, &anchor)

	listed := []seriesRecord{
		// Tombstones sort ahead of every entry in the one directory they share,
		// so the fold knows what has been pruned before it looks at anything.
		series.id.tombstone(at(40, 0), "scan-pruned"),
		series.id.entry(at(60, 0), "scan-newer", "idle"),
		anchor,
		series.id.entry(at(45, 0), "scan-failed", "error"),
		series.id.entry(at(44, 0), "scan-skipped", "skipped"),
		series.id.entry(at(43, 0), "scan-truncated", "timeout"),
		series.id.entry(at(40, 0), "scan-pruned", "idle"),
		series.id.entry(at(30, 0), "scan-good", "idle"),
	}

	for _, record := range listed {
		if err := fold.observe(record); err != nil {
			t.Fatalf("observing %s: %v", record, err)
		}
	}

	var got []string

	for _, entry := range fold.answer() {
		got = append(got, string(entry.record.scan))
	}

	want := []string{"scan-truncated", "scan-good"}

	if !slices.Equal(got, want) {
		t.Errorf("the fold selected %v, want %v: a truncated scan is still an observation, "+
			"and everything newer than the anchor, failed, skipped or pruned is not", got, want)
	}
}

// TestAListingOfEverythingIsStillBounded holds the one place the store
// interface's "zero means all" meets a bucket that charges per request.
func TestAListingOfEverythingIsStillBounded(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		limit int
		want  int
	}{
		"a limit the interface will render": {limit: 50, want: 50},
		"all of them":                       {limit: 0, want: blobMaxHydrate},
		"a negative limit":                  {limit: -1, want: blobMaxHydrate},
		"more than the cap":                 {limit: blobMaxHydrate * 4, want: blobMaxHydrate},
		"exactly the cap":                   {limit: blobMaxHydrate, want: blobMaxHydrate},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := hydrateLimit(tc.limit); got != tc.want {
				t.Errorf("hydrateLimit(%d) = %d, want %d", tc.limit, got, tc.want)
			}
		})
	}
}

// TestAnEntryIsCheckedAgainstTheKeyItWasFoundUnder is the check that the key
// being a pure function of the body buys for free.
//
// Nothing in an entry object points back at its key, because nothing needs to:
// the summary carries the start time, the scan ID and the termination, which is
// every field of the key. That makes "is this the object that belongs here" a
// comparison rather than a trust decision, and it is the one that catches a body
// written to the wrong key.
func TestAnEntryIsCheckedAgainstTheKeyItWasFoundUnder(t *testing.T) {
	t.Parallel()

	series := foldSeries()

	body := entryBody{Summary: Summary{
		ScanID:      "scan-1",
		Target:      "site",
		ConsentMode: model.ConsentReject,
		StartedAt:   at(20, 0),
		Termination: model.TermIdle,
	}}

	if !body.describes(body.record(series.id)) {
		t.Fatal("an entry does not describe the key derived from it")
	}

	cases := map[string]seriesRecord{
		"another scan at the same instant": series.id.entry(at(20, 0), "scan-2", tm(model.TermIdle)),
		"the same scan at another instant": series.id.entry(at(21, 0), "scan-1", tm(model.TermIdle)),
		"another termination":              series.id.entry(at(20, 0), "scan-1", tm(model.TermError)),
		"another target": blobSeriesFor("other", model.ConsentReject).id.
			entry(at(20, 0), "scan-1", tm(model.TermIdle)),
		"another consent mode": blobSeriesFor("site", model.ConsentAccept).id.
			entry(at(20, 0), "scan-1", tm(model.TermIdle)),
	}

	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if body.describes(record) {
				t.Errorf("an entry claimed to describe %s", record)
			}
		})
	}
}

// TestTheOrderingFieldInvertsBackToTheInstant is what lets a listing report when
// a scan started without reading anything.
//
// Retention by age depends on it, and so does a listing that finds an entry
// object damaged and still has to say when the scan it names happened rather
// than show no time at all.
func TestTheOrderingFieldInvertsBackToTheInstant(t *testing.T) {
	t.Parallel()

	for _, want := range []time.Time{
		at(20, 0),
		at(20, 123456789),
		time.Unix(0, 0).UTC(),
		time.Unix(0, math.MaxInt64).UTC(),
	} {
		if got := instantAt(inv(want)); !got.Equal(want) {
			t.Errorf("instantAt(inv(%s)) = %s", want, got)
		}
	}

	for _, field := range []string{"", "not-a-number", "12345", strings.Repeat("9", 19)} {
		if got := instantAt(field); !got.IsZero() {
			t.Errorf("instantAt(%q) = %s, want the zero time", field, got)
		}
	}
}

// --- the baseline fold ----------------------------------------------------

// decisionKeys builds the keys of one series' decisions from (instant,
// operation, digest) triples, in the order a listing would return them.
func decisionKeys(t *testing.T, triples ...[3]string) []decisionKey {
	t.Helper()

	keys := make([]decisionKey, 0, len(triples))

	for _, triple := range triples {
		keys = append(keys, decisionKey{
			series: foldSeries().id,
			inv:    triple[0],
			stamp:  stamp(instantAt(triple[0])),
			op:     baselineOp(triple[1]),
			did:    triple[2],
		})
	}

	return keys
}

// digest is a decision-body digest of the right width, distinguished by its
// first byte, so that a tie-break by digest can be written down and read.
func digest(first string) string {
	return first + strings.Repeat("0", identityHexLen-len(first))
}

// TestTheBaselineInForceIsTheNewestDecisionAndTiesGoToTheLowerDigest is the fold
// AC8 asks for, written as the table of cases it has to get right.
//
// The ordering field is inverted, so the newest decision is the one with the
// smallest field and the listing has already put it first. A tie is broken by
// the body digest and deliberately not by where the key fell in the listing: the
// field after the ordering pair is the operation, and "approve" sorts before
// "revoke", so taking the first key would resolve every simultaneous approval
// and withdrawal in favour of the approval — which is the direction a compliance
// store must not lean.
func TestTheBaselineInForceIsTheNewestDecisionAndTiesGoToTheLowerDigest(t *testing.T) {
	t.Parallel()

	newer, older := inv(at(2, 0)), inv(at(1, 0))

	cases := map[string]struct {
		keys    []decisionKey
		wantAny bool
		wantOp  baselineOp
		wantDid string
	}{
		"nothing has ever been decided": {
			keys: nil,
		},
		"one approval": {
			keys:    decisionKeys(t, [3]string{newer, "approve", digest("a")}),
			wantAny: true, wantOp: opApprove, wantDid: digest("a"),
		},
		"a withdrawal after an approval": {
			keys: decisionKeys(t,
				[3]string{newer, "revoke", digest("b")},
				[3]string{older, "approve", digest("a")}),
			wantAny: true, wantOp: opRevoke, wantDid: digest("b"),
		},
		"a re-approval after a withdrawal": {
			keys: decisionKeys(t,
				[3]string{newer, "approve", digest("c")},
				[3]string{older, "revoke", digest("b")}),
			wantAny: true, wantOp: opApprove, wantDid: digest("c"),
		},
		"a withdrawal at the same instant as an approval": {
			// The listing puts "approve" first, because that is where the
			// operation field sorts. The fold must not.
			keys: decisionKeys(t,
				[3]string{newer, "approve", digest("f")},
				[3]string{newer, "revoke", digest("1")}),
			wantAny: true, wantOp: opRevoke, wantDid: digest("1"),
		},
		"a tie is decided before an older decision is even looked at": {
			keys: decisionKeys(t,
				[3]string{newer, "revoke", digest("d")},
				[3]string{newer, "revoke", digest("2")},
				[3]string{older, "approve", digest("0")}),
			wantAny: true, wantOp: opRevoke, wantDid: digest("2"),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := currentDecision(tc.keys)

			if ok != tc.wantAny {
				t.Fatalf("currentDecision found a decision = %v, want %v", ok, tc.wantAny)
			}

			if !ok {
				return
			}

			if got.op != tc.wantOp || got.did != tc.wantDid {
				t.Errorf("the decision in force is %s/%s, want %s/%s",
					got.op, got.did, tc.wantOp, tc.wantDid)
			}
		})
	}
}

// TestTheDecisionListingStopsWhenTheRunOfOneInstantHasEnded pins the condition
// that decides whether readDecisions has to ask for another page.
//
// A fold cannot choose between decisions taken at one instant until it has seen
// all of them, so a page whose every key shares the newest instant is a page
// that may have been cut in half. A page that does not is complete for the
// purpose, whatever else the prefix holds behind it.
func TestTheDecisionListingStopsWhenTheRunOfOneInstantHasEnded(t *testing.T) {
	t.Parallel()

	newer, older := inv(at(2, 0)), inv(at(1, 0))

	cases := map[string]struct {
		keys []decisionKey
		want bool
	}{
		"nothing parsed, so the walk is not finished": {
			keys: nil, want: true,
		},
		"one decision, which may have a twin on the next page": {
			keys: decisionKeys(t, [3]string{newer, "approve", digest("a")}), want: true,
		},
		"two decisions of one instant": {
			keys: decisionKeys(t,
				[3]string{newer, "approve", digest("a")},
				[3]string{newer, "revoke", digest("b")}),
			want: true,
		},
		"the run has ended": {
			keys: decisionKeys(t,
				[3]string{newer, "approve", digest("a")},
				[3]string{older, "revoke", digest("b")}),
			want: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := oneInstant(tc.keys); got != tc.want {
				t.Errorf("oneInstant = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnAuditPointerIsAPureFunctionOfItsDecision is why audit compaction can
// never orphan a baseline.
//
// The pointer's key is built from the decision's own ordering fields and its own
// body digest, so anything that can see a decision can work out where its audit
// entry belongs and put it back — without a listing, without a back-pointer, and
// without a second place where the two could drift apart.
func TestAnAuditPointerIsAPureFunctionOfItsDecision(t *testing.T) {
	t.Parallel()

	key := foldSeries().id.decision(at(1, 0), opApprove, digest("a"))
	pointer := key.auditPointer()

	if pointer.inv != key.inv || pointer.stamp != key.stamp || pointer.did != key.did {
		t.Fatalf("the pointer %s does not carry the decision's own fields (%s)", pointer, key)
	}

	// Twice, from the same decision, is the same key: that is what the self-heal
	// depends on.
	if again := key.auditPointer(); again.String() != pointer.String() {
		t.Errorf("two derivations of one pointer gave %s and %s", pointer, again)
	}

	// And it parses back, so the read path can find it.
	parsed, err := parseAuditKey(pointer.String())
	if err != nil {
		t.Fatalf("the pointer key %s does not parse: %v", pointer, err)
	}

	if parsed != pointer {
		t.Errorf("the pointer key %s parsed back as %v", pointer, parsed)
	}
}
