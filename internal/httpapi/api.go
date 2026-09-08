package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// storeProbeTimeout bounds the readiness store check. A probe that hangs is
// worse than one that fails: an orchestrator waits on it instead of acting.
const storeProbeTimeout = 3 * time.Second

// JSON field names that appear in more than one response.
const (
	fieldReason  = "reason"
	fieldRunning = "running"
	fieldReady   = "ready"
)

func (s *Server) routes() {
	// API. Read paths first; write paths are gated by writeAllowed.
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/ready", s.handleReady)
	s.mux.HandleFunc("GET /api/v1/targets", s.handleTargets)
	s.mux.HandleFunc("GET /api/v1/running", s.handleRunning)
	s.mux.HandleFunc("GET /api/v1/results/{target}/{mode}", s.handleResults)
	s.mux.HandleFunc("GET /api/v1/results/{target}/{mode}/{scan}", s.handleResult)
	s.mux.HandleFunc("GET /api/v1/results/{target}/{mode}/{scan}/har", s.handleResultHAR)
	s.mux.HandleFunc("GET /api/v1/artifacts/{ref...}", s.handleArtifact)
	s.mux.HandleFunc("GET /api/v1/results/{target}/{mode}/{scan}/csv", s.handleResultCSV)
	s.mux.HandleFunc("GET /api/v1/results/{target}/{mode}/{scan}/report", s.handleResultMarkdown)
	s.mux.HandleFunc("GET /api/v1/diff/{target}/{mode}/{scan}", s.handleDiff)
	s.mux.HandleFunc("GET /api/v1/baseline/{target}/{mode}", s.handleGetBaseline)
	s.mux.HandleFunc("GET /api/v1/audit", s.handleAudit)
	s.mux.HandleFunc("GET /api/v1/schedule", s.handleSchedule)

	s.mux.HandleFunc("POST /api/v1/baseline/{target}/{mode}", s.handleSetBaseline)
	s.mux.HandleFunc("DELETE /api/v1/baseline/{target}/{mode}", s.handleDeleteBaseline)
	s.mux.HandleFunc("POST /api/v1/scan/{target}/{mode}", s.handleTriggerScan)

	s.shareRoutes()

	if s.opts.MetricsEnabled && s.deps.Metrics != nil {
		s.mux.HandleFunc("GET "+s.opts.MetricsPath, s.handleMetrics)
	}

	if s.opts.WebUI {
		s.uiRoutes()
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "alive",
		"version": s.opts.Version,
	})
}

// handleReady distinguishes "the process is alive" from "wsaw can actually
// scan", which are very different things to an operator (Story 6.5).
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// The store is checked first and regardless of what else is tracked. With
	// a server database it is a network dependency that can be down while
	// everything else is fine, and a wsaw that cannot record what it observes
	// is not ready however healthy its browser is (Story 4.7).
	storeCtx, cancel := context.WithTimeout(r.Context(), storeProbeTimeout)
	defer cancel()

	if err := s.deps.Store.Ping(storeCtx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			fieldReady:  false,
			fieldReason: err.Error(),
		})

		return
	}

	if s.deps.Metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{fieldReady: true, fieldReason: "readiness is not tracked"})

		return
	}

	ready, reason := s.deps.Metrics.Ready()

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}

	stale := s.deps.Metrics.Stale(time.Now(), s.opts.StaleAfter)

	writeJSON(w, status, map[string]any{
		fieldReady:     ready,
		fieldReason:    reason,
		"staleTargets": stale,
	})
}

// TargetView is a target plus its current state, which is what the dashboard
// and any external monitor both need.
type TargetView struct {
	Name         string              `json:"name"`
	URL          string              `json:"url"`
	Labels       map[string]string   `json:"labels,omitempty"`
	ConsentModes []model.ConsentMode `json:"consentModes"`
	Series       []SeriesView        `json:"series"`
}

