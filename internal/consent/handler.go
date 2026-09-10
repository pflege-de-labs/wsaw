package consent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Mechanism names how a consent state was reached. It is recorded in every
// result so a reviewer can weigh the evidence: a TCF API call is strong, a
// label guess is weak.
const (
	MechanismTCF = "tcf-api"
	// DetectionTCF and CMPTCF name a CMP found through the TCF API, where
	// wsaw knows the interface but not the vendor.
	DetectionTCF       = "tcf-api"
	CMPTCF             = "TCF v2.2 CMP"
	MechanismVendorAPI = "vendor-api"
	MechanismSelector  = "selector"
	MechanismHeuristic = "heuristic"
)

// mechanismEscalatedSuffix is appended to a mechanism when a fallback click
// was needed to close a banner that the primary mechanism left displayed —
// "vendor-api+click" and "selector+click" report a different story than
// "vendor-api" and "selector" alone (Story 2.7, AC4).
const mechanismEscalatedSuffix = "+click"

// defaultDismissedExpr decides whether a banner is gone when a rule does not
// define its own Dismissed expression. It reuses the same container check the
// heuristic fallback already relies on, rather than inventing a second
// definition of "banner-shaped" (Story 2.7, AC1).
const defaultDismissedExpr = "!window.__wsawConsentContainer()"

// FailurePolicy decides what a failed interaction does to the scan.
type FailurePolicy string

// Failure policies.
const (
	// FailFlag records the failure and keeps the result. This is the default:
	// the traffic observed under a failed interaction is exactly what a
	// reviewer needs to see.
	FailFlag FailurePolicy = "flag"
	// FailScan turns a failed interaction into a scan error, for operators
	// who would rather have no result than an unreliable one.
	FailScan FailurePolicy = "fail"
)

// Options configures the handler.
type Options struct {
	// Mode is the consent state to reach.
	Mode model.ConsentMode

	// Rules is the compiled rule set to try.
	Rules *RuleSet

	// Host and Domain restrict which rules are candidates.
	Host   string
	Domain string

	// StepTimeout bounds one rule action.
	StepTimeout time.Duration
	// TotalTimeout bounds the whole interaction, so a CMP that never settles
	// cannot consume the scan's entire budget.
	TotalTimeout time.Duration

	// AllowHeuristic permits the label-guessing fallback. On by default via
	// New; turning it off means unknown banners are reported rather than
	// guessed at.
	AllowHeuristic bool

	// OnFailure decides whether a failed interaction fails the scan.
	OnFailure FailurePolicy

	// Screenshot captures evidence, if enabled by the caller.
	Screenshot func(kind string) error

	// OnInteract is called the moment the consent action has been performed,
	// before verification. Verification can take seconds, and the requests
	// the action triggers arrive during that window; reporting the boundary
	// late would attribute them to the pre-consent phase.
	OnInteract func()

	Logger *slog.Logger
}

// Defaults for interaction budgets.
const (
	DefaultStepTimeout  = 10 * time.Second
	DefaultTotalTimeout = 30 * time.Second
)

func (o *Options) withDefaults() Options {
	out := *o

	if out.StepTimeout <= 0 {
		out.StepTimeout = DefaultStepTimeout
	}

	if out.TotalTimeout <= 0 {
		out.TotalTimeout = DefaultTotalTimeout
	}

	if out.OnFailure == "" {
		out.OnFailure = FailFlag
	}

	if out.Logger == nil {
		out.Logger = slog.Default()
	}

	return out
}

// ErrConsentFailed reports a failed interaction when the policy is FailScan.
var ErrConsentFailed = errors.New("consent interaction failed")

// Apply brings the page into the requested consent state and returns what
// happened. It never returns a zero Consent: an unrecognized page yields an
// explicit "not-needed" with a reason, because silence would be indistinguishable
// from success (Tenet 5).
func Apply(ctx context.Context, rawOpts Options) (model.Consent, error) {
	opts := rawOpts.withDefaults()

	if opts.Mode == model.ConsentNone {
		return model.Consent{
			Outcome: model.OutcomeNotNeeded,
			Reason:  "consent mode is none; the page was not interacted with",
		}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.TotalTimeout)
	defer cancel()

	h := &handler{opts: opts}

	consent, err := h.apply(runCtx)

	// A timeout is a recorded outcome, not a lost scan.
	if err == nil && runCtx.Err() != nil && consent.Outcome == model.OutcomeApplied {
		consent.Outcome = model.OutcomeUnverified
		consent.Reason = appendReason(consent.Reason, "interaction budget expired before verification completed")
	}

	if opts.OnFailure == FailScan && consent.Outcome == model.OutcomeFailed {
		return consent, fmt.Errorf("%w: %s", ErrConsentFailed, consent.Reason)
	}

	return consent, err
}

