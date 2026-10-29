package report_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// fakePrinter records what it was asked to print and answers with a minimal
// PDF, so everything up to the browser is tested without one.
type fakePrinter struct {
	html   string
	footer string
	out    []byte
	err    error
}

func (f *fakePrinter) PrintPDF(_ context.Context, html []byte, footer string) ([]byte, error) {
	f.html, f.footer = string(html), footer

	if f.out == nil && f.err == nil {
		return []byte("%PDF-1.7 fake"), nil
	}

	return f.out, f.err
}

func printPDF(t *testing.T, res *model.Result, rep *diff.Report, opts report.PDFOptions) *fakePrinter {
	t.Helper()

	p := &fakePrinter{}

	var out bytes.Buffer
	if err := report.WritePDF(t.Context(), &out, p, res, rep, opts); err != nil {
		t.Fatalf("WritePDF: %v", err)
	}

	if !strings.HasPrefix(out.String(), "%PDF-") {
		t.Fatalf("WritePDF wrote %q, want the printer's PDF", out.String())
	}

	return p
}

// A reader must learn whether the scan can be believed before any finding
// (Story 5.39, AC2): each statement comes before the first section.
func TestPDFLeadsWithWhyTheScanCannotBeTrusted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.Result)
		want   string
	}{
		{"failed", func(r *model.Result) {
			r.Termination, r.Error = model.TermError, "navigation failed"
		}, "This scan did not complete."},
		{"skipped", func(r *model.Result) { r.Termination = model.TermSkipped }, "This scan did not complete."},
		{"truncated", func(r *model.Result) { r.Termination = model.TermTimeout }, "This scan stopped early"},
		{"consent unverified", func(r *model.Result) {
			r.Consent.Outcome = model.OutcomeUnverified
		}, "Consent state not confirmed:"},
		{"low confidence", func(r *model.Result) {
			r.Confidence = &model.Confidence{Score: 59, Band: model.ConfidenceLow}
		}, "Low confidence"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := fixture()
			tt.mutate(res)

			html := printPDF(t, res, nil, report.PDFOptions{}).html

			at, first := strings.Index(html, tt.want), strings.Index(html, "<h2>")
			if at < 0 || at > first {
				t.Errorf("%q is at %d, want it before the first section at %d", tt.want, at, first)
			}
		})
	}
}

// A failed scan that recorded nothing must not read as a page that loads
// nothing (AC2, Tenet 5).
func TestAFailedScanWithNoRequestsIsNotAnEmptyPage(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Termination, res.Requests = model.TermError, nil

	html := printPDF(t, res, nil, report.PDFOptions{}).html

	if strings.Contains(html, "<p>No network requests were recorded.</p>") ||
		!strings.Contains(html, "says nothing about what the page loads") {
		t.Error("a failed scan's empty host table reads as a page that loads nothing")
	}
}

func TestPDFOfACleanScanHasNoCaveat(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Confidence = &model.Confidence{Score: 100, Band: model.ConfidenceHigh}

	html := printPDF(t, res, nil, report.PDFOptions{}).html

	for _, caveat := range []string{"did not complete", "stopped early", "not confirmed", "Low confidence"} {
		if strings.Contains(html, caveat) {
			t.Errorf("a clean scan's report says %q", caveat)
		}
	}
}

var (
	htmlHeading = regexp.MustCompile(`<h[123]>(.*?)</h[123]>`)
	mdHeading   = regexp.MustCompile(`(?m)^#{1,3} (.*)$`)
)

// One document drives both formats, so their sections cannot drift (AC1).
func TestPDFAndMarkdownHaveTheSameSections(t *testing.T) {
	t.Parallel()

	res := fixture()
	rep := &diff.Report{Comparable: true, BaselineScanID: "scan-old", Changes: []diff.Change{
		{Severity: diff.SeverityHigh, Type: diff.HostAdded, Subject: "ads.test"},
	}}

	var md strings.Builder
	if err := report.WriteMarkdown(&md, res, rep, report.BasisBaseline); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}

	html := printPDF(t, res, rep, report.PDFOptions{Basis: report.BasisBaseline}).html

	var fromHTML, fromMD []string
	for _, m := range htmlHeading.FindAllStringSubmatch(html, -1) {
		fromHTML = append(fromHTML, m[1])
	}

	for _, m := range mdHeading.FindAllStringSubmatch(md.String(), -1) {
		fromMD = append(fromMD, m[1])
	}

	if strings.Join(fromHTML, "\n") != strings.Join(fromMD, "\n") {
		t.Errorf("sections differ\nPDF:      %q\nMarkdown: %q", fromHTML, fromMD)
	}

	if !strings.Contains(html, "Compared against the approved baseline, scan <code>scan-old</code>") {
		t.Error("the PDF does not name the baseline it was compared against")
	}
}

