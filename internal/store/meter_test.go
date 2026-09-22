package store_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// meter totals what Options.OnBucketOp reports, under a lock because the hook
// is called on whichever goroutine made the request.
type meter struct {
	mu    sync.Mutex
	ops   int
	bytes int64
	names map[string]int
}

func (m *meter) record(op string, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.names == nil {
		m.names = map[string]int{}
	}

	m.ops++
	m.bytes += bytes
	m.names[op]++
}

func (m *meter) totals() (ops int, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.ops, m.bytes
}

// TestTheBucketMeterCountsRequestsAndBytes is what Story 8.9, AC6 rests on: the
// soak test can only report what a long run cost if the store will say.
//
// The numbers are asserted as inequalities rather than as exact counts on
// purpose. What each read path costs in requests is pinned down exactly
// elsewhere, per store kind and per path, by the tests that own that promise;
// what this one owns is that the meter is connected to the bucket at all — a
// hook that silently reported zero would make the soak's new line a lie, and a
// convincing one.
func TestTheBucketMeterCountsRequestsAndBytes(t *testing.T) {
	t.Parallel()

	var m meter

	opts := storeOptions(t)
	opts.OnBucketOp = m.record

	s := openAt(t, opts)

	res := result("scan-1", time.Now(), model.ConsentReject)
	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	ops, bytes := m.totals()
	if ops == 0 {
		t.Fatal("storing a scan made no bucket request that the meter could see")
	}

	if bytes == 0 {
		t.Error("storing a scan moved no bytes, and its document is not empty")
	}

	// A stored artifact's bytes are counted where they are written, so a
	// screenshot of a known size moves at least that much.
	const shot = "a screenshot of a known size"

	before := bytes

	if _, err := s.PutArtifact("screenshot-before-consent", []byte(shot)); err != nil {
		t.Fatal(err)
	}

	if _, after := m.totals(); after-before < int64(len(shot)) {
		t.Errorf("storing %d bytes of screenshot moved %d bytes by the meter", len(shot), after-before)
	}

	// And a read is counted as well as a write, so the totals are traffic
	// rather than growth.
	_, before = m.totals()

	if _, err := s.GetResult("site", model.ConsentReject, "scan-1"); err != nil {
		t.Fatal(err)
	}

	if _, after := m.totals(); after <= before {
		t.Error("reading a result moved no bytes by the meter")
	}
}

// TestTheBucketMeterNamesTheRequest keeps the hook's op argument useful: it is
// the same prose the retry policy uses, so an operator counting requests and an
// operator reading a retry log are looking at one vocabulary.
func TestTheBucketMeterNamesTheRequest(t *testing.T) {
	t.Parallel()

	var m meter

	opts := storeOptions(t)
	opts.OnBucketOp = m.record

	s := openAt(t, opts)

	if _, err := s.PutArtifact("body", []byte("a stored body")); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var stored bool

	for name := range m.names {
		if strings.Contains(name, "storing an artifact") {
			stored = true
		}

		if name == "" {
			t.Error("a bucket request was reported to the meter with no name")
		}
	}

	if !stored {
		t.Errorf("no request was reported as storing an artifact; the meter saw %v", m.names)
	}
}

// The meter is on Options, so nothing that does not ask for one pays for it.
var _ = store.Options{OnBucketOp: nil}
