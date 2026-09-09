package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is the key grammar of the bucket index (Story 8.10): every key
// that index writes is built here and parsed here, and nothing else in the
// package concatenates one.
//
// It is pure — no bucket, no context, no clock of its own — because in a store
// whose index is a set of object names, the interesting invariants are all
// properties of the names, and a property of a pure function is one a table
// test and a fuzz target can pin down in milliseconds. A key bug in a store
// like this does not announce itself: it merges two targets' histories, or it
// hides a scan behind a name nothing lists, and it does so quietly.
//
// Three things the rest of the package leans on and this file is answerable
// for:
//
//   - Nothing page-controlled chooses a key shape (Tenet 9). A target is a
//     URL; a scan ID and a consent mode are page-adjacent. All three reach a
//     key only through an encoder that is total, that produces a fixed
//     alphabet, and whose result is a named type — so the one way to hold a
//     key component is to have encoded a value into one, and a raw string
//     cannot be passed off as one by accident.
//   - Lexicographic order of the keys is the order a reader wants. Newest
//     first comes from an inverted timestamp, and the three record kinds that
//     share a series directory come back grouped, because the bucket sorts and
//     this store does not (see the listing-order invariant on indexRoot's
//     neighbours below, and TestIndexListingOrderIsTheOrderAFoldNeeds).
//   - An index key can never be an artifact key, and an artifact key can never
//     be an index key. See indexRoot for why that is a proof and not a
//     convention.

// errInvalidIndexKey marks a key that is not one this grammar defines.
//
// It is a sentinel for the same reason errInvalidRef is: a malformed index key
// is a bug in wsaw or an object some other tool left in the bucket, and either
// way it is a different thing from a key that is well formed and absent.
var errInvalidIndexKey = errors.New("invalid index key")

