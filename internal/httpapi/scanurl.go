package httpapi

// Scanning a URL that is not in the target list (Story 5.27).
//
// Everywhere else, "which addresses may this wsaw fetch" is answered by a
// configuration file. Here it is answered by whoever is looking at the page,
// which is why the feature is off unless a deployment turned it on, why the
// admission checks live behind a seam that re-runs them at scan time, and why
// a budget bounds how often it can happen at all.
//
// The interface and the API reach the same seam: the page gets no privileged
// path into scanning that a client with the token does not have (Tenet 16).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
)

// URLScanner scans an address that nobody configured as a target.
//
// It is two calls rather than one because the interface answers the reader
// before the scan finishes: AcceptURL says whether the address will be fetched
// and under what name its results are stored, so a refusal lands on the page
// the URL was typed on, and Scan does the work afterwards.
type URLScanner interface {
	// AcceptURL validates a typed URL and reports the target name its results
	// are stored under, or why it will not be scanned. It starts nothing.
	AcceptURL(ctx context.Context, rawURL string, mode model.ConsentMode) (string, error)
	// ScanURL runs one scan of a typed URL. It repeats AcceptURL's checks: the
	// address is examined again at the moment it is about to be fetched.
	ScanURL(ctx context.Context, rawURL string, mode model.ConsentMode) (scanner.Outcome, error)
}

// urlScanPath is the form's page, and the write action it posts to.
const urlScanPath = "/scan"

// maxURLScanBody bounds the JSON body of an API request. A URL and a consent
// mode need nothing like this much room.
const maxURLScanBody = 1 << 13

func (s *Server) urlScanEnabled() bool {
	return s.opts.AllowURLScan && s.deps.URLScanner != nil && !s.opts.ReadOnly
}

// urlScanModes are the consent modes the form offers, falling back to the
// mode every other target gets when a deployment named none.
func (s *Server) urlScanModes() []model.ConsentMode {
	if len(s.opts.URLScanModes) > 0 {
		return s.opts.URLScanModes
	}

	return []model.ConsentMode{model.ConsentReject}
}

func (s *Server) urlScanOffers(mode model.ConsentMode) bool {
	for _, m := range s.urlScanModes() {
		if m == mode {
			return true
		}
	}

	return false
}

// urlScanData is what the form page renders.
type urlScanData struct {
	// URL is what the reader last typed, kept so a refusal does not also cost
	// them their typing.
	URL string

	Modes     []model.ConsentMode
	Mode      model.ConsentMode
	PerHour   int
	Remaining int

	// AllowPrivateHosts is stated on the page, because it changes what this
	// wsaw can be pointed at and a reader should not have to read the
	// configuration file to find out.
	AllowPrivateHosts bool
}

func (s *Server) handleUIScanURL(w http.ResponseWriter, r *http.Request) {
	if !s.urlScanEnabled() {
		s.uiError(w, r, http.StatusNotFound, "scanning a typed URL is not enabled on this wsaw")

		return
	}

	data := urlScanData{
		URL:               r.URL.Query().Get("url"),
		Modes:             s.urlScanModes(),
		Mode:              s.urlScanModes()[0],
		PerHour:           s.urlScanBudget.limit,
		Remaining:         s.urlScanBudget.remaining(time.Now()),
		AllowPrivateHosts: s.opts.URLScanPrivateHosts,
	}

	if mode := model.ConsentMode(r.URL.Query().Get("mode")); s.urlScanOffers(mode) {
		data.Mode = mode
	}

	// The page holds still. Everything on it is a form the reader is in the
	// middle of filling in, and a page that reloads under somebody's typing
	// has taken their work away (Story 5.18).
	s.renderStill(w, r, "scanurl.html", "Scan a URL", data,
		"this page holds still while you type")
}

func (s *Server) handleUIScanURLSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}

	if allowed, reason := s.writeAllowed(); !allowed {
		s.uiRedirectError(w, r, "/", reason)

		return
	}

	if !s.urlScanEnabled() {
		s.uiError(w, r, http.StatusNotFound, "scanning a typed URL is not enabled on this wsaw")

		return
	}

	rawURL := strings.TrimSpace(r.FormValue("url"))
	mode := s.urlScanModes()[0]

	if m := model.ConsentMode(r.FormValue("mode")); m != "" {
		if !s.urlScanOffers(m) {
			s.refusedToForm(w, r, rawURL, mode, "consent mode "+string(m)+" is not offered here")

			return
		}

		mode = m
	}

	name, err := s.deps.URLScanner.AcceptURL(r.Context(), rawURL, mode)
	if err != nil {
		s.refusedToForm(w, r, rawURL, mode, err.Error())

		return
	}

	if len(runningFor(s.running(), name, mode)) > 0 {
		s.uiRedirectError(w, r, seriesPath(name, mode),
			"A scan of this address and consent mode is already running. "+
				"It appears as pending until it finishes.")

		return
	}

	if wait, ok := s.urlScanBudget.take(time.Now()); !ok {
		s.refusedToForm(w, r, rawURL, mode, budgetRefusal(s.urlScanBudget.limit, wait))

		return
	}

	s.startURLScan(r, rawURL, name, mode)

	s.uiRedirectOK(w, r, seriesPath(name, mode),
		"Scan started for "+name+" ("+string(mode)+"). It appears as pending until it finishes.")
}

