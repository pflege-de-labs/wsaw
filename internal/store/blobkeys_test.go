package store

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// White-box tests of the bucket index's key grammar (Story 8.10, §4). They
// touch no bucket and no clock, because the grammar touches neither: every
// invariant below is a property of a string, which is what makes it something
// a table and a fuzz target can settle in milliseconds rather than something a
// reviewer has to believe.
//
// The properties under test, and why each one is worth a test rather than a
// comment:
//
//   - Every form round-trips. A parser that disagrees with its encoder is the
//     defect that corrupts an index silently: the write lands, the listing
//     shows it, and the fold skips it.
//   - Nothing page-controlled chooses a key shape (Tenet 9). A target is a
//     URL and a scan ID and a consent mode are page-adjacent, so a separator,
//     an escape marker, a traversal, a NUL or a right-to-left override in any
//     of them must come out as a key in the alphabet the grammar promises.
//   - Lexicographic order is the order a fold needs, so that AC4's "the
//     listing is already sorted" is a fact rather than a hope.
//   - An index key is never an artifact key and an artifact key is never an
//     index key, in both directions and including the case built to look like
//     one.

const (
	// The worked example from §4.5, so that a change to any encoder shows up
	// as a diff against the design and not only against itself.
	exampleTarget = "https://www.pflege.de/pflegeberatung"
	exampleTK     = "c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung"
	exampleScan   = "scan-9f1c0b2d3e4f5a6b7c8d9e0f"
	exampleInv    = "7434507724731319018"
	exampleStamp  = "20260908T104512Z"
	exampleDID    = "ec77eeb0cd4a92113b8f2a6d40915cc7"
	exampleGen    = "3f21ab90cc7e1d55a0be47c1e2d3f905"

	exampleSeriesDir = "_wsaw/index/v1/series/" + exampleTK + "/reject/"
	exampleArtifact  = "screenshot-after-consent/" +
		"9d2c1e4f6a8b0c2d4e6f8a0b2c4d6e8f0a2b4c6d8e0f2a4b6c8d0e2f4a6b8c0d"
)

// exampleStart is the instant §4.4 works its ordering example through. The
// nanosecond is load-bearing: it is what proves the field is nanosecond and
// not millisecond.
var exampleStart = time.Date(2026, 9, 8, 10, 45, 12, 123456789, time.UTC)

// exampleSeries is the target and mode every key below belongs to.
func exampleSeries() seriesID { return seriesFor(exampleTarget, model.ConsentReject) }

