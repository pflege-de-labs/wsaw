// Package store persists scan results, baselines, and evidence artifacts.
//
// Persistence goes through database/sql, which makes the driver the seam
// rather than requiring a second implementation of everything (Tenet 12).
// SQLite is the default, through a CGo-free driver so the single static
// binary still cross-compiles to every supported platform (Tenet 14);
// PostgreSQL and MySQL are options for a deployment that already runs one
// (Story 4.7). What differs between them is confined to dialect.go — the
// queries, the transactions and the decoding are one implementation.
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
	"time"

	"github.com/martint17r/wsaw/internal/model"
	"github.com/martint17r/wsaw/internal/secret"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store is the result database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	d  dialect

	// Retry policy. A server database is reachable over a network, so a
	// dropped connection is an ordinary event rather than a catastrophe
	// (Story 4.7, AC7).
	attempts int
	backoff  time.Duration
	onRetry  func(op string, attempt int, err error)

	// artifactDir holds evidence files. Artifacts live outside the database
	// because screenshots and bodies would bloat it, are written once and
	// read rarely, and keeping them out leaves room to move them to object
	// storage later.
	artifactDir string
}

// Options configures a store.
type Options struct {
	// Driver is sqlite (the default), postgres, or mysql.
	Driver string

	// Path is the database file, for the sqlite driver.
	Path string

	// DSN is the connection string, for a server database. It is a secret:
	// a DSN carries a password, so it is redacted everywhere a webhook token
	// is. Driver-specific settings that are not wsaw's business — TLS mode,
	// connect timeout, the server's own parameters — belong in it.
	DSN secret.Value

	// ArtifactDir holds screenshots and stored bodies.
	ArtifactDir string

	// Timeout is how long a statement waits for a busy database before
	// failing, which is how SQLite's single-writer constraint surfaces.
	Timeout time.Duration

	// MaxOpenConns and MaxIdleConns bound the pool for a server database.
	// Ignored for SQLite, which is deliberately serialised.
	MaxOpenConns int
	MaxIdleConns int
	// ConnMaxLifetime retires a pooled connection before the server does.
	ConnMaxLifetime time.Duration

	// MaxAttempts is how many times a store operation is tried when the
	// failure is transient — a dropped connection, a restarted server, a
	// deadlock. One means no retry. Zero takes the default.
	MaxAttempts int
	// RetryBackoff is the delay before the second attempt; it doubles for
	// each attempt after that.
	RetryBackoff time.Duration

	// OnRetry is called before each retry. A retry that nobody can see is a
	// flapping database that looks healthy, so the caller is given the chance
	// to log and count it (Tenet 8).
	OnRetry func(op string, attempt int, err error)
}

func (o *Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}

	return 5 * time.Second
}

func (o *Options) maxOpenConns() int {
	if o.MaxOpenConns > 0 {
		return o.MaxOpenConns
	}

	// Enough for the browser pool's scans plus the web interface, small
	// enough that several wsaw instances on one shared database do not
	// exhaust its connection limit between them.
	return 8
}

func (o *Options) maxIdleConns() int {
	if o.MaxIdleConns > 0 {
		return o.MaxIdleConns
	}

	return min(2, o.maxOpenConns())
}

func (o *Options) connMaxLifetime() time.Duration {
	if o.ConnMaxLifetime > 0 {
		return o.ConnMaxLifetime
	}

	// Shorter than a typical server-side idle timeout, so wsaw retires a
	// connection before the server drops it under a scan.
	return 30 * time.Minute
}

func (o *Options) maxAttempts() int {
	if o.MaxAttempts > 0 {
		return o.MaxAttempts
	}

	// Three attempts covers a failover or a restart without turning a
	// genuinely broken database into a long wait.
	return 3
}

func (o *Options) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return o.RetryBackoff
	}

	return 200 * time.Millisecond
}

