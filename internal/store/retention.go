package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is retention: what wsaw stops keeping, and what it removes when it
// stops keeping it (Story 8.5).
//
// Pruning used to delete rows and leave every artifact behind. That was
// survivable while the bucket held opt-in screenshots and bodies; it is not
// now that it holds every scan's document as well, and nothing else in wsaw
// removes anything from it. So a prune deletes the artifacts the results it
// removed alone referenced, keeps the ones a surviving result or baseline
// still names (AC1), and a sweep collects what earlier runs and interrupted
// writes left behind (AC3).
//
// Three rules run through all of it. Deletion is decided by reference and
// never by the age of a key, because artifacts are content-addressed and two
// scans that captured identical bytes share one object. A deletion that fails
// is counted and left for next time rather than allowed to abort retention
// (AC4): the reference rows are the work list, so a key that could not be
// deleted is met again rather than forgotten. And nothing here infers absence
// from a gap in what wsaw knows (Tenet 5) — a bucket that has gone away, an
// index that holds nothing, a result whose references could not be derived and
// an object a running scan has taken are each a reason to keep rather than a
// licence to delete, because a deletion cannot be taken back and a refusal
// costs one more run.

// unreferencedArtifactGrace is how long an artifact that nothing references is
// left alone before a sweep will collect it (AC3).
//
// It exists because a sweep looks at a bucket from outside and cannot see
// intent. An object written moments ago with no reference to it is far more
// likely to be a scan in progress than garbage: artifacts are written to the
// bucket first and referenced from the database second, deliberately (Story
// 8.2, AC4), so every screenshot and every body spends the rest of its scan
// unreferenced, and the document spends the instant between its upload and the
// row's commit that way. Collecting one of those would destroy a scan's
// evidence while the scan was still running.
//
// A day is far longer than that window — a scan is bounded in minutes, retries
// included — and the margin is deliberate. What the excess costs is that
// genuine garbage lives one more day before it is collected, which is a
// rounding error on a bucket's bill. What too short a grace period costs is
// deleted evidence, which is not recoverable at any price. A day also absorbs
// the two things that would otherwise have to be reasoned about: a provider
// whose clock differs from wsaw's, since one of the two timestamps compared
// against it is the bucket's own, and a wsaw that was restarted or paused
// mid-scan.
//
// It is measured against two timestamps, not one, because neither answers
// alone. The object's own write time catches an interrupted write — an object
// that exists and is young. The claim wsaw records when it takes an artifact
// (see claims.go) catches the case the write time cannot: an unchanged asset
// content-addresses to a key that is already there, so nothing is written and
// the object keeps the date of the scan that first stored those bytes, while a
// scan running now is about to reference it.
const unreferencedArtifactGrace = 24 * time.Hour

// pruneBatch bounds how many results one delete statement names. A first
// prune after a policy change can condemn tens of thousands of rows, and a
// statement with a placeholder per row is one no database will accept.
const pruneBatch = 200

// artifactRefPageSize is how many artifacts one page of the reference check
// covers. Retention walks its work rather than loading it, so a store with a
// hundred thousand results costs no more memory than one with ten.
const artifactRefPageSize = 256

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

// PrunedResult names one stored result a prune removed, or would remove.
type PrunedResult struct {
	Target      string            `json:"target"`
	ConsentMode model.ConsentMode `json:"consentMode"`
	ScanID      string            `json:"scanId"`
	StartedAt   time.Time         `json:"startedAt"`
}

// PruneStats reports what a prune removed, so retention is observable rather
// than silent. A plan fills the same fields with what a prune would remove.
type PruneStats struct {
	// ResultsDeleted counts rows dropped from the history, ResultsKept the
	// ones the policy decided to keep, and SeriesPruned how many series lost
	// at least one result (Story 4.10).
	ResultsDeleted int `json:"resultsDeleted"`
	ResultsKept    int `json:"resultsKept"`
	SeriesPruned   int `json:"seriesPruned"`

	// OldestKept is the start time of the oldest result still stored, which
	// is the figure an operator checks a policy against.
	OldestKept time.Time `json:"oldestKept,omitzero"`

	// ArtifactsDeleted and BytesFreed count what left the bucket: documents,
	// screenshots and stored bodies that no surviving result or baseline
	// referenced any more (AC5).
	ArtifactsDeleted int   `json:"artifactsDeleted"`
	BytesFreed       int64 `json:"bytesFreed"`

	// ArtifactsFailed counts artifacts the bucket would not delete. Their
	// references are kept, so the next sweep meets the same keys rather than
	// leaking them (AC4).
	ArtifactsFailed int `json:"artifactsFailed"`

	// ArtifactsProtected counts artifacts left alone on purpose: too recently
	// written to be sure they are garbage, or of a kind that cannot be
	// declared unreferenced while some result's references are unknown.
	ArtifactsProtected int `json:"artifactsProtected"`

	// UnknownReferences counts stored results whose artifacts this store could
	// not work out, which is what protects those kinds. It is normally zero;
	// a non-zero value means some result's document is missing from the bucket
	// or no longer decodes.
	UnknownReferences int `json:"unknownReferences"`

	// Results and Artifacts name what would be removed, and Plans says which
	// rule decided each one. They are filled by a plan, which exists so that
	// an operator can see the consequence of a retention setting before it is
	// applied and cannot be undone (Story 8.5, AC6; Story 4.10, AC10). A prune
	// that is actually removing things leaves them empty, because the list
	// would be as long as the work and nothing reads it.
	Results   []PrunedResult `json:"results,omitempty"`
	Artifacts []string       `json:"artifacts,omitempty"`
	Plans     []SeriesPlan   `json:"plans,omitempty"`
}

