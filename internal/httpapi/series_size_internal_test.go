package httpapi

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// These are white-box tests of Story 5.31's pure arithmetic: the total, the
// median, the rate, and the retention ceiling, each testable without a
// server or a store.

func sizedRow(scanID string, at time.Time, total int64) seriesRow {
	return seriesRow{Summary: store.Summary{
		ScanID: scanID, StartedAt: at,
		DocumentBytes: total, ArtifactBytesRecorded: true,
	}}
}

func unsizedRow(scanID string, at time.Time) seriesRow {
	return seriesRow{Summary: store.Summary{
		ScanID: scanID, StartedAt: at, ArtifactBytesRecorded: false,
	}}
}

// TestComputeSeriesSizeTotalsOnlySizedRows: AC5 — an unrecorded row is
// counted separately, never folded into the byte total as zero, but its
// StartedAt still counts toward the span.
func TestComputeSeriesSizeTotalsOnlySizedRows(t *testing.T) {
	t.Parallel()

	now := time.Now()

	rows := []seriesRow{
		sizedRow("a", now.Add(-2*time.Hour), 1000),
		sizedRow("b", now.Add(-time.Hour), 2000),
		unsizedRow("c", now),
	}

	sz := computeSeriesSize(rows)

	if sz.Listed != 3 {
		t.Errorf("Listed = %d, want 3", sz.Listed)
	}

	if sz.Sized != 2 {
		t.Errorf("Sized = %d, want 2", sz.Sized)
	}

	if sz.Unrecorded != 1 {
		t.Errorf("Unrecorded = %d, want 1", sz.Unrecorded)
	}

	if sz.TotalBytes() != 3000 {
		t.Errorf("TotalBytes() = %d, want 3000 (the unrecorded row excluded)", sz.TotalBytes())
	}

	if !sz.Oldest.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("Oldest = %v, want the earliest row including the unrecorded one", sz.Oldest)
	}

	if !sz.Newest.Equal(now) {
		t.Errorf("Newest = %v, want the unrecorded row, which is the newest", sz.Newest)
	}
}

// TestMedianTotalBytesIsNotSwungByAnOutlier: AC6 — the median, not the mean,
// so one enormous scan does not set a year's budget. Ten scans at 1 MB and
// one at 100 MB: the mean would be dragged past 9 MB, the median stays at
// 1 MB.
func TestMedianTotalBytesIsNotSwungByAnOutlier(t *testing.T) {
	t.Parallel()

	now := time.Now()

	rows := make([]seriesRow, 0, 11)

	for i := range 10 {
		rows = append(rows, sizedRow("normal-"+string(rune('a'+i)), now.Add(time.Duration(i)*time.Hour), 1<<20))
	}

	rows = append(rows, sizedRow("outlier", now.Add(11*time.Hour), 100<<20))

	if got := medianTotalBytes(rows); got != 1<<20 {
		t.Errorf("medianTotalBytes = %d, want %d (the median, unmoved by the outlier)", got, int64(1)<<20)
	}
}

// TestObservedScansPerMonthNeedsEnoughScansAndSpan: AC7 — fewer than three
// scans, or three that do not span a day, yield no rate at all.
func TestObservedScansPerMonthNeedsEnoughScansAndSpan(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tooFew := []seriesRow{sizedRow("a", now, 1), sizedRow("b", now.Add(time.Hour), 1)}
	if got := observedScansPerMonth(tooFew, computeSeriesSize(tooFew)); got != 0 {
		t.Errorf("with 2 scans, observedScansPerMonth = %v, want 0", got)
	}

	tooTight := []seriesRow{
		sizedRow("a", now, 1), sizedRow("b", now.Add(time.Minute), 1), sizedRow("c", now.Add(2*time.Minute), 1),
	}
	if got := observedScansPerMonth(tooTight, computeSeriesSize(tooTight)); got != 0 {
		t.Errorf("with 3 scans a minute apart, observedScansPerMonth = %v, want 0", got)
	}

	// Three scans a day apart: a real, if thin, cadence — about one a day,
	// so roughly 30 a month.
	daily := []seriesRow{
		sizedRow("a", now, 1), sizedRow("b", now.Add(24*time.Hour), 1), sizedRow("c", now.Add(48*time.Hour), 1),
	}

	got := observedScansPerMonth(daily, computeSeriesSize(daily))
	if got < 25 || got > 35 {
		t.Errorf("observedScansPerMonth for a daily cadence = %v, want roughly 30", got)
	}
}

