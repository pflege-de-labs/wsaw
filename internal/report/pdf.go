package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The PDF report (Story 5.39) is the Markdown report's document, rendered as
// HTML through html/template and printed by the Chrome wsaw already runs. No
// PDF library: that would be a dependency, and the browser is already one.
//
// Every page-controlled string reaches the HTML as template data, so
// contextual escaping applies to all of it. The only value the template is
// told to trust is a screenshot's data: URI, which this package builds from
// bytes it has checked are a PNG.

//go:embed templates/pdf.html templates/footer.html
var pdfFiles embed.FS

var (
	pdfTemplate = template.Must(template.New("pdf.html").
			Funcs(template.FuncMap{"layout": layoutTable, "clip": clip}).
			ParseFS(pdfFiles, "templates/pdf.html"))
	footerTemplate = template.Must(template.ParseFS(pdfFiles, "templates/footer.html"))
)

// ErrNotPDF is returned when the printer hands back something that is not a
// PDF, so a caller never serves it as one.
var ErrNotPDF = errors.New("the printer did not return a PDF")

// Printer prints a self-contained HTML document as a PDF. footer is Chrome's
// page footer template, already escaped. The browser package implements it.
type Printer interface {
	PrintPDF(ctx context.Context, html []byte, footer string) ([]byte, error)
}

// PDFOptions configures a PDF report.
type PDFOptions struct {
	// Basis is what the diff was computed against.
	Basis Basis

	// Version is the wsaw that renders the report, for the page footer.
	Version string

	// Screenshots loads the scan's screenshots to embed them. Nil leaves them
	// out, which is the default because they make the file large.
	Screenshots BodyLoader

	// JSONPath is where a reader fetches the result document the first page's
	// digest is taken over, e.g. the API path with ?bodies=false. Empty leaves
	// the hint out.
	JSONPath string

	// ResultURL is the scan's page in the web interface, which the first
	// page links the scan ID to (Story 5.39, AC16). Empty means no link: an
	// invented one would be worse than none.
	ResultURL string
}

// WritePDF renders res as a PDF report through p.
//
// A render that fails returns an error and writes nothing: a partial report
// must not reach a reader as if it were the whole one.
func WritePDF(ctx context.Context, w io.Writer, p Printer, res *model.Result, rep *diff.Report, opts PDFOptions) error {
	html, footer, err := pdfPage(res, rep, opts)
	if err != nil {
		return err
	}

	pdf, err := p.PrintPDF(ctx, html, footer)
	if err != nil {
		return fmt.Errorf("printing the report of scan %s: %w", res.ScanID, err)
	}

	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return fmt.Errorf("printing the report of scan %s: %w", res.ScanID, ErrNotPDF)
	}

	if _, err := w.Write(pdf); err != nil {
		return fmt.Errorf("writing the PDF report: %w", err)
	}

	return nil
}

// pdfPage builds the HTML document and the page footer the printer gets.
func pdfPage(res *model.Result, rep *diff.Report, opts PDFOptions) ([]byte, string, error) {
	d := build(res, rep, opts.Basis)

	digest, err := documentDigest(res)
	if err != nil {
		return nil, "", err
	}

	// The first page ties the PDF to the stored result, so a reader holding
	// only the PDF can check it against the store (AC3). build always opens
	// with the title and the header lines.
	provenance := [][]span{
		{bold("Result schema:"), text(" " + res.SchemaVersion)},
		{bold("SHA-256 of the result document:"), text(" "), exact(digest)},
	}

	if opts.JSONPath != "" {
		provenance = append(provenance, []span{text("The digest is over the JSON "), exact("GET " + opts.JSONPath), text(" returns.")})
	}

	if opts.ResultURL != "" {
		linkScanID(d.Blocks[1].Lines, res.ScanID, opts.ResultURL)
	}

	d.Blocks[1].Lines = append(d.Blocks[1].Lines, provenance...)

	if opts.Screenshots != nil {
		addScreenshots(d, res, opts.Screenshots)
	}

	var page bytes.Buffer
	if err := pdfTemplate.Execute(&page, struct {
		Title     string
		Blocks    []block
		Landscape int
	}{spansText(d.Blocks[0].Lines[0]), d.Blocks, d.Landscape}); err != nil {
		return nil, "", fmt.Errorf("rendering the report page: %w", err)
	}

	var footer bytes.Buffer
	if err := footerTemplate.Execute(&footer, map[string]string{
		"Target":  res.Target,
		"Mode":    string(res.ConsentMode),
		"ScanID":  res.ScanID,
		"Scanned": res.StartedAt.UTC().Format(time.RFC3339),
		"Version": orNone(opts.Version),
	}); err != nil {
		return nil, "", fmt.Errorf("rendering the report footer: %w", err)
	}

	return page.Bytes(), footer.String(), nil
}

// ResultURL is the address of res's page in the web interface under base, or
// empty when no base is configured (Story 5.39, AC16).
func ResultURL(base string, res *model.Result) string {
	if base == "" {
		return ""
	}

	return strings.TrimRight(base, "/") + "/results/" + url.PathEscape(res.Target) + "/" +
		url.PathEscape(string(res.ConsentMode)) + "/" + url.PathEscape(res.ScanID)
}

func exact(s string) span { return span{Text: s, Code: true, Exact: true} }

// linkScanID turns the header's scan ID into a link to the scan's page, and
// prints the address beside it, because a printed or forwarded copy cannot
// be clicked.
func linkScanID(lines [][]span, scanID, page string) {
	for i, l := range lines {
		for j, s := range l {
			if s.Code && s.Text == scanID {
				l[j].Href = page
				lines[i] = append(l, text(" — "), span{Text: page, Href: page, Exact: true})

				return
			}
		}
	}
}

