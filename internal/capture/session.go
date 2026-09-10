package capture

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Context is handed to a hook so it can drive the page. It embeds the
// chromedp scan context, so chromedp actions work directly, and carries the
// few capture services a hook legitimately needs.
type Context struct {
	context.Context

	// Screenshot captures evidence under the given kind, if screenshots are
	// enabled. It is a no-op otherwise, so hooks need no conditionals.
	Screenshot func(kind string) error

	// MarkInteracted must be called at the moment the consent action is
	// performed — the click, or the API call — and not after the hook has
	// finished verifying it.
	//
	// Verification can take seconds, and the requests the action triggers
	// arrive during that window. Flipping the phase only at the end would
	// label them pre-consent, which overstates pre-consent tracking: the one
	// number this product must not exaggerate.
	MarkInteracted func()
}

// Run performs one capture. scanCtx must be an isolated chromedp context, as
// produced by browser.Browser.NewScanContext.
//
// Run always returns a Result, even on failure: a scan that broke is a
// recorded outcome, not an absence of data (Tenet 5). The error is returned
// alongside for logging, and is also recorded in the Result.
func Run(ctx context.Context, scanCtx context.Context, rawOpts Options, hooks Hooks) (*model.Result, error) {
	opts := rawOpts.withDefaults()

	if opts.Normalizer == nil {
		return nil, errors.New("capture: Normalizer is required")
	}

	cl, err := classify.New(opts.URL, opts.FirstPartyDomains)
	if err != nil {
		return nil, fmt.Errorf("capture: %w", err)
	}

	start := time.Now()

	res := &model.Result{
		SchemaVersion: model.SchemaVersion,
		Target:        opts.Target,
		URL:           opts.URL,
		Labels:        opts.Labels,
		ConsentMode:   opts.ConsentMode,
		StartedAt:     start,
		Environment:   opts.environment(),
		Consent:       model.Consent{Outcome: model.OutcomeNotNeeded},
		Requests:      []model.Request{},
	}

	// The whole capture is bounded. Exceeding the budget is a recorded
	// termination, not a hang.
	runCtx, cancelRun := context.WithTimeout(scanCtx, opts.HardTimeout)
	defer cancelRun()

	// The caller's cancellation must also stop capture.
	stopParent := context.AfterFunc(ctx, cancelRun)
	defer stopParent()

	rec := newRecorder(start, cl, opts.Normalizer, opts.HashResourceTypes,
		opts.MaxRequests, opts.MaxBytes, opts.StallAfter,
		opts.MaxBodyBytes, opts.StoreBodies, opts.BodySink)

	s := &session{opts: opts, rec: rec, res: res, runCtx: runCtx, cancelRun: cancelRun, start: start}

	s.listen()

	bodyDone := s.startBodyWorker()

	err = s.execute(hooks)

	// The body worker is stopped before the result is assembled so that every
	// digest it produced is included.
	s.stopBodyWorker()
	<-bodyDone

	s.finish(err)

	// A budget that ran out is a recorded outcome, not a failure. The
	// termination reason already carries it and the result is usable, so
	// returning an error as well would have the daemon warn on every scan of
	// any site that never reaches network idle — which is a great many of
	// them. Warnings that fire constantly are warnings operators stop reading.
	//
	// Cancellation is different and stays an error: it means the caller gave
	// up, and the result is genuinely partial.
	if err != nil && errors.Is(err, context.DeadlineExceeded) && res.Truncated() {
		err = nil
	}

	if err != nil {
		return res, err
	}

	return res, nil
}

type session struct {
	opts Options
	rec  *recorder
	res  *model.Result

	start time.Time

	runCtx    context.Context
	cancelRun context.CancelFunc

	bodyStop     chan struct{}
	bodyStopOnce sync.Once

	mu          sync.Mutex
	screenshots []model.Artifact

	// surfaceGone records that a capture from the compositor surface already
	// timed out once. Screenshots are taken repeatedly in one scan, and a
	// browser that produced no frame for the first request will not produce
	// one for the next: retrying would spend the reservation again per frame.
	surfaceGone bool

	interactedMu   sync.Mutex
	interactedOnce bool
	interactedAt   time.Time
}

