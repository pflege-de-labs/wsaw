package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// This file is the store that has no database: the type, how it is opened, and
// the operations that need no index at all (Story 8.10).
//
// Everything here is the half of the store that the bucket already answers.
// Reading, writing, statting and signing an artifact are the same operations
// the SQL stores perform, against the same bucket and the same content
// addresses, and they are delegated rather than reimplemented. What is left —
// results, baselines, the audit log and retention — is the index, and the index
// is what the rest of the blob files build out of objects.
//
// The one thing opening this store does that opening a bucket does not is
// establish which layout wrote the index, which is this store's answer to the
// schema version a SQL store refuses to be newer than (AC14).
//
// **There is no recent-write overlay and no body cache here**, though the
// design (§6.6) placed both in this file. Neither was built, and the omission
// is a decision rather than an oversight: every read of this store is a fold
// over what the bucket currently shows, so the answer is a function of one
// visible key set and of nothing process-local. That makes AC7's determinism
// global rather than per process — two handles on one bucket agree, and a
// handle that wrote a key a moment ago sees exactly what any other reader sees
// — and it removes the hazard §6.6 spends most of its own argument on, a
// remembered write resurrecting a key a prune has since deleted. What it costs
// is read-your-writes: against a provider whose listings lag, a PutResult
// followed by a ListResults in one process can omit the scan just written, and
// the store reports it as "not there yet" rather than as deleted (§6.7). The
// deviation is recorded in the design's §7.6, and a cache can be added later
// without a layout change because no key is ever rewritten.

// Blob is the store whose index is objects in the same bucket as the evidence.
//
// It exists so that a wsaw deployment can be one binary and one bucket, with no
// database to run, back up or fail over. What that costs is real and is stated
// rather than hidden: the index answers the handful of queries wsaw makes, by
// key layout and by nothing else, and every read is requests against object
// storage rather than a query against a local file.
//
// It is deliberately not a fourth SQL dialect. A bucket has no transaction, no
// cross-key atomicity and no portable compare-and-swap, so an index that
// rewrote an object would lose a concurrent write and would read a version a
// later listing contradicts. The design is append-only instead: every write
// goes to a key derived from the fact it records, nothing rewrites a key, and
// every read is a fold over what is currently visible.
//
// It holds no context and no cancellation of its own. The store interface's
// read and write methods take none — that is recorded as its own story in the
// design — so each of them bounds itself with opCtx, exactly as the SQL stores
// do, and the methods that already take a caller's context pass it down.
type Blob struct {
	// bucket holds both halves of this store: the evidence under its
	// content-addressed keys, and the index under the reserved "_wsaw/" root
	// that no artifact key can collide with (see indexRoot).
	bucket *bucket

	// log reports what an operator has to be able to watch. For this store
	// that is compaction and retention, which move keys around underneath a
	// history nobody asked to have moved.
	log *slog.Logger

	// now is the clock every key's ordering and every grace period is derived
	// from. It is a field rather than a call to time.Now so that a test can
	// place a scan a week ago without waiting a week (AGENTS §5).
	now func() time.Time

	// checkpointGrace is how long a compaction checkpoint must have been
	// visible before the loose entries it covers may be deleted. It is
	// carried on the store because the rule it feeds is a property of this
	// index rather than of one call.
	//
	// It is invariant I3's grace period and its one reader is collectCovered
	// in blobcompact.go, which is also where the rest of I3 is argued. The
	// design's §7.6 records that an earlier build declared this field and read
	// it nowhere, because compaction had not landed yet; it has, and it does.
	checkpointGrace time.Duration

	// writes is compaction's schedule: how many results this process has
	// stored per series, checked at the end of each PutResult. There is no
	// ticker and no goroutine behind it (AGENTS §4); see seriesWrites.
	writes *seriesWrites

	// version identifies the build that opened this store, for the layout
	// object it may have to write. Provenance only: nothing reads it back.
	version string
}

