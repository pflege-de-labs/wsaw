package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// This file is retention against the bucket index: applying the configured
// policy to a history, and collecting the evidence nothing references any more
// (Story 8.10, AC12).
//
// It keeps the shape Story 8.5 defined and answers its one question — does
// anything still reference these bytes — from index objects rather than from a
// query. Three rules run through all of it, and they are the same three
// retention.go opens with.
//
// **Nothing here rewrites a live index object.** A result leaves the history by
// a tombstone appended in front of it and by the deletion of keys no reader can
// still need, which is AC12 literally. The tombstone is written first,
// unconditionally, before anything is deleted: it is what makes a prune's effect
// independent of a listing being fresh, because a stale listing that hid a
// checkpoint would otherwise skip it and leave the pruned result in every
// listing for the life of that checkpoint.
//
// **Deletion is decided by reference and by age, never by age alone.** Artifacts
// are content-addressed, so two scans that captured identical bytes are one
// object and an object's write time says nothing about who needs it. What says
// so here is the pin directory: a pin per reference, and a take per capture that
// has not been stored yet (see ownerTake, and PutArtifact for why this store
// writes the take that a SQL store writes as a claim row).
//
// **A failed delete is counted and left for the next run**, never allowed to
// abort retention (Story 8.5, AC4). The pins are the work list, exactly as the
// reference rows are in SQL: a pin is removed once the artifact it names has
// gone or once something else is keeping it, so a key the bucket refused to
// delete is met again by the next sweep rather than forgotten.

// --- what pins one artifact ------------------------------------------------

// artifactPin is one key in an artifact's pin directory: who holds it, and when
// the bucket says it was written.
//
// The time is the bucket's own, and it is the only clock this file compares two
// index objects with. A pin written after a take is the result that take was
// waiting for, and reading both times from the same clock is what makes that
// comparison mean something on a provider whose clock differs from wsaw's.
type artifactPin struct {
	key     string
	owner   refOwner
	modTime time.Time
}

// artifactPins is one listing of everything pinning one artifact.
type artifactPins struct {
	// refs are the references: a stored result's pin and a standing baseline
	// decision's. They sort ahead of the takes, so the question that decides
	// whether an object may go is answered by the front of the first page.
	refs []artifactPin

	// takes record wsaw storing these bytes for a scan whose result is not in
	// the index yet.
	takes []artifactPin

	// strays counts keys in the directory this grammar does not produce. They
	// are reported and left exactly as they are: a sweep may only collect what
	// wsaw wrote (Story 8.5, AC4).
	strays int
}

// add files one parsed marker.
func (p *artifactPins) add(marker refMarker, object indexObject) {
	pin := artifactPin{key: object.key, owner: marker.owner, modTime: object.modTime}

	if marker.owner.kind == ownerTake {
		p.takes = append(p.takes, pin)

		return
	}

	p.refs = append(p.refs, pin)
}

// observe classifies one key from an artifact's pin directory.
func (p *artifactPins) observe(object indexObject) {
	marker, err := parseRefMarkerKey(object.key)
	if err != nil {
		p.strays++

		return
	}

	p.add(marker, object)
}

// newestRef is when the most recent reference to this artifact was pinned,
// counting the pins this run is about to remove.
//
// It counts them deliberately. A take says "a scan has these bytes and has not
// stored the result that names them yet"; a pin written after it is that result
// arriving, and from that moment the pin is what keeps the object — whether or
// not the pin is now expiring. Ignoring the pins about to go would make every
// artifact of every pruned result look like one a scan had just taken, and
// retention would delete nothing at all.
func (p artifactPins) newestRef() time.Time {
	pin, _ := p.newestPin()

	return pin.modTime
}

// newestPin is that same reference, as the pin rather than as the time,
// reporting separately that there is none.
//
// The sweep needs the pin and not only its age: what an object's newest pin is
// decides whether pins that all read as gone are one write in flight or the
// leftovers of a prune, and those two are told apart by who the pin belongs to
// (sweepRun.inFlight).
func (p artifactPins) newestPin() (artifactPin, bool) {
	var (
		newest artifactPin
		found  bool
	)

	for _, pin := range p.refs {
		if !found || pin.modTime.After(newest.modTime) {
			newest, found = pin, true
		}
	}

	return newest, found
}

// liveTake reports whether a scan that has not finished has these bytes.
//
// Two conditions, and each answers something the other cannot. Younger than the
// grace period, because a take that old belongs to a scan that is not running
// any more (see unreferencedArtifactGrace for why a day is far beyond the
// window it has to cover). Newer than every pin, because a pin written after
// the take is the release: this is how an append-only index expresses what
// releaseClaimsTx does by deleting the claim row, without a write that rewrites
// or deletes anything on the ordinary path (AC3).
//
// **It does not cover a PutResult interrupted between its pins and its byid
// object**, and an earlier draft of §7.6 claimed it did. The claim is inverted
// by the very ordering it rests on: PutArtifact writes the take and PutResult
// writes the pin afterwards, so the dangling pin that proves the scan never
// finished is newer than the take and reads as its release. What covers that
// case is the age of the pins themselves, in sweepRun.tooYoungToCall.
//
// **The release comparison assumes the bucket reports modification times more
// finely than the interval between PutArtifact and PutResult**, and that
// assumption is worth stating because on S3 it does not hold: Last-Modified is
// RFC 1123 and therefore whole seconds, so a take and a pin written inside one
// second come back equal, After is false, and the take reads as released while
// the scan that made it is still running. The residual window is one second,
// and it fails toward deletion. That direction is chosen rather than merely
// suffered: treating equal times as protected would keep every artifact of
// every pruned result whose pin landed in the same second as some take, which
// is what TestPruningDeletesTheArtifactsItStopsReferencing forbids. The
// everyday state is safe for a different reason — every completed scan writes
// its own pin, so newestRef is the newest finished scan rather than an old one
// — and the exposure is a running scan that re-captured unchanged bytes and
// wrote its take in the same second as the newest pin already on them. Do not
// change the comparison to >= without a test that pins both directions.
func (p artifactPins) liveTake(now time.Time) bool {
	var (
		pinned = p.newestRef()
		cutoff = now.Add(-unreferencedArtifactGrace)
	)

	for _, take := range p.takes {
		if take.modTime.After(pinned) && take.modTime.After(cutoff) {
			return true
		}
	}

	return false
}

// staleTakes are the take markers that can no longer protect anything, and so
// are keys no reader can still need.
func (p artifactPins) staleTakes(now time.Time) []artifactPin {
	cutoff := now.Add(-unreferencedArtifactGrace)

	var stale []artifactPin

	for _, take := range p.takes {
		if take.modTime.Before(cutoff) {
			stale = append(stale, take)
		}
	}

	return stale
}

// artifactFate is what retention has decided about one artifact.
type artifactFate int

const (
	// artifactNamed means something that survives this run references it.
	artifactNamed artifactFate = iota
	// artifactTaken means a scan that has not finished has stored these bytes,
	// so nothing may be concluded from the pins yet.
	artifactTaken
	// artifactUnreferenced means nothing references it and nothing holds it.
	artifactUnreferenced
)

// fate decides one artifact's fate from its pins.
//
// removed answers, for one pin, whether this run has taken it away — which is
// the one thing a prune and a sweep answer differently. A prune knows the
// results it is removing and needs no I/O to say so; a sweep has to ask whether
// the thing a pin names is still there, which is why the question takes a
// context and may fail. It is asked pin by pin and stops at the first survivor,
// so the healthy case costs one question.
func (p artifactPins) fate(
	ctx context.Context, now time.Time, removed func(context.Context, artifactPin) (bool, error),
) (artifactFate, error) {
	for _, pin := range p.refs {
		gone, err := removed(ctx, pin)
		if err != nil {
			return artifactNamed, err
		}

		if !gone {
			return artifactNamed, nil
		}
	}

	if p.liveTake(now) {
		return artifactTaken, nil
	}

	return artifactUnreferenced, nil
}