// TestIndexKeyGrammarRoundTripsEveryForm pins each production of §4.5 to a
// literal and then reads it back.
//
// The literal matters as much as the round trip. A grammar that only agreed
// with itself would pass a round-trip test after any change to it, including
// one that silently re-keyed an existing bucket — which is a store that has
// lost its history without reporting anything.
func TestIndexKeyGrammarRoundTripsEveryForm(t *testing.T) {
	t.Parallel()

	s := exampleSeries()
	scan := sk(exampleScan)
	term := tm(model.TermIdle)

	marker, err := newRefMarker(exampleArtifact, resultRefOwner(s, scan))
	if err != nil {
		t.Fatalf("newRefMarker(%q): %v", exampleArtifact, err)
	}

	decisionMarker, err := newRefMarker(exampleArtifact, decisionRefOwner(exampleDID))
	if err != nil {
		t.Fatalf("newRefMarker(%q) for a decision: %v", exampleArtifact, err)
	}

	takeMarker, err := newRefMarker(exampleArtifact, takeRefOwner(exampleStart))
	if err != nil {
		t.Fatalf("newRefMarker(%q) for a take: %v", exampleArtifact, err)
	}

	cases := []struct {
		name string
		key  string
		want string
		// round parses the key and renders it again. Its result must be the
		// key it was given, byte for byte.
		round func(string) (string, error)
	}{
		{
			name: "the layout object",
			key:  layoutKey(indexLayoutVersion),
			want: "_wsaw/index/layout/00000001.json",
			round: func(key string) (string, error) {
				v, err := parseLayoutKey(key)

				return layoutKey(v), err
			},
		},
		{
			name: "a target marker",
			key:  s.markerKey(),
			want: "_wsaw/index/v1/targets/" + exampleTK + ".reject",
			round: func(key string) (string, error) {
				got, err := parseTargetMarkerKey(key)

				return got.markerKey(), err
			},
		},
		{
			name: "a series entry",
			key:  s.entry(exampleStart, scan, term).String(),
			want: exampleSeriesDir + "r." + exampleInv + "." + exampleStamp + "." + exampleScan + ".idle",
			round: func(key string) (string, error) {
				got, err := parseSeriesRecord(key)

				return got.String(), err
			},
		},
		{
			name: "a series checkpoint",
			key:  s.checkpoint(exampleStart, exampleGen).String(),
			want: exampleSeriesDir + "k." + exampleInv + "." + exampleGen,
			round: func(key string) (string, error) {
				got, err := parseSeriesRecord(key)

				return got.String(), err
			},
		},
		{
			name: "a series tombstone",
			key:  s.tombstone(exampleStart, scan).String(),
			want: exampleSeriesDir + "d." + exampleInv + "." + exampleScan,
			round: func(key string) (string, error) {
				got, err := parseSeriesRecord(key)

				return got.String(), err
			},
		},
		{
			name: "a scan addressed by its id",
			key:  s.byIDKey(scan),
			want: "_wsaw/index/v1/byid/" + exampleTK + "/reject/" + exampleScan,
			round: func(key string) (string, error) {
				gotSeries, gotScan, err := parseByIDKey(key)

				return gotSeries.byIDKey(gotScan), err
			},
		},
		{
			name: "a baseline decision",
			key:  s.decision(exampleStart, opApprove, exampleDID).String(),
			want: "_wsaw/index/v1/baseline/" + exampleTK + "/reject/" +
				exampleInv + "." + exampleStamp + ".approve." + exampleDID,
			round: func(key string) (string, error) {
				got, err := parseDecisionKey(key)

				return got.String(), err
			},
		},
		{
			name: "a withdrawn baseline",
			key:  s.decision(exampleStart, opRevoke, exampleDID).String(),
			want: "_wsaw/index/v1/baseline/" + exampleTK + "/reject/" +
				exampleInv + "." + exampleStamp + ".revoke." + exampleDID,
			round: func(key string) (string, error) {
				got, err := parseDecisionKey(key)

				return got.String(), err
			},
		},
		{
			name: "an audit entry",
			key:  auditEntryAt(exampleStart, exampleDID).String(),
			want: "_wsaw/index/v1/audit/" + exampleInv + "." + exampleStamp + "." + exampleDID,
			round: func(key string) (string, error) {
				got, err := parseAuditKey(key)

				return got.String(), err
			},
		},
		{
			name: "an audit checkpoint",
			key:  auditCheckpointAt(exampleStart, exampleGen).String(),
			want: "_wsaw/index/v1/auditckpt/" + exampleInv + "." + exampleGen,
			round: func(key string) (string, error) {
				got, err := parseAuditCheckpointKey(key)

				return got.String(), err
			},
		},
		{
			name: "a result's pin on an artifact",
			key:  marker.String(),
			want: "_wsaw/index/v1/ref/" + exampleArtifact + "/r." + exampleTK + ".reject." + exampleScan,
			round: func(key string) (string, error) {
				got, err := parseRefMarkerKey(key)

				return got.String(), err
			},
		},
		{
			name: "a baseline's pin on an artifact",
			key:  decisionMarker.String(),
			want: "_wsaw/index/v1/ref/" + exampleArtifact + "/d." + exampleDID,
			round: func(key string) (string, error) {
				got, err := parseRefMarkerKey(key)

				return got.String(), err
			},
		},
		{
			// Not a reference: the record that wsaw stored these bytes for a
			// scan whose result is not in the index yet. Its tag sorts after
			// both of the pins above, so a listing of one artifact's directory
			// answers "does anything reference this" before it reaches any of
			// these.
			name: "a take on an artifact",
			key:  takeMarker.String(),
			want: "_wsaw/index/v1/ref/" + exampleArtifact + "/t." + exampleInv,
			round: func(key string) (string, error) {
				got, err := parseRefMarkerKey(key)

				return got.String(), err
			},
		},
		{
			name: "a rebuild marker",
			key:  rebuildKey(scan),
			want: "_wsaw/index/v1/rebuild/" + exampleScan,
			round: func(key string) (string, error) {
				got, err := parseRebuildKey(key)

				return rebuildKey(got), err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.key != tc.want {
				t.Errorf("key = %q, want %q", tc.key, tc.want)
			}

			if err := validateIndexKey(tc.key); err != nil {
				t.Errorf("validateIndexKey(%q): %v", tc.key, err)
			}

			back, err := tc.round(tc.key)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.key, err)
			}

			if back != tc.key {
				t.Errorf("round trip of %q gave %q", tc.key, back)
			}
		})
	}
}

// TestTheVersionedPrefixAndTheLayoutVersionAgree stops the tree and the object
// that names it from drifting apart.
//
// A bucket whose layout object says 2 over a tree spelled v1 is an index that
// lies about itself, and the lie would only surface as a store refusing to
// open long after the write that caused it.
func TestTheVersionedPrefixAndTheLayoutVersionAgree(t *testing.T) {
	t.Parallel()

	want := fmt.Sprintf("/v%d/", indexLayoutVersion)
	if !strings.HasSuffix(indexPrefix, want) {
		t.Errorf("indexPrefix = %q, want it to end in %q for layout %d",
			indexPrefix, want, indexLayoutVersion)
	}
}

// TestLayoutKeyInvertsExactlyOnItsDomain covers the numeric field, which is
// the one place in the grammar where a standard-library parser is not the
// inverse of the printer that produced it.
func TestLayoutKeyInvertsExactlyOnItsDomain(t *testing.T) {
	t.Parallel()

	for _, version := range []int{0, 1, 2, 10, 99, maxLayoutVersion - 1} {
		key := layoutKey(version)

		if err := validateIndexKey(key); err != nil {
			t.Errorf("validateIndexKey(layoutKey(%d) = %q): %v", version, key, err)
		}

		got, err := parseLayoutKey(key)
		if err != nil {
			t.Errorf("parseLayoutKey(%q): %v", key, err)

			continue
		}

		if got != version {
			t.Errorf("parseLayoutKey(%q) = %d, want %d", key, got, version)
		}
	}

	// A signed field is what strconv.Atoi would have accepted and what would
	// have given layout 1 a second spelling.
	for _, leaf := range []string{"+0000001", "-0000001", "0000001", "000000001", "0000_001", " 0000001", "0000000a"} {
		key := indexLayoutPrefix + leaf + layoutSuffix
		if _, err := parseLayoutKey(key); !errors.Is(err, errInvalidIndexKey) {
			t.Errorf("parseLayoutKey(%q) = %v, want an invalid-key error", key, err)
		}
	}
}

