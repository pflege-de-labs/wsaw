package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file is where the bucket index keeps the decisions: which scan is the
// approved baseline for a series, and the audit log of who decided what
// (Story 8.10, AC8 and AC9).
//
// Nothing here is edited and nothing here is deleted. Approving appends a
// decision object, changing the approval appends another, withdrawing appends a
// revocation, and the current baseline is the fold over the decisions one
// listing of the series' prefix shows. That is AC8 literally: a compliance
// decision whose reversal leaves no trace is not an auditable decision, and
// last-writer-wins on a mutated object is exactly the failure mode where the
// loser leaves none.
//
// The cross-row guarantee is the other half. Story 4.6 AC7 requires that
// approving a baseline and recording its audit entry either both happen or
// neither does, and a bucket has no transaction to get that from. So the two
// are not two writes: the decision object carries the approval and the audit
// entry it explains, in one body, under one key, created in one request. A
// partial write leaves neither (AC9).
//
// What the audit prefix then holds for a decision is a *derived pointer* — a
// copy of the entry, filed where the log is read from, at a key that is a pure
// function of the decision object. The arrow points from the audit log to
// nothing: compaction may fold and delete audit keys freely without ever
// endangering a baseline, and anything holding a decision can put its pointer
// back without a listing and without a second source of truth.

const (
	// auditNonceBytes is the width of the value that keeps two identical audit
	// entries two entries.
	//
	// Sixty-four bits, which is not an identity and does not need to be: the
	// nonce is not looked up, compared or parsed, it only has to make a
	// collision between two entries recorded in one clock tick not happen. Two
	// entries would have to draw the same eight bytes *and* share a nanosecond
	// *and* be otherwise byte-identical for one to shadow the other.
	auditNonceBytes = 8

	// auditInstantProbes is how many nanoseconds RecordAudit may step forward
	// looking for one no entry is already filed at.
	//
	// The step exists because a bucket has no insertion order. SQL breaks a tie
	// between two entries recorded at one instant with the row identity, which
	// is monotonic and is the order they were written in; here the only thing
	// that orders two keys is what is in them, so the ordering field has to
	// carry the tie-break or the log would come back in digest order — stable,
	// but unrelated to the sequence of events an audit log exists to record.
	//
	// Sixty-four, because entries that genuinely share a nanosecond come in
	// twos and threes — an action and the note about it, a caller back-filling
	// a decision with one timestamp — and sixty-four of them is already not a
	// sequence. Past it the entry is filed anyway, beside the others, with a
	// warning: mis-ordering an audit record is bad and refusing to record one
	// is worse.
	auditInstantProbes = 64

	// auditHealProbes is how many of a series' earlier decisions one new
	// decision checks for a missing audit pointer.
	//
	// It is a bound rather than "the page readDecisions already has", which is
	// what an earlier version of healAuditPointers used, and the difference
	// matters to an operator: every entry checked is one attribute read, they
	// are serial, and a target that has been re-approved after every CMP change
	// for two years holds hundreds of decisions. Unbounded, the healthy case
	// paid a HEAD request per decision ever taken — hundreds of billed requests
	// and seconds of latency on an interactive approval — to re-establish
	// something that was already true.
	//
	// Eight, because of where the gap can be. A pointer is missing only when a
	// process died between the decision object and its pointer, so the gap is at
	// the *head* of the series when the next decision of that series runs, and
	// one probe would find it; eight covers a run of consecutive interruptions
	// and the case where the repair itself failed a few decisions ago. A gap
	// older than that is not repaired by a write at all — Story 8.11's --verify
	// is what finds it, and the decision itself was never at risk, because the
	// audit entry is inside the decision object (see healAuditPointers).
	auditHealProbes = 8
)

// --- the decision object ---------------------------------------------------

// decisionBody is one baseline decision, whole: what was decided, the audit
// entry that records it, and — for an approval — the approved scan by value
// together with the artifacts that copy names.
//
// **The approved result is embedded and not referenced**, exactly as the SQL
// stores embed it, and the comment on Baseline.Result gives the reason: a
// retention prune of old history must not silently invalidate what "expected"
// means. Referencing the document instead would give this store a GetBaseline
// that can answer ErrEvidenceGone, which scanner.compare turns into an evidence
// finding on every subsequent scan of that target — a visible difference in
// compliance output between store kinds, which is precisely what AC2 exists to
// exclude. Approvals are human-generated and rare; the duplicated bytes are the
// price of parity.
//
// Refs is what the copy names, recorded here so that pinning its evidence costs
// retention no read of the body. It is the same set PutResult pinned for the
// scan itself, which is the exact analogue of the SQL stores' liveOwners union:
// a baseline names its scan's artifacts, so those artifacts survive the expiry
// of the history around them.
//
// There is deliberately **no nonce**, and §6.5's draft called for one. Two
// distinct decisions produce distinct bodies and therefore distinct digests, so
// nothing collapses that should not; and the one case a nonce would change is
// two writers committing the *identical* decision at the identical instant,
// where one object is the right answer and two would be one approval recorded
// twice. The monotonic bump in decisionInstant is what keeps two decisions of
// one series off one instant, and it does it without giving up AC10's "the same
// fact writes the same bytes to the same key".
type decisionBody struct {
	Layout   int        `json:"layout"`
	Op       baselineOp `json:"op"`
	Audit    AuditEntry `json:"audit"`
	Baseline *Baseline  `json:"baseline,omitempty"`
	Refs     []string   `json:"refs,omitempty"`
}

