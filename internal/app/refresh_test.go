package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/container"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The browser image refresh (Story 6.13, AC14): against a runtime faked in Go
// and a clock the test moves. No network, no Chrome, no Podman or Docker, and
// nothing waits on a real timer.

// fakeImageRuntime is a container runtime holding images in a map.
type fakeImageRuntime struct {
	mu sync.Mutex

	images  map[string]container.Image // by name
	version map[string]string          // by image ID
	sandbox bool

	// What the next build or pull produces.
	nextID, nextVersion string

	// block makes a build wait for its context to end, after saying on
	// building that it has started.
	block    bool
	building chan struct{}
	// failWith makes a build print this and fail.
	failWith string
	// refuse names images whose removal the runtime refuses, as it does for
	// one a container is running.
	refuse map[string]bool

	builds, pulls, removed []string
	buildOpts              []container.BuildOptions
}

func newFakeImageRuntime() *fakeImageRuntime {
	return &fakeImageRuntime{
		images:   map[string]container.Image{},
		version:  map[string]string{},
		sandbox:  true,
		refuse:   map[string]bool{},
		building: make(chan struct{}, 1),
	}
}

func (f *fakeImageRuntime) add(name, id, version string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.images[name] = container.Image{Name: name, ID: id, Size: size}
	f.version[id] = version
}

func (f *fakeImageRuntime) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.images[name]

	return ok
}

func (f *fakeImageRuntime) String() string { return "podman test" }

func (f *fakeImageRuntime) RefreshImages(context.Context) ([]container.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []container.Image

	for name, img := range f.images {
		if _, ok := container.ParseRefreshTag(name); ok {
			out = append(out, img)
		}
	}

	slices.SortFunc(out, func(a, b container.Image) int { return strings.Compare(b.Name, a.Name) })

	return out, nil
}

func (f *fakeImageRuntime) InspectImage(_ context.Context, name string) (container.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	img, ok := f.images[name]
	if !ok {
		return container.Image{}, fmt.Errorf("%w: %s", container.ErrImageMissing, name)
	}

	return img, nil
}

func (f *fakeImageRuntime) RemoveImage(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.refuse[name] {
		return fmt.Errorf("removing browser image %s: image is in use by a container", name)
	}

	delete(f.images, name)
	f.removed = append(f.removed, name)

	return nil
}

func (f *fakeImageRuntime) BuildImage(ctx context.Context, opts container.BuildOptions) (string, error) {
	f.mu.Lock()
	f.builds = append(f.builds, opts.Tag)
	f.buildOpts = append(f.buildOpts, opts)
	block := f.block
	f.mu.Unlock()

	if f.failWith != "" {
		_, _ = fmt.Fprintln(opts.Output, "STEP 2/6: RUN apk add chromium-headless-shell>=152")
		_, _ = fmt.Fprintln(opts.Output, f.failWith)

		return "", errors.New("podman build: exit status 1")
	}

	if block {
		f.building <- struct{}{}

		<-ctx.Done()

		return "", fmt.Errorf("building %s: %w", opts.Tag, ctx.Err())
	}

	f.add(opts.Tag, f.nextID, f.nextVersion, 500)

	return opts.Tag, nil
}

func (f *fakeImageRuntime) Pull(_ context.Context, image string, _ io.Writer) error {
	f.mu.Lock()
	f.pulls = append(f.pulls, image)
	f.mu.Unlock()

	f.add(image, f.nextID, f.nextVersion, 400)

	return nil
}

func (f *fakeImageRuntime) Tag(_ context.Context, image, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	img, ok := f.images[image]
	if !ok {
		return fmt.Errorf("no image %s", image)
	}

	img.Name = name
	f.images[name] = img

	return nil
}

func (f *fakeImageRuntime) BrowserVersion(_ context.Context, image string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	img, ok := f.images[image]
	if !ok {
		return "", fmt.Errorf("%w: %s", container.ErrImageMissing, image)
	}

	return f.version[img.ID], nil
}

func (f *fakeImageRuntime) ImageKeepsSandbox(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sandbox, nil
}

