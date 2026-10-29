package browser_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
)

// printPool starts a pool on a real Chrome, or skips saying why there is
// none.
func printPool(t *testing.T) (*browser.Pool, string) {
	t.Helper()

	if os.Getenv("WSAW_SKIP_BROWSER_TESTS") != "" {
		t.Skip("skipping browser test: WSAW_SKIP_BROWSER_TESTS is set")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	info, err := browser.Discover(ctx, os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("skipping browser test: no usable Chrome found (%v)", err)
	}

	profiles := t.TempDir()

	return browser.NewPool(browser.PoolOptions{
		Size:   1,
		Launch: browser.Options{Info: info, ProfileDir: profiles, LaunchTimeout: 40 * time.Second},
	}), profiles
}

func closePool(t *testing.T, p *browser.Pool) {
	t.Helper()

	if err := p.Close(); err != nil {
		t.Errorf("closing the pool: %v", err)
	}
}

// The report embeds text the scanned page chose. Whatever the document asks
// for, the printer must not fetch it (Story 5.39, AC5). The document here has
// no Content-Security-Policy, so the printer's own blocking is what is tested.
func TestPrintPDFContactsNothing(t *testing.T) {
	t.Parallel()

	p, _ := printPool(t)
	defer closePool(t, p)

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	html := `<!doctype html><html><head>
<link rel="stylesheet" href="` + srv.URL + `/style.css">
<script src="` + srv.URL + `/script.js"></script>
</head><body>
<h1>hostile</h1>
<img src="` + srv.URL + `/pixel.gif">
<iframe src="` + srv.URL + `/frame"></iframe>
<div style="background-image:url(` + srv.URL + `/bg.png)">x</div>
<script>fetch("` + srv.URL + `/xhr")</script>
</body></html>`

	pdf, err := browser.NewPrinter(p, 0).PrintPDF(t.Context(), []byte(html), "<span></span>")
	if err != nil {
		t.Fatalf("PrintPDF: %v", err)
	}

	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF-: %q", pdf[:min(len(pdf), 16)])
	}

	if n := hits.Load(); n != 0 {
		t.Errorf("printing made %d request(s) to the server the document named, want none", n)
	}
}

// Chrome stamps each PDF with the time it was made; nothing else may differ.
var pdfTimestamps = regexp.MustCompile(`/(CreationDate|ModDate) ?\(D:[^)]*\)`)

func TestPrintPDFIsDeterministic(t *testing.T) {
	t.Parallel()

	p, _ := printPool(t)
	defer closePool(t, p)

	pr := browser.NewPrinter(p, 0)
	html := []byte(`<!doctype html><title>t</title><h1>same</h1><p>twice</p>`)

	first, err := pr.PrintPDF(t.Context(), html, "<span>footer</span>")
	if err != nil {
		t.Fatalf("first print: %v", err)
	}

	second, err := pr.PrintPDF(t.Context(), html, "<span>footer</span>")
	if err != nil {
		t.Fatalf("second print: %v", err)
	}

	if !bytes.Equal(pdfTimestamps.ReplaceAll(first, nil), pdfTimestamps.ReplaceAll(second, nil)) {
		t.Error("two prints of the same document differ beyond their timestamps")
	}
}

// A print that misses its deadline returns an error, never a partial PDF,
// and leaves nothing behind once the pool is closed (AGENTS.md §3.5).
func TestPrintPDFDeadline(t *testing.T) {
	t.Parallel()

	p, profiles := printPool(t)

	// Warm the pool, so the short deadline below lands on the print and not
	// on launching Chrome.
	if _, err := browser.NewPrinter(p, 0).PrintPDF(t.Context(), []byte("<p>warm</p>"), ""); err != nil {
		t.Fatalf("warm-up print: %v", err)
	}

	pdf, err := browser.NewPrinter(p, time.Millisecond).PrintPDF(t.Context(), []byte("<p>late</p>"), "")
	if err == nil {
		t.Fatal("a print with a 1ms deadline succeeded")
	}

	if pdf != nil {
		t.Errorf("a failed print returned %d bytes", len(pdf))
	}

	closePool(t, p)

	left, err := os.ReadDir(profiles)
	if err != nil {
		t.Fatalf("reading the profile directory: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("%d profile director(ies) left behind after the pool closed", len(left))
	}
}