// decision is one decision reduced to the key and the bytes that will record
// it.
//
// It is computed once, before the first write, for the reason indexedResult is:
// every object this write produces has to be derivable from the same body, so
// that an interrupted call and the call that replaces it cannot spell one
// decision two ways (AC10, I4).
type decision struct {
	series blobSeries
	key    decisionKey
	audit  AuditEntry
	refs   []string
	body   []byte
}

// newDecision derives the key and the one body a baseline decision needs.
//
// The instant is taken from the audit entry rather than passed beside it, so
// that the time the key orders by and the time the log reports can never be two
// different times.
func newDecision(series blobSeries, op baselineOp, e AuditEntry, b *Baseline, refs []string) (decision, error) {
	body, err := json.Marshal(decisionBody{
		Layout:   indexLayoutVersion,
		Op:       op,
		Audit:    e,
		Baseline: b,
		Refs:     refs,
	})
	if err != nil {
		return decision{}, fmt.Errorf("building the baseline decision for %s: %w", series, err)
	}

	// The key carries the digest of the body, which is what makes two writers
	// of one decision write one object and what lets a reader check that the
	// object under a key is the decision the key names.
	did := shortSum(body, identityHexLen)

	return decision{
		series: series,
		key:    series.id.decision(e.At, op, did),
		audit:  e,
		refs:   refs,
		body:   body,
	}, nil
}

// --- the writes ------------------------------------------------------------

// SetBaseline approves one scan as the expected state of a target.
//
// The order is the one §6.5 argues for, and every step of it is chosen for what
// an interruption immediately after it leaves behind:
//
//  1. read the scan and refuse one that is not an observation of the site.
//     Approving a failed scan would pin a broken observation as the definition
//     of "correct" and make every later comparison meaningless. Nothing has
//     been written, so a refusal here leaves neither an approval nor an audit
//     entry — which is what TestBaselineApprovalAndAuditAreAtomic asks.
//  2. list the decisions of this series. One listing gives the decision that
//     stands, the newest instant the bump has to clear, and the keys the
//     audit-pointer self-heal covers.
//  3. derive the instant, the body and the key — all of it once, above any
//     retry, so that a replay lands the identical bytes at the identical key.
//  4. pin the evidence the copy names, before the decision that names it is
//     visible, so a live approval never points at artifacts nothing has
//     claimed. Pins whose decision never arrived are the sweep's dangling-owner
//     case, the same one an interrupted PutResult leaves.
//  5. write the decision object. **This is the commit point**: the approval and
//     its audit entry are now recorded, together, in one object.
//  6. write the derived audit pointer, and re-issue any pointer missing for the
//     decisions step 2 showed. Neither can fail the call: the approval is
//     already committed, and reporting a derived write as a failed approval is
//     how one human decision becomes two (see putAuditPointer).
//
// The two directions do not fail symmetrically, and only one of them needs
// watching. An approval that did not land — or that landed and lost the fold to
// a concurrent one — leaves the previous state standing and silences nothing,
// which is why nothing here reads the log back. A withdrawal that landed and
// lost the fold leaves findings silenced against a baseline the operator was
// told was gone, so DeleteBaseline does read it back; verifyDecision is that
// check and argues it.
func (s *Blob) SetBaseline(
	target string, mode model.ConsentMode, scanID, approvedBy, note string,
) (*Baseline, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	entry, res, err := s.resultByID(ctx, series, scanID)
	if err != nil {
		return nil, err
	}

	if !res.OK() {
		return nil, fmt.Errorf("scan %s did not produce a trustworthy asset list (%s) and cannot be a baseline",
			scanID, res.Termination)
	}

	prior, err := s.readDecisions(ctx, series)
	if err != nil {
		return nil, fmt.Errorf("approving a baseline for %s: %w", series, err)
	}

	at := s.decisionInstant(series, prior)

	b := &Baseline{
		Target:      target,
		ConsentMode: mode,
		ScanID:      scanID,
		ApprovedAt:  at,
		ApprovedBy:  approvedBy,
		Note:        note,
		Result:      res,
	}

	d, err := newDecision(series, opApprove, AuditEntry{
		At:      at,
		Actor:   approvedBy,
		Action:  auditBaselineApproved,
		Target:  target,
		Mode:    mode,
		Subject: scanID,
		Note:    note,
	}, b, artifactRefsOf(res, entry.Document.Ref))
	if err != nil {
		return nil, err
	}

	if err := s.appendDecision(ctx, d, prior); err != nil {
		return nil, fmt.Errorf("approving a baseline for %s: %w", series, err)
	}

	return b, nil
}

