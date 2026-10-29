package httpapi

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// pdfRetryAfter is what a client is told to wait when every print slot is
// busy: about one print's deadline.
var pdfRetryAfter = strconv.Itoa(int(browser.DefaultPrintTimeout.Seconds()))

func (s *Server) pdfRoutes() {
	s.mux.HandleFunc("GET /api/v1/results/{target}/{mode}/{scan}/pdf", s.handleResultPDF)
	s.mux.HandleFunc("GET /shared/{target}/{mode}/{scan}/pdf", s.handleSharedPDF)
}

// handleResultPDF serves one scan as a PDF report (Story 5.39).
func (s *Server) handleResultPDF(w http.ResponseWriter, r *http.Request) {
	res, ok := s.loadResult(w, r)
	if !ok {
		return
	}

	c := s.comparisonFor(res)

	jsonPath := "/api/v1/results/" + url.PathEscape(res.Target) + "/" + url.PathEscape(string(res.ConsentMode)) +
		"/" + url.PathEscape(res.ScanID) + "?bodies=false"

	s.writePDF(w, r, res, c.Report, c.basis(), jsonPath)
}

// handleSharedPDF serves the shared result as a PDF, holding nothing the
// shared page does not show (Story 5.19, AC3): the scan it was compared
// against is not named, as the page does not name it.
func (s *Server) handleSharedPDF(w http.ResponseWriter, r *http.Request) {
	res, _, ok := s.sharedResult(w, r)
	if !ok {
		return
	}

	rep := s.diffFor(res)
	if rep != nil {
		hidden := *rep
		hidden.BaselineScanID = ""
		rep = &hidden
	}

	// No token in the path: the PDF outlives the link it was fetched with
	// and may be forwarded, and a token is a credential.
	jsonPath := "/shared/" + url.PathEscape(res.Target) + "/" + url.PathEscape(string(res.ConsentMode)) +
		"/" + url.PathEscape(res.ScanID) + "/json?bodies=false"

	s.writePDF(w, r, res, rep, report.BasisUnknown, jsonPath)
}

// writePDF renders the report into memory first, so a render that fails
// leaves the client an error and not the first half of a PDF.
func (s *Server) writePDF(w http.ResponseWriter, r *http.Request, res *model.Result, rep *diff.Report,
	basis report.Basis, jsonPath string,
) {
	log := s.deps.Logger.With("scan_id", res.ScanID, "target", res.Target, "consent_mode", string(res.ConsentMode))

	if s.deps.Printer == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "PDF reports need a browser, and this server has none")

		return
	}

	opts := report.PDFOptions{Basis: basis, Version: s.opts.Version, JSONPath: jsonPath}

	// The signed-in page, from a shared view too: a PDF is forwarded and
	// outlives a share token, and the token is a credential (AC16). The
	// address is configured, never read from this request's headers.
	opts.ResultURL = report.ResultURL(s.opts.BaseURL, res)

	if r.URL.Query().Get("screenshots") == "true" {
		opts.Screenshots = s.deps.Store.GetArtifact
	}

	var pdf bytes.Buffer

	err := report.WritePDF(r.Context(), &pdf, s.deps.Printer, res, rep, opts)

	switch {
	case errors.Is(err, browser.ErrPrinterBusy):
		log.Warn("PDF report not rendered: no browser was free", "error", err)
		w.Header().Set("Retry-After", pdfRetryAfter)
		writeJSONError(w, http.StatusServiceUnavailable, "every browser is busy; try again shortly")

	case err != nil:
		log.Warn("PDF report not rendered", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "the PDF report could not be rendered")

	default:
		log.Debug("PDF report rendered", "bytes", pdf.Len())
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeFilename(res, "pdf")+`"`)

		if _, err := w.Write(pdf.Bytes()); err != nil {
			log.Debug("writing the PDF report", "error", err)
		}
	}
}
