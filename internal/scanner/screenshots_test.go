package scanner_test

import (
	"context"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// A before/after pair asserts that something happened to the page between the
// two frames. When nothing did — consent mode "none", or a page with no
// banner to click — the second frame is the first one again, and storing it
// costs a Chrome round-trip to produce evidence of an interaction that never
// took place.

// newScannerWithArtifacts builds a scanner that can store screenshots. The
// shared helper leaves Store nil, which turns every artifact sink into an
// error.
func newScannerWithArtifacts(t *testing.T, info browser.Info, resolverRules string) (*scanner.Scanner, func()) {
	t.Helper()

	launch := browser.Options{
		Info:          info,
		LaunchTimeout: 40 * time.Second,
		ProfileDir:    t.TempDir(),
	}

	if resolverRules != "" {
		launch.ExtraArgs = []string{"host-resolver-rules=" + resolverRules}
	}

	pool := browser.NewPool(browser.PoolOptions{Size: 1, Launch: launch})

	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{Path: dir + "/wsaw.db", ArtifactDir: dir + "/artifacts"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})

	normalizer, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, err := scanner.New(scanner.Deps{Pool: pool, Store: st, Rules: rules}, scanner.Options{
		Normalizer:            normalizer,
		AllowHeuristicConsent: true,
		WsawVersion:           "test",
		ChromeVersion:         info.Version,
	})
	if err != nil {
		t.Fatal(err)
	}

	return s, func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing pool: %v", err)
		}
	}
}

func screenshotKinds(res *model.Result) []string {
	out := make([]string, 0, len(res.Screenshots))
	for _, a := range res.Screenshots {
		out = append(out, a.Kind)
	}

	return out
}

// Consent mode "none" says outright that the page will not be touched, so
// there is no "after" to photograph.
func TestAScanThatNeverInteractsStoresOneScreenshot(t *testing.T) {
	info := requireChrome(t)

	site := newFixtureSite(t, true)

	s, closePool := newScannerWithArtifacts(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	target := site.target(t)
	target.Screenshots = true

	out, err := s.Scan(ctx, target, model.ConsentNone)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if !res.OK() {
		t.Fatalf("scan not OK: termination=%s error=%s warnings=%v",
			res.Termination, res.Error, res.Warnings)
	}

	kinds := screenshotKinds(res)

	if len(kinds) != 1 || kinds[0] != "screenshot-before-consent" {
		t.Fatalf("screenshots = %v, want just screenshot-before-consent: nothing "+
			"interacted with the page, so a second frame would be the first one "+
			"again (warnings=%v)", kinds, res.Warnings)
	}
}

// The pair is still captured when the interaction really happens: this is the
// evidence the product exists to produce, and the fix must not cost it.
func TestAScanThatInteractsStoresBothScreenshots(t *testing.T) {
	info := requireChrome(t)

	site := newCMPSite(t)

	s, closePool := newScannerWithArtifacts(t, info, site.resolverRules())
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	target := site.target()
	target.Screenshots = true

	out, err := s.Scan(ctx, target, model.ConsentReject)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result

	if res.Consent.Outcome != model.OutcomeApplied {
		t.Fatalf("outcome = %q (%s), want applied", res.Consent.Outcome, res.Consent.Reason)
	}

	kinds := screenshotKinds(res)

	if len(kinds) != 2 {
		t.Fatalf("screenshots = %v, want a before and an after frame (warnings=%v)",
			kinds, res.Warnings)
	}

	var before, after model.Artifact

	for _, a := range res.Screenshots {
		switch a.Kind {
		case "screenshot-before-consent":
			before = a
		case "screenshot-after-consent":
			after = a
		}
	}

	if before.Ref == "" || after.Ref == "" {
		t.Fatalf("screenshots = %v, want one of each kind", kinds)
	}

	if before.SHA256 == after.SHA256 {
		t.Error("the two frames are the same image: dismissing the banner should have changed the page")
	}
}
