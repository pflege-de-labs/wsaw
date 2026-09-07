// Package daemon runs wsaw as a long-lived service.
//
// The scheduler's job is to keep producing observations without becoming a
// nuisance to the sites it watches (NFR §8) or to the host it runs on
// (NFR §1). Both are enforced here rather than left to configuration
// discipline.
package daemon

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// job is one target-and-mode pair with its own schedule.
type job struct {
	target config.Resolved
	mode   model.ConsentMode

	interval time.Duration
	schedule cron.Schedule

	// next is when this job should run.
	next time.Time
	// lastRun enforces the minimum interval between two scans of one target.
	lastRun time.Time

	// attempt is which attempt of the current scan the next run will be,
	// counting from 1, and prevError is why the last one failed. Both reset
	// once a scan produces a usable observation or runs out of attempts
	// (Story 3.8).
	attempt   int
	prevError string
	// retrying marks a job whose next run is a retry rather than a scheduled
	// scan, so the minimum interval does not hold it back.
	retrying bool
}

func (j *job) key() string {
	return j.target.Name + "/" + string(j.mode)
}

// lastScanFunc reports when a target and mode were last scanned, and whether
// they ever were.
type lastScanFunc func(target string, mode model.ConsentMode) (time.Time, bool)

// buildJobs builds the job list from resolved targets.
//
// lastScan may be nil, in which case every job starts as though its target
// had never been scanned. That is only correct for a process that never
// restarts, which is why the daemon always supplies it in practice.
func buildJobs(targets []config.Resolved, now time.Time, catchUp bool, lastScan lastScanFunc) ([]*job, error) {
	var jobs []*job

	for _, t := range targets {
		for _, mode := range t.ConsentModes {
			j := &job{target: t, mode: mode, interval: t.Interval, attempt: 1}

			if t.Cron != "" {
				sched, err := cron.ParseStandard(t.Cron)
				if err != nil {
					return nil, fmt.Errorf("target %q: cron %q: %w", t.Name, t.Cron, err)
				}

				j.schedule = sched
			}

			// Seeded from the store, so a restart continues a target's
			// schedule instead of starting it over. Without this the minimum
			// interval has nothing to measure from and every target is due at
			// startup.
			if lastScan != nil {
				if at, ok := lastScan(t.Name, mode); ok && !at.IsZero() {
					// A timestamp in the future would park the job for as
					// long as the clock is wrong, so it is treated as now:
					// clock skew must not stop a watcher watching (Tenet 8).
					if at.After(now) {
						at = now
					}

					j.lastRun = at
				}
			}

			j.next = j.firstRun(now, catchUp)
			jobs = append(jobs, j)
		}
	}

	return jobs, nil
}

// firstRun decides when a job runs for the first time after startup.
//
// A target with a known last scan continues its schedule from that scan
// rather than from the restart: a daily target scanned an hour ago is due in
// twenty-three hours, not now. Only a target that is genuinely overdue — or
// has never been scanned — runs at startup, and then soon but jittered, so a
// restart does not fire every overdue target at once. A restart storm against
// a shared origin is exactly the behaviour that gets a scanner blocked.
func (j *job) firstRun(now time.Time, catchUp bool) time.Time {
	// A cron schedule is absolute: the next occurrence does not depend on when
	// the process started or when the target last ran. The minimum interval
	// still applies to it, and now has a last scan to measure from.
	if j.schedule != nil {
		return j.schedule.Next(now)
	}

	soon := now.Add(j.startupDelay())
	if catchUp {
		soon = now
	}

	if j.lastRun.IsZero() || j.interval <= 0 {
		return soon
	}

	if due := j.lastRun.Add(j.interval + j.jitter(j.lastRun)); due.After(now) {
		return due
	}

	return soon
}

// startupDelay spreads jobs deterministically across the jitter window. It is
// derived from the job key rather than randomly, so a restart reproduces the
// same spread instead of reshuffling every target's phase.
func (j *job) startupDelay() time.Duration {
	window := j.target.Jitter
	if window <= 0 {
		window = 30 * time.Second
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(j.key()))

	// Sum32 is 32 bits and window is positive, so this cannot overflow.
	return time.Duration(int64(h.Sum32()) % int64(window))
}

