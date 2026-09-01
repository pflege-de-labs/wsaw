// Microsoft Teams delivery (Story 5.14).
//
// Teams is not just another templated webhook any more. Microsoft retired the
// Office 365 connector webhooks that a `MessageCard` template targeted; new
// integrations post to a Power Automate Workflow webhook, which expects an
// Adaptive Card inside a message envelope.
//
// The card is built in Go rather than in a user-supplied template on purpose.
// A malformed card fails *silently*: the Workflow returns success and posts
// nothing, or posts an empty card. An alert path that reports itself healthy
// while delivering nothing is the failure mode this product exists least to
// tolerate (Tenet 8), and it is not something an operator can be expected to
// catch by eyeballing a template.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/martint17r/wsaw/internal/diff"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/secret"
)

// The card format this notifier emits. Documented so a Teams administrator can
// see exactly what will be posted (AC1). 1.5 is the highest version Teams
// renders in a Workflow-posted card.
const (
	adaptiveCardSchema  = "http://adaptivecards.io/schemas/adaptive-card.json"
	adaptiveCardVersion = "1.5"
	adaptiveCardType    = "application/vnd.microsoft.card.adaptive"
)

// Adaptive Card field and palette names used in more than one place.
const (
	fieldType     = "type"
	colourDefault = "Default"
)

// maxCardBytes bounds the posted payload. Teams rejects a message larger than
// roughly 28 KB, and a rejection is a delivery failure rather than a partial
// post, so the notifier shrinks the card itself instead of finding out
// afterwards (AC8).
const maxCardBytes = 26 * 1024

// defaultMaxChanges is how many changes one card lists before it starts saying
// how many it left out. A card is a summary, not a report.
const defaultMaxChanges = 12

// TeamsConfig configures the Teams notifier.
type TeamsConfig struct {
	Name string

	// URL is the Power Automate Workflow webhook. It is a secret reference
	// like any other: a Workflow URL carries its authorisation in the query
	// string, so treating it as ordinary configuration would leak it into
	// logs, errors and results (AC2).
	URL secret.Value

	MinSeverity diff.Severity
	Targets     []string
	Labels      map[string]string

	// Legacy posts the retired MessageCard format, for a tenant still running
	// an Office 365 connector webhook. Deprecated, opt-in, never the default
	// (AC9).
	Legacy bool

	// BaseURL is wsaw's externally reachable web interface, used to link back
	// to the full result. Empty means no link, and the card says so.
	BaseURL string

	// MaxChanges caps how many changes the card lists.
	MaxChanges int

	Headers map[string]secret.Value

	Timeout    time.Duration
	MaxRetries int
}

// Teams posts one card per scan to a Teams webhook.
type Teams struct {
	cfg    TeamsConfig
	client *http.Client
	log    *slog.Logger
}

// NewTeams builds the notifier, validating what can be validated before the
// first alert has to fire.
func NewTeams(cfg TeamsConfig, client *http.Client, log *slog.Logger) (*Teams, error) {
	if !cfg.URL.IsSet() {
		return nil, fmt.Errorf("notifier %q has no url", cfg.Name)
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}

	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 4
	}

	if cfg.MaxChanges <= 0 {
		cfg.MaxChanges = defaultMaxChanges
	}

	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf(
				"notifier %q: baseUrl must be an absolute http or https URL such as \"https://wsaw.example.com\", got %q",
				cfg.Name, cfg.BaseURL)
		}

		cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	}

	t := &Teams{cfg: cfg, client: client, log: log}

	if t.client == nil {
		t.client = &http.Client{Timeout: cfg.Timeout}
	}

	if t.log == nil {
		t.log = slog.Default()
	}

	return t, nil
}

// Name identifies the notifier.
func (t *Teams) Name() string { return t.cfg.Name }

// Legacy reports whether this notifier posts the retired connector format, so
// the caller can warn about it once at startup rather than on every delivery.
func (t *Teams) Legacy() bool { return t.cfg.Legacy }

// WantsScan applies the shared scan-level filter.
func (t *Teams) WantsScan(ev ScanEvent) bool {
	return wantsScanEvent(ev, t.cfg.MinSeverity, t.cfg.Targets, t.cfg.Labels)
}

