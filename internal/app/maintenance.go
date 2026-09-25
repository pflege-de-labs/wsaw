package app

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// This file is the daemon's maintenance: the hourly prune that enforces
// retention (Story 8.5), the scheduled sweep that collects what a prune
// cannot see (Story 4.12), and the scheduled vacuum that gives a SQLite file
// back the pages both leave free (Story 4.13).
//
// All three run in one goroutine, one after the other, and that is the whole
// of how they are kept apart (Story 4.12, AC5; Story 4.13, AC6). A vacuum
// holds the database's write lock for as long as it rewrites the file, so
// scans that finish meanwhile queue behind it on the store's one connection;
// running it after the prune and the sweep is also when it has most to
// reclaim. A prune and a sweep on the same store at the same
// time would each be correct — both are already safe beside live scans — but
// they would compete for the same database and the same bucket, and a sweep
// walking a bucket while a prune deletes from it would report counts that
// describe neither. One loop makes "never at the same time" true by
// construction rather than by a lock somebody has to remember to take, and it
// makes "a sweep still running when the next falls due is not started twice"
// true for the same reason: the next one cannot start until this one returns.
// The cost is that a long sweep delays the prune behind it, which is the
// serialisation asked for.
//
// A sweep run by `wsaw store sweep` in another process is not serialised with
// any of this, and does not need to be: the grace period protects in-flight
// evidence from either one, which is what made a sweep safe beside live scans
// in the first place (Story 8.5, AC3).

// pruneInterval is how often the daemon prunes, as it always has.
const pruneInterval = time.Hour

// The first scheduled sweep of a process that has never swept, or whose last
// sweep is overdue, starts between sweepStartupDelay and sweepStartupDelay +
// sweepStartupJitter after startup (Story 4.12, AC3).
//
// The floor is for a daemon that is crash-looping. A sweep is a full listing of
// the bucket, and a process restarted every few minutes by a supervisor that
// has not given up on it yet must not start one on every restart; ten minutes
// outlasts the backoff of the common supervisors while still being "shortly"
// against a schedule measured in days. The jitter is for a fleet restarted
// together by one deploy, which would otherwise list its buckets in the same
// second.
const (
	sweepStartupDelay  = 10 * time.Minute
	sweepStartupJitter = 10 * time.Minute
)

// VacuumSchedule is whether the daemon vacuums on a schedule, how often, and
// how much of the file must be free before a vacuum rewrites it.
type VacuumSchedule struct {
	Enabled      bool
	Interval     time.Duration
	MinFreeRatio float64
}

// VacuumScheduleFor reads the vacuum schedule out of configuration, for the
// reason SweepScheduleFor is a function of configuration alone (Story 4.13,
// AC11).
func VacuumScheduleFor(cfg *config.Config) VacuumSchedule {
	return VacuumSchedule{
		Enabled:      cfg.Store.VacuumEnabled(),
		Interval:     cfg.Store.VacuumEvery(),
		MinFreeRatio: cfg.Store.VacuumFreeRatio(),
	}
}

// vacuumer is the part of a store a scheduled vacuum needs, declared where it
// is consumed rather than added to store.Store: only a SQLite store has one to
// offer, and the loop asks rather than assumes.
type vacuumer interface {
	SupportsVacuum() bool
	Vacuum(ctx context.Context, trigger string, opts store.VacuumOptions) (store.VacuumStats, error)
}

// SweepSchedule is whether the daemon sweeps on a schedule, and how often.
type SweepSchedule struct {
	Enabled  bool
	Interval time.Duration
}

// SweepScheduleFor reads the sweep schedule out of configuration. It is a
// function of the configuration alone so that a reload can hand the running
// loop a new one without the loop re-reading the file (Story 4.12, AC8).
func SweepScheduleFor(cfg *config.Config) SweepSchedule {
	return SweepSchedule{Enabled: cfg.Store.SweepEnabled(), Interval: cfg.Store.SweepEvery()}
}

