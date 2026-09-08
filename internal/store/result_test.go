package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// These are white-box tests of the seam between a row and its document. They
// need to see the bucket to count what it is asked to do, which is exactly what
// the black-box suite in store_test.go cannot.

// countedBucketOps makes the store's bucket count every operation it performs,
// and returns the counter.
//
// It counts through the retry policy the bucket runs on, because every one of
// its operations goes through it — a read, a write, an existence check and a
// listing alike. Counting there rather than per method means a method added
// later is counted without anyone remembering to.
func countedBucketOps(t *testing.T, s *Store) *atomic.Int64 {
	t.Helper()

	var ops atomic.Int64

	s.bucket.setRetry(func(ctx context.Context, op string, fn func(context.Context) error) error {
		ops.Add(1)

		return s.retry(ctx, op, fn)
	})

	return &ops
}

func openCounted(t *testing.T) (*Store, *atomic.Int64) {
	t.Helper()

	dir := t.TempDir()

	s, err := Open(t.Context(), Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s, countedBucketOps(t, s)
}

func storedResult(id string, at time.Time) *model.Result {
	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        id,
		Target:        "site",
		URL:           "https://example.com/",
		ConsentMode:   model.ConsentReject,
		StartedAt:     at,
		FinishedAt:    at.Add(time.Second),
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied},
		Requests: []model.Request{{
			URL: "https://tracker.test/px", Domain: "tracker.test",
			Party: model.ThirdParty, Phase: model.PhasePre,
		}},
	}
}