// TestComponentEncodersRefuseToLetAValueChooseAKeyShape is Tenet 9 applied to
// the three caller-supplied values that reach a key.
//
// Each hostile value below is one a page or a page-adjacent field can carry:
// the field separator, the escape marker, a path separator, a traversal, a
// percent-encoded separator, a control byte, a confusable, and a value long
// enough to overrun a filesystem's path component. None of them may reach a
// key as itself, and none may make the key that carries it anything other than
// a key this grammar defines.
func TestComponentEncodersRefuseToLetAValueChooseAKeyShape(t *testing.T) {
	t.Parallel()

	hostile := map[string]string{
		"empty":                   "",
		"the field separator":     "a.b",
		"the escape marker":       "_wsaw",
		"a path separator":        "a/b",
		"a parent directory":      "../../etc/passwd",
		"an absolute path":        "/etc/passwd",
		"a windows separator":     `a\b`,
		"a percent-encoded slash": "a%2fb",
		"a newline":               "a\nb",
		"a carriage return":       "a\rb",
		"a nul byte":              "a\x00b",
		"a tab":                   "a\tb",
		"only punctuation":        "...///...",
		"a leading dash":          "-lead",
		"a trailing dash":         "trail-",
		"a run of dashes":         "a----b",
		// Written as escapes rather than as the characters themselves, the
		// same way bucket_test.go writes its hostile references: a
		// bidirectional override in source is a hazard of its own and an
		// invisible space is unreviewable. The last two are one string in NFD
		// and in NFC, which some filesystems normalise into one name.
		"an uppercase host":        "https://EXAMPLE.com/",
		"a cyrillic homoglyph":     "https://\u0430pple.com/",
		"a right to left override": "https://example.com/\u202eqrs",
		"a zero width space":       "https://exa\u200bmple.com/",
		"a full width solidus":     "a\uff0fb",
		"a combining sequence":     "https://cafe\u0301.example/",
		"a precomposed equivalent": "https://caf\u00e9.example/",
		"an emoji":                 "https://example.com/\U0001f600",
		"a very long host":         "https://" + strings.Repeat("a", 3000) + ".example/",
		"a very long path":         "https://example.com/" + strings.Repeat("b/", 1500),
		"a bucket key of our own":  "_wsaw/index/v1/series/x/reject/r.1.2.3.4",
		"an artifact key of our own": "screenshot/" +
			strings.Repeat("f", digestLength),
	}

	// Injectivity is checked over the whole set at once: two hostile values
	// that encoded to one component would file two targets' scans in one
	// history, which is the failure the 128-bit identity exists to prevent and
	// the one a per-case assertion cannot see.
	seenTargets := map[encodedTarget]string{}
	seenScans := map[encodedScan]string{}

	for name, value := range hostile {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assertKeyComponent(t, "tk", string(tk(value)))
			assertKeyComponent(t, "mk", string(mk(model.ConsentMode(value))))
			assertKeyComponent(t, "sk", string(sk(value)))
			assertKeyComponent(t, "tm", string(tm(model.TerminationReason(value))))

			// The component has to survive being placed in a key, not merely
			// to look well formed on its own.
			s := seriesFor(value, model.ConsentMode(value))
			key := s.entry(exampleStart, sk(value), tm(model.TerminationReason(value))).String()

			if err := validateIndexKey(key); err != nil {
				t.Errorf("validateIndexKey(%q): %v", key, err)
			}

			back, err := parseSeriesRecord(key)
			if err != nil {
				t.Fatalf("parseSeriesRecord(%q): %v", key, err)
			}

			if back.String() != key {
				t.Errorf("round trip of %q gave %q", key, back.String())
			}

			if back.series != s {
				t.Errorf("parsed series %+v, want %+v", back.series, s)
			}
		})

		if other, clash := seenTargets[tk(value)]; clash {
			t.Errorf("tk(%q) and tk(%q) are both %q", value, other, tk(value))
		}

		seenTargets[tk(value)] = value

		if other, clash := seenScans[sk(value)]; clash {
			t.Errorf("sk(%q) and sk(%q) are both %q", value, other, sk(value))
		}

		seenScans[sk(value)] = value
	}
}

// assertKeyComponent holds one encoded component to what §4.3 promises of all
// of them: never empty, inside the fixed alphabet, and short enough that the
// longest key it can appear in still fits a filesystem path component.
func assertKeyComponent(t *testing.T, encoder, got string) {
	t.Helper()

	if got == "" {
		t.Errorf("%s produced an empty component", encoder)
	}

	if len(got) > maxIndexComponentBytes {
		t.Errorf("%s produced a %d-byte component, over the %d limit: %q",
			encoder, len(got), maxIndexComponentBytes, got)
	}

	for i := range len(got) {
		c := got[i]

		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == slugSeparator[0]:
		case c == escapeMarker[0] && i == 0:
		default:
			t.Errorf("%s produced %q, which holds a byte outside [a-z0-9-] at %d", encoder, got, i)

			return
		}
	}
}

// TestTargetsThatDifferOnlyInCaseAreDifferentSeries is the case-folding
// filesystem argument for hashing the target, kept as a test because the
// filesystem that would break it is the one most of this repository's
// development happens on.
func TestTargetsThatDifferOnlyInCaseAreDifferentSeries(t *testing.T) {
	t.Parallel()

	upper, lower := tk("Site"), tk("site")

	if upper == lower {
		t.Fatalf("tk(%q) and tk(%q) are both %q", "Site", "site", upper)
	}

	if want := "fa7955814e32aed3a240ee46fcd053dd-site"; string(upper) != want {
		t.Errorf("tk(%q) = %q, want %q", "Site", upper, want)
	}

	if want := "fbae041b02c41ed0fd8a4efb039bc780-site"; string(lower) != want {
		t.Errorf("tk(%q) = %q, want %q", "site", lower, want)
	}

	// The readable halves are identical, which is the point: the decoration
	// folds and the identity does not, so a case-folding filesystem sees two
	// directory names that differ in their first 32 bytes.
	if strings.TrimPrefix(string(upper), "fa7955814e32aed3a240ee46fcd053dd") !=
		strings.TrimPrefix(string(lower), "fbae041b02c41ed0fd8a4efb039bc780") {
		t.Error("the two slugs differ; the test no longer covers the case-folding hazard")
	}
}