// Prune enforces retention: it drops the results that fall outside it and
// deletes the artifacts they alone referenced.
//
// Baselines are never pruned, and neither is anything a baseline names: a
// baseline holds its own copy of the approved result, so history can expire
// without invalidating the definition of "expected", and deleting the evidence
// that copy points at would undo that (AC1).
//
// It takes a context because it is no longer only a statement or two. Deleting
// what a prune orphaned is one bucket call per artifact, which against object
// storage is a network round trip each, and a daemon shutting down has to be
// able to stop it rather than wait for it.
//
// It needs the bucket to be there, and says so rather than working around it:
// a prune against a bucket that has gone away would delete rows and report
// every artifact as already collected.
func (s *SQL) Prune(ctx context.Context, trigger string, now time.Time, r Retention) (PruneStats, error) {
	return s.prune(ctx, trigger, now, r, false)
}

// PlanPrune reports what Prune would remove, and removes nothing (AC6).
//
// It works by doing the prune inside a transaction and rolling it back. That
// is deliberate rather than clever: the alternative is a second definition of
// "what this would orphan", and a dry run that answers a slightly different
// question from the prune it is describing is worse than no dry run. Nothing
// is written and no artifact is touched — the bucket is only asked how large
// the objects are, and only after the transaction has been rolled back.
func (s *SQL) PlanPrune(ctx context.Context, now time.Time, r Retention) (PruneStats, error) {
	return s.prune(ctx, "", now, r, true)
}

// prune carries out Prune or PlanPrune. A real run (plan is false) records
// its own receipt as the last thing it does, whether or not it returns an
// error, from whatever stats it accumulated before failing (Story 4.11, AC3
// and AC4) — never for a plan, which changed nothing and already printed its
// own answer (Story 8.5, AC6).
func (s *SQL) prune(ctx context.Context, trigger string, now time.Time, r Retention, plan bool) (stats PruneStats, err error) {
	if !plan {
		started := time.Now()

		defer func() {
			if recErr := s.RecordPruneRun(ctx, trigger, started, stats, err); recErr != nil {
				s.log.Warn("a prune run could not be recorded", "error", recErr)
			}
		}()
	}

	if !r.Active() {
		return stats, nil
	}

	// Established before a single row is deleted. The collection half reads
	// "this key is already gone" as "somebody else collected it", which is the
	// right answer for one key and the wrong one for a bucket that has gone
	// away: an unmounted volume answers NotFound for every artifact, and a
	// prune would then report thousands of deletions, forget the reference
	// rows that were the work list, and leave the objects behind for ever once
	// the volume came back. A failed observation must not read as a clean
	// result (Tenet 5), so the bucket is asked whether it is there.
	if err := s.bucket.reachable(ctx); err != nil {
		return stats, fmt.Errorf("pruning needs the artifact bucket: %w", err)
	}

	unknown, err := s.resultsWithUnknownRefs(ctx)
	if err != nil {
		return stats, err
	}

	stats.UnknownReferences = unknown

	// Decided before anything is deleted, and decided once: the plan is what a
	// dry run prints and what a prune carries out, so the two cannot describe
	// different sets (Story 8.5, AC6; Story 4.10, AC10).
	plans, err := s.prunePlans(ctx, now, r)
	if err != nil {
		return stats, err
	}

	accountForKept(plans, &stats)

	if plan {
		return s.planPrune(ctx, plans, now, stats)
	}

	if err := s.retry(ctx, "pruning results", func(ctx context.Context) error {
		// Assigned inside the retry rather than accumulated across attempts: a
		// replayed transaction starts again, and a count that added up every
		// attempt would report rows that were only deleted once.
		stats.ResultsDeleted, stats.SeriesPruned = 0, 0

		deleted, series, err := s.deleteCondemnedTx(ctx, plans)

		stats.ResultsDeleted, stats.SeriesPruned = deleted, series

		return err
	}); err != nil {
		return stats, err
	}

	// Outside the transaction, on the same argument that keeps a bucket write
	// out of one (Story 8.2, AC7): a delete against object storage is a network
	// round trip, and holding a database lock across it would make retention a
	// source of contention. The rows the transaction left behind are what say
	// which artifacts to consider, so nothing is lost by committing first.
	if err := s.collectDangling(ctx, s.db, now, &stats); err != nil {
		return stats, err
	}

	// Last, and only once the collection above has had its answer: a claim
	// that is too old to protect anything is a row nothing will read again
	// (see claims.go). Its failure is not the prune's — the rows and the
	// artifacts are already gone — so it is logged rather than returned.
	if err := s.forgetStaleClaims(ctx, now.Add(-unreferencedArtifactGrace)); err != nil {
		s.log.Warn("the artifact claims of scans that never finished could not be cleared",
			"error", err)
	}

	return stats, nil
}

