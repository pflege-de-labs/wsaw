package model

import (
	"math"
	"strings"
	"time"
)

// BodyStore selects which bodies a scan keeps (Story 1.11).
type BodyStore string

// Body storage modes.
const (
	// BodyStoreNone keeps no body. It is the default, because bodies are
	// large and can carry personal data (Tenet 19).
	BodyStoreNone BodyStore = "none"
	// BodyStoreHashed keeps the bodies of the fingerprinted resource types,
	// which is what storeBodies: true has always meant.
	BodyStoreHashed BodyStore = "hashed"
	// BodyStoreAll keeps the body of every network request.
	BodyStoreAll BodyStore = "all"
)

// Valid reports whether s is one of the known modes.
func (s BodyStore) Valid() bool {
	switch s {
	case BodyStoreNone, BodyStoreHashed, BodyStoreAll:
		return true
	default:
		return false
	}
}

// BodyDecision is why a scan did or did not store bodies.
type BodyDecision string

// Body sampling decisions.
const (
	// BodyDecisionDisabled means body storage is not configured.
	BodyDecisionDisabled BodyDecision = "disabled"
	// BodyDecisionSampled means the ratio called for this scan.
	BodyDecisionSampled BodyDecision = "sampled"
	// BodyDecisionNotSampled means the ratio did not call for this scan.
	BodyDecisionNotSampled BodyDecision = "not-sampled"
	// BodyDecisionForced means an operator asked for this scan's bodies.
	BodyDecisionForced BodyDecision = "forced"
	// BodyDecisionLedgerUnavailable means the sampling ledger could not be
	// read, so a ratio between 0 and 1 could not be honoured and the scan
	// stored nothing. It is recorded so the gap is not mistaken for an
	// ordinary unsampled scan (Tenet 5).
	BodyDecisionLedgerUnavailable BodyDecision = "ledger-unavailable"
	// BodyDecisionDisabledByReload means a retry inherited a sampled
	// decision, but a reload had turned body storage off in between. Turning
	// off the collection of possibly personal data does not wait for a retry
	// to finish (Tenet 19).
	BodyDecisionDisabledByReload BodyDecision = "storage-disabled-by-reload"
)

// BodyCapture records the body storage a scan was configured with, whether it
// was sampled, and what it stored (Story 1.11). A scan without bodies can
// then be told apart as "not in the sample" rather than read as "no bodies
// could be read".
//
// Nil on a result whose scan never reached capture, and on one written before
// schema 2.2.
type BodyCapture struct {
	Store         BodyStore     `json:"store"`
	Ratio         float64       `json:"ratio"`
	RatioWindow   time.Duration `json:"ratioWindowNs,omitempty"`
	RequestBodies bool          `json:"requestBodies,omitempty"`

	Sampled  bool         `json:"sampled"`
	Forced   bool         `json:"forced,omitempty"`
	Decision BodyDecision `json:"decision"`
	// DecidedBy is the scan ID of the attempt that took the decision. A retry
	// inherits its first attempt's decision, so on a retry it names an
	// earlier scan.
	DecidedBy string `json:"decidedBy,omitempty"`

	// WindowScans and WindowSampled are the counts the decision was taken
	// from: the series' scans in the trailing window, this one included, and
	// how many of them were sampled, this one included if it was.
	WindowScans   int `json:"windowScans,omitempty"`
	WindowSampled int `json:"windowSampled,omitempty"`

	// MaxBodyBytes and MaxScanBytes are the caps the scan stored under.
	MaxBodyBytes int64 `json:"maxBodyBytes,omitempty"`
	MaxScanBytes int64 `json:"maxScanBytes,omitempty"`

	// What the scan stored, and how much it could not. BudgetExhausted is set
	// once the stored bytes reached MaxScanBytes, after which every further
	// body is recorded as unavailable for that reason.
	ResponseBodiesStored int   `json:"responseBodiesStored,omitempty"`
	ResponseBodyBytes    int64 `json:"responseBodyBytes,omitempty"`
	RequestBodiesStored  int   `json:"requestBodiesStored,omitempty"`
	RequestBodyBytes     int64 `json:"requestBodyBytes,omitempty"`
	BodiesUnavailable    int   `json:"bodiesUnavailable,omitempty"`
	BudgetExhausted      bool  `json:"budgetExhausted,omitempty"`
}

