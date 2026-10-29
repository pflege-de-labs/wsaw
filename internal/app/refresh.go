package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	deploybrowser "github.com/pflege-de-labs/wsaw/deploy/browser"
	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// This file is the browser image refresh (Story 6.13): on a schedule the
// operator opted into, build or pull a newer browser image, check it, and
// switch the pool to it between scans.
//
// It is the one place wsaw fetches a browser without being asked at that
// moment, which Tenet 14 allows only in this shape: opt-in, off by default,
// beside the scheduler and never on a scan's path, and never leaving the
// daemon without a browser it has checked. Every outcome — switched,
// unchanged, failed, skipped — is logged, counted and recorded.

// refreshTimeout bounds one refresh. A cold build downloads a base image and
// Chromium, so it is generous, as `wsaw browser build`'s is.
const refreshTimeout = 30 * time.Minute

// RefreshSchedule is the browser image refresh in force.
type RefreshSchedule struct {
	Mode     string
	Interval time.Duration
	// Image is browser.container.image: the tag a pull refreshes, and for a
	// build an image that pins the browser and stops the refresh.
	Image string
	Keep  KeepRule
}

// RefreshScheduleFor reads the refresh out of configuration, so a reload can
// hand the running loop a new one (AC11).
func RefreshScheduleFor(cfg *config.Config) RefreshSchedule {
	ct := cfg.Browser.Container

	return RefreshSchedule{
		Mode:     ct.RefreshMode(),
		Interval: ct.RefreshEvery(),
		Image:    ct.Image,
		Keep:     KeepRuleFor(ct),
	}
}

// refreshRuntime is what a refresh asks of a container runtime, declared
// where it is consumed so the refresh is tested against a fake.
type refreshRuntime interface {
	imageRuntime
	BuildImage(ctx context.Context, opts container.BuildOptions) (string, error)
	Pull(ctx context.Context, image string, output io.Writer) error
	Tag(ctx context.Context, image, name string) error
	BrowserVersion(ctx context.Context, image string) (string, error)
	ImageKeepsSandbox(ctx context.Context, image string) (bool, error)
	String() string
}

// RefreshLoop records which browser image is in service, then refreshes it on
// its schedule until ctx ends. A schedule received on schedules replaces the
// one in force, which is how a reload applies the refresh settings; one that
// turns the refresh off cancels a refresh in progress (AC6, AC11).
func (a *App) RefreshLoop(ctx context.Context, schedules <-chan RefreshSchedule) {
	if a.Runtime == nil {
		if RefreshScheduleFor(a.Config).Mode != config.RefreshOff {
			a.Logger.Warn("browser.container.refresh is ignored: the browser does not run in a container",
				"runtime", a.BrowserRuntimeName())
		}

		return
	}

	a.refreshLoop(ctx, a.Runtime, schedules, systemClock())
}

func (a *App) refreshLoop(ctx context.Context, rt refreshRuntime, schedules <-chan RefreshSchedule, clock maintenanceClock) {
	r := &refresher{app: a, rt: rt, clock: clock}
	sched := RefreshScheduleFor(a.Config)

	// Read before the startup inventory replaces it: a browser other than the
	// one last in service is one the results are about to change with.
	if prev := InServiceImage(ctx, a.Store); prev.Image != "" && differentBrowser(prev, a.Pool.Identity()) {
		a.auditBrowser(store.AuditBrowserChanged, prev, a.Pool.Identity(),
			"found at startup: the daemon started on another browser than the one last in service")
	}

	// The storage page shows the image in service whether or not a refresh
	// ever runs (AC13), and the prune command reads it from here (AC9).
	r.recordInventory(ctx, store.TriggerStartup, clock.now(), sched.Keep)

	next := r.firstDue(ctx, sched)

	for {
		var fire <-chan time.Time

		stop := func() {}
		if !next.IsZero() {
			fire, stop = clock.timer(next.Sub(clock.now()))
		}

		select {
		case <-ctx.Done():
			stop()

			return

		case s := <-schedules:
			stop()

			if s != sched {
				sched = s
				next = r.firstDue(ctx, sched)
			}

		case <-fire:
			if s, changed := r.run(ctx, sched, schedules); changed {
				sched = s
			}

			next = time.Time{}
			if sched.Mode != config.RefreshOff {
				next = clock.now().Add(sched.Interval)
			}
		}
	}
}

// refresher is the loop's state, touched only by the loop's goroutine and the
// one refresh it runs at a time.
type refresher struct {
	app   *App
	rt    refreshRuntime
	clock maintenanceClock
}

