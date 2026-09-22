package store

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Artifact compression modes, as Options.ArtifactCompression takes them.
const (
	// CompressionGzip stores an artifact gzipped when that makes it smaller.
	// It is the default.
	CompressionGzip = "gzip"
	// CompressionNone writes every artifact as it arrives. Compressed
	// artifacts already on disk are still read: turning compression off is a
	// decision about writing, never about what can be read back.
	CompressionNone = "none"
)

// compressedSuffix marks a stored artifact as a gzip member.
//
// The reference never carries it. It names the artifact — the digest of the
// evidence itself — and the suffix describes the file that happens to hold
// it, so the same artifact keeps the same reference whichever way it is
// stored (Story 4.8, AC2 and AC3).
const compressedSuffix = ".gz"

// compressionRatio is how much smaller a compressed artifact has to be to be
// worth storing compressed.
//
// Compressing costs once and decompressing costs on every read, so a few per
// cent is not a saving, it is a tax with a rebate. PNG screenshots land here
// and are stored as Chrome produced them (Story 4.8, AC1).
const compressionRatio = 0.9

// defaultMaxArtifactBytes bounds what decompressing a stored artifact may
// produce.
//
// The bytes on disk are an artifact of a hostile page, in a directory wsaw
// does not have exclusive claim to, so they are untrusted on the way back in
// and a gzip member's own claims about its size are worth nothing. The
// default is far above anything capture can write — a whole page's byte
// budget is 256 MiB and a single body's is a fraction of that — so it bounds
// a crafted or corrupt file and nothing else (Story 4.8, AC6).
const defaultMaxArtifactBytes = 256 << 20

// errArtifactTooLarge reports a stored artifact that inflates past the cap.
var errArtifactTooLarge = errors.New("decompressed artifact exceeds the size cap")

// errArtifactTruncated reports a stored gzip member too short to carry the
// trailer its own size is read from.
var errArtifactTruncated = errors.New("stored artifact is truncated")

// compressArtifact gzips data, and reports whether the result is worth
// storing instead of the original.
func compressArtifact(data []byte) ([]byte, bool) {
	var buf bytes.Buffer

	// Sized for the best case, so a compressible body does not grow the
	// buffer repeatedly on the way in.
	buf.Grow(len(data)/2 + 64)

	zw := gzip.NewWriter(&buf)

	if _, err := zw.Write(data); err != nil {
		// A bytes.Buffer does not fail, so this is unreachable; treating it
		// as "not worth compressing" keeps the artifact storable either way.
		_ = zw.Close()

		return nil, false
	}

	if err := zw.Close(); err != nil {
		return nil, false
	}

	if float64(buf.Len()) >= float64(len(data))*compressionRatio {
		return nil, false
	}

	return buf.Bytes(), true
}

// decompressArtifact inflates a stored gzip member and checks it against the
// digest its reference names.
//
// The digest check is what separates "this evidence has been corrupted" from
// "here is some evidence": a reference is a content address, so bytes that do
// not hash to it are not the artifact that was asked for, whatever they are
// (Story 4.8, AC6).
func decompressArtifact(ref string, stored []byte, maxBytes int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		// Present and not what it claims to be, which is corruption rather
		// than absence: the key is there, and what is under it is not the
		// artifact (Story 8.2, AC6).
		return nil, fmt.Errorf("artifact %s does not decompress: %w: %w", ref, ErrCorrupt, err)
	}

	defer func() {
		_ = zr.Close()
	}()

	// One byte past the cap, so hitting the limit is distinguishable from an
	// artifact that is exactly as large as the cap allows.
	data, err := io.ReadAll(io.LimitReader(zr, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("artifact %s does not decompress: %w: %w", ref, ErrCorrupt, err)
	}

	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("reading artifact %s: %w", ref, errArtifactTooLarge)
	}

	if err := verifyArtifactDigest(ref, data); err != nil {
		return nil, err
	}

	return data, nil
}

// verifyArtifactDigest checks decompressed bytes against the digest in their
// reference. A reference wsaw did not write — one with no digest in it — is
// not a reason to refuse: the caller asked for a file, and the store's own
// traversal check already decided the file is one it may read.
func verifyArtifactDigest(ref string, data []byte) error {
	_, digest, ok := strings.Cut(strings.TrimSuffix(ref, compressedSuffix), "/")
	if !ok || len(digest) != sha256.Size*2 {
		return nil
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("artifact %s: %w", ref, errArtifactDigestMismatch)
	}

	return nil
}

// errArtifactDigestMismatch reports stored bytes that are not the artifact
// their reference names.
//
// It wraps ErrCorrupt because that is what it is from every caller's point of
// view: the object is present and is not the evidence (Story 8.2, AC6). The
// sentinel underneath stays, so a test can name the specific failure.
var errArtifactDigestMismatch = fmt.Errorf("%w: stored bytes do not match the reference's digest", ErrCorrupt)

// gzipSize reads the uncompressed size a gzip member declares in its trailer.
//
// It exists so StatArtifact can answer with the size of the evidence rather
// than the size of the file holding it, without inflating a megabyte of
// JavaScript to count the bytes (Story 4.8, AC7). The trailer is the member's
// own claim and is modulo 2^32, which is sound here because nothing wsaw
// writes approaches 4 GiB — and because the number is shown to a human, never
// used to size a buffer.
func gzipSize(trailer []byte) (int64, bool) {
	if len(trailer) < 4 {
		return 0, false
	}

	t := trailer[len(trailer)-4:]
	size := int64(t[0]) | int64(t[1])<<8 | int64(t[2])<<16 | int64(t[3])<<24

	return size, true
}
