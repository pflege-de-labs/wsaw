package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/notify"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Story 5.37: a notifier's minimum confidence, against local fixture servers
// and fixture results only.

// networkChanged is the largest reason of the scan that prompted the story:
// service.pflege.de in accept mode on 2026-10-02, which scored 59 and reported
// sentry.io as a new pre-consent host because the page's own script failed to
// load.
const networkChanged = "8 of 114 network requests not observed, most often net::ERR_NETWORK_CHANGED"

func bandFor(score int) model.ConfidenceBand {
	switch {
	case score >= 90:
		return model.ConfidenceHigh
	case score >= 60:
		return model.ConfidenceMedium
	default:
		return model.ConfidenceLow
	}
}

func scoredResult(score int, mutate func(*model.Result)) *model.Result {
	return teamsResult(func(r *model.Result) {
		r.Confidence = &model.Confidence{
			Score: score,
			Band:  bandFor(score),
			Reasons: []model.ConfidenceReason{
				{Signal: model.SignalTermination, Observed: "the page went idle"},
				{Signal: model.SignalFetches, Points: 100 - score, Observed: networkChanged},
			},
		}

		if mutate != nil {
			mutate(r)
		}
	})
}

func sentryReport() *diff.Report {
	return teamsReport(change(diff.SeverityHigh, "sentry.io"))
}

// syncBuffer is a log sink the dispatcher's worker goroutine can write to
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []map[string]any

	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if l == "" {
			continue
		}

		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err == nil {
			out = append(out, m)
		}
	}

	return out
}

// gateRun is one dispatcher run with a webhook and a Teams notifier sharing
// the same confidence gate.
type gateRun struct {
	webhook    *capture
	teams      *capture
	logs       *syncBuffer
	suppressed map[string]int
}

func runGate(t *testing.T, gate notify.ConfidenceGate, res *model.Result, rep *diff.Report) *gateRun {
	t.Helper()

	run := &gateRun{
		webhook:    newCapture(http.StatusOK),
		teams:      newCapture(http.StatusAccepted),
		logs:       &syncBuffer{},
		suppressed: map[string]int{},
	}

	webhookSrv := run.webhook.serve()
	t.Cleanup(webhookSrv.Close)

	teamsSrv := run.teams.serve()
	t.Cleanup(teamsSrv.Close)

	wh, err := notify.NewWebhook(notify.Config{
		Name: "hook", URL: secret.Literal(webhookSrv.URL), MaxRetries: 1, Confidence: gate,
	}, webhookSrv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}

	tn := newTeams(t, teamsSrv, func(c *notify.TeamsConfig) { c.Confidence = gate })

	var mu sync.Mutex

	d := notify.NewDispatcher([]notify.Notifier{wh}, notify.DispatcherOptions{
		Logger:        slog.New(slog.NewJSONHandler(run.logs, nil)),
		ScanNotifiers: []notify.ScanNotifier{tn},
		OnSuppressed: func(notifier, reason string) {
			mu.Lock()
			defer mu.Unlock()

			run.suppressed[notifier+"/"+reason]++
		},
	})

	ctx, cancel := context.WithCancel(t.Context())

	d.Start(ctx)
	d.Publish(res, rep)

	// Cancelling drains the queue before Wait returns, so every delivery
	// has happened by the time the assertions run.
	cancel()
	d.Wait()

	return run
}

func (r *gateRun) webhookEvents(t *testing.T) []map[string]any {
	t.Helper()

	r.webhook.mu.Lock()
	defer r.webhook.mu.Unlock()

	out := make([]map[string]any, 0, len(r.webhook.bodies))

	for _, b := range r.webhook.bodies {
		var m map[string]any
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatalf("webhook body is not JSON: %v", err)
		}

		out = append(out, m)
	}

	return out
}

func (r *gateRun) cardText(t *testing.T) string {
	t.Helper()

	if r.teams.count() != 1 {
		t.Fatalf("teams received %d cards, want 1", r.teams.count())
	}

	return cardText(t, decodeCard(t, r.teams.last()))
}

func gate(minimum int, drop bool) notify.ConfidenceGate {
	return notify.ConfidenceGate{Enabled: true, Min: minimum, Drop: drop}
}

