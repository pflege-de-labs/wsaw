package store_test

import (
	"bytes"
	"compress/gzip"
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

// artifactStore opens a store whose artifact directory the test can inspect,
// because how an artifact is stored is exactly what these tests are about.
//
// It is SQLite-only on purpose: artifacts are files whichever driver is in
// use, so running these against a server database would test the same code
// twice and need a server to do it.
func artifactStore(t *testing.T, opts store.Options) (*store.Store, string) {
	t.Helper()

	dir := t.TempDir()
	artifacts := filepath.Join(dir, "artifacts")

	opts.Path = filepath.Join(dir, "wsaw.db")
	opts.ArtifactDir = artifacts

	s, err := store.Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s, artifacts
}

// compressibleBody is the shape of the thing this story exists for: a script
// body, which is text and repeats itself.
func compressibleBody() []byte {
	return bytes.Repeat([]byte("function track(event){window.dataLayer.push(event);}\n"), 200)
}

func artifactFile(t *testing.T, dir, ref string) (string, os.FileInfo) {
	t.Helper()

	path := filepath.Join(dir, filepath.FromSlash(ref))

	if info, err := os.Stat(path); err == nil {
		return path, info
	}

	info, err := os.Stat(path + ".gz")
	if err != nil {
		t.Fatalf("artifact %s is on disk in neither form: %v", ref, err)
	}

	return path + ".gz", info
}

// TestCompressibleArtifactIsStoredCompressed covers the point of the story:
// a body that compresses is smaller on disk, and comes back unchanged.
func TestCompressibleArtifactIsStoredCompressed(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})
	body := compressibleBody()

	ref, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	if strings.HasSuffix(ref, ".gz") {
		t.Errorf("reference %q carries the storage suffix; it must name the artifact, not the file", ref)
	}

	path, info := artifactFile(t, dir, ref)

	if !strings.HasSuffix(path, ".gz") {
		t.Fatalf("a compressible body was stored raw at %s", path)
	}

	if info.Size() >= int64(len(body)) {
		t.Errorf("stored size %d is not smaller than the artifact's %d", info.Size(), len(body))
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("artifact permissions = %o, want 600", perm)
	}

	got, err := s.GetArtifact(ref)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Error("artifact did not round-trip through compression")
	}

	// The reference is the digest of the evidence, not of the file, so a
	// reader can still verify what they were given (AC2).
	sum := sha256.Sum256(got)
	if want := "body/" + hex.EncodeToString(sum[:]); ref != want {
		t.Errorf("reference = %q, want %q", ref, want)
	}
}

// TestIncompressibleArtifactIsStoredRaw: a PNG is already compressed, and
// gzipping it would cost CPU on every read to save nothing (AC1).
func TestIncompressibleArtifactIsStoredRaw(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	// Random bytes stand in for a PNG's pixel data: incompressible by
	// construction, so the test does not depend on a fixture's entropy.
	noise := make([]byte, 4096)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}

	png := append([]byte("\x89PNG\r\n\x1a\n"), noise...)

	ref, err := s.PutArtifact("screenshot", png)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	path, _ := artifactFile(t, dir, ref)

	if strings.HasSuffix(path, ".gz") {
		t.Errorf("an incompressible artifact was stored compressed at %s", path)
	}

	got, err := s.GetArtifact(ref)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}

	if !bytes.Equal(got, png) {
		t.Error("artifact did not round-trip")
	}
}

// TestArtifactStoredBeforeCompressionStillReads: an installation upgrading
// into this story keeps every artifact it already had, where it already is
// (AC4), and does not store a second copy of it (AC5).
func TestArtifactStoredBeforeCompressionStillReads(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})
	body := compressibleBody()

	// What the previous version wrote: the raw bytes, under the plain name.
	sum := sha256.Sum256(body)
	ref := "body/" + hex.EncodeToString(sum[:])
	path := filepath.Join(dir, filepath.FromSlash(ref))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetArtifact(ref)
	if err != nil {
		t.Fatalf("GetArtifact on a pre-compression artifact: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Error("a pre-compression artifact did not read back unchanged")
	}

	if size, err := s.StatArtifact(ref); err != nil || size != int64(len(body)) {
		t.Errorf("StatArtifact = %d, %v; want %d", size, err, len(body))
	}

	again, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	if again != ref {
		t.Errorf("reference = %q, want %q", again, ref)
	}

	if _, err := os.Stat(path + ".gz"); err == nil {
		t.Error("a compressed second copy was written of an artifact already on disk")
	}
}

// TestCompressedArtifactIsNotStoredTwice is the same guarantee the other way
// round, and the one that keeps content addressing honest: the same bytes
// cost one file (AC5).
func TestCompressedArtifactIsNotStoredTwice(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})
	body := compressibleBody()

	ref, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	path, first := artifactFile(t, dir, ref)

	again, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	if again != ref {
		t.Errorf("reference = %q, want %q", again, ref)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("storing the same body twice left %d files, want 1", len(entries))
	}

	_, second := artifactFile(t, dir, ref)
	if !second.ModTime().Equal(first.ModTime()) {
		t.Error("an artifact already on disk was rewritten")
	}
}

// TestStatArtifactReportsTheArtifactSize: the number the interface shows is
// how much evidence there is, and compression must not change it (AC7).
func TestStatArtifactReportsTheArtifactSize(t *testing.T) {
	t.Parallel()

	s, _ := artifactStore(t, store.Options{})
	body := compressibleBody()

	ref, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	size, err := s.StatArtifact(ref)
	if err != nil {
		t.Fatalf("StatArtifact: %v", err)
	}

	if size != int64(len(body)) {
		t.Errorf("StatArtifact = %d, want the artifact's own size %d", size, len(body))
	}
}

