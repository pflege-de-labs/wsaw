// Package httpapi serves wsaw's JSON API and its web interface.
//
// The web interface is a strict consumer of the same data as any external
// client: it gets no privileged endpoints and no separate path into the store
// (Tenet 16). Everything it renders comes from stored results.
//
// Every value rendered originates from a scanned page and is therefore
// hostile. html/template's contextual escaping is the primary defence; a
// strict Content-Security-Policy with no unsafe-inline is the second.
package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/share"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// ScanTrigger runs an ad-hoc scan of an already-configured target.
type ScanTrigger interface {
	Trigger(ctx context.Context, target string, mode model.ConsentMode) (scanner.Outcome, error)
}

// Options configures the server.
type Options struct {
	Listen string

	// Token protects both the API and the UI. Required for any non-loopback
	// listener, which config validation enforces.
	Token secret.Value

	TLSCert string
	TLSKey  string

	// WebUI serves the browser interface in addition to the API.
	WebUI bool
	// ReadOnly disables every write action, for shared reviewer deployments.
	ReadOnly bool
	// AllowAdHocScan permits triggering scans through the API and UI.
	AllowAdHocScan bool

	// MetricsEnabled serves the Prometheus endpoint.
	MetricsEnabled bool
	MetricsPath    string

	// Share signs and verifies the links that let somebody read one result
	// without the API token (Story 5.19). Nil disables sharing entirely,
	// which is the default: a share link publishes data that can be personal
	// to whoever holds it.
	Share *share.Signer
	// ShareBaseURL is where this wsaw is reachable from, so a minted link is
	// something an operator can copy and send rather than assemble.
	ShareBaseURL string

	// SignedArtifactURLs lets an artifact request be answered with a redirect
	// to the bucket rather than with the bytes. Off unless an operator asked
	// for it: a redirect moves access control from wsaw to whoever holds the
	// URL until it expires (Story 8.7, AC3).
	SignedArtifactURLs bool
	// SignedArtifactURLTTL is how long such a redirect may be honoured. A
	// redirect issued to a shared reader is additionally cut to what is left
	// of their share link.
	SignedArtifactURLTTL time.Duration

	// RefreshDefault is how often the interface reloads itself for a viewer
	// who has expressed no preference. Zero means not at all. It is only the
	// default: the choice belongs to whoever is looking (Story 5.16).
	RefreshDefault time.Duration

	// StaleAfter is how old a target's last success may be before the
	// dashboard flags it. A stalled watcher must be obvious in the UI, not
	// only in metrics (Story 5.8).
	StaleAfter time.Duration

	Version string
}

// Deps are the collaborators the server reads from.
type Deps struct {
	Store   *store.Store
	Metrics *metrics.Registry
	Daemon  *daemon.Daemon
	Trigger ScanTrigger
	Rules   *consent.RuleSet
	Logger  *slog.Logger

	// Targets returns the current target list. It is a function so a config
	// reload is reflected without restarting the server.
	Targets func() []config.Resolved

	// Running returns the scans in flight. Optional: without it the interface
	// says that activity is not tracked rather than that nothing is running,
	// because those are different claims (Tenet 5).
	Running func() []scanner.Running

	// ConfigPath is shown in the UI so a write action can say where a
	// file-based change belongs.
	ConfigPath string
}

// Server serves the API and UI.
type Server struct {
	opts Options
	deps Deps

	mux  *http.ServeMux
	http *http.Server

	ui *uiRenderer

	// noSigning records that the artifact bucket cannot produce signed URLs,
	// so the fallback to serving the bytes costs one attempt for the life of
	// the process rather than one per request. Whether a provider signs is a
	// property of the provider (Story 8.7, AC3).
	noSigning atomic.Bool
	// signingWarned keeps a bucket that fails to sign for some other reason
	// from writing a log line per screenshot on every page view.
	signingWarned atomic.Bool
}

