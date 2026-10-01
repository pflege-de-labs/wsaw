package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// --- Story 1.11: the body sampling ledger ----------------------------------

func sampleRequest(scanID string, at time.Time) store.BodySampleRequest {
	return store.BodySampleRequest{
		Target: "site", Mode: model.ConsentReject, ScanID: scanID, Now: at,
		Ratio: 0.25, Window: 24 * time.Hour,
	}
}

// decideAndComplete decides one observation and settles it as completed, the
// way a successful scan does.
func decideAndComplete(t *testing.T, s store.Store, req store.BodySampleRequest) store.BodySampleDecision {
	t.Helper()

	d, err := s.DecideBodySample(t.Context(), req)
	if err != nil {
		t.Fatalf("DecideBodySample: %v", err)
	}

	if err := s.SettleBodySample(t.Context(), req.ScanID, req.ScanID, store.BodySampleCompleted); err != nil {
		t.Fatalf("SettleBodySample: %v", err)
	}

	return d
}

// AC4: the ratio is honoured over the window, with the first scan of an empty
// window sampled and the rest spaced out.
func TestBodySamplesHonourTheRatio(t *testing.T) {
	t.Parallel()

	s := open(t)
	start := time.Unix(1_700_000_000, 0)

	var got []bool

	for i := range 8 {
		d := decideAndComplete(t, s, sampleRequest(fmt.Sprintf("scan-%d", i), start.Add(time.Duration(i)*time.Hour)))
		got = append(got, d.Sampled)

		if d.WindowScans != i+1 {
			t.Errorf("scan %d: window holds %d scans, want %d", i, d.WindowScans, i+1)
		}
	}

	want := []bool{true, false, false, false, true, false, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sampled = %v, want %v", got, want)

			break
		}
	}
}

// AC5: a restart continues a window rather than starting it over. A store
// opened again on the same database sees the decisions the first one took.
func TestBodySamplesSurviveARestart(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	start := time.Unix(1_700_000_000, 0)

	first, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	if d := decideAndComplete(t, first, sampleRequest("before-restart", start)); !d.Sampled {
		t.Fatal("the first scan of an empty window was not sampled")
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openAt(t, opts)

	d := decideAndComplete(t, second, sampleRequest("after-restart", start.Add(time.Hour)))
	if d.Sampled {
		t.Error("the scan after a restart was sampled again: the window started over")
	}

	if d.WindowScans != 2 || d.WindowSampled != 1 {
		t.Errorf("after the restart the window holds %d scans, %d sampled; want 2 and 1", d.WindowScans, d.WindowSampled)
	}
}

// AC5: two decisions for one series that run at once claim one slot between
// them, never two.
func TestConcurrentBodySamplesClaimOneSlot(t *testing.T) {
	t.Parallel()

	s := open(t)
	at := time.Unix(1_700_000_000, 0)

	const n = 6

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sampled int
		errs    []error
	)

	for i := range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			req := sampleRequest(fmt.Sprintf("concurrent-%d", i), at)
			req.Ratio = 0.1

			d, err := s.DecideBodySample(t.Context(), req)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)

				return
			}

			if d.Sampled {
				sampled++
			}
		}()
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("DecideBodySample: %v", err)
	}

	if sampled != 1 {
		t.Errorf("%d concurrent decisions sampled %d scans; a ratio of 0.1 over %d scans allows exactly 1", n, sampled, n)
	}
}

