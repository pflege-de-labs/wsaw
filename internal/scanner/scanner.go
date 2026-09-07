// Package scanner performs one complete scan: acquire a browser, capture the
// page, drive consent, store the result, and diff it against a baseline.
//
// The blast radius of anything that goes wrong here is exactly one scan
// (Tenet 7). A panic is recovered, a broken browser is discarded rather than
// returned to the pool, and every failure is recorded as a result rather than
// discarded.
package scanner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/capture"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/robots"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Metrics receives the counters a scan produces. Every field is optional, so
// a caller that wants none of it passes the zero value.
type Metrics struct {
	ScanStarted    func(target string, mode model.ConsentMode)
	ScanFinished   func(target string, mode model.ConsentMode, res *model.Result)
	ScanFailed     func(target string, mode model.ConsentMode, reason string)
	BrowserRestart func(target, reason string)
}

// Deps are the collaborators a Scanner needs. They are interfaces at the
// seams that plausibly get a second implementation (Tenet 12).
type Deps struct {
	Pool    *browser.Pool
	Store   *store.Store
	Robots  *robots.Checker
	Rules   *consent.RuleSet
	Secrets *secret.Registry
	Logger  *slog.Logger
	Metrics Metrics
}

// Options configures a Scanner.
type Options struct {
	Normalizer *normalize.Normalizer

	// Baseline selects what a result is compared against.
	Baseline config.BaselineMode

	// HashResourceTypes selects which bodies are fingerprinted.
	HashResourceTypes []string

	// ConsentStepTimeout and ConsentTotalTimeout bound banner interaction.
	ConsentStepTimeout  time.Duration
	ConsentTotalTimeout time.Duration
	// AllowHeuristicConsent permits label guessing as a last resort.
	AllowHeuristicConsent bool
	// ConsentOnFailure decides whether a failed interaction fails the scan.
	ConsentOnFailure consent.FailurePolicy

	// WsawVersion and ChromeVersion are recorded in every result.
	WsawVersion   string
	ChromeVersion string

	// BrowserRuntime is "local", or the container runtime rendering pages.
	BrowserRuntime string
	// BrowserImage is the container image, empty when local.
	BrowserImage string
	// BrowserSandbox reports whether Chrome's own sandbox is active.
	BrowserSandbox bool

	// RobotsFallbackAllow is used when robots.txt cannot be fetched.
	RobotsFallbackAllow bool
}

// Scanner runs scans. It is safe for concurrent use.
type Scanner struct {
	deps Deps
	opts Options

	// live is the in-flight registry. It is owned rather than injected: every
	// scan goes through this type, so there is no second place that could
	// know what is running (Story 5.12).
	live *Live
}

// New creates a Scanner.
func New(deps Deps, opts Options) (*Scanner, error) {
	if deps.Pool == nil {
		return nil, errors.New("scanner: browser pool is required")
	}

	if opts.Normalizer == nil {
		return nil, errors.New("scanner: normalizer is required")
	}

	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	return &Scanner{deps: deps, opts: opts, live: NewLive()}, nil
}

// Outcome is everything one scan produced.
type Outcome struct {
	Result *model.Result
	Diff   *diff.Report
}

