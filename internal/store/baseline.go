package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/martint17r/wsaw/internal/model"
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
func (s *Store) SetBaseline(target string, mode model.ConsentMode, scanID, approvedBy, note string) (*Baseline, error) {
	res, err := s.GetResult(target, mode, scanID)
	if err != nil {
		return nil, err
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
		Result:      res,
	}

	payload, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encoding baseline: %w", err)
	}

	err = s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBaselines).Put(seriesKey(target, mode), payload)
	})
	if err != nil {
		return nil, fmt.Errorf("storing baseline for %s/%s: %w", target, mode, err)
	}

	if err := s.appendAudit(AuditEntry{
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

	return b, nil
}

// GetBaseline returns the approved baseline for a series.
func (s *Store) GetBaseline(target string, mode model.ConsentMode) (*Baseline, error) {
	var out *Baseline

	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketBaselines).Get(seriesKey(target, mode))
		if v == nil {
			return nil
		}

		var b Baseline
		if err := json.Unmarshal(v, &b); err != nil {
			return fmt.Errorf("decoding baseline: %w", err)
		}

		out = &b

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading baseline for %s/%s: %w", target, mode, err)
	}

	if out == nil {
		return nil, fmt.Errorf("baseline for %s/%s: %w", target, mode, ErrNotFound)
	}

	return out, nil
}

// DeleteBaseline removes an approval.
func (s *Store) DeleteBaseline(target string, mode model.ConsentMode, actor string) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketBaselines).Delete(seriesKey(target, mode))
	})
	if err != nil {
		return fmt.Errorf("deleting baseline for %s/%s: %w", target, mode, err)
	}

	return s.appendAudit(AuditEntry{
		At:     time.Now().UTC(),
		Actor:  actor,
		Action: "baseline-deleted",
		Target: target,
		Mode:   mode,
	})
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

func (s *Store) appendAudit(e AuditEntry) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding audit entry: %w", err)
	}

	err = s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketAudit)

		id, err := b.NextSequence()
		if err != nil {
			return fmt.Errorf("allocating audit sequence: %w", err)
		}

		key := make([]byte, 8)
		putUint64(key, id)

		return b.Put(key, payload)
	})
	if err != nil {
		return fmt.Errorf("appending audit entry: %w", err)
	}

	return nil
}

// Audit returns the most recent audit entries, newest first.
func (s *Store) Audit(limit int) ([]AuditEntry, error) {
	var out []AuditEntry

	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketAudit).Cursor()

		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var e AuditEntry
			if err := json.Unmarshal(v, &e); err != nil {
				continue
			}

			out = append(out, e)

			if limit > 0 && len(out) >= limit {
				return nil
			}
		}

		return nil
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

	return s.appendAudit(e)
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

// Prune enforces retention. Baselines are never pruned: they hold their own
// copy of the approved result, so history can expire without invalidating the
// definition of "expected".
func (s *Store) Prune(now time.Time, r Retention) (PruneStats, error) {
	var stats PruneStats

	if r.MaxAge <= 0 && r.MaxPerSeries <= 0 {
		return stats, nil
	}

	keep := make(map[string]struct{})

	err := s.db.Update(func(tx *bbolt.Tx) error {
		results := tx.Bucket(bucketResults)

		return results.ForEachBucket(func(name []byte) error {
			series := results.Bucket(name)

			type entry struct {
				key []byte
				at  time.Time
			}

			var entries []entry

			if err := series.ForEach(func(k, _ []byte) error {
				if len(k) < 8 {
					return nil
				}

				key := make([]byte, len(k))
				copy(key, k)

				entries = append(entries, entry{key: key, at: time.Unix(0, int64(getUint64(k[:8])))})

				return nil
			}); err != nil {
				return err
			}

			// Newest first, so the count limit keeps recent history.
			sort.Slice(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })

			for i, e := range entries {
				expiredByAge := r.MaxAge > 0 && now.Sub(e.at) > r.MaxAge
				expiredByCount := r.MaxPerSeries > 0 && i >= r.MaxPerSeries

				if !expiredByAge && !expiredByCount {
					if len(e.key) > 8 {
						keep[string(e.key[8:])] = struct{}{}
					}

					continue
				}

				if err := series.Delete(e.key); err != nil {
					return fmt.Errorf("deleting expired result: %w", err)
				}

				stats.ResultsDeleted++
			}

			return nil
		})
	})
	if err != nil {
		return stats, fmt.Errorf("pruning results: %w", err)
	}

	return stats, nil
}

// PutArtifact stores an evidence file and returns its reference. Artifacts
// are content-addressed, so storing the same screenshot twice costs one copy.
func (s *Store) PutArtifact(kind string, data []byte) (string, error) {
	if s.artifactDir == "" {
		return "", fmt.Errorf("artifact storage is not configured")
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
		return nil, fmt.Errorf("artifact storage is not configured")
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

func putUint64(b []byte, v uint64) {
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
}

func getUint64(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}
