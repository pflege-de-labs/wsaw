// Package model defines the wsaw result schema.
//
// The schema is the product's real interface (Tenet 16): human-readable
// reports and the web interface are derived views over these types. Changes
// here are additive by default; removing or repurposing a field is a breaking
// change (see AGENTS.md §8).
package model

import (
	"time"
)

// SchemaVersion is the version of the result schema produced by this build.
// Additive changes do not bump the major version.
const SchemaVersion = "1.0"

// ConsentMode is the consent state a scan was performed in. It is part of a
// result's identity: results are never compared across modes.
type ConsentMode string

// Consent modes.
const (
	// ConsentNone performs no interaction with a consent banner.
	ConsentNone ConsentMode = "none"
	// ConsentReject expresses rejection of all purposes and vendors.
	ConsentReject ConsentMode = "reject"
	// ConsentAccept expresses consent to all purposes and vendors.
	ConsentAccept ConsentMode = "accept"
)

// Valid reports whether m is a known consent mode.
func (m ConsentMode) Valid() bool {
	switch m {
	case ConsentNone, ConsentReject, ConsentAccept:
		return true
	default:
		return false
	}
}

// ConsentOutcome records what actually happened when wsaw tried to reach the
// requested consent state. A failed interaction must never look like a clean
// scan (Tenet 5), so this is always populated.
type ConsentOutcome string

// Consent outcomes.
const (
	// OutcomeApplied means the interaction was performed and verified.
	OutcomeApplied ConsentOutcome = "applied"
	// OutcomeNotNeeded means no consent interaction was required, either
	// because the mode was "none" or because no CMP was present.
	OutcomeNotNeeded ConsentOutcome = "not-needed"
	// OutcomeFailed means the interaction was attempted and did not succeed.
	OutcomeFailed ConsentOutcome = "failed"
	// OutcomeUnverified means the interaction was performed but its effect
	// could not be confirmed.
	OutcomeUnverified ConsentOutcome = "unverified"
)

// TerminationReason records why capture stopped. Determinism requires that
// truncation is always visible in the result (Tenet 6).
type TerminationReason string

// Termination reasons.
const (
	// TermIdle means the network went quiet for the configured period.
	TermIdle TerminationReason = "idle"
	// TermTimeout means the hard wall-clock budget was exhausted.
	TermTimeout TerminationReason = "timeout"
	// TermRequestCap means the maximum request count was reached.
	TermRequestCap TerminationReason = "request-cap"
	// TermByteCap means the maximum transferred bytes were reached.
	TermByteCap TerminationReason = "byte-cap"
	// TermError means capture ended because of an error.
	TermError TerminationReason = "error"
	// TermSkipped means the scan was not performed at all, e.g. disallowed
	// by robots.txt. This is a recorded outcome, not an error.
	TermSkipped TerminationReason = "skipped"
)

// ConsentPhase marks whether a request was observed before or after the
// consent interaction. Requests in the pre-interaction phase are the basis of
// pre-consent tracking findings.
type ConsentPhase string

// Consent phases.
const (
	// PhasePre covers requests observed before any consent interaction.
	PhasePre ConsentPhase = "pre-interaction"
	// PhasePost covers requests observed after the consent interaction.
	PhasePost ConsentPhase = "post-interaction"
)

// Party classifies a request relative to the scanned origin.
type Party string

// Parties.
const (
	// FirstParty is the scanned site's own registrable domain, plus any
	// domains the target declares as its own.
	FirstParty Party = "first"
	// ThirdParty is everything else.
	ThirdParty Party = "third"
)