// DeliverScan posts one card.
func (t *Teams) DeliverScan(ctx context.Context, ev ScanEvent) error {
	body, err := t.payload(ev)
	if err != nil {
		return err
	}

	return retryPost(ctx, t.cfg.Name, t.cfg.MaxRetries, func(ctx context.Context) error {
		return t.post(ctx, body)
	})
}

// payload builds the request body, shrinking the card until it fits and
// verifying that what came out is valid JSON (AC8).
func (t *Teams) payload(ev ScanEvent) ([]byte, error) {
	listed := t.cfg.MaxChanges

	for {
		body, err := t.encode(ev, listed)
		if err != nil {
			return nil, err
		}

		if len(body) <= maxCardBytes {
			// Belt and braces. The card is assembled from Go values, so it is
			// valid by construction — but "valid by construction" is exactly
			// the assumption that lets a silent-failure bug ship, and the
			// check costs nothing next to an HTTP round trip.
			if !json.Valid(body) {
				return nil, fmt.Errorf("notifier %q: built an invalid JSON payload; refusing to post it", t.cfg.Name)
			}

			return body, nil
		}

		if listed == 0 {
			// Nothing left to drop: the card's fixed part alone is too large,
			// which means something about this scan is pathological. A
			// delivery failure is the honest outcome — it is logged and
			// counted rather than posted half-formed.
			return nil, fmt.Errorf(
				"notifier %q: the card is %d bytes with no changes listed, above the %d-byte limit",
				t.cfg.Name, len(body), maxCardBytes)
		}

		listed /= 2

		t.log.Warn("teams card too large, listing fewer changes",
			"notifier", t.cfg.Name, "scan_id", ev.ScanID, "listed", listed)
	}
}

func (t *Teams) encode(ev ScanEvent, listed int) ([]byte, error) {
	var payload any
	if t.cfg.Legacy {
		payload = t.messageCard(ev, listed)
	} else {
		payload = t.message(ev, listed)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("notifier %q: encoding the card: %w", t.cfg.Name, err)
	}

	return body, nil
}

func (t *Teams) post(ctx context.Context, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.cfg.URL.Reveal(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", scrubURL(err, t.cfg.URL))
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wsaw")

	for name, value := range t.cfg.Headers {
		req.Header.Set(name, value.Reveal())
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting: %w", scrubURL(err, t.cfg.URL))
	}

	defer func() { _ = resp.Body.Close() }()

	// Every 2xx is a success. A Workflow answers 202 Accepted rather than 200,
	// and treating that as a failure would retry messages that were in fact
	// delivered — turning one alert into four (AC3).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode}
	}

	return nil
}

// message wraps the card in the envelope a Workflow expects.
func (t *Teams) message(ev ScanEvent, listed int) map[string]any {
	return map[string]any{
		fieldType: "message",
		"attachments": []any{
			map[string]any{
				"contentType": adaptiveCardType,
				"contentUrl":  nil,
				"content":     t.card(ev, listed),
			},
		},
	}
}

func (t *Teams) card(ev ScanEvent, listed int) map[string]any {
	card := map[string]any{
		fieldType: "AdaptiveCard",
		"$schema": adaptiveCardSchema,
		"version": adaptiveCardVersion,
		"body":    t.cardBody(ev, listed),
	}

	if action := t.resultAction(ev); action != nil {
		card["actions"] = []any{action}
	}

	return card
}

// cardBody leads with what a reviewer needs first — which site, which consent
// mode, how bad, and whether consent actually applied — and only then lists
// the changes (AC5).
func (t *Teams) cardBody(ev ScanEvent, listed int) []any {
	body := []any{
		textBlock(ev.Target+" — "+string(ev.ConsentMode)+" mode", "Bolder", "Large", ""),
		textBlock(headline(ev), "Bolder", colourDefault, severityColour(ev.Highest)),
	}

	if !ev.Trustworthy {
		// Placed above the findings, because "few changes" means nothing when
		// the scan itself did not complete (AC7).
		body = append(body, textBlock("⚠ "+ev.Caveat, "Bolder", colourDefault, "Attention"))
	}

	body = append(body, factSet(t.facts(ev)))

	body = append(body, t.changeBlocks(ev, listed)...)

	note := "This is a summary of one scan, not the finding itself."
	if t.cfg.BaseURL == "" {
		note += " The full result is in wsaw; no web interface URL is configured for this notifier, so there is no link."
	} else {
		note += " Open the result in wsaw for the full asset list."
	}

	body = append(body, wrapBlock(note, "Small", colourDefault))

	return body
}

