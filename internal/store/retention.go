package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Retention bounds how much history is kept.
//
// It carries either the two blunt bounds wsaw has always had, or a thinning
// Keep policy that replaces them (Story 4.10). The two forms are never mixed:
// a bound that quietly cut into a policy designed to keep a year of monthly
// scans would defeat the policy without saying so.
type Retention struct {
	// MaxAge drops results older than this. Zero means no age limit.
	MaxAge time.Duration
	// MaxPerSeries keeps at most this many results per target and mode. Zero
	// means no count limit.
	MaxPerSeries int

	// Keep, when set, replaces MaxAge and MaxPerSeries with a thinning
	// policy in the terms restic's forget uses.
	Keep *Keep
}

// Active reports whether this policy would do anything at all.
func (r Retention) Active() bool {
	if r.Keep != nil {
		return !r.Keep.Empty()
	}

	return r.MaxAge > 0 || r.MaxPerSeries > 0
}

// Keep is a thinning retention policy: keep the best result in each of the
// newest N hours, days, weeks, months and years, and forget the rest.
//
// Every field only ever keeps results. A rule added to the policy can
// therefore never shrink what is stored, which is what makes a retention
// policy safe to reason about and safe to edit.
type Keep struct {
	// Last keeps the newest N results of each series regardless of when they
	// ran; Within keeps everything younger than a duration.
	Last   int
	Within time.Duration

	// Hourly through Yearly keep one result in each of the newest N periods
	// of that length that hold any result at all. An empty period is not
	// counted, so "keep 12 monthly" means twelve months that were scanned.
	Hourly  int
	Daily   int
	Weekly  int
	Monthly int
	Yearly  int

	// Location decides where an hour, a day, a week, a month and a year
	// begin. Nil means the process's local zone.
	Location *time.Location
}

// Empty reports whether the policy would keep nothing at all.
func (k Keep) Empty() bool {
	return k.Last <= 0 && k.Within <= 0 &&
		k.Hourly <= 0 && k.Daily <= 0 && k.Weekly <= 0 && k.Monthly <= 0 && k.Yearly <= 0
}

// bounded reports whether the policy states an explicit age limit. Such a
// limit is allowed to empty a series: an operator who says "keep 30 days"
// about data that can itself be personal has said something deliberate, and
// quietly holding one result of a target nobody scans any more would be the
// opposite of data minimization (Tenet 19).
func (k Keep) bounded() bool { return k.Within > 0 }

func (k Keep) location() *time.Location {
	if k.Location == nil {
		return time.Local
	}

	return k.Location
}

// Rules a decision can name. They are constants because a dry run prints
// them and the tests assert on them.
const (
	ruleLast    = "last"
	ruleWithin  = "within"
	ruleHourly  = "hourly"
	ruleDaily   = "daily"
	ruleWeekly  = "weekly"
	ruleMonthly = "monthly"
	ruleYearly  = "yearly"
	ruleNewest  = "newest"
	ruleAge     = "maxAge"
	ruleCount   = "maxPerSeries"
)

// candidate is one stored result as retention sees it: the index columns and
// nothing else. The document is never read to decide what to prune — a store
// holding a year of scans would have to be loaded in full to answer a
// question three columns already answer (Story 4.10, AC5).
type candidate struct {
	ScanID      string
	StartedAt   time.Time
	Termination model.TerminationReason
}

// Decision records what retention decided about one result, and which rule
// decided it. The rule is what makes a dry run answer "why is this still
// here?" rather than only "it is".
type Decision struct {
	ScanID      string                  `json:"scanId"`
	StartedAt   time.Time               `json:"startedAt"`
	Termination model.TerminationReason `json:"termination"`
	Keep        bool                    `json:"keep"`
	Rule        string                  `json:"rule,omitempty"`
}

// SeriesPlan is what a prune would do to one series, newest result first.
type SeriesPlan struct {
	Series    Series     `json:"series"`
	Decisions []Decision `json:"decisions"`
}

// Kept and Deleted count the two halves of a plan.
func (p SeriesPlan) Kept() int { return len(p.Decisions) - p.Deleted() }

// Deleted counts the results a plan would remove.
func (p SeriesPlan) Deleted() int {
	n := 0

	for _, d := range p.Decisions {
		if !d.Keep {
			n++
		}
	}

	return n
}

// tier ranks how much a result is worth keeping, lowest first: a scan that
// ran to idle, then one that was cut short by a cap or the clock, then one
// that produced no asset list at all.
//
// This is the whole point of the completeness preference: a day whose newest
// scan hit the hard timeout, and which also holds a clean scan, must keep the
// clean one. Keeping the timeout would leave a year-old record that reads
// like a quiet day rather than like a failed observation (Tenet 5).
func tier(t model.TerminationReason) int {
	switch {
	case t.Failed():
		return 2
	case t.Truncated():
		return 1
	default:
		return 0
	}
}

// period identifies the bucket a start time falls in. It is a comparable
// struct rather than a formatted string so grouping costs no allocation and
// cannot be confused by a locale.
type period struct{ a, b, c, d int }

