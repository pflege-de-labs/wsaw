package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// PutArtifact stores an evidence file and returns its reference. Artifacts
// are content-addressed, so storing the same screenshot twice costs one copy.
//
// The reference is the digest of the bytes handed in, never of the bytes
// written: an artifact may be stored gzipped, and how it is packed on disk is
// not part of what it is (Story 4.8, AC2).
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

	// Either form counts as stored. An installation upgraded into
	// compression must not write a compressed copy of everything it already
	// holds uncompressed, and one that turned compression off must not write
	// a raw copy of everything it holds compressed (Story 4.8, AC5).
	if artifactExists(path) || artifactExists(path+compressedSuffix) {
		return ref, nil
	}

	stored := data

	if s.compressArtifacts {
		if packed, worth := compressArtifact(data); worth {
			stored = packed
			path += compressedSuffix
		}
	}

	if err := writeArtifactFile(path, stored); err != nil {
		return "", err
	}

	if s.onArtifactStored != nil {
		s.onArtifactStored(kind, int64(len(data)), int64(len(stored)))
	}

	return ref, nil
}

// artifactExists reports whether a stored artifact is already on disk. An
// error other than absence — an unreadable directory — is left for the write
// to report, where it carries the operation that hit it.
func artifactExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// writeArtifactFile writes an artifact's bytes via a temporary file and a
// rename, so a crash never leaves a half-written artifact that a reader would
// treat as evidence.
func writeArtifactFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wsaw-artifact-*")
	if err != nil {
		return fmt.Errorf("creating temporary artifact file: %w", err)
	}

	defer func() {
		_ = os.Remove(tmp.Name())
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("setting artifact permissions: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("writing artifact: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing artifact: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("finalizing artifact: %w", err)
	}

	return nil
}

// StatArtifact reports an artifact's size without reading it.
//
// It exists so the interface can tell "this evidence has been pruned" from
// "this evidence is here" without loading a megabyte of PNG to find out
// (Story 5.17, AC3).
//
// The size is the artifact's own, not the stored file's. A reader is being
// told how much evidence there is, and that answer must not change because
// the bytes were packed differently (Story 4.8, AC7).
func (s *Store) StatArtifact(ref string) (int64, error) {
	if s.artifactDir == "" {
		return 0, errors.New("artifact storage is not configured")
	}

	path, err := s.artifactPath(ref)
	if err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err == nil {
		return info.Size(), nil
	}

	if !os.IsNotExist(err) {
		return 0, fmt.Errorf("reading artifact %s: %w", ref, err)
	}

	size, err := statCompressedArtifact(path + compressedSuffix)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("artifact %s: %w", ref, ErrNotFound)
		}

		return 0, fmt.Errorf("reading artifact %s: %w", ref, err)
	}

	return size, nil
}

// statCompressedArtifact reads the uncompressed size out of a gzip member's
// trailer, which is four bytes at the end of the file rather than a whole
// inflation.
func statCompressedArtifact(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // path is confined to artifactDir by artifactPath
	if err != nil {
		return 0, err
	}

	defer func() {
		_ = f.Close()
	}()

	trailer := make([]byte, 4)

	if _, err := f.Seek(-int64(len(trailer)), io.SeekEnd); err != nil {
		return 0, fmt.Errorf("seeking to the artifact trailer: %w", err)
	}

	if _, err := io.ReadFull(f, trailer); err != nil {
		return 0, fmt.Errorf("reading the artifact trailer: %w", err)
	}

	size, ok := gzipSize(trailer)
	if !ok {
		return 0, errors.New("artifact trailer is truncated")
	}

	return size, nil
}

// artifactPath resolves a reference to a path inside the artifact directory.
//
// References originate from stored results, which derive from page-controlled
// data, so they are untrusted: a crafted one must not escape the directory
// (Tenet 9).
func (s *Store) artifactPath(ref string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(ref))
	path := filepath.Join(s.artifactDir, clean)

	rel, err := filepath.Rel(s.artifactDir, path)
	if err != nil || rel == ".." || len(rel) >= 2 && rel[:2] == ".." {
		return "", fmt.Errorf("artifact reference %q is outside the artifact directory", ref)
	}

	return path, nil
}

// GetArtifact reads a stored artifact. The reference is validated so that a
// crafted path cannot escape the artifact directory.
//
// Whether an artifact is stored compressed is the store's business and no
// caller's: both forms come back as the bytes that were handed to
// PutArtifact (Story 4.8, AC4).
func (s *Store) GetArtifact(ref string) ([]byte, error) {
	if s.artifactDir == "" {
		return nil, errors.New("artifact storage is not configured")
	}

	path, err := s.artifactPath(ref)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is confined to artifactDir by artifactPath
	if err == nil {
		return data, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading artifact %s: %w", ref, err)
	}

	stored, err := os.ReadFile(path + compressedSuffix) //nolint:gosec // path is confined to artifactDir by artifactPath
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("artifact %s: %w", ref, ErrNotFound)
		}

		return nil, fmt.Errorf("reading artifact %s: %w", ref, err)
	}

	return decompressArtifact(ref, stored, s.maxArtifactBytes)
}
