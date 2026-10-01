package capture

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/network"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Storing every body a scan sees (Story 1.11).
//
// Capture decides nothing about sampling: the scanner passes BodyStore none for
// a scan that is not in the sample, so an unsampled scan costs exactly what a
// scan cost before this story — the same buffers, the same reads, the same
// digests. What capture owns is reading and storing, under caps, and saying
// for every request that should have yielded a body why it did not.

// bodyQueueSize bounds how many bodies can wait to be read at once. A page
// that finishes more requests than this in the moment between two reads gets
// the overflow recorded as unavailable rather than silently skipped.
const bodyQueueSize = 1024

// bodyWorkers is how many bodies are read at once. Reads are CDP round trips
// to one browser, so more workers buy little and risk the evictions they are
// racing against.
const bodyWorkers = 4

// bodyStorage is what a scan stores beyond the digests it always takes.
type bodyStorage struct {
	mode          model.BodyStore
	requestBodies bool
	maxScanBytes  int64
	maxBodyBytes  int64
	// scrub replaces credentials wsaw supplied itself, for request payloads.
	scrub func(string) string
}

// legacyBodyStorage is what the recorder's storeBodies flag has always meant.
func legacyBodyStorage(storeBodies bool) bodyStorage {
	if storeBodies {
		return bodyStorage{mode: model.BodyStoreHashed}
	}

	return bodyStorage{mode: model.BodyStoreNone}
}

// stores reports whether this scan keeps any body at all.
func (b bodyStorage) stores() bool {
	return b.mode == model.BodyStoreHashed || b.mode == model.BodyStoreAll
}

// configureBodies sets the scan's body storage. It is called once, before the
// first event can arrive.
func (r *recorder) configureBodies(b bodyStorage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if b.mode == "" {
		b.mode = model.BodyStoreNone
	}

	r.bodies = b
}

// planBodyLocked decides whether a hop's body is read, and whether it carries
// a digest. Callers hold r.mu.
func (r *recorder) planBodyLocked(rec *record) {
	if rec.req.NonNetwork {
		return
	}

	if _, ok := r.hashTypes[rec.req.ResourceType]; ok {
		rec.hashed = true
		rec.wantBody = true
	}

	if r.bodies.mode == model.BodyStoreAll && rec.req.ResourceType != resourceWebSocket {
		rec.wantBody = true
	}
}

const resourceWebSocket = "websocket"

// bodyPlan reports, for a body the worker is about to read, whether it is
// fingerprinted and whether it is stored.
func (r *recorder) bodyPlan(id network.RequestID) (hashed, store bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.current[id]
	if !ok {
		return false, false
	}

	switch r.bodies.mode {
	case model.BodyStoreAll:
		store = true
	case model.BodyStoreHashed:
		store = rec.hashed
	}

	return rec.hashed, store
}

// reserveBodyBytes claims n bytes of the scan's body budget, and reports
// false once the budget would be exceeded. A body that does not fit is not
// stored, and the budget stays spent: later, smaller bodies are not squeezed
// in after a larger one was refused, because what a scan stores must not
// depend on the order Chrome happened to finish requests in.
func (r *recorder) reserveBodyBytes(n int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.budgetExhausted {
		return false
	}

	if r.bodies.maxScanBytes > 0 && r.storedBodyBytes+n > r.bodies.maxScanBytes {
		r.budgetExhausted = true

		return false
	}

	r.storedBodyBytes += n

	return true
}

// bodyResult is what reading one response body produced.
type bodyResult struct {
	digest      string
	size        int64
	ref         string
	storedSize  int64
	unavailable string
}

