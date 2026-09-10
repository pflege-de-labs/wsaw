package capture

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

// The recorder is the part of capture that turns CDP events into records, and
// it is deliberately free of any browser dependency so it can be tested
// exhaustively without Chrome (Tenet 13).

func newTestRecorder(t *testing.T) *recorder {
	t.Helper()

	cl, err := classify.New("https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}

	n, err := normalize.New(normalize.Rules{DropQueryParams: []string{"cb"}})
	if err != nil {
		t.Fatal(err)
	}

	return newRecorder(time.Now(), cl, n, []string{"script"}, 100, 1<<20, DefaultStallAfter,
		DefaultMaxBodyBytes, false, nil)
}

func mono(offset time.Duration) *cdp.MonotonicTime {
	t := cdp.MonotonicTime(time.Now().Add(offset))

	return &t
}

func willBeSent(id, url, method string, typ network.ResourceType) *network.EventRequestWillBeSent {
	return &network.EventRequestWillBeSent{
		RequestID: network.RequestID(id),
		Request:   &network.Request{URL: url, Method: method},
		Type:      typ,
		Timestamp: mono(0),
		Initiator: &network.Initiator{Type: network.InitiatorTypeParser, URL: "https://example.com/"},
	}
}

func TestRecorderRecordsBasicRequest(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://example.com/app.js?cb=99", "GET", network.ResourceTypeScript))
	r.responseReceived(&network.EventResponseReceived{
		RequestID: "1",
		Type:      network.ResourceTypeScript,
		Response: &network.Response{
			URL: "https://example.com/app.js?cb=99", Status: 200, StatusText: "OK",
			MimeType: "text/javascript", Protocol: "h2", RemoteIPAddress: "93.184.216.34",
			Timing: &network.ResourceTiming{ReceiveHeadersEnd: 42},
		},
	})
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", EncodedDataLength: 1234, Timestamp: mono(0)})

	reqs := r.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}

	got := reqs[0]

	if got.Status != 200 {
		t.Errorf("Status = %d, want 200", got.Status)
	}

	if got.NormalizedURL != "https://example.com/app.js" {
		t.Errorf("NormalizedURL = %q, want the cache buster removed", got.NormalizedURL)
	}

	if got.URL != "https://example.com/app.js?cb=99" {
		t.Errorf("URL = %q, want the raw URL preserved", got.URL)
	}

	if got.Party != model.FirstParty {
		t.Errorf("Party = %q, want first", got.Party)
	}

	if got.Protocol != "h2" || got.RemoteIP != "93.184.216.34" {
		t.Errorf("Protocol/RemoteIP = %q/%q", got.Protocol, got.RemoteIP)
	}

	if got.TransferSize != 1234 {
		t.Errorf("TransferSize = %d, want 1234", got.TransferSize)
	}

	if got.Timing.TTFB != 42*time.Millisecond {
		t.Errorf("TTFB = %v, want 42ms", got.Timing.TTFB)
	}

	if inflight, _ := r.snapshot(); inflight != 0 {
		t.Errorf("inflight = %d after completion, want 0", inflight)
	}
}

// TestRecorderKeepsRedirectHopsSeparate is the behaviour that makes a
// redirect chain auditable: collapsing hops would hide which host actually
// received the first request.
func TestRecorderKeepsRedirectHopsSeparate(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	// Chrome reuses one request ID for the whole chain.
	r.requestWillBeSent(willBeSent("1", "https://example.com/go", "GET", network.ResourceTypeDocument))

	redirect := willBeSent("1", "https://tracker.test/land", "GET", network.ResourceTypeDocument)
	redirect.RedirectResponse = &network.Response{URL: "https://example.com/go", Status: 302, StatusText: "Found"}
	r.requestWillBeSent(redirect)

	r.responseReceived(&network.EventResponseReceived{
		RequestID: "1", Type: network.ResourceTypeDocument,
		Response: &network.Response{URL: "https://tracker.test/land", Status: 200},
	})
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", EncodedDataLength: 500, Timestamp: mono(0)})

	reqs := r.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2 hops: %+v", len(reqs), reqs)
	}

	first, second := reqs[0], reqs[1]

	if first.URL != "https://example.com/go" || first.Status != 302 {
		t.Errorf("first hop = %s (%d), want the 302 on example.com", first.URL, first.Status)
	}

	if first.RedirectTo != "https://tracker.test/land" {
		t.Errorf("first hop RedirectTo = %q", first.RedirectTo)
	}

	if second.URL != "https://tracker.test/land" || second.Status != 200 {
		t.Errorf("second hop = %s (%d)", second.URL, second.Status)
	}

	if second.RedirectFrom != "https://example.com/go" {
		t.Errorf("second hop RedirectFrom = %q", second.RedirectFrom)
	}

	if second.Party != model.ThirdParty {
		t.Errorf("redirect destination Party = %q, want third", second.Party)
	}

	if inflight, _ := r.snapshot(); inflight != 0 {
		t.Errorf("inflight = %d, want 0 (redirect must not leak an inflight count)", inflight)
	}
}

