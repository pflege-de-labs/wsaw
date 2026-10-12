package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// This file is the door onto the one kind of object wsaw keeps in the artifact
// bucket that is not an artifact: the marker that says a rebuild of the index
// is running against this bucket and that a sweep must collect nothing while
// it is (Story 8.10, AC12).
//
// It is a second door and not a widening of the first: bucket.go admits exactly
// "<kind>/<sha256hex>" and nothing else, and the value of that whitelist is
// that it has to be right about one shape (Tenet 9). What it borrows rather
// than duplicates is bucket.do — the retry policy, the per-request deadline and
// the classification of transient failures — so there is one answer to "is
// this failure worth another attempt".
//
// The key space is disjoint from the artifacts' by its first byte. An artifact
// key is "<kind>/<sha256hex>" and validKind is a closed list spelled in
// [a-z0-9-]; a marker key begins with "_", which no kind can, so neither sweep
// has to know the other's shapes to leave them alone (Story 8.5, AC4).
//
// The key layout is fixed by the buckets already in service: markers written by
// earlier builds are at these keys, and a sweep that stopped finding them would
// collect the evidence a running rebuild is protecting.

// errInvalidIndexKey is what a key outside the marker grammar is refused with.
var errInvalidIndexKey = errors.New("invalid index key")

const (
	// indexRebuildPrefix holds the in-progress markers, in a directory of their
	// own so that finding them is one bounded prefix listing.
	indexRebuildPrefix = "_wsaw/index/v1/rebuild/"

	// markerLayout is the shape of a marker body this build writes and reads.
	// It moves with the "v1" in indexRebuildPrefix.
	markerLayout = 1

	// markerIDMax bounds the hex digits after markerIDPrefix.
	//
	// newRebuildMarker mints thirty-two of them, and the door admits any number
	// up to this bound rather than exactly that many, so that a change of
	// entropy is not a change of key space that leaves markers already in a
	// bucket unreadable.
	markerIDMax = 64

	// markerIDPrefix is what every marker id starts with, so that a key under
	// this directory reads as what it is to whoever is looking at the bucket.
	markerIDPrefix = "rebuild-"

	// markerListPageSize is the most keys every provider gocloud reaches will
	// return in one listing response, so it is the fewest requests a listing
	// can cost.
	markerListPageSize = 1000
)

// rebuildKey names the marker of the rebuild with this id.
func rebuildKey(id string) string {
	return indexRebuildPrefix + id
}

// parseRebuildKey reads a marker's id back out of its key, refusing any key
// that is not one rebuildKey could have produced.
func parseRebuildKey(key string) (string, error) {
	leaf, ok := strings.CutPrefix(key, indexRebuildPrefix)
	if !ok || !validMarkerID(leaf) {
		return "", fmt.Errorf("index key %q is not one this store writes: %w",
			truncateForMessage(key), errInvalidIndexKey)
	}

	return leaf, nil
}

// validMarkerID accepts exactly the shape newRebuildMarker produces, so that a
// key that lists is a key the parser here will accept.
func validMarkerID(s string) bool {
	leaf, ok := strings.CutPrefix(s, markerIDPrefix)

	return ok && leaf != "" && len(leaf) <= markerIDMax && isHex(leaf)
}

// putMarker creates the marker with this id and never rewrites one.
//
// Markers are not content-addressed — one is named after its run, not its
// body — so the conditional create is what keeps one run from writing over
// another's on the providers that honour it. A key that is already there is
// not an error: distinct runs mint distinct ids, so it can only be a retry of
// this write.
//
// Atomicity comes from the driver, as it does for an artifact: an interrupted
// write leaves no key rather than a half-written one.
func (b *bucket) putMarker(ctx context.Context, id string, body []byte) error {
	key := rebuildKey(id)
	if _, err := parseRebuildKey(key); err != nil {
		return err
	}

	if err := b.do(ctx, "writing an index object", func(ctx context.Context) error {
		w, err := b.b.NewWriter(ctx, key, &blob.WriterOptions{
			ContentType: artifactContentType,
			IfNotExist:  true,
			BeforeWrite: restrictLocalFilePermissions,
		})
		if err != nil {
			return err
		}

		if _, err := w.Write(body); err != nil {
			// Close abandons the partial write. Its error is dropped because the
			// one worth reporting is the one from Write.
			_ = w.Close()

			return err
		}

		if err := w.Close(); err != nil && gcerrors.Code(err) != gcerrors.FailedPrecondition {
			return err
		}

		return nil
	}); err != nil {
		return markerError("writing", key, err)
	}

	return nil
}

// getMarker reads one marker whole; a marker is a few hundred bytes.
//
// The key usually comes from a listing, which is bucket content rather than
// something wsaw chose, so it is parsed before it is used (Tenet 9).
func (b *bucket) getMarker(ctx context.Context, key string) ([]byte, error) {
	if _, err := parseRebuildKey(key); err != nil {
		return nil, err
	}

	var body []byte

	if err := b.do(ctx, "reading an index object", func(ctx context.Context) error {
		read, err := b.b.ReadAll(ctx, key)
		if err != nil {
			return err
		}

		body = read

		return nil
	}); err != nil {
		return nil, markerError("reading", key, err)
	}

	return body, nil
}

// deleteMarker removes one marker. A key that is already gone is reported as
// ErrNotFound, so "somebody else collected it" can be treated as the success
// it is.
func (b *bucket) deleteMarker(ctx context.Context, key string) error {
	if _, err := parseRebuildKey(key); err != nil {
		return err
	}

	if err := b.do(ctx, "deleting an index object", func(ctx context.Context) error {
		return b.b.Delete(ctx, key)
	}); err != nil {
		return markerError("deleting", key, err)
	}

	return nil
}

// listMarkers returns the key of every object under the marker directory,
// unparsed: what a reader does with one that does not parse is its decision.
//
// The token only advances on a successful page, so a retried page resumes
// rather than skips.
func (b *bucket) listMarkers(ctx context.Context) ([]string, error) {
	var keys []string

	for token := blob.FirstPageToken; len(token) > 0; {
		var (
			page []*blob.ListObject
			next []byte
		)

		if err := b.do(ctx, "listing index objects", func(ctx context.Context) error {
			objects, nextToken, err := b.b.ListPage(ctx, token, markerListPageSize,
				&blob.ListOptions{Prefix: indexRebuildPrefix})
			if err != nil {
				return err
			}

			page, next = objects, nextToken

			return nil
		}); err != nil {
			return nil, fmt.Errorf("listing index objects under %q in bucket %s: %w",
				indexRebuildPrefix, b, err)
		}

		for _, obj := range page {
			// Only a listing with a delimiter reports directories; skipped
			// anyway, since one is a name no writer wrote.
			if !obj.IsDir {
				keys = append(keys, obj.Key)
			}
		}

		token = next
	}

	return keys, nil
}

// markerError gives a bucket failure on a marker the shape the rest of the
// store understands: a missing key is ErrNotFound, as it is for an artifact.
func markerError(verb, key string, err error) error {
	if gcerrors.Code(err) == gcerrors.NotFound {
		return fmt.Errorf("index object %s: %w", key, ErrNotFound)
	}

	return fmt.Errorf("%s index object %s: %w", verb, key, err)
}