// SeriesView is the latest state of one target-and-mode stream.
type SeriesView struct {
	Mode model.ConsentMode `json:"consentMode"`

	LastScan    *store.Summary `json:"lastScan,omitempty"`
	HasBaseline bool           `json:"hasBaseline"`

	// Running lists the scans of this series that are in flight. A series with
	// a running scan is active, which is a different state from stale even
	// when its last stored scan is old (Story 5.12).
	Running []scanner.Running `json:"running,omitempty"`

	// Stale marks a series whose last successful scan is older than the
	// configured threshold, or which has never succeeded.
	Stale bool `json:"stale"`
	// StaleReason explains it in operator-readable terms.
	StaleReason string `json:"staleReason,omitempty"`
}

// running returns the in-flight scans, or nil when activity is not tracked.
func (s *Server) running() []scanner.Running {
	if s.deps.Running == nil {
		return nil
	}

	return s.deps.Running()
}

// runningFor filters the in-flight scans down to one series.
func runningFor(all []scanner.Running, target string, mode model.ConsentMode) []scanner.Running {
	var out []scanner.Running

	for _, r := range all {
		if r.Target == target && r.ConsentMode == mode {
			out = append(out, r)
		}
	}

	return out
}

func (s *Server) targetViews() []TargetView {
	var out []TargetView

	if s.deps.Targets == nil {
		return out
	}

	now := time.Now()
	live := s.running()

	for _, t := range s.deps.Targets() {
		view := TargetView{
			Name:         t.Name,
			URL:          t.URL,
			Labels:       t.Labels,
			ConsentModes: t.ConsentModes,
		}

		for _, mode := range t.ConsentModes {
			sv := SeriesView{Mode: mode}

			if summaries, err := s.deps.Store.ListResults(t.Name, mode, 1); err == nil && len(summaries) > 0 {
				sv.LastScan = &summaries[0]
			}

			if _, err := s.deps.Store.GetBaseline(t.Name, mode); err == nil {
				sv.HasBaseline = true
			}

			sv.Running = runningFor(live, t.Name, mode)
			sv.Stale, sv.StaleReason = staleness(sv.LastScan, now, s.opts.StaleAfter)

			view.Series = append(view.Series, sv)
		}

		out = append(out, view)
	}

	return out
}

// staleness decides whether a series should be flagged. Never-scanned and
// last-scan-failed both count: an empty result set must never read as a clean
// site (Tenet 5).
func staleness(last *store.Summary, now time.Time, maxAge time.Duration) (bool, string) {
	switch {
	case last == nil:
		return true, "never scanned"

	case last.Termination == model.TermError:
		return true, "last scan failed: " + last.Error

	case last.Termination == model.TermSkipped:
		return true, "last scan was skipped: " + last.Error

	case maxAge > 0 && now.Sub(last.StartedAt) > maxAge:
		return true, "last scan is older than " + maxAge.String()

	default:
		return false, ""
	}
}

func (s *Server) handleTargets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"targets": s.targetViews()})
}

// handleRunning reports the scans in flight. "tracked" distinguishes a daemon
// that has nothing running from a deployment where activity is not observable
// at all — reporting the second as "nothing running" would be the quiet lie
// Tenet 5 forbids.
func (s *Server) handleRunning(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Running == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"tracked":    false,
			fieldReason:  "this wsaw instance does not run scans",
			fieldRunning: []scanner.Running{},
		})

		return
	}

	live := s.deps.Running()
	if live == nil {
		live = []scanner.Running{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tracked":    true,
		fieldRunning: live,
		"count":      len(live),
	})
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	summaries, err := s.deps.Store.ListResults(target, mode, limit)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": summaries})
}

func (s *Server) loadResult(w http.ResponseWriter, r *http.Request) (*model.Result, bool) {
	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return nil, false
	}

	scan := r.PathValue("scan")

	var (
		res *model.Result
		err error
	)

	if scan == "latest" {
		res, err = s.deps.Store.LatestResult(target, mode)
	} else {
		res, err = s.deps.Store.GetResult(target, mode, scan)
	}

	if err != nil {
		writeStoreError(w, err)

		return nil, false
	}

	return res, true
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResult(w, r)
	if !ok {
		return
	}

	// Stored bodies are inlined unless the caller says otherwise. Keeping
	// them was already an explicit opt-in per target, so a client that asked
	// for bodies to be kept should not have to ask again to see them — but a
	// client polling this endpoint for metadata can turn them off, because a
	// page's worth of scripts is a lot of JSON.
	writeJSON(w, http.StatusOK, report.WithBodies(res, s.bodyLoader(r)))
}