// advance computes the next run time after a completed scan.
func (j *job) advance(now time.Time) {
	switch {
	case j.schedule != nil:
		j.next = j.schedule.Next(now)

	case j.interval > 0:
		j.next = now.Add(j.interval + j.jitter(now))

	default:
		// Without a schedule the job runs once and then waits effectively
		// forever, rather than spinning.
		j.next = now.Add(24 * time.Hour)
	}

	// The minimum interval is a floor regardless of the configured schedule,
	// so a misconfigured cron cannot hammer one origin.
	if floor := j.target.MinInterval; floor > 0 && j.next.Sub(now) < floor {
		j.next = now.Add(floor)
	}
}

// jitter returns a deterministic offset within the configured window, derived
// from the job key and the current period, so scans do not drift into
// lockstep over time.
func (j *job) jitter(now time.Time) time.Duration {
	if j.target.Jitter <= 0 {
		return 0
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(j.key()))
	_, _ = fmt.Fprintf(h, "%d", now.Unix()/60)

	return time.Duration(int64(h.Sum32()) % int64(j.target.Jitter))
}

// dueAt reports whether the job should run, and enforces the minimum interval
// as a hard floor even if the schedule says otherwise.
//
// A retry is exempt from that floor, and deliberately so. The floor exists to
// bound how often wsaw asks a site for the same observation; a retry is the
// same observation, not another one, and it happens at most attempts-1 times
// before the job goes back to its schedule. Holding a retry for an hour
// because minInterval says so would make retrying useless, which is the
// opposite of what Story 3.8 is for — the politeness that still applies is
// the per-origin concurrency limit and the jittered backoff.
func (j *job) dueAt(now time.Time) bool {
	if now.Before(j.next) {
		return false
	}

	if j.retrying {
		return true
	}

	if floor := j.target.MinInterval; floor > 0 && !j.lastRun.IsZero() && now.Sub(j.lastRun) < floor {
		return false
	}

	return true
}

// scheduleRetry puts the job back in the queue for another attempt.
//
// It is a requeue rather than a sleep so the worker is released immediately:
// one flapping target must not hold a slot, or a handful of them would stall
// every other target's schedule (Story 3.8, AC8).
func (j *job) scheduleRetry(now time.Time, delay time.Duration, because string) {
	j.attempt++
	j.prevError = because
	j.retrying = true
	j.next = now.Add(delay)
}

// resetRetries returns the job to its ordinary schedule.
func (j *job) resetRetries() {
	j.attempt = 1
	j.prevError = ""
	j.retrying = false
}

// originLimiter bounds how many scans may run against one origin at a time.
// Without it, a target list containing many URLs from one site would hit that
// site with the full worker pool at once.
type originLimiter struct {
	limit int

	mu      sync.Mutex
	inUse   map[string]int
	waiters map[string][]chan struct{}
}

func newOriginLimiter(limit int) *originLimiter {
	if limit < 1 {
		limit = 1
	}

	return &originLimiter{
		limit:   limit,
		inUse:   make(map[string]int),
		waiters: make(map[string][]chan struct{}),
	}
}

// tryAcquire takes a slot for an origin without blocking.
func (l *originLimiter) tryAcquire(origin string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.inUse[origin] >= l.limit {
		return false
	}

	l.inUse[origin]++

	return true
}

// release returns a slot and wakes one waiter.
func (l *originLimiter) release(origin string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.inUse[origin] > 0 {
		l.inUse[origin]--
	}

	if l.inUse[origin] == 0 {
		delete(l.inUse, origin)
	}

	if queue := l.waiters[origin]; len(queue) > 0 {
		close(queue[0])

		if len(queue) == 1 {
			delete(l.waiters, origin)
		} else {
			l.waiters[origin] = queue[1:]
		}
	}
}