func (t *Teams) changeBlocks(ev ScanEvent, listed int) []any {
	var out []any

	switch {
	case !ev.Comparable:
		reason := ev.Reason
		if reason == "" {
			reason = "this scan could not be compared with an earlier one"
		}

		return append(out, wrapBlock("No comparison: "+reason, colourDefault, colourDefault))

	case len(ev.Changes) == 0:
		return append(out, wrapBlock("No changes against the baseline.", colourDefault, colourDefault))
	}

	shown := min(listed, len(ev.Changes))

	out = append(out, textBlock(changeHeading(ev), "Bolder", colourDefault, ""))

	for _, c := range ev.Changes[:shown] {
		out = append(out, wrapBlock(changeLine(c), colourDefault, severityColour(c.Severity)))
	}

	// The omitted count is derived from the true total, so the number in the
	// card is the number of changes there were, not the number it could fit
	// (AC4).
	if omitted := len(ev.Changes) - shown; omitted > 0 {
		out = append(out, wrapBlock(
			fmt.Sprintf("… and %d more change%s not listed here, of %d in total.",
				omitted, plural(omitted), len(ev.Changes)),
			colourDefault, "Accent"))
	}

	if ev.Suppressed > 0 {
		out = append(out, wrapBlock(
			fmt.Sprintf("%d change%s suppressed by this target's allow list.",
				ev.Suppressed, plural(ev.Suppressed)),
			"Small", colourDefault))
	}

	return out
}

func changeHeading(ev ScanEvent) string {
	return fmt.Sprintf("%d change%s", len(ev.Changes), plural(len(ev.Changes)))
}

// headline states the severity as text as well as colour, so a colour-blind
// reader, a monochrome digest, and a plain-text notification preview all carry
// the same information (AC5).
func headline(ev ScanEvent) string {
	switch {
	case !ev.Trustworthy:
		// The termination is named only when it is what went wrong. A scan
		// that reached network idle but failed its consent interaction is not
		// "not trustworthy — idle"; that reads as though idling were the
		// problem. The caveat line below carries the actual reason.
		switch ev.Termination {
		case model.TermError, model.TermSkipped, model.TermTimeout,
			model.TermRequestCap, model.TermByteCap:
			return "Result not trustworthy — " + string(ev.Termination)
		default:
			return "Result not trustworthy"
		}

	case len(ev.Changes) == 0:
		return "No changes"

	default:
		return fmt.Sprintf("Highest severity: %s — %d change%s",
			strings.ToUpper(string(ev.Highest)), len(ev.Changes), plural(len(ev.Changes)))
	}
}

func (t *Teams) facts(ev ScanEvent) [][2]string {
	consent := string(ev.ConsentOutcome)
	if ev.CMP != "" {
		consent += " (" + ev.CMP + ")"
	}

	if ev.ConsentReason != "" {
		consent += ": " + ev.ConsentReason
	}

	facts := [][2]string{
		{"URL", ev.URL},
		{"Consent mode", string(ev.ConsentMode)},
		{"Consent outcome", consent},
		{"Termination", string(ev.Termination)},
		{"Requests", strconv.Itoa(ev.Requests)},
		{"Third-party domains", strconv.Itoa(ev.ThirdPartyDomains)},
		{"Contacted before consent", strconv.Itoa(ev.PreConsentDomains)},
		{"Scan", ev.ScanID},
		{"Started", ev.At.UTC().Format(time.RFC3339)},
	}

	if ev.Duration > 0 {
		facts = append(facts, [2]string{"Duration", ev.Duration.Round(time.Millisecond).String()})
	}

	return facts
}

// resultAction links back to the result in the web interface. Without a
// configured base URL there is no link — an invented one would be worse than
// none (AC6).
func (t *Teams) resultAction(ev ScanEvent) map[string]any {
	if t.cfg.BaseURL == "" {
		return nil
	}

	target := url.PathEscape(ev.Target)
	mode := url.PathEscape(string(ev.ConsentMode))
	scan := url.PathEscape(ev.ScanID)

	return map[string]any{
		fieldType: "Action.OpenUrl",
		"title":   "Open in wsaw",
		"url":     t.cfg.BaseURL + "/results/" + target + "/" + mode + "/" + scan,
	}
}