// firstDue is when the next refresh falls due under sched: one interval after
// the last recorded refresh, and never sooner than the startup delay, so a
// daemon restarted in a loop does not rebuild on every start. Zero means none.
func (r *refresher) firstDue(ctx context.Context, sched RefreshSchedule) time.Time {
	if sched.Mode == config.RefreshOff {
		r.app.Logger.Info("the browser image is not refreshed (browser.container.refresh: off); it changes only when the operator changes it")

		return time.Time{}
	}

	soonest := r.clock.now().Add(sweepStartupDelay + r.clock.jitter(sweepStartupJitter))

	last, found := lastBrowserImagesRun(ctx, r.store(), store.TriggerRefresh)
	due := sweepDue(last.FinishedAt, found, soonest, sched.Interval)

	r.app.Logger.Info("scheduled the next browser image refresh",
		"mode", sched.Mode, "next", due.Format(time.RFC3339), "interval", sched.Interval.String(),
		"keep", sched.Keep.String())

	return due
}

// run performs one refresh in a goroutine of its own, so the loop can go on
// reading schedules: one that turns the refresh off cancels it. It returns the
// newest schedule received meanwhile. At most one refresh runs at a time, by
// construction: the next is not armed until this one returns.
func (r *refresher) run(ctx context.Context, sched RefreshSchedule, schedules <-chan RefreshSchedule) (RefreshSchedule, bool) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		r.refreshOnce(runCtx, sched)
	}()

	latest, changed := sched, false

	for {
		select {
		case <-done:
			return latest, changed

		case s := <-schedules:
			latest, changed = s, true

			if s.Mode == config.RefreshOff {
				r.app.Logger.Info("browser image refresh turned off by a reload; cancelling the refresh in progress")
				cancel()
			}
		}
	}
}

// refreshOnce makes a new image, switches to it if it is a different browser,
// prunes after a switch, and records the inventory either way.
func (r *refresher) refreshOnce(ctx context.Context, sched RefreshSchedule) {
	started := r.clock.now()

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	outcome, err := r.refreshImage(ctx, sched, started)

	r.app.Metrics.BrowserImageRefreshed(outcome, r.clock.now())

	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		// Shutdown, or a reload that turned the refresh off. Nothing was
		// switched, and an inventory now would only hold the shutdown up:
		// the next start schedules from the last refresh that finished.
		r.app.Logger.Info("browser image refresh cancelled; the image in service is unchanged", "mode", sched.Mode)

		return
	case err != nil:
		// Error, not Warn: the daemon keeps working on the image it has, but
		// a refresh that keeps failing leaves it on that image indefinitely,
		// which is what someone has to look at.
		r.app.Logger.Error("browser image refresh failed; the image in service is unchanged",
			"mode", sched.Mode, "image", r.app.Pool.Identity().Image, "error", err)
	}

	var prune *store.BrowserPruneStats

	var pruneErr error

	if outcome == metrics.RefreshSwitched {
		// Not cancelled with the refresh: the switch has happened, and what
		// it superseded is cleaned up whatever happens to the run's budget.
		stats, err := r.prune(context.WithoutCancel(ctx), sched.Keep)
		prune, pruneErr = &stats, err
	}

	r.recordInventoryWithPrune(context.WithoutCancel(ctx), store.TriggerRefresh, started, err, sched.Keep, prune, pruneErr)
}

// refreshImage makes and checks a new image and switches to it if it is a
// different browser. The outcome is one of the metrics.Refresh* values.
func (r *refresher) refreshImage(ctx context.Context, sched RefreshSchedule, at time.Time) (string, error) {
	if sched.Mode == config.RefreshBuild && sched.Image != "" {
		r.app.Logger.Info("browser.container.image pins the browser image; the refresh builds nothing",
			"image", sched.Image)

		return metrics.RefreshSkipped, nil
	}

	name, err := r.make(ctx, sched, at)
	if err != nil {
		return metrics.RefreshFailed, err
	}

	version, img, err := r.check(ctx, name)
	if err != nil {
		r.discard(ctx, name)

		return metrics.RefreshFailed, err
	}

	current := r.app.Pool.Identity()

	if sameChromium(current.Version, version) && (sched.Mode == config.RefreshBuild || img.ID == current.ImageID) {
		// The same browser under a new name: switching would only mark every
		// comparison across it for nothing (AC7).
		r.discard(ctx, name)
		r.app.Logger.Info("browser image refresh found nothing newer; the image in service is unchanged",
			"mode", sched.Mode, "image", current.Image, "browser", current.Version)

		return metrics.RefreshUnchanged, nil
	}

	r.app.switchBrowser(name, version, img.ID)
	r.app.auditBrowser(store.AuditBrowserSwitched, current,
		browser.Identity{Version: version, Image: name, ImageID: img.ID},
		"switched by the "+sched.Mode+" refresh; scans in progress finished on the previous browser")

	r.app.Logger.Info("switched to a refreshed browser image; scans in progress finish on the previous one",
		"mode", sched.Mode,
		"previous_image", current.Image, "previous_id", current.ImageID, "previous_browser", current.Version,
		"image", name, "id", img.ID, "browser", version)

	return metrics.RefreshSwitched, nil
}