// setBody attaches what reading a response body produced. A digest can be
// present together with a reason the body was not stored: the fingerprint
// was taken, and only keeping the bytes failed.
func (r *recorder) setBody(id network.RequestID, b bodyResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.current[id]
	if !ok {
		return
	}

	if b.unavailable != "" {
		rec.req.BodyUnavailable = b.unavailable
	}

	if b.digest != "" {
		rec.req.BodySHA256 = b.digest
	}

	if b.ref != "" {
		rec.req.BodyRef = b.ref
		rec.req.BodyStoredSize = b.storedSize
	}

	if rec.req.DecodedSize == 0 && b.size > 0 {
		rec.req.DecodedSize = b.size
	}
}

// payloadResult is what storing one request payload produced.
type payloadResult struct {
	// wanted is false when the request carries no payload to store.
	wanted bool
	// fetch means the payload was not in the event and must be asked for.
	fetch bool

	ref         string
	digest      string
	size        int64
	mimeType    string
	redacted    bool
	unavailable string
}

// inlinePayload stores a request's payload when the event already carries
// it, and otherwise reports that it has to be fetched.
func (r *recorder) inlinePayload(ev *network.EventRequestWillBeSent) payloadResult {
	if ev.Request == nil || !ev.Request.HasPostData || isNonNetwork(ev.Request.URL) {
		return payloadResult{}
	}

	r.mu.Lock()
	// A request past the request cap is not recorded, so its payload would
	// be stored for nothing and charged to the budget.
	wanted := r.bodies.stores() && r.bodies.requestBodies && r.exceeded == capNone
	r.mu.Unlock()

	if !wanted {
		return payloadResult{}
	}

	out := payloadResult{wanted: true, mimeType: headerValue(ev.Request.Headers, "Content-Type")}

	if len(ev.Request.PostDataEntries) == 0 {
		out.fetch = true

		return out
	}

	var data []byte

	for _, entry := range ev.Request.PostDataEntries {
		chunk, err := base64.StdEncoding.DecodeString(entry.Bytes)
		if err != nil {
			// The entries are Chrome's own encoding, so a bad one means the
			// event cannot be trusted for the payload; asking for it again is
			// the way to get the real bytes.
			out.fetch = true

			return out
		}

		data = append(data, chunk...)
	}

	stored := r.storePayload(data)
	stored.wanted, stored.mimeType = true, out.mimeType

	return stored
}

// storePayload redacts and stores one request payload, under the caps.
func (r *recorder) storePayload(data []byte) payloadResult {
	r.mu.Lock()
	b, sink := r.bodies, r.bodySink
	r.mu.Unlock()

	out := payloadResult{size: int64(len(data))}

	if b.maxBodyBytes > 0 && int64(len(data)) > b.maxBodyBytes {
		out.unavailable = fmt.Sprintf("%s %d byte cap", model.BodyReasonTooLarge, b.maxBodyBytes)

		return out
	}

	// A credential wsaw supplied itself — basic auth, a header value, proxy
	// credentials — can be echoed into a payload by the page. It is replaced
	// before anything is stored, and the request says so. Nothing the page
	// chose is ever touched: redacting page data would falsify the evidence.
	if b.scrub != nil {
		if scrubbed := b.scrub(string(data)); scrubbed != string(data) {
			data = []byte(scrubbed)
			out.redacted = true
		}
	}

	out.digest = sha256Hex(data)

	if sink == nil {
		out.unavailable = model.BodyReasonStoreFailed + ": no artifact store"

		return out
	}

	if !r.reserveBodyBytes(int64(len(data))) {
		out.unavailable = model.BodyReasonBudget

		return out
	}

	ref, err := sink("body", data)
	if err != nil {
		out.unavailable = model.BodyReasonStoreFailed + ": " + scrubWith(b.scrub, err.Error())

		return out
	}

	out.ref = ref

	return out
}

// applyPayloadLocked records a payload result on a hop. Callers hold r.mu.
func (r *recorder) applyPayloadLocked(rec *record, p payloadResult) {
	rec.wantPayload = true

	if p.mimeType != "" {
		rec.req.RequestBodyMimeType = p.mimeType
	}

	if p.fetch {
		return
	}

	rec.req.RequestBodyRef = p.ref
	rec.req.RequestBodySHA256 = p.digest
	rec.req.RequestBodySize = p.size
	rec.req.RequestBodyRedacted = p.redacted
	rec.req.RequestBodyUnavailable = p.unavailable
}