// markInteracted ends the pre-consent phase. It is idempotent, so the consent
// handler can call it as soon as it acts and capture can call it again as a
// backstop when the hook returns.
func (s *session) markInteracted() {
	s.interactedMu.Lock()
	defer s.interactedMu.Unlock()

	if s.interactedOnce {
		return
	}

	s.interactedOnce = true
	s.interactedAt = time.Now()

	s.rec.setPhase(model.PhasePost)
}

// interacted reports whether the pre-consent phase has already been ended by
// an actual interaction, as opposed to capture's own backstop.
func (s *session) interacted() bool {
	s.interactedMu.Lock()
	defer s.interactedMu.Unlock()

	return s.interactedOnce
}

// listen registers the CDP event handlers. Handlers must not block: they hand
// work to the recorder and return immediately.
func (s *session) listen() {
	chromedp.ListenTarget(s.runCtx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			s.rec.requestWillBeSent(e)
		case *network.EventResponseReceived:
			s.rec.responseReceived(e)
		case *network.EventLoadingFinished:
			s.rec.loadingFinished(e)
		case *network.EventLoadingFailed:
			s.rec.loadingFailed(e)
		case *network.EventRequestServedFromCache:
			s.rec.servedFromCache(e)
		case *network.EventWebSocketCreated:
			s.rec.webSocketCreated(e)

		case *page.EventJavascriptDialogOpening:
			// Dialog spam would otherwise block the page forever (Story 6.6).
			// Dismissing runs in its own goroutine because a CDP call from
			// inside a listener would deadlock.
			go s.dismissDialog(e)

		case *browser.EventDownloadWillBegin:
			s.rec.addWarning("page attempted a download; downloads are suppressed")
		}
	})
}

func (s *session) dismissDialog(ev *page.EventJavascriptDialogOpening) {
	const dialogTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(s.runCtx, dialogTimeout)
	defer cancel()

	// beforeunload must be accepted or the page will not navigate away;
	// everything else is dismissed so the page cannot extract a decision.
	accept := ev.Type == page.DialogTypeBeforeunload

	if err := chromedp.Run(ctx, page.HandleJavaScriptDialog(accept)); err != nil {
		s.rec.addWarning("could not dismiss a JavaScript dialog: " + s.scrub(err.Error()))
	}
}

// execute prepares the page, navigates, settles, runs the consent hook, and
// settles again.
func (s *session) execute(hooks Hooks) error {
	if err := s.prepare(); err != nil {
		return err
	}

	if err := s.navigate(); err != nil {
		return err
	}

	// Settle before interacting, so the pre-consent request set is complete —
	// but not for the whole scan budget.
	//
	// Plenty of real sites never reach network idle: analytics heartbeats,
	// long-polling and video players keep requests in flight indefinitely. On
	// those, settling without a reservation consumes the entire budget, the
	// consent hook then runs against an expired context, and the scan reports
	// "no CMP detected" for a page that plainly has one. That is a false
	// negative on the product's headline finding, so the interaction gets its
	// budget reserved up front.
	s.settle(s.preConsentCtx(hooks))

	if err := s.screenshot("before-consent"); err != nil {
		s.rec.addWarning("screenshot before consent failed: " + s.scrub(err.Error()))
	}

	if hooks.AfterLoad != nil {
		hookCtx := Context{
			Context:        s.runCtx,
			Screenshot:     s.screenshot,
			MarkInteracted: s.markInteracted,
		}

		consent, err := hooks.AfterLoad(hookCtx)

		// The consent outcome is recorded whatever happened; a failed
		// interaction is a finding, not a lost scan.
		s.res.Consent = consent

		// Whether the hook itself acted, read before the backstop below makes
		// the answer yes for every scan. Only the hook's own call means the
		// page was touched.
		acted := s.interacted()

		// A hook that never reported an interaction still ends the pre-consent
		// phase here: the phase describes the timeline, not success.
		s.markInteracted()

		if s.res.Consent.InteractedAt == nil {
			s.interactedMu.Lock()
			at := s.interactedAt
			s.interactedMu.Unlock()

			if !at.IsZero() {
				s.res.Consent.InteractedAt = &at
			}
		}

		if err != nil {
			// A consent failure does not abort the scan: the traffic observed
			// under a failed interaction is exactly what a reviewer needs.
			s.rec.addWarning("consent interaction: " + s.scrub(err.Error()))
		}

		// Only shoot the second frame if something actually happened to the
		// page. Consent mode "none" returns from the hook without touching
		// anything, and so does a page with no banner to click; shooting
		// anyway costs a Chrome round-trip to produce a byte-identical copy
		// of the first frame, which a reader then has to be told is not
		// evidence of an interaction that never took place.
		if acted {
			if err := s.screenshot("after-consent"); err != nil {
				s.rec.addWarning("screenshot after consent failed: " + s.scrub(err.Error()))
			} else {
				s.recordScreenshotIdentity()
			}
		}

		s.settle(s.runCtx)
	}

	if s.opts.ScrollToBottom {
		s.scroll()
		s.settle(s.runCtx)
	}

	if s.opts.DwellAfterLoad > 0 {
		s.dwell()
		s.settle(s.runCtx)
	}

	return s.runCtx.Err()
}