// outputTailBytes is how much of a build's or a pull's own output a failed
// refresh carries into its error.
const outputTailBytes = 2048

// outputTail keeps the end of what the runtime printed, so a refresh that
// failed says why in the log line someone has to act on, rather than only
// that the runtime exited non-zero. os/exec calls Write from one goroutine at
// a time when stdout and stderr share a writer.
type outputTail struct{ buf []byte }

func (t *outputTail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - outputTailBytes; over > 0 {
		t.buf = t.buf[over:]
	}

	return len(p), nil
}

func (t *outputTail) wrap(err error) error {
	if err == nil || len(t.buf) == 0 {
		return err
	}

	return fmt.Errorf("%w; the runtime's output ends: %s", err, strings.TrimSpace(string(t.buf)))
}

// make builds or pulls the new image and returns the refresh tag it carries.
func (r *refresher) make(ctx context.Context, sched RefreshSchedule, at time.Time) (string, error) {
	var out outputTail

	switch sched.Mode {
	case config.RefreshBuild:
		name := container.BuildRefreshTag(at)

		// The daemon's /tmp may be a PrivateTmp that rootless podman cannot see.
		dir, err := stateDir(r.app.Config)
		if err != nil {
			return "", err
		}

		_, err = r.rt.BuildImage(ctx, container.BuildOptions{
			Context:         deploybrowser.Context,
			Base:            deploybrowser.BaseAlpine,
			ChromiumVersion: deploybrowser.ChromiumVersion(deploybrowser.BaseAlpine),
			Output:          &out,
			Floor:           true,
			Tag:             name,
			TempDir:         dir,
		})
		if err != nil {
			return "", out.wrap(err)
		}

		return name, nil

	case config.RefreshPull:
		if err := r.rt.Pull(ctx, sched.Image, &out); err != nil {
			return "", out.wrap(err)
		}

		// Named for the refresh, so the image in service keeps a name of its
		// own when the next pull moves the tag.
		name := container.PullRefreshTag(sched.Image, at)
		if err := r.rt.Tag(ctx, sched.Image, name); err != nil {
			return "", err
		}

		return name, nil

	default:
		return "", fmt.Errorf("unknown refresh mode %q", sched.Mode)
	}
}

// check is the startup check, run on the new image before anything uses it
// (AC4): it must run and report a browser version, and declare that it keeps
// Chrome's sandbox.
func (r *refresher) check(ctx context.Context, name string) (string, container.Image, error) {
	version, err := r.rt.BrowserVersion(ctx, name)
	if err != nil {
		return "", container.Image{}, fmt.Errorf("the refreshed image does not run: %w", err)
	}

	keeps, err := r.rt.ImageKeepsSandbox(ctx, name)
	if err != nil {
		return "", container.Image{}, err
	}

	if !keeps {
		return "", container.Image{}, fmt.Errorf("the refreshed image %s does not declare %s=%s",
			name, container.LabelSandbox, container.SandboxEnabled)
	}

	img, err := r.rt.InspectImage(ctx, name)
	if err != nil {
		return "", container.Image{}, err
	}

	return version, img, nil
}

// discard removes a new image the refresh will not use. Failing to is a
// leftover a later prune collects, not a reason to fail the refresh.
func (r *refresher) discard(ctx context.Context, name string) {
	if err := r.rt.RemoveImage(context.WithoutCancel(ctx), name); err != nil {
		r.app.Logger.Warn("could not remove a browser image the refresh did not use; a later prune will",
			"image", name, "error", err)
	}
}

// prune applies the keep rule after a switch.
func (r *refresher) prune(ctx context.Context, rule KeepRule) (store.BrowserPruneStats, error) {
	b := r.images(rule)

	plan, err := b.Plan(ctx)
	if err != nil {
		r.app.Logger.Warn("browser images were not pruned", "error", err)

		return store.BrowserPruneStats{Rule: rule.String()}, err
	}

	stats, err := b.Prune(ctx, plan)

	if len(stats.Removed) > 0 || err != nil {
		r.app.Logger.Info("pruned browser images",
			"removed", len(stats.Removed), "bytes_freed", stats.BytesFreed, "failed", len(stats.Failed), "rule", rule.String())
	}

	if err != nil {
		r.app.Logger.Warn("some browser images could not be removed", "error", err, "failures", stats.Failed)
	}

	return stats, err
}