// Result is one complete scan of one target in one consent mode. It is the
// unit of storage, export, and diffing.
type Result struct {
	SchemaVersion string `json:"schemaVersion"`

	ScanID string `json:"scanId"`
	// Target is the configured target name, stable across scans.
	Target string `json:"target"`
	// URL is the requested URL, before any redirect.
	URL string `json:"url"`
	// Labels are the target's configured labels, copied for filtering.
	Labels map[string]string `json:"labels,omitempty"`

	ConsentMode ConsentMode `json:"consentMode"`
	Consent     Consent     `json:"consent"`

	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	Duration   time.Duration `json:"durationNs"`

	Termination TerminationReason `json:"termination"`
	// Error is set when the scan failed. A non-empty Error means the asset
	// list is incomplete and must not be read as "nothing was loaded".
	Error string `json:"error,omitempty"`

	// Attempt is which attempt of a retried scan produced this result,
	// counting from 1, and Attempts is how many were allowed. They are
	// recorded so a stored result explains itself: a reader looking at a
	// success at 03:05 can see that 03:04 failed without going to find the
	// matching log lines (Story 3.8).
	Attempt  int `json:"attempt,omitempty"`
	Attempts int `json:"attempts,omitempty"`
	// PreviousError is why the attempt before this one failed. Empty on a
	// first attempt.
	PreviousError string `json:"previousError,omitempty"`

	// Superseded marks a failed result that a later attempt replaced. The
	// failure stays in the history — a retry is not a way to make a bad scan
	// disappear (Tenet 5) — but a superseded one is not what the next scan is
	// compared against.
	Superseded bool `json:"superseded,omitempty"`

	// FinalURL is the document URL after redirects, if the document loaded.
	FinalURL string `json:"finalUrl,omitempty"`

	Environment Environment `json:"environment"`

	Requests []Request `json:"requests"`
	Cookies  []Cookie  `json:"cookies,omitempty"`

	// Screenshots holds evidence artifact references, when enabled.
	Screenshots []Artifact `json:"screenshots,omitempty"`

	// Warnings records non-fatal problems that a reader must see, such as
	// bodies that could not be captured.
	Warnings []string `json:"warnings,omitempty"`
}

// Environment records everything needed to reproduce a scan, including the
// browser build and every applied override.
type Environment struct {
	WsawVersion   string `json:"wsawVersion"`
	ChromeVersion string `json:"chromeVersion"`
	UserAgent     string `json:"userAgent"`

	ViewportWidth  int     `json:"viewportWidth"`
	ViewportHeight int     `json:"viewportHeight"`
	DeviceScale    float64 `json:"deviceScaleFactor"`
	Mobile         bool    `json:"mobile"`

	AcceptLanguage string   `json:"acceptLanguage,omitempty"`
	Timezone       string   `json:"timezone,omitempty"`
	Latitude       *float64 `json:"latitude,omitempty"`
	Longitude      *float64 `json:"longitude,omitempty"`

	// ExtraHeaders lists header names only. Values may be secrets and are
	// never serialized.
	ExtraHeaders []string `json:"extraHeaderNames,omitempty"`
	// BasicAuth reports whether basic auth was used, never the credentials.
	BasicAuth bool `json:"basicAuth"`
	// Proxy is the proxy host, with any credentials stripped.
	Proxy string `json:"proxy,omitempty"`

	WarmCache bool `json:"warmCache"`

	// BrowserRuntime is "local" when the browser ran as a host process, or
	// the container runtime that ran it. A result is only comparable with
	// another if a reader can see which rendered it.
	BrowserRuntime string `json:"browserRuntime,omitempty"`
	// BrowserImage is the container image, when containerised.
	BrowserImage string `json:"browserImage,omitempty"`
	// BrowserSandbox reports whether Chrome's own sandbox was active. In a
	// container the container is the boundary and the inner sandbox is
	// usually off, which is a material fact about the scan rather than an
	// implementation detail.
	BrowserSandbox bool `json:"browserSandbox"`

	// BrowserReused is true when this scan ran on a browser process that had
	// already served another scan. Isolation then rests on wsaw clearing
	// state rather than on the process boundary, which is weaker, so it is
	// recorded rather than left invisible.
	BrowserReused bool `json:"browserReused,omitempty"`
}

