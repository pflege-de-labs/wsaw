// Command fixture serves the end-to-end test's website: a small static page
// with a real Klaro consent banner, plus a second origin that stands in for a
// third party (Story 7.1).
//
// It is a fixture rather than a mock on purpose. wsaw already ships a consent
// rule for Klaro, and a banner written to match that rule would only prove
// the rule matches itself. Klaro really does block scripts before consent and
// release them after, so "fired despite reject" is something this fixture can
// actually do.
//
// Two roles, one binary, because the site and the third party differ only in
// what they serve and the stack needs both:
//
//	fixture -role=site        -listen=:8080 -third-party-base=http://tracker.example:8081
//	fixture -role=third-party -listen=:8081
//
// Everything served here is deterministic: no clock, no randomness, no
// generated identifiers. Two scans of an unchanged fixture have to produce an
// empty diff, because that is what makes a non-empty diff mean something
// (Tenet 6).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"
)

// Roles.
const (
	roleSite       = "site"
	roleThirdParty = "third-party"
)

// Variants. The test switches between them to produce a known change, so the
// diff engine and the severity rules are exercised on real captures rather
// than on hand-built results (Story 7.1, AC5).
const (
	// variantBase is the steady state: one unconditional third-party asset,
	// one consent-gated one.
	variantBase = "base"
	// variantChanged adds a second third-party host and changes a
	// first-party script's body, so a scan against it produces a new host
	// and a changed script digest and nothing else.
	variantChanged = "changed"
)

func main() {
	role := flag.String("role", roleSite, "site or third-party")
	listen := flag.String("listen", ":8080", "address to serve on")
	klaroPath := flag.String("klaro", "/srv/klaro/klaro.js", "path to the pinned klaro.js")
	thirdPartyBase := flag.String("third-party-base", "", "absolute base URL of the third-party origin")
	extraBase := flag.String("extra-third-party-base", "",
		"absolute base URL of the extra third-party origin, contacted only in the changed variant")
	selfBase := flag.String("self-base", "",
		"absolute base URL this third-party origin is reached at, used in the scripts it serves")

	flag.Parse()

	if err := run(*role, *listen, *klaroPath, *thirdPartyBase, *extraBase, *selfBase); err != nil {
		log.Fatal(err)
	}
}

func run(role, listen, klaroPath, thirdPartyBase, extraBase, selfBase string) error {
	var (
		mux http.Handler
		err error
	)

	switch role {
	case roleSite:
		mux, err = siteHandler(klaroPath, thirdPartyBase, extraBase)
	case roleThirdParty:
		mux, err = thirdPartyHandler(selfBase)
	default:
		return fmt.Errorf("unknown role %q; use %s or %s", role, roleSite, roleThirdParty)
	}

	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           logRequests(noStore(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listen, err)
	}

	log.Printf("fixture %s listening on %s", role, ln.Addr())

	errCh := make(chan error, 1)

	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		return srv.Shutdown(shutdownCtx)

	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	}
}

// site holds the first-party origin's state.
type site struct {
	tmpl *template.Template

	klaro []byte

	thirdPartyBase string
	extraBase      string

	// variant is switched by the test through the control endpoint. Atomic
	// because a scan may be in flight when it changes.
	variant atomic.Value
}

func siteHandler(klaroPath, thirdPartyBase, extraBase string) (http.Handler, error) {
	if thirdPartyBase == "" {
		return nil, errors.New("-third-party-base is required for the site role: " +
			"a single-origin fixture cannot tell a working first/third-party classifier from a broken one")
	}

	// Read rather than embedded, because klaro.js is fetched and checksummed
	// when the image is built. A missing file has to fail here, loudly, and
	// not as a page that quietly has no consent banner (Tenet 5).
	klaro, err := os.ReadFile(filepath.Clean(klaroPath))
	if err != nil {
		return nil, fmt.Errorf("reading the pinned klaro.js from %s: %w", klaroPath, err)
	}

	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		return nil, fmt.Errorf("parsing the fixture page: %w", err)
	}

	s := &site{
		tmpl:           tmpl,
		klaro:          klaro,
		thirdPartyBase: strings.TrimRight(thirdPartyBase, "/"),
		extraBase:      strings.TrimRight(extraBase, "/"),
	}

	s.variant.Store(variantBase)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /assets/klaro.js", s.handleKlaro)
	mux.HandleFunc("GET /assets/klaro-config.js", s.handleKlaroConfig)
	mux.HandleFunc("GET /assets/site.css", serveStatic("text/css; charset=utf-8", []byte(siteCSS)))
	mux.HandleFunc("GET /assets/app.js", s.handleAppScript)

	// Chrome requests a favicon whether the page asks for one or not. Served
	// rather than 404'd, so the fixture's baseline contains no failed request
	// for a test to have to explain away.
	mux.HandleFunc("GET /favicon.ico", serveStatic("image/gif", onePixelGIF))

	// The control surface. It is deliberately under a path a scan never
	// visits, and it changes only what the *next* page load will contain, so
	// a scan in progress sees one consistent variant.
	mux.HandleFunc("GET /__fixture/variant", s.handleGetVariant)
	mux.HandleFunc("PUT /__fixture/variant", s.handleSetVariant)
	mux.HandleFunc("GET /__fixture/healthz", handleHealth)

	return mux, nil
}