// TestModesAndTerminationsThisRepositoryShipsStayReadable checks the other
// half of the escaping rule: a value in the literal alphabet must not be
// hashed, because a bucket nobody can read by eye is a bucket nobody can
// debug.
func TestModesAndTerminationsThisRepositoryShipsStayReadable(t *testing.T) {
	t.Parallel()

	for _, mode := range []model.ConsentMode{model.ConsentNone, model.ConsentReject, model.ConsentAccept} {
		if got := mk(mode); string(got) != string(mode) {
			t.Errorf("mk(%q) = %q, want it unchanged", mode, got)
		}
	}

	terms := []model.TerminationReason{
		model.TermIdle, model.TermTimeout, model.TermRequestCap,
		model.TermByteCap, model.TermError, model.TermSkipped,
	}

	for _, term := range terms {
		if got := tm(term); string(got) != string(term) {
			t.Errorf("tm(%q) = %q, want it unchanged", term, got)
		}
	}

	// A scan ID this repository mints, and the entropy-failure fallback
	// newScanID falls back to, both stay literal.
	for _, id := range []string{exampleScan, "scan-1757328312123456789", "0"} {
		if got := sk(id); string(got) != id {
			t.Errorf("sk(%q) = %q, want it unchanged", id, got)
		}
	}

	// A mode from a document some future version wrote is hashed rather than
	// refused: an entry whose mode this build does not know is still an entry.
	if got := mk(model.ConsentMode("reject/ALL?")); !strings.HasPrefix(string(got), escapeMarker) {
		t.Errorf("mk(%q) = %q, want it escaped", "reject/ALL?", got)
	}
}

// TestInvertedTimestampsOrderNewestFirst walks §4.4's worked example, which is
// the whole of AC4: blob.ListOptions has neither a reverse listing nor a
// start-after cursor, so the ordering has to be in the name.
func TestInvertedTimestampsOrderNewestFirst(t *testing.T) {
	t.Parallel()

	newer := exampleStart.Add(time.Microsecond)
	older := exampleStart.Add(-24 * time.Hour)

	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"the worked example", exampleStart, exampleInv},
		{"one microsecond newer", newer, "7434507724731318018"},
		{"one day older", older, "7434594124731319018"},
	}

	for _, tc := range cases {
		if got := inv(tc.at); got != tc.want {
			t.Errorf("inv(%s) = %q, want %q", tc.at.Format(time.RFC3339Nano), got, tc.want)
		}

		if got := len(inv(tc.at)); got != invWidth {
			t.Errorf("inv(%s) is %d digits, want %d", tc.at.Format(time.RFC3339Nano), got, invWidth)
		}
	}

	if inv(newer) >= inv(exampleStart) || inv(exampleStart) >= inv(older) {
		t.Errorf("inv is not strictly newest-first: %q %q %q", inv(newer), inv(exampleStart), inv(older))
	}

	// One nanosecond has to be enough to separate two scans, because the
	// resolution is what makes the strictness true rather than lucky.
	if inv(exampleStart.Add(time.Nanosecond)) == inv(exampleStart) {
		t.Error("two instants one nanosecond apart share an inv")
	}

	// The stamp is decoration derived from the same instant, so it can never
	// reorder anything the inv has already ordered.
	if got := stamp(exampleStart); got != exampleStamp {
		t.Errorf("stamp = %q, want %q", got, exampleStamp)
	}
}

// TestInstantsOutsideTheOrderableRangeAreClampedAndNotDropped covers the
// bound: a wrong clock is somebody else's mistake and losing the scan to it
// would be ours (Tenet 5).
func TestInstantsOutsideTheOrderableRangeAreClampedAndNotDropped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		at      time.Time
		want    int64
		clamped bool
	}{
		{"the epoch itself", indexEpoch, 0, false},
		{"the horizon itself", indexHorizon, math.MaxInt64, false},
		{"before the epoch", time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC), 0, true},
		{"the zero time", time.Time{}, 0, true},
		{"after the horizon", time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), math.MaxInt64, true},
		{"inside the range", exampleStart, exampleStart.UnixNano(), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nanos, clamped := clampNano(tc.at)
			if nanos != tc.want || clamped != tc.clamped {
				t.Errorf("clampNano = (%d, %v), want (%d, %v)", nanos, clamped, tc.want, tc.clamped)
			}

			// Clamped or not, the instant still produces a key the grammar
			// admits — which is what "never dropped" means here.
			key := exampleSeries().entry(tc.at, sk(exampleScan), tm(model.TermIdle)).String()
			if err := validateIndexKey(key); err != nil {
				t.Errorf("validateIndexKey(%q): %v", key, err)
			}
		})
	}

	// A far-future instant must pin at the end of the ordering rather than
	// wrap into the middle of it, which is what a naive min/max over
	// time.Time.UnixNano would have done.
	if got := inv(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)); got != strings.Repeat("0", invWidth) {
		t.Errorf("a year-3000 instant has inv %q, want it pinned at zero", got)
	}
}

