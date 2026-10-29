package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// echoPrinter answers with "%PDF-" followed by the HTML it was given, so a
// test reads what would have been printed. err, when set, is returned instead.
type echoPrinter struct {
	err   error
	calls atomic.Int64
}

func (p *echoPrinter) PrintPDF(_ context.Context, html []byte, footer string) ([]byte, error) {
	p.calls.Add(1)

	if p.err != nil {
		return nil, p.err
	}

	return append([]byte("%PDF-"), append(html, footer...)...), nil
}

func withPrinter(p *echoPrinter) func(*httpapi.Deps) {
	return func(d *httpapi.Deps) { d.Printer = p }
}

// seedComparedPair stores a baseline without the tracker and a later scan
// with it, so the later scan has a change to report.
func seedComparedPair(t *testing.T, f *fixture) {
	t.Helper()

	now := time.Now()

	f.seed("scan-0", model.ConsentReject, now.Add(-time.Hour), func(r *model.Result) { r.Requests = r.Requests[:1] })
	f.seed("scan-1", model.ConsentReject, now, nil)

	if _, err := f.store.SetBaseline("site", model.ConsentReject, "scan-0", "tester", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPDFReport(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{}, nil, nil, withPrinter(&echoPrinter{}))
	seedComparedPair(t, f)

	resp := f.get("/api/v1/results/site/reject/scan-1/pdf")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET pdf = %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}

	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.HasSuffix(cd, `.pdf"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}

	got := body(t, resp)

	for _, want := range []string{
		"Compared against the approved baseline, scan <code>scan-0</code>",
		"/api/v1/results/site/reject/scan-1?bodies=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
}

func TestPDFReportRequiresAuthentication(t *testing.T) {
	t.Parallel()

	p := &echoPrinter{}
	f := newFixtureWith(t, httpapi.Options{Token: secret.Literal("s3cret")}, nil, nil, withPrinter(p))
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	if resp := f.get("/api/v1/results/site/reject/scan-1/pdf"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token = %d, want 401", resp.StatusCode)
	}

	if p.calls.Load() != 0 {
		t.Error("an unauthenticated request reached the printer")
	}

	if resp := f.get("/api/v1/results/site/reject/scan-1/pdf", "Authorization", "Bearer s3cret"); resp.StatusCode != http.StatusOK {
		t.Errorf("with the token = %d, want 200", resp.StatusCode)
	}
}

func TestPDFReportOfAnUnknownScanIsNotFound(t *testing.T) {
	t.Parallel()

	p := &echoPrinter{}
	f := newFixtureWith(t, httpapi.Options{}, nil, nil, withPrinter(p))

	if resp := f.get("/api/v1/results/site/reject/scan-nope/pdf"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown scan = %d, want 404", resp.StatusCode)
	}

	if p.calls.Load() != 0 {
		t.Error("an unknown scan reached the printer")
	}
}

// A render that fails is an error the client sees and the log records, never
// a PDF (AC6, AC8).
func TestPDFReportFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		printer    *echoPrinter
		status     int
		retryAfter bool
	}{
		{"busy", &echoPrinter{err: fmt.Errorf("waiting: %w", browser.ErrPrinterBusy)}, http.StatusServiceUnavailable, true},
		{"failed", &echoPrinter{err: errors.New("printing: target crashed")}, http.StatusInternalServerError, false},
		{"no printer", nil, http.StatusServiceUnavailable, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tweak := withPrinter(tt.printer)
			if tt.printer == nil {
				tweak = nil
			}

			f := newFixtureWith(t, httpapi.Options{}, nil, nil, tweak)
			f.seed("scan-1", model.ConsentReject, time.Now(), nil)

			resp := f.get("/api/v1/results/site/reject/scan-1/pdf")
			_ = body(t, resp)

			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}

			if resp.Header.Get("Content-Type") == "application/pdf" {
				t.Error("a failed render was served as a PDF")
			}

			if got := resp.Header.Get("Retry-After") != ""; got != tt.retryAfter {
				t.Errorf("Retry-After present = %v, want %v", got, tt.retryAfter)
			}

			if tt.printer != nil {
				logs := f.logs()
				for _, key := range []string{"level=WARN", "scan_id=scan-1", "target=site", "consent_mode=reject"} {
					if !strings.Contains(logs, key) {
						t.Errorf("the failure's log line lacks %s", key)
					}
				}
			}
		})
	}
}

// The shared view's PDF holds nothing the shared page does not show: not the
// baseline it was compared with, and not the share token (Story 5.19, AC3).
func TestSharedPDFStaysWithinTheSharedResult(t *testing.T) {
	t.Parallel()

	signer := shareSigner(t)
	f := newFixtureWith(t, httpapi.Options{
		WebUI: true, Token: secret.Literal("s3cret"), Share: signer, ShareBaseURL: "https://wsaw.example.com",
	}, nil, nil, withPrinter(&echoPrinter{}))
	seedComparedPair(t, f)

	link := linkFor(t, signer, "site", "reject", "scan-1")
	path, token, _ := strings.Cut(link, "?t=")

	resp := f.get(path + "/pdf?t=" + token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shared pdf = %d, want 200", resp.StatusCode)
	}

	got := body(t, resp)

	if !strings.Contains(got, "host-added") {
		t.Error("the shared PDF does not carry the scan's change")
	}

	for _, leak := range []string{"scan-0", "approved baseline", "tester", token} {
		if strings.Contains(got, leak) {
			t.Errorf("the shared PDF contains %q", leak)
		}
	}

	if resp := f.get("/shared/site/reject/scan-0/pdf?t=" + token); resp.StatusCode == http.StatusOK {
		t.Error("a share link opened another scan's PDF")
	}
}

// The scan ID links to the signed-in result page, from a shared view too,
// and only with a configured address (Story 5.39, AC16).
func TestPDFLinksTheScanIDToItsPage(t *testing.T) {
	t.Parallel()

	const page = `<a href="https://wsaw.example.com/results/site/reject/scan-1">`

	signer := shareSigner(t)
	f := newFixtureWith(t, httpapi.Options{
		WebUI: true, Share: signer, BaseURL: "https://wsaw.example.com/",
	}, nil, nil, withPrinter(&echoPrinter{}))
	seedComparedPair(t, f)

	if got := body(t, f.get("/api/v1/results/site/reject/scan-1/pdf")); !strings.Contains(got, page) {
		t.Error("the PDF does not link the scan ID to its page")
	}

	link := linkFor(t, signer, "site", "reject", "scan-1")
	path, token, _ := strings.Cut(link, "?t=")

	shared := body(t, f.get(path+"/pdf?t="+token))
	if !strings.Contains(shared, page) || strings.Contains(shared, token) {
		t.Error("the shared PDF does not link to the signed-in page, or carries the share token")
	}

	plain := newFixtureWith(t, httpapi.Options{}, nil, nil, withPrinter(&echoPrinter{}))
	seedComparedPair(t, plain)

	if got := body(t, plain.get("/api/v1/results/site/reject/scan-1/pdf")); strings.Contains(got, "<a href") {
		t.Error("the PDF links somewhere without api.baseUrl")
	}
}
