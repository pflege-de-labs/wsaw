package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/share"
)

// Sharing one result by an expiring link (Story 5.19).
//
// Everything a shared reader can reach lives under /shared/, which is the
// only part of the interface that authenticates with something other than the
// API token — it authenticates with the link itself. Keeping it on its own
// prefix is what makes the rule reviewable: no share token is ever consulted
// for a path outside here, and no path here consults the API token.

// shareTokenParam is where the token travels. A query parameter rather than a
// path segment, so the rest of the URL still reads as the result it points at.
const shareTokenParam = "t"

func (s *Server) shareRoutes() {
	if s.opts.Share == nil {
		return
	}

	// Minting needs an authenticated operator, and lives under the API where
	// the rest of the authenticated surface is.
	s.mux.HandleFunc("POST /api/v1/share/{target}/{mode}/{scan}", s.handleMintShare)

	// The button on the result page. It renders the link rather than
	// redirecting with it: a redirect would put the token in the browser's
	// history and in the referrer of whatever the reader clicked next, and a
	// share link is a credential (AC11).
	if s.opts.WebUI {
		s.mux.HandleFunc("POST /share/{target}/{mode}/{scan}", s.handleUIShare)
	}

	s.mux.HandleFunc("GET /shared/{target}/{mode}/{scan}", s.handleSharedResult)
	s.mux.HandleFunc("GET /shared/{target}/{mode}/{scan}/json", s.handleSharedJSON)
	s.mux.HandleFunc("GET /shared/{target}/{mode}/{scan}/har", s.handleSharedHAR)
	s.mux.HandleFunc("GET /shared/{target}/{mode}/{scan}/csv", s.handleSharedCSV)
	s.mux.HandleFunc("GET /shared/{target}/{mode}/{scan}/artifacts/{ref...}", s.handleSharedArtifact)
}

// sharingEnabled reports whether links can be minted or honoured at all.
// Sharing publishes data that can be personal to whoever holds a link, so it
// stays off until configured (Story 5.19, AC12).
func (s *Server) sharingEnabled() bool { return s.opts.Share != nil }

// handleMintShare issues a link for one stored result.
func (s *Server) handleMintShare(w http.ResponseWriter, r *http.Request) {
	target, mode, ok := pathTargetMode(w, r)
	if !ok {
		return
	}

	scan := r.PathValue("scan")

	// The result has to exist. A link to nothing would fail later, in front of
	// whoever it was sent to.
	res, err := s.deps.Store.GetResult(target, mode, scan)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	validity, err := requestedValidity(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())

		return
	}

	token, claims, err := s.opts.Share.Mint(res.Target, string(res.ConsentMode), res.ScanID, validity)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())

		return
	}

	// Logged without the token: a share link is a credential, and a
	// credential in a log is a credential (AC11).
	s.deps.Logger.Info("share link issued",
		"target", res.Target, "consent_mode", string(res.ConsentMode), "scan_id", res.ScanID,
		"expires_at", claims.Expiry().Format(time.RFC3339))

	writeJSON(w, http.StatusOK, map[string]any{
		"url":       s.shareURL(res, token),
		"path":      sharePath(res) + "?" + shareTokenParam + "=" + token,
		"expiresAt": claims.Expiry(),
		"note": "This link grants read-only access to this one scan until it expires. " +
			"It cannot be revoked before then; rotate api.share.key to invalidate every outstanding link.",
	})
}

// requestedValidity reads how long the caller wants the link to last. The
// signer clamps it to the configured maximum, so an over-long request is
// honoured as far as policy allows rather than refused.
func requestedValidity(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("validity")
	if raw == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("validity must be a duration such as \"24h\" or \"7d\" expressed in hours")
	}

	if d <= 0 {
		return 0, errors.New("validity must be positive")
	}

	return d, nil
}

// shareURL builds the absolute link where a base URL is configured, so the
// operator can copy something sendable rather than assemble it.
func (s *Server) shareURL(res *model.Result, token string) string {
	path := sharePath(res) + "?" + shareTokenParam + "=" + token

	if s.opts.ShareBaseURL == "" {
		return path
	}

	return strings.TrimRight(s.opts.ShareBaseURL, "/") + path
}

func sharePath(res *model.Result) string {
	return "/shared/" + url.PathEscape(res.Target) +
		"/" + url.PathEscape(string(res.ConsentMode)) +
		"/" + url.PathEscape(res.ScanID)
}