// DeleteBaseline withdraws an approval.
//
// It appends a revocation rather than deleting the approval, which is AC8's
// whole point: the decision to stop trusting a state is itself a compliance
// decision, and a store where withdrawing an approval erased the approval could
// not show a reviewer what was trusted last month.
//
// It is appended whether or not anything is approved, because the SQL stores
// record the action whether or not a row was deleted, and an audit log that
// dropped the withdrawals that turned out to be redundant would be a log that
// edits itself.
//
// It reports a withdrawal that was recorded and did not take effect, which a
// lagging listing and a clock a second behind can produce between them; see
// verifyDecision. The error is the one thing that separates this from the
// failure it is guarding against — the withdrawal is in the log either way, and
// what a caller may not be told is that the baseline is gone when it is not.
func (s *Blob) DeleteBaseline(target string, mode model.ConsentMode, actor string) error {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	prior, err := s.readDecisions(ctx, series)
	if err != nil {
		return fmt.Errorf("withdrawing the baseline for %s: %w", series, err)
	}

	at := s.decisionInstant(series, prior)

	// No copy and no pins: a revocation approves nothing, so there is nothing
	// for it to keep alive. That is the other half of §8.3's baseline-evidence
	// test — once the approval is withdrawn, the last pin on the approved
	// scan's screenshots is gone and retention may collect them.
	d, err := newDecision(series, opRevoke, AuditEntry{
		At:     at,
		Actor:  actor,
		Action: auditBaselineDeleted,
		Target: target,
		Mode:   mode,
	}, nil, nil)
	if err != nil {
		return err
	}

	if err := s.appendDecision(ctx, d, prior); err != nil {
		return fmt.Errorf("withdrawing the baseline for %s: %w", series, err)
	}

	return nil
}

// decisionInstant is when a new decision of this series happened: now, or one
// nanosecond after the newest decision already recorded, whichever is later.
//
// The bump is what keeps the fold unambiguous. Without it, a revocation and the
// re-approval that follows it can land on one instant — certain under a pinned
// clock, and possible on any host whose timer has coarse resolution — and the
// fold would then be choosing between "approved" and "withdrawn" by digest,
// which is deterministic and meaningless. With it, the decisions of one series
// are strictly ordered by the order they were taken in, which is the order a
// reviewer means when they ask what the baseline is.
//
// It moves the recorded time, not only the key: ApprovedAt and the audit
// entry's At are this value. A nanosecond of drift is the honest price of a
// total order, and the alternative — a key that says one instant and a body
// that says another — would put the two out of step for every reader that
// compares them.
//
// It is per series and never global. The decisions of one target are ordered
// against each other; nothing about them says anything about another target's,
// and a global monotonic clock would let one busy series push another's
// approvals into the future.
// It bumps against the *ordering field* and not against the two instants,
// which is not the same comparison past the edges of the range. clampNano pins
// every instant beyond indexHorizon to one field, so on a host reading year
// 2300 a withdrawal is later than the approval it answers in wall-clock terms
// and lands at the identical position in the log — and comparing the times
// would skip the bump and file the two on top of each other with nothing said.
// Comparing the fields makes the bump fire wherever the fold would see a tie,
// and where the grammar has genuinely run out it says so.
//
// It cannot make a decision the fold could not see: the bump is against what
// one listing showed. currentDecision's tie-break is what covers the rest, and
// verifyDecision is what covers a withdrawal that lost anyway.
func (s *Blob) decisionInstant(series blobSeries, prior []decisionKey) time.Time {
	at := s.now().UTC()

	if len(prior) > 0 {
		// prior is newest first and the ordering field is inverted, so the
		// first key is the newest instant this series has recorded, and a field
		// that is not strictly smaller than it is not strictly newer.
		if inv(at) >= prior[0].inv {
			at = instantAt(prior[0].inv).Add(time.Nanosecond)
		}
	}

	if _, clamped := clampNano(at); clamped {
		// Clamped and never dropped, as a scan's start time is: a decision
		// taken against a wrong clock is still a decision, and refusing to
		// record it would lose a compliance fact to a mistake made elsewhere.
		s.log.Warn("a baseline decision's instant is outside the range the bucket index can order, and was clamped to the edge",
			"target", series.target, "consent_mode", string(series.mode), "at", at)
	}

	if len(prior) > 0 && inv(at) == prior[0].inv {
		// The bump ran out: the field is at an edge of the range and there is
		// no position after it. Reported at Warn because the total order this
		// series' decisions are read in has degraded to currentDecision's
		// tie-break, which is safe and is not an order.
		s.log.Warn("a baseline decision could not be ordered after the one before it, "+
			"because the clock that took it is outside the range the bucket index can order",
			"target", series.target, "consent_mode", string(series.mode), "at", at)
	}

	return at
}

// appendDecision writes the objects one decision produces, in the order
// SetBaseline's comment argues for.
//
// Only the first two steps can fail the call, and that is the whole point of
// the order. The pins have to be there before the decision that names them is
// visible; the decision object is the commit point, and after it the approval
// or the withdrawal is recorded whatever else happens. Everything below it is
// derived — a pointer at a fact the store already holds in full — so a failure
// there is a warning about the log being briefly behind and never a report that
// the decision did not happen. See putAuditPointer for what turning it into an
// error costs: an operator who retries a committed approval records the same
// human decision twice.
func (s *Blob) appendDecision(ctx context.Context, d decision, prior []decisionKey) error {
	if err := s.pinDecisionEvidence(ctx, d); err != nil {
		return err
	}

	// The commit point.
	if _, err := s.putIndex(ctx, d.key.String(), d.body); err != nil {
		return err
	}

	if err := s.putAuditPointer(ctx, d.key, d.audit); err != nil {
		s.log.Warn("a baseline decision is recorded and its audit entry is not, "+
			"and will be re-created by the next decision of this series or by a rebuild",
			"key", d.key.String(), "target", d.series.target,
			"consent_mode", string(d.series.mode), "error", err)
	}

	s.healAuditPointers(ctx, d.series, prior)

	return s.verifyDecision(ctx, d)
}

