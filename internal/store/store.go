// Package store persists scan results, baselines, and evidence artifacts.
//
// Persistence goes through database/sql, which makes the driver the seam
// rather than requiring a second implementation of everything (Tenet 12).
// SQLite is the default, through a CGo-free driver so the single static
// binary still cross-compiles to every supported platform (Tenet 14).
//
// A result is stored as its JSON document plus the few columns needed to
// index it. The document is deliberately not shredded into normalised tables:
// the JSON schema is the product's interface (Tenet 16), and a second schema
// would drift from it.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// The CGo-free SQLite driver. It is a large dependency — a transpiled
	// SQLite — and that is the price of keeping CGo off: a driver needing CGo
	// would end cross-compilation to four platforms from one machine.
	_ "modernc.org/sqlite"

	"github.com/martint17r/wsaw/internal/model"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store is the result database. It is safe for concurrent use.
type Store struct {
	db *sql.DB

	// artifactDir holds evidence files. Artifacts live outside the database
	// because screenshots and bodies would bloat it, are written once and
	// read rarely, and keeping them out leaves room to move them to object
	// storage later.
	artifactDir string
}

// Options configures a store.
type Options struct {
	// Path is the database file.
	Path string
	// ArtifactDir holds screenshots and stored bodies.
	ArtifactDir string
	// Timeout is how long a statement waits for a busy database before
	// failing, which is how SQLite's single-writer constraint surfaces.
	Timeout time.Duration
}

func (o *Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}

	return 5 * time.Second
}

// Open creates or opens a store. Parent directories are created with
// restrictive permissions, since results can contain personal data (NFR §4).
func Open(opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("store: Path is required")
	}

	if dir := filepath.Dir(opts.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dsn(opts))
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", opts.Path, err)
	}

	// SQLite takes one writer at a time. Serialising here respects that
	// rather than discovering it as intermittent "database is locked" errors
	// under concurrent scans; wsaw's statements are short, so the cost is not
	// measurable against a scan that takes seconds.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout()+5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("opening store %s: %w", opts.Path, err)
	}

	s := &Store{db: db, artifactDir: opts.ArtifactDir}

	if err := s.migrate(ctx); err != nil {
		_ = db.Close()

		return nil, err
	}

	if err := s.ensureFilePermissions(opts.Path); err != nil {
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

// dsn builds the connection string, including the pragmas that make SQLite
// behave correctly for a long-running process.
func dsn(opts Options) string {
	pragmas := []string{
		// WAL lets readers proceed while a scan is being written, which is
		// what the web interface needs while the daemon works.
		"journal_mode(WAL)",
		// Wait rather than failing immediately when another statement holds
		// the write lock.
		fmt.Sprintf("busy_timeout(%d)", opts.timeout().Milliseconds()),
		"foreign_keys(ON)",
		// NORMAL is durable under process loss with WAL; FULL would fsync on
		// every commit for protection against power loss that a scan result
		// does not warrant.
		"synchronous(NORMAL)",
	}

	q := make([]string, 0, len(pragmas))
	for _, p := range pragmas {
		q = append(q, "_pragma="+p)
	}

	return "file:" + opts.Path + "?" + strings.Join(q, "&")
}

// ensureFilePermissions tightens the database files. SQLite creates them with
// the process umask, and results can contain personal data.
func (s *Store) ensureFilePermissions(path string) error {
	// The WAL and shared-memory files carry the same data as the database.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}

		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("setting permissions on %s: %w", p, err)
		}
	}

	return nil
}

// migrations are applied in order; the index plus one is the schema version.
// Forward only: a migration that has shipped is never edited, because an
// existing store has already applied it.
var migrations = []string{
	`
	create table results (
		target       text    not null,
		consent_mode text    not null,
		scan_id      text    not null,
		started_at   integer not null,
		termination  text    not null,
		document     text    not null,
		primary key (target, consent_mode, scan_id)
	) strict;

	-- The one index every result query uses: newest-first within a series,
	-- which also serves "the scan before this one" and count-based pruning.
	create index results_series
		on results (target, consent_mode, started_at desc, scan_id desc);

	create table baselines (
		target       text    not null,
		consent_mode text    not null,
		scan_id      text    not null,
		approved_at  integer not null,
		document     text    not null,
		primary key (target, consent_mode)
	) strict;

	create table audit (
		id       integer primary key autoincrement,
		at       integer not null,
		document text    not null
	) strict;
	`,
}

// migrate brings the schema up to date, and refuses to run against a newer
// one. A binary that does not understand the schema must not write to it.
func (s *Store) migrate(ctx context.Context) error {
	var current int

	if err := s.db.QueryRowContext(ctx, "pragma user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading the store's schema version: %w", err)
	}

	if current > len(migrations) {
		return fmt.Errorf(
			"the store was written by a newer wsaw (schema version %d, this build understands %d); upgrade wsaw or point it at a different store",
			current, len(migrations))
	}

	for v := current; v < len(migrations); v++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("starting schema migration %d: %w", v+1, err)
		}

		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			_ = tx.Rollback()

			return fmt.Errorf("applying schema migration %d: %w", v+1, err)
		}

		// user_version does not accept a placeholder.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("pragma user_version = %d", v+1)); err != nil {
			_ = tx.Rollback()

			return fmt.Errorf("recording schema version %d: %w", v+1, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing schema migration %d: %w", v+1, err)
		}
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