// planPrune answers what a prune would do, from inside a transaction it throws
// away.
func (s *SQL) planPrune(ctx context.Context, plans []SeriesPlan, now time.Time, stats PruneStats) (PruneStats, error) {
	// The transaction takes the caller's context, not a bounded one: it spans
	// as many statements as the plan has pages, the way the document migration
	// spans as many as it has rows. Each statement inside it still gets the
	// deadline every store operation gets, so nothing waits for ever — but a
	// deadline on the whole plan would abort a large one halfway through.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("planning a prune: %w", err)
	}

	// Rolled back on every path, including the one where everything worked:
	// this transaction exists to be discarded.
	defer func() { _ = tx.Rollback() }()

	stats.Plans = plans
	stats.Results = condemnedResults(plans)

	if err := boundedStep(ctx, func(ctx context.Context) error {
		stats.ResultsDeleted, stats.SeriesPruned, err = s.deleteCondemned(ctx, tx, plans)

		return err
	}); err != nil {
		return stats, err
	}

	if stats.Artifacts, stats.ArtifactsProtected, err = s.orphanedRefs(ctx, tx, now, stats.UnknownReferences); err != nil {
		return stats, err
	}

	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return stats, fmt.Errorf("discarding a prune plan: %w", err)
	}

	// Measured only now, with the transaction gone: asking the bucket how big
	// an object is means a request per artifact against object storage, and
	// doing that with a write transaction open would hold the database's locks
	// for the length of a network conversation.
	if stats.ArtifactsDeleted, stats.BytesFreed, err = s.measureArtifacts(ctx, stats.Artifacts); err != nil {
		return stats, err
	}

	return stats, nil
}

// boundedStep runs one statement of a longer operation under the deadline
// every store operation gets, inside a transaction that outlives it.
//
// It exists because a plan holds one transaction across several statements and
// several pages: bounding the whole thing would cut a large plan short, and
// bounding nothing would let a database that has stopped answering hang the
// command. This is the same split the document migration makes.
func boundedStep(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return fn(ctx)
}