// AC7: a failed observation does not count, so the next scan takes its
// slot; and a pending one holds its slot while its retry may still come.
func TestBodySampleOutcomes(t *testing.T) {
	t.Parallel()

	s := open(t)
	ctx := t.Context()
	start := time.Unix(1_700_000_000, 0)

	first := sampleRequest("first", start)

	if d, err := s.DecideBodySample(ctx, first); err != nil || !d.Sampled {
		t.Fatalf("first decision = %+v, %v; want sampled", d, err)
	}

	// The first attempt failed and a retry is due: the slot stays claimed.
	if err := s.SettleBodySample(ctx, "first", "first", store.BodySamplePending); err != nil {
		t.Fatal(err)
	}

	if d := decideAndComplete(t, s, sampleRequest("meanwhile", start.Add(time.Minute))); d.Sampled {
		t.Error("a scan sampled the slot a pending retry still holds")
	}

	// Every attempt failed: the slot is released.
	if err := s.SettleBodySample(ctx, "first", "first-retry", store.BodySampleFailed); err != nil {
		t.Fatal(err)
	}

	if d := decideAndComplete(t, s, sampleRequest("after-failure", start.Add(2*time.Minute))); !d.Sampled {
		t.Error("a failed sampled observation kept its slot; the next scan must take its place")
	}

	rows, err := s.BodySamples(ctx, "site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 3 {
		t.Fatalf("ledger holds %d rows, want 3 — a retry adds no row of its own", len(rows))
	}

	if rows[0].Outcome != store.BodySampleFailed || len(rows[0].RetryScanIDs) != 1 || rows[0].RetryScanIDs[0] != "first-retry" {
		t.Errorf("first row = %+v, want failed with the retry's scan ID appended", rows[0])
	}
}

// AC7: a retry that never came — lost to a restart, or to a reload that
// removed the target — stops holding its slot once it is older than
// StaleAfter, and is settled as failed.
func TestAStalePendingSampleIsReleased(t *testing.T) {
	t.Parallel()

	s := open(t)
	ctx := t.Context()
	start := time.Unix(1_700_000_000, 0)

	if _, err := s.DecideBodySample(ctx, sampleRequest("lost", start)); err != nil {
		t.Fatal(err)
	}

	later := sampleRequest("later", start.Add(2*time.Hour))
	later.StaleAfter = time.Hour

	d, err := s.DecideBodySample(ctx, later)
	if err != nil {
		t.Fatal(err)
	}

	if !d.Sampled || d.WindowScans != 1 {
		t.Errorf("decision after a stale pending row = %+v; want sampled, with the stale row not counted", d)
	}

	rows, err := s.BodySamples(ctx, "site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	if rows[0].Outcome != store.BodySampleFailed {
		t.Errorf("stale row outcome = %q, want failed", rows[0].Outcome)
	}
}

// AC8: a forced decision samples whatever the ratio says, and counts towards
// the window so the series is not over its budget afterwards.
func TestAForcedSampleCountsTowardsTheWindow(t *testing.T) {
	t.Parallel()

	s := open(t)
	start := time.Unix(1_700_000_000, 0)

	decideAndComplete(t, s, sampleRequest("scheduled", start))

	forced := sampleRequest("forced", start.Add(time.Hour))
	forced.Forced = true

	if d := decideAndComplete(t, s, forced); !d.Sampled {
		t.Fatal("a forced decision was not sampled")
	}

	// Two of three sampled is already above 0.25 × 3, so the next waits.
	if d := decideAndComplete(t, s, sampleRequest("next", start.Add(2*time.Hour))); d.Sampled {
		t.Error("the scan after a forced sample was sampled too; the forced one did not count")
	}
}

// AC4: only the trailing window counts, and the ledger trims what can no
// longer influence a decision.
func TestBodySamplesOutsideTheWindowDoNotCount(t *testing.T) {
	t.Parallel()

	s := open(t)
	start := time.Unix(1_700_000_000, 0)

	decideAndComplete(t, s, sampleRequest("old", start))

	d := decideAndComplete(t, s, sampleRequest("new", start.Add(25*time.Hour)))
	if !d.Sampled || d.WindowScans != 1 {
		t.Errorf("a scan a window after the last sample = %+v; want sampled as the first of its window", d)
	}

	decideAndComplete(t, s, sampleRequest("much-later", start.Add(72*time.Hour)))

	rows, err := s.BodySamples(t.Context(), "site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range rows {
		if r.ScanID == "old" {
			t.Error("a row two windows old was not trimmed")
		}
	}
}

