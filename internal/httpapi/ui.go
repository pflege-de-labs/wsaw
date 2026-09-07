package httpapi

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

//go:embed templates/*.html static/*
var uiFiles embed.FS

// uiRenderer holds the parsed templates. They are embedded and parsed at
// startup, so the binary needs no asset directory at runtime and a broken
// template fails immediately rather than on the first page view (Story 5.7).
type uiRenderer struct {
	tmpl *template.Template
}

func newUIRenderer() (*uiRenderer, error) {
	tmpl, err := template.New("").Funcs(uiFuncs()).ParseFS(uiFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing web interface templates: %w", err)
	}

	return &uiRenderer{tmpl: tmpl}, nil
}

// uiFuncs are display helpers only. None of them produce raw HTML: every
// value rendered originates from a scanned page, so html/template's
// contextual escaping must stay in force (Story 5.11).
func uiFuncs() template.FuncMap {
	return template.FuncMap{
		"time": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}

			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}

			d := time.Since(t)

			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"dur": func(d time.Duration) string {
			return d.Round(time.Millisecond).String()
		},
		// since is for a scan that has not finished: it has no duration yet,
		// only an elapsed time, and conflating the two would let a running
		// scan look like a completed one.
		"since": func(t time.Time) string {
			return time.Since(t).Round(time.Second).String()
		},
		"bytes": func(n int64) string {
			const unit = 1024

			if n < unit {
				return fmt.Sprintf("%d B", n)
			}

			div, exp := int64(unit), 0

			for m := n / unit; m >= unit; m /= unit {
				div *= unit
				exp++
			}

			return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
		},
		"sevclass": func(s diff.Severity) string {
			return "sev-" + string(s)
		},
		"add": func(a, b int) int { return a + b },
	}
}

func (s *Server) uiRoutes() {
	s.mux.Handle("GET /static/", http.FileServerFS(uiFiles))

	s.mux.HandleFunc("GET /", s.handleUIDashboard)
	s.mux.HandleFunc("GET /login", s.handleUILogin)
	s.mux.HandleFunc("POST /login", s.handleUILoginSubmit)
	s.mux.HandleFunc("GET /targets/{target}/{mode}", s.handleUISeries)
	s.mux.HandleFunc("GET /results/{target}/{mode}/{scan}", s.handleUIResult)
	s.mux.HandleFunc("GET /compare/{target}", s.handleUICompare)
	s.mux.HandleFunc("GET /audit", s.handleUIAudit)

	s.mux.HandleFunc("POST /refresh", s.handleUIRefresh)

	s.mux.HandleFunc("POST /approve/{target}/{mode}", s.handleUIApprove)
	s.mux.HandleFunc("POST /rescan/{target}/{mode}", s.handleUIRescan)
}

// page is the data every template receives.
type page struct {
	Title      string
	Version    string
	ReadOnly   bool
	AllowScan  bool
	ConfigPath string
	// ShareEnabled offers the button that mints a link to one result
	// (Story 5.19). Off unless sharing is configured, so the interface never
	// shows an action that would fail.
	ShareEnabled bool
	CSRF         string
	Flash        string
	FlashError   string

	// Refresh is what this page does about reloading itself, and why
	// (Story 5.16).
	Refresh refreshSetting
	// RefreshChoices are the intervals the control offers.
	RefreshChoices []refreshChoice
	// RefreshTarget is the URL a refresh loads: this page without its flash
	// parameters, so a reload does not re-show a message about something that
	// happened once.
	RefreshTarget string
	// RenderedAt is when this page was built. It is shown, because a page
	// that reloads silently invites the reader to trust whatever is on it
	// (Story 5.16, AC5).
	RenderedAt time.Time

	Data any
}

// refreshChoice is one option in the interval control.
type refreshChoice struct {
	Label    string
	Seconds  int
	Selected bool
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	s.renderWith(w, r, name, title, data, 0, "")
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, name, title string, data any, running int) {
	s.renderWith(w, r, name, title, data, running, "")
}

// renderStill renders a page that must not reload itself, whatever interval
// is in force for the interface (Story 5.18).
//
// The reason is not decoration: this page behaves differently from the one
// the reader came from, so it says why rather than leaving somebody who set
// thirty seconds on the dashboard wondering whether refreshing is broken
// (AC2).
func (s *Server) renderStill(w http.ResponseWriter, r *http.Request, name, title string, data any, reason string) {
	s.renderWith(w, r, name, title, data, 0, reason)
}

