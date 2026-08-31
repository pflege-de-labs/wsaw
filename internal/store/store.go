// Package store persists scan results, baselines, and evidence artifacts.
//
// The store is embedded and pure Go (bbolt), which keeps the single-binary
// and cross-compilation promise (Tenet 14). Results are addressed by target,
// consent mode, and time, because that triple is the identity of a scan.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/martint17r/wsaw/internal/model"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

var (
	bucketResults   = []byte("results")
	bucketBaselines = []byte("baselines")
	bucketMeta      = []byte("meta")
	bucketAudit     = []byte("audit")
)

// Store is the result database. It is safe for concurrent use.
type Store struct {
	db *bbolt.DB

	// artifactDir holds evidence files. Artifacts live outside the database
	// because screenshots and bodies would bloat it and are written once and
	// read rarely.
	artifactDir string
}

// Options configures a store.
type Options struct {
	// Path is the database file.
	Path string
	// ArtifactDir holds screenshots and stored bodies.
	ArtifactDir string
	// Timeout bounds waiting for the file lock, so a second instance fails
	// fast with a clear message instead of hanging.
	Timeout time.Duration
}

// Open creates or opens a store. Parent directories are created with
// restrictive permissions, since results can contain personal data (NFR §4).
func Open(opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("store: Path is required")
	}

	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}

	if dir := filepath.Dir(opts.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
		}
	}

	db, err := bbolt.Open(opts.Path, 0o600, &bbolt.Options{Timeout: opts.Timeout})
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", opts.Path, err)
	}

	s := &Store{db: db, artifactDir: opts.ArtifactDir}

	if err := s.init(); err != nil {
		_ = db.Close()

		return nil, err
	}

	if s.artifactDir != "" {
		if err := os.MkdirAll(s.artifactDir, 0o700); err != nil {
			_ = db.Close()

			return nil, fmt.Errorf("creating artifact directory %s: %w", s.artifactDir, err)
		}
	}

	return s, nil
}

func (s *Store) init() error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketResults, bucketBaselines, bucketMeta, bucketAudit} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("creating bucket %s: %w", name, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("initializing store: %w", err)
	}

	return nil
}

// Close releases the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing store: %w", err)
	}

	return nil
}

// seriesKey identifies a target and consent mode. Every query is scoped by
// it, because comparing across modes is never valid.
func seriesKey(target string, mode model.ConsentMode) []byte {
	return []byte(target + "\x00" + string(mode))
}

// resultKey orders results by time within a series, so a bucket scan returns
// them chronologically without sorting.
func resultKey(at time.Time, scanID string) []byte {
	key := make([]byte, 8, 8+len(scanID))
	binary.BigEndian.PutUint64(key, uint64(at.UTC().UnixNano()))

	return append(key, scanID...)
}

// PutResult stores a result. Storing is atomic: an unclean shutdown leaves
// either the whole result or none of it (NFR §2).
func (s *Store) PutResult(res *model.Result) error {
	if res.Target == "" {
		return errors.New("store: result has no target")
	}

	if res.ScanID == "" {
		return errors.New("store: result has no scan ID")
	}

	payload, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encoding result %s: %w", res.ScanID, err)
	}

	err = s.db.Update(func(tx *bbolt.Tx) error {
		series, err := tx.Bucket(bucketResults).CreateBucketIfNotExists(seriesKey(res.Target, res.ConsentMode))
		if err != nil {
			return fmt.Errorf("creating series bucket: %w", err)
		}

		return series.Put(resultKey(res.StartedAt, res.ScanID), payload)
	})
	if err != nil {
		return fmt.Errorf("storing result %s: %w", res.ScanID, err)
	}

	return nil
}

// Series identifies one target-and-mode stream.
type Series struct {
	Target string            `json:"target"`
	Mode   model.ConsentMode `json:"consentMode"`
}

// Series lists every stored series, sorted for deterministic output.
func (s *Store) Series() ([]Series, error) {
	var out []Series

	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketResults).ForEachBucket(func(k []byte) error {
			target, mode, ok := splitSeriesKey(k)
			if !ok {
				return nil
			}

			out = append(out, Series{Target: target, Mode: mode})

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("listing series: %w", err)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}

		return out[i].Mode < out[j].Mode
	})

	return out, nil
}

func splitSeriesKey(k []byte) (target string, mode model.ConsentMode, ok bool) {
	for i, b := range k {
		if b == 0 {
			return string(k[:i]), model.ConsentMode(k[i+1:]), true
		}
	}

	return "", "", false
}

