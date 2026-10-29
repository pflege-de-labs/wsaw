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
			Funcs(template.FuncMap{"right": func(r []bool, i int) bool { return i < len(r) && r[i] }}).
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
		{bold("SHA-256 of the result document:"), text(" "), code(digest)},
	}

	if opts.JSONPath != "" {
		provenance = append(provenance, []span{text("The digest is over the JSON "), code("GET " + opts.JSONPath), text(" returns.")})
	}

	d.Blocks[1].Lines = append(d.Blocks[1].Lines, provenance...)

	if opts.Screenshots != nil {
		addScreenshots(d, res, opts.Screenshots)
	}

	var page bytes.Buffer
	if err := pdfTemplate.Execute(&page, struct {
		Title  string
		Blocks []block
	}{spansText(d.Blocks[0].Lines[0]), d.Blocks}); err != nil {
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