type handler struct {
	opts Options
}

func (h *handler) apply(ctx context.Context) (model.Consent, error) {
	if err := h.injectHelpers(ctx); err != nil {
		return model.Consent{
			Outcome: model.OutcomeFailed,
			Reason:  "could not prepare the page for consent handling: " + err.Error(),
		}, nil
	}

	probe := h.probe(ctx)

	// The TCF API is tried first: it is documented, version-stable, and
	// reports back what the CMP recorded.
	if probe.TCF {
		consent, done := h.applyTCF(ctx, probe)
		if done {
			return consent, nil
		}

		// TCF was present but could not be driven; fall through to rules and
		// keep what was learned.
		defer func() {}()
	}

	consent := h.applyRules(ctx, probe)

	// Whatever mechanism ran, record the GPP string if the page has one.
	if probe.GPP {
		if gpp := h.readGPP(ctx); gpp != "" {
			consent.GPPString = gpp
		}
	}

	return consent, nil
}

func (h *handler) injectHelpers(ctx context.Context) error {
	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	if err := chromedp.Run(stepCtx, chromedp.Evaluate(helperScript, nil)); err != nil {
		return fmt.Errorf("injecting consent helpers: %w", err)
	}

	return nil
}

type probeResult struct {
	TCF        bool `json:"tcf"`
	GPP        bool `json:"gpp"`
	USP        bool `json:"usp"`
	TCFLocator bool `json:"tcfLocator"`
}

func (h *handler) probe(ctx context.Context) probeResult {
	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var out probeResult

	if err := chromedp.Run(stepCtx, chromedp.Evaluate(tcfProbeScript, &out)); err != nil {
		h.opts.Logger.Debug("consent API probe failed", "error", err)
	}

	return out
}

type tcfResult struct {
	OK          bool   `json:"ok"`
	Reason      string `json:"reason"`
	TCString    string `json:"tcString"`
	EventStatus string `json:"eventStatus"`
	CMPID       any    `json:"cmpId"`
	CMPVersion  any    `json:"cmpVersion"`
}

// applyTCF drives the page through the TCF API. The bool reports whether the
// outcome is conclusive; false means the caller should try selector rules.
func (h *handler) applyTCF(ctx context.Context, probe probeResult) (model.Consent, bool) {
	grant := "false"
	if h.opts.Mode == model.ConsentAccept {
		grant = "true"
	}

	stepCtx, cancel := context.WithTimeout(ctx, h.opts.TotalTimeout)
	defer cancel()

	var out tcfResult

	script := fmt.Sprintf(tcfApplyScript, grant)

	// Marked before the call, for the same reason as the rule path: the CMP
	// may fire requests the moment consent is recorded, well before the API
	// reports useractioncomplete.
	h.interacted()

	if err := chromedp.Run(stepCtx, chromedp.Evaluate(script, &out, awaitPromise)); err != nil {
		return model.Consent{
			Outcome:   model.OutcomeFailed,
			Reason:    "TCF API call failed: " + err.Error(),
			CMP:       CMPTCF,
			Detection: DetectionTCF,
		}, false
	}

	consent := model.Consent{
		CMP:       CMPTCF,
		Detection: DetectionTCF,
		Mechanism: MechanismTCF,
		TCString:  out.TCString,
	}

	if id := numString(out.CMPID); id != "" {
		consent.CMP = "TCF CMP " + id
	}

	consent.CMPVersion = numString(out.CMPVersion)

	if !out.OK {
		consent.Reason = "TCF API did not complete: " + out.Reason

		return consent, false
	}

	now := time.Now()
	consent.InteractedAt = &now
	consent.Outcome = model.OutcomeApplied
	consent.Reason = "CMP reported " + out.EventStatus

	if out.TCString == "" {
		// Without a consent string there is no evidence the choice was
		// recorded, so this is not "applied".
		consent.Outcome = model.OutcomeUnverified
		consent.Reason = appendReason(consent.Reason, "CMP returned no TC string")
	}

	return consent, true
}

func (h *handler) readGPP(ctx context.Context) string {
	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var out struct {
		OK        bool   `json:"ok"`
		GPPString string `json:"gppString"`
	}

	if err := chromedp.Run(stepCtx, chromedp.Evaluate(gppReadScript, &out, awaitPromise)); err != nil {
		return ""
	}

	return out.GPPString
}

