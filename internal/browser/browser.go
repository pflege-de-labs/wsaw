package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Options configures how browsers are launched.
type Options struct {
	// Info is the discovered browser. Required.
	Info Info

	// RemoteURL attaches to an existing CDP endpoint instead of launching a
	// process. When set, wsaw does not own the browser's lifecycle and cannot
	// guarantee profile isolation, so this is recorded in results.
	RemoteURL string

	// NoSandbox disables the Chrome sandbox. Never a default: it is opt-in,
	// warned about at startup, and exists only for constrained container
	// environments (NFR §4).
	NoSandbox bool

	// Proxy is an outbound proxy URL. Credentials, if any, are handled via
	// CDP authentication rather than the command line, where they would be
	// visible in the process list.
	Proxy string

	// ExtraArgs are additional Chrome flags for unusual environments.
	ExtraArgs []string

	// Container, when set, runs the browser inside a container instead of as
	// a host process, and wsaw attaches to it over CDP.
	//
	// An interface rather than a concrete type: this is a seam with a
	// plausible second implementation — a remote browser farm, a different
	// runtime — and it keeps this package free of any container dependency
	// (Tenet 12).
	Container ContainerLauncher

	// ProfileDir is the parent directory for per-browser profiles. Empty
	// means the system temporary directory.
	//
	// Operators need this when /tmp is small, mounted noexec, or shared;
	// tests need it so that inspecting for leaked profiles does not depend on
	// what else happens to be running on the machine.
	ProfileDir string

	// LaunchTimeout bounds browser startup.
	LaunchTimeout time.Duration

	// NoBrowserContexts opens each scan in a tab of the browser's default
	// context instead of in a browser context of its own. It is the fallback
	// for a Chrome that refuses Target.createBrowserContext; every tab then
	// shares one cookie jar, so a pool built with it serves one scan per
	// browser whatever it is configured to do.
	NoBrowserContexts bool

	// Logger receives browser lifecycle events.
	Logger *slog.Logger
}

func (o *Options) launchTimeout() time.Duration {
	if o.LaunchTimeout > 0 {
		return o.LaunchTimeout
	}

	return 30 * time.Second
}

func (o *Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}

	return slog.Default()
}

// ContainerLauncher starts a browser somewhere other than this host and says
// where to reach it.
type ContainerLauncher interface {
	// Start launches a browser and returns a handle to it. The caller owns
	// the handle and must remove it.
	Start(ctx context.Context) (ContainerInstance, error)
}

// ContainerInstance is a running containerised browser.
type ContainerInstance interface {
	// Endpoint is the CDP endpoint to attach to.
	Endpoint() string
	// ID identifies the container, for logs.
	ID() string
	// Remove stops and deletes it. It must be safe to call more than once.
	Remove(ctx context.Context) error
}

// Browser is one running Chrome process, or one attachment to a remote CDP
// endpoint. It is safe for concurrent use only in the sense that Contexts may
// be created from it; each scan gets its own isolated context.
type Browser struct {
	opts Options

	allocCtx    context.Context
	allocCancel context.CancelFunc

	// browserCtx keeps the browser alive independently of any single scan, so
	// one cancelled scan does not tear down a pooled browser.
	browserCtx    context.Context
	browserCancel context.CancelFunc

	// userDataDir is removed on Close. Empty when attaching remotely, since
	// wsaw does not own that browser's profile.
	userDataDir string

	// instance is the container this browser runs in, when containerised.
	// wsaw started it, so wsaw removes it.
	instance ContainerInstance

	// tap sees every event the browser sends, so a scan can listen to its
	// tab's events in order (see Listen).
	tap *eventTap

	scans atomic.Int64

	closeOnce sync.Once
	closeErr  error

	// dead is set when the browser is known to be unusable, so the pool
	// discards it rather than handing it to another scan.
	dead atomic.Bool
}

// Launch starts a browser. The returned Browser must be closed by the caller,
// which removes its temporary profile directory.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	b := &Browser{opts: opts, tap: newEventTap()}

	remoteURL := opts.RemoteURL

	if opts.Container != nil {
		// The container is started before the allocator, so a failure here
		// never leaves a half-built browser holding a pool slot.
		inst, err := opts.Container.Start(ctx)
		if err != nil {
			return nil, err
		}

		b.instance = inst
		remoteURL = inst.Endpoint()
	}

	if remoteURL != "" {
		b.allocCtx, b.allocCancel = chromedp.NewAllocatorContext(context.Background(), newAttacher(remoteURL, b.tap))
	} else {
		dir, err := os.MkdirTemp(opts.ProfileDir, "wsaw-profile-")
		if err != nil {
			return nil, fmt.Errorf("creating browser profile directory: %w", err)
		}

		b.userDataDir = dir

		// The allocator is rooted in context.Background rather than ctx: a
		// pooled browser must outlive the scan that created it. Its lifetime
		// is bounded by Close, which is guaranteed by the pool.
		b.allocCtx, b.allocCancel = chromedp.NewExecAllocator(context.Background(), b.execOptions(dir)...)
	}

	b.browserCtx, b.browserCancel = chromedp.NewContext(b.allocCtx)

	// Starting the browser is bounded: a Chrome that never comes up must not
	// hang the daemon.
	startCtx, cancel := context.WithTimeout(ctx, opts.launchTimeout())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		// chromedp starts the process on first action.
		done <- chromedp.Do(b.browserCtx)
	}()

	select {
	case err := <-done:
		if err != nil {
			b.forceClose()

			return nil, fmt.Errorf("starting browser: %w", err)
		}

	case <-startCtx.Done():
		b.forceClose()

		return nil, fmt.Errorf("starting browser: %w", startCtx.Err())
	}

	return b, nil
}