// pinsOn reads everything pinning one artifact.
//
// One listing of one short directory and no reads at all, which is what makes
// "does anything still reference this" affordable against a bucket that charges
// per request (AC12). It pages, because an asset a target re-captures unchanged
// collects one take per scan until the next run reaps them.
func (s *Blob) pinsOn(ctx context.Context, artifact string) (artifactPins, error) {
	prefix, err := refMarkerDirPrefix(artifact)
	if err != nil {
		return artifactPins{}, err
	}

	var (
		pins   artifactPins
		cursor indexCursor
	)

	for cursor.more() {
		page, err := s.listIndex(ctx, prefix, cursor, indexListPageSize)
		if err != nil {
			return artifactPins{}, fmt.Errorf("reading the pins on artifact %s: %w", artifact, err)
		}

		cursor = page.next

		for _, object := range page.objects {
			pins.observe(object)
		}
	}

	return pins, nil
}

// --- what a baseline protects ---------------------------------------------

// baselineGuard is what the standing baseline decision of one series keeps
// alive through a prune of the history around it.
//
// It is a positive check and not the absence of a marker, which is the whole
// point: a decision's pins would keep the evidence anyway, but a listing served
// from a stale view can hide a pin, and inferring "nothing needs this" from a
// listing that came back short is how a prune deletes the evidence of an
// approved scan. So the decision itself is read, and what it names is protected
// by name (§7.3).
type baselineGuard struct {
	// current is the decision in force, and found says whether there is one.
	current decisionKey
	found   bool

	// refs is every artifact a standing approval names, including the approved
	// scan's document. A withdrawal names nothing, which is what makes
	// approve-then-revoke-then-prune collect the evidence again.
	refs map[string]struct{}

	// prior is every decision key the listing showed, for the audit-pointer
	// self-heal of §6.5 step 6.
	prior []decisionKey

	// mine is the body digest of each of those, which is what a decision pin
	// carries. It is what tells a decision pin of *this* series apart from one
	// of any other, and see supersededDecision for why the difference decides
	// whether the pin is this run's to remove.
	mine map[string]struct{}
}

// protects reports whether the standing decision names this artifact.
func (g baselineGuard) protects(artifact string) bool {
	_, named := g.refs[artifact]

	return named
}

// baselineProtection reads the decision in force for one series and what it
// names. One listing and, when something is approved, one read.
func (s *Blob) baselineProtection(ctx context.Context, series blobSeries) (baselineGuard, error) {
	keys, err := s.readDecisions(ctx, series)
	if err != nil {
		return baselineGuard{}, err
	}

	guard := baselineGuard{
		refs:  make(map[string]struct{}),
		prior: keys,
		mine:  make(map[string]struct{}, len(keys)),
	}

	for _, key := range keys {
		guard.mine[key.did] = struct{}{}
	}

	current, ok := currentDecision(keys)
	if !ok {
		return guard, nil
	}

	guard.current, guard.found = current, true

	if current.op != opApprove {
		return guard, nil
	}

	body, err := s.readDecision(ctx, current)
	if err != nil {
		return baselineGuard{}, err
	}

	for _, ref := range body.Refs {
		guard.refs[ref] = struct{}{}
	}

	return guard, nil
}

// --- the prune -------------------------------------------------------------

// Prune applies a retention policy, deleting the results it ages out and the
// evidence they were the last to name.
//
// Per doomed result, in the order §7.3 argues for: the tombstone first and
// unconditionally, then the loose entry key, then the object addressed by scan
// ID — before any artifact, so a reader that loses the race gets a clean
// ErrNotFound rather than the ErrEvidenceGone the interface renders as lost
// evidence (Story 5.17, AC3). Only then are the artifacts considered.
//
// The artifacts are considered once each and not once per result that named
// them, which is the shape SQL's own work list has: two expiring results that
// share a screenshot must produce one decision about that screenshot, or the
// second would count bytes the first already reclaimed.
//
// Baselines are never pruned and neither is a decision object, which is why
// deleting one is not on the list above: readDecision treats a listed decision
// whose object is absent as corruption rather than as an absent baseline, so a
// prune that removed one would silence exactly the findings nobody approved.
func (s *Blob) Prune(ctx context.Context, now time.Time, r Retention) (PruneStats, error) {
	return s.prune(ctx, now, r, false)
}

// PlanPrune reports what Prune would delete, and writes nothing at all —
// including no tombstone and no audit-pointer self-heal (§7.3).
//
// It answers from the same walk and the same rules rather than from a second
// definition of "what this would orphan", so a dry run cannot describe a
// different set from the prune it claims to describe (Story 8.5, AC6). What it
// keeps that a prune does not is the set of pins it has decided it would
// remove: a prune deletes them as it goes and the next listing shows the state
// it has produced, while a plan would otherwise judge the second series to name
// a shared artifact against pins the prune would already have removed, and
// promise to keep an object the prune deletes.
func (s *Blob) PlanPrune(ctx context.Context, now time.Time, r Retention) (PruneStats, error) {
	return s.prune(ctx, now, r, true)
}

// pruneRun is one prune, or one plan of one.
type pruneRun struct {
	s     *Blob
	now   time.Time
	plan  bool
	stats PruneStats

	// gone is the pins a plan has decided it would remove, keyed by their own
	// keys. It is nil for a prune, which needs no such set; see PlanPrune.
	gone map[string]struct{}

	// decisions is the digest of every baseline decision the index holds, read
	// once for the whole run. A decision pin carries the digest and nothing
	// else that identifies it, so this is what separates "a decision of some
	// other series, which this run may not judge" from "a pin whose decision
	// does not exist"; see supersededDecision.
	decisions map[string]struct{}
}

func (s *Blob) prune(ctx context.Context, now time.Time, r Retention, plan bool) (PruneStats, error) {
	run := &pruneRun{s: s, now: now, plan: plan}

	if plan {
		run.gone = make(map[string]struct{})
	}

	if r.MaxAge <= 0 && r.MaxPerSeries <= 0 {
		return run.stats, nil
	}

	// Established before anything is written or deleted, and for the reason
	// retention.go gives: the collection path reads "this key is already gone"
	// as "somebody else collected it", which is right for one key and wrong for
	// a bucket that has gone away. Here it is worse than for a SQL store,
	// because the index is in that bucket too — a prune against an unmounted
	// volume would find no series, report a clean run over an empty store, and
	// have deleted nothing while saying so (Tenet 5).
	if err := s.bucket.reachable(ctx); err != nil {
		return run.stats, fmt.Errorf("pruning needs the artifact bucket: %w", err)
	}

	decisions, err := s.decisionDigests(ctx)
	if err != nil {
		return run.stats, err
	}

	run.decisions = decisions

	series, err := s.seriesList(ctx)
	if err != nil {
		return run.stats, err
	}

	for _, one := range series {
		if err := ctx.Err(); err != nil {
			return run.stats, fmt.Errorf("pruning the bucket index: %w", err)
		}

		if err := run.pruneSeries(ctx, blobSeriesFor(one.Target, one.Mode), r); err != nil {
			return run.stats, err
		}
	}

	return run.stats, nil
}

// pruneSeries applies the policy to one target and consent mode.
func (r *pruneRun) pruneSeries(ctx context.Context, series blobSeries, ret Retention) error {
	// The whole history, from one listing of one directory: the tombstones, the
	// checkpoints and every loose entry come back together, so what is doomed
	// is decided from one visible key set rather than from several listings that
	// may disagree (AC7). It is the only fold in this store that asks for every
	// entry, because retention is the only operation whose answer is about the
	// oldest of them.
	entries, err := r.s.foldKeys(ctx, series, math.MaxInt, false, nil)
	if err != nil {
		return err
	}

	candidates, err := r.s.expiredEntries(ctx, series, entries, r.now, ret)
	if err != nil {
		return err
	}

	if len(candidates) == 0 {
		return nil
	}

	doomed, err := r.resolve(ctx, series, candidates)
	if err != nil {
		return err
	}

	if len(doomed) == 0 {
		return nil
	}

	guard, err := r.s.baselineProtection(ctx, series)
	if err != nil {
		return err
	}

	if !r.plan {
		// §6.5 step 6: a decision whose derived audit pointer never landed gets
		// it back here. It cannot fail the prune — it is the repair of an
		// earlier interrupted write, and a prune that refused to run because it
		// could not finish one would be a retention policy an old approval had
		// switched off.
		//
		// Bounded to the newest auditHealProbes decisions of this series, for
		// the same reason it is bounded on the approval path and one more: a
		// prune walks every series, so an unbounded probe per series would make
		// a nightly retention run cost an attribute read per decision this
		// store has ever recorded.
		r.s.healAuditPointers(ctx, series, guard.prior)
	}

	if err := r.removeResults(ctx, series, doomed); err != nil {
		return err
	}

	if err := r.collectArtifacts(ctx, series, doomed, guard); err != nil {
		return err
	}

	if len(doomed) == len(entries) {
		return r.forgetSeries(ctx, series)
	}

	return nil
}