func changeLine(c diff.Change) string {
	var b strings.Builder

	b.WriteString("[")
	b.WriteString(strings.ToUpper(string(c.Severity)))
	b.WriteString("] ")
	b.WriteString(string(c.Type))
	b.WriteString(": ")
	b.WriteString(c.Subject)

	if c.Phase == model.PhasePre {
		b.WriteString(" — before any consent decision")
	}

	if c.Detail != "" {
		b.WriteString(" — ")
		b.WriteString(c.Detail)
	}

	return b.String()
}

// severityColour maps a severity onto the Adaptive Card palette. Colour is
// always redundant with text here; it is never the only signal.
func severityColour(s diff.Severity) string {
	switch s {
	case diff.SeverityCritical, diff.SeverityHigh:
		return "Attention"
	case diff.SeverityMedium:
		return "Warning"
	case diff.SeverityLow, diff.SeverityInfo:
		return colourDefault
	default:
		return colourDefault
	}
}

func textBlock(text, weight, size, colour string) map[string]any {
	block := map[string]any{
		fieldType: "TextBlock",
		"text":    text,
		"wrap":    true,
	}

	if weight != "" {
		block["weight"] = weight
	}

	if size != "" && size != colourDefault {
		block["size"] = size
	}

	if colour != "" && colour != colourDefault {
		block["color"] = colour
	}

	return block
}

func wrapBlock(text, size, colour string) map[string]any {
	return textBlock(text, "", size, colour)
}

func factSet(facts [][2]string) map[string]any {
	items := make([]any, 0, len(facts))

	for _, f := range facts {
		if f[1] == "" {
			continue
		}

		items = append(items, map[string]any{"title": f[0], "value": f[1]})
	}

	return map[string]any{fieldType: "FactSet", "facts": items}
}

// messageCard builds the retired connector format, for a tenant that still has
// a working connector webhook (AC9). It is deliberately plainer: this path
// exists to keep an existing integration alive, not to be developed.
func (t *Teams) messageCard(ev ScanEvent, listed int) map[string]any {
	shown := min(listed, len(ev.Changes))

	var text strings.Builder

	if !ev.Trustworthy {
		text.WriteString("**⚠ " + ev.Caveat + "**\n\n")
	}

	for _, c := range ev.Changes[:shown] {
		text.WriteString("- " + changeLine(c) + "\n")
	}

	if omitted := len(ev.Changes) - shown; omitted > 0 {
		fmt.Fprintf(&text, "\n… and %d more change%s not listed here, of %d in total.\n",
			omitted, plural(omitted), len(ev.Changes))
	}

	text.WriteString("\nThis is a summary of one scan, not the finding itself.")

	facts := make([]any, 0, 8)
	for _, f := range t.facts(ev) {
		if f[1] == "" {
			continue
		}

		facts = append(facts, map[string]any{"name": f[0], "value": f[1]})
	}

	section := map[string]any{
		"activityTitle":    ev.Target + " — " + string(ev.ConsentMode) + " mode",
		"activitySubtitle": headline(ev),
		"facts":            facts,
		"text":             text.String(),
		"markdown":         true,
	}

	card := map[string]any{
		"@type":      "MessageCard",
		"@context":   "https://schema.org/extensions",
		"summary":    "wsaw: " + ev.Target + " — " + headline(ev),
		"themeColor": messageCardColour(ev),
		"sections":   []any{section},
	}

	if t.cfg.BaseURL != "" {
		card["potentialAction"] = []any{
			map[string]any{
				"@type": "OpenUri",
				"name":  "Open in wsaw",
				"targets": []any{
					map[string]any{"os": "default", "uri": t.resultAction(ev)["url"]},
				},
			},
		}
	}

	return card
}

func messageCardColour(ev ScanEvent) string {
	if !ev.Trustworthy {
		return "b45309"
	}

	switch ev.Highest {
	case diff.SeverityCritical, diff.SeverityHigh:
		return "b91c1c"
	case diff.SeverityMedium:
		return "b45309"
	default:
		return "1d4ed8"
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "s"
}