func TestRecorderRecordsFailuresAndBlocks(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://ads.test/t.js", "GET", network.ResourceTypeScript))
	r.loadingFailed(&network.EventLoadingFailed{
		RequestID: "1", Type: network.ResourceTypeScript,
		ErrorText: "net::ERR_BLOCKED_BY_CLIENT", BlockedReason: network.BlockedReasonInspector,
		Timestamp: mono(0),
	})

	got := r.requests()[0]

	if !got.Failed {
		t.Error("Failed = false, want true")
	}

	if got.FailureReason != "net::ERR_BLOCKED_BY_CLIENT" {
		t.Errorf("FailureReason = %q", got.FailureReason)
	}

	if !got.Blocked || got.BlockedReason == "" {
		t.Errorf("Blocked = %v, reason = %q; a blocked request must say why", got.Blocked, got.BlockedReason)
	}

	if inflight, _ := r.snapshot(); inflight != 0 {
		t.Errorf("inflight = %d after failure, want 0", inflight)
	}
}

func TestRecorderRequestCapIsRecordedNotIgnored(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})
	r := newRecorder(time.Now(), cl, n, nil, 3, 0, DefaultStallAfter, DefaultMaxBodyBytes, false, nil)

	for i := range 10 {
		r.requestWillBeSent(willBeSent(string(rune('a'+i)), "https://example.com/x", "GET", network.ResourceTypeXHR))
	}

	if got := len(r.requests()); got != 3 {
		t.Errorf("recorded %d requests, want the cap of 3", got)
	}

	if _, exceeded := r.snapshot(); exceeded != capRequests {
		t.Errorf("exceeded = %v, want capRequests so the result can report truncation", exceeded)
	}
}

func TestRecorderByteCap(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})
	r := newRecorder(time.Now(), cl, n, nil, 100, 1000, DefaultStallAfter, DefaultMaxBodyBytes, false, nil)

	r.requestWillBeSent(willBeSent("1", "https://example.com/big", "GET", network.ResourceTypeOther))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", EncodedDataLength: 2000, Timestamp: mono(0)})

	if _, exceeded := r.snapshot(); exceeded != capBytes {
		t.Errorf("exceeded = %v, want capBytes", exceeded)
	}
}

// TestRecorderPhaseSplitsPreAndPostConsent underpins the pre-consent
// tracking finding, which is the product's headline output.
func TestRecorderPhaseSplitsPreAndPostConsent(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://tracker.test/early", "GET", network.ResourceTypeImage))
	r.setPhase(model.PhasePost)
	r.requestWillBeSent(willBeSent("2", "https://ads.test/late", "GET", network.ResourceTypeScript))

	reqs := r.requests()

	var pre, post int

	for _, req := range reqs {
		switch req.Phase {
		case model.PhasePre:
			pre++
		case model.PhasePost:
			post++
		}
	}

	if pre != 1 || post != 1 {
		t.Errorf("pre = %d, post = %d, want 1 and 1", pre, post)
	}
}

// TestRecorderInflightRequestKeepsItsStartingPhase records the honest
// attribution: a request triggered before the click belongs to the
// pre-consent phase even if it completes afterwards.
func TestRecorderInflightRequestKeepsItsStartingPhase(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://tracker.test/slow", "GET", network.ResourceTypeImage))
	r.setPhase(model.PhasePost)
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", EncodedDataLength: 43, Timestamp: mono(0)})

	if got := r.requests()[0].Phase; got != model.PhasePre {
		t.Errorf("Phase = %q, want pre-interaction", got)
	}
}

func TestRecorderMarksNonNetworkURLs(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "data:image/gif;base64,R0lGOD", "GET", network.ResourceTypeImage))

	got := r.requests()[0]
	if !got.NonNetwork {
		t.Error("NonNetwork = false for a data: URL")
	}

	// A non-network resource must never be queued for body fingerprinting.
	r2 := newTestRecorder(t)
	r2.requestWillBeSent(willBeSent("1", "data:text/javascript,void 0", "GET", network.ResourceTypeScript))
	r2.loadingFinished(&network.EventLoadingFinished{RequestID: "1", Timestamp: mono(0)})

	select {
	case id := <-r2.bodyWanted:
		t.Errorf("queued body fingerprint for non-network resource %s", id)
	default:
	}
}