// doomedEntry is one result retention no longer keeps, with the body that says
// what it references.
type doomedEntry struct {
	record seriesRecord
	body   entryBody

	// loose says the fold took this entry's body from an object of its own
	// rather than out of a checkpoint, and therefore that there is a key to
	// delete. A compaction folds an entry's body into a checkpoint and deletes
	// the loose key, so a prune reaching a scan that old has nothing to remove
	// but the tombstone: issuing the delete anyway would be a request per
	// pruned result against a key this run never listed, which is the one thing
	// invariant I3 says a deletion must follow.
	//
	// Inside Options.CheckpointGrace a covered scan still has both, and the
	// fold deliberately prefers the checkpoint's copy (compareFoldEntries), so
	// this reads false while a loose key is still there. The key is not leaked:
	// it is covered, which is exactly the precondition collectCovered deletes
	// it under, and a prune claiming a delete the compactor already owns is how
	// two passes end up disagreeing about which keys this run has seen.
	loose bool
}

// expiredEntries selects the entries one retention policy no longer keeps.
//
// Both limits are applied to the same fold, in the same order every listing
// uses, so "the newest N" means the same thing to retention as it does to the
// interface and a result that matches both limits is selected once.
func (s *Blob) expiredEntries(
	ctx context.Context, series blobSeries, entries []foldEntry, now time.Time, ret Retention,
) ([]foldEntry, error) {
	cutoff := now.Add(-ret.MaxAge)

	var out []foldEntry

	for i, entry := range entries {
		expired := ret.MaxPerSeries > 0 && i >= ret.MaxPerSeries

		if !expired && ret.MaxAge > 0 {
			old, err := s.startedBefore(ctx, series, entry, cutoff)
			if err != nil {
				return nil, err
			}

			expired = old
		}

		if expired {
			out = append(out, entry)
		}
	}

	return out, nil
}

// invAtEpoch and invAtHorizon are the two ordering fields that mean "at or
// beyond the edge of the range this grammar can order" rather than an instant.
//
// They are derived from inv rather than written out, so that a change to the
// ordering field cannot leave this check looking for a spelling nothing
// produces any more.
var (
	invAtEpoch   = inv(indexEpoch)
	invAtHorizon = inv(indexHorizon)
)

// clampedInstant reports whether an ordering field is one of those two.
func clampedInstant(field string) bool {
	return field == invAtEpoch || field == invAtHorizon
}

// startedBefore reports whether one entry's scan started before cutoff,
// deciding from the key wherever the key can decide.
//
// The ordering field is a total function of the start time, so retention by age
// is answered from the listing and reads no bodies at all — which is what keeps
// a prune of a series to one listing (§7.1). The exception is an entry whose
// field sits at either edge of the range: inv clamps a timestamp outside
// 1970–2262 rather than wrapping it, so the field there says "at or beyond the
// edge" and not when. Those cost the one body read that lets the recorded start
// time decide, rather than being pruned or kept on the strength of a clamp.
func (s *Blob) startedBefore(
	ctx context.Context, series blobSeries, entry foldEntry, cutoff time.Time,
) (bool, error) {
	if !clampedInstant(entry.record.inv) {
		return instantAt(entry.record.inv).Before(cutoff), nil
	}

	body, err := s.readEntry(ctx, series, entry)
	if err != nil {
		return false, err
	}

	if body == nil {
		// Deleted between the listing and the read, which is what a concurrent
		// prune does. There is nothing left to expire.
		return false, nil
	}

	return body.Summary.StartedAt.Before(cutoff), nil
}

// resolve reads the bodies of the entries a prune is about to remove.
//
// It has to: the body is where an entry says which artifacts its result named
// (Story 8.5, AC2), so it is what turns "this result goes" into "these objects
// have lost an owner" at no cost to any other read path. Sixteen at a time, and
// an entry a checkpoint already supplied the body for costs no request at all.
//
// An entry whose object is present and unreadable is left in the history and
// counted in PruneStats.UnknownReferences. Retention cannot say what it
// referenced, and deleting a result whose evidence cannot be accounted for
// would be inferring absence from a gap in wsaw's own index (Tenet 5) — the
// same judgement collectable makes for the SQL stores, made per entry because
// here it can be.
func (r *pruneRun) resolve(ctx context.Context, series blobSeries, doomed []foldEntry) ([]doomedEntry, error) {
	bodies := make([]*entryBody, len(doomed))

	err := eachBounded(ctx, len(doomed), blobHydrateConcurrency, func(ctx context.Context, i int) error {
		body, err := r.s.readEntry(ctx, series, doomed[i])
		if err != nil {
			return err
		}

		bodies[i] = body

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]doomedEntry, 0, len(doomed))

	for i, body := range bodies {
		switch {
		case body == nil:
			// Deleted while this prune was reading it.
		case body.Document.Ref == "":
			r.stats.UnknownReferences++

			r.s.log.Warn("a result retention would remove does not say where its document is, "+
				"so it was left in the history",
				"key", doomed[i].record.String(), "target", series.target, "consent_mode", string(series.mode))
		default:
			// A body the fold already held came out of a checkpoint; one it had
			// to read came from a loose object, which is the key removeResult
			// deletes.
			out = append(out, doomedEntry{
				record: doomed[i].record, body: *body, loose: doomed[i].body == nil,
			})
		}
	}

	return out, nil
}

// removeResults takes the doomed results out of the index.
func (r *pruneRun) removeResults(ctx context.Context, series blobSeries, doomed []doomedEntry) error {
	for _, d := range doomed {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pruning %s: %w", series, err)
		}

		if !r.plan {
			if err := r.removeResult(ctx, series, d); err != nil {
				return err
			}
		}

		r.stats.ResultsDeleted++

		if r.plan {
			r.stats.Results = append(r.stats.Results, PrunedResult{
				Target:      d.body.Summary.Target,
				ConsentMode: d.body.Summary.ConsentMode,
				ScanID:      d.body.Summary.ScanID,
				StartedAt:   d.body.Summary.StartedAt,
			})
		}
	}

	return nil
}

// removeResult takes one result out of the index, tombstone first.
//
// The tombstone is the one step here whose failure fails the prune. Everything
// after it is a deletion, and a deletion that fails is harmless — the tombstone
// already hides the entry, and the key is met again by the next run — but a
// tombstone that did not land and an entry that did get deleted would be a
// result removed from the listings without the record that says a checkpoint
// must not bring it back.
func (r *pruneRun) removeResult(ctx context.Context, series blobSeries, d doomedEntry) error {
	// instantAt inverts inv exactly on every field inv can produce, clamps
	// included, so the tombstone carries the same ordering field as the entry it
	// suppresses and one listing pairs them.
	tombstone := series.id.tombstone(instantAt(d.record.inv), d.record.scan)

	if _, err := r.s.putIndex(ctx, tombstone.String(), nil); err != nil {
		return fmt.Errorf("tombstoning scan %s of %s: %w",
			truncateForMessage(d.body.Summary.ScanID), series, err)
	}

	if d.loose {
		if err := r.s.dropIndex(ctx, d.record.String()); err != nil {
			r.s.log.Warn("a pruned result's entry could not be deleted, and is hidden by its tombstone until it is",
				"key", d.record.String(), "error", err)
		}
	}

	if err := r.s.dropIndex(ctx, series.id.byIDKey(d.record.scan)); err != nil {
		// Counted apart from the artifacts, because it means something
		// different: this result is gone from every listing and still fetchable
		// by its scan ID, so a share link to it still resolves. The next sweep
		// collects the key (§7.4).
		r.stats.IndexKeysFailed++

		r.s.log.Warn("a pruned result is still reachable by its scan ID, and will be until the next sweep",
			"scan_id", truncateForMessage(d.body.Summary.ScanID), "target", series.target,
			"consent_mode", string(series.mode), "error", err)
	}

	return nil
}

