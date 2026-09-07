package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/retry"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// Scanner is the scanning capability the daemon drives. It is an interface so
// the scheduler can be tested without a browser (Tenet 13).
type Scanner interface {
	Scan(ctx context.Context, target config.Resolved, mode model.ConsentMode) (scanner.Outcome, error)
}

// Sink receives the outcome of every scan: change events for notification,
// results for export.
type Sink interface {
	Publish(ctx context.Context, out scanner.Outcome)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, out scanner.Outcome)

// Publish implements Sink.
func (f SinkFunc) Publish(ctx context.Context, out scanner.Outcome) { f(ctx, out) }

// Options configures the daemon.
type Options struct {
	Concurrency          int
	PerOriginConcurrency int
	CatchUp              bool
	ShutdownGrace        time.Duration

	// FlapWindow collapses a change and its inverse inside this window.
	FlapWindow time.Duration

	// OnRetry and OnRetriesExhausted report a scan that had to be tried
	// again, and one that ran out of attempts. A target that only works on
	// the third try is a finding of its own, so it has to be countable even
	// though the retry hid it from the notifier (Story 3.8, AC7).
	OnRetry            func(target string, mode model.ConsentMode, attempt int)
	OnRetriesExhausted func(target string, mode model.ConsentMode, attempts int)

	// Tick is how often the scheduler looks for due jobs. It bounds how late
	// a scan can be, not how often scans happen.
	Tick time.Duration

	Logger *slog.Logger

	// OnQueueDepth reports the number of jobs waiting, for metrics.
	OnQueueDepth func(int)
}

func (o *Options) withDefaults() Options {
	out := *o

	if out.Concurrency < 1 {
		out.Concurrency = 1
	}

	if out.PerOriginConcurrency < 1 {
		out.PerOriginConcurrency = 1
	}

	if out.ShutdownGrace <= 0 {
		out.ShutdownGrace = 30 * time.Second
	}

	if out.Tick <= 0 {
		out.Tick = time.Second
	}

	if out.Logger == nil {
		out.Logger = slog.Default()
	}

	return out
}

// Daemon schedules and runs scans.
type Daemon struct {
	opts    Options
	scanner Scanner
	sink    Sink

	origins *originLimiter

	mu   sync.Mutex
	jobs []*job

	flap *diff.FlapSuppressor

	// reload carries a new target list to the running loop.
	reload chan []config.Resolved
}

// New creates a Daemon.
func New(s Scanner, sink Sink, targets []config.Resolved, opts Options) (*Daemon, error) {
	if s == nil {
		return nil, errors.New("daemon: scanner is required")
	}

	resolved := opts.withDefaults()

	jobs, err := buildJobs(targets, time.Now(), resolved.CatchUp)
	if err != nil {
		return nil, err
	}

	return &Daemon{
		opts:    resolved,
		scanner: s,
		sink:    sink,
		origins: newOriginLimiter(resolved.PerOriginConcurrency),
		jobs:    jobs,
		flap:    diff.NewFlapSuppressor(resolved.FlapWindow),
		reload:  make(chan []config.Resolved, 1),
	}, nil
}

// Reload replaces the target list. In-flight scans finish under the config
// they started with, which is what makes a reload safe to trigger at any
// time (Story 3.4).
func (d *Daemon) Reload(targets []config.Resolved) {
	select {
	case d.reload <- targets:
	default:
		// A reload is already queued; the newer list supersedes it.
		select {
		case <-d.reload:
		default:
		}

		d.reload <- targets
	}
}

// Jobs reports the current schedule, for the API and for tests.
func (d *Daemon) Jobs() []JobStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]JobStatus, 0, len(d.jobs))

	for _, j := range d.jobs {
		out = append(out, JobStatus{
			Target:  j.target.Name,
			Mode:    j.mode,
			NextRun: j.next,
			LastRun: j.lastRun,
		})
	}

	return out
}

// JobStatus is one scheduled job's state.
type JobStatus struct {
	Target  string            `json:"target"`
	Mode    model.ConsentMode `json:"consentMode"`
	NextRun time.Time         `json:"nextRun"`
	LastRun time.Time         `json:"lastRun,omitempty"`
}