func (s *Server) renderWith(
	w http.ResponseWriter, r *http.Request, name, title string, data any, running int, hold string,
) {
	p := page{
		Title:        title,
		Version:      s.opts.Version,
		ReadOnly:     s.opts.ReadOnly,
		AllowScan:    s.opts.AllowAdHocScan && !s.opts.ReadOnly,
		ConfigPath:   s.deps.ConfigPath,
		ShareEnabled: s.sharingEnabled(),
		CSRF:         s.csrfToken(),
		Flash:        r.URL.Query().Get("ok"),
		FlashError:   r.URL.Query().Get("err"),
		Data:         data,
	}

	p.Refresh = s.refreshFor(r, running)

	// A page that holds still does so whatever the cookie, the URL parameter
	// or the deployment default says: nothing on it can change, so there is
	// nothing for a refresh to fetch (Story 5.18, AC1 and AC4).
	if hold != "" {
		p.Refresh = p.Refresh.hold(hold)
	}

	// The control is built from the viewer's choice rather than from the
	// interval in force, so it still shows the interface's setting on a page
	// that holds still.
	p.RefreshChoices = refreshChoicesFor(p.Refresh)
	p.RefreshTarget = refreshTarget(r)
	p.RenderedAt = time.Now()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := s.ui.tmpl.ExecuteTemplate(w, name, p); err != nil {
		s.deps.Logger.Error("rendering page", "template", name, "error", err)
	}
}

// refreshChoicesFor builds the control's options, marking the one in force.
// A viewer's own interval that is not in the list is added, so a URL-supplied
// value is visible in the control rather than silently absent from it.
func refreshChoicesFor(current refreshSetting) []refreshChoice {
	intervals := append([]time.Duration(nil), refreshChoices...)

	if current.HasChosen && !containsInterval(intervals, current.Chosen) {
		intervals = append(intervals, current.Chosen)
		sortIntervals(intervals)
	}

	out := make([]refreshChoice, 0, len(intervals))

	for _, d := range intervals {
		out = append(out, refreshChoice{
			Label:    humaniseInterval(d),
			Seconds:  int(d.Seconds()),
			Selected: current.HasChosen && d == current.Chosen,
		})
	}

	return out
}

func containsInterval(list []time.Duration, want time.Duration) bool {
	for _, d := range list {
		if d == want {
			return true
		}
	}

	return false
}

func sortIntervals(list []time.Duration) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// dashboardData is the target overview.
type dashboardData struct {
	Targets []TargetView
	Stale   int
	Ready   bool
	Reason  string
	Jobs    []scheduleRow

	// Running is every scan in flight, across all targets, so the dashboard
	// answers "is wsaw doing anything right now" without drilling in
	// (Story 5.12).
	Running []scanner.Running
	// Tracked is false where this instance does not run scans at all, which
	// must read differently from "nothing is running".
	Tracked bool
}

type scheduleRow struct {
	Target  string
	Mode    model.ConsentMode
	NextRun time.Time
	LastRun time.Time
}

func (s *Server) handleUIDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)

		return
	}

	data := dashboardData{
		Targets: s.targetViews(),
		Ready:   true,
		Reason:  "readiness is not tracked",
		Running: s.running(),
		Tracked: s.deps.Running != nil,
	}

	for _, t := range data.Targets {
		for _, series := range t.Series {
			if series.Stale {
				data.Stale++
			}
		}
	}

	if s.deps.Metrics != nil {
		data.Ready, data.Reason = s.deps.Metrics.Ready()
	}

	if s.deps.Daemon != nil {
		for _, j := range s.deps.Daemon.Jobs() {
			data.Jobs = append(data.Jobs, scheduleRow{
				Target: j.Target, Mode: j.Mode, NextRun: j.NextRun, LastRun: j.LastRun,
			})
		}
	}

	s.renderPage(w, r, "dashboard.html", "wsaw", data, len(data.Running))
}

type seriesData struct {
	Target   string
	Mode     model.ConsentMode
	Results  []store.Summary
	Baseline *store.Baseline

	// Running is shown above the history as a pending row, because the scan
	// that is happening right now is the one an operator is usually looking
	// for and it is in no other view (Story 5.12, AC1).
	Running []scanner.Running
}

func (s *Server) handleUISeries(w http.ResponseWriter, r *http.Request) {
	target, mode, ok := uiTargetMode(w, r)
	if !ok {
		return
	}

	results, err := s.deps.Store.ListResults(target, mode, 100)
	if err != nil {
		s.uiError(w, r, http.StatusInternalServerError, err.Error())

		return
	}

	data := seriesData{
		Target:  target,
		Mode:    mode,
		Results: results,
		Running: runningFor(s.running(), target, mode),
	}

	if b, err := s.deps.Store.GetBaseline(target, mode); err == nil {
		data.Baseline = b
	}

	s.renderPage(w, r, "series.html", target+" — "+string(mode), data, len(data.Running))
}

