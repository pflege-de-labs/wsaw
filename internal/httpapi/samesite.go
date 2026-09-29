package httpapi

import (
	"html/template"
	"log/slog"
	"net/http"
)

// The session cookie is SameSite=Strict, so a browser withholds it from any
// navigation another site started. Following the "Open in wsaw" link on a
// Teams card, a ticket or a mail is such a navigation: the reviewer is
// signed in, the cookie is not sent, and without this they would be shown
// the login form for a session they already have.
//
// Loosening the cookie to Lax would fix that and also hand every GET to
// cross-site requests. Instead, a cross-site navigation that arrives without
// a session is answered with a page that loads the same URL again. That
// second navigation is started by this origin, so the browser sends the
// cookie, and a reviewer without a session goes on to the login form from
// there — the retry is same-origin, so it is never retried again.

// crossSiteNavigation reports whether the browser says another site started
// this request. Sec-Fetch-Site is set by the browser and cannot be forged
// from page script; a browser that does not send it gets the login form
// directly, as before.
func crossSiteNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Site") == "cross-site"
}

// retrySameSiteTmpl uses a meta refresh rather than script: the interface's
// CSP allows no inline script, and a refresh needs none.
var retrySameSiteTmpl = template.Must(template.New("retry").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="referrer" content="no-referrer">
<meta http-equiv="refresh" content="0;url={{.}}">
<title>wsaw</title>
</head>
<body>
<p><a href="{{.}}">Continue to wsaw</a></p>
</body>
</html>
`))

// retrySameSite answers a cross-site navigation with a page that reloads the
// same path from this origin.
func (s *Server) retrySameSite(w http.ResponseWriter, r *http.Request) {
	dest := safeLocal(r.URL.EscapedPath())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := retrySameSiteTmpl.Execute(w, dest); err != nil {
		// The status line is already written; all that is left is to say so.
		slog.WarnContext(r.Context(), "rendering same-site retry", "path", dest, "error", err)
	}
}