// applyRules tries each candidate rule whose detect expression matches.
func (h *handler) applyRules(ctx context.Context, probe probeResult) model.Consent {
	candidates := h.opts.Rules.Candidates(h.opts.Host, h.opts.Domain)

	var attempted []string

	for _, rule := range candidates {
		if rule.Heuristic && !h.opts.AllowHeuristic {
			continue
		}

		steps := rule.Steps(h.opts.Mode)
		if len(steps) == 0 {
			continue
		}

		matched, err := h.detect(ctx, rule)
		if err != nil {
			h.opts.Logger.Debug("consent rule detection failed", "rule", rule.Name, "error", err)

			continue
		}

		if !matched {
			continue
		}

		attempted = append(attempted, rule.Name)

		consent := h.runRule(ctx, rule, steps)
		if consent.Outcome == model.OutcomeApplied || consent.Outcome == model.OutcomeUnverified ||
			consent.Outcome == model.OutcomeBannerVisible {
			return consent
		}

		// A rule that detected but failed is worth reporting if nothing else
		// works, so keep going but remember it.
		if ctx.Err() != nil {
			return consent
		}
	}

	if probe.TCF {
		return model.Consent{
			Outcome:   model.OutcomeFailed,
			Reason:    "a TCF CMP is present but could not be driven, and no rule matched",
			CMP:       CMPTCF,
			Detection: DetectionTCF,
		}
	}

	if len(attempted) > 0 {
		return model.Consent{
			Outcome: model.OutcomeFailed,
			Reason:  "rules matched but none completed: " + strings.Join(attempted, ", "),
		}
	}

	// No CMP at all is a legitimate, common outcome and must not read as an
	// error: many pages simply have no banner.
	return model.Consent{
		Outcome: model.OutcomeNotNeeded,
		Reason:  "no consent management platform detected",
	}
}

func (h *handler) detect(ctx context.Context, rule Rule) (bool, error) {
	if rule.Detect == "" {
		// A rule without a detect expression applies only when it is
		// host-scoped; otherwise it would match every page.
		return len(rule.Hosts) > 0, nil
	}

	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var matched bool

	if err := chromedp.Run(stepCtx, chromedp.Evaluate(rule.Detect, &matched)); err != nil {
		return false, err
	}

	return matched, nil
}

// interacted reports that the consent action has been performed.
func (h *handler) interacted() {
	if h.opts.OnInteract != nil {
		h.opts.OnInteract()
	}
}

func (h *handler) runRule(ctx context.Context, rule Rule, steps []Action) model.Consent {
	consent := model.Consent{
		CMP:       ruleCMPName(rule),
		Detection: "rule:" + rule.Name,
		Mechanism: mechanismFor(rule, steps),
		Heuristic: rule.Heuristic,
	}

	// The phase boundary is marked before the action, not after it.
	//
	// A click and the requests it triggers are both asynchronous: by the time
	// the click step returns, the injected tracker request has often already
	// been recorded. Marking afterwards therefore labels
	// interaction-triggered traffic as pre-consent, which is the one number
	// this product must not exaggerate.
	//
	// Marking beforehand is safe because capture has already waited for
	// network idle: nothing is in flight at this instant, so no genuinely
	// pre-consent request can be swept into the post-consent phase.
	h.interacted()

	for i, step := range steps {
		if err := h.runStep(ctx, step); err != nil {
			if step.Optional {
				continue
			}

			consent.Outcome = model.OutcomeFailed
			consent.Reason = fmt.Sprintf("rule %q step %d failed: %v", rule.Name, i+1, err)

			return consent
		}
	}

	now := time.Now()
	consent.InteractedAt = &now

	// Verification decides between "applied" and "unverified". A rule with no
	// verify expression can never claim to be verified, which is the honest
	// default (Story 2.5).
	if rule.Verify == "" {
		consent.Outcome = model.OutcomeUnverified
		consent.Reason = fmt.Sprintf("rule %q ran but defines no verification", rule.Name)

		return consent
	}

	ok, err := h.verify(ctx, rule)

	switch {
	case err != nil:
		consent.Outcome = model.OutcomeUnverified
		consent.Reason = fmt.Sprintf("rule %q ran but verification errored: %v", rule.Name, err)

		return consent

	case !ok:
		consent.Outcome = model.OutcomeFailed
		consent.Reason = fmt.Sprintf("rule %q ran but verification reported the banner is still present", rule.Name)

		return consent
	}

	// The CMP recorded the choice. Whether the banner itself is gone is a
	// separate question (Story 2.7, AC1): a vendor API can record a
	// rejection without ever running the banner's own dismiss handler, which
	// reacts to its buttons, not to the CMP's internal state.
	consent.Outcome = model.OutcomeApplied
	consent.Reason = fmt.Sprintf("rule %q applied and verified", rule.Name)

	if gone, gerr := h.bannerGone(ctx, rule); gerr == nil && gone {
		return consent
	}

	return h.escalate(ctx, rule, steps, consent)
}