// refusedToForm sends the reader back to the form with what they typed, the
// mode they chose, and why nothing was scanned.
//
// The flash helpers drop any query on their destination, because everywhere
// else in the interface a query on a redirect target is smuggled input
// (safeLocal). Here the query is the message: a refusal that also empties the
// field makes somebody retype an address to find out what was wrong with it.
// So it is rebuilt from values this handler owns, onto a constant path.
func (s *Server) refusedToForm(
	w http.ResponseWriter, r *http.Request, rawURL string, mode model.ConsentMode, msg string,
) {
	q := url.Values{}
	q.Set("url", rawURL)
	q.Set("mode", string(mode))
	q.Set("err", msg)

	http.Redirect(w, r, urlScanPath+"?"+q.Encode(), http.StatusSeeOther)
}

// startURLScan runs the scan after the reader has been answered.
//
// Detached from the request context, which is cancelled the moment the
// redirect is written, and bounded so a wedged scan cannot outlive the budget
// any other ad-hoc scan gets.
func (s *Server) startURLScan(r *http.Request, rawURL, name string, mode model.ConsentMode) {
	scanCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), adHocScanBudget)

	go func() {
		defer cancel()

		if _, err := s.deps.URLScanner.ScanURL(scanCtx, rawURL, mode); err != nil {
			// The series name rather than the address: it identifies the scan
			// in the store and in every other log line about it, and the
			// address itself is not repeated into the log by this handler.
			s.deps.Logger.Warn("scan of a typed URL reported an error",
				"target", name, "consent_mode", string(mode), "error", err)
		}
	}()
}

// handleScanURL is the API's synchronous path: a client that posts a URL
// wants the result, not a page.
func (s *Server) handleScanURL(w http.ResponseWriter, r *http.Request) {
	if allowed, reason := s.writeAllowed(); !allowed {
		writeJSONError(w, http.StatusForbidden, reason)

		return
	}

	if !s.urlScanEnabled() {
		writeJSONError(w, http.StatusForbidden, "scanning a typed URL is not enabled on this wsaw")

		return
	}

	var body struct {
		URL         string            `json:"url"`
		ConsentMode model.ConsentMode `json:"consentMode"`
	}

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxURLScanBody)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())

		return
	}

	mode := body.ConsentMode
	if mode == "" {
		mode = s.urlScanModes()[0]
	}

	if !s.urlScanOffers(mode) {
		writeJSONError(w, http.StatusBadRequest, "consent mode "+string(mode)+" is not offered here")

		return
	}

	name, err := s.deps.URLScanner.AcceptURL(r.Context(), body.URL, mode)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())

		return
	}

	if live := runningFor(s.running(), name, mode); len(live) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			fieldError:   "a scan of this address and consent mode is already running",
			fieldRunning: live,
		})

		return
	}

	if wait, ok := s.urlScanBudget.take(time.Now()); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		writeJSONError(w, http.StatusTooManyRequests, budgetRefusal(s.urlScanBudget.limit, wait))

		return
	}

	out, err := s.deps.URLScanner.ScanURL(r.Context(), body.URL, mode)
	if err != nil {
		s.deps.Logger.Warn("scan of a typed URL reported an error",
			"target", name, "consent_mode", string(mode), "error", err)
	}

	if out.Result == nil {
		writeJSONError(w, http.StatusInternalServerError, "the scan produced no result")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"target": name,
		"result": out.Result,
		"diff":   out.Diff,
	})
}

func seriesPath(target string, mode model.ConsentMode) string {
	return "/targets/" + url.PathEscape(target) + "/" + url.PathEscape(string(mode))
}

func budgetRefusal(limit int, wait time.Duration) string {
	return "This wsaw starts at most " + strconv.Itoa(limit) + " scans of typed URLs per hour, " +
		"and that budget is spent. The next one can start in " + wait.Round(time.Second).String() + "."
}

func retryAfterSeconds(wait time.Duration) string {
	secs := int(wait.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}

	return strconv.Itoa(secs)
}

// rollingBudget bounds how many scans of typed URLs are started in any hour,
// across everybody using this wsaw.
//
// A rolling window rather than a bucket per caller: the limit exists to
// protect the sites being scanned, which do not care which reader asked
// (Tenet 17, Story 5.4 AC3). It is deliberately a whole-deployment budget, so
// a shared interface cannot be turned into a crawler by opening a second tab.
type rollingBudget struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	started []time.Time
}

func newRollingBudget(limit int, window time.Duration) *rollingBudget {
	return &rollingBudget{limit: limit, window: window}
}

// take spends one scan from the budget. When there is none left it reports
// how long until there is.
func (b *rollingBudget) take(now time.Time) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.expire(now)

	if len(b.started) >= b.limit {
		// The oldest start is what frees the next slot.
		return b.window - now.Sub(b.started[0]), false
	}

	b.started = append(b.started, now)

	return 0, true
}

// remaining reports how much budget is left, for the page that says so.
func (b *rollingBudget) remaining(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.expire(now)

	return b.limit - len(b.started)
}

// expire drops the starts that have fallen out of the window. The caller
// holds the lock.
func (b *rollingBudget) expire(now time.Time) {
	cut := 0

	for cut < len(b.started) && now.Sub(b.started[cut]) >= b.window {
		cut++
	}

	b.started = b.started[cut:]
}