// deleteCondemnedTx deletes what the plans condemned, in one transaction so
// that a series is never left half pruned.
func (s *SQL) deleteCondemnedTx(ctx context.Context, plans []SeriesPlan) (deleted, series int, err error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("pruning results: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	deleted, series, err = s.deleteCondemned(ctx, tx, plans)
	if err != nil {
		return deleted, series, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("committing prune: %w", err)
	}

	return deleted, series, nil
}

// prunePlans works out what retention would do to every series, and touches
// nothing.
//
// The selection is Story 4.10's, unchanged: the index columns of a series go
// to selectKeep, which decides each result against the policy and says which
// rule decided it. What Story 8.5 adds is everything after the decision —
// which artifacts those results alone referenced, and what becomes of them.
//
// It reads every series rather than only the ones a bound would touch, because
// a plan is read to answer "what would this policy do to my history", and a
// series it left out would read as a series it would not change.
func (s *SQL) prunePlans(ctx context.Context, now time.Time, r Retention) ([]SeriesPlan, error) {
	series, err := s.Series()
	if err != nil {
		return nil, err
	}

	plans := make([]SeriesPlan, 0, len(series))

	for _, se := range series {
		cands, err := s.candidates(ctx, se)
		if err != nil {
			return nil, err
		}

		plans = append(plans, SeriesPlan{Series: se, Decisions: selectKeep(cands, now, r)})
	}

	return plans, nil
}

// accountForKept records what the policy decided to keep, which is the half of
// a prune's report that says whether the policy is the one the operator meant.
//
// OldestKept is the figure a policy is actually checked against — "keep a year
// of monthly scans" is right or wrong depending on what the oldest surviving
// scan is dated — so it is taken across every series rather than per series.
func accountForKept(plans []SeriesPlan, stats *PruneStats) {
	for _, plan := range plans {
		for _, d := range plan.Decisions {
			if !d.Keep {
				continue
			}

			stats.ResultsKept++

			if stats.OldestKept.IsZero() || d.StartedAt.Before(stats.OldestKept) {
				stats.OldestKept = d.StartedAt
			}
		}
	}
}

// condemnedResults names the results a plan would remove, in the order the
// plans were made, so a dry run and the prune it describes list them alike.
func condemnedResults(plans []SeriesPlan) []PrunedResult {
	var out []PrunedResult

	for _, plan := range plans {
		for _, d := range plan.Decisions {
			if d.Keep {
				continue
			}

			out = append(out, PrunedResult{
				Target:      plan.Series.Target,
				ConsentMode: plan.Series.Mode,
				ScanID:      d.ScanID,
				StartedAt:   d.StartedAt,
			})
		}
	}

	return out
}

// deleteCondemned deletes the results every plan condemned and reports how
// many rows went, plus how many series lost at least one.
//
// The reference rows those results own are deliberately left in place. They are
// what the collection step reads to work out which artifacts have just lost
// their last owner, and leaving them means an interrupted prune resumes rather
// than forgetting what it was about to delete (Story 8.5, AC3 and AC4).
//
// It takes a querier rather than reaching for the database, because the dry run
// runs the same deletes inside a transaction it throws away: one definition of
// what a prune removes, used by both, is what stops a plan describing a
// different set from the prune it claims to describe (AC6).
func (s *SQL) deleteCondemned(ctx context.Context, h querier, plans []SeriesPlan) (deleted, series int, err error) {
	for _, plan := range plans {
		doomed := make([]string, 0, len(plan.Decisions))

		for _, d := range plan.Decisions {
			if !d.Keep {
				doomed = append(doomed, d.ScanID)
			}
		}

		if len(doomed) == 0 {
			continue
		}

		n, err := s.deleteResults(ctx, h, plan.Series, doomed)

		deleted += n

		if n > 0 {
			series++
		}

		if err != nil {
			// The count so far is returned with the error rather than rounded
			// down to zero: a partial prune is a fact the caller has to report,
			// and the rows that did go are gone whatever happens next.
			return deleted, series, err
		}
	}

	return deleted, series, nil
}

// deleteResults removes named results of one series, in bounded batches.
func (s *SQL) deleteResults(ctx context.Context, h querier, se Series, scanIDs []string) (int, error) {
	total := 0

	for start := 0; start < len(scanIDs); start += pruneBatch {
		end := min(start+pruneBatch, len(scanIDs))

		n, err := s.deleteBatch(ctx, h, se, scanIDs[start:end])

		total += n

		if err != nil {
			return total, err
		}
	}

	return total, nil
}

func (s *SQL) deleteBatch(ctx context.Context, h querier, se Series, scanIDs []string) (int, error) {
	args := make([]any, 0, len(scanIDs)+2)
	args = append(args, se.Target, string(se.Mode))

	for _, id := range scanIDs {
		args = append(args, id)
	}

	res, err := h.ExecContext(ctx, s.q(deleteResultsQuery(len(scanIDs))), args...)
	if err != nil {
		return 0, fmt.Errorf("pruning results for %s/%s: %w", se.Target, se.Mode, err)
	}

	deleted := 0

	if n, err := res.RowsAffected(); err == nil {
		deleted = int(n)
	}

	return deleted, nil
}

// deleteResultsQuery deletes n named results of one series.
//
// It is the same statement in every dialect and needs no hook of its own: it
// names results by the identity the schema declares rather than by a physical
// row identifier — SQLite's rowid, PostgreSQL's ctid and MySQL's nothing are
// not the same concept — and it never selects from the table it deletes from,
// which is what MySQL refuses (error 1093).
func deleteResultsQuery(n int) string {
	return `delete from ` + resultsTable + ` where target = ? and consent_mode = ? and scan_id in (` +
		strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)`
}

// candidates reads the index columns of one series, newest first. The document
// is never read to decide what to prune — a store holding a year of scans would
// have to be fetched from the bucket in full to answer a question three columns
// already answer (Story 4.10, AC5), which is the whole reason the summary is
// indexed (Story 8.3).
func (s *SQL) candidates(ctx context.Context, se Series) ([]candidate, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	const q = `
		select scan_id, started_at, termination from ` + resultsTable + `
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

// collectDangling deletes the artifacts whose last owner has gone, and forgets
// the references of those that are still named by something alive.
//
// It is the shared engine of a prune's second half and a sweep's first half.
// A prune reaches it having just deleted rows; a sweep reaches it to finish
// whatever an earlier prune could not (AC3), which is why the same walk serves
// both and why a failed delete simply waits here for the next run.
func (s *SQL) collectDangling(ctx context.Context, h querier, now time.Time, stats *PruneStats) error {
	after := ""

	for {
		page, err := s.danglingRefs(ctx, h, after, artifactRefPageSize)
		if err != nil {
			return err
		}

		if len(page) == 0 {
			return nil
		}

		// One query for the whole page, before anything is deleted: which of
		// these keys a scan that is still running has taken (AC3).
		claimed, err := s.claimedSince(ctx, h, danglingRefKeys(page), now.Add(-unreferencedArtifactGrace))
		if err != nil {
			return err
		}

		for _, d := range page {
			after = d.ref

			if err := ctx.Err(); err != nil {
				return fmt.Errorf("collecting unreferenced artifacts: %w", err)
			}

			if err := s.collectOne(ctx, h, d, claimed, stats); err != nil {
				return err
			}
		}
	}
}

// danglingRefKeys is one page's references, for the queries that take a page
// at a time.
func danglingRefKeys(page []danglingRef) []string {
	refs := make([]string, 0, len(page))
	for _, d := range page {
		refs = append(refs, d.ref)
	}

	return refs
}

// collectOne decides the fate of one artifact whose references have outlived
// their scans.
func (s *SQL) collectOne(
	ctx context.Context, h querier, d danglingRef, claimed map[string]struct{}, stats *PruneStats,
) error {
	if d.stillNamed {
		// A surviving result or baseline names the same bytes — the shared
		// artifact AC1 is about. Only the stale references go.
		return s.forgetRef(ctx, h, d.ref)
	}

	if _, taken := claimed[d.ref]; taken {
		// A scan that is running has stored these bytes and has not committed
		// the row that names them. That is the case content addressing makes
		// invisible from the bucket: an unchanged asset stores nothing, so the
		// object still carries the date of the scan that first saw it while a
		// live scan depends on it (AC3). The reference rows are left in place,
		// so the next prune considers the key again.
		stats.ArtifactsProtected++

		return nil
	}

	if !collectable(d.ref, stats.UnknownReferences) {
		stats.ArtifactsProtected++

		return nil
	}

	size, deleted := s.removeArtifact(ctx, d.ref)
	if !deleted {
		// Left exactly as it is, references included, so the next sweep finds
		// it again (AC4). Retention has still done its job: the rows are gone.
		stats.ArtifactsFailed++

		return nil
	}

	stats.ArtifactsDeleted++
	stats.BytesFreed += size

	return s.forgetRef(ctx, h, d.ref)
}

// removeArtifact deletes one artifact and reports how many bytes that
// reclaimed.
//
// The size is read before the delete because the store does not record how
// large a screenshot or a stored body is — only the document's size is on a row
// — and "bytes freed" that were never measured would be a number an operator
// cannot use (AC5). It costs one extra request per artifact actually deleted,
// which is small beside the delete it accompanies and is only paid for
// artifacts that are going.
//
// A key that is already gone counts as done rather than failed: something else
// collected it, which is the outcome this method wanted.
func (s *SQL) removeArtifact(ctx context.Context, ref string) (bytes int64, deleted bool) {
	return reclaimArtifact(ctx, s.bucket, s.log, ref)
}

// reclaimArtifact is removeArtifact for whichever kind of store is pruning.
//
// It is a function of the bucket and a logger rather than a method, because
// both stores reach this point with the same question and must answer it the
// same way: the bucket-index store's prune decides which artifacts have lost
// their last owner from index objects instead of from rows, and everything
// after that decision — measure, delete, count the bytes, treat an absent key
// as done — is the policy of Story 8.5 and not of a store kind. Two copies
// would be two policies, and the one that drifted would be the one nobody was
// reading.
func reclaimArtifact(ctx context.Context, b *bucket, log *slog.Logger, ref string) (bytes int64, deleted bool) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	obj, err := b.stat(ctx, ref)

	switch {
	case errors.Is(err, ErrNotFound):
		return 0, true

	case err != nil:
		log.Warn("an unreferenced artifact could not be measured, so it was left for the next sweep",
			"artifact", ref, "bucket", b.String(), "error", err)

		return 0, false
	}

	if !dropArtifact(ctx, b, log, ref) {
		return 0, false
	}

	return obj.size, true
}