// lastRunReader is the one read of the receipt log the maintenance loop needs:
// when the last sweep was, so the schedule survives a restart (AC3). It is
// declared here, where it is consumed, rather than added to store.Store — the
// pattern httpapi.Store and scanner.ResultStore set, and the one Story 4.11,
// AC6 asks for.
type lastRunReader interface {
	LastMaintenanceRun(ctx context.Context, kind string) (store.MaintenanceRun, bool, error)
}

// maintenanceClock is what the loop reads time from and waits on. It is a
// value rather than calls to the time package so that the schedule is tested
// against a clock the test moves, not one it waits for (AC9).
type maintenanceClock struct {
	now func() time.Time
	// timer returns a channel that receives once d has passed, and a function
	// that releases it early.
	timer func(d time.Duration) (<-chan time.Time, func())
	// jitter returns an offset in [0, window).
	jitter func(window time.Duration) time.Duration
}

// systemClock is the clock the daemon runs on.
func systemClock() maintenanceClock {
	return maintenanceClock{
		now: time.Now,
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)

			return t.C, func() { t.Stop() }
		},
		jitter: randomJitter,
	}
}

// randomJitter is uniform over [0, window).
//
// Random rather than derived from something about the host, because what it
// has to separate is a fleet restarted together, and hosts built from one
// image agree on more than anyone expects. If the system's random source
// fails — it does not, in practice — the middle of the window is as good a
// place to start as any, and better than refusing to schedule.
func randomJitter(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(window)))
	if err != nil {
		return window / 2
	}

	return time.Duration(n.Int64())
}

// MaintenanceLoop prunes, sweeps and vacuums on their schedules until ctx is
// cancelled. Without it a daemon that runs for months grows without bound
// (NFR §1); with it, all three are observable, because every run is logged,
// counted and recorded.
//
// A schedule received on sweeps replaces the sweep schedule in force, which is
// how a reload applies store.sweep and store.sweepInterval (Story 4.12, AC8);
// one received on vacuums does the same for the vacuum's (Story 4.13, AC11).
func (a *App) MaintenanceLoop(ctx context.Context, sweeps <-chan SweepSchedule, vacuums <-chan VacuumSchedule) {
	a.maintenanceLoop(ctx, sweeps, vacuums, systemClock())
}

func (a *App) maintenanceLoop(
	ctx context.Context, sweeps <-chan SweepSchedule, vacuums <-chan VacuumSchedule, clock maintenanceClock,
) {
	m := a.newMaintenance(ctx, clock)

	for {
		fire, stop := m.arm()

		select {
		case <-ctx.Done():
			stop()

			return

		case s := <-sweeps:
			stop()
			m.reschedule(ctx, s)

		case v := <-vacuums:
			stop()
			m.rescheduleVacuum(ctx, v)

		case <-fire:
			m.runDue(ctx)
		}
	}
}

// maintenance is the loop's state: what is scheduled, and when it is next due.
// It is only ever touched by the loop's own goroutine.
type maintenance struct {
	app   *App
	clock maintenanceClock

	// prune is false when no retention policy is configured or the one
	// configured cannot be used; nextPrune is meaningless then.
	prune     bool
	retention store.Retention
	nextPrune time.Time

	sweep SweepSchedule
	// nextSweep is zero while sweeping is off.
	nextSweep time.Time
	// scheduled is set once a sweep schedule has been put in force, so the
	// first one always is, whatever it says.
	scheduled bool

	// vacuumStore is nil when the store has nothing to vacuum — it is not
	// SQLite — and then no vacuum is ever scheduled, whatever the
	// configuration says.
	vacuumStore     vacuumer
	vacuum          VacuumSchedule
	nextVacuum      time.Time
	vacuumScheduled bool
}

func (a *App) newMaintenance(ctx context.Context, clock maintenanceClock) *maintenance {
	m := &maintenance{app: a, clock: clock}

	retention, err := a.Retention()

	switch {
	case err != nil:
		a.Logger.Error("retention is not usable; no history will be pruned", "error", err)

	case retention.Active():
		m.prune = true
		m.retention = retention
		m.nextPrune = clock.now().Add(pruneInterval)
	}

	m.reschedule(ctx, SweepScheduleFor(a.Config))

	vacuum := VacuumScheduleFor(a.Config)

	if v, ok := a.Store.(vacuumer); ok && v.SupportsVacuum() {
		m.vacuumStore = v
	} else if vacuum.Enabled {
		// Said once, at startup, rather than on every reload: it is a fact
		// about the store, which a reload cannot change (Story 4.13, AC2).
		a.Logger.Info("scheduled vacuuming does not apply to this store; only a SQLite file is vacuumed by wsaw",
			"driver", storeDriver(a.Store))
	}

	m.rescheduleVacuum(ctx, vacuum)

	return m
}

