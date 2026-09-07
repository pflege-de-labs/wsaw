package daemon

import (
	"fmt"
	"hash/fnv"
	"math"
	"testing"
	"time"
)

// spread used to take the hash modulo the window. Sum32 read as nanoseconds
// stops at about 4.3s, so every wider window was silently truncated to that
// and the shipped five minute jitter spread targets over four seconds. These
// tests pin the two properties that regression violated: the result stays
// inside the window, and a wide window is actually used.
func TestSpreadStaysInsideTheWindow(t *testing.T) {
	t.Parallel()

	windows := []time.Duration{
		time.Nanosecond,
		time.Second,
		30 * time.Second,
		5 * time.Minute,
		24 * time.Hour,
		time.Duration(math.MaxInt64),
	}

	hashes := []uint32{0, 1, math.MaxUint32 / 2, math.MaxUint32 - 1, math.MaxUint32}

	for _, window := range windows {
		for _, hash := range hashes {
			got := spread(hash, window)

			if got < 0 || got >= window {
				t.Errorf("spread(%d, %v) = %v, want within [0, %v)", hash, window, got, window)
			}
		}
	}
}

func TestSpreadUsesTheWholeWindow(t *testing.T) {
	t.Parallel()

	const window = 5 * time.Minute

	// Ten buckets across the window; a capped implementation puts every
	// target in the first one.
	var buckets [10]int

	for i := range 500 {
		h := fnv.New32a()
		_, _ = fmt.Fprintf(h, "target-%d/reject", i)

		buckets[int64(spread(h.Sum32(), window))*10/int64(window)]++
	}

	for i, count := range buckets {
		if count == 0 {
			t.Errorf("no target landed in tenth %d of the window; the window is not being used", i)
		}
	}
}

// A window wider than the old ceiling must produce delays beyond it.
func TestSpreadIsNotCappedAtTheHashWidth(t *testing.T) {
	t.Parallel()

	const (
		window  = 5 * time.Minute
		oldCeil = 4300 * time.Millisecond
	)

	var widest time.Duration

	for i := range 500 {
		h := fnv.New32a()
		_, _ = fmt.Fprintf(h, "target-%d/reject", i)

		if d := spread(h.Sum32(), window); d > widest {
			widest = d
		}
	}

	if widest <= oldCeil {
		t.Errorf("widest delay was %v, still within the %v the modulo used to cap at", widest, oldCeil)
	}
}