// deleteArtifact removes one object and reports whether it is gone, without
// asking how large it was.
//
// It is separate because the sweep's walk of the bucket already knows: a
// listing reports every key's size, so measuring one again would be a request
// spent on a number already in hand.
//
// A failure is logged rather than returned. A bucket that refuses one delete
// must not abort retention: the rows are already gone, the rest of the
// artifacts are still worth reclaiming, and the key stays referenced so the
// next sweep meets it again (AC4).
func (s *SQL) deleteArtifact(ctx context.Context, ref string) bool {
	return dropArtifact(ctx, s.bucket, s.log, ref)
}

// dropArtifact is deleteArtifact for whichever kind of store is collecting; see
// reclaimArtifact for why it is shared.
func dropArtifact(ctx context.Context, b *bucket, log *slog.Logger, ref string) bool {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	switch err := b.remove(ctx, ref); {
	case err == nil, errors.Is(err, ErrNotFound):
		return true

	default:
		log.Warn("an unreferenced artifact could not be deleted and was left for the next sweep",
			"artifact", ref, "bucket", b.String(), "error", err)

		return false
	}
}

// orphanedRefs lists the artifacts a plan would delete, without touching the
// bucket, and reports how many were protected instead.
func (s *SQL) orphanedRefs(
	ctx context.Context, h querier, now time.Time, unknownRefs int,
) (refs []string, protected int, err error) {
	after := ""

	for {
		page, err := s.danglingRefs(ctx, h, after, artifactRefPageSize)
		if err != nil {
			return nil, 0, err
		}

		if len(page) == 0 {
			return refs, protected, nil
		}

		// The same question the prune asks, so a dry run cannot promise a
		// deletion the prune would refuse.
		claimed, err := s.claimedSince(ctx, h, danglingRefKeys(page), now.Add(-unreferencedArtifactGrace))
		if err != nil {
			return nil, 0, err
		}

		for _, d := range page {
			after = d.ref

			_, taken := claimed[d.ref]

			switch {
			case d.stillNamed:
			case taken, !collectable(d.ref, unknownRefs):
				protected++
			default:
				refs = append(refs, d.ref)
			}
		}
	}
}

// measureArtifacts asks the bucket how large each artifact is, so a plan can
// report the bytes a prune would reclaim.
//
// An artifact whose size cannot be read is still counted, at zero bytes, and
// the reason is logged: a plan that dropped it from the list would understate
// what is about to be deleted, which is the one direction a dry run must not
// err in.
//
// A cancelled walk is reported as the failure it is, for the same reason. The
// list of keys is complete by the time this runs, so returning the bytes
// measured so far would print a reclaim figure an order of magnitude below the
// listing beside it — a half-measured plan presented as a plan.
func (s *SQL) measureArtifacts(ctx context.Context, refs []string) (count int, bytes int64, err error) {
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return count, bytes, fmt.Errorf("measuring what a prune would reclaim: %w", err)
		}

		size, err := s.statArtifact(ctx, ref)
		if err != nil {
			s.log.Debug("an artifact a prune would delete could not be measured",
				"artifact", ref, "bucket", s.bucket.String(), "error", err)
		}

		count++
		bytes += size
	}

	return count, bytes, nil
}

// statArtifact reads one artifact's size under its own deadline.
func (s *SQL) statArtifact(ctx context.Context, ref string) (int64, error) {
	return measureArtifact(ctx, s.bucket, ref)
}

// measureArtifact reads one artifact's size under its own deadline, for
// whichever kind of store is planning a deletion.
func measureArtifact(ctx context.Context, b *bucket, ref string) (int64, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	obj, err := b.stat(ctx, ref)

	return obj.size, err
}

// collectable reports whether an artifact may be deleted at all, given what
// this store knows about which results name what.
//
// While any stored result's references are unknown, a screenshot or a stored
// body that no row names might still belong to it, and deleting it would be
// inferring absence from a gap in wsaw's own index — the failure Tenet 5 exists
// to prevent.
//
// Result documents stay collectable regardless, because a row's own document
// reference is never unknown. That is what schema version 5 established: it
// records every existing row's artifact_ref as a reference in SQL, before any
// document is read, so a row waiting for the backfill — or one the backfill
// gave up on — still names its own document. Without that statement this
// exemption would be a hole rather than a distinction, and a sweep run while
// the backfill was incomplete would collect the documents of live results.
func collectable(ref string, unknownRefs int) bool {
	if unknownRefs == 0 {
		return true
	}

	return strings.HasPrefix(ref, artifactKindResult+refSeparator)
}