// Run drives the scheduler until ctx is cancelled.
//
// On cancellation, in-flight scans are given the shutdown grace period to
// finish before being cancelled themselves, so a restart does not routinely
// discard work that was nearly done (Story 3.5).
func (d *Daemon) Run(ctx context.Context) error {
	// scanCtx is cancelled only after the grace period, so shutdown is
	// graceful rather than abrupt.
	scanCtx, cancelScans := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelScans()

	sem := make(chan struct{}, d.opts.Concurrency)

	var wg sync.WaitGroup

	ticker := time.NewTicker(d.opts.Tick)
	defer ticker.Stop()

	d.opts.Logger.Info("scheduler started",
		"jobs", len(d.Jobs()),
		"concurrency", d.opts.Concurrency,
		"per_origin_concurrency", d.opts.PerOriginConcurrency,
	)

	for {
		select {
		case <-ctx.Done():
			return d.shutdown(&wg, cancelScans)

		case targets := <-d.reload:
			d.applyReload(targets)

		case now := <-ticker.C:
			d.dispatch(scanCtx, now, sem, &wg)
		}
	}
}

func (d *Daemon) applyReload(targets []config.Resolved) {
	jobs, err := buildJobs(targets, time.Now(), d.opts.CatchUp)
	if err != nil {
		// A reload that cannot be applied leaves the previous schedule
		// running; a watcher must not stop watching because of a bad edit.
		d.opts.Logger.Error("reload rejected, keeping the previous schedule", "error", err)

		return
	}

	d.mu.Lock()

	// Preserve last-run times across a reload, so a reload cannot be used —
	// accidentally or otherwise — to bypass the minimum scan interval.
	previous := make(map[string]time.Time, len(d.jobs))
	for _, j := range d.jobs {
		previous[j.key()] = j.lastRun
	}

	for _, j := range jobs {
		if last, ok := previous[j.key()]; ok {
			j.lastRun = last
		}
	}

	d.jobs = jobs
	d.mu.Unlock()

	d.opts.Logger.Info("configuration reloaded", "jobs", len(jobs))
}

// dispatch starts every job that is due and can get a slot.
func (d *Daemon) dispatch(ctx context.Context, now time.Time, sem chan struct{}, wg *sync.WaitGroup) {
	due := d.dueJobs(now)

	if d.opts.OnQueueDepth != nil {
		d.opts.OnQueueDepth(len(due))
	}

	for _, j := range due {
		origin := originOf(j.target.URL)

		// Both limits are non-blocking here: a job that cannot start now is
		// simply retried on the next tick, which keeps the scheduler
		// responsive to shutdown and reload.
		select {
		case sem <- struct{}{}:
		default:
			return
		}

		if !d.origins.tryAcquire(origin) {
			<-sem

			continue
		}

		attempt, previous := d.markStarted(j, now)

		wg.Add(1)

		go func(j *job, attempt int, previous string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer d.origins.release(origin)

			d.runJob(ctx, j, attempt, previous)
		}(j, attempt, previous)
	}
}

func (d *Daemon) dueJobs(now time.Time) []*job {
	d.mu.Lock()
	defer d.mu.Unlock()

	var due []*job

	for _, j := range d.jobs {
		if j.dueAt(now) {
			due = append(due, j)
		}
	}

	return due
}

// markStarted advances the schedule before the scan runs, so a long scan does
// not queue up duplicates of itself on every tick.
//
// It returns the attempt this run is, read under the same lock that advances
// the schedule so a concurrent reload cannot change it underneath.
func (d *Daemon) markStarted(j *job, now time.Time) (attempt int, previous string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	j.lastRun = now
	j.advance(now)

	// Cleared here: the next tick must not treat this run as still pending a
	// retry, and whether another one follows is decided when it finishes.
	j.retrying = false

	if j.attempt < 1 {
		j.attempt = 1
	}

	return j.attempt, j.prevError
}