// AC5: an index rebuilt from the bucket refills the ledger from the decisions
// the documents record, merging a retry into its first attempt's row whatever
// order the documents are read in.
func TestARebuildRefillsTheBodySampleLedger(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)

	decision := &model.BodyCapture{
		Store: model.BodyStoreAll, Ratio: 0.25, Sampled: true,
		Decision: model.BodyDecisionSampled, DecidedBy: "attempt-1",
	}

	failed := result("attempt-1", at, model.ConsentReject)
	failed.Termination = model.TermError
	failed.Error = "browser crashed"
	failed.BodyCapture = decision

	retried := result("attempt-2", at.Add(time.Minute), model.ConsentReject)
	retried.BodyCapture = decision

	unsampled := result("other", at.Add(time.Hour), model.ConsentReject)
	unsampled.BodyCapture = &model.BodyCapture{
		Store: model.BodyStoreAll, Ratio: 0.25, Decision: model.BodyDecisionNotSampled, DecidedBy: "other",
	}

	off := result("off", at.Add(2*time.Hour), model.ConsentReject)
	off.BodyCapture = &model.BodyCapture{Store: model.BodyStoreNone, Decision: model.BodyDecisionDisabled, DecidedBy: "off"}

	for _, res := range []*model.Result{failed, retried, unsampled, off} {
		if err := first.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	second := openAt(t, lostIndex(t, opts))

	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{}); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	rows, err := second.BodySamples(t.Context(), "site", model.ConsentReject)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("rebuilt ledger holds %d rows, want 2 — one per observation, none for a scan with storage off: %+v",
			len(rows), rows)
	}

	byID := map[string]store.BodySample{}
	for _, r := range rows {
		byID[r.ScanID] = r
	}

	sampled := byID["attempt-1"]
	if !sampled.Sampled || sampled.Outcome != store.BodySampleCompleted ||
		len(sampled.RetryScanIDs) != 1 || sampled.RetryScanIDs[0] != "attempt-2" {
		t.Errorf("the retried observation came back as %+v; want sampled, completed, with attempt-2 appended", sampled)
	}

	if r := byID["other"]; r.Sampled || r.Outcome != store.BodySampleCompleted {
		t.Errorf("the unsampled observation came back as %+v", r)
	}
}

// AC15: a stored request payload is evidence on the same terms as a response
// body — kept while a result names it, collected with the result that alone
// did.
func TestRetentionCollectsRequestPayloads(t *testing.T) {
	t.Parallel()

	s := open(t)
	old := time.Now().Add(-30 * 24 * time.Hour)

	payload, err := s.PutArtifact("body", []byte(`{"event":"pageview"}`))
	if err != nil {
		t.Fatal(err)
	}

	res := result("scan-old", old, model.ConsentReject)
	res.Requests[0].RequestBodyRef = payload
	res.Requests[0].RequestBodySize = 20

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	// A newer scan keeps the series alive, so the prune has one to remove.
	if err := s.PutResult(result("scan-new", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	assertStored(t, s, payload, "the request payload")

	if _, err := s.Prune(t.Context(), store.TriggerCLI, time.Now(), store.Retention{MaxAge: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	assertGone(t, s, payload, "the pruned scan's request payload")
}

// Series are independent: a sample of one consent mode is not a sample of
// another.
func TestBodySampleSeriesAreIsolated(t *testing.T) {
	t.Parallel()

	s := open(t)
	at := time.Unix(1_700_000_000, 0)

	decideAndComplete(t, s, sampleRequest("reject", at))

	accept := sampleRequest("accept", at)
	accept.Mode = model.ConsentAccept

	if d := decideAndComplete(t, s, accept); !d.Sampled {
		t.Error("the first accept scan was not sampled because a reject scan was")
	}
}