// SweepStats reports what a sweep looked at and what it reclaimed.
type SweepStats struct {
	// ArtifactsScanned and BytesScanned are what the bucket holds under
	// wsaw's prefixes — the number an operator wants next to what was freed,
	// because "we deleted 12 objects" means nothing without it.
	ArtifactsScanned int   `json:"artifactsScanned"`
	BytesScanned     int64 `json:"bytesScanned"`

	ArtifactsDeleted int   `json:"artifactsDeleted"`
	BytesFreed       int64 `json:"bytesFreed"`

	// ArtifactsProtected counts objects that were left alone: written or
	// claimed too recently to be called garbage (AC3), or of a kind no sweep
	// may collect while some result's references are unknown.
	ArtifactsProtected int `json:"artifactsProtected"`

	// ForeignObjects counts keys in the bucket that are not the shape this
	// store writes, which a sweep neither deletes nor counts against itself.
	//
	// They are their own number because the alternative was worse in both
	// directions. Attempting to delete them fails — the bucket seam refuses a
	// reference it did not write — so every stray object would be reported as
	// a delete the bucket refused, on every run, and ArtifactsFailed would
	// stop meaning what it says (AC4). Deleting them would be worse still: a
	// sweep would be reaching outside what wsaw wrote.
	ForeignObjects int `json:"foreignObjects"`

	ArtifactsFailed   int `json:"artifactsFailed"`
	UnknownReferences int `json:"unknownReferences"`

	// ResultsWithoutEntry counts result documents the bucket holds that no
	// index entry points at.
	//
	// They are left in place rather than collected, and they are counted
	// rather than passed over in silence, because a result document is
	// self-describing: it decodes to the scan it records, so it is a
	// candidate for a rebuild of the index (Story 8.10) and not garbage. A
	// non-zero value means an interrupted write or an index that is behind the
	// bucket, and a document visible without its index entry must not be
	// silently lost. Screenshots and stored bodies are not self-describing and
	// keep the ordinary grace-based collection.
	ResultsWithoutEntry int `json:"resultsWithoutEntry,omitempty"`

	// RebuildInProgress counts the markers a rebuild of an index leaves while
	// it runs (Story 8.10, AC12).
	//
	// The marker is a fact about the bucket rather than about an index: what a
	// sweep would destroy while a rebuild is half done is the documents of the
	// scans it has not reached, and those are in the bucket.
	//
	// It is its own number rather than part of UnknownReferences, which is
	// where an earlier version of the blob sweep put it. The two say opposite
	// things about the evidence: UnknownReferences says a stored result's
	// document is missing or no longer decodes, which is an integrity problem
	// an operator has to go and look at, while this says the index is half
	// built and the sweep therefore judged nothing — everything is where it
	// was, and the answer is to let the rebuild finish. Reporting the second
	// as the first tells an operator their evidence is damaged when nothing at
	// all is wrong.
	RebuildInProgress int `json:"rebuildInProgress,omitempty"`

	// RebuildMarkers names the objects RebuildInProgress counted.
	//
	// Named and not just counted, because the only way out of a marker whose
	// run was killed is to delete the object, and an operator told "clear its
	// marker" without being told which key that is has been given a puzzle
	// rather than a procedure. A marker older than a day is not in here at all:
	// see rebuildMarkerTTL.
	RebuildMarkers []string `json:"rebuildMarkers,omitempty"`

	// Artifacts names what a plan would delete. A sweep that is deleting
	// leaves it empty.
	Artifacts []string `json:"artifacts,omitempty"`
}

// SweepOptions is what an operator has to say out loud before a sweep will do
// something irreversible on a store that cannot prove what its bucket holds.
type SweepOptions struct {
	// AllowEmptyIndex lets a sweep walk the bucket for a store whose index
	// holds no results and no baselines at all.
	//
	// It is off by default because "no result references this object" and "the
	// index that would say so is gone" look identical from inside a sweep, and
	// the second one is not hypothetical: a restored database without its
	// bucket, a store pointed at the wrong bucket, or a fresh store opened
	// against an existing one produce exactly it — and a sweep would then
	// delete every document, screenshot and body in the bucket and report
	// success. Rebuilding an index from the bucket (Story 8.10) can put the
	// result index back — but only from the documents, which is precisely
	// what such a sweep would have deleted, so there is still no way back
	// (Tenet 5).
	//
	// An operator who really does have a bucket full of garbage and an empty
	// history says so, and the sweep proceeds.
	AllowEmptyIndex bool
}

// Sweep collects artifacts that nothing references (AC3).
//
// It exists because two things leak past a prune. An interrupted write leaves
// an object whose row was never committed — the deliberate consequence of
// writing the bucket first (Story 8.2, AC4) — and a delete the bucket refused
// leaves a key a prune counted and moved on from. Neither is visible from the
// database alone, so this is the one operation that walks the bucket.
//
// It is safe to run while scans are running. An object nothing references but
// that was written, or claimed by a running scan, within the grace period is
// left alone rather than collected, because that is exactly what a scan in
// progress looks like from out here.
//
// It refuses to walk the bucket for a store that holds no scans at all, unless
// asked to in as many words: see SweepOptions.
//
// It is not on a timer. Walking a bucket is a listing of every key wsaw owns,
// which against object storage is a request per page and a line on an invoice,
// so it is something an operator asks for.
func (s *SQL) Sweep(ctx context.Context, trigger string, now time.Time, opts SweepOptions) (SweepStats, error) {
	return s.sweep(ctx, trigger, now, opts, false)
}

