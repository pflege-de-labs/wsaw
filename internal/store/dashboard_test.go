package store

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// These are white-box tests of Story 5.32's own read of the index: per-series
// totals and a month-by-month figure for what is still stored.

// TestSeriesStorageGroupsByTargetAndMode: AC2 — one row per series, its
// count, its document and artifact bytes, and the span it covers.
func TestSeriesStorageGroupsByTargetAndMode(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	now := time.Now()

	site := storedResult("site-a", now.Add(-2*time.Hour))
	if err := s.PutResult(site); err != nil {
		t.Fatal(err)
	}

	site2 := storedResult("site-b", now.Add(-time.Hour))
	if err := s.PutResult(site2); err != nil {
		t.Fatal(err)
	}

	other := storedResult("other-a", now)
	other.Target = "other"
	other.ConsentMode = model.ConsentAccept

	if err := s.PutResult(other); err != nil {
		t.Fatal(err)
	}

	got, err := s.SeriesStorage(t.Context())
	if err != nil {
		t.Fatalf("SeriesStorage: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	byTarget := make(map[string]SeriesStorage, len(got))
	for _, sd := range got {
		byTarget[sd.Series.Target+"/"+string(sd.Series.Mode)] = sd
	}

	siteSeries, ok := byTarget["site/reject"]
	if !ok {
		t.Fatal("site/reject is missing from SeriesStorage")
	}

	if siteSeries.Count != 2 {
		t.Errorf("site/reject Count = %d, want 2", siteSeries.Count)
	}

	if siteSeries.DocumentBytes <= 0 {
		t.Errorf("site/reject DocumentBytes = %d, want > 0", siteSeries.DocumentBytes)
	}

	if !siteSeries.Oldest.Equal(site.StartedAt.UTC()) {
		t.Errorf("site/reject Oldest = %v, want %v", siteSeries.Oldest, site.StartedAt.UTC())
	}

	if !siteSeries.Newest.Equal(site2.StartedAt.UTC()) {
		t.Errorf("site/reject Newest = %v, want %v", siteSeries.Newest, site2.StartedAt.UTC())
	}

	otherSeries, ok := byTarget["other/accept"]
	if !ok {
		t.Fatal("other/accept is missing from SeriesStorage")
	}

	if otherSeries.Count != 1 {
		t.Errorf("other/accept Count = %d, want 1", otherSeries.Count)
	}
}

// TestMonthlyStorageFillsEmptyMonthsWithZero: AC5 — a month with nothing
// stored in it reads as an explicit zero rather than being skipped, so a
// thinned month reads as small instead of vanishing from the chart.
func TestMonthlyStorageFillsEmptyMonthsWithZero(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC)
	twoMonthsAgo := thisMonth.AddDate(0, -2, 0)

	// The middle month gets nothing stored in it at all — that is the gap
	// the zero-fill has to cover.
	if err := s.PutResult(storedResult("old", twoMonthsAgo)); err != nil {
		t.Fatal(err)
	}

	if err := s.PutResult(storedResult("current", thisMonth)); err != nil {
		t.Fatal(err)
	}

	got, err := s.MonthlyStorage(t.Context(), twoMonthsAgo)
	if err != nil {
		t.Fatalf("MonthlyStorage: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (three months from two-months-ago through this month)", len(got))
	}

	if got[0].Bytes <= 0 {
		t.Errorf("the oldest month's bytes = %d, want > 0 (it holds \"old\")", got[0].Bytes)
	}

	if got[1].Bytes != 0 {
		t.Errorf("the middle month's bytes = %d, want 0 (nothing was ever stored in it)", got[1].Bytes)
	}

	if got[2].Bytes <= 0 {
		t.Errorf("this month's bytes = %d, want > 0 (it holds \"current\")", got[2].Bytes)
	}
}

// TestMonthlyStorageExcludesAPrunedScan: the chart is what is still stored,
// not what was ever written (AC5) — a scan retention removed must not go on
// contributing to its month's total.
func TestMonthlyStorageExcludesAPrunedScan(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC)

	older := storedResult("older", thisMonth.Add(-time.Hour))
	if err := s.PutResult(older); err != nil {
		t.Fatal(err)
	}

	newer := storedResult("newer", thisMonth)
	if err := s.PutResult(newer); err != nil {
		t.Fatal(err)
	}

	// Before "older" (both are otherwise inside this same calendar month).
	since := thisMonth.Add(-2 * time.Hour)

	before, err := s.MonthlyStorage(t.Context(), since)
	if err != nil {
		t.Fatalf("MonthlyStorage (before prune): %v", err)
	}

	if len(before) != 1 || before[0].Bytes <= 0 {
		t.Fatalf("before pruning: got %+v, want one month with both scans' bytes", before)
	}

	beforeBytes := before[0].Bytes

	if _, err := s.Prune(t.Context(), TriggerCLI, time.Now(), Retention{MaxPerSeries: 1}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := s.MonthlyStorage(t.Context(), since)
	if err != nil {
		t.Fatalf("MonthlyStorage (after prune): %v", err)
	}

	if len(after) != 1 {
		t.Fatalf("after pruning: len(got) = %d, want 1", len(after))
	}

	if after[0].Bytes >= beforeBytes {
		t.Errorf("after pruning the older scan, this month's bytes = %d, want less than %d (before, both scans)",
			after[0].Bytes, beforeBytes)
	}

	if after[0].Bytes <= 0 {
		t.Errorf("after pruning, this month's bytes = %d, want > 0 (the newer scan MaxPerSeries kept)", after[0].Bytes)
	}
}