type resultData struct {
	Result *model.Result
	Diff   *diff.Report
	Hosts  []model.HostSummary
	Counts map[string]int

	// Screenshots are the scan's evidence images, before and after the
	// consent interaction (Story 5.17).
	Screenshots []screenshotView

	// Filter values are echoed back so the form keeps its state.
	FilterHost  string
	FilterType  string
	FilterParty string
	FilterPhase string

	Requests []model.Request
	// Truncated reports that the request table was cut short for rendering,
	// which must be visible rather than silent.
	Truncated bool
	Total     int
}

// screenshotView is one captured screenshot as the page presents it.
type screenshotView struct {
	// Label says what moment the image is of. The pair is the point: what
	// the banner looked like, and what wsaw's interaction did to it.
	Label string
	Kind  string
	Ref   string

	SHA256 string
	Bytes  int64

	// Missing marks evidence whose file is no longer there — pruned, or on a
	// volume that went away. Expired evidence must not look like evidence
	// that never existed (Story 5.17, AC3).
	Missing bool
	Reason  string
}

// screenshotViews orders and annotates a scan's screenshots.
//
// Before-consent comes first and after-consent second, whatever order they
// were recorded in, because reading them the other way round inverts what
// they say.
func (s *Server) screenshotViews(res *model.Result) []screenshotView {
	labels := map[string]string{
		"screenshot-before-consent": "Before the consent interaction",
		"screenshot-after-consent":  "After the consent interaction",
	}

	order := map[string]int{
		"screenshot-before-consent": 0,
		"screenshot-after-consent":  1,
	}

	out := make([]screenshotView, 0, len(res.Screenshots))

	for _, a := range res.Screenshots {
		view := screenshotView{
			Label:  labels[a.Kind],
			Kind:   a.Kind,
			Ref:    a.Ref,
			SHA256: a.SHA256,
			Bytes:  a.Bytes,
		}

		if view.Label == "" {
			view.Label = a.Kind
		}

		// Checked by size rather than by reading the file: a result page must
		// not load megabytes of PNG just to find out whether it can link to
		// them.
		if size, err := s.deps.Store.StatArtifact(a.Ref); err != nil {
			view.Missing = true
			view.Reason = "the stored image is no longer available: " + err.Error()
		} else if size > 0 {
			view.Bytes = size
		}

		out = append(out, view)
	}

	sort.SliceStable(out, func(i, j int) bool {
		oi, iok := order[out[i].Kind]
		oj, jok := order[out[j].Kind]

		switch {
		case iok && jok:
			return oi < oj
		case iok:
			return true
		case jok:
			return false
		default:
			return out[i].Kind < out[j].Kind
		}
	})

	return out
}

// maxRenderedRequests bounds the request table. A page with tens of thousands
// of requests would otherwise produce an unusable HTML document; the cap is
// disclosed and the full data stays available as JSON.
const maxRenderedRequests = 2000

// holdReasonFinishedScan is what the scan detail page says about not
// refreshing. It names the cause — the scan is over — rather than reporting
// the setting, because the reader's question is why this page differs from
// the one they came from (Story 5.18, AC2).
const holdReasonFinishedScan = "not refreshing: this scan is finished and cannot change"

func (s *Server) handleUIResult(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResultUI(w, r)
	if !ok {
		return
	}

	data := resultData{
		Result:      res,
		Screenshots: s.screenshotViews(res),
		Diff:        s.diffFor(res),
		Hosts:       res.HostSummaries(),
		Counts:      res.CountsByResourceType(),
		FilterHost:  r.URL.Query().Get("host"),
		FilterType:  r.URL.Query().Get("type"),
		FilterParty: r.URL.Query().Get("party"),
		FilterPhase: r.URL.Query().Get("phase"),
	}

	for i := range res.Requests {
		req := &res.Requests[i]
		if !matchesFilter(req, data) {
			continue
		}

		data.Total++

		if len(data.Requests) < maxRenderedRequests {
			data.Requests = append(data.Requests, *req)
		} else {
			data.Truncated = true
		}
	}

	// This page holds still. One finished scan is an immutable record, so a
	// refresh re-renders identical content — and it would do it on precisely
	// the pages worth reading closely, throwing away the scroll position, a
	// filter just typed, and the screenshot being looked at (Story 5.18).
	s.renderStill(w, r, "result.html", res.Target+" — "+res.ScanID, data, holdReasonFinishedScan)
}

