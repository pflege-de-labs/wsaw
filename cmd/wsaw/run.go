package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/notify"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// cmdRun runs wsaw as a daemon.
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)

	var cf configFlags

	cf.register(fs)

	listen := fs.String("listen", "", "serve the API and web interface on this address")
	noAPI := fs.Bool("no-api", false, "do not serve the API or web interface")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cf.load()
	if err != nil {
		return err
	}

	if *listen != "" {
		cfg.API.Enabled = true
		cfg.API.Listen = *listen

		// Re-validate: enabling the API introduces the requirement that a
		// non-loopback listener must be authenticated.
		if err := cfg.Validate(); err != nil {
			return err
		}
	}

	if *noAPI {
		cfg.API.Enabled = false
	}

	a, err := app.New(ctx, cfg, app.Options{Version: version, RequireBrowser: true})
	if err != nil {
		return err
	}

	defer func() {
		if err := a.Close(); err != nil {
			a.Logger.Error("cleanup failed", "error", err)
		}
	}()

	return supervise(ctx, a, cf)
}

// supervise starts the scheduler, the notifier, the HTTP server and the
// reload watcher, and shuts them all down together.
func supervise(ctx context.Context, a *app.App, cf configFlags) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	dispatcher, err := buildDispatcher(a)
	if err != nil {
		return err
	}

	if dispatcher != nil {
		dispatcher.Start(runCtx)
	}

	// The target list is behind a mutex so a reload is visible to the HTTP
	// server without restarting it.
	var (
		targetsMu sync.RWMutex
		targets   = a.Targets
	)

	currentTargets := func() []config.Resolved {
		targetsMu.RLock()
		defer targetsMu.RUnlock()

		return targets
	}

	sink := daemon.SinkFunc(func(_ context.Context, out scanner.Outcome) {
		if dispatcher != nil {
			dispatcher.Publish(out.Result, out.Diff)
		}
	})

	d, err := daemon.New(a.Scanner, sink, a.Targets, daemon.Options{
		Concurrency:          a.Config.Concurrency(),
		PerOriginConcurrency: a.Config.Scheduler.PerOriginConcurrency,
		CatchUp:              a.Config.Scheduler.CatchUp,
		ShutdownGrace:        a.Config.Scheduler.ShutdownGrace.Or(30 * time.Second),
		FlapWindow:           a.Config.Detection.FlapWindow.Duration(),
		Logger:               a.Logger,
		OnQueueDepth:         a.Metrics.SetQueueDepth,
		OnRetry:              func(string, model.ConsentMode, int) { a.Metrics.ScanRetried() },
		OnRetriesExhausted: func(string, model.ConsentMode, int) {
			a.Metrics.ScanRetriesExhausted()
		},
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	errCh := make(chan error, 3)

	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := d.Run(runCtx); err != nil {
			errCh <- fmt.Errorf("scheduler: %w", err)
		}
	}()

	if a.Config.API.Enabled {
		srv, err := buildServer(a, d, currentTargets)
		if err != nil {
			return err
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := srv.Serve(runCtx); err != nil {
				errCh <- err
				cancel()
			}
		}()
	}

	// Retention runs on its own schedule: without it, a long-running daemon
	// grows without bound (NFR §1).
	wg.Add(1)

	go func() {
		defer wg.Done()

		a.PruneLoop(runCtx)
	}()

	// SIGHUP reloads configuration. An invalid new config is rejected and the
	// running one stays active (Story 3.4).
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	defer signal.Stop(hup)

	for {
		select {
		case <-runCtx.Done():
			wg.Wait()

			if dispatcher != nil {
				dispatcher.Wait()
			}

			select {
			case err := <-errCh:
				return err
			default:
				return nil
			}

		case err := <-errCh:
			cancel()
			wg.Wait()

			return err

		case <-hup:
			newTargets, err := reload(a, cf)
			if err != nil {
				a.Logger.Error("reload rejected, keeping the running configuration", "error", err)

				continue
			}

			targetsMu.Lock()
			targets = newTargets
			targetsMu.Unlock()

			d.Reload(newTargets)
		}
	}
}

// reload re-reads and re-validates configuration. It deliberately does not
// swap the store, browser pool, or notifiers: those are process-level
// resources, and silently rebuilding them on a signal would make a reload
// riskier than a restart.
func reload(a *app.App, cf configFlags) ([]config.Resolved, error) {
	cfg, err := cf.load()
	if err != nil {
		return nil, err
	}

	targets, err := cfg.ResolveTargets(a.Secrets)
	if err != nil {
		return nil, err
	}

	a.Logger.Info("configuration reloaded", "targets", len(targets))

	return targets, nil
}