// PlanSweep reports what Sweep would collect, and collects nothing (AC6).
func (s *SQL) PlanSweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error) {
	return s.sweep(ctx, "", now, opts, true)
}

// ErrEmptyIndex refuses a sweep of a bucket that the store holds no index for.
//
// A sweep decides by reference, and a store that holds no scan and has never
// recorded a reference or a claim references nothing — so every object in the
// bucket looks like garbage, including a full history whose database was
// restored without it. The sentinel is exported so the command that offers the
// override can name it.
var ErrEmptyIndex = errors.New(
	"this store's index holds nothing at all, so every artifact in the bucket looks unreferenced",
)

// sweep carries out Sweep or PlanSweep. A real run (plan is false) records
// its own receipt as the last thing it does, whether or not it returns an
// error, the same contract prune keeps (Story 4.11, AC3 and AC4).
func (s *SQL) sweep(ctx context.Context, trigger string, now time.Time, opts SweepOptions, plan bool) (stats SweepStats, err error) {
	if !plan {
		started := time.Now()

		defer func() {
			if recErr := s.RecordSweepRun(ctx, trigger, started, stats, err); recErr != nil {
				s.log.Warn("a sweep run could not be recorded", "error", recErr)
			}
		}()
	}

	// The same reason the prune establishes it: the collection path reads a
	// missing key as a key somebody else collected, which is true of one
	// object and false of a bucket that has gone away (Tenet 5).
	if err := s.bucket.reachable(ctx); err != nil {
		return stats, fmt.Errorf("sweeping needs the artifact bucket: %w", err)
	}

	unknown, err := s.resultsWithUnknownRefs(ctx)
	if err != nil {
		return stats, err
	}

	stats.UnknownReferences = unknown

	rebuilding, err := s.bucket.rebuildMarkers(ctx, now, s.log)
	if err != nil {
		return stats, err
	}

	if len(rebuilding) > 0 {
		// A rebuild of an index is running against this bucket, and until it
		// finishes the documents of the scans it has not reached yet are
		// referenced by nothing. Collecting on that basis would delete the
		// evidence the rebuild exists to recover, so nothing is judged at all
		// (Story 8.10, AC12).
		//
		// The marker is in the bucket rather than in either index because that
		// is where the risk is, so a SQL store honours it exactly as the
		// bucket-index store does — including a rebuild of a bucket-index store
		// running against the same bucket this one keeps its evidence in.
		stats.RebuildInProgress = len(rebuilding)
		stats.RebuildMarkers = rebuilding

		s.log.Warn("a rebuild of an index is in progress, so the sweep collected nothing",
			"markers", len(rebuilding), "bucket", s.bucket.String())

		return stats, nil
	}

	// Asked before anything is collected, because collecting changes the
	// answer: the reference half below empties the very rows that establish
	// that this store and this bucket belong together.
	if err := s.indexCanJudgeTheBucket(ctx, opts); err != nil {
		return stats, err
	}

	// First the references that outlived their results: the leftovers of a
	// prune that was interrupted or whose deletes the bucket refused. They are
	// answered from the database, so they cost nothing to check and they make
	// the walk that follows see fewer keys as unreferenced.
	if err := s.sweepDangling(ctx, now, &stats, plan); err != nil {
		return stats, err
	}

	if err := s.sweepBucket(ctx, now, &stats, plan); err != nil {
		return stats, err
	}

	return stats, nil
}

// indexCanJudgeTheBucket refuses a sweep when the index has nothing to judge
// the bucket with.
//
// What is at stake is only the walk of the bucket — the reference half works
// from rows that exist and cannot mistake an absent index for absent evidence
// — but an index that holds nothing holds no such rows either, so refusing the
// whole sweep costs nothing and refusing it before any collection happens is
// what makes the answer stable.
func (s *SQL) indexCanJudgeTheBucket(ctx context.Context, opts SweepOptions) error {
	if opts.AllowEmptyIndex {
		return nil
	}

	empty, err := s.indexIsEmpty(ctx)
	if err != nil {
		return err
	}

	if empty {
		return fmt.Errorf("%w: if the bucket really does hold nothing but garbage, sweep it with the override; "+
			"otherwise the index this store should hold is missing, and deleting on that basis is not recoverable",
			ErrEmptyIndex)
	}

	return nil
}

// indexIsEmpty reports whether this store's index says anything at all about
// any artifact.
//
// Four tables, because each is on its own enough to establish that this store
// and this bucket belong together. A result or a baseline names evidence
// directly. A reference row without its result is what a prune that could not
// finish leaves behind, and it is precisely a record of an object in this
// bucket. A claim is a note that this store wrote an artifact there. Only a
// store where all four are empty knows nothing — and that is the state a
// restored database, a wrong bucket or a fresh store opened against somebody
// else's evidence is in.
func (s *SQL) indexIsEmpty(ctx context.Context) (bool, error) {
	for _, table := range []string{resultsTable, "baselines", resultArtifactsTable, artifactClaimsTable} {
		present, err := s.hasRows(ctx, table)
		if err != nil {
			return false, err
		}

		if present {
			return false, nil
		}
	}

	return true, nil
}