// Consent describes the CMP encountered and the interaction performed.
type Consent struct {
	Outcome ConsentOutcome `json:"outcome"`
	// Reason explains the outcome in operator-readable terms. Always set for
	// failed and unverified outcomes.
	Reason string `json:"reason,omitempty"`

	// CMP is the detected platform, empty when none was detected.
	CMP string `json:"cmp,omitempty"`
	// CMPVersion is the platform version where readable.
	CMPVersion string `json:"cmpVersion,omitempty"`
	// Detection names how the CMP was found, e.g. "tcf-api" or "selector".
	Detection string `json:"detection,omitempty"`

	// Mechanism names how the interaction was performed: "tcf-api", "gpp",
	// "vendor-api", "selector", or "heuristic".
	Mechanism string `json:"mechanism,omitempty"`
	// Heuristic is true when the mechanism was generic label matching, whose
	// results are less trustworthy and must be visible as such.
	Heuristic bool `json:"heuristic,omitempty"`

	// TCString and GPPString are recorded verbatim when available.
	TCString  string `json:"tcString,omitempty"`
	GPPString string `json:"gppString,omitempty"`

	// InteractedAt is when the consent action completed, used to split the
	// pre- and post-interaction phases.
	InteractedAt *time.Time `json:"interactedAt,omitempty"`
}