// sharedResult verifies the link and loads exactly the result it names.
//
// Nothing downstream re-derives what the caller may see: the scope check
// happens here, against the path being served, so a genuine token for one
// scan cannot be pointed at another (AC2).
func (s *Server) sharedResult(w http.ResponseWriter, r *http.Request) (*model.Result, share.Claims, bool) {
	if !s.sharingEnabled() {
		http.Error(w, "sharing is not enabled on this wsaw", http.StatusNotFound)

		return nil, share.Claims{}, false
	}

	target := r.PathValue("target")
	mode := r.PathValue("mode")
	scan := r.PathValue("scan")

	claims, err := s.opts.Share.Verify(r.URL.Query().Get(shareTokenParam))
	if err != nil {
		// Expired and invalid are different facts for the person holding the
		// link, and neither is worth hiding from them (AC7).
		status := http.StatusForbidden
		if errors.Is(err, share.ErrExpired) {
			status = http.StatusGone
		}

		http.Error(w, err.Error(), status)

		return nil, share.Claims{}, false
	}

	if !claims.Allows(target, mode, scan) {
		http.Error(w, share.ErrScope.Error(), http.StatusForbidden)

		return nil, share.Claims{}, false
	}

	res, err := s.deps.Store.GetResult(target, model.ConsentMode(mode), scan)
	if err != nil {
		writeStoreError(w, err)

		return nil, share.Claims{}, false
	}

	return res, claims, true
}

// shareLinkData is the little page an operator gets after pressing Share.
type shareLinkData struct {
	Target      string
	ConsentMode string
	ScanID      string

	URL       string
	ExpiresAt time.Time
	Validity  string

	// Absolute is false when no base URL is configured and the link is only a
	// path, which is not something an operator can send as it stands.
	Absolute bool
}

// handleUIShare mints a link from the result page.
func (s *Server) handleUIShare(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}

	target, mode, ok := uiTargetMode(w, r)
	if !ok {
		return
	}

	scan := r.PathValue("scan")

	res, err := s.deps.Store.GetResult(target, mode, scan)
	if err != nil {
		s.uiError(w, r, http.StatusNotFound, err.Error())

		return
	}

	validity, err := time.ParseDuration(r.FormValue("validity"))
	if err != nil {
		validity = 0
	}

	token, claims, err := s.opts.Share.Mint(res.Target, string(res.ConsentMode), res.ScanID, validity)
	if err != nil {
		s.uiError(w, r, http.StatusInternalServerError, err.Error())

		return
	}

	s.deps.Logger.Info("share link issued from the web interface",
		"target", res.Target, "consent_mode", string(res.ConsentMode), "scan_id", res.ScanID,
		"expires_at", claims.Expiry().Format(time.RFC3339))

	data := shareLinkData{
		Target:      res.Target,
		ConsentMode: string(res.ConsentMode),
		ScanID:      res.ScanID,
		URL:         s.shareURL(res, token),
		ExpiresAt:   claims.Expiry(),
		Validity:    time.Until(claims.Expiry()).Round(time.Minute).String(),
		Absolute:    s.opts.ShareBaseURL != "",
	}

	// Never cached: a page holding a credential should not sit in a shared
	// browser's cache. The referrer policy is already no-referrer globally.
	w.Header().Set("Cache-Control", "no-store")

	s.render(w, r, "share-link.html", "Share this scan", data)
}

// sharedData is what the shared page renders. It is deliberately not the
// full page struct: a shared reader gets the result and nothing that would
// navigate them into the rest of the interface (AC8).
type sharedData struct {
	Result *model.Result
	Hosts  []model.HostSummary
	Counts map[string]int

	Screenshots []screenshotView
	Diff        *diffView

	Requests  []model.Request
	Truncated bool
	Total     int

	// ExpiresAt tells the reader the link is not permanent, and tells the
	// sender what they sent (AC9).
	ExpiresAt time.Time

	Version string

	// base and token build the page's own links. They are absolute paths
	// rather than relative ones: from /shared/site/reject/scan-1 a relative
	// "json" resolves to /shared/site/reject/json, which is a different
	// result — or none.
	base  string
	token string
}

// Link builds a URL for one of this result's own views, carrying the token.
func (d sharedData) Link(suffix string) string {
	return d.base + "/" + suffix + "?" + shareTokenParam + "=" + url.QueryEscape(d.token)
}