// TestIndexListingOrderIsTheOrderAFoldNeeds is §4.2's invariant, stated as the
// property the fold actually relies on.
//
// One listing of a series directory has to return every tombstone, then every
// checkpoint, then every entry, each group newest first, so that a fold can
// answer from one visible key set instead of from three listings taken at
// three moments (AC7). Sorting the keys here is exactly what a provider does,
// so this is that listing without a bucket.
func TestIndexListingOrderIsTheOrderAFoldNeeds(t *testing.T) {
	t.Parallel()

	s := exampleSeries()

	// Deliberately built oldest-first and interleaved, so that a sort which
	// did nothing would be visible.
	instants := []time.Time{
		exampleStart.Add(-72 * time.Hour),
		exampleStart.Add(-time.Hour),
		exampleStart,
		exampleStart.Add(time.Microsecond),
		exampleStart.Add(time.Nanosecond),
	}

	var keys []string

	for i, at := range instants {
		keys = append(
			keys,
			s.entry(at, sk(fmt.Sprintf("scan-%02d", i)), tm(model.TermIdle)).String(),
			s.tombstone(at, sk(fmt.Sprintf("scan-%02d", i))).String(),
			s.checkpoint(at, shortSum([]byte(at.String()), identityHexLen)).String(),
		)
	}

	slices.Sort(keys)

	// The three groups, in the order the tags sort.
	var groups []string

	for _, key := range keys {
		leaf := strings.TrimPrefix(key, s.dirPrefix())

		tag, _, found := strings.Cut(leaf, fieldSeparator)
		if !found {
			t.Fatalf("key %q has no tag", key)
		}

		if len(groups) == 0 || groups[len(groups)-1] != tag {
			groups = append(groups, tag)
		}
	}

	wantGroups := []string{seriesTombstoneTag, seriesCheckpointTag, seriesEntryTag}
	if !slices.Equal(groups, wantGroups) {
		t.Errorf("a sorted listing gave the groups %v, want %v — a fold would have to list three times",
			groups, wantGroups)
	}

	// Within each group the order is newest first, which is what lets a fold
	// that wants the newest n read one page and stop.
	assertNewestFirst(t, keys, s.dirPrefix()+seriesEntryTag+fieldSeparator, instants)
	assertNewestFirst(t, keys, s.dirPrefix()+seriesTombstoneTag+fieldSeparator, instants)
	assertNewestFirst(t, keys, s.dirPrefix()+seriesCheckpointTag+fieldSeparator, instants)
}

// assertNewestFirst checks that the keys under one tag arrive in descending
// order of the instant they record.
func assertNewestFirst(t *testing.T, keys []string, prefix string, instants []time.Time) {
	t.Helper()

	want := slices.Clone(instants)
	slices.SortFunc(want, func(a, b time.Time) int { return b.Compare(a) })

	var got []string

	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			record, err := parseSeriesRecord(key)
			if err != nil {
				t.Fatalf("parseSeriesRecord(%q): %v", key, err)
			}

			got = append(got, record.inv)
		}
	}

	wantInv := make([]string, 0, len(want))
	for _, at := range want {
		wantInv = append(wantInv, inv(at))
	}

	if !slices.Equal(got, wantInv) {
		t.Errorf("keys under %q listed as %v, want %v", prefix, got, wantInv)
	}
}

// TestEntriesSharingAnInstantFormOneContiguousRun is the other half of the
// ordering contract: §4.4 resolves ties in the fold and not in the key, and
// that is only sound if a tie is a run the fold can recognise.
func TestEntriesSharingAnInstantFormOneContiguousRun(t *testing.T) {
	t.Parallel()

	s := exampleSeries()
	tied := []string{"scan-c", "scan-a", "scan-b"}

	keys := []string{
		s.entry(exampleStart.Add(time.Second), sk("scan-newer"), tm(model.TermIdle)).String(),
		s.entry(exampleStart.Add(-time.Second), sk("scan-older"), tm(model.TermIdle)).String(),
	}

	for _, id := range tied {
		// Different terminations, to prove that <sk> before <tm> is what
		// keeps the run ordered by scan ID and not by why capture stopped.
		keys = append(keys, s.entry(exampleStart, sk(id), tm(model.TermError)).String())
		keys = append(keys, s.entry(exampleStart, sk(id+"x"), tm(model.TermIdle)).String())
	}

	slices.Sort(keys)

	var run []string

	for _, key := range keys {
		record, err := parseSeriesRecord(key)
		if err != nil {
			t.Fatalf("parseSeriesRecord(%q): %v", key, err)
		}

		if record.inv == inv(exampleStart) {
			run = append(run, string(record.scan))
		} else if len(run) > 0 && len(run) < 6 {
			t.Fatalf("the tied run is not contiguous: it broke after %v", run)
		}
	}

	// Ascending by scan ID within the run, because the key sorts ascending;
	// the fold reverses it to match SQL's "scan_id desc". The point of the
	// assertion is that the run is ordered by <sk> alone and not by <tm>.
	want := []string{"scan-a", "scan-ax", "scan-b", "scan-bx", "scan-c", "scan-cx"}
	if !slices.Equal(run, want) {
		t.Errorf("the tied run is %v, want %v", run, want)
	}
}