// preConsentCtx bounds the settle that precedes the consent interaction, so a
// page that never goes idle cannot starve it.
//
// The reservation is capped at half the scan budget: a site that does settle
// normally should still get most of the budget for its initial load, and
// reserving more would trade a real false negative for a different one.
func (s *session) preConsentCtx(hooks Hooks) context.Context {
	if hooks.AfterLoad == nil {
		return s.runCtx
	}

	reserve := s.opts.consentReserve()
	if reserve <= 0 {
		return s.runCtx
	}

	deadline := s.start.Add(s.opts.HardTimeout - reserve)

	// A budget already spent leaves nothing to bound; the interaction runs
	// against whatever remains and reports honestly if that is nothing.
	if !deadline.After(time.Now()) {
		return s.runCtx
	}

	ctx, cancel := context.WithDeadline(s.runCtx, deadline)

	// The context is only used for the settle that follows immediately; the
	// cancel runs when the scan ends.
	context.AfterFunc(s.runCtx, cancel)

	return ctx
}

// prepare configures the browser context before navigation: emulation,
// headers, and the network domain.
func (s *session) prepare() error {
	actions := []chromedp.Action{
		network.Enable(),
		page.Enable(),
		runtime.Enable(),

		// A scanned page must never be able to write a file to disk.
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDeny),

		emulation.SetDeviceMetricsOverride(
			int64(s.opts.ViewportWidth), int64(s.opts.ViewportHeight),
			s.opts.DeviceScale, s.opts.Mobile,
		),
	}

	// Cookies are cleared for every scan, warm cache or not. A stored consent
	// decision is precisely what must not carry over: a scan that inherits one
	// sees no banner and records the site's whole tracking stack as
	// pre-consent traffic. The browser process is the real isolation boundary
	// (one scan per browser by default); this is the belt to that braces, and
	// it is what keeps a deliberately reused browser honest.
	actions = append(actions, network.ClearBrowserCookies())

	if !s.opts.WarmCache {
		// Cold cache is the default: it is what makes two scans comparable
		// and what an unprimed visitor actually experiences.
		actions = append(actions, network.SetCacheDisabled(true), network.ClearBrowserCache())
	}

	if s.opts.UserAgent != "" || s.opts.AcceptLanguage != "" {
		ua := emulation.SetUserAgentOverride(s.opts.UserAgent)
		if s.opts.AcceptLanguage != "" {
			ua = ua.WithAcceptLanguage(s.opts.AcceptLanguage)
		}

		actions = append(actions, ua)
	}

	if s.opts.Timezone != "" {
		actions = append(actions, emulation.SetTimezoneOverride(s.opts.Timezone))
	}

	if s.opts.Latitude != nil && s.opts.Longitude != nil {
		actions = append(actions, emulation.SetGeolocationOverride().
			WithLatitude(*s.opts.Latitude).
			WithLongitude(*s.opts.Longitude).
			WithAccuracy(1))
	}

	if headers := s.headers(); len(headers) > 0 {
		actions = append(actions, network.SetExtraHTTPHeaders(headers))
	}

	ctx, cancel := context.WithTimeout(s.runCtx, s.opts.NavTimeout)
	defer cancel()

	if err := chromedp.Run(ctx, actions...); err != nil {
		return fmt.Errorf("preparing page: %w", err)
	}

	return nil
}