// describe names the store for a log line or an error, without revealing a
// DSN's credentials.
func (o *Options) describe() string {
	if o.Driver == "" || o.Driver == DriverSQLite {
		return o.Path
	}

	if o.DSN.IsSet() {
		return o.Driver + " " + secret.RedactURL(o.DSN.Reveal())
	}

	return o.Driver
}

// Open creates or opens a store. For SQLite, parent directories are created
// with restrictive permissions, since results can contain personal data
// (NFR §4).
func Open(opts Options) (*Store, error) {
	d, err := dialectFor(opts.Driver)
	if err != nil {
		return nil, err
	}

	if d.name() == DriverSQLite {
		if opts.Path == "" {
			return nil, errors.New("store: Path is required")
		}

		if dir := filepath.Dir(opts.Path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
			}
		}
	}

	dsn, err := d.dsn(opts)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(d.sqlDriver(), dsn)
	if err != nil {
		// The DSN is never echoed: it carries a password.
		return nil, fmt.Errorf("opening store %s: %w", opts.describe(), err)
	}

	d.tune(db, opts)

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout()+5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("opening store %s: %w", opts.describe(), err)
	}

	s := &Store{
		db:          db,
		d:           d,
		attempts:    opts.maxAttempts(),
		backoff:     opts.retryBackoff(),
		onRetry:     opts.OnRetry,
		artifactDir: opts.ArtifactDir,
	}

	if err := s.migrate(ctx); err != nil {
		_ = db.Close()

		return nil, err
	}

	if err := d.afterOpen(opts); err != nil {
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

// Driver reports which database this store is using, for logs and for the
// tests that must run against every dialect.
func (s *Store) Driver() string { return s.d.name() }

// retry runs one store operation, trying again while the failure is
// transient. It exists because a server database is reached over a network:
// a restart, a failover, or a deadlock is an ordinary event, and failing a
// scan's result on the first dropped packet would lose an observation for no
// good reason.
//
// A permanent failure — a constraint violation, a malformed statement, a
// missing table — is returned on the first attempt. Retrying one only makes
// the failure slower and hides its cause.
//
// The operations retried here are idempotent by construction: the writes are
// upserts keyed by identity and deletes by identity. The exception is an
// audit entry, which is an append: if a connection drops after the server
// committed but before wsaw heard so, a retry can write it twice. A
// duplicated audit line is visible and harmless; a lost approval record is
// neither, so this is the right way round.
func (s *Store) retry(ctx context.Context, op string, fn func(context.Context) error) error {
	var lastErr error

	for attempt := 1; attempt <= s.attempts; attempt++ {
		if attempt > 1 {
			if err := s.wait(ctx, attempt); err != nil {
				return err
			}
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}

		// A cancelled caller is not a broken database, and retrying its work
		// would only delay the shutdown it asked for.
		if ctx.Err() != nil {
			return err
		}

		if !s.d.isTransient(err) {
			return err
		}

		lastErr = err

		// Reported only when another attempt actually follows: a "retrying"
		// line after the last attempt would overstate what happened.
		if s.onRetry != nil && attempt < s.attempts {
			s.onRetry(op, attempt, err)
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", op, s.attempts, lastErr)
}

// wait sleeps before an attempt, with exponential backoff, and gives up as
// soon as the context does.
func (s *Store) wait(ctx context.Context, attempt int) error {
	delay := s.backoff * (1 << (attempt - 2))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// q rewrites a query written with ? placeholders into the dialect's form.
// Every query in this package is written once, with ?, and passed through
// here — so a query cannot be correct for one database and malformed for
// another.
func (s *Store) q(query string) string { return s.d.rebind(query) }

// migrate brings the schema up to date, and refuses to run against a newer
// one. A binary that does not understand the schema must not write to it.
//
// The mechanism is identical for every dialect: the same numbered migrations,
// applied in the same order, recorded wherever that dialect records them.
func (s *Store) migrate(ctx context.Context) error {
	migrations := s.d.migrations()

	current, err := s.d.schemaVersion(ctx, s.db)
	if err != nil {
		return fmt.Errorf("reading the store's schema version: %w", err)
	}

	if current > len(migrations) {
		return fmt.Errorf(
			"the store was written by a newer wsaw (schema version %d, this build understands %d); upgrade wsaw or point it at a different store",
			current, len(migrations))
	}

	for v := current; v < len(migrations); v++ {
		if err := s.applyMigration(ctx, migrations[v], v+1); err != nil {
			return err
		}
	}

	return nil
}

// applyMigration runs one version's statements and records the new version.
//
// Where DDL is transactional the two happen together, so a half-applied
// schema is impossible. Where it is not — MySQL commits implicitly on DDL —
// the version is recorded only after every statement has succeeded, and the
// statements are written so that a rerun after a partial failure continues
// rather than trips over what already exists. Pretending otherwise, by
// wrapping MySQL in a transaction that cannot roll back, would be worse than
// saying so.
func (s *Store) applyMigration(ctx context.Context, statements []string, version int) error {
	if !s.d.ddlIsTransactional() {
		for _, stmt := range statements {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("applying schema migration %d: %w", version, err)
			}
		}

		if err := s.d.setSchemaVersion(ctx, s.db, version); err != nil {
			return fmt.Errorf("recording schema version %d: %w", version, err)
		}

		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting schema migration %d: %w", version, err)
	}

	defer func() { _ = tx.Rollback() }()

	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("applying schema migration %d: %w", version, err)
		}
	}

	if err := s.d.setSchemaVersion(ctx, tx, version); err != nil {
		return fmt.Errorf("recording schema version %d: %w", version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing schema migration %d: %w", version, err)
	}

	return nil
}

// Ping reports whether the database is reachable.
//
// For SQLite this is nearly free and nearly always true. For a server
// database it is the difference between a wsaw that is working and one that
// is scanning into a void, which readiness has to be able to tell apart: a
// watcher that cannot record what it saw is not watching (Tenet 8).
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("the %s store is not reachable: %w", s.d.name(), err)
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

	q := `
		insert into results (target, consent_mode, scan_id, started_at, termination, document)
		values (?, ?, ?, ?, ?, ?)` +
		s.d.upsert(resultKey, resultUpdate)

	err = s.retry(ctx, "storing a result", func(ctx context.Context) error {
		_, err := s.db.ExecContext(ctx, s.q(q),
			res.Target, string(res.ConsentMode), res.ScanID,
			res.StartedAt.UnixNano(), string(res.Termination), string(document))

		return err
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
	ctx, cancel := s.opCtx()
	defer cancel()

	var out []Series

	// The whole read is inside the retried function, including the row scan:
	// a connection that drops half way through a result set has to be redone
	// from the start, not resumed.
	err := s.retry(ctx, "listing series", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx,
			s.q(`select distinct target, consent_mode from results order by target, consent_mode`))
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				se   Series
				mode string
			)

			if err := rows.Scan(&se.Target, &mode); err != nil {
				return err
			}

			se.Mode = model.ConsentMode(mode)
			out = append(out, se)
		}

		return rows.Err()
	})
	if err != nil {
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

	var out []Summary

	err := s.retry(ctx, "listing results", func(ctx context.Context) error {
		out = nil

		rows, err := s.db.QueryContext(ctx, s.q(q), args...)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var scanID, document string

			if err := rows.Scan(&scanID, &document); err != nil {
				return err
			}

			var res model.Result

			if err := json.Unmarshal([]byte(document), &res); err != nil {
				// One unreadable record must not make a target's whole
				// history unreadable, and it must not vanish either: it is
				// reported in place, as itself (Tenet 5). This is a decoding
				// failure, not a database one, so it is never retried.
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

		return rows.Err()
	})
	if err != nil {
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

	err := s.retry(ctx, "reading a result", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(query), args...).Scan(&document)
	})

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