func buildDispatcher(a *app.App) (*notify.Dispatcher, error) {
	if len(a.Config.Notify) == 0 {
		return nil, nil
	}

	client := &http.Client{Timeout: 30 * time.Second}

	notifiers := make([]notify.Notifier, 0, len(a.Config.Notify))

	var scanNotifiers []notify.ScanNotifier

	for _, n := range a.Config.Notify {
		common, err := resolveNotifier(a, n)
		if err != nil {
			return nil, err
		}

		switch n.NotifierKind() {
		case config.NotifyTeams:
			tn, err := notify.NewTeams(notify.TeamsConfig{
				Name:        n.Name,
				URL:         common.url,
				MinSeverity: common.minSeverity,
				Targets:     n.Targets,
				Labels:      n.Labels,
				Legacy:      n.TeamsFormat() == config.TeamsMessageCard,
				BaseURL:     n.BaseURL,
				MaxChanges:  n.MaxChanges,
				Headers:     common.headers,
				Timeout:     n.Timeout.Or(10 * time.Second),
				MaxRetries:  n.MaxRetries,
			}, client, a.Logger)
			if err != nil {
				return nil, err
			}

			if tn.Legacy() {
				a.Logger.Warn("notifier uses the retired Office 365 connector card format",
					"notifier", n.Name,
					"detail", "Microsoft retired connector webhooks on 30 April 2026; "+
						"move this notifier to a Power Automate Workflow webhook and remove format: messagecard")
			}

			scanNotifiers = append(scanNotifiers, tn)

		default:
			changeTypes := make([]diff.ChangeType, 0, len(n.ChangeTypes))
			for _, t := range n.ChangeTypes {
				changeTypes = append(changeTypes, diff.ChangeType(t))
			}

			wh, err := notify.NewWebhook(notify.Config{
				Name:        n.Name,
				URL:         common.url,
				MinSeverity: common.minSeverity,
				Targets:     n.Targets,
				Labels:      n.Labels,
				ChangeTypes: changeTypes,
				Template:    n.Template,
				Headers:     common.headers,
				Timeout:     n.Timeout.Or(10 * time.Second),
				MaxRetries:  n.MaxRetries,
			}, client, a.Logger)
			if err != nil {
				return nil, err
			}

			notifiers = append(notifiers, wh)
		}
	}

	return notify.NewDispatcher(notifiers, notify.DispatcherOptions{
		Logger:        a.Logger,
		OnSent:        a.Metrics.NotifySent,
		OnFailed:      a.Metrics.NotifyFailed,
		ScanNotifiers: scanNotifiers,
	}), nil
}

// resolvedNotifier holds the settings every notifier kind shares, after secret
// resolution.
type resolvedNotifier struct {
	url         secret.Value
	headers     map[string]secret.Value
	minSeverity diff.Severity
}

func resolveNotifier(a *app.App, n config.Notifier) (resolvedNotifier, error) {
	var out resolvedNotifier

	url, err := secret.Resolve(n.URL)
	if err != nil {
		return out, fmt.Errorf("notifier %q: url: %w", n.Name, err)
	}

	// Registered as a secret whatever the kind: a Workflow URL carries its
	// authorisation in the query string, so it must be redacted everywhere a
	// webhook token would be (Story 5.14, AC2).
	a.Secrets.Add(url)
	out.url = url

	out.headers = make(map[string]secret.Value, len(n.Headers))

	for name, ref := range n.Headers {
		v, err := secret.Resolve(ref)
		if err != nil {
			return out, fmt.Errorf("notifier %q: header %s: %w", n.Name, name, err)
		}

		a.Secrets.Add(v)
		out.headers[name] = v
	}

	if n.MinSeverity != "" {
		out.minSeverity, err = diff.ParseSeverity(n.MinSeverity)
		if err != nil {
			return out, fmt.Errorf("notifier %q: %w", n.Name, err)
		}
	}

	return out, nil
}

func buildServer(a *app.App, d *daemon.Daemon, targets func() []config.Resolved) (*httpapi.Server, error) {
	token, err := secret.Resolve(a.Config.API.Token)
	if err != nil {
		return nil, fmt.Errorf("api.token: %w", err)
	}

	a.Secrets.Add(token)

	webUI := true
	if a.Config.API.WebUI != nil {
		webUI = *a.Config.API.WebUI
	}

	allowAdHoc := true
	if a.Config.API.AllowAdHocScan != nil {
		allowAdHoc = *a.Config.API.AllowAdHocScan
	}

	return httpapi.New(httpapi.Options{
		Listen:         a.Config.API.Listen,
		Token:          token,
		TLSCert:        a.Config.API.TLSCert,
		TLSKey:         a.Config.API.TLSKey,
		WebUI:          webUI,
		ReadOnly:       a.Config.API.ReadOnly,
		AllowAdHocScan: allowAdHoc,
		RefreshDefault: a.Config.API.RefreshInterval.Duration(),
		MetricsEnabled: a.Config.Metrics.Enabled,
		MetricsPath:    a.Config.Metrics.Path,
		Version:        a.Version,
	}, httpapi.Deps{
		Store:      a.Store,
		Metrics:    a.Metrics,
		Daemon:     d,
		Trigger:    a,
		Rules:      a.Rules,
		Logger:     a.Logger,
		Targets:    targets,
		Running:    a.RunningScans,
		ConfigPath: a.Config.Path(),
	})
}