// storeDriver names the store's driver for a log line, or says there is none.
func storeDriver(s store.Store) string {
	if s == nil {
		return "none"
	}

	return s.Driver()
}

// arm waits for whichever run is due first. With nothing scheduled at all it
// returns a channel that never fires, and the loop waits for a reload or for
// shutdown.
func (m *maintenance) arm() (<-chan time.Time, func()) {
	var next time.Time

	if m.prune {
		next = m.nextPrune
	}

	for _, due := range []time.Time{m.nextSweep, m.nextVacuum} {
		if !due.IsZero() && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}

	if next.IsZero() {
		return nil, func() {}
	}

	return m.clock.timer(next.Sub(m.clock.now()))
}

// runDue runs whatever has fallen due, prune first, and schedules each one's
// next run from when it finished. A run that overran its own interval is
// therefore followed by one interval of rest rather than started again at
// once, which is what "not started twice" asks for.
func (m *maintenance) runDue(ctx context.Context) {
	if now := m.clock.now(); m.prune && !now.Before(m.nextPrune) {
		m.app.pruneOnce(ctx, now, m.retention)
		m.nextPrune = m.clock.now().Add(pruneInterval)
	}

	// A shutdown that arrived during the prune is not a reason to start
	// walking a bucket.
	if ctx.Err() != nil {
		return
	}

	if now := m.clock.now(); !m.nextSweep.IsZero() && !now.Before(m.nextSweep) {
		m.app.sweepOnce(ctx, now)
		m.nextSweep = m.clock.now().Add(m.sweep.Interval)
	}

	// Nor is it a reason to start rewriting the database.
	if ctx.Err() != nil {
		return
	}

	if now := m.clock.now(); !m.nextVacuum.IsZero() && !now.Before(m.nextVacuum) {
		m.app.vacuumOnce(ctx, m.vacuumStore, now, m.vacuum)
		m.nextVacuum = m.clock.now().Add(m.vacuum.Interval)
	}
}

// reschedule puts a sweep schedule in force and decides when the next sweep
// is due under it.
//
// A reload that did not change the schedule changes nothing: every accepted
// SIGHUP hands the loop the schedule, and one that only edited the target list
// must not push the next sweep back.
func (m *maintenance) reschedule(ctx context.Context, s SweepSchedule) {
	if m.scheduled && s == m.sweep {
		return
	}

	m.sweep, m.scheduled = s, true

	if !s.Enabled {
		m.nextSweep = time.Time{}

		m.app.Logger.Info("scheduled sweeping is off (store.sweep: false); the artifact bucket is swept only by \"wsaw store sweep\"")

		return
	}

	now := m.clock.now()
	soonest := now.Add(sweepStartupDelay + m.clock.jitter(sweepStartupJitter))

	last, found := m.lastRun(ctx, store.MaintenanceKindSweep)
	m.nextSweep = sweepDue(last, found, soonest, s.Interval)

	m.app.Logger.Info("scheduled the next sweep of the artifact bucket",
		"next", m.nextSweep.Format(time.RFC3339),
		"interval", s.Interval.String())
}