func (b *Browser) execOptions(userDataDir string) []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(b.opts.Info.Path),
		chromedp.UserDataDir(userDataDir),
		chromedp.Headless,

		// A websocket rather than chromedp's default pipe, because only a
		// connection wsaw dials itself can go through the event tap.
		chromedp.WithDialer(b.tap.dialer),

		// Determinism and containment, not convenience: each of these removes
		// a source of noise or of unbounded work.
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-hang-monitor", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-prompt-on-repost", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),

		// Never let a scanned page start a download: a scan must not write
		// attacker-chosen files to disk (NFR §4).
		chromedp.Flag("disable-file-system", true),

		// Chrome's own reporting must not appear in captured traffic.
		chromedp.Flag("disable-features", "Translate,OptimizationHints,MediaRouter,InterestFeedContentSuggestions"),
	}

	if b.opts.NoSandbox {
		// Opt-in only, and warned about at startup by the caller.
		opts = append(opts,
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-setuid-sandbox", true),
		)
	}

	if b.opts.Proxy != "" {
		opts = append(opts, chromedp.ProxyServer(b.opts.Proxy))
	}

	for _, arg := range b.opts.ExtraArgs {
		name, value := splitFlag(arg)
		opts = append(opts, chromedp.Flag(name, value))
	}

	return opts
}

// splitFlag parses "--name=value", "--name" and "name=value" into a chromedp
// flag name and value.
func splitFlag(arg string) (name string, value any) {
	arg = trimLeadingDashes(arg)

	for i := range len(arg) {
		if arg[i] == '=' {
			return arg[:i], arg[i+1:]
		}
	}

	return arg, true
}

func trimLeadingDashes(s string) string {
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}

	return s
}

// NewScanContext returns a context for one scan, isolated from every other
// scan. Isolation is a correctness requirement, not an optimization (Tenet 2):
// consent state leaking between scans would invalidate the product's central
// claim.
//
// The returned cancel function must always be called; it disposes of the
// browser context and its storage.
func (b *Browser) NewScanContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if b.dead.Load() {
		return nil, nil, errors.New("browser is no longer usable")
	}

	// Each scan gets a browser context of its own: a cookie jar, cache and
	// quota storage that exist only for this scan and are disposed of with
	// it. Clearing a shared jar between scans was never enough — a response
	// to the previous tab's unload beacon arrives after the clear and writes
	// its cookie into whatever scan is running by then.
	//
	// Chrome can refuse to create one — a managed install answers
	// Target.createBrowserContext with "Not allowed" — and then the scan
	// opens in a tab of the default context, where the only boundary is the
	// browser process. The pool serves one scan per browser in that case, so
	// isolation never rests on a tab.
	var (
		opts      []chromedp.ContextOption
		contextID cdp.BrowserContextID
	)

	if !b.opts.NoBrowserContexts {
		id, tab, err := b.openBrowserContext(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("opening a browser context for the scan: %w", err)
		}

		contextID = id
		opts = append(opts, chromedp.WithTargetID(tab))
	}

	scanCtx, cancelScan := chromedp.NewContext(b.browserCtx, opts...)

	// chromedp records the browser context only for tabs it created itself.
	// Recorded here, so what reads it — the cookie jar, the download policy —
	// addresses this scan's context and not the browser's default one.
	if contextID != "" {
		chromedp.FromContext(scanCtx).BrowserContextID = contextID
	}

	// The scan's own deadline and cancellation come from ctx, but the browser
	// context must be torn down even if ctx is already done.
	linked, cancelLinked := context.WithCancel(scanCtx)

	stop := context.AfterFunc(ctx, cancelLinked)

	cancel := func() {
		stop()
		cancelLinked()
		cancelScan()

		if contextID != "" {
			b.disposeBrowserContext(contextID)
		}
	}

	// The session must be established here, on the long-lived context, and
	// not left to the caller's first action.
	//
	// chromedp creates the target's session on the first Run against a
	// context and ties the session's message loop to the context it was given.
	// A caller that ran its first action under a short per-step deadline
	// would therefore lose the whole session when that deadline was
	// cancelled, and every later step would time out with nothing captured.
	// Returning a context whose session is already live makes that mistake
	// impossible to repeat.
	initCtx, cancelInit := context.WithTimeout(ctx, b.opts.launchTimeout())
	defer cancelInit()

	done := make(chan error, 1)

	go func() { done <- chromedp.Do(linked) }()

	select {
	case err := <-done:
		if err != nil {
			cancel()

			return nil, nil, fmt.Errorf("opening a scan session: %w", err)
		}

	case <-initCtx.Done():
		cancel()

		return nil, nil, fmt.Errorf("opening a scan session: %w", initCtx.Err())
	}

	b.scans.Add(1)

	return context.WithValue(linked, tapKey{}, b.tap), cancel, nil
}