// headers builds the extra header map, revealing secrets only here and never
// storing them.
func (s *session) headers() network.Headers {
	headers := network.Headers{}

	for name, value := range s.opts.ExtraHeaders {
		headers[name] = value.Reveal()
	}

	if s.opts.BasicAuthUser.IsSet() {
		headers["Authorization"] = "Basic " + basicAuth(
			s.opts.BasicAuthUser.Reveal(),
			s.opts.BasicAuthPassword.Reveal(),
		)
	}

	if len(headers) == 0 {
		return nil
	}

	return headers
}

func (s *session) navigate() error {
	ctx, cancel := context.WithTimeout(s.runCtx, s.opts.NavTimeout)
	defer cancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(s.opts.URL)); err != nil {
		return fmt.Errorf("navigating to %s: %w", s.opts.URL, err)
	}

	return nil
}

// settle waits until the network is quiet, the budget is exhausted, or ctx
// ends. Callers pass a context narrower than the scan budget when work still
// has to happen afterwards.
func (s *session) settle(ctx context.Context) {
	quiet := time.NewTimer(s.opts.IdleQuiet)
	defer quiet.Stop()

	// A short poll covers the case where inflight was already zero before the
	// waiter started, and bounds the effect of a missed signal.
	const poll = 250 * time.Millisecond

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		inflight, exceeded := s.rec.snapshot()

		if exceeded != capNone {
			return
		}

		if inflight == 0 && drained(quiet) {
			return
		}

		select {
		case <-ctx.Done():
			return

		case <-s.rec.idleSignal:
			// The network just went quiet; start the quiet window again.
			s.restartQuiet(quiet)

		case <-ticker.C:
			if busy, _ := s.rec.snapshot(); busy > 0 {
				// Still loading, so the quiet window has not begun.
				s.restartQuiet(quiet)
			}

		case <-quiet.C:
			if idle, _ := s.rec.snapshot(); idle == 0 {
				return
			}

			quiet.Reset(s.opts.IdleQuiet)
		}
	}
}

// drained reports whether the quiet window has already elapsed, without
// blocking on it.
func drained(quiet *time.Timer) bool {
	select {
	case <-quiet.C:
		return true
	default:
		return false
	}
}

// restartQuiet begins the quiet window again, draining the timer first so a
// pending tick cannot end the wait early.
func (s *session) restartQuiet(quiet *time.Timer) {
	if !quiet.Stop() {
		select {
		case <-quiet.C:
		default:
		}
	}

	quiet.Reset(s.opts.IdleQuiet)
}

func (s *session) scroll() {
	const scrollTimeout = 15 * time.Second

	ctx, cancel := context.WithTimeout(s.runCtx, scrollTimeout)
	defer cancel()

	// Stepped scrolling rather than a single jump, because lazy-load
	// observers only fire for viewports they actually pass through.
	const script = `(async () => {
		const step = Math.max(200, window.innerHeight * 0.8);
		let y = 0;
		for (let i = 0; i < 40; i++) {
			window.scrollTo(0, y);
			y += step;
			if (y > document.body.scrollHeight) break;
			await new Promise(r => setTimeout(r, 100));
		}
		window.scrollTo(0, 0);
	})()`

	if err := chromedp.Run(ctx, chromedp.Evaluate(script, nil, awaitPromise)); err != nil {
		s.rec.addWarning("scrolling to bottom failed: " + s.scrub(err.Error()))
	}
}

func (s *session) dwell() {
	timer := time.NewTimer(s.opts.DwellAfterLoad)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-s.runCtx.Done():
	}
}

// startBodyWorker fingerprints response bodies while Chrome still holds them.
func (s *session) startBodyWorker() <-chan struct{} {
	s.bodyStop = make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-s.bodyStop:
				return

			case <-s.runCtx.Done():
				return

			case id := <-s.rec.bodyWanted:
				s.fingerprintBody(id)
			}
		}
	}()

	return done
}