// TestListingPerformsNoBucketOperations is Story 8.3, AC2 and AC3, asserted the
// way the story asks for: as a count that must be zero.
//
// Against object storage the difference is not academic. A listing that reads
// one document per row turns a page of fifty scans into fifty round trips and
// fifty billable requests, and the regression is invisible on a local disk —
// which is where it would be written and reviewed.
func TestListingPerformsNoBucketOperations(t *testing.T) {
	t.Parallel()

	s, ops := openCounted(t)

	base := time.Now().Add(-time.Hour)

	for i := range 5 {
		if err := s.PutResult(storedResult("scan-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}

	// Writing does touch the bucket; only reading must not.
	if ops.Load() == 0 {
		t.Fatal("storing five results performed no bucket operation, so this test cannot prove anything")
	}

	ops.Store(0)

	if got, err := s.ListResults("site", model.ConsentReject, 0); err != nil || len(got) != 5 {
		t.Fatalf("ListResults = %d summaries, %v", len(got), err)
	}

	if got, err := s.HasResult("site", model.ConsentReject, "scan-a"); err != nil || !got {
		t.Fatalf("HasResult = %v, %v", got, err)
	}

	if got, err := s.Series(); err != nil || len(got) != 1 {
		t.Fatalf("Series = %v, %v", got, err)
	}

	if n := ops.Load(); n != 0 {
		t.Errorf("listing five results performed %d bucket operations, want 0", n)
	}

	// And the counter really does count, so a zero above means "did not read"
	// rather than "cannot see".
	if _, err := s.GetResult("site", model.ConsentReject, "scan-a"); err != nil {
		t.Fatal(err)
	}

	if n := ops.Load(); n == 0 {
		t.Error("reading a result performed no bucket operation: the counter is not wired to the bucket")
	}
}

// TestResultArgsMatchTheInsertColumns keeps the two halves of the insert in
// step. They are generated from the same column lists, so the failure this
// catches is a column added to resultUpdate without a value to go in it —
// which the database would report as a mismatched placeholder count, at
// runtime, on whichever dialect ran first.
func TestResultArgsMatchTheInsertColumns(t *testing.T) {
	t.Parallel()

	columns := len(resultKey) + len(resultUpdate)

	if got := len(resultArgs(Summary{}, resultRef{})); got != columns {
		t.Errorf("the insert binds %d values for %d columns", got, columns)
	}

	statement := resultInsert()

	if got := strings.Count(statement, "?"); got != columns {
		t.Errorf("the insert has %d placeholders for %d columns: %s", got, columns, statement)
	}

	for _, col := range append(append([]string{}, resultKey...), resultUpdate...) {
		// Followed by a separator, so "document" is not satisfied by
		// "document_size".
		if !strings.Contains(statement, col+",") && !strings.Contains(statement, col+")") {
			t.Errorf("the insert does not name the %s column: %s", col, statement)
		}
	}
}

// TestVerifyDocumentCatchesWhatTheBucketCannot is the unit behind Story 8.2,
// AC6. A bucket returns what it holds; whether that is still the document the
// store wrote is a question only the row can answer.
func TestVerifyDocumentCatchesWhatTheBucketCannot(t *testing.T) {
	t.Parallel()

	document := []byte(`{"scanId":"scan-1"}`)
	digest := documentDigest(document)

	good := resultRef{
		ref:    artifactKindResult + refSeparator + digest,
		size:   int64(len(document)),
		digest: digest,
	}

	if err := verifyDocument(good, document); err != nil {
		t.Errorf("an untouched document was reported as corrupt: %v", err)
	}

	truncated := good
	truncated.size = int64(len(document)) + 1

	if err := verifyDocument(truncated, document); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a document of the wrong length = %v, want ErrCorrupt", err)
	}

	tampered := good
	tampered.digest = documentDigest([]byte(`{"scanId":"scan-2"}`))

	if err := verifyDocument(tampered, document); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a document that hashes to something else = %v, want ErrCorrupt", err)
	}
}

// TestARowNamingAnImpossibleArtifactIsCorruptNotAbsent is Story 8.2, AC6 for
// the reference itself.
//
// Every other path that meets a malformed reference gets it from a URL, where
// it is ordinary hostile input and the honest answer is "not found". This one
// comes out of wsaw's own row, so it is a corrupt index entry — and reporting
// it as a server fault, which is what an unclassified error becomes over HTTP,
// would raise an alert about wsaw for a row somebody edited.
func TestARowNamingAnImpossibleArtifactIsCorruptNotAbsent(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	for _, ref := range []string{"../escape", "result/not-a-digest", "result", "result/" + strings.Repeat("z", 64)} {
		_, err := s.readDocument(t.Context(), resultRef{ref: ref, size: 1, digest: "x"})

		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("a row naming %q = %v, want ErrCorrupt", ref, err)
		}

		if errors.Is(err, ErrNotFound) {
			t.Errorf("a row naming %q also reports ErrNotFound (%v), which reads as evidence that was pruned", ref, err)
		}
	}
}

// TestAnArtifactCanBeStreamedRatherThanBuffered is Story 8.1, AC7. The reader
// is the seam the HTTP layer serves from, so it has to exist on the store and
// report the size a Content-Length needs.
func TestAnArtifactCanBeStreamedRatherThanBuffered(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	want := []byte(strings.Repeat("evidence ", 4096))

	ref, err := s.PutArtifact("body", want)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	r, err := s.OpenArtifact(t.Context(), ref)
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}

	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("closing the artifact reader: %v", err)
		}
	}()

	if r.Size != int64(len(want)) {
		t.Errorf("OpenArtifact reports %d bytes, want %d: Content-Length is written before the body is", r.Size, len(want))
	}

	// The digest comes back with the stream, because a strong entity tag is
	// written before the body too (Story 8.7, AC4).
	if r.Digest == "" {
		t.Error("OpenArtifact reports no digest")
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Error("the stream did not return the bytes that were stored")
	}

	// A reference this store never wrote is refused as absent, exactly as
	// GetArtifact refuses it: the value reaches both from a URL.
	if _, err := s.OpenArtifact(t.Context(), "../escape"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenArtifact on a crafted reference = %v, want ErrNotFound", err)
	}
}