// fakeImageStore is the store's half: references the test sets, and the
// records the refresh writes.
type fakeImageStore struct {
	store.Store

	mu sync.Mutex

	refs    store.BrowserImageReferences
	refsErr error
	runs    []store.MaintenanceRun // newest first
	nextID  int64
	audit   []store.AuditEntry
}

func (s *fakeImageStore) RecordAudit(e store.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.audit = append(s.audit, e)

	return nil
}

func (s *fakeImageStore) audited() []store.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.audit)
}

func (s *fakeImageStore) BrowserImageReferences(context.Context) (store.BrowserImageReferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.refs, s.refsErr
}

func (s *fakeImageStore) MaintenanceRuns(_ context.Context, kind string, _ int) ([]store.MaintenanceRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []store.MaintenanceRun

	for _, r := range s.runs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}

	return out, nil
}

func (s *fakeImageStore) record(kind, trigger string, at time.Time, stats any, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, mErr := json.Marshal(stats)
	if mErr != nil {
		return mErr
	}

	s.nextID++
	run := store.MaintenanceRun{ID: s.nextID, Kind: kind, Trigger: trigger, StartedAt: at, FinishedAt: at, Stats: raw}

	if err != nil {
		run.Error = err.Error()
	}

	s.runs = append([]store.MaintenanceRun{run}, s.runs...)

	return nil
}

func (s *fakeImageStore) RecordBrowserImages(
	_ context.Context, trigger string, at time.Time, inv store.BrowserImageInventory, err error,
) error {
	return s.record(store.MaintenanceKindBrowserImages, trigger, at, inv, err)
}

func (s *fakeImageStore) RecordBrowserPruneRun(
	_ context.Context, trigger string, at time.Time, stats store.BrowserPruneStats, err error,
) error {
	return s.record(store.MaintenanceKindBrowserPrune, trigger, at, stats, err)
}

// last returns the newest record of kind.
func (s *fakeImageStore) last(t *testing.T, kind string) store.MaintenanceRun {
	t.Helper()

	runs, _ := s.MaintenanceRuns(t.Context(), kind, 0)
	if len(runs) == 0 {
		t.Fatalf("no %s run was recorded", kind)
	}

	return runs[0]
}

const (
	oldBrowser = "Chromium 152.0.7977.82"
	newBrowser = "Chromium 153.0.8001.4"
)

