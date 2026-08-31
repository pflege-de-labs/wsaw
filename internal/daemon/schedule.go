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

	"github.com/martint17r/wsaw/internal/config"
	"github.com/martint17r/wsaw/internal/model"
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
}

func (j *job) key() string {
	return j.target.Name + "/" + string(j.mode)
}

// schedule builds the job list from resolved targets.
func buildJobs(targets []config.Resolved, now time.Time, catchUp bool) ([]*job, error) {
	var jobs []*job

	for _, t := range targets {
		for _, mode := range t.ConsentModes {
			j := &job{target: t, mode: mode, interval: t.Interval}

			if t.Cron != "" {
				sched, err := cron.ParseStandard(t.Cron)
				if err != nil {
					return nil, fmt.Errorf("target %q: cron %q: %w", t.Name, t.Cron, err)
				}

				j.schedule = sched
			}

			j.next = j.firstRun(now, catchUp)
			jobs = append(jobs, j)
		}
	}

	return jobs, nil
}

// firstRun decides when a job runs for the first time after startup.
//
// Without catch-up the first run is soon but jittered, so a restart does not
// fire every target at once — a restart storm against a shared origin is
// exactly the behaviour that gets a scanner blocked.
func (j *job) firstRun(now time.Time, catchUp bool) time.Time {
	if j.schedule != nil {
		return j.schedule.Next(now)
	}

	if catchUp {
		return now
	}

	return now.Add(j.startupDelay())
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

	return time.Duration(uint64(h.Sum32()) % uint64(window))
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
	if min := j.target.MinInterval; min > 0 && j.next.Sub(now) < min {
		j.next = now.Add(min)
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

	return time.Duration(uint64(h.Sum32()) % uint64(j.target.Jitter))
}

// dueAt reports whether the job should run, and enforces the minimum interval
// as a hard floor even if the schedule says otherwise.
func (j *job) dueAt(now time.Time) bool {
	if now.Before(j.next) {
		return false
	}

	if min := j.target.MinInterval; min > 0 && !j.lastRun.IsZero() && now.Sub(j.lastRun) < min {
		return false
	}

	return true
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