func hourly(t time.Time) period {
	y, m, d := t.Date()

	return period{y, int(m), d, t.Hour()}
}

func daily(t time.Time) period {
	y, m, d := t.Date()

	return period{y, int(m), d, 0}
}

func weekly(t time.Time) period {
	y, w := t.ISOWeek()

	return period{y, w, 0, 0}
}

func monthly(t time.Time) period {
	y, m, _ := t.Date()

	return period{y, int(m), 0, 0}
}

func yearly(t time.Time) period { return period{t.Year(), 0, 0, 0} }

// selectKeep decides what survives, newest first.
//
// It is a pure function of the candidates, the policy and the current time,
// which is what lets the whole of retention be tested with a fixed clock and
// no database (Tenet 13).
func selectKeep(cands []candidate, now time.Time, r Retention) []Decision {
	sorted := make([]candidate, len(cands))
	copy(sorted, cands)

	// The order every other result query uses, so "the newest N" means the
	// same thing here as it does in a listing (Tenet 6).
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].StartedAt.Equal(sorted[j].StartedAt) {
			return sorted[i].StartedAt.After(sorted[j].StartedAt)
		}

		return sorted[i].ScanID > sorted[j].ScanID
	})

	kept := make(map[int]string, len(sorted))

	if r.Keep != nil {
		applyKeep(sorted, now, *r.Keep, kept)
	} else {
		applyBounds(sorted, now, r, kept)
	}

	out := make([]Decision, len(sorted))

	for i, c := range sorted {
		rule, ok := kept[i]
		out[i] = Decision{
			ScanID:      c.ScanID,
			StartedAt:   c.StartedAt,
			Termination: c.Termination,
			Keep:        ok,
			Rule:        rule,
		}
	}

	return out
}

// applyBounds is the older policy: an age limit and a count limit, both of
// which delete. It is expressed here rather than in SQL so that both forms
// share one selection, one delete path and one dry run.
func applyBounds(sorted []candidate, now time.Time, r Retention, kept map[int]string) {
	cutoff := now.Add(-r.MaxAge)

	// Both bounds have to allow a result for it to survive, so the rule that
	// kept it is the pair, not one of them.
	rule := ruleAge

	switch {
	case r.MaxAge > 0 && r.MaxPerSeries > 0:
		rule = ruleAge + "+" + ruleCount
	case r.MaxPerSeries > 0:
		rule = ruleCount
	}

	for i, c := range sorted {
		if r.MaxAge > 0 && c.StartedAt.Before(cutoff) {
			continue
		}

		if r.MaxPerSeries > 0 && i >= r.MaxPerSeries {
			continue
		}

		kept[i] = rule
	}
}

func applyKeep(sorted []candidate, now time.Time, k Keep, kept map[int]string) {
	keep := func(i int, rule string) {
		if _, ok := kept[i]; !ok {
			kept[i] = rule
		}
	}

	for i := range sorted {
		if i < k.Last {
			keep(i, ruleLast)
		}
	}

	if k.Within > 0 {
		cutoff := now.Add(-k.Within)

		for i, c := range sorted {
			if !c.StartedAt.Before(cutoff) {
				keep(i, ruleWithin)
			}
		}
	}

	loc := k.location()

	buckets := []struct {
		n    int
		rule string
		key  func(time.Time) period
	}{
		{k.Hourly, ruleHourly, hourly},
		{k.Daily, ruleDaily, daily},
		{k.Weekly, ruleWeekly, weekly},
		{k.Monthly, ruleMonthly, monthly},
		{k.Yearly, ruleYearly, yearly},
	}

	for _, b := range buckets {
		for _, i := range bucketKeepers(sorted, b.n, loc, b.key) {
			keep(i, b.rule)
		}
	}

	// A policy made only of counts must never empty a series: with no result
	// at all, a target reads as one that was never scanned rather than one
	// whose history has expired (Tenet 5). An explicit age limit is allowed
	// to empty it, because that is what an operator asked for.
	if len(kept) == 0 && len(sorted) > 0 && !k.bounded() {
		keep(0, ruleNewest)
	}
}

// bucketKeepers returns the index of the result to keep in each of the newest
// n periods that hold a result.
func bucketKeepers(sorted []candidate, n int, loc *time.Location, key func(time.Time) period) []int {
	if n <= 0 {
		return nil
	}

	var (
		order   []period
		best    = make(map[period]int)
		seen    = make(map[period]bool)
		keepers []int
	)

	for i, c := range sorted {
		p := key(c.StartedAt.In(loc))

		if !seen[p] {
			if len(order) == n {
				// The candidates are newest first, so the first n distinct
				// periods are the newest n. Anything after them belongs to a
				// period this rule does not reach.
				break
			}

			seen[p] = true
			order = append(order, p)
			best[p] = i

			continue
		}

		// Within a period, a better-terminated scan beats a newer one; the
		// candidates are already newest first, so an equal tier never wins.
		if tier(sorted[i].Termination) < tier(sorted[best[p]].Termination) {
			best[p] = i
		}
	}

	for _, p := range order {
		keepers = append(keepers, best[p])
	}

	return keepers
}