// refreshApp is an App running on start, a refresh image already in the
// runtime, with mutate applied to its configuration.
func refreshApp(t *testing.T, mutate func(*config.Config)) (*App, *fakeImageRuntime, *fakeImageStore, *bytes.Buffer) {
	t.Helper()

	cfg := config.New()
	cfg.Browser.Container.Refresh = config.RefreshBuild

	if mutate != nil {
		mutate(cfg)
	}

	rt := newFakeImageRuntime()
	start := container.BuildRefreshTag(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	rt.add(start, "sha256:old", oldBrowser, 500)

	st := &fakeImageStore{refs: store.BrowserImageReferences{ByID: map[string]store.BrowserImageReference{}}}

	var logged bytes.Buffer

	identity := browser.Identity{Version: oldBrowser, Image: start, ImageID: "sha256:old"}

	a := &App{
		Config:   cfg,
		Logger:   slog.New(slog.NewTextHandler(&syncWriter{w: &logged}, nil)),
		Metrics:  metrics.New("test"),
		Store:    st,
		Pool:     browser.NewPool(browser.PoolOptions{Size: 1, Launch: browser.Options{Identity: identity}}),
		launcher: &containerLauncher{spec: container.Spec{Image: start, Memory: "1g"}},
	}

	t.Cleanup(func() { _ = a.Pool.Close() })

	return a, rt, st, &logged
}

func refresherFor(a *App, rt *fakeImageRuntime, clock *fakeClock) *refresher {
	return &refresher{app: a, rt: rt, clock: clock.clock()}
}

var refreshStart = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// TestABuildWithANewerChromiumSwitchesThePool is AC2, AC4, AC5 and AC10: the
// build takes a newer Chromium, passes the checks, becomes what every new
// browser starts from — with every other launch setting kept — and the
// switch is logged, counted and recorded.
func TestABuildWithANewerChromiumSwitchesThePool(t *testing.T) {
	t.Parallel()

	a, rt, st, logged := refreshApp(t, nil)
	rt.nextID, rt.nextVersion = "sha256:new", newBrowser

	clock := newFakeClock(refreshStart)
	refresherFor(a, rt, clock).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	want := container.BuildRefreshTag(refreshStart)

	if len(rt.buildOpts) != 1 || !rt.buildOpts[0].Floor || rt.buildOpts[0].Tag != want {
		t.Fatalf("builds = %+v, want one floor build tagged %s", rt.buildOpts, want)
	}

	if got := a.Pool.Identity(); got != (browser.Identity{Version: newBrowser, Image: want, ImageID: "sha256:new"}) {
		t.Errorf("the pool starts browsers as %+v after the switch", got)
	}

	if a.launcher.spec.Image == want {
		t.Error("the switch changed the startup launcher instead of making a new one")
	}

	if !strings.Contains(logged.String(), "switched to a refreshed browser image") {
		t.Errorf("the switch was not logged:\n%s", logged)
	}

	if line := gauge(t, a.Metrics, "wsaw_browser_image_refreshes_total"); !strings.Contains(line, `outcome="switched"`) {
		t.Errorf("refresh metric = %q", line)
	}

	if line := gauge(t, a.Metrics, "wsaw_browser_info"); !strings.Contains(line, "153.0.8001.4") {
		t.Errorf("browser info = %q", line)
	}

	run := st.last(t, store.MaintenanceKindBrowserImages)
	if run.Trigger != store.TriggerRefresh || run.Error != "" {
		t.Errorf("inventory record = %+v", run)
	}

	inv, err := run.BrowserImages()
	if err != nil {
		t.Fatal(err)
	}

	if inv.Images[0].Name != want || inv.Images[0].State != ImageInService || inv.Images[0].Chromium != newBrowser {
		t.Errorf("the inventory does not show the new image in service: %+v", inv.Images)
	}
}

// TestAPullSwitchesToANewDigestUnderItsOwnName is AC3: the configured tag is
// pulled, and the image in service is named for the refresh, so the next pull
// moving the tag does not move the browser.
func TestAPullSwitchesToANewDigestUnderItsOwnName(t *testing.T) {
	t.Parallel()

	const source = "ghcr.io/example/browser:stable"

	a, rt, _, _ := refreshApp(t, func(c *config.Config) {
		c.Browser.Container.Refresh = config.RefreshPull
		c.Browser.Container.Image = source
	})
	// The same Chromium in a new image is still a new image under pull.
	rt.nextID, rt.nextVersion = "sha256:pulled", oldBrowser

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	if !slices.Equal(rt.pulls, []string{source}) || len(rt.builds) != 0 {
		t.Fatalf("pulls = %v, builds = %v", rt.pulls, rt.builds)
	}

	want := container.PullRefreshTag(source, refreshStart)
	if got := a.Pool.Identity(); got.Image != want || got.ImageID != "sha256:pulled" {
		t.Errorf("the pool starts browsers from %+v, want %s", got, want)
	}
}

// TestNothingNewerChangesNothing is AC7: the same browser under a new name is
// not switched to, and the new name is removed.
func TestNothingNewerChangesNothing(t *testing.T) {
	t.Parallel()

	a, rt, _, _ := refreshApp(t, nil)
	before := a.Pool.Identity()
	rt.nextID, rt.nextVersion = "sha256:rebuilt", oldBrowser

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	if got := a.Pool.Identity(); got != before {
		t.Errorf("an unchanged browser was switched to: %+v", got)
	}

	if rt.has(container.BuildRefreshTag(refreshStart)) {
		t.Error("the rebuilt image of the same browser was kept")
	}

	if line := gauge(t, a.Metrics, "wsaw_browser_image_refreshes_total"); !strings.Contains(line, `outcome="unchanged"`) {
		t.Errorf("refresh metric = %q", line)
	}
}

// TestAnImageThatFailsTheCheckIsNeverUsed is AC4 and Tenet 8: the image
// in service stays, the failed one is removed, the failure is an error in the
// log, counted, and recorded so the schedule still advances.
func TestAnImageThatFailsTheCheckIsNeverUsed(t *testing.T) {
	t.Parallel()

	a, rt, st, logged := refreshApp(t, nil)
	before := a.Pool.Identity()
	rt.nextID, rt.nextVersion = "sha256:bad", newBrowser
	rt.sandbox = false

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	if got := a.Pool.Identity(); got != before {
		t.Errorf("an image without the sandbox label was switched to: %+v", got)
	}

	if rt.has(container.BuildRefreshTag(refreshStart)) {
		t.Error("the image that failed the check was left behind")
	}

	if !strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), container.LabelSandbox) {
		t.Errorf("the failure was not logged as an error naming the label:\n%s", logged)
	}

	if run := st.last(t, store.MaintenanceKindBrowserImages); run.Trigger != store.TriggerRefresh || run.Error == "" {
		t.Errorf("the failed refresh was not recorded with its error: %+v", run)
	}
}