// bannerGone reports whether the banner is still displayed, using the rule's
// own Dismissed expression where it defines one and the shared container
// heuristic otherwise (Story 2.7, AC1).
//
// The check is retried briefly, mirroring verify(): a CMP that recorded the
// choice often still fades its dialog out over the following frames, and a
// single check taken the instant the choice is recorded can catch that
// animation mid-flight. Without the retry, the outcome is decided from a
// frame that is already stale by the time the after-consent screenshot is
// taken a moment later, so the report claims the banner is visible in an
// image that does not show it.
func (h *handler) bannerGone(ctx context.Context, rule Rule) (bool, error) {
	expr := rule.Dismissed
	if expr == "" {
		expr = defaultDismissedExpr
	}

	const (
		attempts = 10
		interval = 250 * time.Millisecond
	)

	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var (
		gone    bool
		lastErr error
	)

	for range attempts {
		err := chromedp.Run(stepCtx, chromedp.Evaluate(expr, &gone))
		if err == nil && gone {
			return true, nil
		}

		if err != nil {
			lastErr = err
		}

		timer := time.NewTimer(interval)

		select {
		case <-timer.C:
		case <-stepCtx.Done():
			timer.Stop()

			return false, lastErr
		}

		timer.Stop()
	}

	if lastErr != nil {
		return false, lastErr
	}

	return gone, nil
}

// escalate is reached when a rule's own verification says the CMP recorded
// the requested choice, but the banner itself is still on screen — the gap
// Story 2.7 exists to close. Tenet 11 ranks a vendor API above clicking for
// expressing the choice; it never licenses treating the API's silence about
// the banner as evidence the banner is gone.
//
// This is one bounded pass, not a retry loop (Tenet 6): every click step the
// rule already defines for this mode is retried once, this time with the
// fuller pointer/mouse event sequence __wsawSimulateClick provides, and if
// none of them close the banner the generic heuristic label match is tried
// for the same mode (Story 2.4, AC5). A CMP whose own mechanism already
// closes its dialog never reaches this method, because the caller only calls
// it once bannerGone has already reported the banner is still displayed.
func (h *handler) escalate(ctx context.Context, rule Rule, steps []Action, consent model.Consent) model.Consent {
	clicked := h.escalateClicks(ctx, steps)

	if !clicked && h.opts.AllowHeuristic {
		clicked = h.escalateHeuristic(ctx, rule)
	}

	if clicked {
		consent.Mechanism += mechanismEscalatedSuffix
	}

	gone, err := h.bannerGone(ctx, rule)

	switch {
	case err != nil:
		consent.Outcome = model.OutcomeUnverified
		consent.Reason = fmt.Sprintf(
			"rule %q recorded the choice, but whether the banner closed could not be checked: %v", rule.Name, err)

	case gone:
		consent.Reason = fmt.Sprintf(
			"rule %q applied and verified; the banner needed a fallback click to close", rule.Name)

	default:
		consent.Outcome = model.OutcomeBannerVisible
		consent.Reason = fmt.Sprintf(
			"rule %q recorded the choice, but the banner was still displayed after a fallback click was attempted",
			rule.Name)
	}

	return consent
}

// escalateClicks retries every click step the rule defines for this mode
// through __wsawSimulateClick, and reports whether any of them found and
// clicked an element.
func (h *handler) escalateClicks(ctx context.Context, steps []Action) bool {
	var clicked bool

	for _, step := range steps {
		if step.Click == "" {
			continue
		}

		if h.simulateClick(ctx, step.Click) {
			clicked = true
		}
	}

	return clicked
}