// ErrBrowserContextsUnavailable reports that the browser refused to create a
// browser context, so scans on it can only be isolated by the process
// boundary.
var ErrBrowserContextsUnavailable = errors.New("the browser refuses to create browser contexts")

// ProbeBrowserContexts opens a browser context with a tab in it, exactly as a
// scan would, and disposes of it again, to learn whether this browser can
// give every scan a context of its own. A refusal wraps
// ErrBrowserContextsUnavailable; any other error means the question could not
// be asked.
func (b *Browser) ProbeBrowserContexts(ctx context.Context) error {
	if b.dead.Load() {
		return errors.New("browser is no longer usable")
	}

	id, _, err := b.openBrowserContext(ctx)
	if err != nil {
		return err
	}

	b.disposeBrowserContext(id)

	return nil
}

// browserTimeout bounds one browser-level CDP exchange: creating or disposing
// of a browser context and its tab.
const browserTimeout = 10 * time.Second

// onBrowser runs fn against the browser target rather than a tab, bounded by
// browserTimeout and by ctx.
func (b *Browser) onBrowser(ctx context.Context, fn func(ctx context.Context, s cdp.Session) error) error {
	// Derived from the browser's own context, which carries the CDP
	// connection; the caller's cancellation still applies.
	runCtx, cancel := context.WithTimeout(b.browserCtx, browserTimeout)
	defer cancel()

	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	c := chromedp.FromContext(runCtx)
	if c == nil || c.Browser == nil {
		return errors.New("no browser connection")
	}

	return fn(runCtx, c.Browser)
}

// openBrowserContext creates a browser context and a blank tab in it.
//
// The tab is created with newWindow set. Headless Chrome answers a plain
// Target.createTarget in a fresh context with "Failed to open new tab - no
// browser is open", because the context has no window to open a tab in —
// which is also why chromedp's own WithNewBrowserContext cannot be used.
//
// Either refusal wraps ErrBrowserContextsUnavailable: a context that cannot
// hold a tab is as unusable to a scan as one that cannot be created.
func (b *Browser) openBrowserContext(ctx context.Context) (cdp.BrowserContextID, target.ID, error) {
	var (
		id  cdp.BrowserContextID
		tab target.ID
	)

	err := b.onBrowser(ctx, func(ctx context.Context, s cdp.Session) error {
		created, err := cdp.Call(ctx, s, target.CreateBrowserContext, target.CreateBrowserContextParams{
			DisposeOnDetach: new(true),
		})
		if err != nil {
			return refusal(ctx, "creating a browser context", err)
		}

		id = created.BrowserContextID

		opened, err := cdp.Call(ctx, s, target.CreateTarget, target.CreateTargetParams{
			URL:              "about:blank",
			BrowserContextID: id,
			NewWindow:        new(true),
		})
		if err != nil {
			return refusal(ctx, "opening a tab in a browser context", err)
		}

		tab = opened.TargetID

		return nil
	})
	if err != nil {
		// A context created without its tab is disposed of here, since the
		// caller never learns its ID.
		if id != "" {
			b.disposeBrowserContext(id)
		}

		return "", "", err
	}

	return id, tab, nil
}

// refusal classifies a failed browser-context call: the browser saying no is
// ErrBrowserContextsUnavailable, a deadline or cancellation is not.
func refusal(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", what, err)
	}

	return fmt.Errorf("%w: %s: %w", ErrBrowserContextsUnavailable, what, err)
}