func matchesFilter(req *model.Request, d resultData) bool {
	if d.FilterHost != "" && !strings.Contains(req.Host, d.FilterHost) && !strings.Contains(req.Domain, d.FilterHost) {
		return false
	}

	if d.FilterType != "" && req.ResourceType != d.FilterType {
		return false
	}

	if d.FilterParty != "" && string(req.Party) != d.FilterParty {
		return false
	}

	if d.FilterPhase != "" && string(req.Phase) != d.FilterPhase {
		return false
	}

	return true
}

// compareData puts the consent modes side by side, which is what makes
// "fired despite reject" visually obvious (Story 5.9).
type compareData struct {
	Target string
	Modes  []compareColumn
	// Domains is the union of third-party domains across modes, sorted.
	Domains []compareRow
}

type compareColumn struct {
	Mode    model.ConsentMode
	Result  *model.Result
	Missing bool
}

type compareRow struct {
	Domain string
	// Present has one entry per column: "" absent, "pre" or "post".
	Present []string
}

func (s *Server) handleUICompare(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")

	data := compareData{Target: target}

	seen := make(map[string][]string)
	modes := []model.ConsentMode{model.ConsentNone, model.ConsentReject, model.ConsentAccept}

	for i, mode := range modes {
		col := compareColumn{Mode: mode}

		res, err := s.deps.Store.LatestResult(target, mode)
		if err != nil {
			col.Missing = true
			data.Modes = append(data.Modes, col)

			continue
		}

		col.Result = res
		data.Modes = append(data.Modes, col)

		for j := range res.Requests {
			req := &res.Requests[j]
			if req.Party != model.ThirdParty || req.NonNetwork || req.Domain == "" {
				continue
			}

			row, ok := seen[req.Domain]
			if !ok {
				row = make([]string, len(modes))
				seen[req.Domain] = row
			}

			// Pre-consent wins over post: it is the stronger finding.
			if row[i] != "pre" {
				if req.Phase == model.PhasePre {
					row[i] = "pre"
				} else {
					row[i] = "post"
				}
			}
		}
	}

	for _, domain := range sortedDomains(seen) {
		data.Domains = append(data.Domains, compareRow{Domain: domain, Present: seen[domain]})
	}

	s.render(w, r, "compare.html", target+" — consent modes", data)
}

func sortedDomains(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}

	return out
}

func (s *Server) handleUIAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.deps.Store.Audit(200)
	if err != nil {
		s.uiError(w, r, http.StatusInternalServerError, err.Error())

		return
	}

	s.render(w, r, "audit.html", "Audit log", entries)
}

func (s *Server) handleUIApprove(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}

	if allowed, reason := s.writeAllowed(); !allowed {
		s.uiRedirectError(w, r, "/", reason)

		return
	}

	target, mode, ok := uiTargetMode(w, r)
	if !ok {
		return
	}

	scanID := r.FormValue("scanId")
	actor := r.FormValue("actor")

	if _, err := s.deps.Store.SetBaseline(target, mode, scanID, actor, r.FormValue("note")); err != nil {
		s.uiRedirectError(w, r, "/targets/"+target+"/"+string(mode), err.Error())

		return
	}

	s.deps.Logger.Info("baseline approved via web interface",
		"target", target, "consent_mode", string(mode), "scan_id", scanID, "actor", actor)

	s.uiRedirectOK(w, r, "/targets/"+target+"/"+string(mode),
		"Baseline approved. It is stored in wsaw's database and recorded in the audit log.")
}

func (s *Server) handleUIRescan(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}

	if allowed, reason := s.writeAllowed(); !allowed {
		s.uiRedirectError(w, r, "/", reason)

		return
	}

	if !s.opts.AllowAdHocScan || s.deps.Trigger == nil {
		s.uiRedirectError(w, r, "/", "ad-hoc scanning is disabled")

		return
	}

	target, mode, ok := uiTargetMode(w, r)
	if !ok {
		return
	}

	if !s.isConfiguredTarget(target, mode) {
		s.uiRedirectError(w, r, "/", "no such target and consent mode is configured")

		return
	}

	dest := "/targets/" + target + "/" + string(mode)

	if len(runningFor(s.running(), target, mode)) > 0 {
		s.uiRedirectError(w, r, dest,
			"A scan of this target and consent mode is already running. Its progress is shown below.")

		return
	}

	// The browser is not held open for the length of a scan. A scan can take
	// the better part of a minute, and the point of the pending row is that
	// activity is visible while it happens rather than only once it is over
	// (Story 5.12). The API's POST /scan stays synchronous: an automated
	// client wants the result, a person wants the page back.
	//
	// Detached from the request context, which is cancelled the moment this
	// redirect is written, and bounded so a wedged scan cannot outlive the
	// budget any other scan gets.
	scanCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), adHocScanBudget)

	go func() {
		defer cancel()

		if _, err := s.deps.Trigger.Trigger(scanCtx, target, mode); err != nil {
			s.deps.Logger.Warn("ad-hoc scan reported an error",
				"target", target, "consent_mode", string(mode), "error", err)
		}
	}()

	s.uiRedirectOK(w, r, dest,
		"Scan started. It appears as pending below and this page refreshes until it finishes.")
}