// Scan performs one scan of one target in one consent mode.
//
// It returns a Result even when it fails, because a failed scan is an
// observation that must be recorded and alerted on, not an absence of data
// (Tenet 5). The error is returned alongside for logging.
func (s *Scanner) Scan(ctx context.Context, target config.Resolved, mode model.ConsentMode) (out Outcome, err error) {
	scanID := newScanID()

	attempt := attemptOf(ctx)

	log := s.deps.Logger.With(
		"scan_id", scanID,
		"target", target.Name,
		"consent_mode", string(mode),
	)

	if attempt.attempt > 1 {
		log = log.With("attempt", attempt.attempt, "attempts", attempt.attempts)
	}

	if s.deps.Metrics.ScanStarted != nil {
		s.deps.Metrics.ScanStarted(target.Name, mode)
	}

	// Emitted before anything can go wrong, so every scan has an opening line
	// even when it is skipped, panics, or never reaches the browser. It pairs
	// with "scan finished" through the shared scan ID, which is what lets an
	// operator match a burst of activity — or a scan that started and never
	// ended — in an aggregator (Story 5.13).
	log.Info("scan started", "url", target.URL, "source", sourceOf(ctx))

	// Registered here, next to the opening log line and before anything can
	// fail, so that a scan is visible as running for exactly as long as it
	// actually is — including while it is being skipped or while it panics.
	// The releaser is deferred first and therefore runs last, after the panic
	// recovery below has recorded its result.
	defer s.live.begin(Running{
		ScanID:      scanID,
		Target:      target.Name,
		URL:         target.URL,
		ConsentMode: mode,
		StartedAt:   time.Now(),
		Source:      sourceOf(ctx),
	})()

	// A panic in one scan must not take down the daemon. It is converted into
	// a recorded failure so the target still shows a result.
	defer func() {
		if r := recover(); r != nil {
			log.Error("scan panicked", "panic", fmt.Sprint(r))

			out.Result = s.failedResult(scanID, target, mode, fmt.Sprintf("internal error: %v", r))
			err = fmt.Errorf("scan panicked: %v", r)

			s.persist(ctx, log, out.Result)
			s.recordFailure(target.Name, mode, "panic")
		}
	}()

	// A containerised browser has its own network namespace, so the host's
	// loopback is not the target's loopback. Refusing with an explanation is
	// the only honest option: silently scanning the container's own localhost
	// would produce a result about nothing, and rewriting the URL would make
	// the result describe an address the operator did not ask for
	// (Story 1.8, AC10).
	if reason, ok := s.unreachableFromContainer(target); !ok {
		log.Warn("scan skipped: target is not reachable from a containerised browser", "reason", reason)

		out.Result = s.failedResult(scanID, target, mode, reason)
		s.persist(ctx, log, out.Result)

		return out, nil
	}

	if decision := s.checkRobots(ctx, target); !decision.Allowed {
		log.Info("scan skipped by robots policy", "reason", decision.Reason)

		out.Result = s.skippedResult(scanID, target, mode, decision.Reason)
		s.persist(ctx, log, out.Result)

		return out, nil
	}

	res, err := s.capture(ctx, scanID, target, mode, log)
	if res == nil {
		// Only a configuration-level failure gets here; still record it.
		res = s.failedResult(scanID, target, mode, errorText(err))
	}

	res.ScanID = scanID
	out.Result = res

	s.persist(ctx, log, res)

	out.Diff = s.compare(target, res, log)

	if s.deps.Metrics.ScanFinished != nil {
		s.deps.Metrics.ScanFinished(target.Name, mode, res)
	}

	if !res.OK() {
		s.recordFailure(target.Name, mode, string(res.Termination))
	}

	log.Info("scan finished",
		"termination", string(res.Termination),
		"requests", len(res.Requests),
		"third_party_domains", len(res.ThirdPartyDomains("")),
		"consent_outcome", string(res.Consent.Outcome),
		"duration", res.Duration.String(),
	)

	return out, err
}

func (s *Scanner) capture(
	ctx context.Context,
	scanID string,
	target config.Resolved,
	mode model.ConsentMode,
	log *slog.Logger,
) (*model.Result, error) {
	lease, err := s.deps.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring a browser: %w", err)
	}

	// The lease is released on every path. Whether it is returned or
	// discarded depends on what went wrong: a browser-level failure must not
	// be handed to the next scan.
	discarded := false

	defer func() {
		if !discarded {
			lease.Release()
		}
	}()

	// Read before the scan context is created, since that increments the
	// counter. A browser that has already served a scan means isolation rests
	// on clearing state rather than on the process boundary.
	reused := lease.Browser.Scans() > 0

	scanCtx, cancelScan, err := lease.Browser.NewScanContext(ctx)
	if err != nil {
		discarded = true

		lease.Discard("could not create a scan context")
		s.browserRestarted(target.Name, "could not create a scan context")

		return nil, fmt.Errorf("creating an isolated scan context: %w", err)
	}

	defer cancelScan()

	opts := s.captureOptions(target, mode)
	opts.BrowserReused = reused
	opts.WsawVersion = s.opts.WsawVersion
	opts.ChromeVersion = s.opts.ChromeVersion
	opts.BrowserRuntime = s.opts.BrowserRuntime
	opts.BrowserImage = s.opts.BrowserImage
	opts.BrowserSandbox = s.opts.BrowserSandbox
	opts.Secrets = s.deps.Secrets

	if target.Screenshots || target.StoreBodies {
		opts.BodySink = s.artifactSink
		opts.ScreenshotSink = s.artifactSink
	}

	hooks := capture.Hooks{AfterLoad: s.consentHook(target, mode, log)}

	res, captureErr := capture.Run(ctx, scanCtx, opts, hooks)
	if res == nil {
		discarded = true

		lease.Discard("capture could not start")
		s.browserRestarted(target.Name, "capture could not start")

		return nil, captureErr
	}

	res.ScanID = scanID

	// A browser-level failure means this browser is suspect. Returning it to
	// the pool would spread one bad scan across the next several.
	if isBrowserFailure(captureErr) {
		discarded = true

		reason := "capture failed at the browser level: " + errorText(captureErr)
		lease.Discard(reason)
		s.browserRestarted(target.Name, reason)
	}

	return res, captureErr
}