// setPayload attaches a payload the worker fetched.
func (r *recorder) setPayload(id network.RequestID, p payloadResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.current[id]
	if !ok {
		return
	}

	p.fetch = false
	if p.mimeType == "" {
		p.mimeType = rec.req.RequestBodyMimeType
	}

	r.applyPayloadLocked(rec, p)
}

// finishBodies gives every request a stored body or a recorded reason, in a
// scan that stores bodies (Story 1.11, AC11). It runs once, after the body
// workers have stopped, so nothing is still being read.
func (r *recorder) finishBodies() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.bodies.stores() {
		return
	}

	for _, rec := range r.records {
		if rec.req.NonNetwork {
			continue
		}

		if rec.wantPayload && rec.req.RequestBodyRef == "" && rec.req.RequestBodyUnavailable == "" {
			rec.req.RequestBodyUnavailable = model.BodyReasonScanEnded
		}

		relevant := r.bodies.mode == model.BodyStoreAll || rec.hashed
		if !relevant || rec.req.BodyRef != "" || rec.req.BodyUnavailable != "" {
			continue
		}

		rec.req.BodyUnavailable = noBodyReason(rec)
	}
}

// noBodyReason says why a request that should have yielded a body has none.
func noBodyReason(rec *record) string {
	req := &rec.req

	// A status that cannot carry a body is checked before a failure: Chrome
	// reports a 204 to a fetch nobody reads as failed once it drops the
	// stream, and "status 204" is the truer account of why there is no body.
	switch {
	case req.ResourceType == resourceWebSocket:
		return model.BodyReasonNoBody + ": websocket"
	case req.RedirectTo != "":
		return model.BodyReasonNoBody + ": redirect"
	case req.Status == 204 || req.Status == 304 || (req.Status >= 100 && req.Status < 200):
		return fmt.Sprintf("%s: status %d", model.BodyReasonNoBody, req.Status)
	case strings.EqualFold(req.Method, "HEAD"):
		return model.BodyReasonNoBody + ": HEAD request"
	case req.Failed:
		return model.BodyReasonNoBody + ": the request failed"
	default:
		// Still in flight, or finished after the workers stopped reading.
		return model.BodyReasonScanEnded
	}
}

// bodyStats summarises what the scan stored, for the result's bodyCapture.
func (r *recorder) bodyStats() *model.BodyCapture {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.bodies.stores() {
		return nil
	}

	out := &model.BodyCapture{
		Store:           r.bodies.mode,
		RequestBodies:   r.bodies.requestBodies,
		MaxBodyBytes:    r.bodies.maxBodyBytes,
		MaxScanBytes:    r.bodies.maxScanBytes,
		BudgetExhausted: r.budgetExhausted,
	}

	for _, rec := range r.records {
		if rec.req.BodyRef != "" {
			out.ResponseBodiesStored++
			out.ResponseBodyBytes += rec.req.BodyStoredSize
		} else if rec.req.BodyUnavailable != "" && !strings.HasPrefix(rec.req.BodyUnavailable, model.BodyReasonNoBody) {
			out.BodiesUnavailable++
		}

		if rec.req.RequestBodyRef != "" {
			out.RequestBodiesStored++
			out.RequestBodyBytes += rec.req.RequestBodySize
		} else if rec.req.RequestBodyUnavailable != "" {
			out.BodiesUnavailable++
		}
	}

	return out
}

// headerValue reads one header case-insensitively. CDP reports headers in
// whatever case the page or the network stack used.
func headerValue(h network.Headers, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}

	return ""
}

func scrubWith(scrub func(string) string, s string) string {
	if scrub == nil {
		return s
	}

	return scrub(s)
}