// ArtifactLink builds a URL for one of this result's stored artifacts.
func (d sharedData) ArtifactLink(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}

	return d.base + "/artifacts/" + strings.Join(parts, "/") +
		"?" + shareTokenParam + "=" + url.QueryEscape(d.token)
}

// diffView is the comparison, reduced to what a shared reader needs.
type diffView struct {
	Comparable bool
	Reason     string
	Changes    []sharedChange
	Suppressed int
}

type sharedChange struct {
	Severity string
	Type     string
	Subject  string
	Detail   string
}

func (s *Server) handleSharedResult(w http.ResponseWriter, r *http.Request) {
	res, claims, ok := s.sharedResult(w, r)
	if !ok {
		return
	}

	data := sharedData{
		Result:      res,
		Hosts:       res.HostSummaries(),
		Counts:      res.CountsByResourceType(),
		Screenshots: s.screenshotViews(r.Context(), res),
		ExpiresAt:   claims.Expiry(),
		Version:     s.opts.Version,
		base:        sharePath(res),
		token:       r.URL.Query().Get(shareTokenParam),
	}

	if rep := s.diffFor(res); rep != nil {
		view := &diffView{
			Comparable: rep.Comparable,
			Reason:     rep.Reason,
			Suppressed: rep.Suppressed,
		}

		for _, c := range rep.Changes {
			view.Changes = append(view.Changes, sharedChange{
				Severity: string(c.Severity),
				Type:     string(c.Type),
				Subject:  c.Subject,
				Detail:   c.Detail,
			})
		}

		data.Diff = view
	}

	for i := range res.Requests {
		data.Total++

		if len(data.Requests) < maxRenderedRequests {
			data.Requests = append(data.Requests, res.Requests[i])
		} else {
			data.Truncated = true
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := s.ui.tmpl.ExecuteTemplate(w, "shared.html", data); err != nil {
		s.deps.Logger.Error("rendering the shared page", "error", err)
	}
}

func (s *Server) handleSharedJSON(w http.ResponseWriter, r *http.Request) {
	res, _, ok := s.sharedResult(w, r)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, report.WithBodies(res, s.bodyLoader(r)))
}

func (s *Server) handleSharedHAR(w http.ResponseWriter, r *http.Request) {
	res, _, ok := s.sharedResult(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(res, "har")+`"`)

	if err := report.WriteHAR(w, res, s.bodyLoader(r)); err != nil {
		s.deps.Logger.Error("writing a shared HAR", "error", err)
	}
}

func (s *Server) handleSharedCSV(w http.ResponseWriter, r *http.Request) {
	res, _, ok := s.sharedResult(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(res, "csv")+`"`)

	if err := report.WriteCSV(w, res); err != nil {
		s.deps.Logger.Error("writing a shared CSV", "error", err)
	}
}

// handleSharedArtifact serves a screenshot or body belonging to the shared
// result — and only to it.
//
// Artifacts are content-addressed rather than scoped by scan, so a reference
// alone says nothing about who may read it. Without this check a link to one
// harmless scan would be a key to every stored artifact wsaw has (AC10) —
// which a shared bucket makes truer rather than less true, since every scan's
// evidence is one keyspace and the digest is the whole of a key
// (Story 8.7, AC2).
func (s *Server) handleSharedArtifact(w http.ResponseWriter, r *http.Request) {
	res, claims, ok := s.sharedResult(w, r)
	if !ok {
		return
	}

	ref := r.PathValue("ref")

	if !resultNamesArtifact(res, ref) {
		http.Error(w, "this link does not cover that file", http.StatusForbidden)

		return
	}

	// The link's own expiry bounds anything issued off the back of it. A
	// signed URL outliving the link would leave a reader holding access that
	// the link's expiry was supposed to end (Story 8.7, AC3).
	s.serveArtifactUntil(w, r, ref, claims.Expiry())
}

// resultNamesArtifact reports whether a result actually references an
// artifact: one of its screenshots, or one of its stored response bodies.
func resultNamesArtifact(res *model.Result, ref string) bool {
	if ref == "" {
		return false
	}

	for _, a := range res.Screenshots {
		if a.Ref == ref {
			return true
		}
	}

	for i := range res.Requests {
		if res.Requests[i].BodyRef == ref {
			return true
		}
	}

	return false
}
