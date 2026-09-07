package notify_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/notify"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Story 5.14, AC10: the card structure is covered against a local fixture
// server. A Teams tenant is never required to run the test suite.

type capture struct {
	mu     sync.Mutex
	bodies []string

	status int
}

func newCapture(status int) *capture {
	return &capture{status: status}
}

func (c *capture) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()

		w.WriteHeader(c.status)
	}))
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.bodies)
}

func (c *capture) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.bodies) == 0 {
		return ""
	}

	return c.bodies[len(c.bodies)-1]
}

// message is the envelope a Power Automate Workflow expects.
type message struct {
	Type        string `json:"type"`
	Attachments []struct {
		ContentType string          `json:"contentType"`
		ContentURL  *string         `json:"contentUrl"`
		Content     json.RawMessage `json:"content"`
	} `json:"attachments"`
}

type card struct {
	Type    string            `json:"type"`
	Schema  string            `json:"$schema"`
	Version string            `json:"version"`
	Body    []json.RawMessage `json:"body"`
	Actions []struct {
		Type  string `json:"type"`
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"actions"`
}

func decodeCard(t *testing.T, body string) card {
	t.Helper()

	var msg message
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("the posted payload is not valid JSON: %v", err)
	}

	if msg.Type != "message" {
		t.Errorf("envelope type = %q, want \"message\"", msg.Type)
	}

	if len(msg.Attachments) != 1 {
		t.Fatalf("envelope carries %d attachments, want 1", len(msg.Attachments))
	}

	if got := msg.Attachments[0].ContentType; got != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("attachment contentType = %q, want the Adaptive Card type", got)
	}

	var c card
	if err := json.Unmarshal(msg.Attachments[0].Content, &c); err != nil {
		t.Fatalf("the card is not valid JSON: %v", err)
	}

	if c.Type != "AdaptiveCard" {
		t.Errorf("card type = %q, want AdaptiveCard", c.Type)
	}

	if c.Version == "" || c.Schema == "" {
		t.Errorf("card omits its schema or version: %+v", c)
	}

	return c
}

// cardText flattens every rendered string in the card, which is what a reader
// actually sees.
func cardText(t *testing.T, c card) string {
	t.Helper()

	var b strings.Builder

	for _, raw := range c.Body {
		var block struct {
			Text  string `json:"text"`
			Facts []struct {
				Title string `json:"title"`
				Value string `json:"value"`
			} `json:"facts"`
		}

		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatalf("a card block is not an object: %v", err)
		}

		b.WriteString(block.Text)
		b.WriteString("\n")

		for _, f := range block.Facts {
			b.WriteString(f.Title + ": " + f.Value + "\n")
		}
	}

	return b.String()
}

func teamsResult(mutate func(*model.Result)) *model.Result {
	res := &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        "scan-abc",
		Target:        "site",
		URL:           "https://example.com/",
		ConsentMode:   model.ConsentReject,
		StartedAt:     time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		FinishedAt:    time.Date(2026, 9, 1, 10, 0, 14, 0, time.UTC),
		Duration:      14 * time.Second,
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied, CMP: "consentmanager"},
		Requests:      []model.Request{},
	}

	if mutate != nil {
		mutate(res)
	}

	return res
}

func teamsReport(changes ...diff.Change) *diff.Report {
	return &diff.Report{
		Target:      "site",
		ConsentMode: model.ConsentReject,
		Changes:     changes,
		Comparable:  true,
	}
}

func change(sev diff.Severity, subject string) diff.Change {
	return diff.Change{
		Type:        diff.HostAdded,
		Severity:    sev,
		Target:      "site",
		ConsentMode: model.ConsentReject,
		Subject:     subject,
		Domain:      subject,
		Party:       model.ThirdParty,
		Phase:       model.PhasePre,
		Detail:      "contacted before any consent decision",
	}
}

func newTeams(t *testing.T, srv *httptest.Server, mutate func(*notify.TeamsConfig)) *notify.Teams {
	t.Helper()

	cfg := notify.TeamsConfig{
		Name:       "teams",
		URL:        secret.Literal(srv.URL),
		MaxRetries: 1,
		BaseURL:    "https://wsaw.example.com",
	}

	if mutate != nil {
		mutate(&cfg)
	}

	tn, err := notify.NewTeams(cfg, srv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}

	return tn
}

func TestTeamsPostsOneCardPerScan(t *testing.T) {
	t.Parallel()

	// 202 Accepted, which is what a Workflow answers. Treating it as a failure
	// would retry a message that was delivered (AC3).
	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, nil)

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(
		change(diff.SeverityHigh, "tracker.example"),
		change(diff.SeverityLow, "cdn.example"),
	))

	if err := tn.DeliverScan(t.Context(), ev); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}

	if got := hits.count(); got != 1 {
		t.Fatalf("two changes produced %d posts, want exactly 1 (AC4)", got)
	}

	c := decodeCard(t, hits.last())
	text := cardText(t, c)

	// AC5: the reviewer's first four questions, answered before the changes.
	for _, want := range []string{
		"site", "reject", "HIGH", "applied", "consentmanager",
		"tracker.example", "cdn.example",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the card does not mention %q:\n%s", want, text)
		}
	}

	// AC6: links back, and says it is a summary.
	if len(c.Actions) != 1 || c.Actions[0].Type != "Action.OpenUrl" {
		t.Fatalf("the card carries no link back to the result: %+v", c.Actions)
	}

	if want := "https://wsaw.example.com/results/site/reject/scan-abc"; c.Actions[0].URL != want {
		t.Errorf("link = %q, want %q", c.Actions[0].URL, want)
	}

	if !strings.Contains(text, "summary") {
		t.Error("the card does not state that it is a summary (AC6)")
	}
}

// AC5: severity must survive the loss of colour.
func TestTeamsStatesSeverityAsText(t *testing.T) {
	t.Parallel()

	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, nil)

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityCritical, "tracker.example")))

	if err := tn.DeliverScan(t.Context(), ev); err != nil {
		t.Fatal(err)
	}

	text := cardText(t, decodeCard(t, hits.last()))

	if !strings.Contains(text, "CRITICAL") {
		t.Errorf("severity is conveyed only by colour:\n%s", text)
	}
}

// AC4: the count in the card is the true total, not the number that fitted.
func TestTeamsReportsTheTrueTotalWhenItOmitsChanges(t *testing.T) {
	t.Parallel()

	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.MaxChanges = 3 })

	changes := make([]diff.Change, 0, 40)
	for i := range 40 {
		changes = append(changes, change(diff.SeverityMedium, fmt.Sprintf("host-%02d.example", i)))
	}

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(changes...))

	if err := tn.DeliverScan(t.Context(), ev); err != nil {
		t.Fatal(err)
	}

	text := cardText(t, decodeCard(t, hits.last()))

	if !strings.Contains(text, "37 more") {
		t.Errorf("the card does not say how many changes it omitted:\n%s", text)
	}

	if !strings.Contains(text, "of 40 in total") {
		t.Errorf("the card does not report the true total:\n%s", text)
	}

	if strings.Contains(text, "host-39.example") {
		t.Error("the card listed more changes than its cap allows")
	}
}

// AC8: the payload stays inside what Teams accepts, and the count it reports
// is still the truth after shrinking.
func TestTeamsBoundsThePayloadSize(t *testing.T) {
	t.Parallel()

	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.MaxChanges = 5000 })

	changes := make([]diff.Change, 0, 5000)

	for i := range 5000 {
		changes = append(changes, change(diff.SeverityMedium,
			fmt.Sprintf("host-%04d.a-fairly-long-domain-name-to-make-the-card-big.example", i)))
	}

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(changes...))

	if err := tn.DeliverScan(t.Context(), ev); err != nil {
		t.Fatalf("delivery of a very large diff failed: %v", err)
	}

	body := hits.last()

	if len(body) > 28*1024 {
		t.Errorf("posted payload is %d bytes, above what Teams accepts", len(body))
	}

	text := cardText(t, decodeCard(t, body))

	if !strings.Contains(text, "of 5000 in total") {
		t.Errorf("after shrinking, the card no longer reports the true total:\n%s", text)
	}
}

// AC7: a scan whose result cannot be trusted must not read as a clean one.
func TestTeamsReportsAnUntrustworthyScanAsSuch(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		mutate func(*model.Result)
		want   string
	}{
		"failed": {
			mutate: func(r *model.Result) {
				r.Termination = model.TermError
				r.Error = "the browser crashed"
			},
			want: "failed",
		},
		"skipped": {
			mutate: func(r *model.Result) {
				r.Termination = model.TermSkipped
				r.Error = "disallowed by robots.txt"
			},
			want: "did not run",
		},
		"truncated": {
			mutate: func(r *model.Result) { r.Termination = model.TermTimeout },
			want:   "cut short",
		},
		"consent failed": {
			mutate: func(r *model.Result) {
				r.Consent = model.Consent{Outcome: model.OutcomeFailed, Reason: "no banner found"}
			},
			want: "consent interaction failed",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			hits := newCapture(http.StatusAccepted)
			srv := hits.serve()

			t.Cleanup(srv.Close)

			tn := newTeams(t, srv, nil)

			// No changes at all: without the caveat this would read as a clean
			// scan with nothing to report.
			ev := notify.NewScanEvent(teamsResult(tc.mutate), teamsReport())

			if !tn.WantsScan(ev) {
				t.Fatal("an untrustworthy scan was filtered out; silence about it reads as a clean result")
			}

			if err := tn.DeliverScan(t.Context(), ev); err != nil {
				t.Fatal(err)
			}

			text := cardText(t, decodeCard(t, hits.last()))

			if !strings.Contains(text, tc.want) {
				t.Errorf("the card does not disclose %q:\n%s", tc.want, text)
			}

			if !strings.Contains(text, "not trustworthy") {
				t.Errorf("the card does not head its result as untrustworthy:\n%s", text)
			}
		})
	}
}

// The termination is named only when it is what went wrong. A scan that
// reached network idle but failed its consent interaction is not "not
// trustworthy — idle": that reads as though idling were the problem.
func TestTeamsNamesTheTerminationOnlyWhenItIsTheProblem(t *testing.T) {
	t.Parallel()

	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, nil)

	consentFailed := notify.NewScanEvent(teamsResult(func(r *model.Result) {
		r.Consent = model.Consent{Outcome: model.OutcomeFailed, Reason: "no banner found"}
	}), teamsReport())

	if err := tn.DeliverScan(t.Context(), consentFailed); err != nil {
		t.Fatal(err)
	}

	if text := cardText(t, decodeCard(t, hits.last())); strings.Contains(text, "trustworthy — idle") {
		t.Errorf("a consent failure is blamed on the termination:\n%s", text)
	}

	truncated := notify.NewScanEvent(teamsResult(func(r *model.Result) {
		r.Termination = model.TermTimeout
	}), teamsReport())

	if err := tn.DeliverScan(t.Context(), truncated); err != nil {
		t.Fatal(err)
	}

	if text := cardText(t, decodeCard(t, hits.last())); !strings.Contains(text, "trustworthy — timeout") {
		t.Errorf("a truncated scan does not name the termination that cut it short:\n%s", text)
	}
}

// A clean scan with nothing to say must not post: a channel that reports
// every scan gets muted, and then it reports nothing at all.
func TestTeamsStaysQuietForACleanScanWithNoChanges(t *testing.T) {
	t.Parallel()

	srv := newCapture(http.StatusAccepted).serve()
	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, nil)

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport())

	if tn.WantsScan(ev) {
		t.Error("a clean scan with no changes would post a card")
	}
}

func TestTeamsHonoursMinSeverity(t *testing.T) {
	t.Parallel()

	srv := newCapture(http.StatusAccepted).serve()
	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.MinSeverity = diff.SeverityHigh })

	low := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityLow, "cdn.example")))
	if tn.WantsScan(low) {
		t.Error("a low-severity scan passed a high-severity threshold")
	}

	high := notify.NewScanEvent(teamsResult(nil), teamsReport(
		change(diff.SeverityLow, "cdn.example"),
		change(diff.SeverityHigh, "tracker.example"),
	))

	if !tn.WantsScan(high) {
		t.Error("a scan containing a high-severity change was filtered out")
	}
}

// AC3: every 2xx is a success. A 202 that retried would post the same alert
// four times.
func TestTeamsTreatsEveryTwoHundredAsSuccess(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent} {
		hits := newCapture(status)
		srv := hits.serve()

		tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.MaxRetries = 4 })

		ev := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityHigh, "tracker.example")))

		if err := tn.DeliverScan(t.Context(), ev); err != nil {
			t.Errorf("status %d treated as a failure: %v", status, err)
		}

		if got := hits.count(); got != 1 {
			t.Errorf("status %d produced %d posts, want 1", status, got)
		}

		srv.Close()
	}
}

// AC2: the URL carries its authorisation in the query string, so it must not
// reach an error message.
func TestTeamsDoesNotLeakItsWebhookURL(t *testing.T) {
	t.Parallel()

	const token = "sig=super-secret-signature"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, func(c *notify.TeamsConfig) {
		c.URL = secret.Literal(srv.URL + "/workflow?" + token)
		c.MaxRetries = 1
	})

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityHigh, "tracker.example")))

	err := tn.DeliverScan(t.Context(), ev)
	if err == nil {
		t.Fatal("a 500 was reported as a success")
	}

	if strings.Contains(err.Error(), token) {
		t.Errorf("the webhook's authorisation leaked into an error: %v", err)
	}
}

// AC9: the retired format is reachable, and is not what an unconfigured
// notifier sends.
func TestTeamsLegacyFormatIsOptIn(t *testing.T) {
	t.Parallel()

	hits := newCapture(http.StatusOK)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.Legacy = true })

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityHigh, "tracker.example")))

	if err := tn.DeliverScan(t.Context(), ev); err != nil {
		t.Fatal(err)
	}

	var legacy struct {
		Type     string `json:"@type"`
		Summary  string `json:"summary"`
		Sections []struct {
			Title string `json:"activityTitle"`
			Text  string `json:"text"`
		} `json:"sections"`
	}

	if err := json.Unmarshal([]byte(hits.last()), &legacy); err != nil {
		t.Fatalf("the legacy payload is not valid JSON: %v", err)
	}

	if legacy.Type != "MessageCard" {
		t.Errorf("legacy payload type = %q, want MessageCard", legacy.Type)
	}

	if len(legacy.Sections) != 1 || !strings.Contains(legacy.Sections[0].Text, "tracker.example") {
		t.Errorf("the legacy card does not carry the change: %+v", legacy)
	}

	// And the default is not this.
	def := newTeams(t, srv, nil)

	if err := def.DeliverScan(t.Context(), ev); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(hits.last(), "MessageCard") {
		t.Error("the retired format is the default")
	}
}

// A link is omitted rather than invented when no base URL is configured, and
// the card says so.
func TestTeamsWithoutABaseURLSaysThereIsNoLink(t *testing.T) {
	t.Parallel()

	hits := newCapture(http.StatusAccepted)
	srv := hits.serve()

	t.Cleanup(srv.Close)

	tn := newTeams(t, srv, func(c *notify.TeamsConfig) { c.BaseURL = "" })

	ev := notify.NewScanEvent(teamsResult(nil), teamsReport(change(diff.SeverityHigh, "tracker.example")))

	if err := tn.DeliverScan(t.Context(), ev); err != nil {
		t.Fatal(err)
	}

	c := decodeCard(t, hits.last())

	if len(c.Actions) != 0 {
		t.Errorf("a link was produced without a configured base URL: %+v", c.Actions)
	}

	if !strings.Contains(cardText(t, c), "no link") {
		t.Error("the card does not explain why it has no link")
	}
}

func TestTeamsRejectsAnUnusableBaseURL(t *testing.T) {
	t.Parallel()

	srv := newCapture(http.StatusAccepted).serve()
	t.Cleanup(srv.Close)

	for _, bad := range []string{"wsaw.example.com", "ftp://wsaw.example.com", "/results"} {
		_, err := notify.NewTeams(notify.TeamsConfig{
			Name:    "teams",
			URL:     secret.Literal(srv.URL),
			BaseURL: bad,
		}, srv.Client(), nil)

		if err == nil {
			t.Errorf("baseUrl %q was accepted", bad)
		}
	}
}

// The dispatcher must give each notifier the shape it asked for: one event per
// change to the webhook, one event per scan to Teams.
func TestDispatcherPublishesBothShapes(t *testing.T) {
	t.Parallel()

	webhookHits := newCapture(http.StatusOK)
	webhookSrv := webhookHits.serve()

	t.Cleanup(webhookSrv.Close)

	teamsHits := newCapture(http.StatusAccepted)
	teamsSrv := teamsHits.serve()

	t.Cleanup(teamsSrv.Close)

	wh, err := notify.NewWebhook(notify.Config{
		Name: "hook", URL: secret.Literal(webhookSrv.URL), MaxRetries: 1,
	}, webhookSrv.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}

	tn := newTeams(t, teamsSrv, nil)

	d := notify.NewDispatcher([]notify.Notifier{wh}, notify.DispatcherOptions{
		ScanNotifiers: []notify.ScanNotifier{tn},
	})

	ctx, cancel := context.WithCancel(t.Context())

	d.Start(ctx)

	d.Publish(teamsResult(nil), teamsReport(
		change(diff.SeverityHigh, "a.example"),
		change(diff.SeverityHigh, "b.example"),
		change(diff.SeverityHigh, "c.example"),
	))

	cancel()
	d.Wait()

	if got := webhookHits.count(); got != 3 {
		t.Errorf("webhook received %d events, want one per change (3)", got)
	}

	if got := teamsHits.count(); got != 1 {
		t.Errorf("teams received %d cards, want one per scan (1)", got)
	}
}