// TestIndexKeysAndArtifactKeysCannotCollide is the disjointness claim of §4.1,
// asserted rather than argued.
//
// It matters in both directions and for the retention sweeps on both sides: an
// artifact door that accepted an index key could overwrite the index, and a
// sweep that mistook an index key for an artifact would delete the history it
// was protecting (Story 8.5, AC4).
func TestIndexKeysAndArtifactKeysCannotCollide(t *testing.T) {
	t.Parallel()

	s := exampleSeries()
	scan := sk(exampleScan)

	marker, err := newRefMarker(exampleArtifact, resultRefOwner(s, scan))
	if err != nil {
		t.Fatalf("newRefMarker: %v", err)
	}

	takeMarker, err := newRefMarker(exampleArtifact, takeRefOwner(exampleStart))
	if err != nil {
		t.Fatalf("newRefMarker for a take: %v", err)
	}

	indexKeys := []string{
		layoutKey(indexLayoutVersion),
		s.markerKey(),
		s.entry(exampleStart, scan, tm(model.TermIdle)).String(),
		s.checkpoint(exampleStart, exampleGen).String(),
		s.tombstone(exampleStart, scan).String(),
		s.byIDKey(scan),
		s.decision(exampleStart, opApprove, exampleDID).String(),
		auditEntryAt(exampleStart, exampleDID).String(),
		auditCheckpointAt(exampleStart, exampleGen).String(),
		// The adversarial one: it ends in a real kind and a real 64-hex
		// digest, so a shape test applied to a suffix rather than to the whole
		// key would accept it as an artifact.
		marker.String(),
		takeMarker.String(),
		rebuildKey(scan),
	}

	for _, key := range indexKeys {
		if isArtifactRef(key) {
			t.Errorf("the artifact door accepts the index key %q", key)
		}

		if err := validateRef(key); !errors.Is(err, errInvalidRef) {
			t.Errorf("validateRef(%q) = %v, want an invalid-reference error", key, err)
		}

		if err := validatePrefix(key); !errors.Is(err, errInvalidRef) {
			t.Errorf("validatePrefix(%q) = %v, want an invalid-reference error", key, err)
		}
	}

	// And the other direction, over every kind this store writes.
	digest := strings.Repeat("a", digestLength)
	for _, kind := range []string{
		artifactKindBody, artifactKindResult, artifactKindProbe,
		artifactKindScreenshot, testKind, "screenshot-after-consent",
	} {
		ref := kind + refSeparator + digest
		if err := validateIndexKey(ref); !errors.Is(err, errInvalidIndexKey) {
			t.Errorf("validateIndexKey(%q) = %v, want an invalid-key error", ref, err)
		}
	}

	// The proof is about the first byte, so a kind that begins with the escape
	// marker must be impossible rather than merely absent.
	if validKind("_wsaw") {
		t.Error(`validKind accepts "_wsaw"; the disjointness argument in indexRoot no longer holds`)
	}
}

// TestValidateIndexKeyRefusesWhatThisStoreDoesNotWrite covers the door itself
// with keys that are inside the root and still wrong, which is the class a
// prefix check alone would let through.
func TestValidateIndexKeyRefusesWhatThisStoreDoesNotWrite(t *testing.T) {
	t.Parallel()

	s := exampleSeries()
	entry := s.entry(exampleStart, sk(exampleScan), tm(model.TermIdle)).String()

	hostile := map[string]string{
		"empty":                       "",
		"the root alone":              indexRoot,
		"a prefix with no leaf":       s.dirPrefix(),
		"an unknown top-level prefix": indexPrefix + "sessions/x",
		"an unversioned tree":         indexRoot + "index/v2/targets/" + exampleTK + ".reject",
		"a traversal inside the root": indexPrefix + "series/../../../etc/passwd",
		"a nul byte":                  entry + "\x00",
		"a newline":                   entry + "\n",
		"an uppercase field":          strings.ToUpper(entry),
		"a short inv":                 strings.Replace(entry, exampleInv, exampleInv[1:], 1),
		"a signed inv":                strings.Replace(entry, exampleInv, "+"+exampleInv[1:], 1),
		"an impossible month":         strings.Replace(entry, exampleStamp, "20261308T104512Z", 1),
		"an impossible day":           strings.Replace(entry, exampleStamp, "20260932T104512Z", 1),
		"a missing field":             strings.Replace(entry, "."+exampleStamp, "", 1),
		"an extra field":              entry + ".extra",
		"an unknown record tag":       strings.Replace(entry, "/r."+exampleInv, "/z."+exampleInv, 1),
		"an unknown baseline op": strings.Replace(
			s.decision(exampleStart, opApprove, exampleDID).String(), ".approve.", ".ignore.", 1,
		),
		"a short decision digest": strings.Replace(
			auditEntryAt(exampleStart, exampleDID).String(), exampleDID, exampleDID[1:], 1,
		),
		"a ref marker on a foreign artifact": indexRefPrefix + "vendor-export/" +
			strings.Repeat("a", digestLength) + "/d." + exampleDID,
		"a ref marker with no owner": indexRefPrefix + exampleArtifact + refSeparator,
		"an over-long component": indexTargetsPrefix +
			strings.Repeat("a", maxIndexComponentBytes+1) + ".reject",
		"an over-long key": indexByIDPrefix + exampleTK + "/reject/" +
			strings.Repeat("a", maxIndexKeyBytes),
	}

	for name, key := range hostile {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := validateIndexKey(key); !errors.Is(err, errInvalidIndexKey) {
				t.Errorf("validateIndexKey(%q) = %v, want an invalid-key error", key, err)
			}

			if wellFormedIndexKey(key) {
				t.Errorf("wellFormedIndexKey(%q) = true", key)
			}
		})
	}
}

// TestAMalformedKeyCannotCarryAPayloadIntoALogLine is the same defence
// truncateForMessage gives an artifact reference: a key is untrusted input as
// soon as it comes back from a listing.
func TestAMalformedKeyCannotCarryAPayloadIntoALogLine(t *testing.T) {
	t.Parallel()

	huge := indexRoot + strings.Repeat("payload", 10000)

	err := validateIndexKey(huge)
	if !errors.Is(err, errInvalidIndexKey) {
		t.Fatalf("validateIndexKey of a huge key = %v, want an invalid-key error", err)
	}

	if len(err.Error()) > 200 {
		t.Errorf("the error message is %d bytes; a key is not a thing to put in a log whole", len(err.Error()))
	}
}