// clipAt is how long a page-controlled value may be before the PDF shortens
// it; clipHead and clipTail are how much of each end it keeps (Story 5.39,
// AC12). A login callback URL with a token in its query can otherwise fill
// half a page.
const (
	clipAt   = 200
	clipHead = 120
	clipTail = 60
)

// clipped is a span's text as the PDF prints it.
type clipped struct {
	// Parts is the text, split where the PDF may wrap it.
	Parts []string

	// Note says the text was shortened, and where the whole of it is.
	Note string
}

// clip shortens a long value in the middle, visibly. Only the PDF does
// this: the Markdown, the JSON and the stored result keep every value whole
// (Tenet 4), and a value the reader must type back is never cut.
func clip(s span) clipped {
	t := s.Text
	if s.PDFText != "" {
		t = s.PDFText
	}

	r := []rune(t)
	token := (s.Code || s.Untrusted) && !s.Exact

	var c clipped

	// wsaw's own prose — an error, a warning — is never cut: it may be the
	// line that says why the scan cannot be trusted (Tenet 5).
	if token && len(r) > clipAt {
		t = string(r[:clipHead]) + "…" + string(r[len(r)-clipTail:])
		c.Note = fmt.Sprintf(" (%d characters; full value in the result document)", len(r))
	}

	// Prose wraps at its spaces. Only a long token — a URL, a host, a key —
	// needs places to wrap that are not spaces.
	if token && len(r) > breakAt {
		c.Parts = breaks(t)
	} else {
		c.Parts = []string{t}
	}

	return c
}

// breakAt is the length from which a value gets wrap points; anything
// shorter fits a wide column as it is.
const breakAt = 30

// breaks splits text after the separators of a URL, a host or a key, so the
// PDF can wrap a long value there rather than anywhere (Story 5.39, AC10).
// The template puts a <wbr> between the parts; each part is still escaped.
func breaks(s string) []string {
	const separators = "/?&=.-_|:"

	var parts []string

	start := 0

	// After the last of a run of separators, so "https://" stays whole.
	for i := range len(s) - 1 {
		if strings.IndexByte(separators, s[i]) >= 0 && strings.IndexByte(separators, s[i+1]) < 0 {
			parts = append(parts, s[start:i+1])
			start = i + 1
		}
	}

	return append(parts, s[start:])
}

// pdfTable is a table block as the PDF lays it out.
type pdfTable struct {
	Cols []pdfCell
	Rows [][]pdfCell
}

// pdfCell is one cell: its class, and its content as lines.
type pdfCell struct {
	Class string
	Head  string
	Lines [][]span
}

// layoutTable gives every column its class, compact or wide (AC10), and
// folds a Lead column into the top of the cell before it (AC11).
func layoutTable(b block) pdfTable {
	var t pdfTable

	for _, c := range b.Cols {
		if c.Lead && len(t.Cols) > 0 {
			prev := &t.Cols[len(t.Cols)-1]
			prev.Head = c.Head + " / " + prev.Head
			prev.Class = colClass(column{Wide: true})

			continue
		}

		t.Cols = append(t.Cols, pdfCell{Class: colClass(c), Head: c.Head})
	}

	for _, row := range b.Rows {
		cells := make([]pdfCell, 0, len(t.Cols))

		for i, spans := range row {
			if i < len(b.Cols) && b.Cols[i].Lead && len(cells) > 0 {
				prev := &cells[len(cells)-1]
				prev.Lines = append([][]span{spans}, prev.Lines...)

				continue
			}

			cells = append(cells, pdfCell{Class: t.Cols[len(cells)].Class, Lines: [][]span{spans}})
		}

		t.Rows = append(t.Rows, cells)
	}

	return t
}

func colClass(c column) string {
	class := "compact"
	if c.Wide {
		class = "wide"
	}

	if c.Right {
		class += " num"
	}

	return class
}

// documentDigest is the SHA-256 of the result as WriteJSON writes it without
// bodies, which is byte for byte what the API serves with ?bodies=false.
func documentDigest(res *model.Result) (string, error) {
	var b bytes.Buffer
	if err := WriteJSON(&b, res, nil); err != nil {
		return "", err
	}

	sum := sha256.Sum256(b.Bytes())

	return hex.EncodeToString(sum[:]), nil
}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// addScreenshots appends the scan's screenshots as an appendix. One that
// cannot be loaded, or is not a PNG, is named and left out rather than
// silently dropped.
func addScreenshots(d *document, res *model.Result, load BodyLoader) {
	if len(res.Screenshots) == 0 {
		return
	}

	d.heading(2, "Screenshots")

	for _, a := range res.Screenshots {
		data, err := load(a.Ref)

		switch {
		case err != nil:
			d.para(text("Screenshot "), code(a.Kind), text(" could not be loaded: "+err.Error()))
		case !bytes.HasPrefix(data, pngSignature):
			d.para(text("Screenshot "), code(a.Kind), text(" is not a PNG and is not shown."))
		default:
			d.Blocks = append(d.Blocks, block{
				Kind: kindImage,
				Alt:  a.Kind,
				// Safe to trust: wsaw built this URI from bytes it checked
				// are a PNG, and base64 cannot carry markup or a quote.
				Src: template.URL("data:image/png;base64," + //nolint:gosec // see above
					base64.StdEncoding.EncodeToString(data)),
			})
		}
	}
}
