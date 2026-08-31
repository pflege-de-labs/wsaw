package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"

	"github.com/martint17r/wsaw/internal/classify"
	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/normalize"
)

// capReason reports which capture budget a recorder exhausted.
type capReason int

const (
	capNone capReason = iota
	capRequests
	capBytes
)

// recorder turns the stream of CDP network events into request records.
//
// Chrome reuses one request ID across a redirect chain, so the recorder keeps
// a slice of hops per ID: each redirect finalizes the current hop and starts a
// new one, which is what makes redirects visible as their own requests rather
// than collapsed into the final destination.
type recorder struct {
	mu sync.Mutex

	start      time.Time
	classifier *classify.Classifier
	normalizer *normalize.Normalizer

	hashTypes map[string]struct{}

	// records holds every hop in observation order.
	records []*record
	// current maps a live request ID to its latest hop.
	current map[network.RequestID]*record

	// inflight counts requests that have neither finished nor failed, which
	// drives idle detection.
	inflight int

	totalBytes  int64
	maxRequests int
	maxBytes    int64
	exceeded    capReason

	// phase is flipped to post-interaction once the consent hook returns.
	phase model.ConsentPhase

	// idle is signalled whenever inflight reaches zero, so the waiter does
	// not have to poll.
	idleSignal chan struct{}

	// bodyWanted receives request IDs whose body should be fingerprinted.
	bodyWanted chan network.RequestID

	warnings []string
}

// record is one request hop, plus the bookkeeping needed to finalize it.
type record struct {
	req model.Request

	finished bool
	// wantBody marks a hop selected for body hashing.
	wantBody bool
}

func newRecorder(start time.Time, c *classify.Classifier, n *normalize.Normalizer, hashTypes []string, maxRequests int, maxBytes int64) *recorder {
	types := make(map[string]struct{}, len(hashTypes))
	for _, t := range hashTypes {
		types[t] = struct{}{}
	}

	return &recorder{
		start:       start,
		classifier:  c,
		normalizer:  n,
		hashTypes:   types,
		current:     make(map[network.RequestID]*record),
		maxRequests: maxRequests,
		maxBytes:    maxBytes,
		phase:       model.PhasePre,
		idleSignal:  make(chan struct{}, 1),
		bodyWanted:  make(chan network.RequestID, 256),
	}
}

// setPhase marks subsequent requests as belonging to a different consent
// phase. Requests already in flight keep the phase they started in, which is
// the honest attribution: they were triggered before the interaction.
func (r *recorder) setPhase(p model.ConsentPhase) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.phase = p
}

func (r *recorder) offset(t *time.Time) time.Duration {
	if t == nil {
		return 0
	}

	d := t.Sub(r.start)
	if d < 0 {
		return 0
	}

	return d
}

// requestWillBeSent records a new hop. When the event carries a redirect
// response it first finalizes the previous hop for that ID.
func (r *recorder) requestWillBeSent(ev *network.EventRequestWillBeSent) {
	r.mu.Lock()

	if ev.RedirectResponse != nil {
		if prev, ok := r.current[ev.RequestID]; ok && !prev.finished {
			r.applyResponseLocked(prev, ev.RedirectResponse)
			prev.req.RedirectTo = ev.Request.URL
			prev.finished = true
			r.inflightDoneLocked()
		}
	}

	if r.exceeded != capNone {
		r.mu.Unlock()

		return
	}

	if len(r.records) >= r.maxRequests {
		r.exceeded = capRequests
		r.mu.Unlock()

		return
	}

	host := r.classifier.Classify(ev.Request.URL)

	rec := &record{
		req: model.Request{
			RequestID:     string(ev.RequestID),
			URL:           ev.Request.URL,
			NormalizedURL: r.normalizer.Key(ev.Request.URL),
			Method:        ev.Request.Method,
			ResourceType:  resourceType(ev.Type),
			Host:          host.Host,
			Domain:        host.Domain,
			Party:         host.Party,
			Phase:         r.phase,
			NonNetwork:    isNonNetwork(ev.Request.URL),
			Initiator:     convertInitiator(ev.Initiator),
			Timing:        model.Timing{StartOffset: r.offset(monotonic(ev.Timestamp))},
		},
	}

	if ev.RedirectResponse != nil {
		rec.req.RedirectFrom = ev.RedirectResponse.URL
	}

	if _, ok := r.hashTypes[rec.req.ResourceType]; ok && !rec.req.NonNetwork {
		rec.wantBody = true
	}

	r.records = append(r.records, rec)
	r.current[ev.RequestID] = rec
	r.inflight++

	r.mu.Unlock()
}