const (
	// indexRoot is the first segment of every key the bucket index writes, and
	// it is the whole of the argument that the index and the evidence can
	// share one bucket without either being able to name the other's objects.
	//
	// The argument is a proof rather than a reserved-word list anyone has to
	// maintain. An artifact key is "<kind>/<sha256hex>" and validKind is a
	// closed list — body, result, probe, screenshot and screenshot-<suffix> —
	// every member of which is spelled in [a-z0-9] with an internal "-", with
	// the suffix checked byte-wise in that same alphabet. No kind begins with
	// "_", and none can while validKind stays an allow-list. So the two key
	// spaces are disjoint at their first byte, whichever way either grammar
	// later grows, and no sweep on either side has to know the other's shapes
	// to leave them alone (Story 8.5, AC4).
	//
	// "index/" would not have done. "index" is a legal kind the moment someone
	// adds it to validKind, and the collision would appear years later in a
	// bucket that already held both.
	indexRoot = "_wsaw" + refSeparator

	// indexLayoutVersion is the layout this build reads and writes. It is the
	// index's equivalent of a schema version: a store refuses an index written
	// by a newer layout rather than misreading it (AC14).
	indexLayoutVersion = 1

	// indexLayoutPrefix holds the single object that records which layout
	// wrote this index. It sits outside the versioned tree on purpose — the
	// version is what a reader has to learn before it knows which tree to
	// read, so it cannot live inside one.
	indexLayoutPrefix = indexRoot + "index" + refSeparator + "layout" + refSeparator

	// indexPrefix roots everything the current layout writes. The "v1" here
	// and indexLayoutVersion above move together; a test asserts they agree,
	// because a version object claiming 2 over a tree spelled v1 would be an
	// index that lies about itself.
	indexPrefix = indexRoot + "index" + refSeparator + "v1" + refSeparator

	// The eight prefixes the layout is made of. Each is its own directory, and
	// which of them a question is answered from is the whole of the query plan
	// (AC4): a read is a listing of one of these, never a walk of the bucket.
	//
	// They also carry the listing-order invariant, which is what lets every
	// fold in this store trust the order a listing arrives in instead of
	// sorting it.
	//
	// The hazard is fileblob. It lists through filepath.WalkDir, which sorts
	// the entries of each directory and then descends, and it does not sort
	// the keys that walk produces — it only swaps a pair that came out
	// adjacent and inverted. A cloud provider sorts the whole key. The two
	// agree unless, inside some directory a listing walks, one entry's name is
	// a proper prefix of a sibling's and the sibling's next byte sorts below
	// "/" (0x2f). Then the walk descends into the shorter name first while the
	// key order puts the longer one ahead, because "-" (0x2d) and "." (0x2e)
	// are below "/". A directory holding both an object "x-b" and a directory
	// "x/" is the everyday instance of that rule and the one §4.2 states; two
	// sibling directories, "screenshot/" and "screenshot-after-consent/", are
	// the same rule and it is the one that bites here.
	//
	// Every prefix below satisfies it. targets, audit, auditckpt and rebuild
	// hold objects only, and their leaf names are fixed-width or separated by
	// "." at a fixed position, so no leaf is a prefix of a sibling. series,
	// byid and baseline hold only <tk> directories, then only <mk>
	// directories, then only objects: two distinct targets have distinct
	// 32-hex identities in the first 32 bytes of <tk>, so neither <tk> can be
	// a prefix of the other, and the four <mk> spellings — none, reject,
	// accept, and "_" plus 32 hex — are pairwise non-prefixed.
	//
	// indexRefPrefix is the exception, and it is a deliberate one. See its own
	// comment below.
	indexTargetsPrefix   = indexPrefix + "targets" + refSeparator
	indexSeriesPrefix    = indexPrefix + "series" + refSeparator
	indexByIDPrefix      = indexPrefix + "byid" + refSeparator
	indexBaselinePrefix  = indexPrefix + "baseline" + refSeparator
	indexAuditPrefix     = indexPrefix + "audit" + refSeparator
	indexAuditCkptPrefix = indexPrefix + "auditckpt" + refSeparator

	// indexRefPrefix holds one directory per artifact — "<kind>/<digest>/" —
	// and below it one zero-byte object per owner.
	//
	// **It must be listed one kind at a time, never as a whole.** Its first
	// level holds the kinds validKind admits, and that set contains
	// "screenshot" alongside "screenshot-before-consent" and
	// "screenshot-after-consent": a name that is a proper prefix of a sibling
	// whose next byte is "-", which is exactly the case the rule above
	// excludes. Listing ref/ entire therefore returns one order on fileblob
	// and another on S3, and a test asserts that it does rather than leaving
	// the claim to be rediscovered.
	//
	// This costs nothing the sweep was not already paying. The artifact key
	// space it is joined against is "<kind>/<digest>" at the bucket root and
	// has the identical property for the identical reason, so a whole-bucket
	// walk of the evidence is not order-stable either. Both sides of §7.4's
	// merge join have to be streamed per kind, and per kind both are: within
	// ref/<kind>/ every child is a 64-hex digest directory and within <kind>/
	// every child is a 64-hex object, and no fixed-width name can be a prefix
	// of a sibling.
	//
	// Flattening the two segments into "ref/<kind>.<digest>/" would remove the
	// exception, and it was rejected: it buys order-stability only for a
	// listing nothing performs, and it costs the property that a marker's key
	// contains the artifact's key verbatim, which is what makes the merge join
	// a string comparison rather than a reassembly.
	indexRefPrefix = indexPrefix + "ref" + refSeparator

	indexRebuildPrefix = indexPrefix + "rebuild" + refSeparator

	// fieldSeparator joins the fields of one key's last segment. It is "." and
	// no component alphabet contains it, so every leaf splits unambiguously —
	// which stops mattering in the abstract and starts mattering the moment a
	// termination is "request-cap" and a scan ID is "scan-1", where a "-"
	// separator would have made the split a guess.
	fieldSeparator = "."

	// escapeMarker prefixes a component that had to be hashed because its
	// value was not in the literal alphabet. No literal alphabet contains it,
	// so the escaped and the literal forms of every component are disjoint and
	// every encoder below is injective by construction rather than by luck.
	escapeMarker = "_"

	// slugSeparator joins a target's hashed identity to its readable slug, and
	// stands in for every byte a slug had to drop.
	slugSeparator = "-"

	// slugFallback is what a target whose every byte is outside [a-z0-9]
	// slugifies to. The slug is decoration; the identity is the hash in front
	// of it, so an unreadable target loses its decoration and nothing else.
	slugFallback = "x"

	// The three record kinds that share a series directory, tagged by their
	// first field. The tags are one byte and they are chosen so that "d" < "k"
	// < "r": a single listing of the directory therefore returns every
	// tombstone, then every checkpoint, then every entry, all three newest
	// first. That is what lets a fold answer from one visible key set instead
	// of from three listings that may disagree with one another (AC7).
	seriesTombstoneTag  = "d"
	seriesCheckpointTag = "k"
	seriesEntryTag      = "r"

	// The three kinds of owner a ref marker can have. They share their letters
	// with the series tags by coincidence rather than by meaning: an owner
	// lives in a different directory and is parsed by a different function, so
	// nothing reads one as the other.
	//
	// The tags are chosen so that "d" < "r" < "t": a listing of one artifact's
	// pin directory therefore returns every decision pin, then every result
	// pin, then every take. Retention reads that order — the pins that answer
	// "does anything reference this" come first, and the takes that answer
	// "has a scan that has not finished stored these bytes" come after them,
	// so the question that decides whether an object may be deleted is
	// answered by the front of the first page.
	ownerResultTag   = "r"
	ownerDecisionTag = "d"
	ownerTakeTag     = "t"

	// identityHexLen is the hex width of every truncated digest that carries
	// an identity: a hashed target, a hashed mode or scan ID, a decision body,
	// a checkpoint body. 128 bits, not 64, because a collision here does not
	// degrade a lookup — it merges two targets' histories, keeps the wrong
	// compliance decision, or discards a checkpoint, all of which are
	// correctness failures. Thirty-two bytes of key is free; the layout has
	// hundreds of bytes of headroom.
	identityHexLen = 32

	// termHexLen is the hex width of a hashed termination reason. Sixty-four
	// bits is enough here and nowhere else: a termination is drawn from a
	// closed set this repository ships, a collision would only mis-file a
	// scan among six known values, and the field is on the hot path of every
	// entry key.
	termHexLen = 16

	// invWidth is the number of digits an inverted timestamp is padded to.
	// MaxInt64 has nineteen, so a fixed nineteen means the field is the same
	// width for every instant and no later field of the key can influence the
	// order of two different instants.
	invWidth = 19

	// stampLayout renders the readable half of the ordering pair. It exists so
	// that an operator looking at a bucket browser sees a date and not only a
	// nineteen-digit number; nothing ever reads it back as a fact.
	stampLayout = "20060102T150405Z"

	// targetSlugMax bounds the readable part of a target component, scanIDMax
	// and termMax the literal forms of a scan ID and a termination. The
	// numbers exist so that the longest path component this grammar can
	// produce — a ref marker's owner, at "r." plus 81 plus 33 plus 64 plus two
	// separators, about 182 bytes — stays inside the 255-byte component limit
	// that ext4, APFS and NTFS all impose, and far inside S3's 1,024-byte key
	// limit. That is the number that decides whether the layout is
	// filesystem-safe, and a fuzz test asserts it.
	targetSlugMax = 48
	scanIDMax     = 64
	termMax       = 24

	// layoutDigits is the zero-padded width of the layout version in its key,
	// so that layout 2 and layout 10 sort in numeric order. layoutSuffix is
	// the one extension this grammar uses: the layout object is the single
	// index object a person is expected to open by hand.
	layoutDigits = 8
	layoutSuffix = ".json"

	// maxIndexComponentBytes bounds one path segment, and maxIndexKeyBytes one
	// whole key. Neither is reachable by the grammar as it stands — the
	// longest component is about 182 bytes and the longest key about 250 — and
	// that is the point: they are the guard that keeps it that way as the
	// grammar grows, checked at the door rather than discovered as a write
	// that succeeds on S3 and fails on the local disk.
	maxIndexComponentBytes = 200
	maxIndexKeyBytes       = 900
)

// indexEpoch and indexHorizon bound the instants this grammar can order.
//
// They are the range of time.Time that survives a round trip through Unix
// nanoseconds, which is what the ordering field is. Outside it, time.UnixNano
// is documented as undefined rather than saturating, so the bound is tested
// against the time values themselves; a naive min/max over UnixNano would
// silently wrap a year-3000 timestamp into the middle of the ordering.
var (
	indexEpoch   = time.Unix(0, 0)
	indexHorizon = time.Unix(0, math.MaxInt64)
)