func (s *session) stopBodyWorker() {
	s.bodyStopOnce.Do(func() {
		// Drain what is already queued before stopping, so digests are not
		// lost just because the page finished quickly.
		for {
			select {
			case id := <-s.rec.bodyWanted:
				s.fingerprintBody(id)
			default:
				close(s.bodyStop)

				return
			}
		}
	})
}

func (s *session) fingerprintBody(id network.RequestID) {
	ctx, cancel := context.WithTimeout(s.runCtx, DefaultBodyTimeout)
	defer cancel()

	var body []byte

	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		b, err := network.GetResponseBody(id).Do(ctx)
		if err != nil {
			return err
		}

		body = b

		return nil
	}))
	if err != nil {
		// Chrome evicts bodies from its cache aggressively. That is a fact
		// about the observation, so it is recorded rather than ignored.
		s.rec.setBodyDigest(id, "", 0, "", "body not retrievable: "+s.scrub(err.Error()))

		return
	}

	if int64(len(body)) > s.opts.MaxBodyBytes {
		s.rec.setBodyDigest(id, "", 0, "",
			fmt.Sprintf("body larger than the %d byte cap", s.opts.MaxBodyBytes))

		return
	}

	digest := sha256Hex(body)

	var ref string

	if s.opts.StoreBodies && s.opts.BodySink != nil {
		r, err := s.opts.BodySink("body", body)
		if err != nil {
			s.rec.addWarning("storing a response body failed: " + s.scrub(err.Error()))
		} else {
			ref = r
		}
	}

	s.rec.setBodyDigest(id, digest, len(body), ref, "")

	// The identity is extracted here, where the body is still in hand: the
	// diff is pure and never reads a stored artifact (Tenet 3), so anything
	// it needs to compare has to be recorded as an observation.
	if s.opts.Normalizer != nil && s.opts.Normalizer.HasBodyIdentities() {
		if u := s.rec.requestURL(id); u != "" {
			label, value := s.opts.Normalizer.BodyIdentity(u, string(body))
			s.rec.setBodyIdentity(id, label, value)
		}
	}
}

// recordScreenshotIdentity notes when the before- and after-interaction
// screenshots are byte-for-byte identical, directly on the consent result.
//
// The HTTP UI already computes this to caption the screenshot pair, but a
// caption is not evidence a JSON consumer can see (Tenet 16): a claimed
// interaction that left the page looking unchanged belongs in the document
// wsaw hands over, not only in a page rendered from it (Story 2.7, AC7).
func (s *session) recordScreenshotIdentity() {
	s.mu.Lock()
	defer s.mu.Unlock()

	var before, after string

	for _, shot := range s.screenshots {
		switch shot.Kind {
		case "screenshot-before-consent":
			before = shot.SHA256
		case "screenshot-after-consent":
			after = shot.SHA256
		}
	}

	if before != "" && before == after {
		s.res.Consent.ScreenshotsIdentical = true
	}
}

func (s *session) screenshot(kind string) error {
	if !s.opts.Screenshots || s.opts.ScreenshotSink == nil {
		return nil
	}

	buf, err := s.captureFrame()
	if err != nil {
		return fmt.Errorf("capturing screenshot: %w", err)
	}

	ref, err := s.opts.ScreenshotSink("screenshot-"+kind, buf)
	if err != nil {
		return fmt.Errorf("storing screenshot: %w", err)
	}

	s.mu.Lock()
	s.screenshots = append(s.screenshots, model.Artifact{
		Kind:   "screenshot-" + kind,
		Ref:    ref,
		SHA256: sha256Hex(buf),
		Bytes:  int64(len(buf)),
	})
	s.mu.Unlock()

	return nil
}