// TestRecorderExtractsDataURIBody covers Story 1.9: a data: URI's payload
// becomes a body — hashed, and truncated out of the URL field — the same way
// a fetched body already is, instead of sitting inline in every consumer of
// the request record.
func TestRecorderExtractsDataURIBody(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})

	payload := []byte("a small embedded png, pretend")
	encoded := base64.StdEncoding.EncodeToString(payload)

	r := newRecorder(time.Now(), cl, n, nil, 100, 0, DefaultStallAfter, DefaultMaxBodyBytes, false, nil)
	r.requestWillBeSent(willBeSent("1", "data:image/png;base64,"+encoded, "GET", network.ResourceTypeImage))

	got := r.requests()[0]

	wantDigest := sha256Hex(payload)
	if got.BodySHA256 != wantDigest {
		t.Errorf("BodySHA256 = %q, want %q", got.BodySHA256, wantDigest)
	}

	if got.DecodedSize != int64(len(payload)) {
		t.Errorf("DecodedSize = %d, want %d", got.DecodedSize, len(payload))
	}

	if got.BodyRef != "" {
		t.Errorf("BodyRef = %q, want empty: body storage was not enabled", got.BodyRef)
	}

	if strings.Contains(got.URL, encoded) {
		t.Errorf("URL still contains the raw base64 payload: %q", got.URL)
	}

	wantURL := fmt.Sprintf("data:image/png;base64,<truncated: %d bytes>", len(payload))
	if got.URL != wantURL {
		t.Errorf("URL = %q, want %q", got.URL, wantURL)
	}
}

// TestRecorderStoresDataURIBody covers the --store-bodies path: with a sink
// configured, the decoded payload is handed to it exactly like any other
// captured body, and the returned reference is what the scan detail page's
// existing body-link renders (Story 5.11, AC5).
func TestRecorderStoresDataURIBody(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})

	payload := []byte("a small embedded png, pretend")
	encoded := base64.StdEncoding.EncodeToString(payload)

	var stored []byte
	sink := func(kind string, data []byte) (string, error) {
		if kind != "body" {
			t.Errorf("sink kind = %q, want %q", kind, "body")
		}

		stored = data

		return "ref-123", nil
	}

	r := newRecorder(time.Now(), cl, n, nil, 100, 0, DefaultStallAfter, DefaultMaxBodyBytes, true, sink)
	r.requestWillBeSent(willBeSent("1", "data:image/png;base64,"+encoded, "GET", network.ResourceTypeImage))

	got := r.requests()[0]

	if got.BodyRef != "ref-123" {
		t.Errorf("BodyRef = %q, want %q", got.BodyRef, "ref-123")
	}

	if string(stored) != string(payload) {
		t.Errorf("sink received %q, want %q", stored, payload)
	}
}

// TestRecorderCapsDataURIBody covers a data: URI whose payload is too large
// to decode, mirroring the fetched-body cap (Story 1.6): the URL is still
// truncated so the payload never renders inline, but no digest is claimed
// for bytes that were never read (Tenet 5).
func TestRecorderCapsDataURIBody(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})

	payload := make([]byte, 100)
	encoded := base64.StdEncoding.EncodeToString(payload)

	r := newRecorder(time.Now(), cl, n, nil, 100, 0, DefaultStallAfter, 10, false, nil)
	r.requestWillBeSent(willBeSent("1", "data:image/png;base64,"+encoded, "GET", network.ResourceTypeImage))

	got := r.requests()[0]

	if got.BodySHA256 != "" {
		t.Errorf("BodySHA256 = %q, want empty: payload was over the cap", got.BodySHA256)
	}

	if got.BodyUnavailable == "" {
		t.Error("BodyUnavailable is empty for a payload over the cap")
	}

	if strings.Contains(got.URL, encoded) {
		t.Errorf("URL still contains the raw base64 payload: %q", got.URL)
	}
}