// recordInventory records the images as they are now. It is the startup
// record; a refresh records its own with recordInventoryWithPrune.
func (r *refresher) recordInventory(ctx context.Context, trigger string, at time.Time, rule KeepRule) {
	inv := r.recordInventoryWithPrune(ctx, trigger, at, nil, rule, nil, nil)
	r.warnUnrecorded(inv, rule)
}

// recordInventoryWithPrune records the images as they are now, the refresh
// error if there was one, and a prune's receipt when there was a prune. A
// refresh that failed is recorded too: the schedule is kept by these records,
// and a refresh that failed must not be retried on every restart.
func (r *refresher) recordInventoryWithPrune(ctx context.Context, trigger string, at time.Time, runErr error,
	rule KeepRule, prune *store.BrowserPruneStats, pruneErr error,
) store.BrowserImageInventory {
	b := r.images(rule)

	inv, err := b.Plan(ctx)
	if err != nil && !errors.Is(err, ErrReferencesUnknown) {
		r.app.Logger.Warn("could not take an inventory of the browser images", "error", err)
	}

	if err := b.Record(ctx, trigger, at, inv, errors.Join(runErr, err), prune, pruneErr); err != nil {
		r.app.Logger.Warn("could not record the browser image inventory", "error", err)
	}

	return inv
}

// warnUnrecorded says, once per start, that results stored before image IDs
// were recorded protect no image (AC9).
func (r *refresher) warnUnrecorded(inv store.BrowserImageInventory, rule KeepRule) {
	if rule.Referenced && inv.UnrecordedScans > 0 {
		r.app.Logger.Warn("some stored results do not record which browser image rendered them, so they keep no image",
			"results", inv.UnrecordedScans)
	}
}

func (r *refresher) images(rule KeepRule) BrowserImages {
	return BrowserImages{
		Runtime:     r.rt,
		Store:       r.store(),
		Rule:        rule,
		InService:   r.app.Pool.Identity(),
		Pinned:      r.app.Config.Browser.Container.Image,
		RuntimeName: r.rt.String(),
	}
}

func (r *refresher) store() browserImageStore {
	bs, ok := r.app.Store.(browserImageStore)
	if !ok {
		return nil
	}

	return bs
}

// differentBrowser reports whether two identities name different browsers:
// another version, or another image ID where both are known. A new name for
// the same image is not one.
func differentBrowser(a, b browser.Identity) bool {
	if a.Version != b.Version {
		return true
	}

	return a.ImageID != "" && b.ImageID != "" && a.ImageID != b.ImageID
}

// auditBrowser records a change of browser in the audit log (Story 6.13,
// AC16), beside the approvals, because both change what the findings that
// follow mean. A log that cannot take it is a warning, never a reason to
// undo the change.
func (a *App) auditBrowser(action string, before, after browser.Identity, note string) {
	if a.Store == nil {
		return
	}

	err := a.Store.RecordAudit(store.AuditEntry{
		Actor:   "wsaw",
		Action:  action,
		Subject: after.Image,
		Note: fmt.Sprintf("%s → %s (from %s, now %s); %s",
			cmp.Or(before.Version, "unknown"), cmp.Or(after.Version, "unknown"),
			cmp.Or(before.Image, "no image"), cmp.Or(after.ImageID, "no image ID"), note),
	})
	if err != nil {
		a.Logger.Warn("could not record the change of browser in the audit log", "action", action, "error", err)
	}
}

// switchBrowser makes every browser launched from now on come from image
// (AC5). A browser already serving a scan finishes it.
func (a *App) switchBrowser(image, version, id string) {
	launcher := *a.launcher
	launcher.spec.Image = image

	a.Pool.Switch(&launcher, browser.Identity{Version: version, Image: image, ImageID: id})
	a.Metrics.SetBrowser(version, image)
}

// resumeImage is the image a refresh left in service, for a daemon starting
// up: without it a restart would go back to the configured image and undo the
// refresh. Only an image the refresh made from the current settings
// qualifies, and only while it is still there; anything else leaves image as
// it is.
func (a *App) resumeImage(ctx context.Context, rt *container.Runtime, image string) string {
	sched := RefreshScheduleFor(a.Config)

	switch {
	case sched.Mode == config.RefreshOff:
		return image
	case sched.Mode == config.RefreshBuild && sched.Image != "":
		return image
	}

	inService := InServiceImage(ctx, a.Store)
	if !container.RefreshTagFrom(inService.Image, sched.Mode == config.RefreshPull, sched.Image) {
		return image
	}

	if _, err := rt.InspectImage(ctx, inService.Image); err != nil {
		a.Logger.Warn("the browser image the last refresh left in service is gone; starting from the configured one",
			"refreshed", inService.Image, "image", image, "error", err)

		return image
	}

	return inService.Image
}