// captureFrame photographs the page.
//
// Two capture paths exist and both are needed. Capturing from the surface is
// what the compositor actually put on screen, so it is tried first — but
// Chrome answers it only once the compositor produces a frame, and a headless
// browser with nothing left to draw produces none. On a Linux CI runner that
// is the ordinary case after the page has settled: the request never returns
// and the whole screenshot budget is spent waiting for a frame that has no
// reason to exist. That cost the after-consent frame on every Linux scan, and
// a missing frame is exactly the evidence this product is for.
//
// So the surface path gets a reservation rather than the whole budget, and
// what remains pays for a capture from the renderer view, which composes the
// page on demand and needs no frame. The fallback is second, not first,
// because it omits anything the browser draws over the page.
func (s *session) captureFrame() ([]byte, error) {
	const (
		surfaceBudget = 5 * time.Second
		viewBudget    = 10 * time.Second
	)

	var surfaceErr error

	if !s.surfaceUnavailable() {
		surfaceCtx, cancelSurface := context.WithTimeout(s.runCtx, surfaceBudget)
		defer cancelSurface()

		var buf []byte

		// Brought to front first: Chrome composites the visible tab, and a
		// scan tab that is not the frontmost one has no frame to hand over at
		// all.
		buf, surfaceErr = s.captureFromSurface(surfaceCtx)
		if surfaceErr == nil {
			return buf, nil
		}

		s.markSurfaceUnavailable()

		// Recorded once, because the two paths can differ and a reader
		// comparing frames is entitled to know they were taken differently.
		s.rec.addWarning("screenshots taken from the renderer view: the " +
			"compositor produced no frame (" + s.scrub(surfaceErr.Error()) + ")")
	}

	viewCtx, cancelView := context.WithTimeout(s.runCtx, viewBudget)
	defer cancelView()

	buf, viewErr := s.captureFromView(viewCtx)
	if viewErr != nil {
		if surfaceErr != nil {
			return nil, fmt.Errorf("from the surface: %w; from the renderer view: %w",
				surfaceErr, viewErr)
		}

		return nil, viewErr
	}

	return buf, nil
}

func (s *session) surfaceUnavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.surfaceGone
}

func (s *session) markSurfaceUnavailable() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.surfaceGone = true
}

func (s *session) captureFromSurface(ctx context.Context) ([]byte, error) {
	var buf []byte

	if err := chromedp.Run(ctx, page.BringToFront(), captureAction(&buf, true)); err != nil {
		return nil, err
	}

	return buf, nil
}

func (s *session) captureFromView(ctx context.Context) ([]byte, error) {
	var buf []byte

	if err := chromedp.Run(ctx, captureAction(&buf, false)); err != nil {
		return nil, err
	}

	return buf, nil
}

func captureAction(buf *[]byte, fromSurface bool) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		out, err := page.CaptureScreenshot().WithFromSurface(fromSurface).Do(ctx)
		if err != nil {
			return err
		}

		*buf = out

		return nil
	})
}

// finish assembles the result, including the termination reason, which must
// always explain why capture stopped.
func (s *session) finish(runErr error) {
	s.res.Requests = s.rec.requests()
	s.res.Warnings = append(s.res.Warnings, s.rec.capturedWarnings()...)

	// Requests still in flight at the end are reported rather than left to be
	// inferred from a missing end offset, so a reader can tell an incomplete
	// observation from a fast one.
	if stalled := s.rec.stalled(); len(stalled) > 0 {
		s.res.Warnings = append(s.res.Warnings, fmt.Sprintf(
			"%d request(s) never reported completion and were not waited for; "+
				"this is usual for cross-origin iframes, whose completion events go to a separate browser target",
			len(stalled)))
	}

	s.mu.Lock()
	s.res.Screenshots = s.screenshots
	s.mu.Unlock()

	s.collectCookies()
	s.collectFinalURL()

	s.res.FinishedAt = time.Now()
	s.res.Duration = s.res.FinishedAt.Sub(s.res.StartedAt)

	_, exceeded := s.rec.snapshot()

	switch {
	case runErr != nil && !errors.Is(runErr, context.DeadlineExceeded):
		s.res.Termination = model.TermError
		s.res.Error = s.scrub(runErr.Error())

	case exceeded == capRequests:
		s.res.Termination = model.TermRequestCap

	case exceeded == capBytes:
		s.res.Termination = model.TermByteCap

	case errors.Is(s.runCtx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded):
		s.res.Termination = model.TermTimeout

	case s.runCtx.Err() != nil:
		// Cancelled by the caller: the result is partial and must say so.
		s.res.Termination = model.TermError
		s.res.Error = "scan cancelled: " + s.runCtx.Err().Error()

	default:
		s.res.Termination = model.TermIdle
	}
}

