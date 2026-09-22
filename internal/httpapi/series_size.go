package httpapi

import (
	"sort"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// This file is Story 5.31's arithmetic: what the history page's total and
// estimate say, computed entirely from the rows the table already lists (or,
// for the estimate's rate and ceiling, the target's schedule and the store's
// retention policy) — never a second read of the store or the bucket.

// minObservedScansForRate is AC7's floor: fewer listed scans than this, and a
// cadence computed from them is measuring the scheduler's own jitter, not a
// rate. Below it the estimate is withheld rather than guessed.
const minObservedScansForRate = 3

// minSpanForRate is AC7's other floor: three scans a minute apart span "enough
// time to mean anything" not at all. An invented denominator is worse than a
// blank (Tenet 5).
const minSpanForRate = 24 * time.Hour

const daysPerMonth = 30

// seriesSize is what the series total beneath the history table states: what
// the listed, sizeable rows hold together, how many of the listed rows could
// not be sized, and the span every listed row covers.
type seriesSize struct {
	// Listed is every row the table shows.
	Listed int
	// Sized is the rows among them whose ArtifactBytesRecorded is true —
	// the ones DocumentBytes and ArtifactBytes below actually total.
	Sized      int
	Unrecorded int

	DocumentBytes int64
	ArtifactBytes int64

	// Oldest and Newest span every listed row, sized or not: the span is a
	// statement about time, which every row's StartedAt answers regardless
	// of whether its evidence was sized (AC5).
	Oldest, Newest time.Time

	// Estimate is nil when there is nothing to estimate from: no sizeable
	// row (AC6) or no usable rate (AC7).
	Estimate *seriesEstimate
}

// TotalBytes is what the sizeable listed rows hold together.
func (s seriesSize) TotalBytes() int64 { return s.DocumentBytes + s.ArtifactBytes }

// seriesEstimate is the month/year projection beneath the series total.
type seriesEstimate struct {
	// MedianBytes is the median stored size (document plus artifacts) of the
	// sizeable rows — the median, never the mean, so one outlier scan
	// cannot set a year's budget (AC6).
	MedianBytes int64

	// ScansPerMonth is the cadence the estimate multiplies MedianBytes by.
	ScansPerMonth float64
	// Scheduled is true when ScansPerMonth came from the target's own
	// configured interval, false when it was observed from the scans
	// listed instead (AC6, AC7).
	Scheduled bool

	MonthBytes, YearBytes int64

	// Bounded is true when an active retention policy caps this series
	// rather than letting it grow without bound (AC8): the estimate then
	// shows CeilingBytes, the steady-state ceiling, instead of MonthBytes
	// and YearBytes, and names PolicyName. Unbounded (Bounded false) is the
	// case where the growth figure is shown, because it is a warning rather
	// than a budget.
	Bounded        bool
	PolicyName     string
	CeilingResults int
	CeilingBytes   int64
}

// computeSeriesSize totals a history page's listed rows (AC5).
func computeSeriesSize(results []seriesRow) seriesSize {
	var sz seriesSize

	for _, r := range results {
		sz.Listed++

		if sz.Oldest.IsZero() || r.StartedAt.Before(sz.Oldest) {
			sz.Oldest = r.StartedAt
		}

		if r.StartedAt.After(sz.Newest) {
			sz.Newest = r.StartedAt
		}

		if !r.ArtifactBytesRecorded {
			sz.Unrecorded++

			continue
		}

		sz.Sized++
		sz.DocumentBytes += r.DocumentBytes
		sz.ArtifactBytes += r.ArtifactBytes
	}

	return sz
}

// computeSeriesEstimate builds the month/year projection, or reports why it
// cannot by returning nil: nothing sizeable to measure (AC6), or nothing
// resembling a rate to multiply it by (AC7).
//
// interval and hasInterval come from a lookup by name against the current
// configuration: a series whose target was renamed or removed out from under
// it, or one scheduled by Cron rather than an interval, is one this function
// has no schedule for — exactly the case AC7's observed-cadence fallback
// exists to answer instead.
func computeSeriesEstimate(results []seriesRow, sz seriesSize, interval time.Duration, hasInterval bool) *seriesEstimate {
	median := medianTotalBytes(results)
	if median <= 0 {
		return nil
	}

	perMonth, scheduled := scansPerMonth(results, sz, interval, hasInterval)
	if perMonth <= 0 {
		return nil
	}

	return &seriesEstimate{
		MedianBytes:   median,
		ScansPerMonth: perMonth,
		Scheduled:     scheduled,
		MonthBytes:    int64(float64(median) * perMonth),
		YearBytes:     int64(float64(median) * perMonth * 12),
	}
}

// medianTotalBytes is AC6's median over the sizeable rows' own totals
// (document plus artifacts).
func medianTotalBytes(results []seriesRow) int64 {
	sizes := make([]int64, 0, len(results))

	for _, r := range results {
		if !r.ArtifactBytesRecorded {
			continue
		}

		sizes = append(sizes, r.DocumentBytes+r.ArtifactBytes)
	}

	if len(sizes) == 0 {
		return 0
	}

	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })

	mid := len(sizes) / 2
	if len(sizes)%2 == 1 {
		return sizes[mid]
	}

	return (sizes[mid-1] + sizes[mid]) / 2
}

