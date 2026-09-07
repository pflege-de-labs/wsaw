package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
func (s *Store) SetBaseline(target string, mode model.ConsentMode, scanID, approvedBy, note string) (*Baseline, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	var b *Baseline

	// The retry encloses the whole transaction: a replayed transaction has to
	// begin again, not resume.
	err := s.retry(ctx, "approving a baseline", func(ctx context.Context) error {
		var err error

		b, err = s.setBaselineTx(ctx, target, mode, scanID, approvedBy, note)

		return err
	})
	if err != nil {
		return nil, err
	}

	return b, nil
}

func (s *Store) setBaselineTx(
	ctx context.Context,
	target string,
	mode model.ConsentMode,
	scanID, approvedBy, note string,
) (*Baseline, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("approving baseline for %s/%s: %w", target, mode, err)
	}

	defer func() { _ = tx.Rollback() }()

	var document string

	err = tx.QueryRowContext(ctx,
		s.q(`select document from results where target = ? and consent_mode = ? and scan_id = ?`),
		target, string(mode), scanID).Scan(&document)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("result %s for %s/%s: %w", scanID, target, mode, ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("reading result %s: %w", scanID, err)
	}

	var res model.Result

	if err := json.Unmarshal([]byte(document), &res); err != nil {
		return nil, fmt.Errorf("decoding result %s: %w", scanID, err)
	}

	if !res.OK() {
		// Approving a failed scan would pin a broken observation as the
		// definition of "correct", making every later comparison meaningless.
		return nil, fmt.Errorf("scan %s did not produce a trustworthy asset list (%s) and cannot be a baseline",
			scanID, res.Termination)
	}

	b := &Baseline{
		Target:      target,
		ConsentMode: mode,
		ScanID:      scanID,
		ApprovedAt:  time.Now().UTC(),
		ApprovedBy:  approvedBy,
		Note:        note,
		Result:      &res,
	}

	payload, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encoding baseline: %w", err)
	}

	_, err = tx.ExecContext(ctx, s.q(`
		insert into baselines (target, consent_mode, scan_id, approved_at, document)
		values (?, ?, ?, ?, ?)`+
		s.d.upsert(baselineKey, baselineUpdate)),
		target, string(mode), scanID, b.ApprovedAt.UnixNano(), string(payload))
	if err != nil {
		return nil, fmt.Errorf("storing baseline for %s/%s: %w", target, mode, err)
	}

	if err := s.appendAuditTx(ctx, tx, AuditEntry{
		At:      b.ApprovedAt,
		Actor:   approvedBy,
		Action:  "baseline-approved",
		Target:  target,
		Mode:    mode,
		Subject: scanID,
		Note:    note,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing baseline approval for %s/%s: %w", target, mode, err)
	}

	return b, nil
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

// Retention bounds how much history is kept.
type Retention struct {
	// MaxAge drops results older than this. Zero means no age limit.
	MaxAge time.Duration
	// MaxPerSeries keeps at most this many results per target and mode. Zero
	// means no count limit.
	MaxPerSeries int
}

// PruneStats reports what a prune removed, so retention is observable rather
// than silent.
type PruneStats struct {
	ResultsDeleted   int
	ArtifactsDeleted int
	BytesFreed       int64
}

// Prune enforces retention.
//
// Baselines are never pruned: they hold their own copy of the approved
// result, so history can expire without invalidating the definition of
// "expected".
func (s *Store) Prune(now time.Time, r Retention) (PruneStats, error) {
	var stats PruneStats

	if r.MaxAge <= 0 && r.MaxPerSeries <= 0 {
		return stats, nil
	}

	ctx, cancel := s.opCtx()
	defer cancel()

	err := s.retry(ctx, "pruning results", func(ctx context.Context) error {
		var err error

		stats, err = s.pruneTx(ctx, now, r)

		return err
	})

	return stats, err
}

func (s *Store) pruneTx(ctx context.Context, now time.Time, r Retention) (PruneStats, error) {
	var stats PruneStats

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("pruning results: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	if r.MaxAge > 0 {
		res, err := tx.ExecContext(ctx,
			s.q(`delete from results where started_at < ?`), now.Add(-r.MaxAge).UnixNano())
		if err != nil {
			return stats, fmt.Errorf("pruning results by age: %w", err)
		}

		if n, err := res.RowsAffected(); err == nil {
			stats.ResultsDeleted += int(n)
		}
	}

	if r.MaxPerSeries > 0 {
		// Ranked within each series by the same ordering every listing uses,
		// so "the newest N" means the same thing here as it does there. The
		// statement itself is the dialect's: MySQL refuses to select from the
		// table a delete targets, so it needs a different shape.
		res, err := tx.ExecContext(ctx, s.q(s.d.pruneByCount()), r.MaxPerSeries)
		if err != nil {
			return stats, fmt.Errorf("pruning results by count: %w", err)
		}

		if n, err := res.RowsAffected(); err == nil {
			stats.ResultsDeleted += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("committing prune: %w", err)
	}

	return stats, nil
}

// PutArtifact stores an evidence file and returns its reference. Artifacts
// are content-addressed, so storing the same screenshot twice costs one copy.
func (s *Store) PutArtifact(kind string, data []byte) (string, error) {
	if s.artifactDir == "" {
		return "", errors.New("artifact storage is not configured")
	}

	sum := sha256.Sum256(data)
	ref := kind + "/" + hex.EncodeToString(sum[:])
	path := filepath.Join(s.artifactDir, filepath.FromSlash(ref))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating artifact directory: %w", err)
	}

	if _, err := os.Stat(path); err == nil {
		return ref, nil
	}

	// Written via a temporary file and renamed, so a crash never leaves a
	// half-written artifact that a reader would treat as evidence.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wsaw-artifact-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary artifact file: %w", err)
	}

	defer func() {
		_ = os.Remove(tmp.Name())
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return "", fmt.Errorf("setting artifact permissions: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return "", fmt.Errorf("writing artifact: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing artifact: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("finalizing artifact: %w", err)
	}

	return ref, nil
}

// GetArtifact reads a stored artifact. The reference is validated so that a
// crafted path cannot escape the artifact directory.
func (s *Store) GetArtifact(ref string) ([]byte, error) {
	if s.artifactDir == "" {
		return nil, errors.New("artifact storage is not configured")
	}

	clean := filepath.Clean(filepath.FromSlash(ref))
	path := filepath.Join(s.artifactDir, clean)

	// Path traversal defence: references originate from stored results, which
	// derive from page-controlled data, so they are untrusted (Tenet 9).
	rel, err := filepath.Rel(s.artifactDir, path)
	if err != nil || rel == ".." || len(rel) >= 2 && rel[:2] == ".." {
		return nil, fmt.Errorf("artifact reference %q is outside the artifact directory", ref)
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is confined to artifactDir by the check above
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact %s: %w", ref, ErrNotFound)
		}

		return nil, fmt.Errorf("reading artifact %s: %w", ref, err)
	}

	return data, nil
}