// Encoded key components. Each is a named type rather than a string so that
// the only way to hold one is to have produced it with the matching encoder
// or to have parsed it out of a key that already existed. A raw target,
// consent mode or scan ID cannot be spliced into a key by mistake, which is
// the compile-time half of Tenet 9's promise; the encoders below are the
// run-time half.
type (
	// encodedTarget is the <tk> component: a target reduced to an identity and
	// a decoration.
	encodedTarget string

	// encodedMode is the <mk> component: a consent mode, literal when it is
	// one of the three wsaw knows and hashed when it is not.
	encodedMode string

	// encodedScan is the <sk> component: a scan ID, literal when its shape
	// allows and hashed when it does not.
	encodedScan string

	// encodedTerm is the <tm> component: why capture stopped, carried in the
	// key because PreviousResult has to skip error and skipped scans and would
	// otherwise pay a GET per failure to find that out.
	encodedTerm string
)

// shortSum is the truncated SHA-256 every escaped component and every body
// digest in this grammar is built from. Truncation is safe here because the
// hash is used for identity and not for authentication: nothing in this store
// treats a matching digest as proof that a body was not tampered with, and the
// widths are argued where they are declared.
func shortSum(b []byte, hexLen int) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])[:hexLen]
}

// tk encodes a target as a key component: 128 bits of its SHA-256, a dash, and
// a readable slug.
//
// The target is always hashed, and each of these four reasons would be enough
// on its own. A case-folding filesystem stores "Site" and "site" as one
// directory, which would merge two targets' histories on macOS and not on
// Linux. A target may be a 2,000-byte URL against a 255-byte path component.
// S3 caps a key at 1,024 bytes. And NFC and NFD spellings of one URL are
// different byte strings that some filesystems normalise into one name.
//
// The slug that follows is decoration and is never read back: tk is parsed
// positionally, the first 32 bytes are the identity, and everything after the
// first dash cannot affect it. It is there so that a person looking at the
// bucket can tell which target a directory belongs to.
func tk(target string) encodedTarget {
	return encodedTarget(shortSum([]byte(target), identityHexLen) + slugSeparator + slug(target, targetSlugMax))
}

// mk encodes a consent mode. The three modes wsaw scans in appear literally,
// because a directory named "reject" is worth more to whoever has to debug
// this bucket than a hash is; anything else is hashed behind the escape
// marker.
//
// What counts as one of the three is model.ConsentMode.Valid, so this grammar
// and the schema cannot drift into two answers to that question.
func mk(mode model.ConsentMode) encodedMode {
	if mode.Valid() {
		return encodedMode(mode)
	}

	return encodedMode(escapeMarker + shortSum([]byte(mode), identityHexLen))
}

// sk encodes a scan ID. wsaw's own scan IDs pass through literally — they are
// hex with a "scan-" prefix — which keeps a byid key readable and a support
// question answerable by eye. An ID from anywhere else, including the entropy
// failure fallback and anything a caller invents, is hashed.
func sk(scanID string) encodedScan {
	if literalToken(scanID, scanIDMax, false) {
		return encodedScan(scanID)
	}

	return encodedScan(escapeMarker + shortSum([]byte(scanID), identityHexLen))
}

// tm encodes a termination reason. Every reason model defines is literal;
// a reason from a document written by some future version is hashed rather
// than refused, because an entry whose termination this build does not
// recognise is still an entry and must still be listed (Tenet 5).
func tm(term model.TerminationReason) encodedTerm {
	if literalToken(string(term), termMax, true) {
		return encodedTerm(term)
	}

	return encodedTerm(escapeMarker + shortSum([]byte(term), termHexLen))
}

// slug reduces a string to at most limit bytes of [a-z0-9-].
//
// Lowercase, because a case-folding filesystem would otherwise make two slugs
// one directory; every other byte becomes a dash, runs of dashes collapse, and
// the ends are trimmed. Truncation is by bytes and needs no rune care, because
// by the time it happens every byte is ASCII.
func slug(s string, limit int) string {
	var b strings.Builder

	b.Grow(len(s))

	// Starting "in a dash" is what suppresses a leading one; the same flag
	// collapses runs, so the two rules are one piece of state.
	dash := true

	for i := range len(s) {
		c := s[i]

		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))

			dash = false
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)

			dash = false
		case !dash:
			b.WriteByte('-')

			dash = true
		}
	}

	out := strings.TrimSuffix(b.String(), slugSeparator)
	if len(out) > limit {
		out = out[:limit]
	}

	// Again after truncating, which is the one operation that can put a dash
	// back on the end.
	out = strings.TrimSuffix(out, slugSeparator)
	if out == "" {
		return slugFallback
	}

	return out
}

// literalToken reports whether s may appear in a key as itself: at most limit
// bytes of [a-z0-9-], starting with a letter or digit, or with a letter alone
// when letterFirst is set.
//
// It is deliberately not a regexp. The alphabet is the point of the function
// and a byte loop states it in the same terms the rest of this package's
// validators use, with no dependency on a pattern being read as carefully as
// it was written.
func literalToken(s string, limit int, letterFirst bool) bool {
	if s == "" || len(s) > limit {
		return false
	}

	switch first := s[0]; {
	case first >= 'a' && first <= 'z':
	case first >= '0' && first <= '9' && !letterFirst:
	default:
		return false
	}

	for i := 1; i < len(s); i++ {
		c := s[i]

		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}

	return true
}

// clampNano reduces an instant to the nanoseconds this grammar can order, and
// reports whether it had to.
//
// Out of range is clamped and never dropped: a scan with a wrong clock is
// still a scan, and refusing to key it would lose evidence to a mistake made
// elsewhere (Tenet 5). The caller reports the clamping, because only the
// caller holds the scan_id, target and consent_mode that a warning about it
// has to carry (AGENTS §4).
//
// The bounds are compared as times rather than as nanoseconds because
// time.Time.UnixNano is documented as undefined outside 1678–2262: reading it
// first and clamping afterwards would wrap a far-future timestamp into the
// middle of the ordering instead of pinning it at the end.
func clampNano(t time.Time) (int64, bool) {
	switch {
	case t.Before(indexEpoch):
		return 0, true
	case t.After(indexHorizon):
		return math.MaxInt64, true
	default:
		return t.UnixNano(), false
	}
}

