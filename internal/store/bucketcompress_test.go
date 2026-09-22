package store_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Compression through the bucket seam (Story 4.8 against Story 8.1).
//
// The tests next door in compress_test.go are about a directory: they stat the
// file and read it back with os.ReadFile. These are about the seam, and the
// difference matters because the bucket is the only implementation left — a
// deployment on object storage has no files to stat, and every one of the
// promises below has to hold there too.

// TestTheResultDocumentIsStoredCompressed is the question Epic 8 raised and
// could not answer on its own: the document is the largest and most
// compressible artifact wsaw writes, and once it became an artifact it should
// be packed like one.
func TestTheResultDocumentIsStoredCompressed(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	// Repetitive enough to compress, which a real scan document is: the same
	// field names, domains and URLs over and over.
	res := result("scan-1", time.Now(), model.ConsentReject)
	for i := range 200 {
		res.Requests = append(res.Requests, model.Request{
			URL:           "https://tracker.test/px?" + strings.Repeat("a", 40),
			NormalizedURL: "https://tracker.test/px",
			Domain:        "tracker.test",
			Party:         model.ThirdParty,
			Phase:         model.PhasePre,
		})

		_ = i
	}

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	bucket := evidence(t, opts)

	ref, size, _ := documentRefFor(t, bucket, "scan-1")

	key := bucket.StoredKey(ref)
	if key == ref {
		t.Fatalf("the document at %s was stored uncompressed", ref)
	}

	if stored := bucket.Size(key); stored >= size {
		t.Errorf("the packed document is %d bytes and the document %d: packing saved nothing", stored, size)
	}

	// And none of that reaches the caller: what comes back is the scan.
	got, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("reading back a packed document: %v", err)
	}

	if got.ScanID != res.ScanID || len(got.Requests) != len(res.Requests) {
		t.Errorf("the document read back is not the one stored: %d requests, want %d",
			len(got.Requests), len(res.Requests))
	}
}

// TestAReferenceNamesTheEvidenceNotThePacking is Story 4.8, AC2 at the seam:
// the reference a result carries is the digest of the scan's own bytes, so it
// does not change when the storage decision does.
func TestAReferenceNamesTheEvidenceNotThePacking(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("window.dataLayer.push({event:'pageview'});\n"), 200)

	packed := openAt(t, storeOptions(t))

	plainOpts := storeOptions(t)
	plainOpts.ArtifactCompression = store.CompressionNone
	plain := openAt(t, plainOpts)

	packedRef, err := packed.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	plainRef, err := plain.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	if packedRef != plainRef {
		t.Errorf("the same bytes are named %s packed and %s plain", packedRef, plainRef)
	}

	// Both read back as the bytes that went in, whichever way they are held.
	for name, s := range map[string]store.Store{"packed": packed, "plain": plain} {
		got, err := s.GetArtifact(packedRef)
		if err != nil {
			t.Errorf("%s: GetArtifact: %v", name, err)

			continue
		}

		if !bytes.Equal(got, body) {
			t.Errorf("%s: read back %d bytes, want the %d that were stored", name, len(got), len(body))
		}
	}
}

// TestAPackedArtifactStreamsAndStatsAsItsOwnSize is Story 4.8, AC7 where it is
// load-bearing: the HTTP layer writes a Content-Length before it writes a byte
// of the body (Story 8.7, AC4), so a packed artifact that reported its packed
// length would hand every client a truncated download.
func TestAPackedArtifactStreamsAndStatsAsItsOwnSize(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	body := bytes.Repeat([]byte("function track(event){window.dataLayer.push(event);}\n"), 200)

	ref, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	if key := evidence(t, opts).StoredKey(ref); key == ref {
		t.Fatal("the body was stored uncompressed, so this test proves nothing")
	}

	info, err := s.StatArtifact(t.Context(), ref)
	if err != nil {
		t.Fatalf("StatArtifact: %v", err)
	}

	if info.Size != int64(len(body)) {
		t.Errorf("StatArtifact reports %d bytes, want the artifact's own %d", info.Size, len(body))
	}

	r, err := s.OpenArtifact(t.Context(), ref)
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}

	defer func() { _ = r.Close() }()

	if r.Size != int64(len(body)) {
		t.Errorf("the reader reports %d bytes, want %d", r.Size, len(body))
	}

	var got bytes.Buffer
	if _, err := got.ReadFrom(r); err != nil {
		t.Fatalf("streaming a packed artifact: %v", err)
	}

	if !bytes.Equal(got.Bytes(), body) {
		t.Errorf("streamed %d bytes, want the %d that were stored", got.Len(), len(body))
	}
}

