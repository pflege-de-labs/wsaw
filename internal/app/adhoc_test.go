package app

// Story 5.27: what wsaw does with a URL somebody typed, before a browser is
// pointed at it. No browser is needed to see any of it.

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func adHocApp(t *testing.T, mutate func(*config.Config)) *App {
	t.Helper()

	cfg := minimalConfig(t)
	cfg.API.Enabled = true
	cfg.API.AdHocURLs.Enabled = true

	if mutate != nil {
		mutate(cfg)
	}

	a, err := New(t.Context(), cfg, Options{Version: "test", LogOutput: discardWriter{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = a.Close() })

	return a
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// Off is off, whatever is typed.
func TestAcceptURLRefusesWhenTheFeatureIsOff(t *testing.T) {
	t.Parallel()

	a := adHocApp(t, func(c *config.Config) { c.API.AdHocURLs.Enabled = false })

	_, err := a.AcceptURL(t.Context(), "https://example.com/", model.ConsentReject)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("err = %v, want it to say the feature is not enabled", err)
	}
}

// A mode arrives in a request. Which modes exist is not the question; which
// ones this deployment agreed to run is.
func TestAdHocModeOffered(t *testing.T) {
	t.Parallel()

	a := adHocApp(t, func(c *config.Config) {
		c.API.AdHocURLs.ConsentModes = []model.ConsentMode{model.ConsentReject}
	})

	if !a.adHocModeOffered(model.ConsentReject) {
		t.Error("the configured mode is not offered")
	}

	if a.adHocModeOffered(model.ConsentAccept) {
		t.Error("a mode this deployment did not configure is offered")
	}

	if _, err := a.AcceptURL(t.Context(), "https://example.com/", model.ConsentAccept); err == nil ||
		!strings.Contains(err.Error(), "not offered") {
		t.Errorf("err = %v, want a mode that is not offered to be refused", err)
	}
}

// A button is easier to press than a schedule is to edit, and the site on the
// other end did not ask to be scanned at all (Tenet 17).
func TestAdHocCooldownRefusesAnAddressScannedTooRecently(t *testing.T) {
	t.Parallel()

	a := adHocApp(t, nil)

	target := config.Resolved{Name: "example.com", URL: "https://example.com/", MinInterval: time.Hour}

	// Nothing scanned yet: nothing to wait for.
	if err := a.adHocCooldown(target, model.ConsentReject); err != nil {
		t.Fatalf("a first scan was refused: %v", err)
	}

	seedResult(t, a.Store, "example.com", model.ConsentReject, time.Now().Add(-5*time.Minute))

	err := a.adHocCooldown(target, model.ConsentReject)
	if err == nil {
		t.Fatal("an address scanned five minutes ago was accepted again")
	}

	if !strings.Contains(err.Error(), "Try again in") {
		t.Errorf("err = %q, want it to say how long to wait", err)
	}

	// A different consent mode is a different series and is not held back by
	// this one.
	if err := a.adHocCooldown(target, model.ConsentAccept); err != nil {
		t.Errorf("another consent mode was refused: %v", err)
	}

	// And an older scan is no longer in the way.
	seedResult(t, a.Store, "old.example", model.ConsentReject, time.Now().Add(-2*time.Hour))

	old := config.Resolved{Name: "old.example", URL: "https://old.example/", MinInterval: time.Hour}
	if err := a.adHocCooldown(old, model.ConsentReject); err != nil {
		t.Errorf("a scan older than the interval was still refused: %v", err)
	}
}

func TestAdHocCooldownIsSkippedWithoutAMinimumInterval(t *testing.T) {
	t.Parallel()

	a := adHocApp(t, nil)

	seedResult(t, a.Store, "example.com", model.ConsentReject, time.Now())

	target := config.Resolved{Name: "example.com", URL: "https://example.com/"}
	if err := a.adHocCooldown(target, model.ConsentReject); err != nil {
		t.Errorf("err = %v, want no cooldown when none is configured", err)
	}
}

func seedResult(t *testing.T, st *store.Store, target string, mode model.ConsentMode, at time.Time) {
	t.Helper()

	res := &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        target + "-" + string(mode),
		Target:        target,
		URL:           "https://" + target + "/",
		ConsentMode:   mode,
		StartedAt:     at,
		FinishedAt:    at.Add(time.Second),
		Termination:   model.TermIdle,
	}

	if err := st.PutResult(res); err != nil {
		t.Fatalf("seeding a result: %v", err)
	}
}