// inv renders an instant as the field that orders a listing: MaxInt64 minus
// the nanosecond, zero-padded to a fixed nineteen digits.
//
// Inversion, because blob.ListOptions offers neither a reverse listing nor a
// start-after cursor, so making the bucket's ascending order mean newest-first
// is the only way to get AC4's "the listing is already in the required order"
// rather than sorting what a listing returned. Nanosecond resolution, because
// two scans of one target can start in the same millisecond and their order
// still has to be strict.
func inv(t time.Time) string {
	nanos, _ := clampNano(t)

	// The subtraction cannot overflow: clampNano's result is in [0, MaxInt64],
	// so the difference is too.
	return fmt.Sprintf("%0*d", invWidth, math.MaxInt64-nanos)
}

// instantAt inverts inv: the instant an ordering field was built from.
//
// It exists because the ordering field is not decoration — it is the only
// record of a scan's start time that a read can have without fetching a body,
// and two paths need it that way. Retention by age decides which entries are
// past their keep-until from the listing alone (§7.1), and a listing that finds
// an entry object damaged still has to report when the scan it names started
// rather than pretend it has no time at all (Tenet 5).
//
// It is exact on everything inv can produce, and the round trip is only lossy
// where inv itself was: an instant clamped at either edge of the range comes
// back as the edge. A field that is not one inv wrote returns the zero time,
// which every caller treats as a value it may not decide anything from.
func instantAt(field string) time.Time {
	if !validInv(field) {
		return time.Time{}
	}

	inverted, err := strconv.ParseUint(field, 10, 64)
	if err != nil || inverted > math.MaxInt64 {
		return time.Time{}
	}

	return time.Unix(0, math.MaxInt64-int64(inverted)).UTC()
}

// literalOrEmpty returns a key component when it is the value itself, and the
// empty string when it is the escaped form of one.
//
// The escaped form is a truncated hash and is deliberately not invertible, so
// there is nothing honest to return for it. Reporting the token as though it
// were the value would put "_4c9f…" in front of a reader as a scan ID, which is
// a spelling of the index rather than a fact about the scan.
func literalOrEmpty(component string) string {
	if strings.HasPrefix(component, escapeMarker) {
		return ""
	}

	return component
}

// stamp renders the readable half of the ordering pair. It is derived from the
// same clamped value inv is, so it is constant whenever inv is and can never
// reorder anything; it is decoration placed after the field that sorts.
func stamp(t time.Time) string {
	nanos, _ := clampNano(t)

	return time.Unix(0, nanos).UTC().Format(stampLayout)
}

// seriesID is one target-and-consent-mode history, named by its encoded
// components.
//
// It is a type rather than two string arguments because every key below takes
// both, in the same order, and a caller that swapped them would produce keys
// that are individually well formed, that pass every validator, and that file
// a scan under a target nobody will ever list.
type seriesID struct {
	target encodedTarget
	mode   encodedMode
}

// seriesFor encodes a target and a consent mode into a series identity. It is
// the only way to build one from raw values, which is what makes "a page
// cannot choose a key shape" checkable rather than a matter of review.
func seriesFor(target string, mode model.ConsentMode) seriesID {
	return seriesID{target: tk(target), mode: mk(mode)}
}

// markerKey is the object that records that this series exists.
//
// It is one flat object per series rather than a directory, because Series()
// wants every target and mode in one listing and nothing else about them.
func (s seriesID) markerKey() string {
	return indexTargetsPrefix + string(s.target) + fieldSeparator + string(s.mode)
}

// parseTargetMarkerKey reads a series identity back out of its marker.
func parseTargetMarkerKey(key string) (seriesID, error) {
	leaf, ok := strings.CutPrefix(key, indexTargetsPrefix)
	if !ok {
		return seriesID{}, indexKeyError(key)
	}

	target, mode, found := strings.Cut(leaf, fieldSeparator)
	if !found {
		return seriesID{}, indexKeyError(key)
	}

	s, ok := seriesFromSegments(target, mode)
	if !ok {
		return seriesID{}, indexKeyError(key)
	}

	return s, nil
}

// dirPrefix is the one directory a fold of this series lists. Everything a
// fold needs — the tombstones, the checkpoints and the entries — is in it, so
// the answer to "what is this target's history" is a function of one visible
// key set rather than of three listings taken at three moments.
func (s seriesID) dirPrefix() string {
	return indexSeriesPrefix + string(s.target) + refSeparator + string(s.mode) + refSeparator
}

// byIDDirPrefix holds one object per scan, addressed by its ID.
//
// It is a separate prefix from the series directory and not a fourth tag
// within it, because it is point-addressed and never folded: ten thousand
// scans of one target would otherwise put ten thousand keys in front of every
// listing that only wanted the newest few.
func (s seriesID) byIDDirPrefix() string {
	return indexByIDPrefix + string(s.target) + refSeparator + string(s.mode) + refSeparator
}

// byIDKey addresses one scan of this series directly.
func (s seriesID) byIDKey(scan encodedScan) string {
	return s.byIDDirPrefix() + string(scan)
}

// parseByIDKey reads the series and the scan back out of a byid key.
func parseByIDKey(key string) (seriesID, encodedScan, error) {
	rest, ok := strings.CutPrefix(key, indexByIDPrefix)
	if !ok {
		return seriesID{}, "", indexKeyError(key)
	}

	segments := strings.Split(rest, refSeparator)
	if len(segments) != 3 || !validEncodedScan(segments[2]) {
		return seriesID{}, "", indexKeyError(key)
	}

	s, ok := seriesFromSegments(segments[0], segments[1])
	if !ok {
		return seriesID{}, "", indexKeyError(key)
	}

	return s, encodedScan(segments[2]), nil
}

// seriesRecordKind is which of the three objects that share a series directory
// a key names. The values are ordered as their tags sort, so a listing returns
// them in this order and a fold that reads a page top to bottom sees every
// tombstone before the checkpoint or entry it might suppress.
type seriesRecordKind int

const (
	// seriesTombstone marks a scan that has been pruned. It is a zero-byte
	// object: everything it says is in its name.
	seriesTombstone seriesRecordKind = iota
	// seriesCheckpoint summarises the entries an earlier compaction folded
	// away, so a long history does not cost a listing proportional to itself.
	seriesCheckpoint
	// seriesEntry is one scan's place in the history.
	seriesEntry
)