// Request is a single observed network fetch. Redirects are recorded as
// separate requests rather than collapsed into one.
type Request struct {
	// RequestID is Chrome's identifier, stable within one scan only.
	RequestID string `json:"requestId"`

	URL string `json:"url"`
	// NormalizedURL is the diffing key derived from URL. It is stored so a
	// reader sees exactly what was compared.
	NormalizedURL string `json:"normalizedUrl"`

	Method       string `json:"method"`
	ResourceType string `json:"resourceType"`

	Host string `json:"host"`
	// Domain is the registrable domain (eTLD+1).
	Domain string `json:"domain"`
	Party  Party  `json:"party"`

	Phase ConsentPhase `json:"phase"`

	Status     int    `json:"status,omitempty"`
	StatusText string `json:"statusText,omitempty"`
	MimeType   string `json:"mimeType,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	RemoteIP   string `json:"remoteIp,omitempty"`

	TransferSize int64 `json:"transferSize"`
	DecodedSize  int64 `json:"decodedSize"`

	FromCache bool `json:"fromCache,omitempty"`
	// NonNetwork marks data: and blob: URLs, which are recorded but did not
	// leave the browser.
	NonNetwork bool `json:"nonNetwork,omitempty"`

	// RedirectFrom is the URL this request was redirected from, if any.
	RedirectFrom string `json:"redirectFrom,omitempty"`
	// RedirectTo is the Location target when this request was itself a
	// redirect hop.
	RedirectTo string `json:"redirectTo,omitempty"`

	Initiator Initiator `json:"initiator"`

	// Failed and FailureReason record blocked and errored requests, which
	// are as important as successful ones.
	Failed        bool   `json:"failed,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	Blocked       bool   `json:"blocked,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`

	// BodySHA256 is the hex digest of the response body, for resource types
	// selected for hashing.
	BodySHA256 string `json:"bodySha256,omitempty"`
	// BodyUnavailable explains why a body that should have been hashed was
	// not, so a missing digest is never silently ambiguous.
	BodyUnavailable string `json:"bodyUnavailable,omitempty"`
	// BodyRef points at a stored body artifact, when body storage is on.
	BodyRef string `json:"bodyRef,omitempty"`

	// BodyIdentity is a stable identifier lifted out of the response body by
	// a configured rule, and BodyIdentityLabel names what it is.
	//
	// Some scripts rewrite their own body on every response — a tag manager
	// embeds experiment flags that change without the container changing — so
	// a digest reports a change on nearly every scan while the thing an
	// operator cares about sits inside the body as a version number. Storing
	// the extracted value alongside the digest lets a comparison use the
	// identity the script publishes about itself, and keeps the raw digest
	// available so a reader can still see what was hashed.
	BodyIdentity      string `json:"bodyIdentity,omitempty"`
	BodyIdentityLabel string `json:"bodyIdentityLabel,omitempty"`

	// Body carries the stored response body itself. It is present only in an
	// export — a HAR, or a downloaded result — and never in the stored
	// document, which keeps bodies outside it as content-addressed files
	// (Story 4.6, AC9). A result read back from the store therefore has
	// BodyRef and no Body; an exported one has both.
	Body string `json:"body,omitempty"`
	// BodyEncoding is empty when Body is the body as text, and "base64" when
	// the bytes are not text and had to be encoded.
	BodyEncoding string `json:"bodyEncoding,omitempty"`
	// BodyStoredSize is how many bytes were actually kept, which is smaller
	// than DecodedSize when the size cap truncated the body. Stating both is
	// the difference between a short body and a truncated one.
	BodyStoredSize int64 `json:"bodyStoredSize,omitempty"`

	Timing Timing `json:"timing"`
}

// Initiator records what caused a request, so a user can trace a third-party
// fetch back to the script that triggered it.
type Initiator struct {
	// Type is Chrome's initiator type: parser, script, preload, SignedExchange
	// or other.
	Type string `json:"type"`
	// URL is the initiating script or document URL, where reported.
	URL string `json:"url,omitempty"`
	// LineNumber is 1-based when reported.
	LineNumber int `json:"lineNumber,omitempty"`
	// Stack is the initiating script URLs, outermost last, when available.
	Stack []string `json:"stack,omitempty"`
}

// Timing holds the request timeline relative to the scan start.
type Timing struct {
	StartOffset time.Duration `json:"startOffsetNs"`
	TTFB        time.Duration `json:"ttfbNs,omitempty"`
	EndOffset   time.Duration `json:"endOffsetNs,omitempty"`
}

// Cookie is a cookie present after the scan completed.
type Cookie struct {
	Name     string    `json:"name"`
	Domain   string    `json:"domain"`
	Path     string    `json:"path"`
	Expires  time.Time `json:"expires,omitempty"`
	Session  bool      `json:"session"`
	SameSite string    `json:"sameSite,omitempty"`
	Secure   bool      `json:"secure"`
	HTTPOnly bool      `json:"httpOnly"`
	Party    Party     `json:"party"`
	// ValueSHA256 fingerprints the value without storing it, since cookie
	// values routinely contain identifiers (data minimization, Tenet 19).
	ValueSHA256 string `json:"valueSha256,omitempty"`
	ValueLength int    `json:"valueLength,omitempty"`
}

// Artifact references a stored evidence file.
type Artifact struct {
	// Kind is e.g. "screenshot-before-consent".
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	// SHA256 lets a reader verify the artifact was not altered.
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// OK reports whether the scan produced a trustworthy asset list. A result
// that is not OK must never be presented as "no assets found".
func (r *Result) OK() bool {
	return r.Error == "" && r.Termination != TermError && r.Termination != TermSkipped
}

// Truncated reports whether capture stopped before the page went idle, which
// means the asset list may be incomplete.
func (r *Result) Truncated() bool {
	switch r.Termination {
	case TermTimeout, TermRequestCap, TermByteCap:
		return true
	default:
		return false
	}
}

// captureFailures are the Chrome network errors that mean wsaw failed to
// observe a request, as opposed to the page or the site choosing not to
// complete it.
//
// The distinction matters because it decides whether a missing asset is a
// finding. A request that never opened a connection did not tell us anything
// about the site, and every asset it would have initiated is missing for a
// reason the site did not choose — so "this asset is gone" is unprovable.
// Whereas net::ERR_ABORTED is ordinary: it is what a beacon looks like when
// the page is torn down around it, and such a request routinely carries a
// status because the server did answer. Treating it as a capture defect would
// mark almost every accept-mode scan degraded.
//
// Errors that report a deliberate decision — blocked by a policy, by an
// extension, or by ORB — are likewise not defects: they are observations, and
// an accurate one.
var captureFailures = map[string]struct{}{
	"net::ERR_INSUFFICIENT_RESOURCES": {},
	"net::ERR_NAME_NOT_RESOLVED":      {},
	"net::ERR_NAME_RESOLUTION_FAILED": {},
	"net::ERR_NETWORK_CHANGED":        {},
	"net::ERR_INTERNET_DISCONNECTED":  {},
	"net::ERR_ADDRESS_UNREACHABLE":    {},
	"net::ERR_CONNECTION_CLOSED":      {},
	"net::ERR_CONNECTION_RESET":       {},
	"net::ERR_CONNECTION_REFUSED":     {},
	"net::ERR_CONNECTION_TIMED_OUT":   {},
	"net::ERR_CONNECTION_FAILED":      {},
	"net::ERR_SOCKET_NOT_CONNECTED":   {},
	"net::ERR_OUT_OF_MEMORY":          {},
	"net::ERR_TIMED_OUT":              {},
	"net::ERR_TOO_MANY_RETRIES":       {},
}

// IsCaptureFailure reports whether a request failure reason means wsaw could
// not observe the request, rather than that the site or page declined it.
func IsCaptureFailure(reason string) bool {
	_, ok := captureFailures[reason]

	return ok
}

// CaptureFailure reports whether this request failed for a reason that makes
// the observation incomplete rather than informative.
func (r *Request) CaptureFailure() bool {
	return !r.NonNetwork && r.Failed && IsCaptureFailure(r.FailureReason)
}

// ReasonNeverCompleted stands in for a failure reason on a request that was
// still in flight when capture ended. Chrome reported no error, because from
// its point of view nothing went wrong yet.
const ReasonNeverCompleted = "never reported completion"

// NeverCompleted reports whether this request was still in flight when
// capture stopped waiting: no status, no end offset, and no error.
//
// Idle detection deliberately stops waiting for such a request — a
// cross-origin iframe's completion events go to a separate browser target and
// would otherwise never arrive, running every scan of a page with an embedded
// widget to its hard timeout. The consequence is that a request which stalls
// early can leave the page half-built while the scan still ends on schedule,
// and nothing in the termination reason says so.
func (r *Request) NeverCompleted() bool {
	return !r.NonNetwork && !r.Failed && r.Status == 0 && r.Timing.EndOffset == 0
}

// Incomplete reports whether this request produced no usable observation,
// either because it failed in transit or because it never finished. Both mean
// the same thing for a comparison: whatever this request would have loaded is
// absent for a reason the site did not choose.
func (r *Request) Incomplete() bool {
	return r.CaptureFailure() || r.NeverCompleted()
}

// IncompleteObservations counts the requests wsaw could not observe. A
// non-zero count means the asset list is missing entries for reasons that
// have nothing to do with the site.
func (r *Result) IncompleteObservations() int {
	n := 0

	for i := range r.Requests {
		if r.Requests[i].Incomplete() {
			n++
		}
	}

	return n
}

// IncompleteRatio is the share of network requests that produced no usable
// observation. A ratio is used rather than a count because it is comparable
// across pages: ten lost requests mean something different on a page that
// issues twenty than on one that issues three hundred.
func (r *Result) IncompleteRatio() float64 {
	total, lost := 0, 0

	for i := range r.Requests {
		if r.Requests[i].NonNetwork {
			continue
		}

		total++

		if r.Requests[i].Incomplete() {
			lost++
		}
	}

	if total == 0 {
		return 0
	}

	return float64(lost) / float64(total)
}

// TopIncompleteReason returns the most frequent reason an observation was
// incomplete and how often it occurred, so a degradation report names the
// actual fault instead of saying only that something went wrong.
func (r *Result) TopIncompleteReason() (reason string, count int) {
	tally := make(map[string]int)

	for i := range r.Requests {
		req := &r.Requests[i]

		switch {
		case req.CaptureFailure():
			tally[req.FailureReason]++
		case req.NeverCompleted():
			tally[ReasonNeverCompleted]++
		}
	}

	// Ties break on the reason string so the same result always produces the
	// same report (Tenet 6).
	for candidate, n := range tally {
		if n > count || (n == count && candidate < reason) {
			reason, count = candidate, n
		}
	}

	return reason, count
}

// ThirdPartyDomains returns the sorted distinct registrable domains of
// third-party requests, optionally restricted to one consent phase. An empty
// phase means all phases.
func (r *Result) ThirdPartyDomains(phase ConsentPhase) []string {
	seen := make(map[string]struct{})

	for i := range r.Requests {
		req := &r.Requests[i]
		if req.Party != ThirdParty || req.NonNetwork {
			continue
		}

		if phase != "" && req.Phase != phase {
			continue
		}

		if req.Domain != "" {
			seen[req.Domain] = struct{}{}
		}
	}

	return sortedKeys(seen)
}