// AC1: with no minConfidence, a low-scoring scan is delivered exactly as
// before the story: trusted, with no confidence label.
func TestNoMinConfidenceLeavesDeliveryUnchanged(t *testing.T) {
	t.Parallel()

	cases := map[string]*model.Result{
		"high":         scoredResult(100, nil),
		"low":          scoredResult(59, nil),
		"not computed": teamsResult(nil),
	}

	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			run := runGate(t, notify.ConfidenceGate{}, res, sentryReport())

			text := run.cardText(t)
			if strings.Contains(text, "Low confidence") || strings.Contains(text, "not trustworthy") {
				t.Errorf("card changed without a threshold:\n%s", text)
			}

			for _, ev := range run.webhookEvents(t) {
				if _, ok := ev["belowConfidence"]; ok {
					t.Errorf("webhook event carries belowConfidence without a threshold: %v", ev)
				}
			}
		})
	}
}

// AC1, AC3, AC6: the boundary is "below", for both kinds alike.
func TestMinConfidenceBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		score int
		below bool
	}{
		{59, true},
		{60, false},
		{61, false},
	} {
		run := runGate(t, gate(60, false), scoredResult(tc.score, nil), sentryReport())

		text := run.cardText(t)
		if got := strings.Contains(text, "Low confidence"); got != tc.below {
			t.Errorf("score %d at minConfidence 60: card low confidence = %v, want %v\n%s",
				tc.score, got, tc.below, text)
		}

		events := run.webhookEvents(t)
		if len(events) != 1 {
			t.Fatalf("score %d: webhook received %d events, want 1", tc.score, len(events))
		}

		if got := events[0]["belowConfidence"] == true; got != tc.below {
			t.Errorf("score %d: webhook belowConfidence = %v, want %v", tc.score, got, tc.below)
		}
	}
}

// AC3 and AC10's fixture: the service.pflege.de case is posted with the
// caveat ahead of the finding, still naming its severity.
func TestCaveatLabelsTheFindingsOfALowConfidenceScan(t *testing.T) {
	t.Parallel()

	run := runGate(t, gate(60, false), scoredResult(59, nil), sentryReport())

	text := run.cardText(t)

	wantCaveat := "confidence 59 of 100, below this channel's 60: " + networkChanged
	for _, want := range []string{"Low confidence — Highest severity: HIGH — 1 change", wantCaveat, "sentry.io"} {
		if !strings.Contains(text, want) {
			t.Errorf("card does not contain %q:\n%s", want, text)
		}
	}

	if strings.Index(text, wantCaveat) > strings.Index(text, "sentry.io") {
		t.Errorf("the caveat must come before the findings:\n%s", text)
	}

	events := run.webhookEvents(t)
	if len(events) != 1 {
		t.Fatalf("webhook received %d events, want 1", len(events))
	}

	conf, _ := events[0]["confidence"].(map[string]any)
	if conf["score"] != float64(59) || conf["band"] != "low" || events[0]["belowConfidence"] != true {
		t.Errorf("webhook event confidence = %v, belowConfidence = %v", conf, events[0]["belowConfidence"])
	}
}

// AC3: a low-confidence scan with nothing worth reporting posts nothing; a
// flapping scan is not news on its own.
func TestCaveatStaysQuietWithoutQualifyingChanges(t *testing.T) {
	t.Parallel()

	run := runGate(t, gate(60, false), scoredResult(40, nil), teamsReport())

	if got := run.teams.count(); got != 0 {
		t.Errorf("teams posted %d cards for a low-confidence scan with no changes, want 0", got)
	}
}