// seriesRecord is one parsed key from a series directory.
//
// The three kinds are one type with a kind rather than three types, because
// they arrive together: a fold takes one listing of the directory and has to
// classify what comes back, and three types would make that a switch over
// three parsers at every call site instead of a switch over a field at one.
// The fields each kind does not use stay zero, and String reads only the ones
// its kind carries.
type seriesRecord struct {
	kind   seriesRecordKind
	series seriesID

	// inv orders the record. On an entry it is the scan's start time; on a
	// tombstone it is the start time of the scan being suppressed, so the two
	// carry the same value and a fold can match them without a second listing;
	// on a checkpoint it is the start time of the newest entry it covers.
	inv string

	// stamp and term are carried by an entry alone.
	stamp string
	term  encodedTerm

	// scan is the scan an entry records or a tombstone removes. A checkpoint
	// leaves it empty: it covers many scans rather than one.
	scan encodedScan

	// gen is the digest of a checkpoint's body, and empty on the other two. It
	// makes an interrupted compaction that re-runs write the identical key
	// with the identical bytes rather than a second checkpoint (AC10).
	gen string
}

// entry names one scan's place in this series.
//
// The scan ID comes before the termination because entries that share an
// instant form one contiguous run in the listing, the fold resolves that run
// by scan ID descending exactly as SQL does, and <sk> is therefore the only
// field within a run that may vary. Ordering by termination first would sort a
// tie differently from the SQL stores, and no test in the shared suite would
// have caught it.
func (s seriesID) entry(at time.Time, scan encodedScan, term encodedTerm) seriesRecord {
	return seriesRecord{
		kind:   seriesEntry,
		series: s,
		inv:    inv(at),
		stamp:  stamp(at),
		scan:   scan,
		term:   term,
	}
}

// tombstone names the entry a prune has removed. at is the start time of the
// scan being removed and not the moment of removal, so that the tombstone and
// the entry it suppresses carry the same inv and one listing pairs them.
func (s seriesID) tombstone(at time.Time, scan encodedScan) seriesRecord {
	return seriesRecord{kind: seriesTombstone, series: s, inv: inv(at), scan: scan}
}

// checkpoint names a compaction's summary object. at is the start time of the
// newest entry the checkpoint covers, so checkpoints sort among the entries
// they replace rather than at whatever moment compaction happened to run —
// which is also what keeps the key a pure function of what was folded.
func (s seriesID) checkpoint(at time.Time, gen string) seriesRecord {
	return seriesRecord{kind: seriesCheckpoint, series: s, inv: inv(at), gen: gen}
}

// String renders the record as the key it is stored under.
func (r seriesRecord) String() string {
	var leaf string

	switch r.kind {
	case seriesTombstone:
		leaf = joinFields(seriesTombstoneTag, r.inv, string(r.scan))
	case seriesCheckpoint:
		leaf = joinFields(seriesCheckpointTag, r.inv, r.gen)
	case seriesEntry:
		leaf = joinFields(seriesEntryTag, r.inv, r.stamp, string(r.scan), string(r.term))
	default:
		// A kind outside the three is a programmer error, and the honest
		// answer is a key the door will refuse rather than a plausible-looking
		// one it will accept. Returning the prefix alone does exactly that:
		// validateIndexKey rejects a directory as a key.
		return r.series.dirPrefix()
	}

	return r.series.dirPrefix() + leaf
}

// parseSeriesRecord reads back any of the three keys a series directory holds.
func parseSeriesRecord(key string) (seriesRecord, error) {
	rest, ok := strings.CutPrefix(key, indexSeriesPrefix)
	if !ok {
		return seriesRecord{}, indexKeyError(key)
	}

	segments := strings.Split(rest, refSeparator)
	if len(segments) != 3 {
		return seriesRecord{}, indexKeyError(key)
	}

	s, ok := seriesFromSegments(segments[0], segments[1])
	if !ok {
		return seriesRecord{}, indexKeyError(key)
	}

	record, ok := parseSeriesLeaf(segments[2])
	if !ok {
		return seriesRecord{}, indexKeyError(key)
	}

	record.series = s

	return record, nil
}

// parseSeriesLeaf classifies and validates the last segment of a series key,
// leaving the series itself to its caller.
func parseSeriesLeaf(leaf string) (seriesRecord, bool) {
	fields := strings.Split(leaf, fieldSeparator)

	switch {
	case len(fields) == 3 && fields[0] == seriesTombstoneTag:
		if !validInv(fields[1]) || !validEncodedScan(fields[2]) {
			return seriesRecord{}, false
		}

		return seriesRecord{kind: seriesTombstone, inv: fields[1], scan: encodedScan(fields[2])}, true

	case len(fields) == 3 && fields[0] == seriesCheckpointTag:
		if !validInv(fields[1]) || !validIdentityDigest(fields[2]) {
			return seriesRecord{}, false
		}

		return seriesRecord{kind: seriesCheckpoint, inv: fields[1], gen: fields[2]}, true

	case len(fields) == 5 && fields[0] == seriesEntryTag:
		if !validInv(fields[1]) || !validStamp(fields[2]) ||
			!validEncodedScan(fields[3]) || !validEncodedTerm(fields[4]) {
			return seriesRecord{}, false
		}

		return seriesRecord{
			kind:  seriesEntry,
			inv:   fields[1],
			stamp: fields[2],
			scan:  encodedScan(fields[3]),
			term:  encodedTerm(fields[4]),
		}, true

	default:
		return seriesRecord{}, false
	}
}

// baselineOp is what a decision object records: an approval or a withdrawal.
// It is in the key so that the current baseline is the fold over a listing
// that reads no bodies at all (AC8, §6.5).
type baselineOp string

const (
	// opApprove records a baseline being set.
	opApprove baselineOp = "approve"
	// opRevoke records one being withdrawn. A withdrawal is an entry in the
	// log rather than the deletion of an approval, because a compliance
	// decision whose reversal leaves no trace is not an auditable decision.
	opRevoke baselineOp = "revoke"
)

// valid reports whether op is one of the two decisions this store records.
func (o baselineOp) valid() bool { return o == opApprove || o == opRevoke }

// decisionKey names one baseline decision for one series.
type decisionKey struct {
	series seriesID
	inv    string
	stamp  string
	op     baselineOp

	// did is the digest of the decision body. It makes the key a function of
	// the decision, so two writers of the identical decision at the identical
	// instant write one object rather than two (AC10).
	did string
}