// bodyLoader resolves stored bodies for an export, unless the request opted
// out with bodies=false.
func (s *Server) bodyLoader(r *http.Request) report.BodyLoader {
	if v := r.URL.Query().Get("bodies"); v == "false" || v == "0" || v == "no" {
		return nil
	}

	return func(ref string) ([]byte, error) {
		return s.deps.Store.GetArtifact(ref)
	}
}

// handleArtifact serves a stored body or screenshot.
//
// Without this route a stored artifact was unreachable: capture wrote it, the
// result named it, and nothing could read it back. The reference is validated
// by the store, which confines it to the artifact directory (Tenet 9).
//
// How it comes back depends on what it is, and the distinction is the whole
// of Story 5.17's AC5. A response body *is* the scanned page's bytes, so it
// is served as an opaque attachment and never as anything a browser will
// render — rendering it on wsaw's own origin would hand a hostile page a
// same-origin script context. A screenshot is a PNG that Chrome produced
// under wsaw's control: the page influenced its pixels, not its bytes, so it
// may be shown as an image. As an image and nothing else.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if ref == "" {
		writeJSONError(w, http.StatusBadRequest, "an artifact reference is required")

		return
	}

	s.serveArtifact(w, r, ref)
}

// serveArtifact writes one stored artifact with the headers its kind
// deserves. It is shared by the authenticated route and by the share-link
// route, so a shared reader cannot be served bytes under weaker headers than
// an operator would get.
//
// The object is streamed rather than buffered. Evidence now lives in a bucket
// and a result document runs to tens of megabytes, so reading it whole before
// the first byte reaches the client would make the daemon's memory track the
// size of the largest artifact times the number of readers (Story 8.1, AC7).
func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, ref string) {
	body, size, err := s.deps.Store.OpenArtifact(ref)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	defer func() {
		if err := body.Close(); err != nil {
			s.deps.Logger.Error("closing an artifact reader", "error", err)
		}
	}()

	// Only the magic bytes are pulled ahead of the copy. A screenshot is served
	// as an image and a body never is, and that decision has to be made before
	// the headers go out — but it needs eight bytes, not the whole object.
	head := bufio.NewReaderSize(body, len(pngMagic))

	magic, err := head.Peek(len(pngMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		writeStoreError(w, err)

		return
	}

	// Two independent conditions, deliberately. The kind comes from wsaw's own
	// code rather than from a page, and the magic bytes are the file itself:
	// requiring both means a response body cannot be served as an image even
	// if some future caller passed a reference that claimed to be one.
	inline := !wantsDownload(r) && isScreenshotRef(ref) && isPNG(magic)

	// Written from what the bucket reported rather than from what is copied,
	// so the interface can show a size and a browser can cache instead of
	// re-fetching megabytes (Story 5.17, AC4).
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")

	if inline {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `inline; filename="`+safeArtifactFilename(ref, "png")+`"`)
		// An image and nothing else: no script, no styles, no subresources,
		// whatever a browser might otherwise try to do with these bytes.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; sandbox")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeArtifactFilename(ref, "bin")+`"`)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}

	// #nosec G705 -- a body's bytes are page-controlled, which is why the
	// headers above make them inert: an opaque type, an attachment
	// disposition, nosniff, and a sandbox policy. A screenshot is wsaw's own
	// PNG and is served as an image with an equally strict policy. The
	// analyser sees the taint and not the mitigation; the tests in
	// bodies_test.go and screenshots_test.go see the mitigation.
	if _, err := io.Copy(w, head); err != nil {
		// The status and the headers are already gone, so there is nothing to
		// report to the client; the log line is what tells an operator that a
		// download died half way rather than completing.
		s.deps.Logger.Error("writing artifact", "error", err)
	}
}

// wantsDownload reports whether the caller asked for the file rather than a
// rendering of it, so a screenshot can still be saved as evidence.
func wantsDownload(r *http.Request) bool {
	switch r.URL.Query().Get("download") {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// isScreenshotRef reports whether a reference names a screenshot. The kind is
// the first path segment and is written by wsaw, never by a scanned page.
func isScreenshotRef(ref string) bool {
	kind, _, ok := strings.Cut(ref, "/")

	return ok && strings.HasPrefix(kind, "screenshot")
}

// pngMagic is the signature every PNG starts with, and the only part of an
// artifact serveArtifact has to look at before it chooses headers.
const pngMagic = "\x89PNG\r\n\x1a\n"

// isPNG checks the file's own magic bytes, so what is served as an image is
// an image regardless of what its reference claimed.
func isPNG(data []byte) bool {
	return bytes.HasPrefix(data, []byte(pngMagic))
}

// safeArtifactFilename builds a download name from a reference. References
// are content-addressed — a kind and a hex digest — but the value still
// arrives from the request, so it is reduced to characters that cannot break
// the header.
func safeArtifactFilename(ref, ext string) string {
	out := make([]rune, 0, len(ref))

	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}

	return "wsaw-" + string(out) + "." + ext
}

func (s *Server) handleResultHAR(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResult(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(res, "har")+`"`)

	if err := report.WriteHAR(w, res, s.bodyLoader(r)); err != nil {
		s.deps.Logger.Error("writing HAR", "error", err)
	}
}

func (s *Server) handleResultCSV(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResult(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(res, "csv")+`"`)

	if err := report.WriteCSV(w, res); err != nil {
		s.deps.Logger.Error("writing CSV", "error", err)
	}
}

func (s *Server) handleResultMarkdown(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResult(w, r)
	if !ok {
		return
	}

	rep := s.diffFor(res)

	// text/plain, not text/markdown: the content embeds page-controlled text,
	// and nosniff plus a non-renderable type keeps a browser from doing
	// anything with it.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if err := report.WriteMarkdown(w, res, rep); err != nil {
		s.deps.Logger.Error("writing Markdown report", "error", err)
	}
}

// diffFor compares a result against its baseline, falling back to the
// previous scan.
//
// The one failure it does not shrug off is a previous result whose document
// has left the bucket. Treating that as "there is nothing earlier" would render
// the page a first-ever scan renders, and a reader would have no way to know
// that a comparison was owed and could not be made (Story 8.2, AC5; Tenet 5).
func (s *Server) diffFor(res *model.Result) *diff.Report {
	var (
		baseline *model.Result
		gone     bool
	)

	if b, err := s.deps.Store.GetBaseline(res.Target, res.ConsentMode); err == nil {
		baseline = b.Result
	} else {
		prev, err := s.deps.Store.PreviousResult(res.Target, res.ConsentMode, res.ScanID)

		switch {
		case err == nil:
			baseline = prev

		case errors.Is(err, store.ErrEvidenceGone):
			gone = true

			s.deps.Logger.Error("the result this one should be compared against names evidence the artifact bucket no longer holds",
				"target", res.Target, "consent_mode", string(res.ConsentMode),
				"scan_id", res.ScanID, "error", err)
		}
	}

	rep := diff.Compare(baseline, res, diff.Options{})

	if gone {
		rep.Reason = diff.ReasonEvidenceGone
	}

	return rep
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResult(w, r)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, s.diffFor(res))
}