// AC4: drop sends nothing, and says so in the log and the counter, once per
// held-back delivery.
func TestDropHoldsBackAndRecordsIt(t *testing.T) {
	t.Parallel()

	rep := teamsReport(change(diff.SeverityHigh, "sentry.io"), change(diff.SeverityMedium, "b.example"))

	run := runGate(t, gate(60, true), scoredResult(59, nil), rep)

	if run.teams.count() != 0 || run.webhook.count() != 0 {
		t.Fatalf("drop delivered %d cards and %d events, want none", run.teams.count(), run.webhook.count())
	}

	if got := run.suppressed["teams/low-confidence"]; got != 1 {
		t.Errorf("teams suppressions = %d, want 1 (one card)", got)
	}

	if got := run.suppressed["hook/low-confidence"]; got != 2 {
		t.Errorf("webhook suppressions = %d, want 2 (one per change)", got)
	}

	var held []map[string]any

	for _, l := range run.logs.lines() {
		if l["level"] == "WARN" && strings.HasPrefix(l["msg"].(string), "notification held back") {
			held = append(held, l)
		}
	}

	if len(held) != 3 {
		t.Fatalf("got %d held-back log lines, want 3", len(held))
	}

	for _, l := range held {
		for _, key := range []string{"scan_id", "target", "consent_mode", "notifier", "confidence", "min_confidence", "changes"} {
			if _, ok := l[key]; !ok {
				t.Errorf("held-back log line has no %q: %v", key, l)
			}
		}

		if l["notifier"] == "teams" && l["changes"] != float64(2) {
			t.Errorf("teams held-back line says %v changes, want 2", l["changes"])
		}
	}
}

// AC5: a scan that observed nothing is reported whatever the gate says.
func TestDropNeverHoldsBackAScanThatObservedNothing(t *testing.T) {
	t.Parallel()

	for _, term := range []model.TerminationReason{model.TermError, model.TermSkipped} {
		res := teamsResult(func(r *model.Result) {
			r.Termination = term
			r.Error = "browser did not start"
			r.Confidence = &model.Confidence{Score: 0, Band: model.ConfidenceNone}
		})

		run := runGate(t, gate(90, true), res, nil)

		text := run.cardText(t)
		if !strings.Contains(text, "not trustworthy") || strings.Contains(text, "Low confidence") {
			t.Errorf("%s: card should report the failure, not a confidence caveat:\n%s", term, text)
		}

		if len(run.suppressed) != 0 {
			t.Errorf("%s: suppressions recorded for a failed scan: %v", term, run.suppressed)
		}
	}
}

// AC2: a scan without a stored score is below any threshold, including 0.
func TestAMissingScoreIsBelowEveryThreshold(t *testing.T) {
	t.Parallel()

	run := runGate(t, gate(0, false), teamsResult(nil), sentryReport())

	text := run.cardText(t)
	if !strings.Contains(text, "confidence was not computed for this scan, and this channel requires 0 of 100") {
		t.Errorf("card does not state the missing score:\n%s", text)
	}

	drop := runGate(t, gate(0, true), teamsResult(nil), sentryReport())
	if drop.teams.count() != 0 || drop.webhook.count() != 0 {
		t.Errorf("a missing score was delivered under drop at minConfidence 0")
	}
}

// AC6: a scan the existing rules already distrust keeps its own caveat, even
// above the threshold.
func TestTheThresholdNeverOverridesAnExistingCaveat(t *testing.T) {
	t.Parallel()

	res := scoredResult(95, func(r *model.Result) {
		r.Consent = model.Consent{Outcome: model.OutcomeFailed, Reason: "no reject control found"}
	})

	run := runGate(t, gate(60, false), res, sentryReport())

	text := run.cardText(t)
	if !strings.Contains(text, "not trustworthy") || strings.Contains(text, "Low confidence") {
		t.Errorf("an untrustworthy scan above the threshold lost its caveat:\n%s", text)
	}
}

// AC6: the webhook and the Teams card agree on every scan in the table.
func TestWebhookAndTeamsAgree(t *testing.T) {
	t.Parallel()

	cases := map[string]*model.Result{
		"100":          scoredResult(100, nil),
		"75":           scoredResult(75, nil),
		"60":           scoredResult(60, nil),
		"59":           scoredResult(59, nil),
		"1":            scoredResult(1, nil),
		"not computed": teamsResult(nil),
	}

	for name, res := range cases {
		run := runGate(t, gate(60, false), res, sentryReport())

		teamsLow := strings.Contains(run.cardText(t), "Low confidence")
		webhookLow := run.webhookEvents(t)[0]["belowConfidence"] == true

		if teamsLow != webhookLow {
			t.Errorf("%s: teams low confidence = %v, webhook = %v", name, teamsLow, webhookLow)
		}
	}
}
