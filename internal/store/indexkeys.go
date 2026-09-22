package store

import (
	"errors"
	"fmt"
	"strings"
)

// This file is the key space wsaw writes in the artifact bucket that is not an
// artifact.
//
// There is exactly one such object today — the marker that says a rebuild of
// the index is running against this bucket and that a sweep must collect
// nothing while it is (Story 8.10) — and it still needs a key space of its own,
// because it is not evidence and must never be mistaken for it. A sweep walks
// the bucket deciding what nothing references any more, and an object it cannot
// account for is an object it would delete.
//
// The separation is a proof rather than a reserved-word list anyone has to
// maintain. An artifact key is "<kind>/<sha256hex>" and validKind is a closed
// list — body, result, probe, screenshot and screenshot-<suffix> — every member
// of which is spelled in [a-z0-9] with an internal "-". No kind begins with
// "_", and none can while validKind stays an allow-list, so the two key spaces
// are disjoint at their first byte whichever way either grammar later grows,
// and neither sweep has to know the other's shapes to leave them alone
// (Story 8.5, AC4).
//
// "index/" would not have done. "index" is a legal kind the moment someone adds
// it to validKind, and the collision would appear years later in a bucket that
// already held both.

// errInvalidIndexKey is what a key outside this grammar is refused with.
var errInvalidIndexKey = errors.New("invalid index key")

const (
	// indexRoot is the prefix every non-artifact object of wsaw's lives under.
	indexRoot = "_wsaw" + refSeparator

	// indexPrefix roots the current layout. The "v1" here and markerLayout
	// below move together: a marker body claiming a layout the tree is not
	// spelled for would be a bucket that lies about itself.
	indexPrefix = indexRoot + "index" + refSeparator + "v1" + refSeparator

	// indexRebuildPrefix holds the in-progress markers. It is a directory of
	// its own so that listing them is one bounded prefix listing rather than a
	// walk of anything else.
	indexRebuildPrefix = indexPrefix + "rebuild" + refSeparator

	// markerLayout is the shape of a marker body this build writes and reads.
	markerLayout = 1

	// markerIDMax bounds the hex digits after the "rebuild-" prefix.
	//
	// newRebuildMarker mints thirty-two of them — sixteen random bytes, so two
	// runs against one bucket cannot collide on an id and clear each other's
	// protection — and the door admits any number up to this bound rather than
	// exactly that many. The id is the one part of the key nothing else
	// derives, so pinning the grammar to today's width would make a change of
	// entropy a change of key space, and every marker already in a bucket
	// unreadable by the build that made it.
	markerIDMax = 64

	// markerIDPrefix is what every marker id starts with, so that a key under
	// this directory reads as what it is to whoever is looking at the bucket.
	markerIDPrefix = "rebuild-"

	// maxIndexComponentBytes bounds one path segment, and maxIndexKeyBytes one
	// whole key. Neither is reachable by the grammar as it stands — the
	// longest key this file can produce is about 60 bytes — and that is the
	// point: they are the guard that keeps it that way as the grammar grows,
	// checked at the door rather than discovered as a write that succeeds on
	// S3 and fails on the local disk.
	maxIndexComponentBytes = 200
	maxIndexKeyBytes       = 900
)

// rebuildKey names the marker that says a rebuild of the index is in progress
// and that nothing may be collected while it is (Story 8.10).
func rebuildKey(id string) string {
	return indexRebuildPrefix + id
}

// parseRebuildKey reads a rebuild marker's id back out of its key.
func parseRebuildKey(key string) (string, error) {
	leaf, ok := strings.CutPrefix(key, indexRebuildPrefix)
	if !ok || !validMarkerID(leaf) {
		return "", indexKeyError(key)
	}

	return leaf, nil
}

// validMarkerID accepts exactly what newRebuildMarker produces.
//
// By construction rather than by approximation: the ids are minted in this
// package from a fixed prefix and a fixed number of hex digits, so the check
// that admits a write to this key space can be the same shape as the mint, and
// a key that lists is a key some parser here will accept.
func validMarkerID(s string) bool {
	leaf, ok := strings.CutPrefix(s, markerIDPrefix)

	return ok && leaf != "" && len(leaf) <= markerIDMax && isHex(leaf)
}

// wellFormedIndexKey reports whether key is one of the productions above.
//
// It answers by parsing, so the door that admits a write and the parser that
// reads a listing back cannot drift into two ideas of what a key is.
func wellFormedIndexKey(key string) bool {
	if !strings.HasPrefix(key, indexRebuildPrefix) {
		return false
	}

	_, err := parseRebuildKey(key)

	return err == nil
}

// indexKeyError reports a key that is not one this grammar defines, with the
// key truncated: a malformed key can carry a page's payload into a log line,
// and this is the same defence truncateForMessage gives an artifact reference.
func indexKeyError(key string) error {
	return fmt.Errorf("index key %q is not one this store writes: %w",
		truncateForMessage(key), errInvalidIndexKey)
}