// OpenBlob opens the store whose index is objects in the artifact bucket.
//
// It takes a context because reaching the bucket is I/O and because the layout
// probe below is more of it: an operator who has pointed wsaw at the wrong
// endpoint should be able to interrupt the start rather than wait out the
// deadlines.
//
// It does not probe that the bucket is writable. That check is
// ProbeArtifactBucket, which startup calls immediately after opening any store
// (Story 8.6, AC4), and doing it here as well would cost every process start a
// second write and a second delete to learn what it is about to be told. What
// this does do is write the layout object on a bucket that has none, so a
// bucket that refuses writes still fails at the first start rather than at the
// first scan.
func OpenBlob(ctx context.Context, opts Options) (*Blob, error) {
	if opts.ArtifactDir == "" {
		// No default is possible and none is guessed. For a SQLite store the
		// evidence can sit beside the database file; here there is no file to
		// sit beside, and the bucket is not somewhere the evidence goes — it
		// is the store (Tenet 15).
		return nil, fmt.Errorf(
			"store: the %s driver keeps its index in the artifact bucket, so a location is required and "+
				"there is no default: set store.artifactURL for a bucket, or store.artifactDir for a "+
				"directory on local disk", DriverBlob,
		)
	}

	b, err := openBucket(ctx, opts.ArtifactDir)
	if err != nil {
		return nil, err
	}

	s := &Blob{
		bucket:          b,
		log:             opts.logger(),
		now:             opts.clock(),
		checkpointGrace: opts.checkpointGrace(),
		writes:          newSeriesWrites(),
		version:         opts.version(),
	}

	// The same retry policy a SQL store lends its bucket, so Options.MaxAttempts
	// and Options.RetryBackoff mean the same thing for a deployment that has no
	// database as for one that has (Story 8.1, AC8).
	//
	// What counts as worth another attempt is not borrowed from a dialect,
	// though: there is no dialect here, and a bucket failure has already been
	// classified by gcerrors inside bucket.do, which marks the retryable ones.
	// Honouring that mark is one answer to the question rather than a second
	// one that reads provider messages.
	b.setRetry(retrier{
		attempts:  opts.maxAttempts(),
		backoff:   opts.retryBackoff(),
		onRetry:   opts.OnRetry,
		transient: isMarkedTransient,
	}.run)

	probeCtx, cancel := opCtxFrom(ctx)
	defer cancel()

	if err := s.checkLayout(probeCtx); err != nil {
		// Closed on the way out, because a store that could not be opened must
		// not leave a bucket handle behind it. The close error is dropped: the
		// caller is about to be told why the store is unusable, and a failure
		// to tidy up would only obscure it.
		_ = s.bucket.close()

		return nil, err
	}

	return s, nil
}

// isMarkedTransient reports whether a bucket failure has already been judged
// worth another attempt.
//
// The judgement itself is worthRetrying, made inside bucket.do against the
// gcerrors code every driver is obliged to produce, and recorded by wrapping
// the failure. Reading the mark rather than re-deriving it is what keeps this
// store from holding a second opinion about which failures are transient — the
// drift shared.go exists to prevent — and it means a permanent failure such as
// a denied request is returned on the first attempt rather than retried three
// times into the same wall.
func isMarkedTransient(err error) bool {
	var marked *transientBucketError

	return errors.As(err, &marked)
}

// layoutRecord is the object that says which layout wrote this index.
//
// It is this store's schema version. A binary that does not understand the
// layout must not write to it, for exactly the reason a binary that does not
// understand a schema must not write to a database (AC14, Story 4.6 AC5): a
// newer layout may mean keys this build cannot see, and writing beside them
// produces a history that neither build reads correctly.
//
// createdAt and writtenBy are provenance for whoever is looking at a bucket
// with no database beside it. Neither is read back, and neither has to be a
// pure function of anything, because the object is written once with
// IfNotExist: the loser of a race never lands its bytes.
type layoutRecord struct {
	Layout    int    `json:"layout"`
	CreatedAt string `json:"createdAt"`
	WrittenBy string `json:"writtenBy"`
}

