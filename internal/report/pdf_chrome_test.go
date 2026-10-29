package report_test

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// chromePool starts one browser for a test, or skips the test, saying why,
// where there is no Chrome (Tenet 13).
func chromePool(t *testing.T) *browser.Pool {
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

	pool := browser.NewPool(browser.PoolOptions{
		Size:   1,
		Launch: browser.Options{Info: info, ProfileDir: t.TempDir(), LaunchTimeout: 40 * time.Second},
	})

	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the pool: %v", err)
		}
	})

	return pool
}

// The whole path, template to Chrome, on a fixture result.
func TestPDFRendersInChrome(t *testing.T) {
	t.Parallel()

	pool := chromePool(t)

	res := fixture()
	res.Confidence = &model.Confidence{Score: 59, Band: model.ConfidenceLow, Reasons: []model.ConfidenceReason{
		{Signal: model.SignalFetches, Points: 41, Observed: "8 of 114 requests not observed"},
	}}
	res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: "screenshot/1"}}

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4" +
		"\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

	var out bytes.Buffer

	err := report.WritePDF(t.Context(), &out, browser.NewPrinter(pool, 0), res, nil, report.PDFOptions{
		Version:     "test",
		Screenshots: func(string) ([]byte, error) { return png, nil },
	})
	if err != nil {
		t.Fatalf("WritePDF: %v", err)
	}

	if !bytes.HasPrefix(out.Bytes(), []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF-")
	}
}

// wideFixture has the shapes that broke the first real report: a long
// query-string URL, a storage change, a seven-digit byte count and a full
// hosts table.
func wideFixture() (*model.Result, *diff.Report) {
	res := fixture()
	res.Requests[0].TransferSize = 2271497
	res.Requests[1].Domain = "otlp-http-production.shopifysvc.com"
	res.Storage = []model.StorageEntry{{
		Origin: "https://shop.staging.example.io", Area: model.StorageSession,
		Key: "wpm-ri-1744cf0d-21ea-40f3-b23d-b6e6a82d5a6a-1793272159-z", Party: model.FirstParty, ValueLength: 13,
	}}

	rep := &diff.Report{Comparable: true, BaselineScanID: "scan-old", Changes: []diff.Change{
		{
			Severity: diff.SeverityLow, Type: diff.StorageRemoved,
			Subject: "https://shop.staging.example.io|session:wpm-ri-1d349cd3-a274-4be6-95cf-3d56f1899c43-1793267656-z",
			Detail:  `sessionStorage key "wpm-ri-1d349cd3-a274-4be6-95cf-3d56f1899c43-1793267656-z" for https://shop.staging.example.io is no longer set`,
		},
		{Severity: diff.SeverityInfo, Type: diff.AssetAdded, Subject: longURL, Detail: "new document asset loaded from shop.example"},
	}}

	return res, rep
}

// layoutPrinter lays the report out in Chrome at the content width of a
// portrait A4 page, the narrowest a reader may print it at, and counts the
// lines of every header and compact cell. It prints nothing.
type layoutPrinter struct {
	pool *browser.Pool

	// wrapped names each compact cell laid out on more than one line; long
	// is how many lines the longest wide cell took, to show the measurement
	// can see a wrap at all.
	wrapped []string
	long    int
}

// portraitContent is A4's 210 mm less the stylesheet's 2 × 14 mm margins,
// in CSS pixels.
const portraitContent = 182 * 96 / 25.4

func (p *layoutPrinter) PrintPDF(ctx context.Context, html []byte, _ string) ([]byte, error) {
	lease, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	tab, closeTab, err := lease.Browser.NewScanContext(ctx)
	if err != nil {
		return nil, err
	}
	defer closeTab()

	// Layout is measured through CDP; page JavaScript stays off, as it is
	// when the report is printed (Story 5.39, AC5).
	if _, err := chromedp.Call(tab, emulation.SetScriptExecutionDisabled,
		emulation.SetScriptExecutionDisabledParams{Value: true}); err != nil {
		return nil, err
	}

	if _, err := chromedp.Call(tab, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width: int64(math.Round(portraitContent)), Height: 1100, DeviceScaleFactor: 1,
	}); err != nil {
		return nil, err
	}

	if _, err := chromedp.Call(tab, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{Media: "print"}); err != nil {
		return nil, err
	}

	tree, err := chromedp.Call(tab, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return nil, err
	}

	if _, err := chromedp.Call(tab, page.SetDocumentContent, page.SetDocumentContentParams{
		FrameID: tree.FrameTree.Frame.ID, HTML: string(html),
	}); err != nil {
		return nil, err
	}

	doc, err := chromedp.Call(tab, dom.GetDocument, dom.GetDocumentParams{Depth: new(int64(-1))})
	if err != nil {
		return nil, err
	}

	var walk func(n *cdp.Node) error

	walk = func(n *cdp.Node) error {
		if n.NodeName == "TH" || n.NodeName == "TD" {
			lines, err := lineCount(tab, n)
			if err != nil {
				return err
			}

			if slices.Contains(strings.Fields(attr(n, "class")), "compact") || n.NodeName == "TH" {
				if lines > 1 {
					p.wrapped = append(p.wrapped, fmt.Sprintf("%s %q on %d lines", n.NodeName, nodeText(n), lines))
				}
			} else {
				p.long = max(p.long, lines)
			}

			return nil
		}

		for _, c := range n.Children {
			if err := walk(c); err != nil {
				return err
			}
		}

		return nil
	}

	if err := walk(doc.Root); err != nil {
		return nil, err
	}

	return []byte("%PDF-1.7 layout only"), nil
}

