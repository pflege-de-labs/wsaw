package browser

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
)

// eventWait bounds how long a test waits for the listener's goroutine.
const eventWait = 5 * time.Second

// fakeTransport hands out a fixed list of messages, then reports the
// connection closed.
type fakeTransport struct {
	msgs   []*cdproto.Message
	debugf func(string, ...any)
}

func (f *fakeTransport) Read(_ context.Context, msg *cdproto.Message) error {
	if len(f.msgs) == 0 {
		return io.EOF
	}

	*msg = *f.msgs[0]
	f.msgs = f.msgs[1:]

	return nil
}

func (f *fakeTransport) Write(context.Context, *cdproto.Message) error { return nil }

func (f *fakeTransport) Close() error { return nil }

func (f *fakeTransport) SetDebugf(fn func(string, ...any)) { f.debugf = fn }

func event(session target.SessionID, method, params string) *cdproto.Message {
	return &cdproto.Message{SessionID: session, Method: cdproto.MethodType(method), Params: []byte(params)}
}

// readAll drains tr the way chromedp's read loop does.
func readAll(t *testing.T, tr interface {
	Read(context.Context, *cdproto.Message) error
},
) {
	t.Helper()

	for {
		var msg cdproto.Message
		if err := tr.Read(t.Context(), &msg); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read: %v", err)
			}

			return
		}
	}
}

// collect returns a listener that hands every event to a channel.
func collect() (func(ev any), <-chan any) {
	ch := make(chan any, 16)

	return func(ev any) { ch <- ev }, ch
}

func next(t *testing.T, ch <-chan any) any {
	t.Helper()

	select {
	case ev := <-ch:
		return ev
	case <-time.After(eventWait):
		t.Fatal("no event arrived")

		return nil
	}
}

// The reason the tap exists: a listener sees the events of different methods
// in the order the browser sent them, and only those of its own tab.
func TestTapKeepsOrderAcrossMethods(t *testing.T) {
	t.Parallel()

	tap := newEventTap()
	fn, ch := collect()

	if err := tap.listen(t.Context(), "tab", fn); err != nil {
		t.Fatal(err)
	}

	readAll(t, tap.wrap(&fakeTransport{msgs: []*cdproto.Message{
		event("tab", "Network.requestWillBeSent", `{"requestId":"1","request":{"url":"https://a.test/","method":"GET"}}`),
		event("other", "Network.loadingFinished", `{"requestId":"9","timestamp":1}`),
		event("tab", "Network.responseReceived", `{"requestId":"1","response":{"url":"https://a.test/","status":200}}`),
		event("", "Network.loadingFinished", `{"requestId":"8","timestamp":1}`),
		{ID: 7, SessionID: "tab", Result: []byte(`{}`)},
		event("tab", "Network.loadingFinished", `{"requestId":"1","timestamp":2}`),
	}}))

	if _, ok := next(t, ch).(*network.EventRequestWillBeSent); !ok {
		t.Fatal("first event is not requestWillBeSent")
	}

	if _, ok := next(t, ch).(*network.EventResponseReceived); !ok {
		t.Fatal("second event is not responseReceived")
	}

	finished, ok := next(t, ch).(*network.EventLoadingFinished)
	if !ok || finished.RequestID != "1" {
		t.Fatalf("third event = %#v, want loadingFinished of request 1", finished)
	}

	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %T: another tab's events, browser events and replies are not this tab's", ev)
	default:
	}
}

// An event that cannot be decoded is a gap the listener is told about; one
// this cdproto has never heard of is not.
func TestTapReportsUndecodableEvents(t *testing.T) {
	t.Parallel()

	tap := newEventTap()
	fn, ch := collect()

	if err := tap.listen(t.Context(), "tab", fn); err != nil {
		t.Fatal(err)
	}

	readAll(t, tap.wrap(&fakeTransport{msgs: []*cdproto.Message{
		event("tab", "Future.somethingNew", `{}`),
		event("tab", "Network.requestWillBeSent", `{"requestId":`),
		event("tab", "Network.loadingFinished", `{"requestId":"1","timestamp":2}`),
	}}))

	var evErr *EventError
	if err, _ := next(t, ch).(error); !errors.As(err, &evErr) || evErr.Method != "Network.requestWillBeSent" {
		t.Fatalf("first delivery = %v, want an EventError for the broken requestWillBeSent", err)
	}

	if _, ok := next(t, ch).(*network.EventLoadingFinished); !ok {
		t.Fatal("the events after a broken one must still arrive")
	}
}

// A listener that falls behind loses events, and is told how many, after the
// ones it was given.
func TestSinkReportsDroppedEvents(t *testing.T) {
	t.Parallel()

	const extra = 3

	delivered := 0

	var dropped error

	done := make(chan struct{})
	sink := &eventSink{wake: make(chan struct{}, 1), fn: func(ev any) {
		if err, ok := ev.(error); ok {
			dropped = err

			close(done)

			return
		}

		delivered++
	}}

	for range maxQueuedEvents + extra {
		sink.push(event("tab", "Network.loadingFinished", `{"requestId":"1","timestamp":1}`))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go sink.pump(ctx)

	select {
	case <-done:
	case <-time.After(10 * eventWait):
		t.Fatal("no drop was reported")
	}

	if delivered != maxQueuedEvents {
		t.Errorf("delivered %d events before the drop, want %d", delivered, maxQueuedEvents)
	}

	if !errors.Is(dropped, ErrEventsDropped) {
		t.Errorf("drop reported as %v, want ErrEventsDropped", dropped)
	}
}

// A listener ends with its context, and its tab can then be listened to
// again; while it lives, a second one is refused rather than starved.
func TestListenerLifetime(t *testing.T) {
	t.Parallel()

	tap := newEventTap()

	ctx, cancel := context.WithCancel(t.Context())

	if err := tap.listen(ctx, "tab", func(any) {}); err != nil {
		t.Fatal(err)
	}

	if err := tap.listen(t.Context(), "tab", func(any) {}); err == nil {
		t.Fatal("a second listener on the same tab was accepted")
	}

	cancel()

	deadline := time.Now().Add(eventWait)

	for {
		tap.mu.Lock()
		_, still := tap.sinks["tab"]
		tap.mu.Unlock()

		if !still {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the listener outlived its context")
		}

		<-time.After(time.Millisecond)
	}

	if err := tap.listen(t.Context(), "tab", func(any) {}); err != nil {
		t.Fatalf("listening again after the first listener ended: %v", err)
	}
}

func TestListenNeedsAScanContext(t *testing.T) {
	t.Parallel()

	if err := Listen(t.Context(), func(any) {}); !errors.Is(err, errNoEventTap) {
		t.Fatalf("Listen on a plain context = %v, want errNoEventTap", err)
	}
}

// chromedp hands its protocol logger only to the transport it is given, so
// the tap passes it on.
func TestTappedConnPassesTheLoggerOn(t *testing.T) {
	t.Parallel()

	inner := &fakeTransport{}

	tapped, ok := newEventTap().wrap(inner).(interface{ SetDebugf(func(string, ...any)) })
	if !ok {
		t.Fatal("the tapped connection has no SetDebugf")
	}

	tapped.SetDebugf(func(string, ...any) {})

	if inner.debugf == nil {
		t.Fatal("the logger did not reach the wrapped transport")
	}
}