// verifyDecision reads the log back and reports a withdrawal that did not take
// effect.
//
// The bump in decisionInstant orders a new decision past the newest one the
// caller could *see*, and against a provider whose listings lag that is not
// necessarily the newest one there is. When the pre-write listing missed an
// approval and the host taking the withdrawal has a clock behind the host that
// took it, the withdrawal is filed *before* the approval it answers and loses
// the fold.
//
// Which direction that fails in is what decides whether it is reported. An
// approval that lost is the previous state standing: nothing is silenced, the
// decision is in the log, and a later approval superseding it is ordinary
// concurrency rather than a failure — so SetBaseline does not pay for this.
// A withdrawal that lost leaves findings silenced against a baseline the
// operator was told was gone, with nothing anywhere saying so, which is the
// failure AC8 names as unacceptable.
//
// One listing, after the commit, and the decision just written is folded in
// rather than looked for: a post-write listing need not show our own key any
// more than the pre-write one showed theirs, and treating an invisible own
// write as a loss would report a failure on every lagging provider.
//
// It is a report and not a repair. The withdrawal stays exactly where it is —
// nothing here deletes a decision — and the caller retries, which against a
// listing that has by then caught up bumps past the approval and takes effect.
// What it cannot promise is that the post-write listing is fresh either: an
// approval neither listing showed is still lost, and that residue is the price
// of a store with no transaction. It is a strict improvement on being told the
// withdrawal succeeded.
func (s *Blob) verifyDecision(ctx context.Context, d decision) error {
	if d.key.op != opRevoke {
		return nil
	}

	after, err := s.readDecisions(ctx, d.series)
	if err != nil {
		return err
	}

	// Newest first, and our own key belongs wherever the fold would put it.
	after = append(after, d.key)
	slices.SortFunc(after, func(a, b decisionKey) int { return strings.Compare(a.inv, b.inv) })

	current, ok := currentDecision(after)
	if !ok || current == d.key {
		return nil
	}

	return fmt.Errorf(
		"the withdrawal %s is recorded and did not take effect, because %s is ordered after it "+
			"and was not visible when it was taken; retrying withdraws the baseline: %w",
		d.key, current, ErrIndexIncomplete,
	)
}

// pinDecisionEvidence writes the reverse index that keeps an approved scan's
// evidence alive for as long as the approval stands.
//
// One zero-byte object per artifact, named for the artifact and for this
// decision, exactly as a stored result pins its own. Retention deletes an
// artifact by reference and never by age, so the pin is the whole of why
// pruning the history around a baseline does not delete the screenshots the
// baseline's copy points at.
func (s *Blob) pinDecisionEvidence(ctx context.Context, d decision) error {
	owner := decisionRefOwner(d.key.did)

	for _, artifact := range d.refs {
		marker, err := newRefMarker(artifact, owner)
		if err != nil {
			// artifactRefsOf already dropped every reference that is not one
			// this store wrote, so this is unreachable from a stored document.
			// It is checked anyway because the alternative is splicing an
			// unvalidated reference into a key (Tenet 9).
			return err
		}

		if _, err := s.putIndex(ctx, marker.String(), nil); err != nil {
			return err
		}
	}

	return nil
}

// putAuditPointer files one decision's audit entry where the log is read from.
//
// A failure here is returned and the message says what state the store is in:
// the decision is committed and stands, and its entry will appear in the log
// the next time a decision of this series heals it. **It is never the call's
// failure**, and appendDecision logs it rather than passing it on, because the
// two are not distinguishable to a caller and the difference matters: an
// operator told an approval failed approves again, readDecisions now shows the
// first one, and one human decision becomes two approvals in a compliance log —
// while the first was in force the whole time. What went wrong is that the log
// is briefly behind, and the log heals itself (healAuditPointers).
func (s *Blob) putAuditPointer(ctx context.Context, key decisionKey, e AuditEntry) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("building the audit entry for %s: %w", key, err)
	}

	if _, err := s.putIndex(ctx, key.auditPointer().String(), body); err != nil {
		return fmt.Errorf("the decision %s is recorded and its audit entry is not: %w", key, err)
	}

	return nil
}

