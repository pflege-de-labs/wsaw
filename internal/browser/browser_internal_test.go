package browser

// Flag parsing, profile cleanup and the pool's bookkeeping are unexported, and
// the external browser_test package can only reach them by launching a real
// Chrome. None of them needs one: a Browser configured with a RemoteURL owns
// no process and no profile directory, so it closes cleanly as a stand-in, and
// the pool's recycling and discard decisions are made before any CDP call.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubBrowser is a Browser that owns nothing: no allocator, no process, no
// profile directory. Close is therefore a no-op that returns nil, which makes
// it usable wherever the pool only needs a browser to account for.
func stubBrowser() *Browser {
	return &Browser{opts: Options{RemoteURL: "ws://stub.invalid/devtools/browser/stub"}}
}

func TestSplitFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		arg   string
		name  string
		value any
	}{
		{"--host-resolver-rules=MAP a b", "host-resolver-rules", "MAP a b"},
		{"--disable-gpu", "disable-gpu", true},
		{"lang=de-DE", "lang", "de-DE"},
		{"-single-dash=1", "single-dash", "1"},
		{"--empty=", "empty", ""},
		{"", "", true},
		{"--", "", true},
	}

	for _, c := range cases {
		name, value := splitFlag(c.arg)
		if name != c.name || value != c.value {
			t.Errorf("splitFlag(%q) = (%q, %#v), want (%q, %#v)", c.arg, name, value, c.name, c.value)
		}
	}
}

func TestExecOptionsReflectTheConfiguration(t *testing.T) {
	t.Parallel()

	base := &Browser{opts: Options{Info: Info{Path: "/usr/bin/chromium"}}}
	baseline := len(base.execOptions("/tmp/profile"))

	// The sandbox is opt-in and adds both of its flags together, so it cannot
	// be half-disabled (NFR §4).
	sandboxed := &Browser{opts: Options{Info: Info{Path: "/usr/bin/chromium"}, NoSandbox: true}}
	if got := len(sandboxed.execOptions("/tmp/profile")); got != baseline+2 {
		t.Errorf("no-sandbox added %d options, want 2", got-baseline)
	}

	proxied := &Browser{opts: Options{Info: Info{Path: "/usr/bin/chromium"}, Proxy: "http://proxy:3128"}}
	if got := len(proxied.execOptions("/tmp/profile")); got != baseline+1 {
		t.Errorf("proxy added %d options, want 1", got-baseline)
	}

	extra := &Browser{opts: Options{
		Info:      Info{Path: "/usr/bin/chromium"},
		ExtraArgs: []string{"--lang=de-DE", "--disable-gpu", "--window-size=1280,800"},
	}}
	if got := len(extra.execOptions("/tmp/profile")); got != baseline+3 {
		t.Errorf("three extra args added %d options, want 3", got-baseline)
	}
}

func TestOptionDefaults(t *testing.T) {
	t.Parallel()

	var zero Options

	if got := zero.launchTimeout(); got != 30*time.Second {
		t.Errorf("default launch timeout = %v, want 30s", got)
	}

	if got := (&Options{LaunchTimeout: time.Second}).launchTimeout(); got != time.Second {
		t.Errorf("configured launch timeout = %v, want 1s", got)
	}

	if zero.logger() == nil {
		t.Error("logger() returned nil; every lifecycle event logs through it")
	}

	configured := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if (&Options{Logger: configured}).logger() != configured {
		t.Error("logger() replaced the configured logger")
	}
}

func TestWellKnownPathsAreAbsolute(t *testing.T) {
	t.Parallel()

	paths := wellKnownPaths()

	switch runtime.GOOS {
	case "darwin", "linux":
		if len(paths) == 0 {
			t.Fatalf("no well-known browser locations for %s", runtime.GOOS)
		}
	default:
		// wsaw supports Linux and macOS only; elsewhere PATH lookup and a
		// clear error are the whole story.
		return
	}

	for _, p := range paths {
		if !filepath.IsAbs(p) {
			t.Errorf("well-known path %q is not absolute", p)
		}
	}
}

func TestRemoveWithRetryOnAnAlreadyGoneDirectory(t *testing.T) {
	t.Parallel()

	if err := removeWithRetry(filepath.Join(t.TempDir(), "never-existed")); err != nil {
		t.Errorf("removing a directory that is already gone: %v", err)
	}
}

