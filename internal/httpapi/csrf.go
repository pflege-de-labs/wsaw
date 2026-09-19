package httpapi

import (
	"crypto/subtle"
	"net/http"
	"net/url"
)

// CSRF protection for the web interface's write actions (Story 5.11).
//
// The token is the session token itself, which is safe here because it never
// leaves the origin: it is HttpOnly in the cookie, and the copy in the form
// body is only readable by same-origin script, which the CSP already
// forbids. When no token is configured — the single-user localhost case —
// there is no session to forge, so CSRF protection would be theatre.

func (s *Server) csrfToken() string {
	if !s.opts.Token.IsSet() {
		return ""
	}

	return s.opts.Token.Reveal()
}

// checkCSRF verifies a state-changing form submission. The defence is the
// token comparison plus the cookie's SameSite=Strict: a cross-site form
// post cannot carry the session cookie, so it cannot present a matching
// token, and the CSP's lack of unsafe-inline means no script context exists
// to read one for an attacker either.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form submission", http.StatusBadRequest)

		return false
	}

	if !s.opts.Token.IsSet() {
		return true
	}

	supplied := r.FormValue("csrf")
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.csrfToken())) != 1 {
		http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)

		return false
	}

	return true
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}
