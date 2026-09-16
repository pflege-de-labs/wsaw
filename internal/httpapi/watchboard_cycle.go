package httpapi

import (
	"strconv"
	"time"
)

// What a tile says about time.
//
// Two facts, both already published by the daemon: how old the scan the tile
// describes is, and how long until the next one. The bar under them is a
// third reading of the same two facts, not a third fact: its total is their
// sum (age plus time-to-next), and it fills for the age's share of that
// total. Where the schedule says nothing, the tile says "not scheduled"
// rather than guessing, and draws no bar at all.

// AgeLabel is the left half of the tile's time line: how old the shown scan
// is. A running scan replaces it, because "3h old" next to a scan in flight
// describes a result that is already being superseded.
func (r modeRow) AgeLabel() string {
	if len(r.Series.Running) > 0 {
		return "scanning now"
	}

	start := r.scanStart()
	if start.IsZero() {
		return "never scanned"
	}

	return shortDur(time.Since(start)) + " old"
}

// NextLabel is the right half of the time line: how long until the next
// scheduled scan.
func (r modeRow) NextLabel() string {
	if len(r.Series.Running) > 0 {
		return "in flight"
	}

	if r.NextRun.IsZero() {
		return "not scheduled"
	}

	if time.Until(r.NextRun) <= 0 {
		return "next scan overdue"
	}

	return "next in " + shortDur(time.Until(r.NextRun))
}

// Overdue reports a scan the schedule expected by now. It is a statement
// about the daemon, not about the site, so it is styled as a warning and not
// as a finding.
func (r modeRow) Overdue() bool {
	return len(r.Series.Running) == 0 && !r.NextRun.IsZero() && time.Until(r.NextRun) <= 0
}

// CyclePercent is how far through the current scan interval the board is, as
// a whole percentage. A running or overdue cycle reads full: in both cases
// the wait this bar measures is over.
func (r modeRow) CyclePercent() int {
	if len(r.Series.Running) > 0 || r.Overdue() {
		return 100
	}

	start, interval := r.scanStart(), r.interval()
	if start.IsZero() || interval <= 0 {
		return 0
	}

	pct := int(float64(time.Since(start)) / float64(interval) * 100)

	switch {
	case pct < 0:
		return 0
	case pct > 100:
		return 100
	default:
		return pct
	}
}

// CycleClass marks the bar's three states. The words above it say the same
// thing — the bar is a second reading of a fact already written out, never
// the only carrier of it.
func (r modeRow) CycleClass() string {
	switch {
	case len(r.Series.Running) > 0:
		return "watch-cycle-fill is-running"
	case r.Overdue():
		return "watch-cycle-fill is-overdue"
	default:
		return "watch-cycle-fill"
	}
}

// Scheduled reports whether there is a cadence to draw at all. An unscheduled
// series gets the time line without the bar: an empty track would read as
// "due any moment now" for a series nothing is going to scan.
func (r modeRow) Scheduled() bool {
	return !r.NextRun.IsZero()
}

// interval is the bar's total: the shown scan's age plus the time to the
// next scheduled one, i.e. NextRun minus the same start AgeLabel measures
// from. Using scanStart rather than LastRun keeps the bar's total equal to
// the sum of the two numbers written above it — age and next-in — even when
// a failed scan has moved LastRun without producing a result.
func (r modeRow) interval() time.Duration {
	if r.NextRun.IsZero() {
		return 0
	}

	start := r.scanStart()
	if start.IsZero() {
		return 0
	}

	return r.NextRun.Sub(start)
}

// scanStart is when the scan the tile describes began, falling back to the
// schedule's last run when there is no stored result to point at.
func (r modeRow) scanStart() time.Time {
	if r.Series.LastScan != nil {
		return r.Series.LastScan.StartedAt
	}

	return r.LastRun
}

// shortDur is a duration at tile width: one unit under an hour, two above,
// never a decimal. "18m", "1h 12m", "45s".
func shortDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}

	switch {
	case d < time.Minute:
		s := int(d.Seconds())
		if s < 1 {
			s = 1
		}

		return strconv.Itoa(s) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Round(time.Minute).Minutes())) + "m"
	default:
		d = d.Round(time.Minute)
		h, m := int(d.Hours()), int(d.Minutes())%60

		if m == 0 {
			return strconv.Itoa(h) + "h"
		}

		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
}

// attachSchedule gives every displayed row the schedule entry for its series,
// so a tile can state its own cadence.
//
// It must run before shapeWatchboard: the groups hold copies of these rows,
// and a value attached afterwards would reach the page for the Schedule table
// and for nothing else.
func (d *dashboardData) attachSchedule() {
	if len(d.Jobs) == 0 {
		return
	}

	type key struct {
		target string
		mode   string
	}

	next := make(map[key]scheduleRow, len(d.Jobs))
	for _, j := range d.Jobs {
		next[key{j.Target, string(j.Mode)}] = j
	}

	for i := range d.Targets {
		for k := range d.Targets[i].Rows {
			row := &d.Targets[i].Rows[k]

			if j, ok := next[key{d.Targets[i].Name, string(row.Series.Mode)}]; ok {
				row.NextRun, row.LastRun = j.NextRun, j.LastRun
			}
		}
	}
}