// healAuditPointers re-creates the audit entries of earlier decisions of this
// series that have none.
//
// This is the self-heal AC9 asks for where one write cannot carry everything:
// the decision object is authoritative and complete, the audit key is a derived
// pointer at it, and a process that died between the two left a decision the
// log does not show. The next decision of the same series repairs it, from the
// listing it already made.
//
// **It heals on the next decision, not on the next read**, which is a deviation
// from AC9's wording and is deliberate. Healing in Audit() would mean listing
// every series' baseline prefix on every read of the log — the one read path
// that has no series to scope it — to repair a state that is rare and never
// endangers the record itself: the entry is inside the decision object, so what
// is briefly missing is a pointer at a fact the store still holds in full. A gap
// older than auditHealProbes decisions is therefore reported rather than
// repaired, and reporting it is Story 8.11's --verify.
//
// Bounded to auditHealProbes, and see that constant for why the bound is where
// the gap is rather than where the listing ends.
//
// Nothing it fails at fails the operation. The decision that brought us here is
// already committed and its own entry is already written; a repair that cannot
// run is a warning about an older write, and turning it into a failure would
// make the caller retry an approval that succeeded and append a second one.
func (s *Blob) healAuditPointers(ctx context.Context, series blobSeries, prior []decisionKey) {
	for _, key := range prior[:min(len(prior), auditHealProbes)] {
		_, err := s.statIndex(ctx, key.auditPointer().String())

		switch {
		case err == nil:
			continue

		case errors.Is(err, ErrNotFound):
			s.healOneAuditPointer(ctx, series, key)

		default:
			s.log.Warn("whether an earlier baseline decision has an audit entry could not be established",
				"key", key.String(), "target", series.target,
				"consent_mode", string(series.mode), "error", err)
		}
	}
}

// healOneAuditPointer puts back the audit entry of one decision that lost it.
func (s *Blob) healOneAuditPointer(ctx context.Context, series blobSeries, key decisionKey) {
	body, err := s.readDecision(ctx, key)
	if err != nil {
		s.log.Warn("an earlier baseline decision has no audit entry and could not be read to re-create one",
			"key", key.String(), "target", series.target,
			"consent_mode", string(series.mode), "error", err)

		return
	}

	if err := s.putAuditPointer(ctx, key, body.Audit); err != nil {
		s.log.Warn("an earlier baseline decision has no audit entry and one could not be written",
			"key", key.String(), "target", series.target,
			"consent_mode", string(series.mode), "error", err)

		return
	}

	// Reported at Info because it is not routine: it means an earlier write of
	// this store was interrupted between its two objects, which an operator
	// looking at an audit log with a gap in it needs to be able to correlate.
	s.log.Info("the audit entry of an earlier baseline decision was missing and has been re-created",
		"key", key.String(), "target", series.target, "consent_mode", string(series.mode))
}

// --- the fold --------------------------------------------------------------

// readDecisions returns the baseline decisions of one series, newest first.
//
// One listing, and no bodies at all: everything the fold selects on — when the
// decision was taken and whether it approved or withdrew — is in the key, which
// is what makes HasBaseline a listing and no read (§6.5).
//
// Only the first page is read. The decision that stands is at the front of it,
// and the rest of the page is what the self-heal covers; a series with more
// decisions than one page holds is a target somebody has approved a thousand
// times, and the oldest of those need neither resolving nor healing. Paging
// continues only while every key seen so far shares one instant, because a fold
// cannot choose between decisions of one instant until it has seen all of them.
func (s *Blob) readDecisions(ctx context.Context, series blobSeries) ([]decisionKey, error) {
	var (
		out    []decisionKey
		cursor indexCursor
	)

	for cursor.more() {
		page, err := s.listIndex(ctx, series.id.baselineDirPrefix(), cursor, indexListPageSize)
		if err != nil {
			return nil, fmt.Errorf("reading the baseline decisions of %s: %w", series, err)
		}

		cursor = page.next
		out = s.observeDecisions(series, out, page)

		if !oneInstant(out) {
			break
		}

		if len(out) > maxTieRun {
			return nil, fmt.Errorf(
				"more than %d baseline decisions of %s claim to have been taken at the same nanosecond: %w",
				maxTieRun, series, ErrCorrupt,
			)
		}
	}

	return out, nil
}

// oneInstant reports whether every decision seen so far shares one instant,
// which is the condition under which the listing is not finished yet.
//
// An empty set counts, so a page that held nothing this grammar produces does
// not end the walk while there are pages left.
func oneInstant(keys []decisionKey) bool {
	return len(keys) == 0 || keys[len(keys)-1].inv == keys[0].inv
}

// observeDecisions parses one page of the baseline listing.
func (s *Blob) observeDecisions(series blobSeries, out []decisionKey, page indexPage) []decisionKey {
	for _, object := range page.objects {
		key, err := parseDecisionKey(object.key)
		if err != nil {
			// Reported and stepped over rather than failing the read: something
			// else has written into the index, and refusing to say what the
			// baseline is because of it would turn one stray object into an
			// approval nobody can read.
			s.log.Warn("an object among the baseline decisions is not a key this store writes, and was ignored",
				"key", object.key, "target", series.target, "consent_mode", string(series.mode))

			continue
		}

		out = append(out, key)
	}

	return out
}

