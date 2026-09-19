package app

// Scanning a URL that is not in the target list, typed into the web interface
// (Story 5.27).
//
// Everything a configured target's scan goes through, a typed URL's scan goes
// through too — the same resolution, the same normalizer, the same robots
// policy, the same store. What is different is who chose the address, and
// that is what the checks here are about.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/adhoc"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// AcceptURL validates a typed URL and reports the target name its results are
// stored under, or why it will not be scanned. It starts nothing.
//
// The web interface asks before it redirects: a reader who typed something
// wsaw will not fetch should be told so on the page they typed it on, not by
// finding a failed result later.
func (a *App) AcceptURL(ctx context.Context, rawURL string, mode model.ConsentMode) (string, error) {
	t, err := a.adHocTarget(ctx, rawURL, mode)
	if err != nil {
		return "", err
	}

	return t.Name, nil
}

// ScanURL runs one scan of a typed URL, satisfying the web interface's seam.
//
// It repeats every check AcceptURL makes rather than trusting that one ran:
// this is also the API's path, and the interface's press is answered before
// the scan starts, so the address is re-examined at the moment it is about to
// be fetched.
func (a *App) ScanURL(ctx context.Context, rawURL string, mode model.ConsentMode) (scanner.Outcome, error) {
	t, err := a.adHocTarget(ctx, rawURL, mode)
	if err != nil {
		return scanner.Outcome{}, err
	}

	// Labelled as its own source: an operator looking at what is running
	// should be able to tell an address somebody typed from a target they
	// configured (Story 5.12).
	return a.Scanner.Scan(scanner.WithSource(ctx, scanner.SourceURL), t, mode)
}

// adHocTarget is the whole admission path: is this wsaw willing to do this at
// all, is the address one it will fetch, and is it too soon to ask the site
// again.
func (a *App) adHocTarget(ctx context.Context, rawURL string, mode model.ConsentMode) (config.Resolved, error) {
	if !a.Config.API.AdHocURLs.Enabled {
		return config.Resolved{}, errors.New("scanning a typed URL is not enabled on this wsaw")
	}

	if !a.adHocModeOffered(mode) {
		return config.Resolved{}, fmt.Errorf("consent mode %q is not offered for typed URLs", string(mode))
	}

	// Checked after the policy questions and before the address is examined:
	// what this deployment allows does not depend on whether a browser was
	// found, and a lookup should not happen for a scan that cannot run.
	if a.Scanner == nil {
		return config.Resolved{}, errors.New("scanning is not available in this mode")
	}

	name, err := adhoc.Accept(ctx, rawURL, adhoc.Policy{
		AllowPrivateHosts: a.Config.API.AdHocURLs.AllowPrivateHosts,
	})
	if err != nil {
		return config.Resolved{}, err
	}

	t := a.Config.ResolveAdHocTarget(name, rawURL, mode)

	if err := a.adHocCooldown(t, mode); err != nil {
		return config.Resolved{}, err
	}

	return t, nil
}

// adHocModeOffered reports whether the form offers this consent mode. A mode
// arrives in a request, so which modes exist is not the question — which ones
// this deployment agreed to run is.
func (a *App) adHocModeOffered(mode model.ConsentMode) bool {
	for _, m := range a.Config.AdHocModes() {
		if m == mode {
			return true
		}
	}

	return false
}

// adHocCooldown refuses a scan that would ask the same site again too soon.
//
// A button is easier to press than a schedule is to edit, and the site on the
// other end did not ask to be scanned at all. minInterval is the same floor
// the scheduler respects, applied here to the same address (Tenet 17).
func (a *App) adHocCooldown(t config.Resolved, mode model.ConsentMode) error {
	if t.MinInterval <= 0 {
		return nil
	}

	last, ok := a.LastScan(t.Name, mode)
	if !ok {
		return nil
	}

	wait := t.MinInterval - time.Since(last)
	if wait <= 0 {
		return nil
	}

	return fmt.Errorf("%s was scanned in %s mode %s ago; wsaw waits %s between scans of the same address. "+
		"Try again in %s",
		t.Name, string(mode), time.Since(last).Round(time.Second), t.MinInterval, wait.Round(time.Second))
}