// collectCookies reads the cookie jar after the scan. Values are fingerprinted
// rather than stored, since they routinely contain identifiers.
func (s *session) collectCookies() {
	const cookieTimeout = 10 * time.Second

	// A fresh context: the run context may already be done, and the cookies
	// are still worth collecting.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.runCtx), cookieTimeout)
	defer cancel()

	cl, err := classify.New(s.opts.URL, s.opts.FirstPartyDomains)
	if err != nil {
		return
	}

	var cookies []*network.Cookie

	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		get := storage.GetCookies()

		// Scoped to this scan's browser context. Unscoped, Storage.getCookies
		// answers for the browser's default context, which is not where an
		// isolated scan's cookies live — it would report another scan's jar,
		// or none at all.
		if c := chromedp.FromContext(ctx); c != nil && c.BrowserContextID != "" {
			get = get.WithBrowserContextID(c.BrowserContextID)
		}

		res, err := get.Do(ctx)
		if err != nil {
			return err
		}

		cookies = res

		return nil
	}))
	if err != nil {
		s.res.Warnings = append(s.res.Warnings, "cookies could not be read: "+s.scrub(err.Error()))

		return
	}

	out := make([]model.Cookie, 0, len(cookies))

	for _, c := range cookies {
		mc := model.Cookie{
			Name:        c.Name,
			Domain:      c.Domain,
			Path:        c.Path,
			SameSite:    string(c.SameSite),
			Secure:      c.Secure,
			HTTPOnly:    c.HTTPOnly,
			Party:       cl.ClassifyCookieDomain(c.Domain),
			ValueSHA256: sha256Hex([]byte(c.Value)),
			ValueLength: len(c.Value),
		}

		if c.Session || c.Expires <= 0 {
			mc.Session = true
		} else {
			mc.Expires = time.Unix(int64(c.Expires), 0).UTC()
		}

		out = append(out, mc)
	}

	sortCookies(out)
	s.res.Cookies = out
}

func (s *session) collectFinalURL() {
	const urlTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.runCtx), urlTimeout)
	defer cancel()

	var final string

	if err := chromedp.Run(ctx, chromedp.Location(&final)); err == nil {
		s.res.FinalURL = final
	}
}

// scrub removes any registered secret from text before it is stored or logged.
func (s *session) scrub(text string) string {
	if s.opts.Secrets == nil {
		return text
	}

	return s.opts.Secrets.Scrub(text)
}

func (o *Options) environment() model.Environment {
	env := model.Environment{
		WsawVersion:    o.WsawVersion,
		ChromeVersion:  o.ChromeVersion,
		UserAgent:      o.UserAgent,
		ViewportWidth:  o.ViewportWidth,
		ViewportHeight: o.ViewportHeight,
		DeviceScale:    o.DeviceScale,
		Mobile:         o.Mobile,
		AcceptLanguage: o.AcceptLanguage,
		Timezone:       o.Timezone,
		Latitude:       o.Latitude,
		Longitude:      o.Longitude,
		BasicAuth:      o.BasicAuthUser.IsSet(),
		Proxy:          secret.RedactURL(o.Proxy),
		WarmCache:      o.WarmCache,
		BrowserReused:  o.BrowserReused,
		BrowserRuntime: o.BrowserRuntime,
		BrowserImage:   o.BrowserImage,
		BrowserSandbox: o.BrowserSandbox,
	}

	// Header names are echoed for reproducibility; values never are.
	for name := range o.ExtraHeaders {
		env.ExtraHeaders = append(env.ExtraHeaders, name)
	}

	sortStrings(env.ExtraHeaders)

	return env
}

func basicAuth(user, password string) string {
	return base64Encode(user + ":" + password)
}

// awaitPromise makes chromedp await the evaluated promise.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}
