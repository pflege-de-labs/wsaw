package httpapi

import (
	"net/http"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Where a "Scan now" press goes back to, and how a scripted press is answered
// (Story 5.26).
//
// The button used to send every reader to the target's own history page,
// whichever page they pressed it on. That is right on the history page and
// wrong on the watchboard: a reader working through the board loses their
// filter, their collapsed groups and their place in the list every time they
// start a scan. The form now says which page it was submitted from, and a
// press made by scan.js is answered with a small JSON body instead of a
// redirect, because that press has no page to land on — it never left.

// rescanReply is what a scripted press gets back. It carries the same
// sentence the redirect would have carried in its flash parameter, so both
// paths tell the reader the same thing (AC3).
type rescanReply struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// rescanReturn decides where a rescan press returns to.
//
// The value arrives in a form field and is therefore attacker-influenced, so
// it is not merely sanitised but checked against the pages the interface
// actually has: the board, or a target's own history page. Anything else — an
// off-origin URL, a scheme-relative one, a path this interface does not
// serve, or no value at all — falls back to the destination the handler used
// before this story, which is safe by construction (AC2, Tenet 9).
func rescanReturn(from, target string, mode model.ConsentMode) string {
	fallback := "/targets/" + target + "/" + string(mode)

	if from == "" {
		return fallback
	}

	// safeLocal answers "/" both for a value that is already the board and
	// for one it rejected outright, so "did safeLocal change this?" is asked
	// separately from "is this a page we have?". Without that, a rejected
	// value would quietly become the board rather than the fallback.
	raw := from
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}

	dest := safeLocal(from)
	if dest != raw {
		return fallback
	}

	if dest == "/" || isSeriesPath(dest) {
		return dest
	}

	return fallback
}

// isSeriesPath reports whether p is a target's history page,
// /targets/{name}/{mode}, with a consent mode this build knows.
func isSeriesPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/targets/")
	if !ok {
		return false
	}

	name, mode, ok := strings.Cut(rest, "/")
	if !ok || name == "" || strings.Contains(mode, "/") {
		return false
	}

	return model.ConsentMode(mode).Valid()
}

// scriptedRescan reports whether the press came from scan.js rather than from
// a plain form submission.
//
// The script asks for JSON; a browser submitting the form asks for HTML. The
// difference decides whether the answer is a body or a redirect, and nothing
// else: the same checks run, in the same order, for both (AC4).
func scriptedRescan(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// rescanStarted reports a scan that was started.
func (s *Server) rescanStarted(w http.ResponseWriter, r *http.Request, dest, msg string) {
	if scriptedRescan(r) {
		writeJSON(w, http.StatusOK, rescanReply{OK: true, Message: msg})

		return
	}

	s.uiRedirectOK(w, r, dest, msg)
}

// rescanRefused reports a press that started nothing, and why.
//
// A scripted press gets the status as well as the sentence: scan.js has to
// tell "this did not happen" from "this happened" without reading prose, and
// a press that silently appeared to work is the failure mode this repository
// treats as the worst one there is (Tenet 5).
func (s *Server) rescanRefused(
	w http.ResponseWriter, r *http.Request, dest, msg string, status int,
) {
	if scriptedRescan(r) {
		writeJSONError(w, status, msg)

		return
	}

	s.uiRedirectError(w, r, dest, msg)
}