// TestASeriesRecordOfNoKnownKindIsNotAKey covers the one branch of String that
// no constructor can reach, because the honest answer to a programmer error is
// a value the door refuses rather than a plausible-looking key it accepts.
func TestASeriesRecordOfNoKnownKindIsNotAKey(t *testing.T) {
	t.Parallel()

	broken := seriesRecord{kind: seriesRecordKind(99), series: exampleSeries()}

	if err := validateIndexKey(broken.String()); !errors.Is(err, errInvalidIndexKey) {
		t.Errorf("validateIndexKey(%q) = %v, want an invalid-key error", broken.String(), err)
	}
}

// TestSlugKeepsTheDecorationInsideItsAlphabet exercises the reduction directly,
// because it is the one encoder whose output is not a fixed width and whose
// truncation rule has an edge — a cut that lands on a dash — that no key-level
// test would distinguish from a cut that does not.
func TestSlugKeepsTheDecorationInsideItsAlphabet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"https://www.pflege.de/pflegeberatung", 48, "https-www-pflege-de-pflegeberatung"},
		{"Site", 48, "site"},
		{"", 48, slugFallback},
		{"...", 48, slugFallback},
		{"\x00\n\t", 48, slugFallback},
		{"日本語", 48, slugFallback},
		{"-lead-", 48, "lead"},
		{"a--b", 48, "a-b"},
		// The truncation edge: the cut lands immediately after a dash, which
		// the second trim has to remove.
		{"abcd efgh", 5, "abcd"},
		{"abcd efgh", 6, "abcd-e"},
		{"abcdefghij", 4, "abcd"},
	}

	for _, tc := range cases {
		got := slug(tc.in, tc.limit)
		if got != tc.want {
			t.Errorf("slug(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}

		if !validSlug(got, tc.limit) {
			t.Errorf("slug(%q, %d) = %q, which validSlug rejects", tc.in, tc.limit, got)
		}
	}
}

// TestLiteralTokenStatesTheAlphabetTheEncodersPromise pins the shape test the
// scan-ID and termination encoders branch on, including the one difference
// between them: a termination may not begin with a digit.
func TestLiteralTokenStatesTheAlphabetTheEncodersPromise(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in          string
		limit       int
		letterFirst bool
		want        bool
	}{
		{"scan-1", scanIDMax, false, true},
		{"0", scanIDMax, false, true},
		{"0abc", scanIDMax, false, true},
		{"0abc", termMax, true, false},
		{"request-cap", termMax, true, true},
		{"", scanIDMax, false, false},
		{"-lead", scanIDMax, false, false},
		{"trail-", scanIDMax, false, true},
		{"Upper", scanIDMax, false, false},
		{"has.dot", scanIDMax, false, false},
		{"has/slash", scanIDMax, false, false},
		{"_escaped", scanIDMax, false, false},
		{strings.Repeat("a", scanIDMax), scanIDMax, false, true},
		{strings.Repeat("a", scanIDMax+1), scanIDMax, false, false},
	}

	for _, tc := range cases {
		if got := literalToken(tc.in, tc.limit, tc.letterFirst); got != tc.want {
			t.Errorf("literalToken(%q, %d, %v) = %v, want %v", tc.in, tc.limit, tc.letterFirst, got, tc.want)
		}
	}
}

// TestTheLongestKeyTheGrammarCanProduceIsFilesystemSafe is the number §4.5
// says decides whether this layout can live on a local disk.
//
// The worst case is built rather than guessed: the longest target, the longest
// escaped mode, the longest scan ID and the longest kind, all at once.
func TestTheLongestKeyTheGrammarCanProduceIsFilesystemSafe(t *testing.T) {
	t.Parallel()

	// A target long enough that its slug is truncated, a mode that has to be
	// escaped, and a scan ID at the literal limit.
	s := seriesFor("https://"+strings.Repeat("a", 4000)+".example/", model.ConsentMode("mode/with?punctuation"))
	scan := sk(strings.Repeat("z", scanIDMax))
	longestKind := artifactKindScreenshot + kindSuffixSeparator + strings.Repeat("q", maxKindLength-len(artifactKindScreenshot)-1)

	marker, err := newRefMarker(longestKind+refSeparator+strings.Repeat("f", digestLength),
		resultRefOwner(s, scan))
	if err != nil {
		t.Fatalf("newRefMarker: %v", err)
	}

	keys := []string{
		marker.String(),
		s.entry(exampleStart, scan, tm(model.TerminationReason(strings.Repeat("t", termMax)))).String(),
		s.byIDKey(scan),
		s.decision(exampleStart, opApprove, exampleDID).String(),
		s.markerKey(),
	}

	// 255 bytes is the path-component limit ext4, APFS and NTFS all impose,
	// and 1,024 is S3's cap on a whole key. Both are the provider's numbers,
	// not this store's: maxIndexComponentBytes and maxIndexKeyBytes are the
	// self-imposed guards that keep the grammar well inside them.
	const (
		filesystemComponentLimit = 255
		s3KeyLimit               = 1024
	)

	for _, key := range keys {
		if err := validateIndexKey(key); err != nil {
			t.Errorf("validateIndexKey(%q): %v", key, err)
		}

		if len(key) > s3KeyLimit {
			t.Errorf("key of %d bytes exceeds S3's limit: %q", len(key), key)
		}

		for _, segment := range strings.Split(key, refSeparator) {
			if len(segment) > filesystemComponentLimit {
				t.Errorf("path component of %d bytes exceeds the filesystem limit: %q", len(segment), segment)
			}
		}
	}

	// The ref marker's owner is the longest component the grammar produces,
	// and §4.5 puts it at about 182 bytes. Assert the shape of that claim
	// rather than the exact number, so a component that grew past the guard
	// fails here instead of on somebody's disk.
	owner := marker.owner.String()
	if len(owner) > maxIndexComponentBytes {
		t.Errorf("the ref owner is %d bytes, over the %d guard", len(owner), maxIndexComponentBytes)
	}

	if len(owner) < 150 {
		t.Errorf("the ref owner is only %d bytes; this test is no longer building the worst case", len(owner))
	}
}