// checkLayout establishes that this build understands the index in the bucket,
// and records the layout on a bucket that has none yet.
//
// It is a GET per candidate version and never a listing, which is the whole
// point of the design. A listing is the one bucket operation that may be served
// from a stale view, and a stale listing against a store already at layout 2
// would come back empty, let this build write its own layout object and
// conclude the index was its own — defeating the refusal AC14 asks for with the
// exact consistency property this store refuses to trust anywhere else. A GET
// of a named key is read-after-write consistent on every provider gocloud
// reaches.
//
// Every candidate from 1 to layoutProbeAhead past what this build understands
// is probed, and the loop does not stop at the first absent one. Stopping there
// would miss the case that matters most: a bucket first written by a newer wsaw
// holds 00000003.json and nothing below it — layout objects are written once,
// by the build that lays the index out — so a build that gave up at the first
// gap would see an empty layout, write its own, and read a newer index as if it
// were its own.
func (s *Blob) checkLayout(ctx context.Context) error {
	highest := 0

	for version := 1; version <= indexLayoutVersion+layoutProbeAhead; version++ {
		present, err := s.layoutPresent(ctx, version)
		if err != nil {
			return err
		}

		if present {
			highest = version
		}
	}

	if highest > indexLayoutVersion {
		return fmt.Errorf(
			"the index in %s was written by a newer wsaw (index layout %d, this build understands %d); "+
				"upgrade wsaw or point it at a different bucket",
			s.bucket, highest, indexLayoutVersion,
		)
	}

	if highest > 0 {
		return nil
	}

	return s.recordLayout(ctx)
}

// layoutPresent reports whether the layout object for one version is in the
// bucket, and refuses to guess when it is there and unreadable.
//
// A body that does not decode, or that claims a version its key does not, is
// reported rather than treated as absent. This is the one object that says
// which tree the rest of the index is, so "I could not read it" and "it is not
// there" are different facts and only the second one may lead to writing
// (Tenet 5).
func (s *Blob) layoutPresent(ctx context.Context, version int) (bool, error) {
	key := layoutKey(version)

	body, err := s.bucket.getIndex(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}

		return false, fmt.Errorf("reading the index layout of %s: %w", s.bucket, err)
	}

	var rec layoutRecord

	if err := json.Unmarshal(body, &rec); err != nil {
		return false, fmt.Errorf("the index layout object %s in %s does not decode: %w: %w",
			key, s.bucket, ErrCorrupt, err)
	}

	if rec.Layout != version {
		return false, fmt.Errorf("the index layout object %s in %s claims layout %d: %w",
			key, s.bucket, rec.Layout, ErrCorrupt)
	}

	return true, nil
}

// recordLayout writes the layout object for a bucket that has no index yet.
func (s *Blob) recordLayout(ctx context.Context) error {
	body, err := json.Marshal(layoutRecord{
		Layout: indexLayoutVersion,
		// Seconds are enough: this records which day an index was laid out,
		// not an ordering anything depends on.
		CreatedAt: s.now().UTC().Format(time.RFC3339),
		WrittenBy: "wsaw " + s.version,
	})
	if err != nil {
		return fmt.Errorf("building the index layout object for %s: %w", s.bucket, err)
	}

	// The outcome is discarded because both of its values are success. Created
	// means this process laid the index out; existed means another opener won
	// the race and wrote the same layout number, which is the same fact and
	// leaves nothing to reconcile.
	if _, err := s.bucket.putIndex(ctx, layoutKey(indexLayoutVersion), body); err != nil {
		return fmt.Errorf("recording the index layout of %s: %w", s.bucket, err)
	}

	// Reported because it happens once per bucket and is the one line that
	// tells an operator wsaw has started a history somewhere new. A wrong
	// endpoint, a mistyped prefix or a volume that did not mount produce an
	// empty bucket and a silent start otherwise, and the history the operator
	// meant to be adding to sits elsewhere (Tenet 8).
	s.log.Info("a new index was laid out in the artifact bucket",
		"bucket", s.bucket.String(), "layout", indexLayoutVersion)

	return nil
}