// PutResult stores a result, replacing any earlier record of the same scan.
func (s *Store) PutResult(res *model.Result) error {
	if res.Target == "" {
		return errors.New("store: result has no target")
	}

	if res.ScanID == "" {
		return errors.New("store: result has no scan ID")
	}

	document, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encoding result %s: %w", res.ScanID, err)
	}

	ctx, cancel := s.opCtx()
	defer cancel()

	const q = `
		insert into results (target, consent_mode, scan_id, started_at, termination, document)
		values (?, ?, ?, ?, ?, ?)
		on conflict (target, consent_mode, scan_id) do update set
			started_at  = excluded.started_at,
			termination = excluded.termination,
			document    = excluded.document`

	_, err = s.db.ExecContext(ctx, q,
		res.Target, string(res.ConsentMode), res.ScanID,
		res.StartedAt.UnixNano(), string(res.Termination), string(document))
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
	ctx, cancel := s.opCtx()
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`select distinct target, consent_mode from results order by target, consent_mode`)
	if err != nil {
		return nil, fmt.Errorf("listing series: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Series

	for rows.Next() {
		var se Series

		var mode string

		if err := rows.Scan(&se.Target, &mode); err != nil {
			return nil, fmt.Errorf("listing series: %w", err)
		}

		se.Mode = model.ConsentMode(mode)
		out = append(out, se)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing series: %w", err)
	}

	return out, nil
}

// Summary is a lightweight description of a stored result, for listings.
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
//
// Summaries are derived from the stored document rather than from columns
// alongside it. Cached counts would drift the moment the derivation changed,
// and a listing that disagrees with the result it points at is worse than one
// that costs a parse.
func (s *Store) ListResults(target string, mode model.ConsentMode, limit int) ([]Summary, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	q := `
		select scan_id, document from results
		where target = ? and consent_mode = ?
		order by started_at desc, scan_id desc`

	args := []any{target, string(mode)}

	if limit > 0 {
		q += " limit ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing results for %s/%s: %w", target, mode, err)
	}

	defer func() { _ = rows.Close() }()

	var out []Summary

	for rows.Next() {
		var scanID, document string

		if err := rows.Scan(&scanID, &document); err != nil {
			return nil, fmt.Errorf("listing results for %s/%s: %w", target, mode, err)
		}

		var res model.Result

		if err := json.Unmarshal([]byte(document), &res); err != nil {
			// One unreadable record must not make a target's whole history
			// unreadable, and it must not vanish either: it is reported in
			// place, as itself (Tenet 5).
			out = append(out, Summary{
				ScanID:      scanID,
				Target:      target,
				ConsentMode: mode,
				Termination: model.TermError,
				Error:       "stored result could not be decoded: " + err.Error(),
			})

			continue
		}

		out = append(out, summarize(&res))
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing results for %s/%s: %w", target, mode, err)
	}

	return out, nil
}

// GetResult returns one result by scan ID.
func (s *Store) GetResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	const q = `select document from results where target = ? and consent_mode = ? and scan_id = ?`

	res, err := s.queryResult(ctx, q, target, string(mode), scanID)
	if err != nil {
		return nil, fmt.Errorf("reading result %s: %w", scanID, err)
	}

	if res == nil {
		return nil, fmt.Errorf("result %s for %s/%s: %w", scanID, target, mode, ErrNotFound)
	}

	return res, nil
}

// LatestResult returns the most recent result in a series.
func (s *Store) LatestResult(target string, mode model.ConsentMode) (*model.Result, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	const q = `
		select document from results
		where target = ? and consent_mode = ?
		order by started_at desc, scan_id desc
		limit 1`

	res, err := s.queryResult(ctx, q, target, string(mode))
	if err != nil {
		return nil, fmt.Errorf("reading latest result for %s/%s: %w", target, mode, err)
	}

	if res == nil {
		return nil, fmt.Errorf("latest result for %s/%s: %w", target, mode, ErrNotFound)
	}

	return res, nil
}

// PreviousResult returns the result immediately before the given scan ID,
// which is what "compare against the previous scan" needs.
func (s *Store) PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	ctx, cancel := s.opCtx()
	defer cancel()

	// A row-value comparison against the anchor scan, so results sharing a
	// start time still order deterministically — the same ordering the index
	// and every listing use.
	const q = `
		select document from results
		where target = ? and consent_mode = ?
		  and (started_at, scan_id) <
		      (select started_at, scan_id from results
		       where target = ? and consent_mode = ? and scan_id = ?)
		order by started_at desc, scan_id desc
		limit 1`

	res, err := s.queryResult(ctx, q, target, string(mode), target, string(mode), scanID)
	if err != nil {
		return nil, fmt.Errorf("reading result before %s: %w", scanID, err)
	}

	if res == nil {
		return nil, fmt.Errorf("result before %s: %w", scanID, ErrNotFound)
	}

	return res, nil
}

// queryResult runs a query returning one document, or nil when there is none.
func (s *Store) queryResult(ctx context.Context, query string, args ...any) (*model.Result, error) {
	var document string

	err := s.db.QueryRowContext(ctx, query, args...).Scan(&document)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}

	var res model.Result

	if err := json.Unmarshal([]byte(document), &res); err != nil {
		return nil, fmt.Errorf("decoding stored result: %w", err)
	}

	return &res, nil
}

// opCtx bounds a single database operation. Every call has a deadline, for the
// same reason every browser interaction does: nothing waits for ever.
func (s *Store) opCtx() (context.Context, context.CancelFunc) {
	const opTimeout = 30 * time.Second

	return context.WithTimeout(context.Background(), opTimeout)
}
