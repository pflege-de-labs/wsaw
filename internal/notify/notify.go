// Package notify delivers change events to webhooks.
//
// Delivery is best-effort but never silent: a notification that could not be
// delivered is logged and counted, because an alerting path that fails
// quietly is worse than having no alerting at all (Tenet 8, NFR §2).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Event is what a notifier delivers: one change plus enough context to act on
// it without another lookup.
type Event struct {
	Target      string            `json:"target"`
	URL         string            `json:"url"`
	ConsentMode model.ConsentMode `json:"consentMode"`
	Labels      map[string]string `json:"labels,omitempty"`

	ScanID string    `json:"scanId"`
	At     time.Time `json:"at"`

	Change diff.Change `json:"change"`
}

// Notifier delivers events. It is an interface because a second
// implementation — e-mail, a queue — is plausible (Tenet 12).
type Notifier interface {
	Name() string
	Deliver(ctx context.Context, ev Event) error
	Wants(ev Event) bool
}

// Config describes one webhook notifier.
type Config struct {
	Name string
	// URL is resolved from a secret reference, since webhook URLs carry
	// tokens in their path or query.
	URL secret.Value

	MinSeverity diff.Severity
	Targets     []string
	Labels      map[string]string
	ChangeTypes []diff.ChangeType

	// Template renders the payload. Empty sends the event as JSON.
	Template string
	Headers  map[string]secret.Value

	Timeout    time.Duration
	MaxRetries int
	QueueSize  int
}

// Webhook posts events to an HTTP endpoint.
type Webhook struct {
	cfg    Config
	client *http.Client
	tmpl   *template.Template
	log    *slog.Logger
}

// NewWebhook builds a webhook notifier. The template is compiled here so a
// broken template fails at startup rather than when the first alert fires.
func NewWebhook(cfg Config, client *http.Client, log *slog.Logger) (*Webhook, error) {
	if !cfg.URL.IsSet() {
		return nil, fmt.Errorf("notifier %q has no URL", cfg.Name)
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}

	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 4
	}

	w := &Webhook{cfg: cfg, client: client, log: log}

	if w.client == nil {
		w.client = &http.Client{Timeout: cfg.Timeout}
	}

	if w.log == nil {
		w.log = slog.Default()
	}

	if cfg.Template != "" {
		// text/template, not html/template: the output is a JSON payload for
		// a webhook, and HTML escaping would corrupt it. The values
		// interpolated are URLs and hostnames from a scanned page, so the
		// template is expected to place them inside JSON string literals.
		tmpl, err := template.New(cfg.Name).Funcs(templateFuncs()).Parse(cfg.Template)
		if err != nil {
			return nil, fmt.Errorf("notifier %q: parsing template: %w", cfg.Name, err)
		}

		w.tmpl = tmpl
	}

	return w, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// json renders a value as a JSON literal, which is how untrusted
		// strings are safely placed into a webhook payload.
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return "", fmt.Errorf("encoding template value: %w", err)
			}

			return string(b), nil
		},
	}
}

// Name identifies the notifier.
func (w *Webhook) Name() string { return w.cfg.Name }

// Wants reports whether this notifier is interested in an event.
func (w *Webhook) Wants(ev Event) bool {
	if w.cfg.MinSeverity != "" && !ev.Change.Severity.AtLeast(w.cfg.MinSeverity) {
		return false
	}

	if len(w.cfg.Targets) > 0 && !contains(w.cfg.Targets, ev.Target) {
		return false
	}

	if len(w.cfg.ChangeTypes) > 0 && !containsType(w.cfg.ChangeTypes, ev.Change.Type) {
		return false
	}

	for k, v := range w.cfg.Labels {
		if ev.Labels[k] != v {
			return false
		}
	}

	return true
}

// Deliver posts one event, retrying with exponential backoff.
func (w *Webhook) Deliver(ctx context.Context, ev Event) error {
	body, contentType, err := w.render(ev)
	if err != nil {
		return err
	}

	return retryPost(ctx, w.cfg.Name, w.cfg.MaxRetries, func(ctx context.Context) error {
		return w.post(ctx, body, contentType)
	})
}