// hasRows reports whether a table holds anything, without counting it. A
// count(*) over a year of history is a scan on some databases, and the
// question here is only whether the table is empty.
func (s *SQL) hasRows(ctx context.Context, table string) (bool, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	q := `select count(*) from (select 1 from ` + table + ` limit 1) as sample`

	var n int

	err := s.retry(ctx, "checking whether the store holds any scan", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(q)).Scan(&n)
	})
	if err != nil {
		return false, fmt.Errorf("checking whether %s holds any row: %w", table, err)
	}

	return n > 0, nil
}

// sweepDangling runs the reference-side half of a sweep and folds its counts
// into the sweep's own.
func (s *SQL) sweepDangling(ctx context.Context, now time.Time, stats *SweepStats, plan bool) error {
	prune := PruneStats{UnknownReferences: stats.UnknownReferences}

	if plan {
		refs, protected, err := s.orphanedRefs(ctx, s.db, now, stats.UnknownReferences)
		if err != nil {
			return err
		}

		count, bytes, err := s.measureArtifacts(ctx, refs)
		if err != nil {
			return err
		}

		stats.Artifacts = append(stats.Artifacts, refs...)
		stats.ArtifactsDeleted += count
		stats.BytesFreed += bytes
		stats.ArtifactsProtected += protected

		return nil
	}

	if err := s.collectDangling(ctx, s.db, now, &prune); err != nil {
		return err
	}

	stats.ArtifactsDeleted += prune.ArtifactsDeleted
	stats.BytesFreed += prune.BytesFreed
	stats.ArtifactsFailed += prune.ArtifactsFailed
	stats.ArtifactsProtected += prune.ArtifactsProtected

	return nil
}

// sweepBucket walks every key wsaw owns and collects the ones no result names.
//
// The keys are checked a page at a time rather than one by one: a bucket
// holding every document of every scan has more keys than a query per key
// would be reasonable for, and more than a sweep should hold in memory at once.
//
// The walk starts at the root of the bucket, because that is where wsaw's own
// kinds are, and every key it meets is put through the same shape test the
// bucket seam applies to a reference. A key that fails it was not written by
// this store — a leftover from another tool, a provider's placeholder, another
// deployment's layout — and is counted apart and left alone: a sweep may only
// collect what wsaw wrote (AC3, AC4).
func (s *SQL) sweepBucket(ctx context.Context, now time.Time, stats *SweepStats, plan bool) error {
	cutoff := now.Add(-unreferencedArtifactGrace)
	page := make([]artifactObject, 0, artifactRefPageSize)

	flush := func() error {
		if len(page) == 0 {
			return nil
		}

		err := s.collectUnreferenced(ctx, now, page, stats, plan)
		page = page[:0]

		return err
	}

	err := s.bucket.list(ctx, "", func(obj artifactObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !isArtifactRef(obj.ref) {
			// Not counted as scanned either: ArtifactsScanned is what wsaw
			// holds in the bucket, and this is not wsaw's.
			stats.ForeignObjects++

			s.log.Debug("an object in the artifact bucket was not written by wsaw, so the sweep left it alone",
				"key", truncateForMessage(obj.ref), "bucket", s.bucket.String())

			return nil
		}

		stats.ArtifactsScanned++
		stats.BytesScanned += obj.size

		if obj.modTime.After(cutoff) {
			// Too young to be called garbage: this is what an artifact of a
			// scan that has not finished writing its row looks like (AC3).
			stats.ArtifactsProtected++

			return nil
		}

		page = append(page, obj)

		if len(page) < artifactRefPageSize {
			return nil
		}

		return flush()
	})
	if err != nil {
		return fmt.Errorf("sweeping artifact bucket %s: %w", s.bucket, err)
	}

	return flush()
}

// collectUnreferenced deletes the objects in one page that no stored result
// names.
func (s *SQL) collectUnreferenced(
	ctx context.Context, now time.Time, page []artifactObject, stats *SweepStats, plan bool,
) error {
	refs := make([]string, 0, len(page))
	for _, obj := range page {
		refs = append(refs, obj.ref)
	}

	named, err := s.referencedRefs(ctx, refs)
	if err != nil {
		return err
	}

	// Two questions per page, both answered from the index: which of these
	// keys a stored result names, and which a scan that is still running has
	// taken. The second is what the object's own age cannot answer — a scan
	// that captured an unchanged asset wrote nothing, so the key it depends on
	// carries the date of the scan that first stored those bytes (AC3).
	claimed, err := s.claimedSince(ctx, s.db, refs, now.Add(-unreferencedArtifactGrace))
	if err != nil {
		return err
	}

	for _, obj := range page {
		if _, referenced := named[obj.ref]; referenced {
			continue
		}

		if _, taken := claimed[obj.ref]; taken {
			stats.ArtifactsProtected++

			continue
		}

		if !collectable(obj.ref, stats.UnknownReferences) {
			stats.ArtifactsProtected++

			continue
		}

		if plan {
			stats.Artifacts = append(stats.Artifacts, obj.ref)
			stats.ArtifactsDeleted++
			// The listing already reported the size, so a plan of the bucket
			// half needs no request of its own.
			stats.BytesFreed += obj.size

			continue
		}

		if !s.deleteArtifact(ctx, obj.ref) {
			stats.ArtifactsFailed++

			continue
		}

		stats.ArtifactsDeleted++
		stats.BytesFreed += obj.size
	}

	return nil
}
