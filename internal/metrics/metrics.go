// Package metrics exposes wsaw's own health.
//
// The most important thing here is the per-target last-successful-scan
// timestamp: a watcher that silently stops watching is this product's worst
// failure mode, and it is only detectable from outside (Tenet 8).
//
// The Prometheus text format is small and stable, so it is rendered directly
// rather than pulling in a client library and its dependency tree — every
// dependency is a liability in a tool whose selling point is detecting
// supply-chain change (Tenet 19).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/martint17r/wsaw/internal/model"
)

// Registry holds wsaw's self-metrics. It is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex

	startedAt time.Time
	version   string

	scansStarted   map[labels]int64
	scansSucceeded map[labels]int64
	scansFailed    map[labels]int64

	consentOutcomes map[labels]int64

	browserRestarts int64
	notifyFailures  int64
	storeRetries    int64
	notifySent      int64

	durations map[labels]*histogram
	requests  map[labels]*histogram

	lastSuccess map[labels]time.Time
	lastAttempt map[labels]time.Time

	queueDepth int64

	// ready reports whether the daemon can actually scan.
	chromeUsable bool
	configLoaded bool
}

type labels struct {
	target string
	mode   model.ConsentMode
	reason string
}

// New creates a Registry.
func New(version string) *Registry {
	return &Registry{
		startedAt:       time.Now(),
		version:         version,
		scansStarted:    make(map[labels]int64),
		scansSucceeded:  make(map[labels]int64),
		scansFailed:     make(map[labels]int64),
		consentOutcomes: make(map[labels]int64),
		durations:       make(map[labels]*histogram),
		requests:        make(map[labels]*histogram),
		lastSuccess:     make(map[labels]time.Time),
		lastAttempt:     make(map[labels]time.Time),
	}
}

// ScanStarted records the beginning of a scan.
func (r *Registry) ScanStarted(target string, mode model.ConsentMode) {
	key := labels{target: target, mode: mode}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.scansStarted[key]++
	r.lastAttempt[key] = time.Now()
}

// ScanFinished records a completed scan and its shape.
func (r *Registry) ScanFinished(target string, mode model.ConsentMode, res *model.Result) {
	if res == nil {
		return
	}

	key := labels{target: target, mode: mode}

	r.mu.Lock()
	defer r.mu.Unlock()

	if res.OK() {
		r.scansSucceeded[key]++
		// Only a trustworthy scan updates the liveness timestamp. A failed
		// scan that refreshed it would hide exactly the condition this
		// metric exists to expose.
		r.lastSuccess[key] = time.Now()
	}

	r.histogramFor(r.durations, key, durationBuckets).observe(res.Duration.Seconds())
	r.histogramFor(r.requests, key, requestBuckets).observe(float64(len(res.Requests)))

	r.consentOutcomes[labels{target: target, mode: mode, reason: string(res.Consent.Outcome)}]++
}

// ScanFailed records a failure with its reason, so alerting can distinguish a
// timeout from a browser crash.
func (r *Registry) ScanFailed(target string, mode model.ConsentMode, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.scansFailed[labels{target: target, mode: mode, reason: reason}]++
}

// BrowserRestarted counts a browser replacement.
func (r *Registry) BrowserRestarted(_, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.browserRestarts++
}

// NotifySent and NotifyFailed count delivery outcomes, so a silently broken
// alerting path is visible.
func (r *Registry) NotifySent() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.notifySent++
}

// NotifyFailed counts a notification that could not be delivered.
func (r *Registry) NotifyFailed() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.notifyFailures++
}

// StoreRetried counts a store operation that had to be retried. A database
// that is flapping while every scan succeeds on the second attempt is a
// degradation worth alerting on before it becomes an outage (Story 4.7).
func (r *Registry) StoreRetried() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.storeRetries++
}

// SetQueueDepth records how many scans are waiting.
func (r *Registry) SetQueueDepth(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.queueDepth = int64(n)
}

// SetReady records readiness inputs.
func (r *Registry) SetReady(chromeUsable, configLoaded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.chromeUsable = chromeUsable
	r.configLoaded = configLoaded
}

// Ready reports whether wsaw can actually do its job, which is different from
// the process being alive.
func (r *Registry) Ready() (bool, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	switch {
	case !r.configLoaded:
		return false, "configuration is not loaded"
	case !r.chromeUsable:
		return false, "Chrome is not usable"
	default:
		return true, "ready"
	}
}

// Staleness describes a target that has not produced a successful scan
// recently, which is the alertable condition.
type Staleness struct {
	Target         string            `json:"target"`
	Mode           model.ConsentMode `json:"consentMode"`
	LastSuccess    time.Time         `json:"lastSuccess,omitempty"`
	LastAttempt    time.Time         `json:"lastAttempt,omitempty"`
	NeverSucceeded bool              `json:"neverSucceeded"`
}

// Stale returns targets whose last successful scan is older than maxAge, plus
// those that have never succeeded.
func (r *Registry) Stale(now time.Time, maxAge time.Duration) []Staleness {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Staleness

	for key, attempt := range r.lastAttempt {
		success, ok := r.lastSuccess[key]

		switch {
		case !ok:
			out = append(out, Staleness{
				Target: key.target, Mode: key.mode,
				LastAttempt: attempt, NeverSucceeded: true,
			})

		case maxAge > 0 && now.Sub(success) > maxAge:
			out = append(out, Staleness{
				Target: key.target, Mode: key.mode,
				LastSuccess: success, LastAttempt: attempt,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}

		return out[i].Mode < out[j].Mode
	})

	return out
}

func (r *Registry) histogramFor(m map[labels]*histogram, key labels, buckets []float64) *histogram {
	h, ok := m[key]
	if !ok {
		h = newHistogram(buckets)
		m[key] = h
	}

	return h
}

var (
	durationBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 45, 60, 120}
	requestBuckets  = []float64{10, 25, 50, 100, 200, 400, 800, 1600, 3000}
)