func (d *Daemon) runJob(ctx context.Context, j *job, attempt int, previous string) {
	policy := j.target.Retry

	scanCtx := scanner.WithAttempt(ctx, attempt, policy.MaxAttempts(), previous)

	out, err := d.scanner.Scan(scanCtx, j.target, j.mode)
	if err != nil {
		d.opts.Logger.Warn("scan reported an error",
			"target", j.target.Name, "consent_mode", string(j.mode), "error", err)
	}

	// A scan that produced no usable observation gets another attempt, and its
	// result is not published: a failure a retry fixes must not page anyone
	// (Story 3.8, AC7). It is still stored — the scanner did that already —
	// so the failure remains in the history either way (Tenet 5).
	if d.considerRetry(ctx, j, out, err, attempt, policy) {
		return
	}

	if out.Result == nil {
		return
	}

	d.publish(ctx, out)
}

// considerRetry requeues the job when another attempt is warranted, and
// reports whether it did.
func (d *Daemon) considerRetry(
	ctx context.Context,
	j *job,
	out scanner.Outcome,
	scanErr error,
	attempt int,
	policy retry.Policy,
) bool {
	if !policy.Retryable(out.Result) {
		d.finishRetries(j)

		return false
	}

	if attempt >= policy.MaxAttempts() {
		if policy.Enabled() {
			d.opts.Logger.Warn("scan failed on every attempt",
				"target", j.target.Name, "consent_mode", string(j.mode),
				"attempts", policy.MaxAttempts())

			if d.opts.OnRetriesExhausted != nil {
				d.opts.OnRetriesExhausted(j.target.Name, j.mode, policy.MaxAttempts())
			}
		}

		d.finishRetries(j)

		return false
	}

	// Cancellation is not a reason to retry: the daemon is going away, and a
	// requeue would either be dropped or delay the shutdown it was asked to
	// perform.
	if ctx.Err() != nil {
		d.finishRetries(j)

		return false
	}

	next := attempt + 1
	delay := policy.Delay(next, j.key())

	d.mu.Lock()
	j.scheduleRetry(time.Now(), delay, retryReason(out, scanErr))
	d.mu.Unlock()

	d.opts.Logger.Info("scan produced no usable observation, retrying",
		"target", j.target.Name,
		"consent_mode", string(j.mode),
		"attempt", attempt,
		"attempts", policy.MaxAttempts(),
		"next_attempt_in", delay.String(),
		"reason", retryReason(out, scanErr),
	)

	if d.opts.OnRetry != nil {
		d.opts.OnRetry(j.target.Name, j.mode, attempt)
	}

	return true
}

func (d *Daemon) finishRetries(j *job) {
	d.mu.Lock()
	defer d.mu.Unlock()

	j.resetRetries()
}

// retryReason describes why an attempt did not produce an observation, for
// the next attempt's result to carry.
func retryReason(out scanner.Outcome, scanErr error) string {
	switch {
	case out.Result == nil && scanErr != nil:
		return scanErr.Error()
	case out.Result == nil:
		return "the scan produced no result"
	case out.Result.Error != "":
		return string(out.Result.Termination) + ": " + out.Result.Error
	default:
		return string(out.Result.Termination)
	}
}

// publish applies flap suppression and hands the outcome to the sink.
func (d *Daemon) publish(ctx context.Context, out scanner.Outcome) {
	if d.sink == nil {
		return
	}

	if out.Diff != nil && len(out.Diff.Changes) > 0 {
		emit, suppressed := d.flap.Filter(time.Now(), out.Diff.Changes)
		out.Diff.Changes = emit
		out.Diff.Suppressed += suppressed
	}

	d.sink.Publish(ctx, out)
}

// shutdown waits for in-flight scans, then cancels them if they overrun.
func (d *Daemon) shutdown(wg *sync.WaitGroup, cancelScans context.CancelFunc) error {
	d.opts.Logger.Info("shutting down, waiting for in-flight scans", "grace", d.opts.ShutdownGrace.String())

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(d.opts.ShutdownGrace)
	defer timer.Stop()

	select {
	case <-done:
		d.opts.Logger.Info("all scans finished")

	case <-timer.C:
		d.opts.Logger.Warn("grace period expired, cancelling in-flight scans")
		cancelScans()

		// Even after cancelling, wait for the goroutines to return so no
		// browser or temp directory outlives the process (NFR §2).
		<-done
	}

	// The flap suppressor is pruned so a long-running process does not carry
	// stale identities forward.
	d.flap.Prune(time.Now())

	return nil
}

// originOf reduces a URL to scheme://host, the unit per-origin limiting
// applies to.
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}
