package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// PoolOptions configures browser reuse.
type PoolOptions struct {
	// Size is the maximum number of concurrently live browsers. It, not the
	// target count, determines peak memory (NFR §1).
	Size int

	// MaxScansPerBrowser recycles a browser after this many scans, to bound
	// the effect of slow leaks inside Chrome itself. Zero means no recycling.
	MaxScansPerBrowser int64

	// Launch configures each launched browser.
	Launch Options

	// OnRestart is called whenever a browser is discarded and replaced, so
	// the caller can count restarts as a self-metric (Story 6.5).
	OnRestart func(reason string)
}

// ErrPoolClosed is returned once the pool has been closed.
var ErrPoolClosed = errors.New("browser pool is closed")

// Pool hands out browsers to scans, reusing processes while preserving
// per-scan isolation. It applies backpressure: when every browser is busy,
// Acquire blocks rather than spawning unbounded Chrome processes.
type Pool struct {
	opts PoolOptions

	// slots bounds concurrency. A token in the channel is permission to hold
	// one browser.
	slots chan struct{}

	mu     sync.Mutex
	idle   []*Browser
	live   int
	closed bool
}

// NewPool creates a pool. It does not launch anything yet; browsers start on
// first use so that startup stays fast (NFR §1).
func NewPool(opts PoolOptions) *Pool {
	if opts.Size < 1 {
		opts.Size = 1
	}

	p := &Pool{
		opts:  opts,
		slots: make(chan struct{}, opts.Size),
	}

	for range opts.Size {
		p.slots <- struct{}{}
	}

	return p
}

// Lease is a borrowed browser. Release must be called exactly once.
type Lease struct {
	Browser *Browser

	pool     *Pool
	released bool
}

// Release returns the browser to the pool, or discards it if it is no longer
// usable.
func (l *Lease) Release() {
	if l == nil || l.released {
		return
	}

	l.released = true
	l.pool.put(l.Browser)
}

// Discard returns the browser to the pool marked unusable, so it is replaced
// rather than handed to the next scan. Callers use this after a browser-level
// failure: a crashed page, a hung CDP call, a panic during capture.
func (l *Lease) Discard(reason string) {
	if l == nil || l.released {
		return
	}

	l.released = true
	l.Browser.MarkDead()
	l.pool.restarted(reason)
	l.pool.put(l.Browser)
}

// Acquire borrows a browser, launching one if the pool has a free slot and no
// idle browser. It blocks while every slot is busy, which is the backpressure
// that keeps resource use bounded.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	select {
	case <-p.slots:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a browser: %w", ctx.Err())
	}

	// From here on the slot must be returned on every path.
	b, err := p.take(ctx)
	if err != nil {
		p.slots <- struct{}{}

		return nil, err
	}

	return &Lease{Browser: b, pool: p}, nil
}

func (p *Pool) take(ctx context.Context) (*Browser, error) {
	for {
		p.mu.Lock()

		if p.closed {
			p.mu.Unlock()

			return nil, ErrPoolClosed
		}

		var candidate *Browser

		if n := len(p.idle); n > 0 {
			candidate = p.idle[n-1]
			p.idle = p.idle[:n-1]
		}

		p.mu.Unlock()

		if candidate == nil {
			return p.launch(ctx)
		}

		if reason, ok := p.shouldRecycle(ctx, candidate); ok {
			p.discard(candidate, reason)

			continue
		}

		return candidate, nil
	}
}

func (p *Pool) shouldRecycle(ctx context.Context, b *Browser) (string, bool) {
	switch {
	case p.opts.MaxScansPerBrowser > 0 && b.Scans() >= p.opts.MaxScansPerBrowser:
		return "scan limit reached", true
	case !b.Alive(ctx):
		return "browser stopped responding", true
	default:
		return "", false
	}
}

func (p *Pool) launch(ctx context.Context) (*Browser, error) {
	b, err := Launch(ctx, p.opts.Launch)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()

		// Losing a race with Close must not leak a process.
		_ = b.Close()

		return nil, ErrPoolClosed
	}

	p.live++
	p.mu.Unlock()

	return b, nil
}

// put returns a browser to the idle set, or closes it when it is dead or the
// pool is closed. The slot is always returned.
func (p *Pool) put(b *Browser) {
	defer func() { p.slots <- struct{}{} }()

	if b == nil {
		return
	}

	p.mu.Lock()

	switch {
	case p.closed:
		p.mu.Unlock()
		p.closeBrowser(b)

	case b.dead.Load():
		p.mu.Unlock()
		p.closeBrowser(b)

	default:
		p.idle = append(p.idle, b)
		p.mu.Unlock()
	}
}

func (p *Pool) discard(b *Browser, reason string) {
	p.restarted(reason)
	p.closeBrowser(b)
}

func (p *Pool) closeBrowser(b *Browser) {
	if err := b.Close(); err != nil {
		p.logger().Warn("closing browser", "error", err)
	}

	p.mu.Lock()
	p.live--
	p.mu.Unlock()
}

func (p *Pool) restarted(reason string) {
	p.logger().Info("replacing browser", "reason", reason)

	if p.opts.OnRestart != nil {
		p.opts.OnRestart(reason)
	}
}

func (p *Pool) logger() *slog.Logger {
	return p.opts.Launch.logger()
}

// Live reports how many browser processes the pool currently holds.
func (p *Pool) Live() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.live
}

// Close shuts down every browser. It waits for in-flight leases to be
// released, so no Chrome process and no profile directory is left behind
// (Story 3.5, Story 6.6).
func (p *Pool) Close() error {
	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()

		return nil
	}

	p.closed = true
	p.mu.Unlock()

	// Draining every slot means every lease has been released.
	for range p.opts.Size {
		<-p.slots
	}

	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	var errs []error

	for _, b := range idle {
		if err := b.Close(); err != nil {
			errs = append(errs, err)
		}

		p.mu.Lock()
		p.live--
		p.mu.Unlock()
	}

	return errors.Join(errs...)
}