func (r *recorder) responseReceived(ev *network.EventResponseReceived) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.current[ev.RequestID]
	if !ok {
		return
	}

	// The resource type is more accurate here than at request time.
	if t := resourceType(ev.Type); t != "" {
		rec.req.ResourceType = t

		if _, want := r.hashTypes[t]; want && !rec.req.NonNetwork {
			rec.wantBody = true
		}
	}

	r.applyResponseLocked(rec, ev.Response)
}

func (r *recorder) applyResponseLocked(rec *record, resp *network.Response) {
	if resp == nil {
		return
	}

	rec.req.Status = int(resp.Status)
	rec.req.StatusText = resp.StatusText
	rec.req.MimeType = resp.MimeType
	rec.req.Protocol = resp.Protocol
	rec.req.RemoteIP = resp.RemoteIPAddress

	if resp.FromDiskCache || resp.FromPrefetchCache {
		rec.req.FromCache = true
	}

	if resp.Timing != nil && resp.Timing.ReceiveHeadersEnd > 0 {
		// Timing ticks are milliseconds relative to the request start.
		rec.req.Timing.TTFB = time.Duration(resp.Timing.ReceiveHeadersEnd * float64(time.Millisecond))
	}
}

func (r *recorder) loadingFinished(ev *network.EventLoadingFinished) {
	r.mu.Lock()

	rec, ok := r.current[ev.RequestID]
	if !ok || rec.finished {
		r.mu.Unlock()

		return
	}

	rec.req.TransferSize = int64(ev.EncodedDataLength)
	rec.req.Timing.EndOffset = r.offset(monotonic(ev.Timestamp))
	rec.finished = true

	r.totalBytes += rec.req.TransferSize
	if r.maxBytes > 0 && r.totalBytes > r.maxBytes && r.exceeded == capNone {
		r.exceeded = capBytes
	}

	wantBody := rec.wantBody
	r.inflightDoneLocked()
	r.mu.Unlock()

	if wantBody {
		// Bodies must be fetched while Chrome still holds them, but a CDP
		// call from inside an event listener would deadlock, so the request
		// is handed to a worker.
		select {
		case r.bodyWanted <- ev.RequestID:
		default:
			r.addWarning("body fingerprint queue full; some script digests were skipped")
		}
	}
}

func (r *recorder) loadingFailed(ev *network.EventLoadingFailed) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.current[ev.RequestID]
	if !ok || rec.finished {
		return
	}

	rec.req.Failed = true
	rec.req.FailureReason = ev.ErrorText
	rec.req.Timing.EndOffset = r.offset(monotonic(ev.Timestamp))

	if ev.BlockedReason != "" {
		rec.req.Blocked = true
		rec.req.BlockedReason = string(ev.BlockedReason)
	}

	if ev.CorsErrorStatus != nil && ev.CorsErrorStatus.CorsError != "" {
		rec.req.Blocked = true
		if rec.req.BlockedReason == "" {
			rec.req.BlockedReason = "cors: " + string(ev.CorsErrorStatus.CorsError)
		}
	}

	if t := resourceType(ev.Type); t != "" {
		rec.req.ResourceType = t
	}

	rec.finished = true
	r.inflightDoneLocked()
}

func (r *recorder) servedFromCache(ev *network.EventRequestServedFromCache) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if rec, ok := r.current[ev.RequestID]; ok {
		rec.req.FromCache = true
	}
}