// decision names the object that records a baseline decision taken at at.
func (s seriesID) decision(at time.Time, op baselineOp, did string) decisionKey {
	return decisionKey{series: s, inv: inv(at), stamp: stamp(at), op: op, did: did}
}

// baselineDirPrefix is the log of decisions for one series, newest first.
func (s seriesID) baselineDirPrefix() string {
	return indexBaselinePrefix + string(s.target) + refSeparator + string(s.mode) + refSeparator
}

// String renders the decision as the key it is stored under.
func (d decisionKey) String() string {
	return d.series.baselineDirPrefix() + joinFields(d.inv, d.stamp, string(d.op), d.did)
}

// parseDecisionKey reads a baseline decision back out of its key. The op is
// read from the key and not from the body on purpose: GetBaseline after a
// revocation then costs a listing and no GET at all.
func parseDecisionKey(key string) (decisionKey, error) {
	rest, ok := strings.CutPrefix(key, indexBaselinePrefix)
	if !ok {
		return decisionKey{}, indexKeyError(key)
	}

	segments := strings.Split(rest, refSeparator)
	if len(segments) != 3 {
		return decisionKey{}, indexKeyError(key)
	}

	s, ok := seriesFromSegments(segments[0], segments[1])
	if !ok {
		return decisionKey{}, indexKeyError(key)
	}

	fields := strings.Split(segments[2], fieldSeparator)
	if len(fields) != 4 || !validInv(fields[0]) || !validStamp(fields[1]) ||
		!baselineOp(fields[2]).valid() || !validIdentityDigest(fields[3]) {
		return decisionKey{}, indexKeyError(key)
	}

	return decisionKey{series: s, inv: fields[0], stamp: fields[1], op: baselineOp(fields[2]), did: fields[3]}, nil
}

// auditKey names one entry of the audit log.
//
// The log is flat rather than per-series because that is how it is read: the
// audit view is every decision in time order, and a per-series layout would
// make it one listing per target.
type auditKey struct {
	inv   string
	stamp string
	did   string
}

// auditEntryAt names the audit object for a decision taken at at, whose
// canonical body hashes to did.
func auditEntryAt(at time.Time, did string) auditKey {
	return auditKey{inv: inv(at), stamp: stamp(at), did: did}
}

// auditPointer is the audit key one baseline decision's entry is filed under:
// the decision's own ordering fields and its own body digest, copied across
// rather than recomputed.
//
// Being a pure function of the decision key — and therefore of the decision
// object, which the key is derived from — is the whole of why audit compaction
// can never orphan a baseline. The authoritative record of an approval is the
// decision object, the audit object is a derived pointer at it, and anything
// that finds a decision can work out where its audit entry belongs and put it
// back without a listing and without a second source of truth (§6.5).
func (d decisionKey) auditPointer() auditKey {
	return auditKey{inv: d.inv, stamp: d.stamp, did: d.did}
}

// String renders the audit entry as the key it is stored under.
func (a auditKey) String() string {
	return indexAuditPrefix + joinFields(a.inv, a.stamp, a.did)
}

// parseAuditKey reads an audit entry back out of its key.
func parseAuditKey(key string) (auditKey, error) {
	leaf, ok := strings.CutPrefix(key, indexAuditPrefix)
	if !ok {
		return auditKey{}, indexKeyError(key)
	}

	fields := strings.Split(leaf, fieldSeparator)
	if len(fields) != 3 || !validInv(fields[0]) || !validStamp(fields[1]) || !validIdentityDigest(fields[2]) {
		return auditKey{}, indexKeyError(key)
	}

	return auditKey{inv: fields[0], stamp: fields[1], did: fields[2]}, nil
}

// auditCheckpointKey names a compaction of the audit log. It carries no stamp:
// a checkpoint is machinery rather than a decision, and nobody reads the audit
// checkpoints by eye.
type auditCheckpointKey struct {
	inv string
	gen string
}

// auditCheckpointAt names the audit checkpoint covering entries up to at.
//
// Nothing in this build calls it: compaction of the audit prefix is what writes
// these objects (§7.2) and it is a later step of Story 8.10, while the reader
// this build does have — noAuditCheckpoints — only detects that a checkpoint
// exists and refuses the read. The constructor is here with the rest of the
// grammar, and exercised by the grammar tests, so that the writer arrives to a
// key it cannot spell two ways; the same promise rebuildKey carries for Story
// 8.11.
func auditCheckpointAt(at time.Time, gen string) auditCheckpointKey {
	return auditCheckpointKey{inv: inv(at), gen: gen}
}

// String renders the audit checkpoint as the key it is stored under.
func (a auditCheckpointKey) String() string {
	return indexAuditCkptPrefix + joinFields(a.inv, a.gen)
}

// parseAuditCheckpointKey reads an audit checkpoint back out of its key.
func parseAuditCheckpointKey(key string) (auditCheckpointKey, error) {
	leaf, ok := strings.CutPrefix(key, indexAuditCkptPrefix)
	if !ok {
		return auditCheckpointKey{}, indexKeyError(key)
	}

	fields := strings.Split(leaf, fieldSeparator)
	if len(fields) != 2 || !validInv(fields[0]) || !validIdentityDigest(fields[1]) {
		return auditCheckpointKey{}, indexKeyError(key)
	}

	return auditCheckpointKey{inv: fields[0], gen: fields[1]}, nil
}

// refOwnerKind is which kind of thing holds a pin on an artifact.
type refOwnerKind int

const (
	// ownerResult is a stored scan: it pins every artifact its document names,
	// including the document itself.
	ownerResult refOwnerKind = iota
	// ownerDecision is a baseline approval: it pins the evidence of the scan
	// it approved for as long as the approval stands.
	ownerDecision
	// ownerTake is not a reference at all: it records that wsaw stored these
	// bytes for a scan whose result is not in the index yet.
	//
	// It exists because content addressing makes an object's own age the wrong
	// question, and this store has no row to write a claim in (see claims.go
	// for the same argument against a database). A scan that captures an
	// unchanged asset stores nothing — the key is already there from the scan
	// that first saw those bytes, and its write time is that scan's, which may
	// be months old — while the scan running now will name it from the result
	// it stores minutes later. Retention that reasoned from the object's age
	// would collect exactly that object.
	//
	// A take carries the instant it was taken at rather than an identity,
	// because what retention asks of it is when, not who: it protects the
	// object while it is younger than the grace period and no pin has been
	// written since. A pin written after a take is the result the take was
	// waiting for, and from that moment the pin is what keeps the object —
	// which is how an append-only index expresses the release a SQL store
	// performs by deleting the claim row.
	ownerTake
)