func (s *Server) handleGetBaseline(w http.ResponseWriter, r *http.Request) {
	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return
	}

	b, err := s.deps.Store.GetBaseline(target, mode)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleSetBaseline(w http.ResponseWriter, r *http.Request) {
	if allowed, reason := s.writeAllowed(); !allowed {
		writeJSONError(w, http.StatusForbidden, reason)

		return
	}

	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return
	}

	var body struct {
		ScanID string `json:"scanId"`
		Actor  string `json:"actor"`
		Note   string `json:"note"`
	}

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())

		return
	}

	if body.ScanID == "" {
		writeJSONError(w, http.StatusBadRequest, "scanId is required")

		return
	}

	b, err := s.deps.Store.SetBaseline(target, mode, body.ScanID, body.Actor, body.Note)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())

			return
		}

		writeJSONError(w, http.StatusBadRequest, err.Error())

		return
	}

	s.deps.Logger.Info("baseline approved",
		"target", target, "consent_mode", string(mode), "scan_id", body.ScanID, "actor", body.Actor)

	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleDeleteBaseline(w http.ResponseWriter, r *http.Request) {
	if allowed, reason := s.writeAllowed(); !allowed {
		writeJSONError(w, http.StatusForbidden, reason)

		return
	}

	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return
	}

	if err := s.deps.Store.DeleteBaseline(target, mode, r.URL.Query().Get("actor")); err != nil {
		writeStoreError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleTriggerScan(w http.ResponseWriter, r *http.Request) {
	if allowed, reason := s.writeAllowed(); !allowed {
		writeJSONError(w, http.StatusForbidden, reason)

		return
	}

	if !s.opts.AllowAdHocScan || s.deps.Trigger == nil {
		writeJSONError(w, http.StatusForbidden, "ad-hoc scanning is disabled")

		return
	}

	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return
	}

	// Only configured targets may be scanned. Accepting an arbitrary URL here
	// would turn the API into a request-forgery primitive.
	if !s.isConfiguredTarget(target, mode) {
		writeJSONError(w, http.StatusNotFound, "no such target and consent mode is configured")

		return
	}

	// Two concurrent scans of the same target and consent mode would produce
	// two results for the same moment and double the load wsaw puts on
	// somebody else's site (Tenet 17). Refusing is more useful than queueing,
	// because the caller learns that the work is already under way.
	if live := runningFor(s.running(), target, mode); len(live) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "a scan of this target and consent mode is already running",
			fieldRunning: live,
		})

		return
	}

	out, err := s.deps.Trigger.Trigger(r.Context(), target, mode)
	if err != nil {
		// A scan that failed still produced a result, which is the useful
		// thing to return.
		s.deps.Logger.Warn("ad-hoc scan reported an error", "target", target, "error", err)
	}

	if out.Result == nil {
		writeJSONError(w, http.StatusInternalServerError, "the scan produced no result")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"result": out.Result, "diff": out.Diff})
}