// TestASwitchIsInTheAuditLog is AC16: a refresh that switched the pool says
// so in the audit log, naming both browsers; one that found nothing newer
// records nothing there.
func TestASwitchIsInTheAuditLog(t *testing.T) {
	t.Parallel()

	a, rt, st, _ := refreshApp(t, nil)
	rt.nextID, rt.nextVersion = "sha256:new", newBrowser

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	entries := st.audited()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %+v, want one", entries)
	}

	e := entries[0]
	if e.Action != store.AuditBrowserSwitched || e.Subject != container.BuildRefreshTag(refreshStart) ||
		!strings.Contains(e.Note, oldBrowser) || !strings.Contains(e.Note, newBrowser) {
		t.Errorf("audit entry = %+v", e)
	}

	same, rt2, st2, _ := refreshApp(t, nil)
	rt2.nextID, rt2.nextVersion = "sha256:rebuilt", oldBrowser

	refresherFor(same, rt2, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(same.Config))

	if got := st2.audited(); len(got) != 0 {
		t.Errorf("an unchanged refresh was audited: %+v", got)
	}
}

// TestStartingOnAnotherBrowserIsInTheAuditLog is AC16's other half: a daemon
// that starts on a different browser than the one last recorded in service —
// an operator changed the image and restarted — says so, and one that starts
// on the same browser does not.
func TestStartingOnAnotherBrowserIsInTheAuditLog(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		prev    store.BrowserImage
		audited bool
	}{
		"another version": {store.BrowserImage{Name: "localhost/wsaw-browser:151.0.1.1-r0", ID: "sha256:older",
			Chromium: "Chromium 151.0.1.1", State: ImageInService}, true},
		"same browser": {store.BrowserImage{Name: "localhost/wsaw-browser:build-20261001T000000Z", ID: "sha256:old",
			Chromium: oldBrowser, State: ImageInService}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			a, rt, st, _ := refreshApp(t, func(c *config.Config) { c.Browser.Container.Refresh = "" })

			inv := store.BrowserImageInventory{Images: []store.BrowserImage{tc.prev}}
			if err := st.RecordBrowserImages(t.Context(), store.TriggerStartup, refreshStart, inv, nil); err != nil {
				t.Fatal(err)
			}

			_, stop := runRefreshLoop(t, a, rt, newFakeClock(refreshStart))
			stop()

			entries := st.audited()
			if (len(entries) == 1) != tc.audited {
				t.Fatalf("audit entries = %+v, want audited=%v", entries, tc.audited)
			}

			if tc.audited && (entries[0].Action != store.AuditBrowserChanged || !strings.Contains(entries[0].Note, "Chromium 151.0.1.1")) {
				t.Errorf("audit entry = %+v", entries[0])
			}
		})
	}
}

// TestAFailedBuildSaysWhy: the error someone has to act on carries the end of
// the runtime's own output, not only its exit status.
func TestAFailedBuildSaysWhy(t *testing.T) {
	t.Parallel()

	a, rt, st, logged := refreshApp(t, nil)
	rt.failWith = "ERROR: unable to select packages: chromium-headless-shell (no such package)"

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	if !strings.Contains(logged.String(), "no such package") {
		t.Errorf("the log does not say why the build failed:\n%s", logged)
	}

	if run := st.last(t, store.MaintenanceKindBrowserImages); !strings.Contains(run.Error, "no such package") {
		t.Errorf("the record does not say why: %+v", run)
	}
}