// refOwner names whoever holds one pin on one artifact.
//
// The owner is in the key rather than in a body so that "does anything still
// reference this artifact" is a listing of one short directory and no GET at
// all, which is what makes the retention sweep affordable against a bucket
// that charges per request (AC12).
type refOwner struct {
	kind   refOwnerKind
	series seriesID
	scan   encodedScan
	did    string

	// at is the inverted instant a take was taken at, and empty on the other
	// two kinds. It is inverted like every other ordering field in this
	// grammar, so the newest take of one artifact is the first of them a
	// listing returns.
	at string
}

// resultRefOwner is the pin a stored result holds.
func resultRefOwner(s seriesID, scan encodedScan) refOwner {
	return refOwner{kind: ownerResult, series: s, scan: scan}
}

// decisionRefOwner is the pin a baseline decision holds.
func decisionRefOwner(did string) refOwner {
	return refOwner{kind: ownerDecision, did: did}
}

// takeRefOwner is the record that wsaw stored these bytes at at, for a scan
// whose result is not indexed yet.
func takeRefOwner(at time.Time) refOwner {
	return refOwner{kind: ownerTake, at: inv(at)}
}

// String renders the owner as the last segment of a ref marker's key.
func (o refOwner) String() string {
	switch o.kind {
	case ownerDecision:
		return joinFields(ownerDecisionTag, o.did)
	case ownerTake:
		return joinFields(ownerTakeTag, o.at)
	default:
		return joinFields(ownerResultTag, string(o.series.target), string(o.series.mode), string(o.scan))
	}
}

// parseRefOwner reads an owner back out of a ref marker's last segment.
func parseRefOwner(s string) (refOwner, bool) {
	fields := strings.Split(s, fieldSeparator)

	switch {
	case len(fields) == 2 && fields[0] == ownerDecisionTag:
		if !validIdentityDigest(fields[1]) {
			return refOwner{}, false
		}

		return decisionRefOwner(fields[1]), true

	case len(fields) == 2 && fields[0] == ownerTakeTag:
		if !validInv(fields[1]) {
			return refOwner{}, false
		}

		return refOwner{kind: ownerTake, at: fields[1]}, true

	case len(fields) == 4 && fields[0] == ownerResultTag:
		series, ok := seriesFromSegments(fields[1], fields[2])
		if !ok || !validEncodedScan(fields[3]) {
			return refOwner{}, false
		}

		return resultRefOwner(series, encodedScan(fields[3])), true

	default:
		return refOwner{}, false
	}
}

// refMarker is one owner's pin on one artifact: a zero-byte object whose whole
// content is its name.
type refMarker struct {
	// artifact is an artifact reference in the bucket's own "<kind>/<digest>"
	// spelling, so the marker's key contains the artifact's key verbatim and
	// the sweep's merge join compares like with like.
	artifact string
	owner    refOwner
}

// newRefMarker pins artifact for owner, refusing an artifact reference that is
// not one this store wrote.
//
// It validates rather than assumes because the references it is given come out
// of stored documents, which are built from page-controlled data (Tenet 9),
// and because splicing an unvalidated reference into a key is exactly how a
// crafted document would name an object outside the index tree.
func newRefMarker(artifact string, owner refOwner) (refMarker, error) {
	if err := validateRef(artifact); err != nil {
		return refMarker{}, fmt.Errorf("pinning an artifact from the index: %w", err)
	}

	return refMarker{artifact: artifact, owner: owner}, nil
}

// String renders the marker as the key it is stored under.
func (m refMarker) String() string {
	return indexRefPrefix + m.artifact + refSeparator + m.owner.String()
}

// parseRefMarkerKey reads the artifact and its owner back out of a ref key.
func parseRefMarkerKey(key string) (refMarker, error) {
	rest, ok := strings.CutPrefix(key, indexRefPrefix)
	if !ok {
		return refMarker{}, indexKeyError(key)
	}

	segments := strings.Split(rest, refSeparator)
	if len(segments) != 3 {
		return refMarker{}, indexKeyError(key)
	}

	artifact := segments[0] + refSeparator + segments[1]
	if err := validateRef(artifact); err != nil {
		return refMarker{}, indexKeyError(key)
	}

	owner, ok := parseRefOwner(segments[2])
	if !ok {
		return refMarker{}, indexKeyError(key)
	}

	return refMarker{artifact: artifact, owner: owner}, nil
}

// refMarkerDirPrefix is every pin on one artifact. A sweep asks "is this
// directory empty" and needs no other question answered.
func refMarkerDirPrefix(artifact string) (string, error) {
	if err := validateRef(artifact); err != nil {
		return "", fmt.Errorf("listing the pins on an artifact: %w", err)
	}

	return indexRefPrefix + artifact + refSeparator, nil
}

// maxLayoutVersion is one past the largest version layoutKey can spell in its
// zero-padded field, and therefore the domain parseLayoutKey inverts exactly.
// It is a bound on a number this repository chooses one at a time; a hundred
// million layout revisions is not a thing that happens.
const maxLayoutVersion = 100000000

// layoutKey names the object that records which layout wrote this index.
//
// The version is zero-padded so that layout 2 and layout 10 sort in numeric
// order, which is what lets a reader find the newest one by listing rather
// than by guessing at names. version must be in [0, maxLayoutVersion): outside
// it the padding stops being a fixed width and the key stops sorting, so the
// bound is the domain on which this and parseLayoutKey are inverses, and a
// test holds them to it. Every caller passes indexLayoutVersion.
func layoutKey(version int) string {
	return indexLayoutPrefix + fmt.Sprintf("%0*d", layoutDigits, version) + layoutSuffix
}

