package browser_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
)

// The probe against a real Chrome: an unmanaged one allows browser contexts,
// and the browser the probe launched is kept for the first scan rather than
// thrown away. A managed one may refuse, and then the pool must fall back.
func TestProbeAgainstARealChrome(t *testing.T) {
	if os.Getenv("WSAW_SKIP_BROWSER_TESTS") != "" {
		t.Skip("skipping browser test: WSAW_SKIP_BROWSER_TESTS is set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := browser.Discover(ctx, os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("skipping browser test: no usable Chrome found (%v)", err)
	}

	p := browser.NewPool(browser.PoolOptions{
		MaxScansPerBrowser: 10,
		Launch:             browser.Options{Info: info, ProfileDir: t.TempDir(), LaunchTimeout: 40 * time.Second},
	})

	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("closing the pool: %v", err)
		}
	})

	err = p.ProbeBrowserContexts(ctx)

	switch {
	case errors.Is(err, browser.ErrBrowserContextsUnavailable):
		t.Logf("this Chrome refuses browser contexts: %v", err)

		if p.UsesBrowserContexts() || p.MaxScansPerBrowser() != 1 {
			t.Errorf("refused, but the pool did not fall back: contexts=%v max=%d",
				p.UsesBrowserContexts(), p.MaxScansPerBrowser())
		}

	case err != nil:
		t.Fatalf("ProbeBrowserContexts: %v", err)

	default:
		if !p.UsesBrowserContexts() || p.MaxScansPerBrowser() != 10 {
			t.Errorf("the probe succeeded but the pool changed: contexts=%v max=%d",
				p.UsesBrowserContexts(), p.MaxScansPerBrowser())
		}

		if got := p.Live(); got != 1 {
			t.Errorf("Live() = %d after a successful probe, want the probe's browser kept for the first scan", got)
		}
	}
}