// TestScansPerMonthPrefersTheConfiguredInterval: AC6 — a target with a
// configured interval is scheduled, not observed, even when the listed scans
// would otherwise support an observed rate.
func TestScansPerMonthPrefersTheConfiguredInterval(t *testing.T) {
	t.Parallel()

	now := time.Now()
	rows := []seriesRow{
		sizedRow("a", now, 1), sizedRow("b", now.Add(24*time.Hour), 1), sizedRow("c", now.Add(48*time.Hour), 1),
	}
	sz := computeSeriesSize(rows)

	perMonth, scheduled := scansPerMonth(rows, sz, time.Hour, true)
	if !scheduled {
		t.Error("scheduled = false with a configured interval, want true")
	}

	// Every hour is 24 scans a day, roughly 720 a month.
	if perMonth < 700 || perMonth > 740 {
		t.Errorf("perMonth = %v, want roughly 720 for an hourly schedule", perMonth)
	}

	_, scheduled = scansPerMonth(rows, sz, 0, false)
	if scheduled {
		t.Error("scheduled = true with no configured interval, want the observed fallback")
	}
}

// TestTargetIntervalFallsBackWhenTheTargetIsGone: AC7 — a series whose
// target was renamed or removed reports no interval, rather than one this
// function invents.
func TestTargetIntervalFallsBackWhenTheTargetIsGone(t *testing.T) {
	t.Parallel()

	targets := []config.Resolved{{Name: "site", Interval: time.Hour}}

	if got, ok := targetInterval(targets, "site"); !ok || got != time.Hour {
		t.Errorf("targetInterval(site) = %v, %v, want %v, true", got, ok, time.Hour)
	}

	if _, ok := targetInterval(targets, "renamed-away"); ok {
		t.Error("targetInterval for a target no longer configured reports true, want false")
	}

	cronOnly := []config.Resolved{{Name: "site", Cron: "0 * * * *"}}
	if _, ok := targetInterval(cronOnly, "site"); ok {
		t.Error("targetInterval for a Cron-scheduled target reports true, want the observed-cadence fallback")
	}
}

// TestApplyRetentionCeilingNamesAKeepPolicyAndBoundsTheEstimate: AC8 — an
// active Keep policy produces a ceiling, not a growth figure, and names
// itself.
func TestApplyRetentionCeilingNamesAKeepPolicyAndBoundsTheEstimate(t *testing.T) {
	t.Parallel()

	est := &seriesEstimate{MedianBytes: 1000, ScansPerMonth: 30}

	applyRetentionCeiling(est, store.Retention{Keep: &store.Keep{Daily: 7, Weekly: 4}})

	if !est.Bounded {
		t.Fatal("Bounded = false with an active Keep policy, want true")
	}

	if est.PolicyName != "keep" {
		t.Errorf("PolicyName = %q, want %q", est.PolicyName, "keep")
	}

	if est.CeilingResults != 11 {
		t.Errorf("CeilingResults = %d, want 11 (7 daily + 4 weekly)", est.CeilingResults)
	}

	if est.CeilingBytes != 11000 {
		t.Errorf("CeilingBytes = %d, want 11000", est.CeilingBytes)
	}
}

// TestApplyRetentionCeilingLeavesAnInactivePolicyUnbounded: no policy at
// all means nothing bounds the series, and the estimate must say so rather
// than reporting a spurious ceiling of zero.
func TestApplyRetentionCeilingLeavesAnInactivePolicyUnbounded(t *testing.T) {
	t.Parallel()

	est := &seriesEstimate{MedianBytes: 1000, ScansPerMonth: 30}

	applyRetentionCeiling(est, store.Retention{})

	if est.Bounded {
		t.Error("Bounded = true with no active retention policy, want false")
	}
}

// TestApplyRetentionCeilingTakesTheTighterOfMaxAgeAndMaxPerSeries: a
// MaxPerSeries cap tighter than what MaxAge's rate implies wins, and names
// both settings.
func TestApplyRetentionCeilingTakesTheTighterOfMaxAgeAndMaxPerSeries(t *testing.T) {
	t.Parallel()

	// 30 scans/month is 1/day; 10 days of MaxAge implies about 10 results,
	// which is looser than a MaxPerSeries of 3.
	est := &seriesEstimate{MedianBytes: 500, ScansPerMonth: 30}

	applyRetentionCeiling(est, store.Retention{MaxAge: 10 * 24 * time.Hour, MaxPerSeries: 3})

	if est.CeilingResults != 3 {
		t.Errorf("CeilingResults = %d, want 3 (the tighter of the two settings)", est.CeilingResults)
	}

	if est.PolicyName != "maxAge and maxPerSeries" {
		t.Errorf("PolicyName = %q, want both settings named", est.PolicyName)
	}
}