// currentDecision is the fold: which of the visible decisions is in force
// (AC8).
//
// Newest wins, and newest is the smallest ordering field because the field is
// inverted — so the listing has already put it first.
//
// **A tie at one instant goes to the withdrawal.** Two decisions of one series
// can share an ordering field by two mechanisms the bump in decisionInstant
// cannot close: a listing that had not caught up with the earlier decision, so
// there was nothing to bump against; and a host past indexHorizon, where every
// instant clamps to one field and the bump has nowhere left to go. Both are
// rare and both are reachable, and when one happens the fold has to choose
// between "approved" and "withdrawn" from two keys that say the same about
// when.
//
// The earlier draft chose the smaller body digest on the grounds that it is
// host-independent and gives every reader the same answer. It is both of those
// and it is still wrong here: a digest is a coin flip, so it leaves an approval
// the operator has withdrawn standing about half the time, and findings stay
// silenced against it. First-in-the-listing is worse again — the field after
// the ordering pair is the operation, and "approve" sorts before "revoke", so
// it would favour the approval every time.
//
// Revoke-wins is host-independent, deterministic, and causally the right
// answer: a withdrawal can only be issued against an approval that already
// existed, so at a tie the withdrawal is the later of the two whatever the
// clocks said. It is also the only direction a store that silences findings may
// lean (AC8, Tenet 5).
//
// Two decisions sharing an instant *and* an operation still fall through to the
// digest, where the choice is between two facts of the same kind and either
// answer is safe.
//
// Every decision, the winner and the losers, stays visible in the audit log.
func currentDecision(keys []decisionKey) (decisionKey, bool) {
	if len(keys) == 0 {
		return decisionKey{}, false
	}

	best := keys[0]

	for _, key := range keys[1:] {
		if key.inv != best.inv {
			break
		}

		if beatsAtOneInstant(key, best) {
			best = key
		}
	}

	return best, true
}

// beatsAtOneInstant is the tie-break currentDecision argues for, applied to two
// decisions already known to share an ordering field.
func beatsAtOneInstant(key, best decisionKey) bool {
	if key.op != best.op {
		return key.op == opRevoke
	}

	return key.did < best.did
}

// GetBaseline reads the approved baseline for one target and consent mode.
//
// One listing and, when something is approved, one read. A withdrawn baseline
// costs no read at all: the operation is in the key, so the fold knows there is
// nothing to fetch before it fetches anything.
func (s *Blob) GetBaseline(target string, mode model.ConsentMode) (*Baseline, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	keys, err := s.readDecisions(ctx, series)
	if err != nil {
		return nil, fmt.Errorf("reading the baseline for %s: %w", series, err)
	}

	key, ok := currentDecision(keys)
	if !ok || key.op != opApprove {
		return nil, fmt.Errorf("baseline for %s: %w", series, ErrNotFound)
	}

	body, err := s.readDecision(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("reading the baseline for %s: %w", series, err)
	}

	if body.Baseline == nil {
		return nil, fmt.Errorf("the baseline decision %s approves nothing: %w", key, ErrCorrupt)
	}

	return body.Baseline, nil
}

// HasBaseline reports whether a baseline exists, without reading the scan it
// approved.
//
// It is the question the targets page asks once per series on every render, and
// here it is one listing and zero reads — against GetBaseline's read of a whole
// approved result, which for a page of forty targets would be forty documents
// fetched and discarded.
//
// It cannot disagree with GetBaseline, because both are the same fold over the
// same listing and differ only in whether they go on to open the object.
func (s *Blob) HasBaseline(target string, mode model.ConsentMode) (bool, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	series := blobSeriesFor(target, mode)

	keys, err := s.readDecisions(ctx, series)
	if err != nil {
		return false, fmt.Errorf("checking for a baseline for %s: %w", series, err)
	}

	key, ok := currentDecision(keys)

	return ok && key.op == opApprove, nil
}

// readDecision opens one decision object and establishes that it is the
// decision its key names.
//
// A key that listed and whose object is absent is **ErrCorrupt and never
// ErrNotFound**. Nothing in this store deletes a decision object — not
// retention, not compaction, not a withdrawal — so absence there is a bucket
// that lost an object or a person who removed one, and inferring "there is no
// baseline" from it would silence exactly the findings an approval was never
// given for. That is the quiet lie Tenet 5 forbids, and it is the one place in
// this store where a missing object is not allowed to mean "not there".
//
// The digest in the key is checked against the bytes, because the key is the
// digest: a body that does not hash to it is not the decision that was
// recorded, whatever else it may be.
func (s *Blob) readDecision(ctx context.Context, key decisionKey) (decisionBody, error) {
	raw, err := s.getIndex(ctx, key.String())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return decisionBody{}, fmt.Errorf(
				"the baseline decision %s is listed and its object is gone: %w", key, ErrCorrupt,
			)
		}

		return decisionBody{}, err
	}

	if digest := shortSum(raw, identityHexLen); digest != key.did {
		return decisionBody{}, fmt.Errorf("the baseline decision %s holds a body that hashes to %s: %w",
			key, digest, ErrCorrupt)
	}

	var body decisionBody

	if err := json.Unmarshal(raw, &body); err != nil {
		return decisionBody{}, fmt.Errorf("the baseline decision %s does not decode: %w: %w", key, ErrCorrupt, err)
	}

	if body.Op != key.op {
		return decisionBody{}, fmt.Errorf("the baseline decision %s records a %s instead: %w",
			key, truncateForMessage(string(body.Op)), ErrCorrupt)
	}

	if body.Baseline != nil && seriesFor(body.Baseline.Target, body.Baseline.ConsentMode) != key.series {
		return decisionBody{}, fmt.Errorf("the baseline decision %s approves a scan of %q instead: %w",
			key, truncateForMessage(body.Baseline.Target), ErrCorrupt)
	}

	return body, nil
}