// retryPost runs post with exponential backoff, stopping early on an error
// that another attempt cannot fix. It is shared by every notifier: a delivery
// path that retries differently depending on which channel it is would be a
// second thing to reason about during an outage.
func retryPost(ctx context.Context, name string, maxRetries int, post func(context.Context) error) error {
	if maxRetries <= 0 {
		maxRetries = 1
	}

	var lastErr error

	for attempt := range maxRetries {
		if attempt > 0 {
			// Backoff with a deterministic step; jitter is unnecessary here
			// because a single wsaw instance is not a thundering herd.
			delay := time.Duration(1<<uint(attempt-1)) * time.Second

			timer := time.NewTimer(delay)

			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()

				return fmt.Errorf("notifier %q: %w", name, ctx.Err())
			}

			timer.Stop()
		}

		err := post(ctx)
		if err == nil {
			return nil
		}

		lastErr = err

		if !retryable(err) {
			break
		}
	}

	return fmt.Errorf("notifier %q: delivery failed after %d attempts: %w",
		name, maxRetries, lastErr)
}

func (w *Webhook) render(ev Event) (body []byte, contentType string, err error) {
	if w.tmpl == nil {
		b, err := json.Marshal(ev)
		if err != nil {
			return nil, "", fmt.Errorf("notifier %q: encoding event: %w", w.cfg.Name, err)
		}

		return b, "application/json", nil
	}

	var buf bytes.Buffer

	if err := w.tmpl.Execute(&buf, ev); err != nil {
		return nil, "", fmt.Errorf("notifier %q: rendering template: %w", w.cfg.Name, err)
	}

	return buf.Bytes(), "application/json", nil
}

func (w *Webhook) post(ctx context.Context, body []byte, contentType string) error {
	reqCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.cfg.URL.Reveal(), bytes.NewReader(body))
	if err != nil {
		// The URL may contain a token, so it is never echoed into the error.
		return fmt.Errorf("building request: %w", scrubURL(err, w.cfg.URL))
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "wsaw")

	for name, value := range w.cfg.Headers {
		req.Header.Set(name, value.Reveal())
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting: %w", scrubURL(err, w.cfg.URL))
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode}
	}

	return nil
}

type httpError struct {
	status int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("endpoint returned status %d", e.status)
}

// retryable decides whether another attempt could plausibly succeed. A 4xx
// other than 429 means the request itself is wrong, so retrying only wastes
// time and hides the real problem.
func retryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status == http.StatusTooManyRequests || he.status >= 500
	}

	return true
}

// scrubURL removes the endpoint URL from an error, since it commonly embeds a
// token.
func scrubURL(err error, u secret.Value) error {
	if !u.IsSet() {
		return err
	}

	msg := strings.ReplaceAll(err.Error(), u.Reveal(), secret.RedactURL(u.Reveal()))

	return errors.New(msg)
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}

	return false
}

func containsType(list []diff.ChangeType, v diff.ChangeType) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}

	return false
}

// envelope carries either shape of event through the one queue, so that
// ordering and the drain-on-shutdown guarantee apply to both.
type envelope struct {
	change *Event
	scan   *ScanEvent
}

// Dispatcher fans events out to notifiers without blocking the scan that
// produced them.
type Dispatcher struct {
	notifiers []Notifier
	scanners  []ScanNotifier
	queue     chan envelope
	log       *slog.Logger

	onSent   func()
	onFailed func()

	wg   sync.WaitGroup
	once sync.Once
}

// DispatcherOptions configures a Dispatcher.
type DispatcherOptions struct {
	QueueSize int
	Logger    *slog.Logger
	OnSent    func()
	OnFailed  func()

	// ScanNotifiers receive one event per scan instead of one per change.
	ScanNotifiers []ScanNotifier
}

// NewDispatcher creates a Dispatcher and starts its worker.
func NewDispatcher(notifiers []Notifier, opts DispatcherOptions) *Dispatcher {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Dispatcher{
		notifiers: notifiers,
		scanners:  opts.ScanNotifiers,
		queue:     make(chan envelope, opts.QueueSize),
		log:       opts.Logger,
		onSent:    opts.OnSent,
		onFailed:  opts.OnFailed,
	}
}