// Driver reports which kind of store this is.
func (s *Blob) Driver() string { return DriverBlob }

// Ping reports whether the store is reachable.
//
// There is one half to check rather than two. For a SQL store readiness has to
// tell a database that is not answering from a bucket that is not, because
// either can fail alone; here the index and the evidence are objects in the
// same bucket, so a bucket that cannot be reached is the whole store being
// unreachable and ErrDatabaseUnreachable can never be the honest answer
// (Story 8.6, AC5).
func (s *Blob) Ping(ctx context.Context) error {
	if err := s.bucket.reachable(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrBucketUnreachable, err)
	}

	return nil
}

// ProbeArtifactBucket writes a small object to the bucket and removes it again,
// so that a bucket which lists happily and refuses writes fails at startup
// rather than at the first scan (Story 8.6, AC4).
//
// It matters more here than it does for a SQL store. There, a bucket that
// refuses writes loses the evidence and keeps the index; here it loses both,
// and the deployment that chose this store is the one with nothing else to fall
// back on.
func (s *Blob) ProbeArtifactBucket(ctx context.Context) error {
	return s.bucket.probeWritable(ctx)
}

// Close releases the bucket.
func (s *Blob) Close() error { return s.bucket.close() }

// PutArtifact stores an evidence file and returns its reference. Artifacts are
// content-addressed, so storing the same screenshot twice costs one copy.
//
// A SQL store follows the write with a claim row, which is what keeps retention
// off those bytes between a scan taking them and the result that names them
// being stored — the case that needs it being an unchanged asset, which
// content-addresses to a key that is already in the bucket and so has no fresh
// write time to protect it (Story 8.5, AC3). This store has no row to write
// that in, so it writes the same fact as an object: one zero-byte take marker
// beside the artifact's pins, named for the instant the bytes were taken (see
// ownerTake). The reverse index that *references* an artifact here is still
// written by PutResult, and a take is not a reference — it expires, and a pin
// written after it releases it.
//
// The take's failure fails the artifact write that asked for it, exactly as
// claimArtifact's does, and for the same reason: an artifact stored without one
// is an artifact a concurrent prune or sweep may collect out from under the
// scan that is about to name it, and reporting the storage of evidence as
// having succeeded when the record that protects it did not is the failure
// Tenet 5 exists to prevent.
func (s *Blob) PutArtifact(kind string, data []byte) (string, error) {
	ctx, cancel := opCtx()
	defer cancel()

	ref, err := s.bucket.put(ctx, kind, data)
	if err != nil {
		return "", err
	}

	if err := s.recordTake(ctx, ref); err != nil {
		return "", err
	}

	return ref, nil
}

// recordTake writes the marker that says wsaw has just taken these bytes.
//
// The key carries the instant and nothing else, so two takes of one artifact in
// one nanosecond are one object rather than two (AC10) and two takes a day
// apart are two — which is what retention needs, because the question it asks
// is when the most recent take was, not how many there have been.
//
// A document is deliberately not taken this way. It is written by putDocument
// rather than through here, it is named by exactly one result, and no other
// scan can content-address to it, so there is no window for a take to close.
// That is also what the SQL stores do: a claim is recorded for an artifact a
// scan captured, never for the document the store itself writes.
func (s *Blob) recordTake(ctx context.Context, ref string) error {
	marker, err := newRefMarker(ref, takeRefOwner(s.now()))
	if err != nil {
		return err
	}

	if _, err := s.putIndex(ctx, marker.String(), nil); err != nil {
		return fmt.Errorf("recording that artifact %s is in use: %w", ref, err)
	}

	return nil
}

// GetArtifact reads a stored artifact.
//
// The reference is validated by the bucket against the shape this store writes,
// so a crafted one cannot address anything else — including anything under the
// index's own root, which no artifact reference can spell (Tenet 9, and see
// indexRoot for why that is a proof rather than a filter).
func (s *Blob) GetArtifact(ref string) ([]byte, error) {
	return s.bucket.artifactBytes(ref)
}