// collectArtifacts decides the fate of every artifact the doomed results named.
func (r *pruneRun) collectArtifacts(
	ctx context.Context, series blobSeries, doomed []doomedEntry, guard baselineGuard,
) error {
	owners := make(map[string]struct{}, len(doomed))
	artifacts := make([]string, 0, 3*len(doomed))

	for _, d := range doomed {
		owners[resultRefOwner(series.id, d.record.scan).String()] = struct{}{}
		artifacts = append(artifacts, d.body.Refs...)
	}

	// Sorted and deduplicated so that each artifact is decided once, whichever
	// of the expiring results named it, and in an order that does not depend on
	// which of them the fold returned first.
	slices.Sort(artifacts)
	artifacts = slices.Compact(artifacts)

	removed := r.removing(owners, guard)

	for _, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("collecting the artifacts of %s: %w", series, err)
		}

		if err := r.collectOne(ctx, artifact, guard, removed); err != nil {
			return err
		}
	}

	return nil
}

// removing answers, for one pin, whether this prune is taking it away.
//
// Three ways it can be: it belongs to one of the results expiring now, it
// belongs to a baseline decision that is no longer in force, or a plan has
// already decided it would go.
func (r *pruneRun) removing(
	owners map[string]struct{}, guard baselineGuard,
) func(context.Context, artifactPin) (bool, error) {
	return func(_ context.Context, pin artifactPin) (bool, error) {
		if _, planned := r.gone[pin.key]; planned {
			return true, nil
		}

		switch pin.owner.kind {
		case ownerResult:
			_, doomed := owners[pin.owner.String()]

			return doomed, nil
		case ownerDecision:
			return r.supersededDecision(pin, guard), nil
		default:
			return false, nil
		}
	}
}

// supersededDecision reports whether one decision pin belongs to a decision
// that is no longer in force, and has existed long enough to be sure of it.
//
// Without it every superseded and every revoked approval would pin its result
// document and every screenshot it named for the life of the bucket, and
// "retention deletes what it stops referencing" would be false.
//
// The three tests below are three different questions, and an earlier version
// asked only the last two — with the effect that a prune of one series read
// *another* series' standing baseline pin as a superseded approval. Artifacts
// are content-addressed, so one asset captured identically in two consent modes
// is one object with a pin from each, and pruning the unapproved series then
// deleted the evidence the approved one's copy names. That is a compliance
// failure and not a leak: GetBaseline goes on answering and every screenshot it
// points at is gone.
//
//   - **Old enough.** A pin younger than the grace period may belong to a
//     decision this run's listings could not see, so it is left for the next
//     one. This is the same guard the rest of the file uses against a stale
//     listing, and it is asked first because it is free.
//   - **Dangling.** A pin whose decision is in none of this run's listings at
//     all is the leftover of an interrupted approval or of a prune whose
//     deletes the bucket refused, and it protects nothing. The digests were
//     read once, at the start of the run, over one prefix of human-generated
//     volume — the same listing the sweep makes for the same question.
//   - **This series', and not the one in force.** A decision this run *can* see
//     is only this prune's to unpin when the prune knows it has been
//     superseded, and it only knows that for the series it is holding the fold
//     of. Another series' decision is left exactly as it is; the prune of that
//     series is what decides it.
func (r *pruneRun) supersededDecision(pin artifactPin, guard baselineGuard) bool {
	if !pin.modTime.Before(r.now.Add(-unreferencedArtifactGrace)) {
		return false
	}

	if _, held := r.decisions[pin.owner.did]; !held {
		return true
	}

	if _, ours := guard.mine[pin.owner.did]; !ours {
		return false
	}

	return !guard.found || pin.owner.did != guard.current.did
}

// collectOne decides one artifact's fate and acts on it.
func (r *pruneRun) collectOne(
	ctx context.Context, artifact string, guard baselineGuard,
	removed func(context.Context, artifactPin) (bool, error),
) error {
	pins, err := r.s.pinsOn(ctx, artifact)
	if err != nil {
		return err
	}

	fate := artifactNamed

	// The positive check first, and before the pins are consulted at all: a
	// baseline holds its own copy of the approved scan precisely so that
	// expiring history does not invalidate what "expected" means, and the
	// evidence that copy names has to survive with it (Story 8.5, AC1).
	if !guard.protects(artifact) {
		if fate, err = pins.fate(ctx, r.now, removed); err != nil {
			return err
		}
	}

	switch fate {
	case artifactNamed:
		r.unpin(ctx, pins, removed)

		return nil

	case artifactTaken:
		r.stats.ArtifactsProtected++

		return nil
	}

	if r.plan {
		size, err := measureArtifact(ctx, r.s.bucket, artifact)
		if err != nil {
			// Still counted, at zero bytes: a plan that dropped it from the
			// list would understate what is about to be deleted, which is the
			// one direction a dry run must not err in.
			r.s.log.Debug("an artifact a prune would delete could not be measured",
				"artifact", artifact, "error", err)
		}

		r.stats.Artifacts = append(r.stats.Artifacts, artifact)
		r.stats.ArtifactsDeleted++
		r.stats.BytesFreed += size
		r.unpin(ctx, pins, removed)

		return nil
	}

	size, deleted := reclaimArtifact(ctx, r.s.bucket, r.s.log, artifact)
	if !deleted {
		// Left exactly as it is, pins included, so the next sweep finds it
		// again (Story 8.5, AC4). Retention has still done its job: the result
		// is out of the history.
		r.stats.ArtifactsFailed++

		return nil
	}

	r.stats.ArtifactsDeleted++
	r.stats.BytesFreed += size
	r.unpin(ctx, pins, removed)

	return nil
}

// unpin removes the pins this run has taken away, and reaps the takes that can
// no longer protect anything.
//
// It runs after the artifact has gone rather than before, which is the one place
// this file departs from §7.3's step order, and the reason is AC4. The pins are
// the work list: a pin removed before an artifact the bucket then refused to
// delete would leave that object with nothing pointing at it, and a sweep would
// find it young and unreferenced and protect it by age for a day — a refused
// delete turned into a leak, where SQL leaves the reference row and meets the
// key again on the next run. Interrupting between the two leaves a pin whose
// owner is gone, which is the sweep's dangling-owner case and exactly what an
// interrupted SQL prune leaves as well.
func (r *pruneRun) unpin(
	ctx context.Context, pins artifactPins, removed func(context.Context, artifactPin) (bool, error),
) {
	for _, pin := range pins.refs {
		gone, err := removed(ctx, pin)
		if err != nil || !gone {
			continue
		}

		if r.plan {
			r.gone[pin.key] = struct{}{}

			continue
		}

		if err := r.s.dropIndex(ctx, pin.key); err != nil {
			// Not counted against the artifacts: nothing was left unreclaimed.
			// A pin whose owner has gone is what the sweep's first pass is for,
			// so this heals rather than accumulating.
			r.s.log.Warn("a pin on an artifact retention has stopped referencing could not be removed",
				"key", pin.key, "error", err)
		}
	}

	if r.plan {
		return
	}

	for _, take := range pins.staleTakes(r.now) {
		if err := r.s.dropIndex(ctx, take.key); err != nil {
			r.s.log.Debug("a take marker that protects nothing any more could not be removed",
				"key", take.key, "error", err)
		}
	}
}

// forgetSeries removes the marker that puts a target and consent mode in
// Series(), once its history has expired entirely.
//
// SQL's Series() is a select distinct over the results, so a series with no rows
// left disappears from it; without this the dashboard would render
// decommissioned targets for ever. PutResult re-creates the marker with a
// conditional create, so a scan arriving in the same moment is unharmed.
//
// The listing is what decides, not the arithmetic above it: a checkpoint or an
// entry written since the fold means the history is not empty after all, and
// one listing is a great deal cheaper than a target that vanishes from the
// dashboard while its scans are still arriving.
func (r *pruneRun) forgetSeries(ctx context.Context, series blobSeries) error {
	if r.plan {
		return nil
	}

	var cursor indexCursor

	for cursor.more() {
		page, err := r.s.listIndex(ctx, series.id.dirPrefix(), cursor, indexListPageSize)
		if err != nil {
			return fmt.Errorf("checking whether the history of %s is empty: %w", series, err)
		}

		cursor = page.next

		for _, object := range page.objects {
			record, err := parseSeriesRecord(object.key)
			if err != nil || record.kind == seriesTombstone {
				continue
			}

			return nil
		}
	}

	if err := r.s.dropIndex(ctx, series.id.markerKey()); err != nil {
		r.s.log.Warn("the marker of a series whose history has expired could not be removed",
			"target", series.target, "consent_mode", string(series.mode), "error", err)

		return nil
	}

	r.s.log.Info("a series whose history has expired entirely was removed from the target list",
		"target", series.target, "consent_mode", string(series.mode))

	return nil
}