// escalateHeuristic tries the generic label-matching fallback for the current
// mode, reusing the shipped heuristic rule rather than a second copy of its
// label lists (Story 2.4, AC5).
func (h *handler) escalateHeuristic(ctx context.Context, originating Rule) bool {
	for _, r := range h.opts.Rules.Rules() {
		if !r.Heuristic || r.Name == originating.Name {
			continue
		}

		steps := r.Steps(h.opts.Mode)
		if len(steps) == 0 {
			continue
		}

		var acted bool

		for _, step := range steps {
			if step.Click == "" && step.Eval == "" {
				continue
			}

			if err := h.runStep(ctx, step); err == nil {
				acted = true
			}
		}

		if acted {
			return true
		}
	}

	return false
}

func (h *handler) simulateClick(ctx context.Context, selector string) bool {
	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var out clickResult

	expr := fmt.Sprintf("window.__wsawSimulateClick(%s)", jsString(selector))

	if err := chromedp.Run(stepCtx, chromedp.Evaluate(expr, &out)); err != nil {
		return false
	}

	return out.Clicked
}

type clickResult struct {
	Clicked bool   `json:"clicked"`
	Reason  string `json:"reason"`
	Matched string `json:"matched"`
}

func (h *handler) runStep(ctx context.Context, step Action) error {
	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	switch {
	case step.WaitFor != "":
		return h.waitFor(stepCtx, step.WaitFor)

	case step.WaitMillis > 0:
		timer := time.NewTimer(time.Duration(step.WaitMillis) * time.Millisecond)
		defer timer.Stop()

		select {
		case <-timer.C:
			return nil
		case <-stepCtx.Done():
			return stepCtx.Err()
		}

	case step.Click != "":
		var out clickResult

		expr := fmt.Sprintf("window.__wsawClick(%s)", jsString(step.Click))

		if err := chromedp.Run(stepCtx, chromedp.Evaluate(expr, &out)); err != nil {
			return fmt.Errorf("clicking %s: %w", step.Click, err)
		}

		if !out.Clicked {
			return fmt.Errorf("clicking %s: %s", step.Click, out.Reason)
		}

		return nil

	case step.Eval != "":
		// A rule's eval may return a boolean to signal whether it did
		// anything, or nothing at all. Both are accepted; only an explicit
		// false is a failure, so that vendor scripts can report "not
		// applicable" and let the next step try.
		var raw json.RawMessage

		if err := chromedp.Run(stepCtx, chromedp.Evaluate(step.Eval, &raw, awaitPromise)); err != nil {
			return fmt.Errorf("evaluating rule step: %w", err)
		}

		if isFalsyResult(raw) {
			return errors.New("rule step reported it could not act")
		}

		return nil

	default:
		return errors.New("rule step is empty")
	}
}

func (h *handler) waitFor(ctx context.Context, selector string) error {
	const poll = 200 * time.Millisecond

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	expr := fmt.Sprintf("!!window.__wsawQuery(%s)", jsString(selector))

	for {
		var found bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &found)); err == nil && found {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", selector, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (h *handler) verify(ctx context.Context, rule Rule) (bool, error) {
	// Verification is retried briefly: CMPs animate their dialogs away, and
	// checking one frame too early would report a false failure.
	const (
		attempts = 10
		interval = 250 * time.Millisecond
	)

	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var lastErr error

	for range attempts {
		var ok bool

		err := chromedp.Run(stepCtx, chromedp.Evaluate(rule.Verify, &ok))
		if err == nil && ok {
			return true, nil
		}

		if err != nil {
			lastErr = err
		}

		timer := time.NewTimer(interval)

		select {
		case <-timer.C:
		case <-stepCtx.Done():
			timer.Stop()

			return false, lastErr
		}

		timer.Stop()
	}

	return false, lastErr
}

func mechanismFor(rule Rule, steps []Action) string {
	if rule.Heuristic {
		return MechanismHeuristic
	}

	for _, s := range steps {
		if s.Eval != "" {
			return MechanismVendorAPI
		}
	}

	return MechanismSelector
}

func ruleCMPName(rule Rule) string {
	if rule.Vendor != "" {
		return rule.Vendor
	}

	if rule.Heuristic {
		return "unknown (heuristic match)"
	}

	return rule.Name
}

func appendReason(existing, add string) string {
	if existing == "" {
		return add
	}

	return existing + "; " + add
}

// numString renders a JSON number or string identifier without printing
// "%!s(float64=...)" noise into a result.
func numString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%d", int64(t))
	case int:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// isFalsyResult reports whether a rule step explicitly returned false.
func isFalsyResult(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))

	return s == "false"
}

func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// A selector that cannot be JSON-encoded cannot be sent to the page;
		// an empty string makes the step fail with a clear message.
		return `""`
	}

	return string(b)
}

func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}