// lineCount is how many distinct lines the text inside n sits on.
func lineCount(tab context.Context, n *cdp.Node) (int, error) {
	tops := map[int]bool{}

	var visit func(c *cdp.Node) error

	visit = func(c *cdp.Node) error {
		if c.NodeType == textNode && strings.TrimSpace(c.NodeValue) != "" {
			q, err := chromedp.Call(tab, dom.GetContentQuads, dom.GetContentQuadsParams{BackendNodeID: c.BackendNodeID})
			if err != nil {
				return err
			}

			for _, quad := range q.Quads {
				tops[int(math.Round(min(quad[1], quad[3], quad[5], quad[7])))] = true
			}
		}

		for _, cc := range c.Children {
			if err := visit(cc); err != nil {
				return err
			}
		}

		return nil
	}

	err := visit(n)

	return len(tops), err
}

// textNode is the DOM's nodeType for text.
const textNode = 3

func attr(n *cdp.Node, name string) string {
	for i := 0; i+1 < len(n.Attributes); i += 2 {
		if n.Attributes[i] == name {
			return n.Attributes[i+1]
		}
	}

	return ""
}

func nodeText(n *cdp.Node) string {
	if n.NodeType == textNode {
		return n.NodeValue
	}

	var b strings.Builder
	for _, c := range n.Children {
		b.WriteString(nodeText(c))
	}

	return b.String()
}

// No column header and no short value breaks inside a word, even on a
// portrait page (AC10, AC15).
func TestPDFHeadersAndShortValuesStayOnOneLine(t *testing.T) {
	t.Parallel()

	res, rep := wideFixture()
	p := &layoutPrinter{pool: chromePool(t)}

	if err := report.WritePDF(t.Context(), &bytes.Buffer{}, p, res, rep, report.PDFOptions{}); err != nil {
		t.Fatalf("WritePDF: %v", err)
	}

	for _, w := range p.wrapped {
		t.Errorf("wraps: %s", w)
	}

	if p.long < 2 {
		t.Errorf("the longest wide cell is on %d line(s); the measurement cannot see a wrap", p.long)
	}
}

var mediaBox = regexp.MustCompile(`/MediaBox\s*\[\s*0 0 ([\d.]+) ([\d.]+)\s*\]`)

// The data tables are printed landscape, everything before them portrait,
// and the first page links the scan ID to its page (AC15, AC16).
func TestPDFTurnsLandscapeAndLinksInChrome(t *testing.T) {
	t.Parallel()

	res, rep := wideFixture()

	var out bytes.Buffer

	err := report.WritePDF(t.Context(), &out, browser.NewPrinter(chromePool(t), 0), res, rep, report.PDFOptions{
		Version: "test", ResultURL: report.ResultURL("https://wsaw.example.com", res),
	})
	if err != nil {
		t.Fatalf("WritePDF: %v", err)
	}

	if dir := os.Getenv("WSAW_PDF_OUT"); dir != "" {
		if err := os.WriteFile(dir+"/layout.pdf", out.Bytes(), 0o600); err != nil {
			t.Fatalf("keeping the PDF: %v", err)
		}
	}

	var orientation string

	for _, m := range mediaBox.FindAllSubmatch(out.Bytes(), -1) {
		if string(m[1]) < string(m[2]) {
			orientation += "P"
		} else {
			orientation += "L"
		}
	}

	if !regexp.MustCompile(`^P+L+$`).MatchString(orientation) {
		t.Errorf("pages are %q, want portrait pages followed only by landscape ones", orientation)
	}

	if want := "/URI (https://wsaw.example.com/results/example/reject/scan-abc)"; !bytes.Contains(out.Bytes(), []byte(want)) {
		t.Errorf("the PDF has no link annotation %s", want)
	}
}