func TestRemoveWithRetryGivesUpAndReportsWhy(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, which can remove a directory from a read-only parent")
	}

	if runtime.GOOS == "windows" {
		t.Skip("wsaw does not support Windows")
	}

	parent := t.TempDir()

	profile := filepath.Join(parent, "wsaw-profile-stuck")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatalf("creating the profile directory: %v", err)
	}

	// A parent without write permission makes the unlink fail on every
	// attempt, which is the shape of the real failure: a directory that
	// cannot be removed must be reported, not swallowed, because a leaked
	// profile per browser fills a long-running daemon's disk (Tenet 1).
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("making the parent read-only: %v", err)
	}

	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	err := removeWithRetry(profile)
	if err == nil {
		t.Fatal("removeWithRetry reported success for a directory it could not remove")
	}
}

func TestMarkDeadStopsABrowserBeingUsedAgain(t *testing.T) {
	t.Parallel()

	b := stubBrowser()

	if b.dead.Load() {
		t.Fatal("a fresh browser is already marked dead")
	}

	b.MarkDead()

	if !b.dead.Load() {
		t.Fatal("MarkDead did not mark the browser")
	}

	// Both entry points must refuse a dead browser without touching CDP: the
	// pool discards it rather than handing it to another scan.
	if b.Alive(context.Background()) {
		t.Error("a browser marked dead reports itself alive")
	}

	if _, _, err := b.NewScanContext(context.Background()); err == nil {
		t.Error("a dead browser handed out a scan context")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	b := stubBrowser()

	if err := b.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	if err := b.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestNewPoolClampsSizeToAtLeastOne(t *testing.T) {
	t.Parallel()

	for _, size := range []int{-3, 0, 1} {
		p := NewPool(PoolOptions{Size: size})

		if got := cap(p.slots); got != 1 {
			t.Errorf("size %d produced %d slots, want 1", size, got)
		}

		if err := p.Close(); err != nil {
			t.Errorf("closing the pool: %v", err)
		}
	}
}

func TestAcquireRespectsACancelledContext(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{Size: 1})
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lease, err := p.Acquire(ctx)
	if err == nil {
		lease.Release()
		t.Fatal("Acquire handed out a browser for a cancelled context")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}

	// No slot may be consumed by a failed acquisition, or the pool leaks
	// capacity until restart.
	if got := len(p.slots); got != 1 {
		t.Errorf("%d slots free after a failed acquire, want 1", got)
	}
}

func TestAcquireOnAClosedPoolReturnsItsSlot(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{Size: 2})

	// Set the flag directly rather than calling Close, which drains the slots
	// on its way out: the race being tested is a scan arriving after the
	// daemon started shutting down but before the drain completed.
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	lease, err := p.Acquire(context.Background())
	if !errors.Is(err, ErrPoolClosed) {
		if err == nil {
			lease.Release()
		}

		t.Fatalf("error = %v, want ErrPoolClosed", err)
	}

	if got := len(p.slots); got != 2 {
		t.Errorf("%d slots free after acquiring from a closed pool, want 2", got)
	}
}

func TestShouldRecycleRetiresABrowserBeforeItIsHandedOn(t *testing.T) {
	t.Parallel()

	// Only the two branches that decide before touching CDP are exercised
	// here. A browser under the scan limit falls through to Alive, which
	// needs a real Chrome, and the integration tests cover it there.
	ctx := context.Background()

	limited := &Pool{opts: PoolOptions{MaxScansPerBrowser: 2}}

	at := stubBrowser()
	at.scans.Store(2)

	reason, ok := limited.shouldRecycle(ctx, at)
	if !ok {
		t.Error("a browser at the scan limit was kept")
	}

	if !strings.Contains(reason, "scan limit") {
		t.Errorf("reason = %q, want it to name the scan limit", reason)
	}

	// A browser that stopped answering is replaced rather than waited on.
	// Alive short-circuits on the dead flag, so this reaches no CDP either.
	gone := stubBrowser()
	gone.MarkDead()

	reason, ok = (&Pool{}).shouldRecycle(ctx, gone)
	if !ok {
		t.Error("an unresponsive browser was kept")
	}

	if !strings.Contains(reason, "responding") {
		t.Errorf("reason = %q, want it to say the browser stopped responding", reason)
	}
}