// rescheduleVacuum puts a vacuum schedule in force and decides when the next
// vacuum is due under it, the way reschedule does for the sweep: one interval
// after the last recorded vacuum — a skip included, since a skip is a vacuum
// that looked and found no need — and never sooner than the startup delay
// (Story 4.13, AC7). A store upgraded through Story 8.4 has no receipt, so it
// gets its space back shortly after its first start.
func (m *maintenance) rescheduleVacuum(ctx context.Context, v VacuumSchedule) {
	if m.vacuumScheduled && v == m.vacuum {
		return
	}

	m.vacuum, m.vacuumScheduled = v, true

	if m.vacuumStore == nil {
		m.nextVacuum = time.Time{}

		return
	}

	if !v.Enabled {
		m.nextVacuum = time.Time{}

		m.app.Logger.Info("scheduled vacuuming is off (store.vacuum: false); the database file is vacuumed only by \"wsaw store vacuum\"")

		return
	}

	soonest := m.clock.now().Add(sweepStartupDelay + m.clock.jitter(sweepStartupJitter))

	last, found := m.lastRun(ctx, store.MaintenanceKindVacuum)
	m.nextVacuum = sweepDue(last, found, soonest, v.Interval)

	m.app.Logger.Info("scheduled the next vacuum of the database file",
		"next", m.nextVacuum.Format(time.RFC3339),
		"interval", v.Interval.String(),
		"min_free_ratio", v.MinFreeRatio)
}

// lastRun reads when the last recorded run of kind finished, whichever
// process ran it.
//
// A store that cannot say, or a read that fails, is treated as a store that
// has never run one. That is the answer that runs soonest, and it is safe to
// be wrong in that direction: the startup delay still applies, a sweep that
// was not needed yet costs one listing, and a vacuum that was not needed yet
// is skipped by its threshold.
func (m *maintenance) lastRun(ctx context.Context, kind string) (time.Time, bool) {
	reader, ok := m.app.Store.(lastRunReader)
	if !ok {
		return time.Time{}, false
	}

	run, found, err := reader.LastMaintenanceRun(ctx, kind)
	if err != nil {
		m.app.Logger.Warn("the last run could not be read from the maintenance log, so the next is scheduled as though none had run",
			"kind", kind, "error", err)

		return time.Time{}, false
	}

	if !found {
		return time.Time{}, false
	}

	return run.FinishedAt, true
}

// sweepDue is when the next sweep falls due: one interval after the last
// recorded one, and never sooner than soonest (AC3). The vacuum's schedule is
// kept the same way.
//
// It is measured from the receipt, not from when this process started,
// because a daemon restarted every night — by a deploy, or by a host that
// sleeps — would otherwise never reach its first sweep at all.
func sweepDue(lastFinished time.Time, found bool, soonest time.Time, interval time.Duration) time.Time {
	if !found {
		return soonest
	}

	if due := lastFinished.Add(interval); due.After(soonest) {
		return due
	}

	return soonest
}

// sweepOnce runs one scheduled sweep and reports it where a prune's report
// already goes (AC6): the log, the metrics, and — written by Sweep itself —
// the receipt log.
//
// It never sets AllowEmptyIndex. A daemon cannot say out loud that it means to
// empty a bucket, so a store whose index holds nothing is refused on every
// scheduled run, and the next attempt is one interval later like any other
// outcome, never a retry in a loop (AC4).
func (a *App) sweepOnce(ctx context.Context, now time.Time) {
	stats, err := a.Store.Sweep(ctx, store.TriggerSchedule, now, store.SweepOptions{})

	// Counted from the same stats whatever the outcome, for the reason
	// pruneOnce gives: a sweep deletes as it walks, so one that failed half
	// way has still removed everything it counted.
	a.Metrics.Pruned(0, stats.ArtifactsDeleted, stats.BytesFreed)
	a.Metrics.ArtifactDeletionsFailed(stats.ArtifactsFailed)

	if err != nil {
		a.Metrics.SweepRun(metrics.SweepError)
		a.logFailedSweep(ctx, stats, err)

		return
	}

	a.Metrics.SweepRun(metrics.SweepSuccess)
	// Only on this path, as with the prune gauge: "how long ago did a sweep
	// last succeed" is not answered by one that did not.
	a.Metrics.SweepSucceeded(now)

	if stats.ArtifactsFailed > 0 {
		a.Logger.Warn("some unreferenced artifacts could not be deleted and were left for the next sweep",
			"artifacts_failed", stats.ArtifactsFailed)
	}

	a.Logger.Info("swept the artifact bucket", sweepStatsAttrs(stats)...)
}