// adHocScanBudget bounds a scan started from the web interface. It is generous
// because a target's own hard timeout is the real limit; this only stops a
// detached goroutine from living forever if that limit somehow does not fire.
const adHocScanBudget = 15 * time.Minute

func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Token.IsSet() {
		http.Redirect(w, r, "/", http.StatusSeeOther)

		return
	}

	s.render(w, r, "login.html", "Sign in", nil)
}

func (s *Server) handleUILoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Token.IsSet() {
		http.Redirect(w, r, "/", http.StatusSeeOther)

		return
	}

	if err := r.ParseForm(); err != nil {
		s.uiRedirectError(w, r, "/login", "invalid form submission")

		return
	}

	if r.FormValue("token") != s.opts.Token.Reveal() {
		// No detail about why: a login form should not help enumerate.
		s.uiRedirectError(w, r, "/login", "invalid token")

		return
	}

	// Secure is set only when wsaw is serving TLS. The default listener is
	// loopback over plain HTTP, where a Secure cookie would simply never be
	// sent and the session would appear broken; HttpOnly and SameSite=Strict
	// carry the protection there.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above: Secure follows TLS
		Name:     sessionCookie,
		Value:    s.opts.Token.Reveal(),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.opts.TLSCert != "",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((12 * time.Hour).Seconds()),
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) loadResultUI(w http.ResponseWriter, r *http.Request) (*model.Result, bool) {
	target, mode, ok := uiTargetMode(w, r)
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
		s.uiError(w, r, http.StatusNotFound, err.Error())

		return nil, false
	}

	return res, true
}

func uiTargetMode(w http.ResponseWriter, r *http.Request) (string, model.ConsentMode, bool) {
	target := r.PathValue("target")
	mode := model.ConsentMode(r.PathValue("mode"))

	if target == "" || !mode.Valid() {
		http.Error(w, "unknown target or consent mode", http.StatusBadRequest)

		return "", "", false
	}

	return target, mode, true
}

func (s *Server) uiError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.WriteHeader(status)
	s.render(w, r, "error.html", "Error", msg)
}

func (s *Server) uiRedirectOK(w http.ResponseWriter, r *http.Request, dest, msg string) {
	// #nosec G710 -- safeLocal guarantees a path on this origin; the taint
	// analyser cannot follow the sanitiser, but the test below it can.
	http.Redirect(w, r, safeLocal(dest)+"?ok="+urlQueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) uiRedirectError(w http.ResponseWriter, r *http.Request, dest, msg string) {
	// #nosec G710 -- see uiRedirectOK.
	http.Redirect(w, r, safeLocal(dest)+"?err="+urlQueryEscape(msg), http.StatusSeeOther)
}

// safeLocal reduces a redirect destination to a path on this origin.
//
// Destinations are assembled from path values — a target name, a consent mode
// — which arrive from the request and are therefore attacker-influenced. The
// current callers cannot in fact produce an off-site URL, but relying on that
// means every future caller has to be audited for it. Enforcing it here makes
// an off-site redirect impossible to introduce (Tenet 9).
func safeLocal(dest string) string {
	const fallback = "/"

	if dest == "" {
		return fallback
	}

	// A scheme, or a protocol-relative "//host", would leave this origin.
	if strings.Contains(dest, "://") || strings.HasPrefix(dest, "//") {
		return fallback
	}

	if !strings.HasPrefix(dest, "/") {
		return fallback
	}

	// A control character could split the header.
	if strings.ContainsFunc(dest, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fallback
	}

	// Any query or fragment the caller appended is replaced by the flash
	// parameter, so it must not be smuggled in through dest.
	if i := strings.IndexAny(dest, "?#"); i >= 0 {
		dest = dest[:i]
	}

	return dest
}
