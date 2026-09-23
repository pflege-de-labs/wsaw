package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// TestACancelledSweepStopsAtTheNextKeyAndStillLeavesAReceipt is Story 4.12,
// AC7: a daemon shutting down in the middle of a sweep does not wait for the
// walk to finish, and what the sweep had already deleted is still recorded
// (Story 4.11, AC4). Nothing it had not reached is touched, and the keys it
// did not reach are not reported as deletes the bucket refused.
//
// The cancellation is placed by the fake bucket, after the first delete,
// rather than by a timer: the test decides where the sweep is interrupted.
func TestACancelledSweepStopsAtTheNextKeyAndStillLeavesAReceipt(t *testing.T) {
	t.Parallel()

	fake, st := faulty(t)

	s, ok := st.(*store.SQL)
	if !ok {
		t.Skipf("the receipt log is a SQL store's; this store is %T", st)
	}

	now := time.Now()

	// A stored result, so the index has something to judge the bucket with
	// and the sweep is not refused as ErrEmptyIndex.
	if err := s.PutResult(result("scan-1", now, model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	// Three objects nothing references, which a sweep a day and a bit from
	// now will find past their grace period.
	orphans := make([]string, 0, 3)

	for i := range 3 {
		ref, err := s.PutArtifact("screenshot-before-consent", fmt.Appendf(nil, "orphan %d", i))
		if err != nil {
			t.Fatal(err)
		}

		orphans = append(orphans, ref)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fake.onDelete(func(string) { cancel() })

	stats, err := s.Sweep(ctx, store.TriggerSchedule, now.Add(25*time.Hour), store.SweepOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep returned %v, want it to report the cancellation", err)
	}

	if stats.ArtifactsDeleted != 1 {
		t.Errorf("ArtifactsDeleted = %d, want 1: the sweep was cancelled by its first delete", stats.ArtifactsDeleted)
	}

	if stats.ArtifactsFailed != 0 {
		t.Errorf("ArtifactsFailed = %d, want 0: keys a cancelled sweep never reached are not refused deletes",
			stats.ArtifactsFailed)
	}

	remaining := 0

	for _, ref := range orphans {
		if _, err := s.StatArtifact(t.Context(), ref); err == nil {
			remaining++
		}
	}

	if remaining != 2 {
		t.Errorf("%d of the three orphans are still stored, want 2: nothing past the cancellation is touched", remaining)
	}

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindSweep)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		t.Fatal("a cancelled sweep left no receipt, so the delete it made is recorded nowhere")
	}

	if run.Error == "" {
		t.Error("the receipt of a cancelled sweep carries no error")
	}

	recorded, err := run.SweepStats()
	if err != nil {
		t.Fatalf("SweepStats: %v", err)
	}

	if recorded.ArtifactsDeleted != 1 {
		t.Errorf("the receipt records %d deletions, want the 1 the sweep made", recorded.ArtifactsDeleted)
	}
}

// TestACancelledPruneStillLeavesAReceipt is the same promise for the prune the
// daemon runs beside the sweep: the receipt is written after the caller's
// context is gone, not under it.
func TestACancelledPruneStillLeavesAReceipt(t *testing.T) {
	t.Parallel()

	s := openSQL(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.Prune(ctx, store.TriggerSchedule, time.Now(), store.Retention{MaxPerSeries: 1}); err == nil {
		t.Fatal("a prune under a cancelled context reported success")
	}

	if _, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune); err != nil || !found {
		t.Errorf("LastMaintenanceRun = found %v, error %v; want the cancelled prune's receipt", found, err)
	}
}
