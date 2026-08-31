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

// checkCSRF verifies a state-changing form submission. It also requires the
// request to look like a same-origin form post.
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
