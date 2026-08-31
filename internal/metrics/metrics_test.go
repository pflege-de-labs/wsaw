package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/metrics"
	"github.com/martint17r/wsaw/internal/model"
)

func result(ok bool) *model.Result {
	res := &model.Result{
		Target:      "site",
		ConsentMode: model.ConsentReject,
		Duration:    3 * time.Second,
		Termination: model.TermIdle,
		Consent:     model.Consent{Outcome: model.OutcomeApplied},
		Requests:    make([]model.Request, 42),
	}

	if !ok {
		res.Termination = model.TermError
		res.Error = "navigate failed"
		res.Consent.Outcome = model.OutcomeFailed
	}

	return res
}

func render(t *testing.T, r *metrics.Registry) string {
	t.Helper()

	var b strings.Builder

	if err := r.WritePrometheus(&b); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}

	return b.String()
}

// TestFailedScanDoesNotRefreshLiveness is the single most important
// behaviour here: if a failing scan updated the timestamp, the metric that
// exists to expose a stalled watcher would hide it instead.
func TestFailedScanDoesNotRefreshLiveness(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	r.ScanStarted("site", model.ConsentReject)
	r.ScanFinished("site", model.ConsentReject, result(false))

	stale := r.Stale(time.Now(), time.Hour)
	if len(stale) != 1 || !stale[0].NeverSucceeded {
		t.Fatalf("a target with only failed scans is not reported as stale: %+v", stale)
	}

	// After a success it is no longer stale.
	r.ScanFinished("site", model.ConsentReject, result(true))

	if got := r.Stale(time.Now(), time.Hour); len(got) != 0 {
		t.Errorf("still stale after a successful scan: %+v", got)
	}
}

func TestStaleDetectsAnAgedSuccess(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	r.ScanStarted("site", model.ConsentReject)
	r.ScanFinished("site", model.ConsentReject, result(true))

	// Looking from far enough in the future, the last success is too old.
	future := time.Now().Add(2 * time.Hour)

	got := r.Stale(future, time.Hour)
	if len(got) != 1 {
		t.Fatalf("stale = %+v, want one entry", got)
	}

	if got[0].NeverSucceeded {
		t.Error("an aged success was reported as never having succeeded")
	}
}

func TestPrometheusOutputHasLivenessMetric(t *testing.T) {
	t.Parallel()

	r := metrics.New("1.0.0")

	r.ScanStarted("site", model.ConsentReject)
	r.ScanFinished("site", model.ConsentReject, result(true))

	out := render(t, r)

	for _, want := range []string{
		"wsaw_last_successful_scan_timestamp_seconds{target=\"site\",consent_mode=\"reject\"}",
		"wsaw_scans_started_total{target=\"site\",consent_mode=\"reject\"} 1",
		"wsaw_scans_succeeded_total{target=\"site\",consent_mode=\"reject\"} 1",
		"wsaw_consent_outcome_total{target=\"site\",consent_mode=\"reject\",reason=\"applied\"} 1",
		"wsaw_build_info{version=\"1.0.0\"} 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestFailureReasonIsALabel(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	r.ScanFailed("site", model.ConsentReject, "timeout")
	r.ScanFailed("site", model.ConsentReject, "error")

	out := render(t, r)

	// Alerting must be able to tell a slow site from a broken browser.
	if !strings.Contains(out, `reason="timeout"`) || !strings.Contains(out, `reason="error"`) {
		t.Errorf("failure reasons are not distinguishable:\n%s", out)
	}
}

func TestHistogramsAreWellFormed(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	for range 3 {
		r.ScanFinished("site", model.ConsentReject, result(true))
	}

	out := render(t, r)

	if !strings.Contains(out, `wsaw_scan_duration_seconds_bucket{target="site",consent_mode="reject",le="5"}`) {
		t.Errorf("duration histogram buckets missing:\n%s", out)
	}

	if !strings.Contains(out, `le="+Inf"`) {
		t.Error("histogram has no +Inf bucket")
	}

	if !strings.Contains(out, "wsaw_scan_duration_seconds_count") {
		t.Error("histogram has no count series")
	}
}

func TestReadinessDistinguishesAliveFromUsable(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	if ready, reason := r.Ready(); ready {
		t.Errorf("a fresh registry reported ready: %s", reason)
	}

	r.SetReady(false, true)

	if ready, reason := r.Ready(); ready || !strings.Contains(reason, "Chrome") {
		t.Errorf("ready=%v reason=%q, want a Chrome complaint", ready, reason)
	}

	r.SetReady(true, true)

	if ready, _ := r.Ready(); !ready {
		t.Error("not ready with Chrome usable and config loaded")
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	for _, target := range []string{"zebra", "alpha", "middle"} {
		r.ScanStarted(target, model.ConsentReject)
		r.ScanFinished(target, model.ConsentReject, result(true))
	}

	// Scrape output that reorders between scrapes churns dashboards and
	// makes diffs useless.
	first := stripVolatile(render(t, r))
	second := stripVolatile(render(t, r))

	if first != second {
		t.Error("metrics output is not deterministically ordered")
	}

	alphaAt := strings.Index(first, `target="alpha"`)
	zebraAt := strings.Index(first, `target="zebra"`)

	if alphaAt < 0 || zebraAt < 0 || alphaAt > zebraAt {
		t.Error("series are not sorted by target")
	}
}

// stripVolatile removes the series that legitimately change between two
// scrapes, so the rest can be compared for stable ordering.
func stripVolatile(s string) string {
	var b strings.Builder

	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "wsaw_uptime_seconds") {
			continue
		}

		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

func TestCountersForBrowserAndNotifications(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	r.BrowserRestarted("site", "crashed")
	r.BrowserRestarted("site", "scan limit")
	r.NotifyFailed()
	r.NotifySent()
	r.SetQueueDepth(7)

	out := render(t, r)

	for _, want := range []string{
		"wsaw_browser_restarts_total 2",
		"wsaw_notifications_failed_total 1",
		"wsaw_notifications_sent_total 1",
		"wsaw_queue_depth 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
}

func TestNilResultIsIgnored(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	// Must not panic: a scan that produced nothing at all still calls through.
	r.ScanFinished("site", model.ConsentReject, nil)
}
