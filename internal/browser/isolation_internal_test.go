package browser

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A pool whose scans share a cookie jar must never let a browser serve a
// second scan, whatever it was configured with: that is the whole of the
// isolation left once browser contexts are off.
func TestNewPoolServesOneScanPerBrowserWithoutBrowserContexts(t *testing.T) {
	t.Parallel()

	for _, configured := range []int64{0, 1, 10} {
		p := NewPool(PoolOptions{
			MaxScansPerBrowser: configured,
			Launch:             Options{NoBrowserContexts: true},
		})

		if got := p.MaxScansPerBrowser(); got != 1 {
			t.Errorf("configured %d without browser contexts: MaxScansPerBrowser() = %d, want 1", configured, got)
		}

		if p.UsesBrowserContexts() {
			t.Errorf("configured %d: UsesBrowserContexts() = true for a pool built without them", configured)
		}

		if err := p.Close(); err != nil {
			t.Errorf("closing the pool: %v", err)
		}
	}
}

func TestNewPoolKeepsTheConfiguredLimitWithBrowserContexts(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{MaxScansPerBrowser: 10})

	t.Cleanup(func() { _ = p.Close() })

	if got := p.MaxScansPerBrowser(); got != 10 {
		t.Errorf("MaxScansPerBrowser() = %d, want the configured 10", got)
	}

	if !p.UsesBrowserContexts() {
		t.Error("a pool uses browser contexts unless told otherwise")
	}
}

// A probe that could not even start a browser has not verified anything, so
// the pool falls back exactly as it would for a refusal — and says it was not
// one.
func TestProbeThatCannotLaunchFallsBackToProcessIsolation(t *testing.T) {
	t.Parallel()

	p := NewPool(PoolOptions{
		MaxScansPerBrowser: 10,
		Launch: Options{
			Info:          Info{Path: filepath.Join(t.TempDir(), "no-such-chrome")},
			ProfileDir:    t.TempDir(),
			LaunchTimeout: 10 * time.Second,
		},
	})

	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := p.ProbeBrowserContexts(ctx)
	if err == nil {
		t.Fatal("ProbeBrowserContexts succeeded without a browser")
	}

	if errors.Is(err, ErrBrowserContextsUnavailable) {
		t.Errorf("a browser that never started was reported as refusing browser contexts: %v", err)
	}

	if p.UsesBrowserContexts() {
		t.Error("the pool still opens scans in browser contexts after an unverified probe")
	}

	if got := p.MaxScansPerBrowser(); got != 1 {
		t.Errorf("MaxScansPerBrowser() = %d after the fallback, want 1", got)
	}

	if got := p.Live(); got != 0 {
		t.Errorf("Live() = %d after a failed launch, want 0", got)
	}
}

func TestProbeIsSkippedWhenBrowserContextsAreAlreadyOff(t *testing.T) {
	t.Parallel()

	// A path that cannot launch: if the probe tried, it would fail.
	p := NewPool(PoolOptions{Launch: Options{
		Info:              Info{Path: filepath.Join(t.TempDir(), "no-such-chrome")},
		NoBrowserContexts: true,
	}})

	t.Cleanup(func() { _ = p.Close() })

	if err := p.ProbeBrowserContexts(context.Background()); err != nil {
		t.Errorf("ProbeBrowserContexts on a pool without browser contexts = %v, want nil", err)
	}
}