// FuzzIndexKeyGrammar is the guard on the property no table can cover: that
// the encoder and the decoder agree about every value, including the ones
// nobody thought of.
//
// A decoder that disagrees with its encoder does not fail loudly in a store
// like this. The write succeeds, the listing shows the key, and the fold that
// parses it drops the entry — a scan that is in the bucket, is in the listing,
// and is not in the history.
func FuzzIndexKeyGrammar(f *testing.F) {
	f.Add(exampleTarget, "reject", exampleScan, "idle", exampleStart.UnixNano(), "seed")
	f.Add("", "", "", "", int64(0), "")
	f.Add("Site", "accept", "0", "request-cap", int64(-1), "\x00")
	f.Add("a.b/c", "_escaped", "scan-", "t", int64(math.MaxInt64), strings.Repeat("x", 300))
	f.Add("../../etc/passwd", "none", "SCAN-1", "\n", int64(1), "%2f")

	f.Fuzz(func(t *testing.T, target, mode, scanID, term string, nanos int64, seed string) {
		at := time.Unix(0, nanos)
		s := seriesFor(target, model.ConsentMode(mode))
		scan := sk(scanID)
		digest := shortSum([]byte(seed), digestLength)
		short := shortSum([]byte(seed), identityHexLen)

		marker, err := newRefMarker(artifactKindResult+refSeparator+digest, resultRefOwner(s, scan))
		if err != nil {
			t.Fatalf("newRefMarker: %v", err)
		}

		decisionMarker, err := newRefMarker(artifactKindBody+refSeparator+digest, decisionRefOwner(short))
		if err != nil {
			t.Fatalf("newRefMarker for a decision: %v", err)
		}

		round := map[string]func(string) (string, error){
			s.markerKey(): func(key string) (string, error) {
				got, err := parseTargetMarkerKey(key)

				return got.markerKey(), err
			},
			s.entry(at, scan, tm(model.TerminationReason(term))).String(): reparseSeriesRecord,
			s.tombstone(at, scan).String():                                reparseSeriesRecord,
			s.checkpoint(at, short).String():                              reparseSeriesRecord,
			s.byIDKey(scan): func(key string) (string, error) {
				gotSeries, gotScan, err := parseByIDKey(key)

				return gotSeries.byIDKey(gotScan), err
			},
			s.decision(at, opApprove, short).String(): func(key string) (string, error) {
				got, err := parseDecisionKey(key)

				return got.String(), err
			},
			auditEntryAt(at, short).String(): func(key string) (string, error) {
				got, err := parseAuditKey(key)

				return got.String(), err
			},
			auditCheckpointAt(at, short).String(): func(key string) (string, error) {
				got, err := parseAuditCheckpointKey(key)

				return got.String(), err
			},
			marker.String():         reparseRefMarker,
			decisionMarker.String(): reparseRefMarker,
			rebuildKey(scan): func(key string) (string, error) {
				got, err := parseRebuildKey(key)

				return rebuildKey(got), err
			},
		}

		for key, reparse := range round {
			if err := validateIndexKey(key); err != nil {
				t.Fatalf("validateIndexKey(%q): %v", key, err)
			}

			assertKeyIsWellShaped(t, key)

			back, err := reparse(key)
			if err != nil {
				t.Fatalf("parsing %q: %v", key, err)
			}

			if back != key {
				t.Fatalf("round trip of %q gave %q", key, back)
			}
		}

		// The identity halves are injective on the values that reach them, so
		// a value and that value plus a byte are never one series and never
		// one scan.
		if tk(target) == tk(target+"!") {
			t.Fatalf("tk collides on %q", target)
		}

		if sk(scanID) == sk(scanID+"!") {
			t.Fatalf("sk collides on %q", scanID)
		}
	})
}

// reparseSeriesRecord and reparseRefMarker are named so the fuzz table above
// reads as a list of forms rather than as a list of closures.
func reparseSeriesRecord(key string) (string, error) {
	got, err := parseSeriesRecord(key)

	return got.String(), err
}

func reparseRefMarker(key string) (string, error) {
	got, err := parseRefMarkerKey(key)

	return got.String(), err
}

// assertKeyIsWellShaped holds a key to the four bounds §4.5 promises of every
// one of them, independently of which production made it.
func assertKeyIsWellShaped(t *testing.T, key string) {
	t.Helper()

	if !strings.HasPrefix(key, indexRoot) {
		t.Fatalf("key %q is outside %q", key, indexRoot)
	}

	if len(key) > maxIndexKeyBytes {
		t.Fatalf("key of %d bytes: %q", len(key), key)
	}

	for i := range len(key) {
		if !indexKeyByte(key[i]) {
			t.Fatalf("key %q holds a byte the grammar never writes at %d", key, i)
		}
	}

	for _, segment := range strings.Split(key, refSeparator) {
		if segment == "" {
			t.Fatalf("key %q has an empty path component", key)
		}

		if len(segment) > maxIndexComponentBytes {
			t.Fatalf("key %q has a %d-byte path component", key, len(segment))
		}
	}
}