func (s *Server) isConfiguredTarget(name string, mode model.ConsentMode) bool {
	if s.deps.Targets == nil {
		return false
	}

	for _, t := range s.deps.Targets() {
		if t.Name != name {
			continue
		}

		for _, m := range t.ConsentModes {
			if m == mode {
				return true
			}
		}
	}

	return false
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	entries, err := s.deps.Store.Audit(limit)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleSchedule(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Daemon == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": []any{}})

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.deps.Daemon.Jobs()})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	if err := s.deps.Metrics.WritePrometheus(w); err != nil {
		s.deps.Logger.Error("writing metrics", "error", err)
	}
}

// pathTargetMode extracts and validates the target and consent mode.
func pathTargetMode(w http.ResponseWriter, r *http.Request) (string, model.ConsentMode, bool) {
	target := r.PathValue("target")
	mode := model.ConsentMode(r.PathValue("mode"))

	if target == "" {
		writeJSONError(w, http.StatusBadRequest, "target is required")

		return "", "", false
	}

	if !mode.Valid() {
		writeJSONError(w, http.StatusBadRequest,
			"consent mode must be one of none, reject, accept")

		return "", "", false
	}

	return target, mode, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())

	case errors.Is(err, store.ErrEvidenceGone):
		// 410 rather than 404 or 500. The scan is in the index, so "there is no
		// such result" would be untrue; wsaw is working, so a server error
		// would be untrue as well. What happened is that the evidence this
		// result names has been deleted, and Gone is the answer that says so
		// (Story 8.2, AC5).
		writeJSONError(w, http.StatusGone, err.Error())

	default:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
}

// safeFilename builds a download filename from data that ultimately comes
// from configuration, keeping it to characters that cannot break the
// Content-Disposition header.
func safeFilename(res *model.Result, ext string) string {
	clean := func(s string) string {
		out := make([]rune, 0, len(s))

		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
				out = append(out, r)
			default:
				out = append(out, '-')
			}
		}

		return string(out)
	}

	return clean(res.Target) + "-" + clean(string(res.ConsentMode)) + "-" + clean(res.ScanID) + "." + ext
}