// disposeBrowserContext discards a scan's browser context and everything in
// it: its tabs, cookie jar, cache and storage. It runs on its own deadline
// because it is called on the way out, when the scan's context is already
// done.
//
// A context that cannot be disposed of costs memory inside this browser, not
// isolation — the next scan gets a new one regardless — so a failure is
// logged rather than returned.
func (b *Browser) disposeBrowserContext(id cdp.BrowserContextID) {
	err := b.onBrowser(context.Background(), func(ctx context.Context, s cdp.Session) error {
		_, err := cdp.Call(ctx, s, target.DisposeBrowserContext, target.DisposeBrowserContextParams{BrowserContextID: id})

		return err
	})
	if err != nil && !b.dead.Load() {
		b.opts.logger().Warn("could not dispose of a scan's browser context",
			"browser_context", string(id), "error", err)
	}
}

// Scans reports how many scan contexts this browser has served, which drives
// pool recycling.
func (b *Browser) Scans() int64 { return b.scans.Load() }

// MarkDead records that the browser is unusable, so the pool discards it.
func (b *Browser) MarkDead() { b.dead.Store(true) }

// Alive checks that the browser still answers CDP within a short deadline. A
// browser that does not answer is marked dead rather than waited on.
func (b *Browser) Alive(ctx context.Context) bool {
	if b.dead.Load() {
		return false
	}

	const aliveTimeout = 5 * time.Second

	checkCtx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()

	// Run an empty action against the browser context; it fails fast if the
	// process is gone.
	if err := chromedp.Do(b.browserCtx); err != nil {
		b.dead.Store(true)

		return false
	}

	select {
	case <-checkCtx.Done():
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			b.dead.Store(true)

			return false
		}
	default:
	}

	return true
}

// Close terminates the browser and removes its profile directory. It is safe
// to call more than once, and it never blocks indefinitely.
func (b *Browser) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.shutdown()
	})

	return b.closeErr
}

func (b *Browser) shutdown() error {
	// Ask Chrome to exit, but never wait on it indefinitely: an unresponsive
	// browser is killed by cancelling its allocator.
	if b.opts.RemoteURL == "" && b.instance == nil {
		const graceful = 5 * time.Second

		ctx, cancel := context.WithTimeout(b.browserCtx, graceful)

		done := make(chan struct{})

		go func() {
			defer close(done)

			_ = chromedp.Cancel(ctx) // best effort; the kill below is the guarantee
		}()

		select {
		case <-done:
		case <-time.After(graceful):
			b.opts.logger().Warn("browser did not exit gracefully, killing it")
		}

		cancel()
	}

	b.forceClose()

	// The container is removed on every exit path, including a failed launch
	// and a panic. A leaked container is worse than a leaked process: it
	// survives the restart that would have cleaned the process up
	// (Story 1.8, AC4).
	if b.instance != nil {
		const removeTimeout = 30 * time.Second

		ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)

		err := b.instance.Remove(ctx)

		cancel()

		if err != nil {
			return err
		}
	}

	// The profile directory is removed on every path, including panic and
	// cancellation, because a leaked directory per browser fills the disk of a
	// long-running daemon (Tenet 1, NFR §2).
	//
	// chromedp only removes a profile directory it created itself, and wsaw
	// supplies its own, so removal is wsaw's responsibility. It has to happen
	// after Chrome has actually exited: cancelling the allocator context only
	// starts the teardown, and a browser still shutting down rewrites its
	// profile, which recreated the directory moments after it was removed.
	if b.userDataDir != "" {
		b.waitForExit()

		if err := removeWithRetry(b.userDataDir); err != nil {
			return fmt.Errorf("removing browser profile directory %s: %w", b.userDataDir, err)
		}
	}

	return nil
}

// waitForExit blocks until the allocator has released the browser process, or
// until a bounded grace period passes. It is bounded because a browser that
// refuses to die must not stop the daemon from shutting down.
func (b *Browser) waitForExit() {
	c := chromedp.FromContext(b.allocCtx)
	if c == nil || c.Allocator == nil {
		return
	}

	const exitGrace = 10 * time.Second

	done := make(chan struct{})

	go func() {
		c.Allocator.Wait()
		close(done)
	}()

	timer := time.NewTimer(exitGrace)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		b.opts.logger().Warn("browser process did not exit within the grace period",
			"profile_dir", b.userDataDir)
	}
}

// removeWithRetry deletes a directory, retrying briefly. A browser that is
// still flushing state can recreate files between the walk and the unlink,
// which surfaces as a spurious "directory not empty".
func removeWithRetry(dir string) error {
	const (
		attempts = 5
		delay    = 200 * time.Millisecond
	)

	var err error

	for i := range attempts {
		if err = os.RemoveAll(dir); err == nil {
			if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
				return nil
			}
		}

		if i < attempts-1 {
			time.Sleep(delay)
		}
	}

	if err != nil {
		return err
	}

	return fmt.Errorf("directory still present after %d attempts", attempts)
}

func (b *Browser) forceClose() {
	b.dead.Store(true)

	if b.browserCancel != nil {
		b.browserCancel()
	}

	if b.allocCancel != nil {
		b.allocCancel()
	}
}
