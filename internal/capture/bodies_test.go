package capture

import (
	"errors"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/network"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 1.11: storing every body a scan sees, exercised without Chrome.

func storingRecorder(t *testing.T, b bodyStorage, sink BodySink) *recorder {
	t.Helper()

	r := newTestRecorder(t)
	r.bodySink = sink
	r.configureBodies(b)

	return r
}

func finished(r *recorder, id, url string, typ network.ResourceType, status int64) {
	r.requestWillBeSent(willBeSent(id, url, "GET", typ))
	r.responseReceived(&network.EventResponseReceived{
		RequestID: network.RequestID(id), Type: typ,
		Response: &network.Response{URL: url, Status: status},
	})
	r.loadingFinished(&network.EventLoadingFinished{RequestID: network.RequestID(id), Timestamp: mono(0)})
}

func queued(r *recorder) []network.RequestID {
	var out []network.RequestID

	for {
		select {
		case id := <-r.bodyWanted:
			out = append(out, id)
		default:
			return out
		}
	}
}

// store: all reads every network body; only fingerprinted types are hashed.
func TestStoreAllQueuesEveryBodyButHashesOnlyScripts(t *testing.T) {
	t.Parallel()

	r := storingRecorder(t, bodyStorage{mode: model.BodyStoreAll}, nil)

	finished(r, "1", "https://example.com/a.js", network.ResourceTypeScript, 200)
	finished(r, "2", "https://example.com/a.png", network.ResourceTypeImage, 200)

	if got := queued(r); len(got) != 2 {
		t.Fatalf("queued %v, want both bodies", got)
	}

	if hashed, store := r.bodyPlan("1"); !hashed || !store {
		t.Errorf("script plan = hashed %v store %v, want both", hashed, store)
	}

	if hashed, store := r.bodyPlan("2"); hashed || !store {
		t.Errorf("image plan = hashed %v store %v, want stored without a digest", hashed, store)
	}
}

// store: hashed keeps exactly what storeBodies always kept.
func TestStoreHashedStoresOnlyFingerprintedTypes(t *testing.T) {
	t.Parallel()

	r := storingRecorder(t, bodyStorage{mode: model.BodyStoreHashed}, nil)

	finished(r, "1", "https://example.com/a.js", network.ResourceTypeScript, 200)
	finished(r, "2", "https://example.com/a.png", network.ResourceTypeImage, 200)

	if got := queued(r); len(got) != 1 || got[0] != "1" {
		t.Fatalf("queued %v, want the script only", got)
	}

	if _, store := r.bodyPlan("2"); store {
		t.Error("an image is stored under store: hashed")
	}
}

// An unsampled scan reads exactly what a scan read before Story 1.11.
func TestStoreNoneReadsOnlyWhatIsHashed(t *testing.T) {
	t.Parallel()

	r := storingRecorder(t, bodyStorage{mode: model.BodyStoreNone}, nil)

	finished(r, "1", "https://example.com/a.js", network.ResourceTypeScript, 200)
	finished(r, "2", "https://example.com/a.png", network.ResourceTypeImage, 200)

	if got := queued(r); len(got) != 1 {
		t.Fatalf("queued %v, want the script only", got)
	}

	if _, store := r.bodyPlan("1"); store {
		t.Error("an unsampled scan stores a body")
	}

	r.finishBodies()

	if got := r.requests()[1].BodyUnavailable; got != "" {
		t.Errorf("an unsampled scan explains a body it never wanted: %q", got)
	}
}

// AC11: in a scan that stores bodies, nothing goes missing without a reason.
func TestEveryRequestGetsABodyOrAReason(t *testing.T) {
	t.Parallel()

	r := storingRecorder(t, bodyStorage{mode: model.BodyStoreAll}, nil)

	// A redirect hop, then its destination.
	r.requestWillBeSent(willBeSent("redir", "https://example.com/old", "GET", network.ResourceTypeDocument))
	next := willBeSent("redir", "https://example.com/new", "GET", network.ResourceTypeDocument)
	next.RedirectResponse = &network.Response{URL: "https://example.com/old", Status: 301}
	r.requestWillBeSent(next)

	r.requestWillBeSent(willBeSent("head", "https://example.com/h", "HEAD", network.ResourceTypeFetch))
	r.loadingFinished(&network.EventLoadingFinished{RequestID: "head", Timestamp: mono(0)})

	finished(r, "empty", "https://example.com/204", network.ResourceTypeFetch, 204)
	finished(r, "unchanged", "https://example.com/304", network.ResourceTypeFetch, 304)

	r.requestWillBeSent(willBeSent("failed", "https://blocked.test/x.js", "GET", network.ResourceTypeScript))
	r.loadingFailed(&network.EventLoadingFailed{RequestID: "failed", ErrorText: "net::ERR_BLOCKED_BY_CLIENT", Timestamp: mono(0)})

	r.webSocketCreated(&network.EventWebSocketCreated{RequestID: "ws", URL: "wss://example.com/live"})

	r.requestWillBeSent(willBeSent("open", "https://example.com/slow", "GET", network.ResourceTypeXHR))

	finished(r, "stored", "https://example.com/a.css", network.ResourceTypeStylesheet, 200)
	r.setBody("stored", bodyResult{ref: "body/aa", storedSize: 3, size: 3})

	r.finishBodies()

	want := map[string]string{
		"https://example.com/old":   "no body: redirect",
		"https://example.com/h":     "no body: HEAD request",
		"https://example.com/204":   "no body: status 204",
		"https://example.com/304":   "no body: status 304",
		"https://blocked.test/x.js": "no body: the request failed",
		"wss://example.com/live":    "no body: websocket",
		"https://example.com/slow":  model.BodyReasonScanEnded,
		"https://example.com/new":   model.BodyReasonScanEnded,
	}

	for _, req := range r.requests() {
		if req.URL == "https://example.com/a.css" {
			if req.BodyRef != "body/aa" || req.BodyUnavailable != "" {
				t.Errorf("stored body = ref %q reason %q", req.BodyRef, req.BodyUnavailable)
			}

			continue
		}

		w, ok := want[req.URL]
		if !ok {
			t.Errorf("unexpected request %s", req.URL)

			continue
		}

		if req.BodyUnavailable != w {
			t.Errorf("%s: bodyUnavailable = %q, want %q", req.URL, req.BodyUnavailable, w)
		}
	}

	stats := r.bodyStats()
	if stats.ResponseBodiesStored != 1 || stats.ResponseBodyBytes != 3 {
		t.Errorf("stats = %+v, want one stored body of 3 bytes", stats)
	}

	// "No body" is not a failure to store one; the two still-open requests are.
	if stats.BodiesUnavailable != 2 {
		t.Errorf("BodiesUnavailable = %d, want 2", stats.BodiesUnavailable)
	}
}

// AC12: the per-scan budget, once exhausted, stays exhausted.
func TestTheBodyBudget(t *testing.T) {
	t.Parallel()

	r := storingRecorder(t, bodyStorage{mode: model.BodyStoreAll, maxScanBytes: 10}, nil)

	if !r.reserveBodyBytes(6) {
		t.Fatal("6 of 10 bytes were refused")
	}

	if r.reserveBodyBytes(6) {
		t.Fatal("12 of 10 bytes were allowed")
	}

	if r.reserveBodyBytes(1) {
		t.Error("a smaller body fitted in after a larger one was refused; what is stored must not depend on finish order")
	}

	if !r.bodyStats().BudgetExhausted {
		t.Error("an exhausted budget is not reported")
	}
}

func payloadEvent(id, body, contentType string) *network.EventRequestWillBeSent {
	ev := willBeSent(id, "https://tracker.test/collect", "POST", network.ResourceTypePing)
	ev.Request.HasPostData = true
	ev.Request.Headers = network.Headers{"content-type": contentType}

	if body != "" {
		ev.Request.PostDataEntries = []*network.PostDataEntry{
			{Bytes: []byte(body[:len(body)/2])},
			{Bytes: []byte(body[len(body)/2:])},
		}
	}

	return ev
}

// AC10: a request payload in the event is stored, with its content type, and a
// credential wsaw supplied is redacted before it is.
func TestRequestPayloadsAreStoredAndRedacted(t *testing.T) {
	t.Parallel()

	var stored []byte

	sink := func(_ string, data []byte) (string, error) {
		stored = data

		return "body/payload", nil
	}

	r := storingRecorder(t, bodyStorage{
		mode: model.BodyStoreAll, requestBodies: true, maxBodyBytes: 1 << 20, maxScanBytes: 1 << 20,
		scrub: func(s string) string { return strings.ReplaceAll(s, "hunter2", "[redacted]") },
	}, sink)

	r.requestWillBeSent(payloadEvent("1", `{"user":"x","auth":"hunter2"}`, "application/json"))

	got := r.requests()[0]

	if got.RequestBodyRef != "body/payload" || got.RequestBodyMimeType != "application/json" {
		t.Errorf("payload = ref %q type %q", got.RequestBodyRef, got.RequestBodyMimeType)
	}

	if !got.RequestBodyRedacted || strings.Contains(string(stored), "hunter2") {
		t.Errorf("stored %q, redacted %v; the credential must not be stored", stored, got.RequestBodyRedacted)
	}

	if got.RequestBodySHA256 != sha256Hex(stored) {
		t.Error("the payload digest is not of the bytes stored")
	}
}

// A payload Chrome left out of the event is asked for.
func TestARequestPayloadNotInTheEventIsFetched(t *testing.T) {
	t.Parallel()

	r := storingRecorder(t, bodyStorage{mode: model.BodyStoreAll, requestBodies: true}, nil)

	r.requestWillBeSent(payloadEvent("1", "", "text/plain"))

	select {
	case id := <-r.payloadWanted:
		if id != "1" {
			t.Errorf("fetched %q, want 1", id)
		}
	default:
		t.Fatal("a payload missing from the event was not queued")
	}

	// The fetch never returned: the scan ended first, and says so.
	r.finishBodies()

	if got := r.requests()[0].RequestBodyUnavailable; got != model.BodyReasonScanEnded {
		t.Errorf("RequestBodyUnavailable = %q, want %q", got, model.BodyReasonScanEnded)
	}
}

// Request payloads are opt-in, and never stored by an unsampled scan.
func TestRequestPayloadsAreNotStoredUnlessAskedFor(t *testing.T) {
	t.Parallel()

	for name, b := range map[string]bodyStorage{
		"requestBodies off": {mode: model.BodyStoreAll},
		"unsampled":         {mode: model.BodyStoreNone, requestBodies: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := storingRecorder(t, b, func(string, []byte) (string, error) {
				t.Error("a payload was stored")

				return "", nil
			})

			r.requestWillBeSent(payloadEvent("1", "a=b", "application/x-www-form-urlencoded"))
			r.finishBodies()

			got := r.requests()[0]
			if got.RequestBodyRef != "" || got.RequestBodyUnavailable != "" {
				t.Errorf("payload = %+v, want nothing recorded", got)
			}
		})
	}
}