func TestRecorderQueuesScriptBodiesOnly(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://example.com/a.js", "GET", network.ResourceTypeScript))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", Timestamp: mono(0)})

	r.requestWillBeSent(willBeSent("2", "https://example.com/a.png", "GET", network.ResourceTypeImage))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "2", Timestamp: mono(0)})

	select {
	case id := <-r.bodyWanted:
		if id != "1" {
			t.Errorf("queued %q, want the script", id)
		}
	default:
		t.Fatal("script body was not queued for fingerprinting")
	}

	select {
	case id := <-r.bodyWanted:
		t.Errorf("queued %q as well; only scripts should be hashed by default", id)
	default:
	}
}

// TestRecorderMissingBodyIsExplained is Tenet 5 in miniature: an absent
// digest must carry a reason, never be silently absent.
func TestRecorderMissingBodyIsExplained(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://example.com/a.js", "GET", network.ResourceTypeScript))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", Timestamp: mono(0)})
	r.setBodyDigest("1", "", 0, "", "body not retrievable: evicted")

	got := r.requests()[0]

	if got.BodySHA256 != "" {
		t.Errorf("BodySHA256 = %q, want empty", got.BodySHA256)
	}

	if got.BodyUnavailable == "" {
		t.Error("BodyUnavailable is empty; a missing digest must be explained")
	}
}

func TestRecorderStoresBodyDigest(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.requestWillBeSent(willBeSent("1", "https://example.com/a.js", "GET", network.ResourceTypeScript))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "1", Timestamp: mono(0)})
	r.setBodyDigest("1", "deadbeef", 7, "ref-1", "")

	got := r.requests()[0]

	if got.BodySHA256 != "deadbeef" || got.BodyRef != "ref-1" || got.DecodedSize != 7 {
		t.Errorf("digest = %q, ref = %q, size = %d", got.BodySHA256, got.BodyRef, got.DecodedSize)
	}
}

func TestRecorderWebSocket(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.webSocketCreated(&network.EventWebSocketCreated{
		RequestID: "ws1",
		URL:       "wss://push.tracker.test/socket",
		Initiator: &network.Initiator{Type: network.InitiatorTypeScript, URL: "https://example.com/a.js"},
	})

	got := r.requests()[0]

	if got.ResourceType != "websocket" {
		t.Errorf("ResourceType = %q, want websocket", got.ResourceType)
	}

	if got.Party != model.ThirdParty {
		t.Errorf("Party = %q, want third", got.Party)
	}
}

func TestRecorderInitiatorLineNumberIsOneBased(t *testing.T) {
	t.Parallel()

	got := convertInitiator(&network.Initiator{
		Type:       network.InitiatorTypeScript,
		URL:        "https://example.com/a.js",
		LineNumber: 41, // CDP is 0-based
	})

	if got.LineNumber != 42 {
		t.Errorf("LineNumber = %d, want 42 (1-based, as an editor shows it)", got.LineNumber)
	}
}

func TestRecorderInitiatorNilIsOther(t *testing.T) {
	t.Parallel()

	if got := convertInitiator(nil); got.Type != "other" {
		t.Errorf("Type = %q, want other", got.Type)
	}
}

func TestRecorderRequestOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	// Two requests sharing a start offset must still order stably, otherwise
	// two scans of the same page produce a spurious diff.
	build := func() []model.Request {
		r := newTestRecorder(t)

		for _, u := range []string{"https://example.com/b.js", "https://example.com/a.js"} {
			ev := willBeSent(u, u, "GET", network.ResourceTypeScript)
			ev.Timestamp = mono(0)
			r.requestWillBeSent(ev)
		}

		for _, rec := range r.records {
			rec.req.Timing.StartOffset = 0
		}

		return r.requests()
	}

	first, second := build(), build()

	for i := range first {
		if first[i].URL != second[i].URL {
			t.Fatalf("ordering not deterministic at %d: %q vs %q", i, first[i].URL, second[i].URL)
		}
	}

	if first[0].NormalizedURL != "https://example.com/a.js" {
		t.Errorf("tie broken by %q, want the lexicographically smaller key first", first[0].NormalizedURL)
	}
}

func TestResourceTypeUnsetBecomesOther(t *testing.T) {
	t.Parallel()

	if got := resourceType(""); got != "other" {
		t.Errorf("resourceType(\"\") = %q, want other", got)
	}

	if got := resourceType(network.ResourceTypeXHR); got != "xhr" {
		t.Errorf("resourceType(XHR) = %q, want xhr", got)
	}
}

func TestRecorderWarningsAreDeduplicated(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	r.addWarning("same")
	r.addWarning("same")
	r.addWarning("other")

	if got := len(r.capturedWarnings()); got != 2 {
		t.Errorf("got %d warnings, want 2", got)
	}
}