// logFailedSweep says why a scheduled sweep did not complete, at the level
// the reason deserves.
func (a *App) logFailedSweep(ctx context.Context, stats store.SweepStats, err error) {
	attrs := append([]any{"error", err}, sweepStatsAttrs(stats)...)

	switch {
	case errors.Is(err, store.ErrEmptyIndex):
		// Degraded and handled: nothing was examined, and nothing will be
		// until somebody decides what this store is. The command is named
		// because the daemon will never run it.
		a.Logger.Warn("the scheduled sweep was refused because this store's index holds nothing, "+
			"so every artifact in the bucket would look unreferenced; if the bucket really holds only leftovers, "+
			"run \"wsaw store sweep --allow-empty-index\" — the daemon never will",
			attrs...)

	case ctx.Err() != nil:
		// A shutdown, not a failure. Whatever had been deleted is in the
		// receipt, and the next process picks up the schedule from it.
		a.Logger.Info("the sweep was interrupted by shutdown; what it had deleted is recorded", attrs...)

	default:
		a.Logger.Error("sweeping the artifact bucket failed", attrs...)
	}
}

func sweepStatsAttrs(stats store.SweepStats) []any {
	return []any{
		"artifacts_scanned", stats.ArtifactsScanned,
		"bytes_scanned", stats.BytesScanned,
		"artifacts_deleted", stats.ArtifactsDeleted,
		"bytes_freed", stats.BytesFreed,
		"artifacts_protected", stats.ArtifactsProtected,
		"artifacts_failed", stats.ArtifactsFailed,
		"foreign_objects", stats.ForeignObjects,
		"unknown_references", stats.UnknownReferences,
	}
}

// vacuumOnce runs one scheduled vacuum and reports it where a sweep's report
// goes (Story 4.13, AC8 and AC9): the log, the metrics, and — written by
// Vacuum itself — the receipt log. Whatever the outcome, the next is one
// interval later; a refusal for want of disk is not retried sooner, since the
// disk will not have grown.
func (a *App) vacuumOnce(ctx context.Context, v vacuumer, now time.Time, s VacuumSchedule) {
	stats, err := v.Vacuum(ctx, store.TriggerSchedule, store.VacuumOptions{MinFreeRatio: s.MinFreeRatio})

	a.Metrics.VacuumRun(stats.Outcome, stats.ReclaimedBytes())

	attrs := vacuumStatsAttrs(stats)
	failed := append([]any{"error", err}, attrs...)

	switch {
	case err == nil:
		a.Metrics.VacuumSucceeded(now)

		if stats.Outcome == store.VacuumSkipped {
			a.Logger.Info("the database file has too few free pages to be worth rewriting; the vacuum was skipped", attrs...)

			return
		}

		if stats.CheckpointIncomplete {
			a.Logger.Warn("vacuumed the database file, but the WAL could not be truncated while another connection "+
				"was reading; its space comes back at the next checkpoint", attrs...)

			return
		}

		a.Logger.Info("vacuumed the database file", attrs...)

	case errors.Is(err, store.ErrVacuumNoSpace):
		a.Logger.Warn("the vacuum was refused because the disk cannot hold the copy it builds; free some space, "+
			"or run \"wsaw store vacuum\" once there is room", failed...)

	case ctx.Err() != nil:
		// A shutdown, not a failure: VACUUM is a transaction, so the file is
		// exactly as it was.
		a.Logger.Info("the vacuum was interrupted by shutdown; the database file is unchanged", failed...)

	default:
		a.Logger.Error("vacuuming the database file failed", failed...)
	}
}

func vacuumStatsAttrs(stats store.VacuumStats) []any {
	return []any{
		"outcome", stats.Outcome,
		"file_bytes_before", stats.FileBytesBefore,
		"file_bytes_after", stats.FileBytesAfter,
		"wal_bytes_before", stats.WALBytesBefore,
		"wal_bytes_after", stats.WALBytesAfter,
		"bytes_reclaimed", stats.ReclaimedBytes(),
		"free_ratio", stats.FreeRatio,
		"min_free_ratio", stats.MinFreeRatio,
		"duration", stats.Duration.String(),
	}
}