func (s *site) currentVariant() string {
	v, _ := s.variant.Load().(string)

	return v
}

func (s *site) handleIndex(w http.ResponseWriter, _ *http.Request) {
	variant := s.currentVariant()

	data := struct {
		ThirdPartyBase string
		ExtraBase      string
		Variant        string
	}{
		ThirdPartyBase: s.thirdPartyBase,
		Variant:        variant,
	}

	if variant == variantChanged && s.extraBase != "" {
		data.ExtraBase = s.extraBase
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("rendering the fixture page: %v", err)
	}
}

func (s *site) handleKlaro(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(s.klaro)
}

// handleKlaroConfig serves Klaro's configuration.
//
// One service, "analytics", marked as requiring consent and not required, so
// Klaro blocks its script until a decision is made and releases it on accept.
// The unconditional third-party asset in the page is deliberately *not* a
// Klaro service: it is what "contacted before any consent decision" looks
// like, and it is the finding the end-to-end test asserts on (AC3).
func (s *site) handleKlaroConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = io.WriteString(w, klaroConfigJS)
}

// handleAppScript serves a first-party script whose body differs between
// variants, so a scan of the changed variant reports a changed script digest
// (AC5). Nothing else about it changes — no clock, no counter.
func (s *site) handleAppScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")

	body := appScriptBase
	if s.currentVariant() == variantChanged {
		body = appScriptChanged
	}

	_, _ = io.WriteString(w, body)
}

func (s *site) handleGetVariant(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, s.currentVariant()+"\n")
}

func (s *site) handleSetVariant(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)

		return
	}

	want := strings.TrimSpace(string(body))

	switch want {
	case variantBase, variantChanged:
	default:
		http.Error(w, fmt.Sprintf("unknown variant %q; use %s or %s", want, variantBase, variantChanged),
			http.StatusBadRequest)

		return
	}

	if want == variantChanged && s.extraBase == "" {
		http.Error(w,
			"the changed variant needs -extra-third-party-base, or its new host would silently not appear",
			http.StatusPreconditionFailed)

		return
	}

	s.variant.Store(want)
	log.Printf("variant switched to %s", want)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, want+"\n")
}

// thirdPartyHandler serves the origin that stands in for a third party. It is
// a separate service in the stack, reached by its own hostname, so
// first-party and third-party attribution is exercised rather than assumed
// (AC2).
func thirdPartyHandler(selfBase string) (http.Handler, error) {
	if selfBase == "" {
		return nil, errors.New("-self-base is required for the third-party role: " +
			"a script that beacons to a relative URL would report to the *page's* origin, " +
			"so the request wsaw records would be first-party and the initiator chain would prove nothing")
	}

	// Templated rather than relative, for the reason above. It is still one
	// fixed URL, so nothing about it varies between scans.
	analytics := strings.ReplaceAll(analyticsJS, "{{selfBase}}", strings.TrimRight(selfBase, "/"))

	mux := http.NewServeMux()

	// A tracking pixel, loaded unconditionally by the page.
	mux.HandleFunc("GET /pixel.gif", serveStatic("image/gif", onePixelGIF))

	// The consent-gated script. Klaro releases it only after a decision that
	// allows the "analytics" service.
	mux.HandleFunc("GET /analytics.js", serveStatic("application/javascript; charset=utf-8",
		[]byte(analytics)))

	// What analytics.js calls. A fixed URL, because a cache buster or a
	// timestamp here would make every scan differ from the last.
	mux.HandleFunc("GET /collect", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})

	// Reached only in the changed variant, and served here as well so the
	// extra host is an alias of this service rather than another container.
	mux.HandleFunc("GET /extra.gif", serveStatic("image/gif", onePixelGIF))

	mux.HandleFunc("GET /favicon.ico", serveStatic("image/gif", onePixelGIF))
	mux.HandleFunc("GET /__fixture/healthz", handleHealth)

	return mux, nil
}

func serveStatic(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

// noStore keeps the browser from serving a previous scan's copy. wsaw scans
// cold by default, but a warm-cache run must still see the current variant
// rather than the one that was current when the cache was filled.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// logRequests makes what the fixture served visible, which is most of what
// makes a failed end-to-end run diagnosable.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		// #nosec G706 -- the path is sanitised and quoted by safeLogPath; the
		// taint analyser cannot follow the sanitiser, but the test below it
		// can. Same treatment as safeLocal in internal/httpapi.
		log.Printf("%s %s", r.Method, safeLogPath(r.URL.Path))
	})
}

// safeLogPath makes a request path safe to write to a log.
//
// URL.Path is percent-decoded, so a request for %0a arrives as a real
// newline: logged raw, one request could be made to look like two, or like a
// line the fixture never wrote. Control characters are dropped, the result is
// quoted, and it is bounded — the same rule Tenet 9 states for wsaw applies to
// the fixture that tests it.
func safeLogPath(path string) string {
	const maxLogged = 200

	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}

		return r
	}, path)

	if len(clean) > maxLogged {
		clean = clean[:maxLogged] + "…"
	}

	return strconv.Quote(clean)
}

// onePixelGIF is a 1x1 transparent GIF, byte for byte the same on every
// request.
var onePixelGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00,
	0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00,
	0x00, 0x02, 0x02, 0x44, 0x01, 0x00, 0x3b,
}
