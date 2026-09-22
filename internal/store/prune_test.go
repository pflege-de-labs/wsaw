package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// terminated builds a result that ended for a given reason, so a test can
// place a failed scan where retention has to choose around it.
func terminated(id string, at time.Time, term model.TerminationReason) *model.Result {
	res := result(id, at, model.ConsentReject)
	res.Termination = term

	return res
}

func storedIDs(t *testing.T, s store.Store) []string {
	t.Helper()

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	out := make([]string, 0, len(got))

	for _, g := range got {
		out = append(out, g.ScanID)
	}

	return out
}

// TestPruneKeepPolicyThinsHistory: four scans a day for ten days, keeping two
// days and three weeks, leaves one scan per period rather than a cut-off.
func TestPruneKeepPolicyThinsHistory(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

	for day := range 10 {
		for hour := range 4 {
			at := now.AddDate(0, 0, -day).Add(time.Duration(-hour*3) * time.Hour)

			if err := s.PutResult(result(fmt.Sprintf("d%d-h%d", day, hour), at, model.ConsentReject)); err != nil {
				t.Fatal(err)
			}
		}
	}

	stats, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{Keep: &store.Keep{Daily: 2, Weekly: 3, Location: time.UTC}})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Two days, plus the weeks those days do not already cover.
	kept := storedIDs(t, s)
	if len(kept) != stats.ResultsKept {
		t.Errorf("stats say %d kept, store holds %d", stats.ResultsKept, len(kept))
	}

	if len(kept) < 3 || len(kept) > 4 {
		t.Fatalf("kept %v, want one scan per kept day and week", kept)
	}

	if kept[0] != "d0-h0" {
		t.Errorf("newest kept = %q, want the newest scan", kept[0])
	}

	if stats.ResultsDeleted != 40-len(kept) {
		t.Errorf("ResultsDeleted = %d, want %d", stats.ResultsDeleted, 40-len(kept))
	}

	if stats.SeriesPruned != 1 {
		t.Errorf("SeriesPruned = %d, want 1", stats.SeriesPruned)
	}
}

// TestPruneKeepsTheUsableScanOfADay is the completeness preference end to
// end: the day's newest scan hit the hard timeout, and the day's clean scan
// is the one that has to survive (Tenet 5).
func TestPruneKeepsTheUsableScanOfADay(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, time.March, 20, 23, 59, 0, 0, time.UTC)

	for _, res := range []*model.Result{
		terminated("timeout", now.Add(-30*time.Minute), model.TermTimeout),
		terminated("clean", now.Add(-12*time.Hour), model.TermIdle),
		terminated("failed", now.Add(-18*time.Hour), model.TermError),
	} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{Keep: &store.Keep{Daily: 1, Location: time.UTC}}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	kept := storedIDs(t, s)
	if len(kept) != 1 || kept[0] != "clean" {
		t.Errorf("kept %v, want only the day's clean scan", kept)
	}
}

// TestPrunePlanDeletesNothing: the dry run has to be readable before it is
// trusted, and it must not touch the store.
func TestPrunePlanDeletesNothing(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

	for i := range 5 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), now.AddDate(0, 0, -i), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.PlanPrune(t.Context(), now, store.Retention{Keep: &store.Keep{Last: 2}})
	if err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}

	if len(stats.Plans) != 1 {
		t.Fatalf("planned %d series, want 1", len(stats.Plans))
	}

	plan := stats.Plans[0]
	if plan.Kept() != 2 || plan.Deleted() != 3 {
		t.Errorf("plan keeps %d and deletes %d, want 2 and 3", plan.Kept(), plan.Deleted())
	}

	for _, d := range plan.Decisions {
		if d.Keep && d.Rule == "" {
			t.Errorf("%s is kept by no named rule", d.ScanID)
		}
	}

	if got := len(storedIDs(t, s)); got != 5 {
		t.Errorf("a dry run left %d results, want all 5", got)
	}
}

// TestPruneIsIdempotent: a second prune against an unchanged store must find
// nothing to do. A policy that kept thinning would eat a history one hourly
// run at a time.
func TestPruneIsIdempotent(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

	for i := range 12 {
		if err := s.PutResult(result(fmt.Sprintf("scan-%d", i), now.Add(-time.Duration(i)*6*time.Hour), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	r := store.Retention{Keep: &store.Keep{Last: 1, Daily: 2, Location: time.UTC}}

	if _, err := s.Prune(t.Context(), store.TriggerCLI, now, r); err != nil {
		t.Fatalf("first Prune: %v", err)
	}

	first := storedIDs(t, s)

	stats, err := s.Prune(t.Context(), store.TriggerCLI, now, r)
	if err != nil {
		t.Fatalf("second Prune: %v", err)
	}

	if stats.ResultsDeleted != 0 {
		t.Errorf("a second prune deleted %d results, want 0", stats.ResultsDeleted)
	}

	if got := storedIDs(t, s); len(got) != len(first) {
		t.Errorf("a second prune left %d results, want %d", len(got), len(first))
	}
}

// TestPruneDeletesInBatches exercises the path a first prune after a policy
// change takes: more condemned results than one statement may name.
func TestPruneDeletesInBatches(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

	const count = 450

	for i := range count {
		if err := s.PutResult(result(fmt.Sprintf("scan-%03d", i), now.Add(-time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{Keep: &store.Keep{Last: 1}})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if stats.ResultsDeleted != count-1 {
		t.Errorf("ResultsDeleted = %d, want %d", stats.ResultsDeleted, count-1)
	}

	kept := storedIDs(t, s)
	if len(kept) != 1 || kept[0] != "scan-000" {
		t.Errorf("kept %v, want only the newest scan", kept)
	}
}

// TestPruneKeepsEverySeriesSeparate: one series being thinned must not touch
// another target's history, or another consent mode of the same target.
func TestPruneKeepsEverySeriesSeparate(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

	for i := range 3 {
		reject := result(fmt.Sprintf("reject-%d", i), now.AddDate(0, 0, -i), model.ConsentReject)
		accept := result(fmt.Sprintf("accept-%d", i), now.AddDate(0, 0, -i), model.ConsentAccept)

		if err := s.PutResult(reject); err != nil {
			t.Fatal(err)
		}

		if err := s.PutResult(accept); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{Keep: &store.Keep{Last: 1}}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, mode := range []model.ConsentMode{model.ConsentReject, model.ConsentAccept} {
		got, err := s.ListResults("site", mode, 0)
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 1 {
			t.Errorf("%s kept %d results, want 1", mode, len(got))
		}
	}
}