// parseLayoutKey reads the layout version back out of its key.
//
// The digits are checked byte-wise before they are converted, because
// strconv.Atoi is not the inverse of a zero-padded field: it accepts a sign,
// so "+0000001" would parse as layout 1 and give the one version object two
// spellings — an index that could disagree with itself about which layout
// wrote it. A digit-only field has exactly one spelling per version.
func parseLayoutKey(key string) (int, error) {
	leaf, ok := strings.CutPrefix(key, indexLayoutPrefix)
	if !ok {
		return 0, indexKeyError(key)
	}

	digits, ok := strings.CutSuffix(leaf, layoutSuffix)
	if !ok || len(digits) != layoutDigits || !isDecimal(digits) {
		return 0, indexKeyError(key)
	}

	version, err := strconv.Atoi(digits)
	if err != nil {
		return 0, indexKeyError(key)
	}

	return version, nil
}

// isDecimal reports whether s is entirely ASCII digits, with no sign, no
// spaces and no underscores — none of which strconv rejects on its own.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// rebuildKey names the marker that says a rebuild of the index is in progress
// and that nothing may be collected while it is (Story 8.11).
//
// The id goes through the scan-ID encoder rather than through an alphabet of
// its own, because a rebuild id is a caller-supplied token exactly as a scan
// ID is, and one escaping rule is one rule to get right.
func rebuildKey(id encodedScan) string {
	return indexRebuildPrefix + string(id)
}

// parseRebuildKey reads a rebuild marker's id back out of its key.
func parseRebuildKey(key string) (encodedScan, error) {
	leaf, ok := strings.CutPrefix(key, indexRebuildPrefix)
	if !ok || !validEncodedScan(leaf) {
		return "", indexKeyError(key)
	}

	return encodedScan(leaf), nil
}

// wellFormedIndexKey reports whether key is one of the productions above.
//
// It answers by parsing, so the door that admits a write and the parsers that
// read a listing back cannot drift into two ideas of what a key is: a key the
// door accepts is a key some parser will accept, today and after the grammar
// changes.
func wellFormedIndexKey(key string) bool {
	switch {
	case strings.HasPrefix(key, indexLayoutPrefix):
		_, err := parseLayoutKey(key)

		return err == nil

	case strings.HasPrefix(key, indexTargetsPrefix):
		_, err := parseTargetMarkerKey(key)

		return err == nil

	case strings.HasPrefix(key, indexSeriesPrefix):
		_, err := parseSeriesRecord(key)

		return err == nil

	case strings.HasPrefix(key, indexByIDPrefix):
		_, _, err := parseByIDKey(key)

		return err == nil

	case strings.HasPrefix(key, indexBaselinePrefix):
		_, err := parseDecisionKey(key)

		return err == nil

	case strings.HasPrefix(key, indexAuditCkptPrefix):
		_, err := parseAuditCheckpointKey(key)

		return err == nil

	case strings.HasPrefix(key, indexAuditPrefix):
		_, err := parseAuditKey(key)

		return err == nil

	case strings.HasPrefix(key, indexRefPrefix):
		_, err := parseRefMarkerKey(key)

		return err == nil

	case strings.HasPrefix(key, indexRebuildPrefix):
		_, err := parseRebuildKey(key)

		return err == nil

	default:
		return false
	}
}

// seriesFromSegments validates two path segments as a series identity.
func seriesFromSegments(target, mode string) (seriesID, bool) {
	if !validEncodedTarget(target) || !validEncodedMode(mode) {
		return seriesID{}, false
	}

	return seriesID{target: encodedTarget(target), mode: encodedMode(mode)}, true
}

// validEncodedTarget accepts what tk produces: 32 hex digits of identity, a
// dash, and a slug. It is parsed positionally, so a slug that happens to
// contain dashes changes nothing about where the identity ends.
func validEncodedTarget(s string) bool {
	if len(s) < identityHexLen+len(slugSeparator)+1 {
		return false
	}

	if !isHex(s[:identityHexLen]) || s[identityHexLen] != slugSeparator[0] {
		return false
	}

	return validSlug(s[identityHexLen+1:], targetSlugMax)
}

// validSlug accepts what slug produces: at most limit bytes of [a-z0-9-],
// no dash at either end and no run of two.
func validSlug(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}

	for i := range len(s) {
		c := s[i]

		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1 && s[i-1] != '-':
		default:
			return false
		}
	}

	return true
}

// validEncodedMode accepts what mk produces.
func validEncodedMode(s string) bool {
	return model.ConsentMode(s).Valid() || validEscaped(s, identityHexLen)
}

// validEncodedScan accepts what sk produces.
func validEncodedScan(s string) bool {
	return literalToken(s, scanIDMax, false) || validEscaped(s, identityHexLen)
}

// validEncodedTerm accepts what tm produces.
func validEncodedTerm(s string) bool {
	return literalToken(s, termMax, true) || validEscaped(s, termHexLen)
}

// validEscaped accepts the escape marker followed by hexLen hex digits, which
// is the form every encoder falls back to.
func validEscaped(s string, hexLen int) bool {
	return len(s) == len(escapeMarker)+hexLen && strings.HasPrefix(s, escapeMarker) && isHex(s[len(escapeMarker):])
}

// validIdentityDigest accepts a truncated body digest: <did> and <gen> are the
// same construction applied to different bodies, so they are the same check.
func validIdentityDigest(s string) bool {
	return len(s) == identityHexLen && isHex(s)
}

// validInv accepts what inv produces: exactly invWidth digits naming a value
// no larger than MaxInt64. The upper bound is checked rather than assumed,
// because nineteen digits can spell a number inv can never produce.
func validInv(s string) bool {
	if len(s) != invWidth || !isDecimal(s) {
		return false
	}

	_, err := strconv.ParseInt(s, 10, 64)

	return err == nil
}

// validStamp accepts what stamp produces, by rendering the parse back and
// requiring it to be identical. That refuses an impossible date — a
// thirteenth month, a thirty-second day — without a second definition of what
// the layout means.
func validStamp(s string) bool {
	t, err := time.Parse(stampLayout, s)

	return err == nil && t.Format(stampLayout) == s
}

// joinFields joins one key leaf's fields with the field separator.
func joinFields(fields ...string) string {
	return strings.Join(fields, fieldSeparator)
}

// indexKeyError reports a key that is not one this grammar defines, with the
// key truncated: a malformed key can carry a page's payload into a log line,
// and this is the same defence truncateForMessage gives an artifact reference.
func indexKeyError(key string) error {
	return fmt.Errorf("index key %q is not one this store writes: %w",
		truncateForMessage(key), errInvalidIndexKey)
}