// TestAPinnedImageIsNotRebuilt is AC9's last paragraph: in build mode a
// configured image pins the browser and the refresh builds nothing.
func TestAPinnedImageIsNotRebuilt(t *testing.T) {
	t.Parallel()

	a, rt, _, _ := refreshApp(t, func(c *config.Config) { c.Browser.Container.Image = "localhost/wsaw-browser:152.0.7977.82-r0" })

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	if len(rt.builds) != 0 {
		t.Errorf("a pinned browser was rebuilt: %v", rt.builds)
	}

	if line := gauge(t, a.Metrics, "wsaw_browser_image_refreshes_total"); !strings.Contains(line, `outcome="skipped"`) {
		t.Errorf("refresh metric = %q", line)
	}
}

// TestASwitchPrunesByTheConfiguredCount is AC9's count rule after a switch:
// the newest n refresh images stay, the image in service among them, and an
// image the refresh did not make is never touched.
func TestASwitchPrunesByTheConfiguredCount(t *testing.T) {
	t.Parallel()

	for _, keep := range []int{0, 1, 3} {
		t.Run(fmt.Sprint("keep ", keep), func(t *testing.T) {
			t.Parallel()

			a, rt, st, _ := refreshApp(t, func(c *config.Config) {
				if keep > 0 {
					c.Browser.Container.RefreshKeep = &keep
				}
			})

			for day := 2; day <= 5; day++ {
				name := container.BuildRefreshTag(time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC))
				rt.add(name, fmt.Sprint("sha256:day", day), oldBrowser, 500)
			}

			rt.add(container.DefaultImage, "sha256:operator", oldBrowser, 500)
			rt.nextID, rt.nextVersion = "sha256:new", newBrowser

			refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

			images, _ := rt.RefreshImages(t.Context())

			want := keep
			if keep == 0 {
				want = config.DefaultRefreshKeep
			}

			if len(images) != want {
				t.Errorf("%d refresh images left, want %d: %v", len(images), want, images)
			}

			if images[0].Name != container.BuildRefreshTag(refreshStart) {
				t.Errorf("the image in service was pruned: %v", images)
			}

			if !rt.has(container.DefaultImage) {
				t.Error("an image the refresh did not make was pruned")
			}

			stats, err := st.last(t, store.MaintenanceKindBrowserPrune).BrowserPruneStats()
			if err != nil || len(stats.Removed) != 6-want || stats.BytesFreed != int64(500*(6-want)) {
				t.Errorf("prune receipt = %+v, %v", stats, err)
			}
		})
	}
}

// TestTheReferencedRuleKeepsWhatStoredScansUsed is AC9's other rule: an
// image a stored scan names survives however old it is, one nobody names
// goes, and with the references unreadable nothing goes at all.
func TestTheReferencedRuleKeepsWhatStoredScansUsed(t *testing.T) {
	t.Parallel()

	referenced := true

	setup := func(t *testing.T) (*App, *fakeImageRuntime, *fakeImageStore) {
		t.Helper()

		a, rt, st, _ := refreshApp(t, func(c *config.Config) { c.Browser.Container.RefreshKeepReferenced = &referenced })

		rt.add(container.BuildRefreshTag(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), "sha256:used", oldBrowser, 500)
		rt.add(container.BuildRefreshTag(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)), "sha256:unused", oldBrowser, 500)
		rt.nextID, rt.nextVersion = "sha256:new", newBrowser

		st.refs = store.BrowserImageReferences{
			ByID:       map[string]store.BrowserImageReference{"sha256:used": {Scans: 3}},
			Unrecorded: 7,
		}

		return a, rt, st
	}

	t.Run("references read", func(t *testing.T) {
		t.Parallel()

		a, rt, _ := setup(t)
		refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

		if !rt.has(container.BuildRefreshTag(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))) {
			t.Error("an image three stored scans used was pruned")
		}

		if rt.has(container.BuildRefreshTag(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))) {
			t.Error("an image no stored scan used survived")
		}

		// The image the pool just left, which no stored scan names yet, is
		// not the newest and goes too.
		if rt.has(container.BuildRefreshTag(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))) {
			t.Error("an unreferenced older image survived")
		}
	})

	t.Run("references unreadable", func(t *testing.T) {
		t.Parallel()

		a, rt, st := setup(t)
		st.refsErr = errors.New("database is locked")

		refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

		if len(rt.removed) != 0 {
			t.Errorf("images were pruned without the references: %v", rt.removed)
		}

		run := st.last(t, store.MaintenanceKindBrowserPrune)
		if !strings.Contains(run.Error, "nothing is pruned") {
			t.Errorf("the receipt does not say why nothing was pruned: %+v", run)
		}
	})
}