// TestBothSpellingsAreReadWhicheverWayTheStoreIsConfigured is AC5: turning
// compression on must not hide what was written before it, and turning it off
// must not strand what was written while it was on.
func TestBothSpellingsAreReadWhicheverWayTheStoreIsConfigured(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("a stored response body\n"), 200)

	// One store writes it packed, and a second store over the same bucket,
	// with compression off, has to read it.
	opts := storeOptions(t)
	packed := openAt(t, opts)

	ref, err := packed.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	if key := evidence(t, opts).StoredKey(ref); key == ref {
		t.Fatal("the body was stored uncompressed, so this test proves nothing")
	}

	reopened := opts
	reopened.ArtifactCompression = store.CompressionNone

	plain := openAt(t, reopened)

	got, err := plain.GetArtifact(ref)
	if err != nil {
		t.Fatalf("a store with compression off cannot read what it wrote with it on: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Errorf("read back %d bytes, want %d", len(got), len(body))
	}

	// And storing the same artifact again writes nothing new: either spelling
	// counts as stored, so an operator who turns compression off does not get
	// a second, plain copy of everything they already hold.
	before := len(evidence(t, opts).Keys("body/"))

	if _, err := plain.PutArtifact("body", body); err != nil {
		t.Fatal(err)
	}

	if after := len(evidence(t, opts).Keys("body/")); after != before {
		t.Errorf("storing a held artifact again left %d objects, want the %d already there", after, before)
	}
}

// TestPruningRemovesAPackedArtifact is the failure this integration would
// otherwise have: a prune that deleted only the plain key would leave every
// packed artifact in the bucket for ever, unreferenced and uncollectable.
func TestPruningRemovesAPackedArtifact(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	now := time.Now()

	// A body that is worth packing, which the shared fixture's short one is
	// not: the ratio gate is about the bytes, not about the kind.
	body := bytes.Repeat([]byte("window.dataLayer.push({event:'pageview'});\n"), 200)

	bodyRef, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	res := result("scan-1", now.Add(-48*time.Hour), model.ConsentReject)
	res.Requests[0].BodyRef = bodyRef

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	bucket := evidence(t, opts)

	if key := bucket.StoredKey(bodyRef); key == bodyRef {
		t.Fatal("the stored body was not packed, so this test proves nothing")
	}

	stats, err := s.Prune(t.Context(), store.TriggerCLI, now, store.Retention{MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if stats.ResultsDeleted != 1 {
		t.Fatalf("the prune removed %d results, want 1", stats.ResultsDeleted)
	}

	if bucket.HasArtifact(bodyRef) {
		t.Error("the packed body outlived the only result that referenced it")
	}

	if stats.BytesFreed <= 0 {
		t.Errorf("BytesFreed = %d after deleting %d artifacts", stats.BytesFreed, stats.ArtifactsDeleted)
	}
}

// TestACorruptPackedArtifactReadsAsCorruptNotAbsent keeps the one distinction
// Story 8.2, AC6 exists for: evidence that has been pruned is absent, and
// evidence that is present and wrong is corrupt. A packed object that will not
// inflate is the second, and must never be reported as the first.
func TestACorruptPackedArtifactReadsAsCorruptNotAbsent(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	s := openAt(t, opts)

	body := bytes.Repeat([]byte("a stored response body\n"), 200)

	ref, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	bucket := evidence(t, opts)

	key := bucket.StoredKey(ref)
	if key == ref {
		t.Fatal("the body was stored uncompressed, so this test proves nothing")
	}

	bucket.Write(key, []byte("this is not a gzip member"))

	_, err = s.GetArtifact(ref)

	if !errors.Is(err, store.ErrCorrupt) {
		t.Errorf("reading a corrupt packed artifact = %v, want ErrCorrupt", err)
	}

	if errors.Is(err, store.ErrNotFound) {
		t.Error("corrupt evidence was reported as evidence that is not there")
	}
}
