package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// A browser a scan discards is one restart. The pool counts it when the lease
// is discarded; nothing else may count it again, or
// wsaw_browser_restarts_total reads double what happened.
func TestADiscardedBrowserIsCountedOnce(t *testing.T) {
	requireChrome(t)

	cfg := minimalConfig(t)
	cfg.Browser.Runtime = "local"
	cfg.Browser.ProfileDir = t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	a, err := New(ctx, cfg, Options{Version: "test", RequireBrowser: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	target, ok := a.TargetByName("site")
	if !ok {
		t.Fatal("the target is not configured")
	}

	// A beacon rule capture refuses, so capture cannot start and the scan
	// discards its browser before anything is fetched — no site is needed.
	target.Beacons = append(target.Beacons, capture.Beacon{})

	if _, err := a.Scanner.Scan(ctx, target, model.ConsentReject); err == nil {
		t.Fatal("a scan with an unusable beacon rule succeeded")
	}

	var scrape bytes.Buffer
	if err := a.Metrics.WritePrometheus(&scrape); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(scrape.String(), "\nwsaw_browser_restarts_total 1\n") {
		t.Errorf("want exactly one restart counted, scrape:\n%s", scrape.String())
	}
}
