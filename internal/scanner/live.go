package scanner

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/martint17r/wsaw/internal/model"
)

// Sources name what started a scan. They are recorded so an operator seeing
// activity can tell a scheduled run from something a person just clicked.
const (
	// SourceSchedule is the scheduler, which is the default: every caller that
	// does not say otherwise is the daemon running its own plan.
	SourceSchedule = "schedule"
	// SourceAPI is an ad-hoc scan triggered through the API or the web
	// interface.
	SourceAPI = "api"
	// SourceCLI is a one-shot or debug run from the command line.
	SourceCLI = "cli"
)

type sourceKey struct{}

// WithSource labels the scans started on this context. It travels in the
// context because Scan's signature is the seam the daemon implements, and
// widening it for a display detail would be the wrong trade.
func WithSource(ctx context.Context, source string) context.Context {
	if source == "" {
		return ctx
	}

	return context.WithValue(ctx, sourceKey{}, source)
}

func sourceOf(ctx context.Context) string {
	if s, ok := ctx.Value(sourceKey{}).(string); ok && s != "" {
		return s
	}

	return SourceSchedule
}

// Running is one scan that has started and not yet finished.
//
// A scan in flight exists nowhere else: the store only learns of it when it
// ends. Without this, an interface cannot distinguish "nothing is happening"
// from "a scan has been running for four minutes", and the second is exactly
// what Tenet 8 says must be visible (Story 5.12).
type Running struct {
	ScanID      string            `json:"scanId"`
	Target      string            `json:"target"`
	URL         string            `json:"url"`
	ConsentMode model.ConsentMode `json:"consentMode"`
	StartedAt   time.Time         `json:"startedAt"`
	// Source is one of the Source constants.
	Source string `json:"source"`
}

// Live tracks the scans running right now. It is in-memory by design: a
// running scan does not survive the process that runs it, so persisting the
// fact would only create stale entries after a crash.
type Live struct {
	mu      sync.RWMutex
	running map[string]Running
}

// NewLive creates an empty registry.
func NewLive() *Live {
	return &Live{running: make(map[string]Running)}
}

// begin records a scan as running and returns the function that unrecords it.
// Returning the releaser rather than exposing a finish method makes it hard to
// leak an entry: the caller defers what begin handed back.
func (l *Live) begin(r Running) func() {
	if l == nil {
		return func() {}
	}

	l.mu.Lock()
	l.running[r.ScanID] = r
	l.mu.Unlock()

	return func() {
		l.mu.Lock()
		delete(l.running, r.ScanID)
		l.mu.Unlock()
	}
}

// Running lists the in-flight scans, oldest first. The order is stable so a
// page that refreshes does not reshuffle under the reader.
func (l *Live) Running() []Running {
	if l == nil {
		return nil
	}

	l.mu.RLock()

	out := make([]Running, 0, len(l.running))
	for _, r := range l.running {
		out = append(out, r)
	}

	l.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}

		return out[i].ScanID < out[j].ScanID
	})

	return out
}

// Count reports how many scans are in flight.
func (l *Live) Count() int {
	if l == nil {
		return 0
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	return len(l.running)
}

// Running exposes the in-flight scans of this scanner, for the API and the web
// interface.
func (s *Scanner) Running() []Running { return s.live.Running() }

// RunningCount reports how many scans this scanner has in flight.
func (s *Scanner) RunningCount() int { return s.live.Count() }