// New builds the server.
func New(opts Options, deps Deps) (*Server, error) {
	if deps.Store == nil {
		return nil, errors.New("httpapi: store is required")
	}

	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:8712"
	}

	if opts.MetricsPath == "" {
		opts.MetricsPath = "/metrics"
	}

	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 48 * time.Hour
	}

	s := &Server{opts: opts, deps: deps, mux: http.NewServeMux()}

	if opts.WebUI {
		ui, err := newUIRenderer()
		if err != nil {
			return nil, err
		}

		s.ui = ui
	}

	s.routes()

	s.http = &http.Server{
		Addr:    opts.Listen,
		Handler: s.handler(),
		// Bounded timeouts: a slow client must not be able to tie up the
		// server that reports whether scanning still works.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s, nil
}

// Handler returns the fully wrapped handler, including authentication and
// security headers. It is exported so the server can be exercised in-process,
// without binding a socket.
func (s *Server) Handler() http.Handler { return s.handler() }

func (s *Server) handler() http.Handler {
	var h http.Handler = s.mux

	h = s.withAuth(h)
	h = s.withSecurityHeaders(h)
	h = s.withLogging(h)

	return h
}

// Addr reports the listen address, useful after binding to port 0 in tests.
func (s *Server) Addr() string { return s.http.Addr }

// Serve runs the server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	// A ListenConfig so that binding respects cancellation, like every other
	// blocking operation in wsaw.
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", s.opts.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.opts.Listen, err)
	}

	s.http.Addr = ln.Addr().String()

	s.deps.Logger.Info(
		"http server listening",
		"addr", s.http.Addr,
		"web_ui", s.opts.WebUI,
		"read_only", s.opts.ReadOnly,
		"authenticated", s.opts.Token.IsSet(),
	)

	errCh := make(chan error, 1)

	go func() {
		var serveErr error

		if s.opts.TLSCert != "" && s.opts.TLSKey != "" {
			serveErr = s.http.ServeTLS(ln, s.opts.TLSCert, s.opts.TLSKey)
		} else {
			serveErr = s.http.Serve(ln)
		}

		errCh <- serveErr
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server: %w", err)
		}

		return nil

	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("http server: %w", err)
	}
}

// withSecurityHeaders applies the headers that keep captured, hostile content
// from doing anything in a reviewer's browser (Story 5.11).
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	// No unsafe-inline anywhere: all CSS and JS is served as its own file, so
	// a captured script fragment rendered into a page cannot execute. No
	// external origins, so the UI cannot be made to call out.
	const csp = "default-src 'none'; " +
		"style-src 'self'; " +
		"script-src 'self'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"form-action 'self'; " +
		"base-uri 'none'; " +
		"frame-ancestors 'none'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")

		if s.opts.TLSCert != "" {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}

		next.ServeHTTP(w, r)
	})
}

const sessionCookie = "wsaw_session"

// withAuth enforces the shared token. A browser gets a session cookie so it
// need not put the token in every URL, where it would land in history and in
// referrer headers.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Token.IsSet() {
			next.ServeHTTP(w, r)

			return
		}

		// The login form itself must be reachable unauthenticated.
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)

			return
		}

		// A shared link carries its own authority and is verified by the
		// handler, which checks the signature, the expiry and the exact
		// result before serving anything. It is the one prefix that does not
		// use the API token — and nothing outside it accepts a share token
		// (Story 5.19).
		if strings.HasPrefix(r.URL.Path, "/shared/") {
			next.ServeHTTP(w, r)

			return
		}

		if s.authenticated(r) {
			next.ServeHTTP(w, r)

			return
		}

		if s.wantsHTML(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)

			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="wsaw"`)
		writeJSONError(w, http.StatusUnauthorized, "authentication required")
	})
}

func (s *Server) authenticated(r *http.Request) bool {
	token := s.opts.Token.Reveal()

	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		// Constant-time comparison: a timing oracle on the token would let it
		// be recovered byte by byte.
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(token)) == 1 {
			return true
		}
	}

	if c, err := r.Cookie(sessionCookie); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1 {
			return true
		}
	}

	return false
}

func (s *Server) wantsHTML(r *http.Request) bool {
	return s.opts.WebUI && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.deps.Logger.Debug(
			"http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// writeAllowed reports whether a write action may proceed, and why not.
func (s *Server) writeAllowed() (bool, string) {
	if s.opts.ReadOnly {
		return false, "wsaw is running in read-only mode"
	}

	return true, ""
}