// --- the sweep -------------------------------------------------------------

// Sweep collects artifacts in the bucket that no result and no baseline
// references any more.
//
// It is the ordered merge join §7.4 describes and not one listing per artifact.
// Within one kind, "<kind>/<digest>" and
// "_wsaw/index/v1/ref/<kind>/<digest>/<owner>" both sort by digest and §4.2
// guarantees both listings come back in that order on every provider, so the
// two are streamed side by side in constant memory. The naive form — asking the
// bucket for the pins of each artifact in turn — would be six million listings
// on the deployment §7.5 describes.
//
// **It is listed one kind at a time and never as a whole**, for the reason
// indexRefPrefix states: "screenshot" is a proper prefix of
// "screenshot-after-consent", so a listing that spanned the kinds would come
// back in one order on a local disk and another on S3. Within a kind every child
// is a fixed-width digest and no name can be a prefix of a sibling.
//
// What it may collect is bounded twice over. A result document is never
// collected, because it decodes to the scan it records and is therefore a
// candidate for a rebuild rather than garbage (AC6, §7.4); and nothing at all is
// collected while a rebuild is in progress, because "I have no pin for this
// artifact" and "I have not derived the pins yet" are the same observation from
// out here.
func (s *Blob) Sweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error) {
	return s.sweep(ctx, now, opts, false)
}

// PlanSweep reports what Sweep would collect, and deletes nothing (Story 8.5,
// AC6). The sizes come from the listing that found the keys, so a plan of the
// bucket half costs no request of its own.
func (s *Blob) PlanSweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error) {
	return s.sweep(ctx, now, opts, true)
}

// sweepRun is one sweep, or one plan of one.
type sweepRun struct {
	s     *Blob
	now   time.Time
	plan  bool
	opts  SweepOptions
	stats SweepStats

	// decisions is the digest of every baseline decision the index holds.
	//
	// A decision pin names its decision by that digest alone, which is not a
	// key: the decision object's key also carries the series and the instant.
	// So the set is read once per sweep — one listing of a prefix that holds
	// human-generated volume — rather than resolved per pin, which would be a
	// listing per pin.
	decisions map[string]struct{}

	// tombstoned is the scans each series' tombstones name, filled in as the
	// walk meets a pin whose owner is gone; see tombstonedScans.
	tombstoned map[seriesID]map[encodedScan]struct{}
}

func (s *Blob) sweep(ctx context.Context, now time.Time, opts SweepOptions, plan bool) (SweepStats, error) {
	run := &sweepRun{
		s: s, now: now, plan: plan, opts: opts,
		tombstoned: make(map[seriesID]map[encodedScan]struct{}),
	}

	// The same reason the prune establishes it, and the same sharper version:
	// this store's index is in the bucket it is sweeping, so a bucket that has
	// gone away is a store that knows nothing rather than a store whose bucket
	// is empty (Tenet 5).
	if err := s.bucket.reachable(ctx); err != nil {
		return run.stats, fmt.Errorf("sweeping needs the artifact bucket: %w", err)
	}

	rebuilding, err := s.rebuildsInProgress(ctx)
	if err != nil {
		return run.stats, err
	}

	if rebuilding > 0 {
		// The blob analogue of SQL's resultsWithUnknownRefs safety valve, which
		// has no counterpart here: a rebuild interrupted at 40 % followed by a
		// sweep would delete the evidence of the other 60 %. Story 8.10 honours
		// the marker; Story 8.11 writes it.
		//
		// Reported in a field of its own rather than as UnknownReferences,
		// because that one means "a stored result's document is missing or no
		// longer decodes" and the command prints it as exactly that. A rebuild
		// in progress is not a fact about the evidence at all (see
		// SweepStats.RebuildInProgress).
		run.stats.RebuildInProgress = rebuilding

		s.log.Warn("a rebuild of the index is in progress, so the sweep collected nothing",
			"markers", rebuilding, "bucket", s.bucket.String())

		return run.stats, nil
	}

	// Asked before anything is collected, because collecting changes the
	// answer: the pins that establish that this store and this bucket belong
	// together are the very keys a sweep removes.
	if err := s.indexCanJudge(ctx, opts); err != nil {
		return run.stats, err
	}

	if err := run.readDecisions(ctx); err != nil {
		return run.stats, err
	}

	if err := run.walkArtifacts(ctx); err != nil {
		return run.stats, err
	}

	return run.stats, run.collectOrphanedScanKeys(ctx)
}

// rebuildsInProgress counts the markers a rebuild leaves while it runs.
func (s *Blob) rebuildsInProgress(ctx context.Context) (int, error) {
	var (
		found  int
		cursor indexCursor
	)

	for cursor.more() {
		page, err := s.listIndex(ctx, indexRebuildPrefix, cursor, indexListPageSize)
		if err != nil {
			return 0, fmt.Errorf("checking whether a rebuild of the index is in progress: %w", err)
		}

		cursor = page.next
		found += len(page.objects)
	}

	return found, nil
}

// indexCanJudge refuses a sweep when the index has nothing to judge the bucket
// with (Story 8.5, AC3, and see ErrEmptyIndex).
func (s *Blob) indexCanJudge(ctx context.Context, opts SweepOptions) error {
	if opts.AllowEmptyIndex {
		return nil
	}

	empty, err := s.indexIsEmpty(ctx)
	if err != nil {
		return err
	}

	if empty {
		return fmt.Errorf("%w: if the bucket really does hold nothing but garbage, sweep it with the override; "+
			"otherwise the index this store should hold is missing, and deleting on that basis is not recoverable",
			ErrEmptyIndex)
	}

	return nil
}

// indexIsEmpty reports whether this index says anything at all about any
// artifact.
//
// Three prefixes, which are this store's answer to the four tables
// indexIsEmpty checks in SQL, for the same reasons. A series marker means a
// result exists; a baseline decision names evidence directly; and a pin is
// either a reference a prune could not finish removing or a take recording that
// this store wrote an artifact here — the two rows SQL keeps apart, which are
// one prefix in this layout. Only a bucket where all three are empty holds an
// index that knows nothing, and that is the state a restored history without its
// index, a wrong bucket, or a fresh store opened against somebody else's
// evidence is in.
func (s *Blob) indexIsEmpty(ctx context.Context) (bool, error) {
	for _, prefix := range []string{indexTargetsPrefix, indexBaselinePrefix, indexRefPrefix} {
		var cursor indexCursor

		for cursor.more() {
			page, err := s.listIndex(ctx, prefix, cursor, indexListPageSize)
			if err != nil {
				return false, fmt.Errorf("checking whether the bucket index holds anything: %w", err)
			}

			if len(page.objects) > 0 {
				return false, nil
			}

			cursor = page.next
		}
	}

	return true, nil
}

// readDecisions loads the digest of every baseline decision the index holds.
func (r *sweepRun) readDecisions(ctx context.Context) error {
	decisions, err := r.s.decisionDigests(ctx)
	if err != nil {
		return err
	}

	r.decisions = decisions

	return nil
}

// decisionDigests is the body digest of every baseline decision the index
// holds, which is what a decision pin names its owner by.
//
// One walk of one prefix, and it is affordable for the reason §7.2 leaves that
// prefix uncompacted: decisions are human-generated, so the whole of them is a
// handful of pages on any deployment. Both passes over the pins need it and
// both need it whole — a decision missing from the set reads as a pin whose
// owner is gone, which is a deletion — so it is read once, before either starts
// judging anything, rather than per series or per artifact.
//
// A key this grammar does not produce is stepped over rather than failing the
// walk, the same judgement observeDecisions makes: something else has written
// into the index, and refusing to run retention because of it would turn one
// stray object into a bucket that never reclaims anything.
func (s *Blob) decisionDigests(ctx context.Context) (map[string]struct{}, error) {
	out := make(map[string]struct{})

	var cursor indexCursor

	for cursor.more() {
		page, err := s.listIndex(ctx, indexBaselinePrefix, cursor, indexListPageSize)
		if err != nil {
			return nil, fmt.Errorf("listing the baseline decisions in %s: %w", s.bucket, err)
		}

		cursor = page.next

		for _, object := range page.objects {
			key, err := parseDecisionKey(object.key)
			if err != nil {
				continue
			}

			out[key.did] = struct{}{}
		}
	}

	return out, nil
}

