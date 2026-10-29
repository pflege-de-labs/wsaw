// Package report renders results and diffs into the formats humans and other
// systems consume.
//
// Every format here is a derived view: the JSON result schema is the real
// interface, and nothing appears in a Markdown report or a CSV that is not
// recoverable from the stored result (Tenet 16).
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// WriteJSON writes a result as indented JSON.
//
// load resolves stored response bodies and may be nil, in which case the
// result carries the references it was stored with and no bodies.
func WriteJSON(w io.Writer, res *model.Result, load BodyLoader) error {
	res = WithBodies(res, load)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("writing JSON result: %w", err)
	}

	return nil
}

// WriteJSONL writes one result per line, for streaming large runs.
func WriteJSONL(w io.Writer, load BodyLoader, results ...*model.Result) error {
	enc := json.NewEncoder(w)

	for _, res := range results {
		if err := enc.Encode(WithBodies(res, load)); err != nil {
			return fmt.Errorf("writing JSONL result: %w", err)
		}
	}

	return nil
}

// WriteCSV writes the host table, which is the view that goes into a
// spreadsheet during a compliance review.
func WriteCSV(w io.Writer, res *model.Result) error {
	cw := csv.NewWriter(w)

	header := []string{
		"target", "consent_mode", "consent_outcome", "scanned_at",
		"domain", "party", "requests", "pre_consent_requests", "bytes", "resource_types", "failed", "hosts",
	}

	if err := cw.Write(header); err != nil {
		return fmt.Errorf("writing CSV header: %w", err)
	}

	scannedAt := res.StartedAt.UTC().Format(time.RFC3339)

	for _, h := range res.HostSummaries() {
		row := []string{
			res.Target,
			string(res.ConsentMode),
			string(res.Consent.Outcome),
			scannedAt,
			h.Domain,
			string(h.Party),
			strconv.Itoa(h.Requests),
			strconv.Itoa(h.PreConsent),
			strconv.FormatInt(h.Bytes, 10),
			strings.Join(h.ResourceTypes, " "),
			strconv.Itoa(h.Failed),
			strings.Join(h.Hosts, " "),
		}

		if err := cw.Write(row); err != nil {
			return fmt.Errorf("writing CSV row: %w", err)
		}
	}

	cw.Flush()

	if err := cw.Error(); err != nil {
		return fmt.Errorf("flushing CSV: %w", err)
	}

	return nil
}
