package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/store"
)

// writeRaw puts an artifact on disk the way wsaw did before compression
// existed, which is the state this command is for.
func writeRaw(t *testing.T, dir, kind string, data []byte) string {
	t.Helper()

	sum := sha256.Sum256(data)
	ref := kind + "/" + hex.EncodeToString(sum[:])
	path := filepath.Join(dir, filepath.FromSlash(ref))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	return ref
}

func exists(t *testing.T, path string) bool {
	t.Helper()

	_, err := os.Stat(path)

	return err == nil
}

// TestCompactArtifactsCompressesWhatIsWorthIt is the command's whole point:
// evidence already on disk gets the saving, and reads back identically.
func TestCompactArtifactsCompressesWhatIsWorthIt(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	body := compressibleBody()
	bodyRef := writeRaw(t, dir, "body", body)

	noise := make([]byte, 4096)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}

	png := append([]byte("\x89PNG\r\n\x1a\n"), noise...)
	pngRef := writeRaw(t, dir, "screenshot", png)

	// An artifact stored compressed already, which the walk must count and
	// leave alone.
	other := append(body, " // second build"...)

	sum := sha256.Sum256(other)
	otherRef := "body/" + hex.EncodeToString(sum[:])

	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(otherRef))+".gz", gzipped(t, other), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := s.CompactArtifacts(t.Context(), store.CompactOptions{})
	if err != nil {
		t.Fatalf("CompactArtifacts: %v", err)
	}

	if stats.Scanned != 2 || stats.Compressed != 1 || stats.NotWorthIt != 1 || stats.AlreadyCompressed != 1 {
		t.Errorf("stats = %+v; want 2 scanned, 1 compressed, 1 not worth it, 1 already compressed", stats)
	}

	if stats.Problems != 0 {
		t.Errorf("problems = %d, want 0", stats.Problems)
	}

	if stats.Saved() <= 0 || stats.BytesAfter >= stats.BytesBefore {
		t.Errorf("stats reported no saving: %+v", stats)
	}

	// The compressible body moved; the PNG did not.
	if exists(t, filepath.Join(dir, filepath.FromSlash(bodyRef))) {
		t.Error("the uncompressed body is still on disk")
	}

	if !exists(t, filepath.Join(dir, filepath.FromSlash(bodyRef))+".gz") {
		t.Error("the compressed body is not on disk")
	}

	if !exists(t, filepath.Join(dir, filepath.FromSlash(pngRef))) {
		t.Error("an incompressible artifact was moved")
	}

	// What matters in the end: every artifact still reads back as itself.
	for ref, want := range map[string][]byte{bodyRef: body, pngRef: png, otherRef: other} {
		got, err := s.GetArtifact(ref)
		if err != nil {
			t.Errorf("GetArtifact(%s): %v", ref, err)

			continue
		}

		if !bytes.Equal(got, want) {
			t.Errorf("artifact %s did not survive compaction", ref)
		}

		if info, err := s.StatArtifact(t.Context(), ref); err != nil || info.Size != int64(len(want)) {
			t.Errorf("StatArtifact(%s) = %d, %v; want %d", ref, info.Size, err, len(want))
		}
	}
}

// TestCompactArtifactsIsIdempotent: a second run has nothing to do, which is
// what makes an interrupted run free to resume (AC5).
func TestCompactArtifactsIsIdempotent(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})
	writeRaw(t, dir, "body", compressibleBody())

	if _, err := s.CompactArtifacts(t.Context(), store.CompactOptions{}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.CompactArtifacts(t.Context(), store.CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.Scanned != 0 || stats.Compressed != 0 || stats.AlreadyCompressed != 1 {
		t.Errorf("second run did work: %+v", stats)
	}
}

// TestCompactArtifactsDryRunChangesNothing (AC7).
func TestCompactArtifactsDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	body := compressibleBody()
	ref := writeRaw(t, dir, "body", body)

	stats, err := s.CompactArtifacts(t.Context(), store.CompactOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	if stats.Compressed != 1 || stats.Saved() <= 0 {
		t.Errorf("dry run reported %+v; want the saving it would make", stats)
	}

	path := filepath.Join(dir, filepath.FromSlash(ref))

	if !exists(t, path) {
		t.Error("a dry run removed the artifact")
	}

	if exists(t, path+".gz") {
		t.Error("a dry run wrote a compressed artifact")
	}
}

// TestCompactArtifactsLeavesWhatItDoesNotUnderstand: this is the one command
// that modifies stored evidence, so anything it cannot verify it does not
// touch (AC6).
func TestCompactArtifactsLeavesWhatItDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	// A file whose contents do not hash to its name: a corrupt artifact, and
	// repacking it would destroy the evidence that it is corrupt.
	corruptRef := writeRaw(t, dir, "body", compressibleBody())
	corruptPath := filepath.Join(dir, filepath.FromSlash(corruptRef))

	if err := os.WriteFile(corruptPath, append(compressibleBody(), " // tampered"...), 0o600); err != nil {
		t.Fatal(err)
	}

	// A file that is not an artifact at all.
	foreign := filepath.Join(dir, "body", "notes.txt")
	if err := os.WriteFile(foreign, []byte(strings.Repeat("a note to self\n", 100)), 0o600); err != nil {
		t.Fatal(err)
	}

	var problems []string

	stats, err := s.CompactArtifacts(t.Context(), store.CompactOptions{
		OnProblem: func(ref string, err error) {
			problems = append(problems, ref+": "+err.Error())
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if stats.Compressed != 0 || stats.Problems != 2 {
		t.Errorf("stats = %+v; want nothing compressed and 2 problems", stats)
	}

	if len(problems) != 2 {
		t.Fatalf("reported %d problems, want 2: %v", len(problems), problems)
	}

	if !exists(t, corruptPath) || exists(t, corruptPath+".gz") {
		t.Error("a corrupt artifact was rewritten")
	}

	if !exists(t, foreign) || exists(t, foreign+".gz") {
		t.Error("a file that is not an artifact was rewritten")
	}
}

// TestCompactArtifactsStopsOnCancellation: SIGTERM stops it between
// artifacts, and what it finished stays finished (AC9).
func TestCompactArtifactsStopsOnCancellation(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	for i := range 8 {
		writeRaw(t, dir, "body", append(compressibleBody(), byte(i)))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stats, err := s.CompactArtifacts(ctx, store.CompactOptions{
		OnArtifact: func(_ string, _, _ int64) {
			cancel()
		},
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompactArtifacts = %v, want context.Canceled", err)
	}

	if stats.Compressed == 0 {
		t.Error("a cancelled run reported none of the work it did")
	}

	if stats.Compressed == 8 {
		t.Error("cancellation did not stop the walk")
	}

	// Everything is still readable: the half-compressed directory is a
	// valid one (AC4).
	entries, err := os.ReadDir(filepath.Join(dir, "body"))
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 8 {
		t.Errorf("%d artifacts on disk after cancellation, want 8", len(entries))
	}
}

// TestCompactArtifactsIgnoresInFlightWrites: the command is meant to be safe
// to run while wsaw is scanning, and a temporary file is not an artifact yet.
func TestCompactArtifactsIgnoresInFlightWrites(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	inflight := filepath.Join(dir, "body", ".wsaw-artifact-123456")
	if err := os.MkdirAll(filepath.Dir(inflight), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(inflight, compressibleBody(), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := s.CompactArtifacts(t.Context(), store.CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.Scanned != 0 || stats.Problems != 0 {
		t.Errorf("stats = %+v; an in-flight write is not this command's business", stats)
	}

	if !exists(t, inflight) {
		t.Error("an in-flight write was touched")
	}
}