// walkArtifacts streams the evidence against the pins, one kind at a time.
func (r *sweepRun) walkArtifacts(ctx context.Context) error {
	kinds, err := r.kinds(ctx)
	if err != nil {
		return err
	}

	for _, kind := range kinds {
		if err := r.sweepKind(ctx, kind); err != nil {
			return err
		}
	}

	return nil
}

// kinds is every artifact kind this sweep has to walk, and the census of what
// in the bucket is not wsaw's at all.
//
// One delimited listing of the root rather than a walk of it: what is wanted is
// which kinds are present, and asking for the kinds costs one page where walking
// every object under them would cost the whole bucket twice.
//
// The kinds that have pins but no objects are included as well, so that a stray
// key under one of them is still reported. A kind is discovered from either
// side or from both.
func (r *sweepRun) kinds(ctx context.Context) ([]string, error) {
	found := make(map[string]struct{})

	err := r.s.bucket.walk(ctx, "", refSeparator, func(entry bucketEntry) error {
		if !entry.dir {
			// A loose object at the root. Every reference this store writes has
			// two segments, so this is not one of ours.
			r.foreign(entry.key)

			return nil
		}

		switch kind := strings.TrimSuffix(entry.key, refSeparator); {
		case entry.key == indexRoot:
			// The index tree, which is not evidence and is not walked: it is
			// eighteen million keys on the deployment §7.5 describes, and what a
			// sweep wants from it is the pins of the artifacts it is looking at.
		case validKind(kind):
			found[kind] = struct{}{}
		default:
			return r.foreignTree(ctx, entry.key)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := r.pinnedKinds(ctx, found); err != nil {
		return nil, err
	}

	return slices.Sorted(maps.Keys(found)), nil
}

// pinnedKinds adds the kinds that have a pin directory, whether or not any of
// their objects are left.
func (r *sweepRun) pinnedKinds(ctx context.Context, found map[string]struct{}) error {
	return r.s.bucket.walk(ctx, indexRefPrefix, refSeparator, func(entry bucketEntry) error {
		if !entry.dir {
			return nil
		}

		kind := strings.TrimSuffix(strings.TrimPrefix(entry.key, indexRefPrefix), refSeparator)
		if validKind(kind) {
			found[kind] = struct{}{}
		}

		return nil
	})
}

// foreign counts one object the sweep will not touch.
func (r *sweepRun) foreign(key string) {
	// Not counted as scanned either: ArtifactsScanned is what wsaw holds in the
	// bucket, and this is not wsaw's.
	r.stats.ForeignObjects++

	r.s.log.Debug("an object in the artifact bucket was not written by wsaw, so the sweep left it alone",
		"key", truncateForMessage(key), "bucket", r.s.bucket.String())
}

// foreignTree counts the objects under a directory that is not a kind this
// store writes.
//
// Attempting to delete them would fail — the bucket seam refuses a reference it
// did not write — so every stray object would be reported as a delete the
// bucket refused, on every run, and ArtifactsFailed would stop meaning what it
// says. Deleting them would be worse still: a sweep reaching outside what wsaw
// wrote.
func (r *sweepRun) foreignTree(ctx context.Context, prefix string) error {
	return r.s.bucket.walk(ctx, prefix, "", func(entry bucketEntry) error {
		if !entry.dir {
			r.foreign(entry.key)
		}

		return nil
	})
}

// sweepKind streams one kind's objects against one kind's pins.
func (r *sweepRun) sweepKind(ctx context.Context, kind string) error {
	pins := &pinStream{s: r.s, prefix: indexRefPrefix + kind + refSeparator}

	err := r.s.bucket.list(ctx, kind+refSeparator, func(obj artifactObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !isArtifactRef(obj.ref) {
			r.foreign(obj.ref)

			return nil
		}

		found, ordered, err := pins.upTo(ctx, obj.ref)
		if err != nil {
			return err
		}

		if !ordered {
			if found, err = r.s.pinsOn(ctx, obj.ref); err != nil {
				return err
			}
		}

		return r.consider(ctx, obj, found)
	})
	if err != nil {
		return fmt.Errorf("sweeping the %s artifacts of bucket %s: %w", kind, r.s.bucket, err)
	}

	strays, err := pins.drain(ctx)
	r.stats.ForeignObjects += strays

	return err
}

// consider decides one artifact's fate and acts on it.
func (r *sweepRun) consider(ctx context.Context, obj artifactObject, pins artifactPins) error {
	r.stats.ArtifactsScanned++
	r.stats.BytesScanned += obj.size
	r.stats.ForeignObjects += pins.strays

	fate, err := pins.fate(ctx, r.now, r.pinIsGone)
	if err != nil {
		return err
	}

	switch fate {
	case artifactNamed:
		r.reapTakes(ctx, pins)

		return nil

	case artifactTaken:
		r.stats.ArtifactsProtected++

		return nil
	}

	keep, err := r.keepUnreferenced(ctx, obj, pins)
	if err != nil {
		return err
	}

	if keep {
		r.stats.ArtifactsProtected++

		return nil
	}

	r.collect(ctx, obj, pins)

	return nil
}

// keepUnreferenced is the two reasons an object nothing references is kept
// anyway: it is something a rebuild can restore, or it is too recently written
// to be called garbage.
func (r *sweepRun) keepUnreferenced(ctx context.Context, obj artifactObject, pins artifactPins) (bool, error) {
	if r.isRebuildCandidate(obj) {
		r.stats.ResultsWithoutEntry++

		return true, nil
	}

	return r.tooYoungToCall(ctx, obj, pins)
}

// isRebuildCandidate reports whether an unreferenced object is a result
// document, which this sweep never collects.
//
// A document decodes to a model.Result carrying its scan ID, target and mode, so
// one the index does not point at is something a rebuild can restore rather than
// garbage — and AC6's "not silently lost" is unconditional, where a grace period
// would protect it for a day and then delete it. Screenshots and stored bodies
// are not self-describing and keep the ordinary grace-based collection.
//
// The override is what lets an operator sweep a bucket whose index really is
// gone. It is not a loophole in the rule: it is the operator stating that there
// is no index to rebuild, which is the premise the rule rests on.
func (r *sweepRun) isRebuildCandidate(obj artifactObject) bool {
	return !r.opts.AllowEmptyIndex && strings.HasPrefix(obj.ref, artifactKindResult+refSeparator)
}

// tooYoungToCall reports whether an unreferenced object is too recently written
// to be called garbage. There are two ways it can be, and they are dated
// against two different clocks because they are two different writes in flight.
//
// **The object's own age**, when no pin has ever covered it. That is what the
// first half of an interrupted write looks like from out here: artifacts are
// written to the bucket before the index that names them, deliberately
// (Story 8.2, AC4).
//
// **The newest pin's age**, when every pin on it reads as gone — which is the
// only way fate reaches this line. A pin whose owner is absent is either a
// write that has not finished, since both PutResult and an approval write their
// pins before the object that commits them, or the leftover of a prune whose
// deletes the bucket refused. Without a grace there the window is not a window
// at all: the very next sweep collects the screenshots and stored bodies of a
// scan interrupted a second ago, and Story 8.11's rebuild — which the sweep
// preserves that scan's document for — would restore an entry naming evidence
// this pass had destroyed (AC6).
//
// The take marker cannot stand in for that, and liveTake says why: it is
// written before the pin, so the pin releases it.
//
// Age alone cannot stand in for it either, which is why inFlight is asked: a
// prune's leftover pin is exactly as young as an interrupted write's, so a
// grace on age would keep every artifact a prune failed to delete for a day and
// make "the next sweep collects it" false (Story 8.5, AC4).
//
// An object that carries a pin whose owner is still there never reaches here at
// all: the pin decided it, and its own age says nothing about who needs it —
// the same split SQL makes between its reference-side pass, where no age is
// consulted, and its walk of the bucket, where it is.
func (r *sweepRun) tooYoungToCall(ctx context.Context, obj artifactObject, pins artifactPins) (bool, error) {
	cutoff := r.now.Add(-unreferencedArtifactGrace)

	// newestRef is the zero time when there are no pins at all, which is before
	// every cutoff and so falls through to the object's own age.
	if !pins.newestRef().Before(cutoff) {
		return r.inFlight(ctx, pins)
	}

	return len(pins.refs) == 0 && !obj.modTime.Before(cutoff), nil
}

// inFlight reports whether the newest pin on an object nothing references any
// more belongs to a write that has not finished.
//
// It is the newest pin and not all of them because that is the one the question
// is about: an older pin whose owner has gone has already outlived any write it
// could have belonged to, and the newest is what the age test above admitted.
//
// A **decision** pin with no decision is always a write in flight, because
// nothing deletes a decision object — not retention, not compaction, not a
// withdrawal — so the only way to hold one whose decision the index does not
// have is to be between pinDecisionEvidence and the object that commits it.
//
// A **result** pin is told apart by the tombstone, which is the same rule
// collectOrphanedScanKeys applies to a scan-ID object and for the same reason:
// a tombstone is a prune saying it meant this result to be gone, and its
// absence is a PutResult that has not reached its own commit point. That makes
// the answer a fact the index holds rather than a guess from a clock, so a
// prune's leftover is collected on the very next sweep and a live write is not.
func (r *sweepRun) inFlight(ctx context.Context, pins artifactPins) (bool, error) {
	newest, ok := pins.newestPin()
	if !ok {
		return false, nil
	}

	if newest.owner.kind != ownerResult {
		return true, nil
	}

	pruned, err := r.prunedAway(ctx, newest.owner)

	return !pruned, err
}

// prunedAway reports whether a tombstone says the result one pin names was
// removed on purpose.
//
// The tombstones of one series, and never its whole history: what is wanted is
// a handful of zero-byte keys, where the directory they sit in holds an entry
// per scan the target has ever had.
//
// A tombstone a compaction has collected and absorbed into a checkpoint is not
// read, deliberately. That only happens after tombstoneGrace — a week — so a
// pin young enough to have reached this question cannot be naming a scan whose
// tombstone is that old, and reading every checkpoint of the series to find out
// would be a GET per checkpoint on a path that exists to be cheap.
func (r *sweepRun) prunedAway(ctx context.Context, owner refOwner) (bool, error) {
	pruned, err := r.tombstonedScans(ctx, owner.series)
	if err != nil {
		return false, err
	}

	_, gone := pruned[owner.scan]

	return gone, nil
}

// tombstonedScans is every scan one series' tombstones name, read once per
// series per sweep.
//
// Memoised because the artifacts are walked in digest order and not in series
// order, so two leftovers of one prune arrive nowhere near each other; without
// it a bucket where one prune failed to delete a thousand objects would list
// the same directory a thousand times. What it holds is small — a tombstone is
// collected once nothing can bring its entry back — and it is only ever
// populated for a series that actually has a pin with no owner.
func (r *sweepRun) tombstonedScans(ctx context.Context, id seriesID) (map[encodedScan]struct{}, error) {
	if found, ok := r.tombstoned[id]; ok {
		return found, nil
	}

	out := make(map[encodedScan]struct{})

	var cursor indexCursor

	for cursor.more() {
		page, err := r.s.listIndex(ctx, id.tombstoneDirPrefix(), cursor, indexListPageSize)
		if err != nil {
			return nil, fmt.Errorf("reading the prunes recorded under %s: %w", id.tombstoneDirPrefix(), err)
		}

		cursor = page.next

		for _, object := range page.objects {
			record, err := parseSeriesRecord(object.key)
			if err != nil || record.kind != seriesTombstone {
				continue
			}

			out[record.scan] = struct{}{}
		}
	}

	r.tombstoned[id] = out

	return out, nil
}

// collect removes one artifact and the keys that pointed at it.
func (r *sweepRun) collect(ctx context.Context, obj artifactObject, pins artifactPins) {
	if r.plan {
		r.stats.Artifacts = append(r.stats.Artifacts, obj.ref)
		r.stats.ArtifactsDeleted++
		// The listing already reported the size, so a plan needs no request of
		// its own.
		r.stats.BytesFreed += obj.size

		return
	}

	if !dropArtifact(ctx, r.s.bucket, r.s.log, obj.ref) {
		r.stats.ArtifactsFailed++

		return
	}

	r.stats.ArtifactsDeleted++
	r.stats.BytesFreed += obj.size

	// Every pin on it now, not only the stale ones: the object is gone, so
	// nothing that pointed at it can be needed by any reader.
	for _, pin := range pins.refs {
		if err := r.s.dropIndex(ctx, pin.key); err != nil {
			r.s.log.Warn("a pin on a collected artifact could not be removed", "key", pin.key, "error", err)
		}
	}

	for _, take := range pins.takes {
		if err := r.s.dropIndex(ctx, take.key); err != nil {
			r.s.log.Debug("a take marker on a collected artifact could not be removed",
				"key", take.key, "error", err)
		}
	}
}

// reapTakes removes the take markers of an artifact that is staying, once they
// can no longer protect anything.
//
// Without it a bucket accumulates one take per capture for ever, and the pin
// directory a prune lists to answer one question grows with the history rather
// than with the references. It is the same work forgetStaleClaims does for a
// SQL store, and it rides along with the sweep for the same reason: the keys are
// already in hand.
func (r *sweepRun) reapTakes(ctx context.Context, pins artifactPins) {
	if r.plan {
		return
	}

	for _, take := range pins.staleTakes(r.now) {
		if err := r.s.dropIndex(ctx, take.key); err != nil {
			r.s.log.Debug("a take marker that protects nothing any more could not be removed",
				"key", take.key, "error", err)
		}
	}
}

// pinIsGone reports whether the thing one pin names has gone.
//
// This is SQL's sweepDangling, expressed against keys: a result pin whose
// scan-ID object is absent, and a decision pin whose decision the index does not
// hold, are the leftovers of an interrupted write or of a prune whose deletes
// the bucket refused.
//
// The result pin costs one attribute read, and the sweep pays it at most once
// per artifact because fate stops at the first pin that survives. The decision
// pin costs nothing: the digests were read once, at the start of the run.
func (r *sweepRun) pinIsGone(ctx context.Context, pin artifactPin) (bool, error) {
	if pin.owner.kind == ownerDecision {
		_, held := r.decisions[pin.owner.did]

		return !held, nil
	}

	key := pin.owner.series.byIDKey(pin.owner.scan)

	if _, err := r.s.statIndex(ctx, key); err != nil {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}

		return false, fmt.Errorf("checking whether the scan a pin names is still stored: %w", err)
	}

	return false, nil
}

// --- prune leftovers the sweep tidies up -----------------------------------

// collectOrphanedScanKeys deletes the scan-ID objects of results a prune
// removed and could not finish removing (§7.4, and PruneStats.IndexKeysFailed).
//
// The rule is narrow on purpose. A scan-ID object is deleted only when its
// series shows a tombstone for that scan and no entry: the tombstone is a prune
// saying it meant this result to be gone, which makes the key unambiguously a
// leftover and needs no grace period to be sure of. A scan-ID object with no
// entry and *no* tombstone is the opposite case — an interrupted PutResult,
// which writes the scan-ID object before the entry — and that is a rebuild
// candidate rather than garbage, so it is left alone (AC6).
//
// The walk is over the scan-ID prefix itself rather than over the series
// markers, because a prune that removed a whole history removes the marker with
// it: a leftover of the very prune this pass exists to finish would otherwise be
// the one thing it could not reach. The keys arrive grouped by series — a
// listing of byid/ descends one target directory at a time — so the history each
// of them is judged against is read once per series and not once per key.
func (r *sweepRun) collectOrphanedScanKeys(ctx context.Context) error {
	if r.plan {
		return nil
	}

	walk := scanKeySweep{run: r}

	var cursor indexCursor

	for cursor.more() {
		page, err := r.s.listIndex(ctx, indexByIDPrefix, cursor, indexListPageSize)
		if err != nil {
			return fmt.Errorf("listing the scans of %s by ID: %w", r.s.bucket, err)
		}

		cursor = page.next

		for _, object := range page.objects {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("collecting the scan-ID objects of pruned results: %w", err)
			}

			if err := walk.consider(ctx, object); err != nil {
				return err
			}
		}
	}

	return nil
}

// scanKeySweep is the walk over the scan-ID objects, carrying the one series'
// history the key in front of it has to be judged against.
type scanKeySweep struct {
	run *sweepRun

	series seriesID
	loaded bool

	// live and tombstoned are the scans that series still has an entry for and
	// the ones its tombstones say have been pruned.
	live       map[encodedScan]struct{}
	tombstoned map[encodedScan]struct{}

	// decidable is false for a series that holds a compaction checkpoint; see
	// load.
	decidable bool
}

// splitByIDKey reads the series and the scan out of a scan-ID key, reporting a
// key this grammar does not produce as a fact rather than as a failure.
//
// A sweep that stopped at the first stray object inside the index tree would
// never reach the leftovers it exists to collect, and it may not delete the
// stray either: a sweep collects only what wsaw wrote (Story 8.5, AC4).
func splitByIDKey(key string) (seriesID, encodedScan, bool) {
	id, scan, err := parseByIDKey(key)

	return id, scan, err == nil
}

// consider decides one scan-ID object.
func (w *scanKeySweep) consider(ctx context.Context, object indexObject) error {
	id, scan, ok := splitByIDKey(object.key)
	if !ok {
		w.run.foreign(object.key)

		return nil
	}

	if !w.loaded || id != w.series {
		if err := w.load(ctx, id); err != nil {
			return err
		}
	}

	if !w.decidable {
		return nil
	}

	if _, held := w.live[scan]; held {
		return nil
	}

	if _, pruned := w.tombstoned[scan]; !pruned {
		return nil
	}

	w.run.forgetScanKey(ctx, object.key)

	return nil
}

// load reads which scans one series still has an entry for and which its
// tombstones say have been pruned, from one listing of the series directory and
// a read of each checkpoint that listing showed.
//
// A series holding a checkpoint used to be skipped entirely, on the grounds
// that nothing wrote one. Compaction does now, so it is read instead: one GET
// per checkpoint per series — a dozen for a series of ten thousand scans — and
// what it is for is the tombstones a checkpoint has absorbed, which are the
// only remaining record that those scans were pruned once compaction has
// collected the tombstone objects themselves. See loadCheckpoint for why the
// entries it carries are deliberately not read into the live set.
func (w *scanKeySweep) load(ctx context.Context, id seriesID) error {
	w.series, w.loaded, w.decidable = id, true, true
	w.live, w.tombstoned = make(map[encodedScan]struct{}), make(map[encodedScan]struct{})

	var cursor indexCursor

	for cursor.more() {
		page, err := w.run.s.listIndex(ctx, id.dirPrefix(), cursor, indexListPageSize)
		if err != nil {
			return fmt.Errorf("reading the history under %s: %w", id.dirPrefix(), err)
		}

		cursor = page.next

		for _, object := range page.objects {
			record, err := parseSeriesRecord(object.key)
			if err != nil {
				continue
			}

			switch record.kind {
			case seriesCheckpoint:
				if err := w.loadCheckpoint(ctx, object.key); err != nil {
					return err
				}
			case seriesTombstone:
				w.tombstoned[record.scan] = struct{}{}
			case seriesEntry:
				w.live[record.scan] = struct{}{}
			}
		}
	}

	return nil
}

// loadCheckpoint adds the prunes one checkpoint has absorbed to the walk's
// tombstoned set.
//
// Only the tombstones, and the asymmetry is worth stating because the entries
// look as though they belong here too. What this walk collects is a scan-ID
// object whose scan a tombstone says was pruned, so a scan that is merely
// inside a checkpoint is already safe: it has no tombstone, and the rule keeps
// what it cannot show was removed. What is *not* safe without this read is the
// other end of §7.2's tombstone collection — once a checkpoint carries a
// pruned scan in its Tombstoned array the tombstone object itself is collected,
// and from then on the checkpoint is the only thing that says the scan was ever
// pruned. Without opening it the sweep would keep that leftover for the life of
// the bucket.
//
// A checkpoint that cannot be read makes the series undecidable rather than
// failing the sweep. Refusing to tidy one series is a leftover left for the
// next run; guessing is a scan-ID object deleted for a result that is still in
// the history.
func (w *scanKeySweep) loadCheckpoint(ctx context.Context, key string) error {
	body, err := w.run.s.readCheckpoint(ctx, key)
	if err != nil {
		w.decidable = false

		w.run.s.log.Warn("a checkpoint could not be read, so the scan-ID objects of that series were left alone",
			"key", key, "error", err)

		return nil
	}

	for _, scan := range body.Tombstoned {
		w.tombstoned[scan] = struct{}{}
	}

	return nil
}

// forgetScanKey removes one leftover scan-ID object.
func (r *sweepRun) forgetScanKey(ctx context.Context, key string) {
	if err := r.s.dropIndex(ctx, key); err != nil {
		r.s.log.Warn("the scan-ID object of a pruned result could not be removed",
			"key", key, "error", err)

		return
	}

	r.s.log.Info("the scan-ID object a prune left behind was removed", "key", key)
}

// --- the pin side of the merge join ----------------------------------------

// pinStream is the pin side of the sweep's merge join: one listing of
// "ref/<kind>/", advanced to meet each artifact the object walk reaches.
//
// It holds one page at a time and never the whole prefix, which is what makes
// the join constant in memory over a bucket with twelve million pins in it.
type pinStream struct {
	s      *Blob
	prefix string
	cursor indexCursor
	page   []indexObject
	at     int

	// passed is the last artifact the stream was asked about, so a listing that
	// does not come back in key order is noticed rather than silently answering
	// "nothing pins it".
	passed string
}

// peek returns the key the stream is on, fetching a page when it has to.
func (st *pinStream) peek(ctx context.Context) (indexObject, bool, error) {
	for st.at >= len(st.page) {
		if !st.cursor.more() {
			return indexObject{}, false, nil
		}

		page, err := st.s.listIndex(ctx, st.prefix, st.cursor, indexListPageSize)
		if err != nil {
			return indexObject{}, false, fmt.Errorf("listing the pins under %s: %w", st.prefix, err)
		}

		st.cursor, st.page, st.at = page.next, page.objects, 0
	}

	return st.page[st.at], true, nil
}

// upTo advances the stream to artifact and returns the pins it holds.
//
// ordered is false when the two listings did not come back in the same key
// order. §4.2 guarantees they do within one kind, and both sides of this join
// hold only fixed-width digests below the kind, so no name can be a prefix of a
// sibling — but a provider nobody has tested is not something to bet evidence
// on. The pins of an artifact the stream has already passed have been skipped,
// so the honest answer is "ask again" rather than "nothing pins it": the second
// would be a sweep deleting live evidence because a listing arrived in the wrong
// order.
func (st *pinStream) upTo(ctx context.Context, artifact string) (pins artifactPins, ordered bool, err error) {
	if artifact < st.passed {
		return artifactPins{}, false, nil
	}

	st.passed = artifact

	for {
		object, ok, err := st.peek(ctx)
		if err != nil || !ok {
			return pins, true, err
		}

		marker, err := parseRefMarkerKey(object.key)
		if err != nil {
			pins.strays++
			st.at++

			continue
		}

		if marker.artifact > artifact {
			return pins, true, nil
		}

		if marker.artifact == artifact {
			pins.add(marker, object)
		}

		st.at++
	}
}

// drain reads out what is left of the stream, reporting the keys in it that this
// grammar does not produce.
//
// What it skips over is a pin on an artifact the bucket does not hold. Those are
// left alone deliberately: a pin whose object is absent is either a prune that
// could not finish or a listing that has not caught up with a write, and the
// second is a state this store is designed to survive rather than to tidy away
// (AC6).
func (st *pinStream) drain(ctx context.Context) (int, error) {
	var strays int

	for {
		object, ok, err := st.peek(ctx)
		if err != nil || !ok {
			return strays, err
		}

		if _, err := parseRefMarkerKey(object.key); err != nil {
			strays++
		}

		st.at++
	}
}