// WritePrometheus renders the registry in the Prometheus text exposition
// format.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder

	writeCounter(&b, "wsaw_scans_started_total", "Scans started.", r.scansStarted)
	writeCounter(&b, "wsaw_scans_succeeded_total", "Scans that produced a trustworthy asset list.", r.scansSucceeded)
	writeCounter(&b, "wsaw_scans_failed_total", "Scans that did not produce a trustworthy asset list, by reason.", r.scansFailed)
	writeCounter(&b, "wsaw_consent_outcome_total", "Consent interaction outcomes.", r.consentOutcomes)

	writeGaugeValue(&b, "wsaw_browser_restarts_total", "Browsers discarded and replaced.", float64(r.browserRestarts))
	writeGaugeValue(&b, "wsaw_notifications_sent_total", "Notifications delivered.", float64(r.notifySent))
	writeGaugeValue(&b, "wsaw_notifications_failed_total", "Notifications that could not be delivered.", float64(r.notifyFailures))
	writeGaugeValue(&b, "wsaw_store_retries_total", "Store operations retried after a transient failure.", float64(r.storeRetries))
	writeGaugeValue(&b, "wsaw_queue_depth", "Scans waiting to start.", float64(r.queueDepth))
	writeGaugeValue(&b, "wsaw_uptime_seconds", "Process uptime.", time.Since(r.startedAt).Seconds())
	writeGaugeValue(&b, "wsaw_ready", "1 when Chrome is usable and configuration is loaded.", boolValue(r.chromeUsable && r.configLoaded))

	fmt.Fprintf(&b, "# HELP wsaw_build_info Build information.\n# TYPE wsaw_build_info gauge\n")
	fmt.Fprintf(&b, "wsaw_build_info{version=%q} 1\n", r.version)

	// The metric that makes a stalled watcher alertable.
	b.WriteString("# HELP wsaw_last_successful_scan_timestamp_seconds Unix time of the last scan that produced a trustworthy asset list.\n")
	b.WriteString("# TYPE wsaw_last_successful_scan_timestamp_seconds gauge\n")

	for _, key := range sortedLabels(r.lastSuccess) {
		fmt.Fprintf(&b, "wsaw_last_successful_scan_timestamp_seconds{target=%q,consent_mode=%q} %d\n",
			key.target, key.mode, r.lastSuccess[key].Unix())
	}

	writeHistogram(&b, "wsaw_scan_duration_seconds", "Scan wall-clock duration.", r.durations)
	writeHistogram(&b, "wsaw_scan_requests", "Requests captured per scan.", r.requests)

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing metrics: %w", err)
	}

	return nil
}

func writeCounter(b *strings.Builder, name, help string, values map[labels]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)

	for _, key := range sortedLabels(values) {
		fmt.Fprintf(b, "%s%s %d\n", name, renderLabels(key), values[key])
	}
}

func writeGaugeValue(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

func writeHistogram(b *strings.Builder, name, help string, values map[labels]*histogram) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)

	for _, key := range sortedLabels(values) {
		h := values[key]
		base := renderLabels(key)

		cumulative := int64(0)

		for i, bound := range h.bounds {
			cumulative += h.counts[i]
			fmt.Fprintf(b, "%s_bucket%s %d\n", name, withLabel(base, "le", formatBound(bound)), cumulative)
		}

		cumulative += h.counts[len(h.counts)-1]
		fmt.Fprintf(b, "%s_bucket%s %d\n", name, withLabel(base, "le", "+Inf"), cumulative)
		fmt.Fprintf(b, "%s_sum%s %g\n", name, base, h.sum)
		fmt.Fprintf(b, "%s_count%s %d\n", name, base, cumulative)
	}
}

func sortedLabels[V any](m map[labels]V) []labels {
	out := make([]labels, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].target != out[j].target {
			return out[i].target < out[j].target
		}

		if out[i].mode != out[j].mode {
			return out[i].mode < out[j].mode
		}

		return out[i].reason < out[j].reason
	})

	return out
}

func renderLabels(l labels) string {
	parts := make([]string, 0, 3)

	if l.target != "" {
		parts = append(parts, fmt.Sprintf("target=%q", l.target))
	}

	if l.mode != "" {
		parts = append(parts, fmt.Sprintf("consent_mode=%q", l.mode))
	}

	if l.reason != "" {
		parts = append(parts, fmt.Sprintf("reason=%q", l.reason))
	}

	if len(parts) == 0 {
		return ""
	}

	return "{" + strings.Join(parts, ",") + "}"
}

func withLabel(base, name, value string) string {
	pair := fmt.Sprintf("%s=%q", name, value)

	if base == "" {
		return "{" + pair + "}"
	}

	return base[:len(base)-1] + "," + pair + "}"
}

func formatBound(v float64) string {
	return fmt.Sprintf("%g", v)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

// histogram is a fixed-bucket histogram.
type histogram struct {
	bounds []float64
	// counts has one more slot than bounds, for the overflow bucket.
	counts []int64
	sum    float64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]int64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	h.sum += v

	for i, bound := range h.bounds {
		if v <= bound {
			h.counts[i]++

			return
		}
	}

	h.counts[len(h.counts)-1]++
}