// Summary is a lightweight description of a stored result, for listings that
// must not decode every full result.
type Summary struct {
	ScanID      string            `json:"scanId"`
	Target      string            `json:"target"`
	ConsentMode model.ConsentMode `json:"consentMode"`
	StartedAt   time.Time         `json:"startedAt"`
	Duration    time.Duration     `json:"durationNs"`

	Termination model.TerminationReason `json:"termination"`
	Error       string                  `json:"error,omitempty"`

	ConsentOutcome model.ConsentOutcome `json:"consentOutcome"`
	ConsentCMP     string               `json:"consentCmp,omitempty"`

	Requests          int `json:"requests"`
	ThirdPartyDomains int `json:"thirdPartyDomains"`
	PreConsentDomains int `json:"preConsentDomains"`
}

func summarize(res *model.Result) Summary {
	return Summary{
		ScanID:            res.ScanID,
		Target:            res.Target,
		ConsentMode:       res.ConsentMode,
		StartedAt:         res.StartedAt,
		Duration:          res.Duration,
		Termination:       res.Termination,
		Error:             res.Error,
		ConsentOutcome:    res.Consent.Outcome,
		ConsentCMP:        res.Consent.CMP,
		Requests:          len(res.Requests),
		ThirdPartyDomains: len(res.ThirdPartyDomains("")),
		PreConsentDomains: len(res.ThirdPartyDomains(model.PhasePre)),
	}
}

// ListResults returns summaries newest first, at most limit entries. A limit
// of zero means all.
func (s *Store) ListResults(target string, mode model.ConsentMode, limit int) ([]Summary, error) {
	var out []Summary

	err := s.db.View(func(tx *bbolt.Tx) error {
		series := tx.Bucket(bucketResults).Bucket(seriesKey(target, mode))
		if series == nil {
			return nil
		}

		c := series.Cursor()

		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var res model.Result
			if err := json.Unmarshal(v, &res); err != nil {
				// One corrupt record must not make the whole history
				// unreadable, so it is skipped and reported as a summary.
				out = append(out, Summary{
					Target:      target,
					ConsentMode: mode,
					Termination: model.TermError,
					Error:       "stored result could not be decoded: " + err.Error(),
				})

				continue
			}

			out = append(out, summarize(&res))

			if limit > 0 && len(out) >= limit {
				return nil
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing results for %s/%s: %w", target, mode, err)
	}

	return out, nil
}

// GetResult returns one result by scan ID.
func (s *Store) GetResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	var out *model.Result

	err := s.db.View(func(tx *bbolt.Tx) error {
		series := tx.Bucket(bucketResults).Bucket(seriesKey(target, mode))
		if series == nil {
			return ErrNotFound
		}

		return series.ForEach(func(k, v []byte) error {
			if len(k) <= 8 || string(k[8:]) != scanID {
				return nil
			}

			var res model.Result
			if err := json.Unmarshal(v, &res); err != nil {
				return fmt.Errorf("decoding result %s: %w", scanID, err)
			}

			out = &res

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("reading result %s: %w", scanID, err)
	}

	if out == nil {
		return nil, fmt.Errorf("result %s for %s/%s: %w", scanID, target, mode, ErrNotFound)
	}

	return out, nil
}

// LatestResult returns the most recent result in a series.
func (s *Store) LatestResult(target string, mode model.ConsentMode) (*model.Result, error) {
	var out *model.Result

	err := s.db.View(func(tx *bbolt.Tx) error {
		series := tx.Bucket(bucketResults).Bucket(seriesKey(target, mode))
		if series == nil {
			return nil
		}

		_, v := series.Cursor().Last()
		if v == nil {
			return nil
		}

		var res model.Result
		if err := json.Unmarshal(v, &res); err != nil {
			return fmt.Errorf("decoding latest result: %w", err)
		}

		out = &res

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading latest result for %s/%s: %w", target, mode, err)
	}

	if out == nil {
		return nil, fmt.Errorf("latest result for %s/%s: %w", target, mode, ErrNotFound)
	}

	return out, nil
}

// PreviousResult returns the result immediately before the given scan ID,
// which is what "compare against the previous scan" needs.
func (s *Store) PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	var out *model.Result

	err := s.db.View(func(tx *bbolt.Tx) error {
		series := tx.Bucket(bucketResults).Bucket(seriesKey(target, mode))
		if series == nil {
			return nil
		}

		c := series.Cursor()

		found := false

		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if found {
				var res model.Result
				if err := json.Unmarshal(v, &res); err != nil {
					return fmt.Errorf("decoding previous result: %w", err)
				}

				out = &res

				return nil
			}

			if len(k) > 8 && string(k[8:]) == scanID {
				found = true
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading result before %s: %w", scanID, err)
	}

	if out == nil {
		return nil, fmt.Errorf("result before %s: %w", scanID, ErrNotFound)
	}

	return out, nil
}