// TestARefusedRemovalIsReportedAndTheRestGoOn is AC9 and AC12: the runtime's
// refusal — an image a container still runs — is recorded, not forced.
func TestARefusedRemovalIsReportedAndTheRestGoOn(t *testing.T) {
	t.Parallel()

	keep := 1

	a, rt, st, _ := refreshApp(t, func(c *config.Config) { c.Browser.Container.RefreshKeep = &keep })
	old := container.BuildRefreshTag(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	rt.refuse[old] = true
	rt.add(container.BuildRefreshTag(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), "sha256:older", oldBrowser, 500)
	rt.nextID, rt.nextVersion = "sha256:new", newBrowser

	refresherFor(a, rt, newFakeClock(refreshStart)).refreshOnce(t.Context(), RefreshScheduleFor(a.Config))

	run := st.last(t, store.MaintenanceKindBrowserPrune)

	stats, err := run.BrowserPruneStats()
	if err != nil || len(stats.Failed) != 1 || len(stats.Removed) != 1 || run.Error == "" {
		t.Errorf("receipt = %+v, stats = %+v", run, stats)
	}

	if !rt.has(old) {
		t.Error("an image the runtime refused to remove is gone")
	}
}

// runRefreshLoop starts the loop and returns its schedule channel and a stop
// function that waits for it.
func runRefreshLoop(t *testing.T, a *App, rt *fakeImageRuntime, clock *fakeClock) (chan RefreshSchedule, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	schedules := make(chan RefreshSchedule, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)

		a.refreshLoop(ctx, rt, schedules, clock.clock())
	}()

	stop := func() {
		cancel()
		<-done
	}

	t.Cleanup(stop)

	return schedules, stop
}

// TestRefreshIsOffUnlessAskedFor is AC1 from the loop's side: with refresh
// off nothing is built or pulled and no timer is armed, but the image in
// service is still recorded for the storage page (AC13).
func TestRefreshIsOffUnlessAskedFor(t *testing.T) {
	t.Parallel()

	a, rt, st, _ := refreshApp(t, func(c *config.Config) { c.Browser.Container.Refresh = "" })
	clock := newFakeClock(refreshStart)

	_, stop := runRefreshLoop(t, a, rt, clock)

	// The loop records the startup inventory before it waits; a reload is the
	// only thing that can wake it, so stopping it is the synchronisation.
	stop()

	if len(rt.builds)+len(rt.pulls) != 0 {
		t.Errorf("refresh off still built %v and pulled %v", rt.builds, rt.pulls)
	}

	select {
	case d := <-clock.armed:
		t.Errorf("refresh off armed a timer for %s", d)
	default:
	}

	if run := st.last(t, store.MaintenanceKindBrowserImages); run.Trigger != store.TriggerStartup {
		t.Errorf("the startup inventory was not recorded: %+v", run)
	}
}