// TestMissingCompressedArtifactIsNotFound: retention removing evidence must
// still read as "pruned", not as "broken" (Story 5.17, AC3).
func TestMissingCompressedArtifactIsNotFound(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{})

	ref, err := s.PutArtifact("body", compressibleBody())
	if err != nil {
		t.Fatal(err)
	}

	path, _ := artifactFile(t, dir, ref)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetArtifact(ref); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetArtifact after pruning = %v, want ErrNotFound", err)
	}

	if _, err := s.StatArtifact(ref); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("StatArtifact after pruning = %v, want ErrNotFound", err)
	}
}

// TestCorruptCompressedArtifactIsRefused: a stored artifact is untrusted on
// the way back in, and bytes that are not the evidence must never be served
// as if they were (AC6).
func TestCorruptCompressedArtifactIsRefused(t *testing.T) {
	t.Parallel()

	body := compressibleBody()

	sum := sha256.Sum256(body)
	ref := "body/" + hex.EncodeToString(sum[:])

	cases := map[string]func(t *testing.T, packed []byte) []byte{
		"truncated": func(_ *testing.T, packed []byte) []byte {
			return packed[:len(packed)/2]
		},
		"not gzip at all": func(_ *testing.T, _ []byte) []byte {
			return []byte("this is not a gzip member")
		},
		"different content under the same reference": func(t *testing.T, _ []byte) []byte {
			t.Helper()

			return gzipped(t, append(body, " // tampered"...))
		},
	}

	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, dir := artifactStore(t, store.Options{})

			path := filepath.Join(dir, filepath.FromSlash(ref)+".gz")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, corrupt(t, gzipped(t, body)), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := s.GetArtifact(ref)
			if err == nil {
				t.Fatalf("GetArtifact returned %d bytes for a corrupt artifact", len(got))
			}

			if !strings.Contains(err.Error(), ref) {
				t.Errorf("error %q does not name the artifact", err)
			}
		})
	}
}

// TestOversizedCompressedArtifactIsRefused: inflation is capped rather than
// trusted, so a crafted member cannot make the store allocate on demand
// (AC6).
func TestOversizedCompressedArtifactIsRefused(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{MaxArtifactBytes: 1024})

	// Compresses to a few dozen bytes and inflates to well past the cap,
	// which is the shape of the attack.
	bomb := bytes.Repeat([]byte{0}, 64<<10)

	sum := sha256.Sum256(bomb)
	ref := "body/" + hex.EncodeToString(sum[:])
	path := filepath.Join(dir, filepath.FromSlash(ref)+".gz")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, gzipped(t, bomb), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetArtifact(ref); err == nil {
		t.Error("GetArtifact inflated an artifact past the cap")
	} else if !strings.Contains(err.Error(), "size cap") {
		t.Errorf("error = %q, want it to name the cap", err)
	}
}

// TestCompressionCanBeTurnedOff: the setting decides how artifacts are
// written and never what can be read (AC8).
func TestCompressionCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	s, dir := artifactStore(t, store.Options{ArtifactCompression: store.CompressionNone})
	body := compressibleBody()

	ref, err := s.PutArtifact("body", body)
	if err != nil {
		t.Fatal(err)
	}

	path, _ := artifactFile(t, dir, ref)
	if strings.HasSuffix(path, ".gz") {
		t.Errorf("compression is off but the artifact was stored compressed at %s", path)
	}

	// An artifact written while compression was on is still readable after
	// it is turned off.
	other := append(body, " // second build"...)

	sum := sha256.Sum256(other)
	otherRef := "body/" + hex.EncodeToString(sum[:])
	otherPath := filepath.Join(dir, filepath.FromSlash(otherRef)+".gz")

	if err := os.WriteFile(otherPath, gzipped(t, other), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetArtifact(otherRef)
	if err != nil {
		t.Fatalf("GetArtifact with compression off: %v", err)
	}

	if !bytes.Equal(got, other) {
		t.Error("a compressed artifact did not read back once compression was turned off")
	}
}

// TestUnknownCompressionIsRefused: a misspelled setting asked for something,
// and quietly doing the other thing is how a setting stops meaning anything.
func TestUnknownCompressionIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	s, err := store.Open(store.Options{
		Path:                filepath.Join(dir, "wsaw.db"),
		ArtifactDir:         filepath.Join(dir, "artifacts"),
		ArtifactCompression: "zstd",
	})
	if err == nil {
		_ = s.Close()

		t.Fatal("Open accepted an unknown artifact compression")
	}

	if !strings.Contains(err.Error(), "zstd") {
		t.Errorf("error = %q, want it to name the rejected value", err)
	}
}

// TestArtifactStoredReportsWhatItSaved: what compression bought is counted,
// and a deduplicated write is not counted because nothing was written (AC10).
func TestArtifactStoredReportsWhatItSaved(t *testing.T) {
	t.Parallel()

	type write struct {
		kind             string
		original, stored int64
	}

	var writes []write

	s, _ := artifactStore(t, store.Options{
		OnArtifactStored: func(kind string, original, stored int64) {
			writes = append(writes, write{kind, original, stored})
		},
	})

	body := compressibleBody()

	if _, err := s.PutArtifact("body", body); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PutArtifact("body", body); err != nil {
		t.Fatal(err)
	}

	if len(writes) != 1 {
		t.Fatalf("%d writes reported, want 1: a deduplicated write stores nothing", len(writes))
	}

	got := writes[0]

	if got.kind != "body" {
		t.Errorf("kind = %q, want %q", got.kind, "body")
	}

	if got.original != int64(len(body)) {
		t.Errorf("original = %d, want %d", got.original, len(body))
	}

	if got.stored >= got.original {
		t.Errorf("stored = %d, want less than the %d handed in", got.stored, got.original)
	}
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)

	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}