func TestDiscardCountsARestartAndDropsTheBrowser(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		reasons []string
	)

	p := NewPool(PoolOptions{
		Size:      1,
		OnRestart: func(reason string) { mu.Lock(); reasons = append(reasons, reason); mu.Unlock() },
	})
	t.Cleanup(func() { _ = p.Close() })

	p.mu.Lock()
	p.live = 1
	p.mu.Unlock()

	p.discard(stubBrowser(), "browser stopped responding")

	mu.Lock()
	defer mu.Unlock()

	if len(reasons) != 1 || reasons[0] != "browser stopped responding" {
		t.Errorf("OnRestart saw %v, want one restart with the reason", reasons)
	}

	if got := p.Live(); got != 0 {
		t.Errorf("pool reports %d live browsers after a discard, want 0", got)
	}
}

func TestLeaseDiscardReplacesTheBrowserRatherThanReusingIt(t *testing.T) {
	t.Parallel()

	var restarts int32

	p := NewPool(PoolOptions{Size: 1, OnRestart: func(string) { restarts++ }})
	t.Cleanup(func() { _ = p.Close() })

	// Stand in for a scan holding the pool's only slot.
	<-p.slots

	p.mu.Lock()
	p.live = 1
	p.mu.Unlock()

	b := stubBrowser()
	lease := &Lease{Browser: b, pool: p}

	lease.Discard("page crashed during capture")

	if !b.dead.Load() {
		t.Error("a discarded browser was not marked dead")
	}

	if restarts != 1 {
		t.Errorf("OnRestart fired %d times, want 1", restarts)
	}

	// The slot always comes back, and the dead browser never reaches the idle
	// set where the next scan would find it.
	if got := len(p.slots); got != 1 {
		t.Errorf("%d slots free after a discard, want 1", got)
	}

	p.mu.Lock()
	idle := len(p.idle)
	p.mu.Unlock()

	if idle != 0 {
		t.Errorf("%d browsers idle after a discard, want 0", idle)
	}

	// Discarding twice, or releasing after discarding, must not return a
	// second slot.
	lease.Discard("again")
	lease.Release()

	if got := len(p.slots); got != 1 {
		t.Errorf("%d slots free after a repeated release, want 1", got)
	}
}

func TestReleaseReturnsTheBrowserToTheIdleSet(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{Size: 1})

	<-p.slots

	p.mu.Lock()
	p.live = 1
	p.mu.Unlock()

	b := stubBrowser()
	lease := &Lease{Browser: b, pool: p}
	lease.Release()

	p.mu.Lock()
	idle := len(p.idle)
	p.mu.Unlock()

	if idle != 1 {
		t.Fatalf("%d browsers idle after a release, want 1", idle)
	}

	// Close waits for every lease and closes what it finds, so nothing is
	// left behind (Story 6.6).
	if err := p.Close(); err != nil {
		t.Fatalf("closing the pool: %v", err)
	}

	if got := p.Live(); got != 0 {
		t.Errorf("pool reports %d live browsers after Close, want 0", got)
	}

	// Close is idempotent, and a second one must not block on drained slots.
	if err := p.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestReleaseOnANilLeaseDoesNothing(t *testing.T) {
	t.Parallel()

	var lease *Lease

	lease.Release()
	lease.Discard("no browser here")
}

func TestPutOnAClosedPoolClosesTheBrowser(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{Size: 1})

	<-p.slots

	p.mu.Lock()
	p.closed = true
	p.live = 1
	p.mu.Unlock()

	p.put(stubBrowser())

	p.mu.Lock()
	idle := len(p.idle)
	p.mu.Unlock()

	if idle != 0 {
		t.Errorf("a browser returned to a closed pool was kept as idle (%d)", idle)
	}

	if got := p.Live(); got != 0 {
		t.Errorf("pool reports %d live browsers, want 0", got)
	}

	if got := len(p.slots); got != 1 {
		t.Errorf("%d slots free, want the slot returned even on a closed pool", got)
	}
}

func TestPutIgnoresANilBrowser(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{Size: 1})
	t.Cleanup(func() { _ = p.Close() })

	<-p.slots
	p.put(nil)

	if got := len(p.slots); got != 1 {
		t.Errorf("%d slots free, want the slot returned", got)
	}
}