// A payload over the cap, or past the budget, or that the store refuses, is
// recorded with its reason.
func TestRequestPayloadFailuresAreExplained(t *testing.T) {
	t.Parallel()

	failing := func(string, []byte) (string, error) { return "", errors.New("disk full") }

	cases := map[string]struct {
		b    bodyStorage
		sink BodySink
		want string
	}{
		"over the cap":  {bodyStorage{mode: model.BodyStoreAll, requestBodies: true, maxBodyBytes: 2}, failing, model.BodyReasonTooLarge},
		"over budget":   {bodyStorage{mode: model.BodyStoreAll, requestBodies: true, maxScanBytes: 2}, failing, model.BodyReasonBudget},
		"store refused": {bodyStorage{mode: model.BodyStoreAll, requestBodies: true}, failing, model.BodyReasonStoreFailed},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := storingRecorder(t, c.b, c.sink)
			r.requestWillBeSent(payloadEvent("1", "abcdef", "text/plain"))

			if got := r.requests()[0].RequestBodyUnavailable; !strings.HasPrefix(got, c.want) {
				t.Errorf("RequestBodyUnavailable = %q, want it to start with %q", got, c.want)
			}
		})
	}
}

func TestHeaderValueIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	h := network.Headers{"Content-Type": "text/plain"}
	if got := headerValue(h, "content-type"); got != "text/plain" {
		t.Errorf("headerValue = %q", got)
	}
}
