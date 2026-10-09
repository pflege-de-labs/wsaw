package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// maxQueuedEvents bounds the events waiting for one scan's listener. A page
// controls how many events it causes, so the queue cannot be unbounded; the
// bound is far above what a listener that keeps up ever holds.
const maxQueuedEvents = 1 << 16

// ErrEventsDropped reports that a listener fell so far behind that events of
// its tab were discarded rather than queued.
var ErrEventsDropped = errors.New("events were dropped because the listener fell behind")

// errNoEventTap reports a context that does not come from NewScanContext.
var errNoEventTap = errors.New("not a scan context of a wsaw browser")

// EventError is handed to a listener in place of events it was not given, so
// a gap in what it saw is a recorded fact and never a silent one (Tenet 5).
type EventError struct {
	// Method is the protocol method of the event, when one event is meant.
	Method string
	Err    error
}

func (e *EventError) Error() string {
	if e.Method == "" {
		return e.Err.Error()
	}

	return fmt.Sprintf("event %s: %v", e.Method, e.Err)
}

func (e *EventError) Unwrap() error { return e.Err }

// Listen calls fn with every event of the scan tab of ctx, decoded and in the
// order the browser sent them, until ctx ends. ctx must come from
// NewScanContext. An event that cannot be decoded, or that was dropped, is
// passed as an *EventError instead.
//
// chromedp only offers one subscription per event type, and those do not keep
// the order between types. A capture pairs a request with its response and
// its completion by that order, so wsaw reads the events off the connection
// itself. fn runs on a goroutine of its own, one call at a time, and may call
// the browser.
func Listen(ctx context.Context, fn func(ev any)) error {
	tap, _ := ctx.Value(tapKey{}).(*eventTap)

	c := chromedp.FromContext(ctx)
	if tap == nil || c == nil || c.Target == nil {
		return errNoEventTap
	}

	return tap.listen(ctx, c.Target.SessionID, fn)
}

// tapKey is the context key under which a scan context carries its browser's
// event tap.
type tapKey struct{}

// eventTap routes the events read from one browser connection to the
// listener of the tab they belong to.
type eventTap struct {
	mu    sync.Mutex
	sinks map[target.SessionID]*eventSink
}

func newEventTap() *eventTap {
	return &eventTap{sinks: make(map[target.SessionID]*eventSink)}
}

// wrap returns tr with every message it reads also offered to the tap.
func (t *eventTap) wrap(tr chromedp.Transport) chromedp.Transport {
	return &tappedConn{Transport: tr, tap: t}
}

func (t *eventTap) listen(ctx context.Context, session target.SessionID, fn func(ev any)) error {
	sink := &eventSink{fn: fn, wake: make(chan struct{}, 1)}

	t.mu.Lock()

	if _, taken := t.sinks[session]; taken {
		t.mu.Unlock()

		return errors.New("the tab already has an event listener")
	}

	t.sinks[session] = sink

	t.mu.Unlock()

	context.AfterFunc(ctx, func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		if t.sinks[session] == sink {
			delete(t.sinks, session)
		}
	})

	go sink.pump(ctx)

	return nil
}

func (t *eventTap) publish(msg *cdproto.Message) {
	t.mu.Lock()
	sink := t.sinks[msg.SessionID]
	t.mu.Unlock()

	if sink != nil {
		sink.push(msg)
	}
}

// tappedConn is a chromedp transport that shows each event it reads to the
// tap before chromedp routes it. It runs on the connection's read loop, so it
// only queues.
type tappedConn struct {
	chromedp.Transport

	tap *eventTap
}

func (c *tappedConn) Read(ctx context.Context, msg *cdproto.Message) error {
	if err := c.Transport.Read(ctx, msg); err != nil {
		return err
	}

	if msg.Method != "" && msg.SessionID != "" {
		c.tap.publish(msg)
	}

	return nil
}

// SetDebugf passes chromedp's protocol logger on to the wrapped transport,
// which chromedp only looks for on the outermost one.
func (c *tappedConn) SetDebugf(f func(string, ...any)) {
	if s, ok := c.Transport.(interface{ SetDebugf(f func(string, ...any)) }); ok {
		s.SetDebugf(f)
	}
}

// eventSink is one listener's queue: filled by the read loop without ever
// blocking it, and emptied in order by the listener's own goroutine.
type eventSink struct {
	fn   func(ev any)
	wake chan struct{}

	mu      sync.Mutex
	queue   []*cdproto.Message
	dropped int
}

func (s *eventSink) push(msg *cdproto.Message) {
	s.mu.Lock()

	if len(s.queue) >= maxQueuedEvents {
		s.dropped++
	} else {
		s.queue = append(s.queue, msg)
	}

	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *eventSink) pump(ctx context.Context) {
	for {
		s.mu.Lock()
		queue, dropped := s.queue, s.dropped
		s.queue, s.dropped = nil, 0
		s.mu.Unlock()

		for _, msg := range queue {
			if ctx.Err() != nil {
				return
			}

			s.deliver(msg)
		}

		// Drops happen only once the queue is full, so they follow everything
		// that was delivered.
		if dropped > 0 && ctx.Err() == nil {
			s.fn(&EventError{Err: fmt.Errorf("%w: %d", ErrEventsDropped, dropped)})
		}

		if len(queue) > 0 || dropped > 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
	}
}

func (s *eventSink) deliver(msg *cdproto.Message) {
	ev, err := cdproto.UnmarshalMessage(msg, chromedp.DefaultUnmarshalOptions)
	if err != nil {
		// An event this cdproto does not know is one no listener can be
		// waiting for; Chrome adds them between releases.
		var unknown cdp.ErrUnknownCommandOrEvent
		if errors.As(err, &unknown) {
			return
		}

		s.fn(&EventError{Method: string(msg.Method), Err: err})

		return
	}

	s.fn(ev)
}