// webSocketCreated records a WebSocket as its own request. Chrome does not
// emit the usual request lifecycle for it, so it is recorded as finished
// immediately: the fact that the connection was opened is the observation.
func (r *recorder) webSocketCreated(ev *network.EventWebSocketCreated) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.exceeded != capNone || len(r.records) >= r.maxRequests {
		if r.exceeded == capNone {
			r.exceeded = capRequests
		}

		return
	}

	host := r.classifier.Classify(ev.URL)

	rec := &record{
		req: model.Request{
			RequestID:     string(ev.RequestID),
			URL:           ev.URL,
			NormalizedURL: r.normalizer.Key(ev.URL),
			Method:        "GET",
			ResourceType:  "websocket",
			Host:          host.Host,
			Domain:        host.Domain,
			Party:         host.Party,
			Phase:         r.phase,
			Initiator:     convertInitiator(ev.Initiator),
			Timing:        model.Timing{StartOffset: time.Since(r.start)},
		},
		finished: true,
	}

	r.records = append(r.records, rec)
}

// setBodyDigest attaches a fingerprint, or records why one is missing. A
// missing digest is never silently ambiguous (Tenet 5).
func (r *recorder) setBodyDigest(id network.RequestID, digest string, size int, ref string, unavailable string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.current[id]
	if !ok {
		return
	}

	if unavailable != "" {
		rec.req.BodyUnavailable = unavailable

		return
	}

	rec.req.BodySHA256 = digest
	rec.req.BodyRef = ref

	if rec.req.DecodedSize == 0 {
		rec.req.DecodedSize = int64(size)
	}
}

func (r *recorder) inflightDoneLocked() {
	r.inflight--

	if r.inflight <= 0 {
		r.inflight = 0

		select {
		case r.idleSignal <- struct{}{}:
		default:
		}
	}
}

func (r *recorder) addWarning(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.warnings {
		if existing == msg {
			return
		}
	}

	r.warnings = append(r.warnings, msg)
}

func (r *recorder) snapshot() (inflight int, exceeded capReason) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.inflight, r.exceeded
}

// requests returns the recorded requests in a deterministic order. Ordering by
// start offset alone is not stable — concurrent requests share a millisecond —
// so the normalized key and method break ties.
func (r *recorder) requests() []model.Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]model.Request, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, rec.req)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Timing.StartOffset != out[j].Timing.StartOffset {
			return out[i].Timing.StartOffset < out[j].Timing.StartOffset
		}

		if out[i].NormalizedURL != out[j].NormalizedURL {
			return out[i].NormalizedURL < out[j].NormalizedURL
		}

		return out[i].Method < out[j].Method
	})

	return out
}

func (r *recorder) capturedWarnings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.warnings))
	copy(out, r.warnings)

	return out
}

// resourceType maps Chrome's resource type onto the lowercase form used in
// results. An unset type becomes "other" rather than an empty string, so
// filtering never has to special-case it.
func resourceType(t network.ResourceType) string {
	if t == "" {
		return "other"
	}

	return lower(string(t))
}

func lower(s string) string {
	b := []byte(s)

	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}

	return string(b)
}

func isNonNetwork(rawURL string) bool {
	for _, prefix := range []string{"data:", "blob:", "about:", "javascript:", "filesystem:"} {
		if len(rawURL) >= len(prefix) && lower(rawURL[:len(prefix)]) == prefix {
			return true
		}
	}

	return false
}

func convertInitiator(in *network.Initiator) model.Initiator {
	if in == nil {
		return model.Initiator{Type: "other"}
	}

	out := model.Initiator{
		Type: lower(string(in.Type)),
		URL:  in.URL,
	}

	if in.LineNumber > 0 {
		// CDP reports 0-based line numbers; results are 1-based to match what
		// a developer sees in an editor.
		out.LineNumber = int(in.LineNumber) + 1
	}

	if in.Stack != nil {
		for _, frame := range in.Stack.CallFrames {
			if frame.URL != "" {
				out.Stack = append(out.Stack, frame.URL)
			}
		}

		// The immediate caller is most useful and is not always in URL.
		if out.URL == "" && len(out.Stack) > 0 {
			out.URL = out.Stack[0]
		}
	}

	return out
}

func monotonic(t *cdp.MonotonicTime) *time.Time {
	if t == nil {
		return nil
	}

	tt := t.Time()

	return &tt
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}