// scansPerMonth is AC6's rate, from the schedule when there is a usable one
// and from the scans listed otherwise (AC7). A Cron schedule is not
// converted to a rate here (no cron library is in this dependency-light
// binary's path for it — Tenet 19), so a Cron-scheduled series falls back to
// the observed cadence exactly as an unscheduled one does.
func scansPerMonth(results []seriesRow, sz seriesSize, interval time.Duration, hasInterval bool) (perMonth float64, scheduled bool) {
	if hasInterval && interval > 0 {
		return (daysPerMonth * 24 * time.Hour).Seconds() / interval.Seconds(), true
	}

	return observedScansPerMonth(results, sz), false
}

// observedScansPerMonth is AC7's fallback: the cadence the listed scans
// themselves show, or 0 when there are too few of them or they do not span
// enough time to mean anything.
func observedScansPerMonth(results []seriesRow, sz seriesSize) float64 {
	if len(results) < minObservedScansForRate {
		return 0
	}

	span := sz.Newest.Sub(sz.Oldest)
	if span < minSpanForRate {
		return 0
	}

	scansPerSecond := float64(len(results)-1) / span.Seconds()

	return scansPerSecond * (daysPerMonth * 24 * time.Hour).Seconds()
}

// applyRetentionCeiling is AC8: where an active policy bounds this series,
// the estimate shows the ceiling it converges on instead of unbounded
// growth, and names the policy producing it.
func applyRetentionCeiling(est *seriesEstimate, r store.Retention) {
	if est == nil || !r.Active() {
		return
	}

	ratePerDay := est.ScansPerMonth / daysPerMonth

	if r.Keep != nil {
		est.Bounded = true
		est.PolicyName = "keep"
		est.CeilingResults = keepCeiling(*r.Keep, ratePerDay)
		est.CeilingBytes = int64(est.CeilingResults) * est.MedianBytes

		return
	}

	byAge, hasAge := -1, r.MaxAge > 0
	if hasAge {
		byAge = int(ratePerDay * r.MaxAge.Hours() / 24)
	}

	byCount, hasCount := -1, r.MaxPerSeries > 0
	if hasCount {
		byCount = r.MaxPerSeries
	}

	var (
		ceiling int
		name    string
	)

	switch {
	case hasAge && hasCount:
		// Both settings are named regardless of which one actually binds:
		// an operator reading "maxPerSeries" alone would have no reason to
		// suspect maxAge is in force too, and the tighter figure is what
		// prune() itself would keep — the smaller of what each rule allows.
		name = "maxAge and maxPerSeries"
		ceiling = min(byAge, byCount)
	case hasAge:
		ceiling, name = byAge, "maxAge"
	default:
		ceiling, name = byCount, "maxPerSeries"
	}

	est.Bounded = true
	est.PolicyName = name
	est.CeilingResults = ceiling
	est.CeilingBytes = int64(ceiling) * est.MedianBytes
}

// keepCeiling is the steady-state count a Keep policy converges on. Each
// tier keeps at most one result per populated period among its newest N
// periods, so summing the configured tiers is the same upper bound restic's
// own documentation gives for "forget" — an upper bound, not a simulation of
// which periods a real timeline would populate, which depends on the scan
// cadence itself and is exactly what would make this exact rather than an
// estimate. Within is the one field this bound cannot state as a period
// count on its own, so it is converted through the observed or scheduled
// rate the same way MaxAge is.
func keepCeiling(k store.Keep, ratePerDay float64) int {
	n := k.Last + k.Hourly + k.Daily + k.Weekly + k.Monthly + k.Yearly

	if k.Within > 0 {
		n += int(ratePerDay * k.Within.Hours() / 24)
	}

	return n
}

// targetInterval looks up a series' target by name and reports its
// configured scan interval, when it has one. A target the configuration no
// longer names, or one scheduled by Cron rather than an interval, reports
// false — the case AC7's observed-cadence fallback exists for.
func targetInterval(targets []config.Resolved, name string) (time.Duration, bool) {
	for _, t := range targets {
		if t.Name == name {
			return t.Interval, t.Interval > 0
		}
	}

	return 0, false
}