// Inherited reports whether this decision was taken by an earlier attempt.
func (b *BodyCapture) Inherited(scanID string) bool {
	return b != nil && b.DecidedBy != "" && b.DecidedBy != scanID
}

// SampleWanted is the sampling rule (Story 1.11, AC4): with scans observations
// in the window, this one included, and sampled of them already sampled, this
// scan is sampled when sampled < ratio × scans, rounded up.
//
// Rounding up is what guarantees one sampled scan per window for any ratio
// above zero: the first scan of an empty window always qualifies. The epsilon
// keeps a product that is an integer in decimal — 0.05 × 20 — from rounding up
// past it in binary.
func SampleWanted(ratio float64, scans, sampled int) bool {
	switch {
	case ratio <= 0:
		return false
	case ratio >= 1:
		return true
	}

	const epsilon = 1e-9

	return float64(sampled) < math.Ceil(ratio*float64(scans)-epsilon)
}

// Fixed reasons a body was not stored. A request's bodyUnavailable starts with
// one of them, optionally followed by ": " and detail, so the reasons can be
// counted while the detail stays readable. Page-controlled text only ever
// appears in the scrubbed detail, never as the reason itself.
const (
	// BodyReasonNoBody means the exchange carried no body to keep: a redirect
	// hop, a HEAD request, a 204 or 304, a request that failed before a
	// response, a WebSocket.
	BodyReasonNoBody = "no body"
	// BodyReasonNotRetrievable means Chrome could not hand the body over,
	// usually because it had already evicted it.
	BodyReasonNotRetrievable = "body not retrievable"
	// BodyReasonTooLarge means the body exceeded the per-body cap.
	BodyReasonTooLarge = "body larger than the"
	// BodyReasonBudget means the scan had already stored its maxScanBytes.
	BodyReasonBudget = "scan body budget exhausted"
	// BodyReasonScanEnded means the scan ended before the body was read.
	BodyReasonScanEnded = "scan ended before the body was read"
	// BodyReasonQueueFull means more bodies finished at once than could be
	// queued for reading.
	BodyReasonQueueFull = "body queue full"
	// BodyReasonStoreFailed means the body was read but could not be stored.
	BodyReasonStoreFailed = "storing the body failed"
)

// bodyReasonCodes maps each fixed reason to its metric label.
var bodyReasonCodes = []struct{ prefix, code string }{
	{BodyReasonNoBody, "no-body"},
	{BodyReasonNotRetrievable, "not-retrievable"},
	{BodyReasonTooLarge, "too-large"},
	{BodyReasonBudget, "budget-exhausted"},
	{BodyReasonScanEnded, "scan-ended"},
	{BodyReasonQueueFull, "queue-full"},
	{BodyReasonStoreFailed, "store-failed"},
}

// BodyReasonCodes lists every label BodyReasonCode can return, so a metric
// can be rendered with a known, bounded label set.
func BodyReasonCodes() []string {
	out := make([]string, 0, len(bodyReasonCodes)+1)
	for _, r := range bodyReasonCodes {
		out = append(out, r.code)
	}

	return append(out, "other")
}

// BodyReasonCode reduces a bodyUnavailable text to its fixed reason, for a
// metric label that cannot grow without bound.
func BodyReasonCode(unavailable string) string {
	for _, r := range bodyReasonCodes {
		if strings.HasPrefix(unavailable, r.prefix) {
			return r.code
		}
	}

	return "other"
}