// --- the audit log ---------------------------------------------------------

// Audit returns the newest entries of the decision log.
//
// Newest is decided by when the action happened, which is what the ordering
// field in the key is, and never by when the object was written — the same rule
// the SQL stores follow since their query became "order by at desc, id desc",
// and the reason the change was worth making: an entry recorded with an At that
// is not now belongs where its timestamp puts it, whichever store holds it.
//
// One listing and one small read per entry, sixteen at a time. A limit of zero
// means all, as it does for every store, bounded by blobMaxHydrate for the
// reason ListResults gives: the interface's "all of them" must not turn into an
// unbounded number of requests (AC11).
func (s *Blob) Audit(limit int) ([]AuditEntry, error) {
	ctx, cancel := s.opBudget()
	defer cancel()

	want := hydrateLimit(limit)

	keys, err := s.auditKeys(ctx, want)
	if err != nil {
		return nil, fmt.Errorf("reading the audit log: %w", err)
	}

	if len(keys) < want {
		// The loose keys did not fill the answer, so what is left of the log
		// has either never existed or been compacted away.
		if err := s.noAuditCheckpoints(ctx); err != nil {
			return nil, fmt.Errorf("reading the audit log: %w", err)
		}
	}

	entries, err := s.readAuditEntries(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("reading the audit log: %w", err)
	}

	return entries, nil
}

// auditKeys lists the newest entries of the log, stopping as soon as it has as
// many as the answer holds.
func (s *Blob) auditKeys(ctx context.Context, want int) ([]auditKey, error) {
	var (
		out    []auditKey
		cursor indexCursor
	)

	for cursor.more() && len(out) < want {
		page, err := s.listIndex(ctx, indexAuditPrefix, cursor, indexListPageSize)
		if err != nil {
			return nil, err
		}

		cursor = page.next

		for _, object := range page.objects {
			if len(out) == want {
				break
			}

			key, err := parseAuditKey(object.key)
			if err != nil {
				s.log.Warn("an object in the audit log is not a key this store writes, and was ignored",
					"key", object.key)

				continue
			}

			out = append(out, key)
		}
	}

	return out, nil
}

// noAuditCheckpoints establishes that the whole of the log is still in the
// loose keys the read above walked.
//
// Compaction of the audit prefix folds old entries into a checkpoint object and
// deletes the keys it folded (§7.2). Nothing in this build writes one and this
// read cannot open one, so a checkpoint being there means the answer is short by
// however much it holds — and a compliance log that is quietly missing its
// older half is the worst way for that to be discovered. Reporting it costs one
// listing, and only on the reads the loose entries did not satisfy.
//
// It is ErrIndexIncomplete rather than ErrCorrupt because nothing is damaged:
// the entries are there and this build cannot see them, which is exactly the
// distinction that sentinel exists to carry.
func (s *Blob) noAuditCheckpoints(ctx context.Context) error {
	page, err := s.listIndex(ctx, indexAuditCkptPrefix, indexCursor{}, 1)
	if err != nil {
		return err
	}

	if len(page.objects) == 0 {
		return nil
	}

	return fmt.Errorf("the audit log has been compacted into %s, which this build does not read: %w",
		page.objects[0].key, ErrIndexIncomplete)
}

// readAuditEntries reads the bodies of the keys one listing selected, keeping
// the order the listing produced.
func (s *Blob) readAuditEntries(ctx context.Context, keys []auditKey) ([]AuditEntry, error) {
	read := make([]*AuditEntry, len(keys))

	err := eachBounded(ctx, len(keys), blobHydrateConcurrency, func(ctx context.Context, i int) error {
		entry, ok, err := s.readAuditEntry(ctx, keys[i])
		if err != nil {
			return err
		}

		if ok {
			read[i] = &entry
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]AuditEntry, 0, len(keys))

	for _, entry := range read {
		if entry == nil {
			continue
		}

		out = append(out, *entry)
	}

	return out, nil
}

// readAuditEntry reads one entry of the log, reporting separately that there
// was nothing usable there.
//
// Both of the ways that can happen leave the rest of the log readable, which is
// the answer the SQL stores already give: one bad row must not hide the others.
// An object that has gone was folded into a checkpoint and deleted between the
// listing and the read; an object that will not decode is damaged, and is
// reported at Warn because — unlike a scan, whose key still says when it
// started and what it was — an audit entry keeps nothing outside its body that
// could be shown in its place.
func (s *Blob) readAuditEntry(ctx context.Context, key auditKey) (AuditEntry, bool, error) {
	raw, err := s.getIndex(ctx, key.String())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.log.Debug("an audit entry was deleted while the log was being read", "key", key.String())

			return AuditEntry{}, false, nil
		}

		return AuditEntry{}, false, err
	}

	var entry AuditEntry

	if err := json.Unmarshal(raw, &entry); err != nil {
		s.log.Warn("an audit entry does not decode and was left out of the log",
			"key", key.String(), "error", err)

		return AuditEntry{}, false, nil
	}

	return entry, true, nil
}