// Page-controlled text is escaped wherever it lands (AC5).
func TestPDFEscapesPageControlledText(t *testing.T) {
	t.Parallel()

	const hostile = `<script>alert(1)</script>"</style><img src=x>`

	res := fixture()
	res.Requests[1].Domain = hostile
	res.Cookies[0].Name = hostile
	res.Consent.CMP = hostile
	res.Consent.BannerHeading = hostile
	res.Target = hostile

	p := printPDF(t, res, nil, report.PDFOptions{})

	for name, out := range map[string]string{"page": p.html, "footer": p.footer} {
		if strings.Contains(out, "<script") || strings.Contains(out, "<img src=x") {
			t.Errorf("the %s carries page-controlled markup unescaped", name)
		}
	}

	if n := strings.Count(p.html, "</style>"); n != 1 {
		t.Errorf("the page closes a style element %d times, want only its own one", n)
	}
}

// The PDF can be traced back to the stored result (AC3).
func TestPDFNamesItsResult(t *testing.T) {
	t.Parallel()

	res := fixture()

	var doc bytes.Buffer
	if err := report.WriteJSON(&doc, res, nil); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	sum := sha256.Sum256(doc.Bytes())
	digest := hex.EncodeToString(sum[:])

	p := printPDF(t, res, nil, report.PDFOptions{Version: "9.9.9", JSONPath: "/api/v1/results/example/reject/scan-abc?bodies=false"})

	for _, want := range []string{digest, model.SchemaVersion, "/api/v1/results/example/reject/scan-abc?bodies=false"} {
		if at, first := strings.Index(p.html, want), strings.Index(p.html, "<h2>"); at < 0 || at > first {
			t.Errorf("first page does not carry %q", want)
		}
	}

	for _, want := range []string{"example", "reject", "scan-abc", "2026-08-31T10:00:00Z", "9.9.9", `class="pageNumber"`, `class="totalPages"`} {
		if !strings.Contains(p.footer, want) {
			t.Errorf("footer lacks %q: %s", want, p.footer)
		}
	}
}

func TestWritePDFNeverWritesAFailedRender(t *testing.T) {
	t.Parallel()

	tests := map[string]*fakePrinter{
		"printer error": {err: errors.New("deadline exceeded")},
		"not a PDF":     {out: []byte("<html>")},
	}

	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			if err := report.WritePDF(t.Context(), &out, p, fixture(), nil, report.PDFOptions{}); err == nil {
				t.Fatal("WritePDF succeeded")
			}

			if out.Len() != 0 {
				t.Errorf("WritePDF wrote %d bytes after a failed render", out.Len())
			}
		})
	}
}

func TestPDFEmbedsScreenshotsOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Screenshots = []model.Artifact{
		{Kind: "screenshot-before-consent", Ref: "screenshot/png"},
		{Kind: "screenshot-after-consent", Ref: "screenshot/other"},
	}

	if html := printPDF(t, res, nil, report.PDFOptions{}).html; strings.Contains(html, "Screenshots") {
		t.Error("screenshots were added without being asked for")
	}

	load := func(ref string) ([]byte, error) {
		if ref == "screenshot/png" {
			return []byte("\x89PNG\r\n\x1a\nrest"), nil
		}

		return []byte("GIF89a"), nil
	}

	html := printPDF(t, res, nil, report.PDFOptions{Screenshots: load}).html

	if !strings.Contains(html, `src="data:image/png;base64,`) {
		t.Error("the PNG screenshot is not embedded as a data: URI")
	}

	if !strings.Contains(html, "<code>screenshot-after-consent</code> is not a PNG") {
		t.Error("a screenshot that is not a PNG was dropped without saying so")
	}
}
