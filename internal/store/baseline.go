package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Baseline is an approved result that later scans are compared against.
type Baseline struct {
	Target      string            `json:"target"`
	ConsentMode model.ConsentMode `json:"consentMode"`

	// ScanID is the result that was approved.
	ScanID string `json:"scanId"`
	// ApprovedAt and ApprovedBy record who accepted this state, which is what
	// makes an approval auditable.
	ApprovedAt time.Time `json:"approvedAt"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
	Note       string    `json:"note,omitempty"`

	// Result is a copy of the approved scan. It is stored by value so that
	// retention pruning of old results cannot silently invalidate a baseline.
	Result *model.Result `json:"result"`
}

// SetBaseline approves a stored result as the baseline for its series.
//
// The approval and its audit entry are written in one transaction. An
// approval is what silences future findings, so an audit log that can lose
// one is not an audit log.
//
// The result itself is read before that transaction opens, because reading it
// now means fetching a document from the bucket, and a bucket call inside a
// database transaction would hold the transaction — and its locks — open
// across a network round trip to object storage (Story 8.2, AC7).
//
// What that window can cost is worth stating exactly, because the schema does
// not close it: there is no foreign key from baselines to results in any
// dialect, so a retention prune that deletes the approved scan's row between
// the read and the insert leaves a baseline naming a scan the history no longer
// lists. It costs the approval nothing — a Baseline carries its own copy of the
// result, deliberately, so that pruning cannot invalidate the definition of
// "expected" — and it is the same outcome as pruning the row one moment after
// the approval committed, which was always possible.
func (s *Store) SetBaseline(target string, mode model.ConsentMode, scanID, approvedBy, note string) (*Baseline, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	res, err := s.resultByID(ctx, target, mode, scanID)
	if err != nil {
		return nil, err
	}

	if !res.OK() {
		// Approving a failed scan would pin a broken observation as the
		// definition of "correct", making every later comparison meaningless.
		return nil, fmt.Errorf("scan %s did not produce a trustworthy asset list (%s) and cannot be a baseline",
			scanID, res.Termination)
	}

	// Built once, outside the retry: a replayed transaction must record the
	// same approval, at the same moment, in both the baseline and the audit
	// entry that explains it.
	b := &Baseline{
		Target:      target,
		ConsentMode: mode,
		ScanID:      scanID,
		ApprovedAt:  time.Now().UTC(),
		ApprovedBy:  approvedBy,
		Note:        note,
		Result:      res,
	}

	payload, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encoding baseline: %w", err)
	}

	// The retry encloses the whole transaction: a replayed transaction has to
	// begin again, not resume.
	if err := s.retry(ctx, "approving a baseline", func(ctx context.Context) error {
		return s.setBaselineTx(ctx, b, payload)
	}); err != nil {
		return nil, err
	}

	return b, nil
}

func (s *Store) setBaselineTx(ctx context.Context, b *Baseline, payload []byte) error {
	target, mode := b.Target, b.ConsentMode

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("approving baseline for %s/%s: %w", target, mode, err)
	}

	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, s.q(`
		insert into baselines (target, consent_mode, scan_id, approved_at, document)
		values (?, ?, ?, ?, ?)`+
		s.d.upsert(baselineKey, baselineUpdate)),
		target, string(mode), b.ScanID, b.ApprovedAt.UnixNano(), string(payload))
	if err != nil {
		return fmt.Errorf("storing baseline for %s/%s: %w", target, mode, err)
	}

	if err := s.appendAuditTx(ctx, tx, AuditEntry{
		At:      b.ApprovedAt,
		Actor:   b.ApprovedBy,
		Action:  "baseline-approved",
		Target:  target,
		Mode:    mode,
		Subject: b.ScanID,
		Note:    b.Note,
	}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing baseline approval for %s/%s: %w", target, mode, err)
	}

	return nil
}

// GetBaseline returns the approved baseline for a series.
func (s *Store) GetBaseline(target string, mode model.ConsentMode) (*Baseline, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	var document string

	err := s.retry(ctx, "reading a baseline", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx,
			s.q(`select document from baselines where target = ? and consent_mode = ?`),
			target, string(mode)).Scan(&document)
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("baseline for %s/%s: %w", target, mode, ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("reading baseline for %s/%s: %w", target, mode, err)
	}

	var b Baseline

	if err := json.Unmarshal([]byte(document), &b); err != nil {
		return nil, fmt.Errorf("decoding baseline for %s/%s: %w", target, mode, err)
	}

	return &b, nil
}

// DeleteBaseline removes an approval, recording that it happened.
func (s *Store) DeleteBaseline(target string, mode model.ConsentMode, actor string) error {
	ctx, cancel := s.opCtx()
	defer cancel()

	return s.retry(ctx, "deleting a baseline", func(ctx context.Context) error {
		return s.deleteBaselineTx(ctx, target, mode, actor)
	})
}

func (s *Store) deleteBaselineTx(ctx context.Context, target string, mode model.ConsentMode, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deleting baseline for %s/%s: %w", target, mode, err)
	}

	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		s.q(`delete from baselines where target = ? and consent_mode = ?`),
		target, string(mode)); err != nil {
		return fmt.Errorf("deleting baseline for %s/%s: %w", target, mode, err)
	}

	if err := s.appendAuditTx(ctx, tx, AuditEntry{
		At:     time.Now().UTC(),
		Actor:  actor,
		Action: "baseline-deleted",
		Target: target,
		Mode:   mode,
	}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing baseline deletion for %s/%s: %w", target, mode, err)
	}

	return nil
}

// AuditEntry records an action that changed approved state. Approvals must be
// auditable, since they are what silences future findings (Story 5.10).
type AuditEntry struct {
	At      time.Time         `json:"at"`
	Actor   string            `json:"actor,omitempty"`
	Action  string            `json:"action"`
	Target  string            `json:"target,omitempty"`
	Mode    model.ConsentMode `json:"consentMode,omitempty"`
	Subject string            `json:"subject,omitempty"`
	Note    string            `json:"note,omitempty"`
}

// appendAuditTx writes an audit entry inside a caller's transaction, so an
// action and its record commit together.
func (s *Store) appendAuditTx(ctx context.Context, tx *sql.Tx, e AuditEntry) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding audit entry: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		s.q(`insert into audit (at, document) values (?, ?)`),
		e.At.UnixNano(), string(payload)); err != nil {
		return fmt.Errorf("appending audit entry: %w", err)
	}

	return nil
}

// Audit returns the most recent audit entries, newest first.
func (s *Store) Audit(limit int) ([]AuditEntry, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	q := `select document from audit order by id desc`

	var args []any

	if limit > 0 {
		q += " limit ?"
		args = append(args, limit)
	}

	var out []AuditEntry

	err := s.retry(ctx, "reading the audit log", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var document string

			if err := rows.Scan(&document); err != nil {
				return err
			}

			var e AuditEntry

			if err := json.Unmarshal([]byte(document), &e); err != nil {
				// Skipped rather than fatal, as elsewhere: one bad row must
				// not hide the rest of the log.
				continue
			}

			out = append(out, e)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("reading audit log: %w", err)
	}

	return out, nil
}

// RecordAudit appends an entry for actions taken outside the store, such as an
// allow-list addition written back to a config file.
func (s *Store) RecordAudit(e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}

	ctx, cancel := s.opCtx()
	defer cancel()

	return s.retry(ctx, "recording an audit entry", func(ctx context.Context) error {
		return s.recordAuditTx(ctx, e)
	})
}

func (s *Store) recordAuditTx(ctx context.Context, e AuditEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording audit entry: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	if err := s.appendAuditTx(ctx, tx, e); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording audit entry: %w", err)
	}

	return nil
}

// PutArtifact stores an evidence file and returns its reference. Artifacts
// are content-addressed, so storing the same screenshot twice costs one copy.
//
// The write is followed by a claim on the reference, which is what keeps
// retention off these bytes until the scan that took them has committed the
// row that names them. It matters most in the case that writes nothing at all:
// an unchanged asset content-addresses to a key that is already in the bucket,
// so there is no fresh object for a sweep to date, and the claim is the only
// record that a running scan depends on it (Story 8.5, AC3; see claims.go).
func (s *Store) PutArtifact(kind string, data []byte) (string, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	ref, err := s.bucket.put(ctx, kind, data)
	if err != nil {
		return "", err
	}

	// Claimed after the write rather than before it: a claim on a key that was
	// never stored would protect nothing and would have to be cleaned up
	// anyway, while the window between the two is covered by the object's own
	// age — it has just been written.
	if err := s.claimArtifact(ctx, ref, time.Now()); err != nil {
		return "", err
	}

	return ref, nil
}

// ArtifactInfo is everything the store can say about a stored artifact
// without reading it.
//
// It is one type rather than three return values because both readers below
// answer the same question — what is this object — and a caller that stats an
// artifact today to show its size needs its digest and its age tomorrow to
// answer a conditional request (Story 8.7, AC4).
type ArtifactInfo struct {
	// Size is what the bucket reports the object holds, which is what a
	// Content-Length has to be written from before any of it is read.
	Size int64

	// Digest is the SHA-256 the reference addresses these bytes by. It comes
	// out of the reference rather than out of the object, so it costs nothing
	// and cannot disagree with the key that was fetched.
	Digest string

	// ModTime is when the bucket last wrote the object. Content addressing
	// makes it uninteresting as a version — the bytes cannot change under a
	// key — but a client that validates its cache by date rather than by
	// entity tag still needs an answer (Story 8.7, AC4).
	ModTime time.Time
}

// StatArtifact reports what the store knows about an artifact without reading
// it.
//
// It exists so the interface can tell "this evidence has been pruned" from
// "this evidence is here" without loading a megabyte of PNG to find out
// (Story 5.17, AC3) — which matters more against a bucket, where reading it
// would also cost a request.
//
// The caller's context bounds the call, so a request that has been abandoned
// stops waiting on a bucket that is not answering (Story 8.7, AC6); the
// store's own deadline still applies on top of it.
func (s *Store) StatArtifact(ctx context.Context, ref string) (ArtifactInfo, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	obj, err := s.bucket.stat(ctx, ref)
	if err != nil {
		return ArtifactInfo{}, absentArtifact(err)
	}

	return ArtifactInfo{Size: obj.size, Digest: artifactDigest(ref), ModTime: obj.modTime}, nil
}

// GetArtifact reads a stored artifact.
//
// The reference is validated by the bucket against the shape this store
// writes, so a crafted one cannot address anything else: references reach here
// from stored results, which are built from page-controlled data (Tenet 9).
//
// It stays for the callers that genuinely need the bytes in hand — a stored
// body being diffed, a document being decoded. Anything that only forwards
// them to a client should use OpenArtifact instead (Story 8.1, AC7).
func (s *Store) GetArtifact(ref string) ([]byte, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	data, err := s.bucket.get(ctx, ref)
	if err != nil {
		return nil, absentArtifact(err)
	}

	return data, nil
}

// OpenArtifact opens a stored artifact for streaming.
//
// It is the shape the HTTP layer serves from: a forty-megabyte document must
// not have to exist in the daemon's memory before a byte reaches the client,
// or ten readers of large evidence would cost ten full copies (Story 8.1,
// AC7). What the bucket said about the object comes back with it, because
// Content-Length, an entity tag and a modification date all have to be written
// before the body is (Story 8.7, AC1 and AC4).
//
// What is bounded is progress, not the transfer. The bucket has opTimeout to
// open the object and opTimeout to produce each further piece of it, and a
// read that stops producing bytes for that long is abandoned — but a transfer
// that keeps flowing runs as long as it takes. A fixed deadline on the whole
// read would be a size and bandwidth limit dressed as a timeout: a
// forty-megabyte document to a client on a slow link takes longer than any
// per-operation deadline, and cutting it off would abort the response after
// Content-Length had been declared and the status sent, with nothing left to
// tell the reader. The reader who has actually gone away is caught by their
// own request's context, which is this call's parent (Story 8.7, AC6).
//
// The caller owns the reader and must close it. Closing is also what releases
// the deadline and its timer, so a reader that is dropped rather than closed
// holds both until the idle bound fires — the same contract every
// io.ReadCloser has, stated because this one carries a context with it.
func (s *Store) OpenArtifact(ctx context.Context, ref string) (*ArtifactReader, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Armed before the open, so the open is bounded by it too, and re-armed by
	// every piece of the body that arrives.
	idle := time.AfterFunc(opTimeout, cancel)

	release := func() {
		idle.Stop()
		cancel()
	}

	stream, err := s.bucket.newReader(ctx, ref)
	if err != nil {
		release()

		return nil, absentArtifact(err)
	}

	return &ArtifactReader{
		ReadCloser: stream,
		ArtifactInfo: ArtifactInfo{
			Size:    stream.size,
			Digest:  artifactDigest(ref),
			ModTime: stream.modTime,
		},
		idle:    idle,
		release: release,
	}, nil
}

// ArtifactReader is a stored artifact opened for reading: the bytes, still in
// the bucket, and the same facts StatArtifact reports about them.
//
// It also ties one artifact read's bound to the reader handed out for it. The
// bound has to outlive OpenArtifact — the bytes are still being copied when it
// returns — and it is a bound on how long the bucket may go without producing
// anything rather than on how long the copy may take, so that a large artifact
// over a slow link is served rather than truncated (Story 8.7, AC1 and AC6).
//
// It is not safe for concurrent use, which no io.Reader is: the idle bound is
// re-armed from Read.
type ArtifactReader struct {
	io.ReadCloser
	ArtifactInfo

	idle    *time.Timer
	release func()
}

// Read forwards to the bucket and re-arms the idle bound whenever bytes
// arrive.
//
// Progress is what the timer measures, so it is reset on bytes rather than on
// the call: a provider that answers every read with zero bytes and no error is
// not making progress, and the bound is what stops that from holding a
// connection for ever.
func (a *ArtifactReader) Read(p []byte) (int, error) {
	n, err := a.ReadCloser.Read(p)
	if n > 0 {
		a.idle.Reset(opTimeout)
	}

	return n, err
}

// Close releases the bucket reader, the idle bound and its timer.
func (a *ArtifactReader) Close() error {
	defer a.release()

	if err := a.ReadCloser.Close(); err != nil {
		return fmt.Errorf("closing an artifact reader: %w", err)
	}

	return nil
}

// SignArtifactURL returns a URL a reader can fetch one artifact from directly,
// valid for ttl and no longer.
//
// It does not establish that the artifact is there, and cannot: signing is
// arithmetic over the key rather than a lookup, so every provider signs a key
// it has never heard of just as readily as one it holds. A caller that means
// to redirect a reader has to ask separately — StatArtifact — or the reader
// meets the bucket's own error page where wsaw's account of pruned evidence
// belongs (Story 8.7, AC5).
//
// A provider that cannot sign at all reports ErrSigningUnsupported, which is
// an invitation to fall back rather than a failure: the caller serves the
// bytes itself, as it did before anyone asked for redirects.
func (s *Store) SignArtifactURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	ctx, cancel := opCtxFrom(ctx)
	defer cancel()

	signed, err := s.bucket.signedURL(ctx, ref, ttl)
	if err != nil {
		return "", absentArtifact(err)
	}

	return signed, nil
}

// absentArtifact reports a reference this store never wrote as absent.
//
// References reach the readers above from URLs a reader typed or a page
// influenced, so a malformed one is ordinary hostile input rather than a fault
// in wsaw: the caller asked for something the store does not have, and the
// honest answer is that it is not here. The reason stays in the message and the
// reference stays refused — only the sentinel changes, so a crafted reference
// produces "not found" rather than a server error and an alert with it.
func absentArtifact(err error) error {
	if errors.Is(err, errInvalidRef) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}

	return err
}