// RecordAudit appends one entry to the decision log.
//
// It is the entry point for actions taken outside the store — an allow-list
// addition written back to a config file — which is why it accepts the instant
// rather than reading a clock: an entry that arrives late belongs where its own
// timestamp puts it, not at the top of the log.
//
// Two things make one call one entry and two calls two, and they are different
// mechanisms answering different questions.
//
// The **nonce** makes the key unique. Two identical actions recorded in one
// clock tick — certain under a pinned clock, possible on a coarse timer — would
// otherwise hash to one digest, land on one key, and become one entry; deleting
// an audit record because it resembled another one is not acceptable. It is
// drawn **once, here, above the write and therefore above every retry inside
// it**, which is the whole reason it is a local and not something the write
// path draws: a nonce redrawn per attempt would turn one retried write into two
// entries, which is the same defect in the opposite direction.
//
// The **instant probe** makes the order right. It is the deviation from §6.5's
// zero-request RecordAudit, and it is here because the shared suite requires
// that two entries recorded at one instant come back in the order they were
// recorded — which SQL gets from its row identity and a bucket has nothing to
// get from except what is in the key. So the entry is filed at the first
// nanosecond at or after its own that no entry occupies, and the body keeps the
// instant the caller gave: the log records when the action happened, and the
// key records where the store filed it.
func (s *Blob) RecordAudit(e AuditEntry) error {
	if e.At.IsZero() {
		e.At = s.now()
	}

	// UTC, because the object records an instant and not the offset the
	// recording host happened to be in — the same normalisation a stored
	// result's start time gets, and for the same reason.
	e.At = e.At.UTC()

	ctx, cancel := s.opBudget()
	defer cancel()

	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding audit entry: %w", err)
	}

	nonce, err := auditNonce()
	if err != nil {
		return err
	}

	at, err := s.freeAuditInstant(ctx, e.At)
	if err != nil {
		return fmt.Errorf("recording an audit entry: %w", err)
	}

	key := auditEntryAt(at, shortSum(slices.Concat(body, nonce), identityHexLen))

	if _, err := s.putIndex(ctx, key.String(), body); err != nil {
		return fmt.Errorf("recording an audit entry: %w", err)
	}

	return nil
}

// auditNonce draws the value that keeps two identical audit entries apart.
//
// crypto/rand rather than a counter or a process identifier, because the
// property wanted is that two writers who never meet do not collide, and that
// is what a random draw gives without any coordination at all.
func auditNonce() ([]byte, error) {
	nonce := make([]byte, auditNonceBytes)

	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("drawing the value that keeps two audit entries of one instant apart: %w", err)
	}

	return nonce, nil
}

// freeAuditInstant is the nanosecond a new entry is filed at: its own, or the
// first one after it that the log has not already used.
//
// It steps one nanosecond at a time rather than jumping, because the position
// has to stay next to the instant it belongs at: an entry pushed a microsecond
// forward would sort past entries that genuinely happened in between.
//
// Running out of steps files the entry anyway. Sixty-four entries sharing a
// nanosecond is not a sequence of actions, and the only thing left to get wrong
// at that point is their order among themselves — where losing the record
// entirely would be the worse answer by far (Tenet 5).
//
// **What it can promise is bounded by the listing it asks**, and that is worth
// saying plainly because it is the one place in this store that leans on a
// listing being fresh. Two entries recorded at one instant come back in
// recording order only when the probe for the second one sees the first, which
// no provider promises and which two processes writing at once can defeat
// outright. When it does not, both are filed at that instant and their order is
// the byte order of their digests — over the entry *and* a random nonce, so it
// is arbitrary rather than merely surprising. Nothing is lost when that
// happens: both entries are in the log, with the instants their callers gave.
// What is lost is the sequence between two of them, and the only mechanism that
// would not lean on a listing is a caller that hands RecordAudit distinct
// instants, which every caller inside this package already does.
func (s *Blob) freeAuditInstant(ctx context.Context, at time.Time) (time.Time, error) {
	for shift := range auditInstantProbes {
		candidate := at.Add(time.Duration(shift) * time.Nanosecond)

		taken, err := s.auditInstantTaken(ctx, candidate)
		if err != nil {
			return time.Time{}, err
		}

		if !taken {
			return candidate, nil
		}
	}

	s.log.Warn("more audit entries share one instant than the log can order, so this one was filed beside them",
		"at", at, "probes", auditInstantProbes)

	return at.Add(time.Duration(auditInstantProbes-1) * time.Nanosecond), nil
}

// auditInstantTaken reports whether the log already holds an entry at one
// instant.
//
// One listing of the ordering field's own prefix, asking for a single key: the
// field is fixed-width and is followed by a separator no component contains, so
// the prefix matches that instant and no other.
func (s *Blob) auditInstantTaken(ctx context.Context, at time.Time) (bool, error) {
	page, err := s.listIndex(ctx, indexAuditPrefix+inv(at)+fieldSeparator, indexCursor{}, 1)
	if err != nil {
		return false, err
	}

	return len(page.objects) > 0, nil
}