// Start runs the delivery worker until ctx is cancelled.
func (d *Dispatcher) Start(ctx context.Context) {
	d.wg.Add(1)

	go func() {
		defer d.wg.Done()

		for {
			select {
			case <-ctx.Done():
				// Drain what is already queued so a shutdown does not silently
				// discard alerts that were about to be sent.
				d.drain(context.WithoutCancel(ctx))

				return

			case env := <-d.queue:
				// Cancellation and a queued event can both be ready, and
				// select picks between ready cases at random. Delivering with
				// the cancelled context would fail exactly the alerts the
				// drain below exists to save, so shutdown is re-checked here.
				if ctx.Err() != nil {
					drainCtx := context.WithoutCancel(ctx)

					d.deliver(drainCtx, env)
					d.drain(drainCtx)

					return
				}

				d.deliver(ctx, env)
			}
		}
	}()
}

func (d *Dispatcher) drain(ctx context.Context) {
	const drainBudget = 10 * time.Second

	drainCtx, cancel := context.WithTimeout(ctx, drainBudget)
	defer cancel()

	for {
		select {
		case ev := <-d.queue:
			d.deliver(drainCtx, ev)
		default:
			return
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, env envelope) {
	switch {
	case env.change != nil:
		d.deliverChange(ctx, *env.change)
	case env.scan != nil:
		d.deliverScan(ctx, *env.scan)
	}
}

func (d *Dispatcher) deliverScan(ctx context.Context, ev ScanEvent) {
	for _, n := range d.scanners {
		if !n.WantsScan(ev) {
			continue
		}

		if err := n.DeliverScan(ctx, ev); err != nil {
			d.log.Error("scan notification delivery failed",
				"notifier", n.Name(),
				"target", ev.Target,
				"consent_mode", string(ev.ConsentMode),
				"scan_id", ev.ScanID,
				"error", err,
			)

			if d.onFailed != nil {
				d.onFailed()
			}

			continue
		}

		if d.onSent != nil {
			d.onSent()
		}
	}
}

func (d *Dispatcher) deliverChange(ctx context.Context, ev Event) {
	for _, n := range d.notifiers {
		if !n.Wants(ev) {
			continue
		}

		if err := n.Deliver(ctx, ev); err != nil {
			// Never silent: the operator must be able to see that alerting
			// itself is broken.
			d.log.Error("notification delivery failed",
				"notifier", n.Name(),
				"target", ev.Target,
				"change", string(ev.Change.Type),
				"error", err,
			)

			if d.onFailed != nil {
				d.onFailed()
			}

			continue
		}

		if d.onSent != nil {
			d.onSent()
		}
	}
}

// Publish queues everything derived from a scan outcome: one event per change
// for the webhook notifiers, and one event for the whole scan for the
// scan-level ones.
func (d *Dispatcher) Publish(res *model.Result, rep *diff.Report) {
	if res == nil {
		return
	}

	if rep != nil {
		for _, c := range rep.Changes {
			change := Event{
				Target:      res.Target,
				URL:         res.URL,
				ConsentMode: res.ConsentMode,
				Labels:      res.Labels,
				ScanID:      res.ScanID,
				At:          res.StartedAt,
				Change:      c,
			}

			d.enqueue(envelope{change: &change},
				"change", string(c.Type), "severity", string(c.Severity))
		}
	}

	// The scan event is queued even with no diff at all: a failed scan has
	// nothing to compare and is exactly what a channel must still hear about.
	if len(d.scanners) > 0 {
		scan := NewScanEvent(res, rep)

		d.enqueue(envelope{scan: &scan},
			"scan_id", scan.ScanID, "changes", len(scan.Changes))
	}
}

func (d *Dispatcher) enqueue(env envelope, detail ...any) {
	select {
	case d.queue <- env:
	default:
		// A full queue is reported rather than silently dropped.
		d.log.Error("notification queue is full, event dropped", detail...)

		if d.onFailed != nil {
			d.onFailed()
		}
	}
}

// Wait blocks until the worker has stopped.
func (d *Dispatcher) Wait() {
	d.once.Do(func() { d.wg.Wait() })
}
