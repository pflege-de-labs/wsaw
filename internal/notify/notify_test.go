package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/notify"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

func event(sev diff.Severity) notify.Event {
	return notify.Event{
		Target:      "site",
		URL:         "https://example.com/",
		ConsentMode: "reject",
		Labels:      map[string]string{"team": "platform"},
		ScanID:      "scan-1",
		At:          time.Now(),
		Change: diff.Change{
			Type:     diff.HostAdded,
			Severity: sev,
			Subject:  "tracker.test",
			Detail:   "new third-party host",
		},
	}
}

func webhook(t *testing.T, cfg notify.Config, srv *httptest.Server) *notify.Webhook {
	t.Helper()

	cfg.URL = secret.Literal(srv.URL)
	if cfg.Name == "" {
		cfg.Name = "test"
	}

	w, err := notify.NewWebhook(cfg, srv.Client(), nil)
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}

	return w
}

func TestDeliverPostsJSON(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		body []byte
		hdr  http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		body, hdr = b, r.Header.Clone()
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := webhook(t, notify.Config{Headers: map[string]secret.Value{"X-Token": secret.Literal("abc")}}, srv)

	if err := wh.Deliver(context.Background(), event(diff.SeverityHigh)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var got notify.Event
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	if got.Target != "site" || got.Change.Subject != "tracker.test" {
		t.Errorf("payload lost data: %+v", got)
	}

	if hdr.Get("X-Token") != "abc" {
		t.Error("configured header was not sent")
	}
}

func TestFilteringBySeverityTargetLabelAndType(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := webhook(t, notify.Config{
		MinSeverity: diff.SeverityHigh,
		Targets:     []string{"site"},
		Labels:      map[string]string{"team": "platform"},
		ChangeTypes: []diff.ChangeType{diff.HostAdded},
	}, srv)

	if !wh.Wants(event(diff.SeverityCritical)) {
		t.Error("a matching event was filtered out")
	}

	if wh.Wants(event(diff.SeverityLow)) {
		t.Error("an event below the severity threshold was accepted")
	}

	other := event(diff.SeverityHigh)
	other.Target = "elsewhere"

	if wh.Wants(other) {
		t.Error("an event for another target was accepted")
	}

	wrongLabel := event(diff.SeverityHigh)
	wrongLabel.Labels = map[string]string{"team": "other"}

	if wh.Wants(wrongLabel) {
		t.Error("an event with a non-matching label was accepted")
	}

	wrongType := event(diff.SeverityHigh)
	wrongType.Change.Type = diff.CookieAdded

	if wh.Wants(wrongType) {
		t.Error("an event of an unwanted type was accepted")
	}
}

func TestRetriesOnServerError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := webhook(t, notify.Config{MaxRetries: 4, Timeout: time.Second}, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := wh.Deliver(ctx, event(diff.SeverityHigh)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestClientErrorIsNotRetried: retrying a malformed request only wastes time
// and buries the real problem.
func TestClientErrorIsNotRetried(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	wh := webhook(t, notify.Config{MaxRetries: 4, Timeout: time.Second}, srv)

	if err := wh.Deliver(context.Background(), event(diff.SeverityHigh)); err == nil {
		t.Fatal("a 400 was reported as success")
	}

	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a 400 is not retryable", got)
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := webhook(t, notify.Config{MaxRetries: 3, Timeout: time.Second}, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := wh.Deliver(ctx, event(diff.SeverityHigh)); err != nil {
		t.Fatalf("a 429 was not retried: %v", err)
	}
}

func TestTemplateRendering(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		body string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		body = string(b)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The Slack-shaped payload the stories call for, without dedicated code.
	wh := webhook(t, notify.Config{
		Template: `{"text": {{ printf "%s on %s: %s" .Change.Severity .Target .Change.Detail | json }}}`,
	}, srv)

	if err := wh.Deliver(context.Background(), event(diff.SeverityCritical)); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	var payload struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("template produced invalid JSON: %v (%s)", err, body)
	}

	if !strings.Contains(payload.Text, "critical on site") {
		t.Errorf("rendered text = %q", payload.Text)
	}
}

// TestTemplateJSONFuncEscapesUntrustedText: change details embed URLs from
// the scanned page, so a crafted one must not break out of the payload.
func TestTemplateJSONFuncEscapesUntrustedText(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		body string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		body = string(b)
		mu.Unlock()
	}))
	defer srv.Close()

	wh := webhook(t, notify.Config{Template: `{"text": {{ .Change.Detail | json }}}`}, srv)

	ev := event(diff.SeverityHigh)
	ev.Change.Detail = `host "evil", "injected": {"a":1}`

	if err := wh.Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("crafted detail broke the payload: %v (%s)", err, body)
	}

	if len(payload) != 1 {
		t.Errorf("crafted detail injected extra keys: %v", payload)
	}
}

func TestBrokenTemplateFailsAtStartup(t *testing.T) {
	t.Parallel()

	_, err := notify.NewWebhook(notify.Config{
		Name:     "bad",
		URL:      secret.Literal("https://example.com/hook"),
		Template: `{{ .Unclosed `,
	}, nil, nil)
	if err == nil {
		t.Fatal("a broken template was accepted; it would fail when the first alert fires")
	}
}

func TestMissingURLIsRejected(t *testing.T) {
	t.Parallel()

	if _, err := notify.NewWebhook(notify.Config{Name: "x"}, nil, nil); err == nil {
		t.Fatal("a notifier without a URL was accepted")
	}
}

// TestDeliveryFailureIsCounted: an alerting path that fails quietly is worse
// than no alerting at all.
func TestDeliveryFailureIsCounted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	var failed atomic.Int32

	wh := webhook(t, notify.Config{MaxRetries: 1, Timeout: time.Second}, srv)

	d := notify.NewDispatcher([]notify.Notifier{wh}, notify.DispatcherOptions{
		OnFailed: func() { failed.Add(1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	res := modelResult()
	d.Publish(&res, &diff.Report{
		Changes: []diff.Change{{Type: diff.HostAdded, Severity: diff.SeverityHigh, Subject: "x.test"}},
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && failed.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	d.Wait()

	if failed.Load() == 0 {
		t.Error("a failed delivery was not counted")
	}
}

func TestDispatcherDeliversAndCountsSuccess(t *testing.T) {
	t.Parallel()

	var received atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var sent atomic.Int32

	wh := webhook(t, notify.Config{Timeout: time.Second}, srv)

	d := notify.NewDispatcher([]notify.Notifier{wh}, notify.DispatcherOptions{
		OnSent: func() { sent.Add(1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	res := modelResult()
	d.Publish(&res, &diff.Report{Changes: []diff.Change{
		{Type: diff.HostAdded, Severity: diff.SeverityHigh, Subject: "a.test"},
		{Type: diff.HostAdded, Severity: diff.SeverityHigh, Subject: "b.test"},
	}})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sent.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	d.Wait()

	if got := sent.Load(); got != 2 {
		t.Errorf("sent = %d, want 2", got)
	}
}

func modelResult() model.Result {
	return model.Result{
		Target:      "site",
		URL:         "https://example.com/",
		ConsentMode: model.ConsentReject,
		ScanID:      "scan-1",
		StartedAt:   time.Now(),
	}
}
