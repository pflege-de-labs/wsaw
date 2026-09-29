package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
)

// isolationFallback is what startup had to change because the browser could
// not give every scan a browser context of its own.
type isolationFallback struct {
	// maxScansPerBrowser is the value browser.maxScansPerBrowser runs with,
	// whatever the file says.
	maxScansPerBrowser int64
}

// checkBrowserContexts learns at startup whether every scan can run in a
// browser context of its own, and when it cannot, overrides the settings that
// would let scans share a cookie jar.
//
// The pool enforces the fallback itself; the configuration is overridden as
// well so that what wsaw reports about itself — the config it runs with, the
// reload comparison — matches what it does.
func (a *App) checkBrowserContexts(ctx context.Context) {
	err := a.Pool.ProbeBrowserContexts(ctx)
	if err == nil {
		a.Logger.Info("each scan runs in a browser context of its own",
			"max_scans_per_browser", a.Config.Browser.MaxScansPerBrowser)

		return
	}

	configured := a.Config.Browser.MaxScansPerBrowser

	a.fallback = &isolationFallback{maxScansPerBrowser: a.Pool.MaxScansPerBrowser()}
	a.ApplyOverrides(a.Config)

	msg, instruction := a.fallbackAdvice(err)

	level := slog.LevelWarn
	if a.Config.Browser.SilenceContextFallbackWarning && a.Config.Browser.RemoteURL == "" {
		level = slog.LevelInfo
	}

	a.Logger.Log(ctx, level, msg,
		"error", err,
		"configured_max_scans_per_browser", configured,
		"max_scans_per_browser", a.fallback.maxScansPerBrowser,
		"overridden", configured != a.fallback.maxScansPerBrowser,
		"instruction", instruction,
	)
}

// fallbackAdvice says what happened and what the operator can do about it.
//
// A remote browser is different in kind: every pooled "browser" is an
// attachment to the same Chrome, so one scan per attachment isolates
// nothing. That warning is not silenced, because the fallback does not make
// wsaw's results trustworthy there — only the operator can.
func (a *App) fallbackAdvice(err error) (msg, instruction string) {
	refused := errors.Is(err, browser.ErrBrowserContextsUnavailable)

	switch {
	case a.Config.Browser.RemoteURL != "":
		return "the remote browser cannot give each scan a browser context of its own; scans on it share one cookie jar",
			"Point browser.remoteUrl at a browser that allows Target.createBrowserContext, " +
				"or remove it so wsaw launches its own browsers."

	case refused:
		return "the browser refuses to create browser contexts; each scan now gets a browser process of its own",
			"Allow browser contexts for this Chrome (a managed install forbids them with the IncognitoModeAvailability " +
				"policy), or run the browser in a container with browser.runtime: podman or docker, to let browsers " +
				"serve several scans. To keep one browser per scan and log this at info level, " +
				"set browser.silenceContextFallbackWarning: true."

	default:
		return "could not check whether the browser can create browser contexts; each scan now gets a browser process of its own",
			"See the error for why the probe browser did not start; wsaw checks again at the next start. " +
				"To log this at info level, set browser.silenceContextFallbackWarning: true."
	}
}

// ApplyOverrides applies what startup overrode to a configuration read later,
// so a reload compares like with like. Without it, a file that still says
// maxScansPerBrowser: 10 would differ from the 1 the daemon runs with, and
// every reload would be refused for a change nobody made.
func (a *App) ApplyOverrides(cfg *config.Config) {
	if a.fallback == nil || cfg == nil {
		return
	}

	cfg.Browser.MaxScansPerBrowser = a.fallback.maxScansPerBrowser
}