// StatArtifact reports what the store knows about an artifact without reading
// it, so "this evidence has been pruned" and "this evidence is here" can be
// told apart without paying for a megabyte of PNG to find out (Story 5.17, AC3).
func (s *Blob) StatArtifact(ctx context.Context, ref string) (ArtifactInfo, error) {
	return s.bucket.artifactInfo(ctx, ref)
}

// OpenArtifact opens a stored artifact for streaming, which is the shape the
// HTTP layer serves from (Story 8.1, AC7).
//
// The caller owns the reader and must close it; closing is also what releases
// the read's idle bound and its timer.
func (s *Blob) OpenArtifact(ctx context.Context, ref string) (*ArtifactReader, error) {
	return s.bucket.artifactReader(ctx, ref)
}

// SignArtifactURL returns a URL a reader can fetch one artifact from directly,
// valid for ttl and no longer.
//
// It does not establish that the artifact is there, and cannot: signing is
// arithmetic over the key rather than a lookup. A provider that cannot sign at
// all reports ErrSigningUnsupported, which is an invitation to fall back to
// serving the bytes rather than a failure (Story 8.7, AC5).
func (s *Blob) SignArtifactURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	return s.bucket.artifactURL(ctx, ref, ttl)
}

// blobOpBudget bounds one exported method of this store from end to end.
//
// It is a second bound rather than a replacement for opTimeout, because the two
// answer different questions. A SQL store's method is one statement and
// opTimeout bounds it directly; a method here is a listing plus up to a
// thousand small reads, each of which gets an opTimeout of its own — so the
// per-request bound says nothing at all about how long the method may take, and
// without a second one a fold against a slow provider would run unbounded.
//
// Two minutes: long enough that a full page of a thousand entries completes
// against a provider answering in tens of milliseconds with a few retries in
// it, short enough that a caller which cannot pass a context of its own is not
// held past any patience an operator has.
const blobOpBudget = 2 * time.Minute

// opBudget bounds one exported method of this store.
//
// It roots a context of its own for the same reason opCtx does: the store
// interface's read and write methods take none, which is recorded as its own
// story rather than fixed here. The methods that do take a caller's context —
// opening, pruning, sweeping, streaming — use it instead.
func (s *Blob) opBudget() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), blobOpBudget)
}

// The index door, with the per-request deadline applied.
//
// Every read and write of an index object outside the layout probe goes through
// one of these rather than reaching bucket_index.go directly, so that the two
// bounds this store runs under stay apart: opTimeout is what one request may
// take, and blobOpBudget is what the whole method may take. Applying it here
// rather than at each of the several dozen call sites is what stops a fold that
// issues a thousand requests from either sharing one thirty-second deadline
// between all of them or running with no per-request deadline at all.

// getIndex reads one index object under the per-request deadline.
func (s *Blob) getIndex(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return s.bucket.getIndex(ctx, key)
}

// putIndex creates one index key under the per-request deadline, reporting
// whether this call was the one that wrote it.
func (s *Blob) putIndex(ctx context.Context, key string, body []byte) (putOutcome, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return s.bucket.putIndex(ctx, key, body)
}

// statIndex reports what the bucket knows about one index object without
// reading it, under the per-request deadline.
func (s *Blob) statIndex(ctx context.Context, key string) (indexObject, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return s.bucket.statIndex(ctx, key)
}

// listIndex returns one page of an index prefix under the per-request deadline.
func (s *Blob) listIndex(ctx context.Context, prefix string, cursor indexCursor, limit int) (indexPage, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	return s.bucket.listIndexPage(ctx, prefix, cursor, limit)
}

// dropIndex removes one index key under the per-request deadline, reporting a
// key that was already gone as the success it is.
//
// Retention and compaction are the only callers, which is what AC3 means by no
// index object being deleted as part of an ordinary write. Idempotence is what
// lets an interrupted prune resume by simply running again: every delete it
// makes is a no-op the second time.
func (s *Blob) dropIndex(ctx context.Context, key string) error {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	if err := s.bucket.deleteIndex(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	return nil
}