// TestTheScheduleSurvivesARestartAndRunsOnInterval: the first refresh is one
// interval after the last recorded one, never at startup, and each next one
// an interval after the last finished.
func TestTheScheduleSurvivesARestartAndRunsOnInterval(t *testing.T) {
	t.Parallel()

	a, rt, st, _ := refreshApp(t, nil)
	rt.nextID, rt.nextVersion = "sha256:new", newBrowser

	lastRefresh := refreshStart.Add(-2 * 24 * time.Hour)
	if err := st.RecordBrowserImages(t.Context(), store.TriggerRefresh, lastRefresh, store.BrowserImageInventory{}, nil); err != nil {
		t.Fatal(err)
	}

	clock := newFakeClock(refreshStart)
	runRefreshLoop(t, a, rt, clock)

	if got, want := clock.waitArmed(t), lastRefresh.Add(config.DefaultRefreshInterval).Sub(refreshStart); got != want {
		t.Fatalf("first refresh armed in %s, want %s (one interval after the last)", got, want)
	}

	clock.set(lastRefresh.Add(config.DefaultRefreshInterval))

	if got := clock.waitArmed(t); got != config.DefaultRefreshInterval {
		t.Errorf("next refresh armed in %s, want one interval", got)
	}

	if len(rt.builds) != 1 {
		t.Errorf("builds = %v, want exactly one", rt.builds)
	}
}

// TestAReloadThatTurnsRefreshOffCancelsTheRefreshInProgress is AC6 and
// AC11: the build in flight is cancelled, nothing is switched, and the loop
// arms nothing more.
func TestAReloadThatTurnsRefreshOffCancelsTheRefreshInProgress(t *testing.T) {
	t.Parallel()

	a, rt, _, logged := refreshApp(t, nil)
	rt.block = true
	before := a.Pool.Identity()

	clock := newFakeClock(refreshStart)
	schedules, stop := runRefreshLoop(t, a, rt, clock)

	clock.set(refreshStart.Add(clock.waitArmed(t)))
	<-rt.building

	off := RefreshScheduleFor(a.Config)
	off.Mode = config.RefreshOff
	schedules <- off

	// Stop waits for the loop, which waits for the refresh it was running:
	// a refresh that ignored the cancellation would hang here.
	stop()

	if got := a.Pool.Identity(); got != before {
		t.Errorf("a cancelled refresh switched the pool: %+v", got)
	}

	if !strings.Contains(logged.String(), "cancelling the refresh in progress") {
		t.Errorf("the cancellation was not logged:\n%s", logged)
	}
}

// TestShutdownCancelsARefreshInProgress is AC6: shutdown ends a build in
// flight rather than waiting the half hour out.
func TestShutdownCancelsARefreshInProgress(t *testing.T) {
	t.Parallel()

	a, rt, _, _ := refreshApp(t, nil)
	rt.block = true

	clock := newFakeClock(refreshStart)
	_, stop := runRefreshLoop(t, a, rt, clock)

	clock.set(refreshStart.Add(clock.waitArmed(t)))
	<-rt.building

	stop()

	if len(rt.builds) != 1 {
		t.Errorf("builds = %v, want the one that was cancelled", rt.builds)
	}
}

// TestARestartResumesTheImageTheRefreshLeftInService: without it a restart
// would quietly go back to the configured image and undo the refresh.
func TestARestartResumesTheImageTheRefreshLeftInService(t *testing.T) {
	t.Parallel()

	a, _, st, _ := refreshApp(t, nil)

	inService := container.BuildRefreshTag(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	inv := store.BrowserImageInventory{Images: []store.BrowserImage{{Name: inService, ID: "sha256:old", State: ImageInService}}}

	if err := st.RecordBrowserImages(t.Context(), store.TriggerRefresh, refreshStart, inv, nil); err != nil {
		t.Fatal(err)
	}

	if got := InServiceImage(t.Context(), st); got.Image != inService {
		t.Fatalf("InServiceImage = %+v", got)
	}

	// The refresh's image qualifies only under the settings that made it.
	if !container.RefreshTagFrom(inService, false, "") || container.RefreshTagFrom(inService, true, "ghcr.io/x:y") {
		t.Error("a built image is not told apart from a pulled one")
	}

	a.Config.Browser.Container.Refresh = config.RefreshOff
	if got := a.resumeImage(t.Context(), nil, container.DefaultImage); got != container.DefaultImage {
		t.Errorf("with refresh off the daemon resumed %s", got)
	}
}