// PruneStats reports what a prune removed, so retention is observable rather
// than silent.
type PruneStats struct {
	ResultsDeleted   int
	ResultsKept      int
	SeriesPruned     int
	ArtifactsDeleted int
	BytesFreed       int64

	// OldestKept is the start time of the oldest result still stored, which
	// is the figure an operator checks a policy against.
	OldestKept time.Time
}

// pruneBatch bounds how many results one delete statement names. A first
// prune after a policy change can condemn tens of thousands of rows, and a
// statement with a placeholder per row is one no database will accept.
const pruneBatch = 200

// PrunePlan reports what a prune would do, without deleting anything.
//
// Deleting evidence is irreversible and a policy change is retroactive, so
// the plan exists to be read before the first run applies it (Story 4.10,
// AC10). It reports every series, including those it would not touch.
func (s *Store) PrunePlan(now time.Time, r Retention) ([]SeriesPlan, error) {
	if !r.Active() {
		return nil, nil
	}

	series, err := s.Series()
	if err != nil {
		return nil, err
	}

	plans := make([]SeriesPlan, 0, len(series))

	for _, se := range series {
		cands, err := s.candidates(se)
		if err != nil {
			return nil, err
		}

		plans = append(plans, SeriesPlan{Series: se, Decisions: selectKeep(cands, now, r)})
	}

	return plans, nil
}

// Prune enforces retention.
//
// Baselines are never pruned: they hold their own copy of the approved
// result, so history can expire without invalidating the definition of
// "expected".
func (s *Store) Prune(now time.Time, r Retention) (PruneStats, error) {
	var stats PruneStats

	if !r.Active() {
		return stats, nil
	}

	plans, err := s.PrunePlan(now, r)
	if err != nil {
		return stats, err
	}

	for _, plan := range plans {
		doomed := make([]string, 0, len(plan.Decisions))

		for _, d := range plan.Decisions {
			if d.Keep {
				stats.ResultsKept++

				if stats.OldestKept.IsZero() || d.StartedAt.Before(stats.OldestKept) {
					stats.OldestKept = d.StartedAt
				}

				continue
			}

			doomed = append(doomed, d.ScanID)
		}

		if len(doomed) == 0 {
			continue
		}

		deleted, err := s.deleteResults(plan.Series, doomed)

		stats.ResultsDeleted += deleted

		if deleted > 0 {
			stats.SeriesPruned++
		}

		if err != nil {
			return stats, err
		}
	}

	return stats, nil
}

// candidates reads the index columns of one series, newest first. The
// document column is deliberately not in the select list.
func (s *Store) candidates(se Series) ([]candidate, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	const q = `
		select scan_id, started_at, termination from results
		where target = ? and consent_mode = ?
		order by started_at desc, scan_id desc`

	var out []candidate

	err := s.retry(ctx, "listing results for retention", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(q), se.Target, string(se.Mode))
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				c       candidate
				started int64
				term    string
			)

			if err := rows.Scan(&c.ScanID, &started, &term); err != nil {
				return err
			}

			c.StartedAt = time.Unix(0, started)
			c.Termination = model.TerminationReason(term)

			out = append(out, c)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing results for %s/%s: %w", se.Target, se.Mode, err)
	}

	return out, nil
}

// deleteResultsQuery deletes n named results of one series.
//
// It is the same statement in every dialect and needs no hook of its own: it
// names results by the identity the schema declares rather than by a physical
// row identifier — SQLite's rowid, PostgreSQL's ctid and MySQL's nothing are
// not the same concept — and it never selects from the table it deletes from,
// which is what MySQL refuses (error 1093).
func deleteResultsQuery(n int) string {
	return `delete from results where target = ? and consent_mode = ? and scan_id in (` +
		strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)`
}

// deleteResults removes named results of one series, in bounded batches. It
// returns how many rows went away even when a later batch fails: a partial
// prune is a fact the caller has to report rather than round down to zero.
func (s *Store) deleteResults(se Series, scanIDs []string) (int, error) {
	total := 0

	for start := 0; start < len(scanIDs); start += pruneBatch {
		end := min(start+pruneBatch, len(scanIDs))

		batch := scanIDs[start:end]

		n, err := s.deleteBatch(se, batch)

		total += n

		if err != nil {
			return total, err
		}
	}

	return total, nil
}

func (s *Store) deleteBatch(se Series, scanIDs []string) (int, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	args := make([]any, 0, len(scanIDs)+2)
	args = append(args, se.Target, string(se.Mode))

	for _, id := range scanIDs {
		args = append(args, id)
	}

	q := deleteResultsQuery(len(scanIDs))

	var deleted int

	err := s.retry(ctx, "pruning results", func(ctx context.Context) error {
		res, err := s.db.ExecContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		if n, err := res.RowsAffected(); err == nil {
			deleted = int(n)
		}

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("pruning results for %s/%s: %w", se.Target, se.Mode, err)
	}

	return deleted, nil
}
