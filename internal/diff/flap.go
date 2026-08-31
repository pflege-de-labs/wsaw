package diff

import (
	"time"

	"github.com/martint17r/wsaw/internal/model"
)

// FlapSuppressor collapses a change that reverts within a window into a
// single event.
//
// Without this, an A/B test or a rotating CDN host produces an alert every
// scan, and an operator learns to ignore the alerts — which is a worse
// failure than not alerting at all.
type FlapSuppressor struct {
	window time.Duration

	// seen maps a change identity to when it was last emitted.
	seen map[flapKey]time.Time
}

type flapKey struct {
	target  string
	mode    model.ConsentMode
	typ     ChangeType
	subject string
}

// NewFlapSuppressor creates a suppressor. A zero or negative window disables
// suppression entirely.
func NewFlapSuppressor(window time.Duration) *FlapSuppressor {
	return &FlapSuppressor{window: window, seen: make(map[flapKey]time.Time)}
}

// keyOf pairs a change with its inverse, so that "host added" and "host
// removed" for the same subject collapse together rather than alternating.
func keyOf(c Change) flapKey {
	typ := c.Type

	switch typ {
	case HostRemoved:
		typ = HostAdded
	case AssetRemoved:
		typ = AssetAdded
	case CookieRemoved:
		typ = CookieAdded
	}

	return flapKey{target: c.Target, mode: c.ConsentMode, typ: typ, subject: c.Subject}
}

// Filter returns the changes that should be emitted, suppressing those whose
// identity was already reported within the window. The count of suppressed
// changes is returned so a report can say how much it hid.
func (f *FlapSuppressor) Filter(now time.Time, changes []Change) (emit []Change, suppressed int) {
	if f == nil || f.window <= 0 {
		return changes, 0
	}

	emit = make([]Change, 0, len(changes))

	for _, c := range changes {
		key := keyOf(c)

		if last, ok := f.seen[key]; ok && now.Sub(last) < f.window {
			suppressed++

			continue
		}

		f.seen[key] = now
		emit = append(emit, c)
	}

	return emit, suppressed
}

// Prune drops entries older than the window, so the suppressor does not grow
// without bound in a process that runs for months (NFR §2).
func (f *FlapSuppressor) Prune(now time.Time) {
	if f == nil {
		return
	}

	for key, at := range f.seen {
		if now.Sub(at) >= f.window {
			delete(f.seen, key)
		}
	}
}

// Len reports how many identities are being tracked, for tests and metrics.
func (f *FlapSuppressor) Len() int {
	if f == nil {
		return 0
	}

	return len(f.seen)
}