// consentHook returns the capture hook that drives the banner. Consent stays
// out of the capture package: capture records, consent acts.
func (s *Scanner) consentHook(target config.Resolved, mode model.ConsentMode, log *slog.Logger) func(capture.Context) (model.Consent, error) {
	if mode == model.ConsentNone {
		return func(capture.Context) (model.Consent, error) {
			return model.Consent{
				Outcome: model.OutcomeNotNeeded,
				Reason:  "consent mode is none; the page was not interacted with",
			}, nil
		}
	}

	host, domain := hostAndDomain(target.URL)

	return func(cctx capture.Context) (model.Consent, error) {
		c, err := consent.Apply(cctx, consent.Options{
			Mode:           mode,
			Rules:          s.deps.Rules,
			Host:           host,
			Domain:         domain,
			StepTimeout:    s.opts.ConsentStepTimeout,
			TotalTimeout:   s.opts.ConsentTotalTimeout,
			AllowHeuristic: s.opts.AllowHeuristicConsent,
			OnFailure:      s.opts.ConsentOnFailure,
			Screenshot:     cctx.Screenshot,
			OnInteract:     cctx.MarkInteracted,
			Logger:         log,
		})
		if err != nil {
			log.Warn("consent interaction failed", "error", err, "outcome", string(c.Outcome))
		}

		return c, err
	}
}

func (s *Scanner) captureOptions(target config.Resolved, mode model.ConsentMode) capture.Options {
	return capture.Options{
		URL:               target.URL,
		Target:            target.Name,
		Labels:            target.Labels,
		ConsentMode:       mode,
		FirstPartyDomains: target.FirstPartyDomains,
		Normalizer:        s.opts.Normalizer,
		IdleQuiet:         target.IdleQuiet,
		HardTimeout:       target.HardTimeout,
		NavTimeout:        target.NavTimeout,
		MaxRequests:       target.MaxRequests,
		MaxBytes:          target.MaxBytes,
		ConsentBudget:     s.opts.ConsentTotalTimeout,
		DwellAfterLoad:    target.DwellAfterLoad,
		ScrollToBottom:    target.ScrollToBottom,
		HashResourceTypes: s.opts.HashResourceTypes,
		StoreBodies:       target.StoreBodies,
		Screenshots:       target.Screenshots,
		ViewportWidth:     target.ViewportWidth,
		ViewportHeight:    target.ViewportHeight,
		DeviceScale:       target.DeviceScale,
		Mobile:            target.Mobile,
		UserAgent:         target.UserAgent,
		AcceptLanguage:    target.AcceptLanguage,
		Timezone:          target.Timezone,
		Latitude:          target.Latitude,
		Longitude:         target.Longitude,
		ExtraHeaders:      target.ExtraHeaders,
		BasicAuthUser:     target.BasicAuthUser,
		BasicAuthPassword: target.BasicAuthPassword,
		Proxy:             target.Proxy,
		WarmCache:         target.WarmCache,
	}
}

func (s *Scanner) artifactSink(kind string, data []byte) (string, error) {
	if s.deps.Store == nil {
		return "", errors.New("artifact storage is not configured")
	}

	return s.deps.Store.PutArtifact(kind, data)
}

func (s *Scanner) checkRobots(ctx context.Context, target config.Resolved) robots.Decision {
	if target.Robots != config.RobotsRespect || s.deps.Robots == nil {
		return robots.Decision{Allowed: true}
	}

	return s.deps.Robots.Check(ctx, target.URL, robots.UserAgent)
}

// persist stores the result. A storage failure is logged but does not discard
// the result: the caller still gets it, and losing the observation silently
// would be worse than a noisy log.
func (s *Scanner) persist(ctx context.Context, log *slog.Logger, res *model.Result) {
	if res == nil {
		return
	}

	// Stamped here rather than at each of the places a result is built,
	// because every result — captured, failed, skipped, panicked — passes
	// through this one function on its way to being stored. Stamping at the
	// call sites would eventually miss one, and the point is that a stored
	// result explains itself (Story 3.8, AC10).
	attempt := attemptOf(ctx)
	res.Attempt = attempt.attempt
	res.Attempts = attempt.attempts
	res.PreviousError = attempt.previous

	if s.deps.Store == nil {
		return
	}

	if err := ctx.Err(); err != nil {
		// Even a cancelled scan's partial result is worth keeping, so the
		// store call is made regardless.
		log.Debug("storing result after cancellation", "error", err)
	}

	if err := s.deps.Store.PutResult(res); err != nil {
		log.Error("storing result failed", "error", err)
	}
}

// compare diffs the result against its baseline. A comparison failure is not
// fatal: the result is already stored and remains useful on its own.
func (s *Scanner) compare(target config.Resolved, res *model.Result, log *slog.Logger) *diff.Report {
	if s.deps.Store == nil {
		return nil
	}

	baseline, err := s.baselineFor(target, res)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Warn("reading baseline failed", "error", err)
	}

	return diff.Compare(baseline, res, diff.Options{
		Allow:    target.Allow,
		Deny:     target.Deny,
		Severity: target.Severity,
	})
}