// TestRecorderOffsetsAreRelativeToTheFirstEvent covers a bug that made every
// completed request look as though the scan had been cut off mid-flight.
//
// CDP timestamps use a monotonic clock whose epoch is unrelated to the wall
// clock. Measuring against time.Now() produced a negative value for every
// event, which clamped to zero, so every request carried a start offset of 0
// and no end offset at all.
func TestRecorderOffsetsAreRelativeToTheFirstEvent(t *testing.T) {
	t.Parallel()

	r := newTestRecorder(t)

	// A monotonic clock far from the wall clock, as Chrome's actually is.
	base := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *cdp.MonotonicTime {
		m := cdp.MonotonicTime(base.Add(d))

		return &m
	}

	first := willBeSent("1", "https://example.com/a.js", "GET", network.ResourceTypeScript)
	first.Timestamp = at(0)
	r.requestWillBeSent(first)

	second := willBeSent("2", "https://example.com/b.js", "GET", network.ResourceTypeScript)
	second.Timestamp = at(300 * time.Millisecond)
	r.requestWillBeSent(second)

	r.loadingFinished(&network.EventLoadingFinished{
		RequestID: "1", EncodedDataLength: 10, Timestamp: at(500 * time.Millisecond),
	})
	r.loadingFinished(&network.EventLoadingFinished{
		RequestID: "2", EncodedDataLength: 20, Timestamp: at(900 * time.Millisecond),
	})

	byURL := make(map[string]model.Request)
	for _, req := range r.requests() {
		byURL[req.URL] = req
	}

	a, b := byURL["https://example.com/a.js"], byURL["https://example.com/b.js"]

	if a.Timing.StartOffset != 0 {
		t.Errorf("first request start offset = %v, want 0", a.Timing.StartOffset)
	}

	if a.Timing.EndOffset != 500*time.Millisecond {
		t.Errorf("first request end offset = %v, want 500ms; a completed request must not look unfinished",
			a.Timing.EndOffset)
	}

	if b.Timing.StartOffset != 300*time.Millisecond {
		t.Errorf("second request start offset = %v, want 300ms", b.Timing.StartOffset)
	}

	if b.Timing.EndOffset != 900*time.Millisecond {
		t.Errorf("second request end offset = %v, want 900ms", b.Timing.EndOffset)
	}
}

// TestStalledRequestsDoNotBlockIdle is the other half of the same symptom: a
// cross-origin iframe's completion events go to a different browser target,
// so its request stays in flight for ever. Counting it would mean the network
// never reads as quiet and every such page ran to its hard timeout.
func TestStalledRequestsDoNotBlockIdle(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})

	const stallAfter = 50 * time.Millisecond

	r := newRecorder(time.Now(), cl, n, nil, 100, 0, stallAfter, DefaultMaxBodyBytes, false, nil)

	// An iframe document that never reports completion.
	r.requestWillBeSent(willBeSent("1", "https://widget.test/frame.html", "GET", network.ResourceTypeDocument))

	if inflight, _ := r.snapshot(); inflight != 1 {
		t.Fatalf("inflight = %d immediately after the request, want 1", inflight)
	}

	time.Sleep(2 * stallAfter)

	if inflight, _ := r.snapshot(); inflight != 0 {
		t.Errorf("inflight = %d after the stall threshold, want 0: a request that never completes must not hold the scan open", inflight)
	}

	// It is still recorded, and still reported as incomplete.
	if got := len(r.requests()); got != 1 {
		t.Errorf("recorded %d requests, want the stalled one kept", got)
	}

	if got := r.stalled(); len(got) != 1 {
		t.Errorf("stalled() = %v, want the one unfinished request reported", got)
	}
}

// TestFreshRequestsStillBlockIdle guards the other direction: the stall
// threshold must not cause a scan to finish while a page is genuinely loading.
func TestFreshRequestsStillBlockIdle(t *testing.T) {
	t.Parallel()

	cl, _ := classify.New("https://example.com/", nil)
	n, _ := normalize.New(normalize.Rules{})

	r := newRecorder(time.Now(), cl, n, nil, 100, 0, time.Hour, DefaultMaxBodyBytes, false, nil)

	for i := range 3 {
		r.requestWillBeSent(willBeSent(string(rune('a'+i)), "https://example.com/x", "GET", network.ResourceTypeScript))
	}

	if inflight, _ := r.snapshot(); inflight != 3 {
		t.Errorf("inflight = %d, want 3: requests still loading must hold the scan open", inflight)
	}
}