func (s *Scanner) baselineFor(target config.Resolved, res *model.Result) (*model.Result, error) {
	if s.opts.Baseline == config.BaselineApproved {
		b, err := s.deps.Store.GetBaseline(target.Name, res.ConsentMode)
		if err != nil {
			return nil, err
		}

		return b.Result, nil
	}

	return s.deps.Store.PreviousResult(target.Name, res.ConsentMode, res.ScanID)
}

func (s *Scanner) recordFailure(target string, mode model.ConsentMode, reason string) {
	if s.deps.Metrics.ScanFailed != nil {
		s.deps.Metrics.ScanFailed(target, mode, reason)
	}
}

func (s *Scanner) browserRestarted(target, reason string) {
	if s.deps.Metrics.BrowserRestart != nil {
		s.deps.Metrics.BrowserRestart(target, reason)
	}
}

func (s *Scanner) failedResult(scanID string, target config.Resolved, mode model.ConsentMode, msg string) *model.Result {
	now := time.Now()

	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        scanID,
		Target:        target.Name,
		URL:           target.URL,
		Labels:        target.Labels,
		ConsentMode:   mode,
		StartedAt:     now,
		FinishedAt:    now,
		Termination:   model.TermError,
		Error:         s.scrub(msg),
		Consent:       model.Consent{Outcome: model.OutcomeFailed, Reason: "the scan did not run"},
		Requests:      []model.Request{},
		Environment: model.Environment{
			WsawVersion:   s.opts.WsawVersion,
			ChromeVersion: s.opts.ChromeVersion,
		},
	}
}

func (s *Scanner) skippedResult(scanID string, target config.Resolved, mode model.ConsentMode, reason string) *model.Result {
	now := time.Now()

	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        scanID,
		Target:        target.Name,
		URL:           target.URL,
		Labels:        target.Labels,
		ConsentMode:   mode,
		StartedAt:     now,
		FinishedAt:    now,
		Termination:   model.TermSkipped,
		Error:         reason,
		Consent:       model.Consent{Outcome: model.OutcomeNotNeeded, Reason: "the scan did not run"},
		Requests:      []model.Request{},
		Environment: model.Environment{
			WsawVersion:   s.opts.WsawVersion,
			ChromeVersion: s.opts.ChromeVersion,
		},
	}
}

func (s *Scanner) scrub(text string) string {
	if s.deps.Secrets == nil {
		return text
	}

	return s.deps.Secrets.Scrub(text)
}

// unreachableFromContainer reports whether a target can be reached from a
// containerised browser, and why not.
func (s *Scanner) unreachableFromContainer(target config.Resolved) (string, bool) {
	if s.opts.BrowserRuntime == "" || s.opts.BrowserRuntime == "local" {
		return "", true
	}

	host, _ := hostAndDomain(target.URL)
	if !isLoopbackHost(host) {
		return "", true
	}

	return fmt.Sprintf(
		"%s resolves to this machine's loopback address, which a browser running in a %s container cannot reach: "+
			"the container has its own network namespace. Scan it by its routable address, "+
			"or set browser.runtime to \"local\" for this deployment",
		host, s.opts.BrowserRuntime), false
}

// isLoopbackHost reports whether a host names the local machine.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}

	switch host {
	case "localhost", "::1", "0.0.0.0":
		return true
	}

	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}

	return strings.HasSuffix(host, ".localhost")
}

// isBrowserFailure distinguishes a page that misbehaved, which is a normal
// finding, from a browser that broke, which means the process is suspect.
func isBrowserFailure(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// A timeout is the page's fault, not the browser's.
		return false
	}

	msg := err.Error()

	for _, marker := range []string{
		"websocket", "connection refused", "broken pipe", "use of closed network connection",
		"target closed", "browser is no longer usable", "page crash", "chrome failed",
	} {
		if containsFold(msg, marker) {
			return true
		}
	}

	return false
}

func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}

	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 'a' - 'A'
		}

		return b
	}

	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true

		for j := range len(needle) {
			if lower(haystack[i+j]) != lower(needle[j]) {
				match = false

				break
			}
		}

		if match {
			return true
		}
	}

	return false
}

func errorText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// newScanID returns a random identifier. Scan IDs are not derived from time
// alone: two scans of different targets can start in the same nanosecond.
func newScanID() string {
	var b [12]byte

	if _, err := rand.Read(b[:]); err != nil {
		// Falling back to a time-based ID is worse than failing loudly here,
		// but a scan must not be blocked by entropy trouble, so it degrades.
		return fmt.Sprintf("scan-%d", time.Now().UnixNano())
	}

	return "scan-" + hex.EncodeToString(b[:])
}
