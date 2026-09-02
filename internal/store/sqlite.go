package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	// The CGo-free SQLite driver. It is a large dependency — a transpiled
	// SQLite — and that is the price of keeping CGo off: a driver needing CGo
	// would end cross-compilation to four platforms from one machine.
	_ "modernc.org/sqlite"
)

// sqliteDialect is the default store: an embedded file, no server to run.
type sqliteDialect struct{}

func (sqliteDialect) name() string      { return DriverSQLite }
func (sqliteDialect) sqlDriver() string { return "sqlite" }

// dsn builds the connection string, including the pragmas that make SQLite
// behave correctly for a long-running process.
func (sqliteDialect) dsn(opts Options) (string, error) {
	if opts.Path == "" {
		return "", fmt.Errorf("store: the %s driver needs store.path", DriverSQLite)
	}

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

	return "file:" + opts.Path + "?" + strings.Join(q, "&"), nil
}

// tune serialises access. SQLite takes one writer at a time, and respecting
// that here beats discovering it as intermittent "database is locked" errors
// under concurrent scans; wsaw's statements are short, so the cost is not
// measurable against a scan that takes seconds.
func (sqliteDialect) tune(db *sql.DB, _ Options) {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
}

func (sqliteDialect) rebind(query string) string { return query }

func (sqliteDialect) ddlIsTransactional() bool { return true }

func (sqliteDialect) upsert(conflict, update []string) string {
	return upsertExcluded(conflict, update)
}

func (sqliteDialect) pruneByCount() string { return pruneByCountPortable }

// migrations are applied in order; the index plus one is the schema version.
// Forward only: a migration that has shipped is never edited, because an
// existing store has already applied it.
func (sqliteDialect) migrations() [][]string {
	return [][]string{
		{
			`create table if not exists results (
				target       text    not null,
				consent_mode text    not null,
				scan_id      text    not null,
				started_at   integer not null,
				termination  text    not null,
				document     text    not null,
				primary key (target, consent_mode, scan_id)
			) strict`,

			// The one index every result query uses: newest-first within a
			// series, which also serves "the scan before this one" and
			// count-based pruning.
			`create index if not exists results_series
				on results (target, consent_mode, started_at desc, scan_id desc)`,

			`create table if not exists baselines (
				target       text    not null,
				consent_mode text    not null,
				scan_id      text    not null,
				approved_at  integer not null,
				document     text    not null,
				primary key (target, consent_mode)
			) strict`,

			`create table if not exists audit (
				id       integer primary key autoincrement,
				at       integer not null,
				document text    not null
			) strict`,
		},
	}
}

// schemaVersion reads SQLite's own per-database counter. Existing stores
// already record their version there, so it stays the mechanism for this
// dialect rather than being migrated into a table for symmetry's sake.
func (sqliteDialect) schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var current int

	if err := db.QueryRowContext(ctx, "pragma user_version").Scan(&current); err != nil {
		return 0, err
	}

	return current, nil
}

func (sqliteDialect) setSchemaVersion(ctx context.Context, ex execer, version int) error {
	// user_version does not accept a placeholder.
	_, err := ex.ExecContext(ctx, fmt.Sprintf("pragma user_version = %d", version))

	return err
}

// isTransient covers the lock contention SQLite expresses as an error once
// busy_timeout has been exhausted. There is no server to lose, so nothing
// else here is retryable.
func (sqliteDialect) isTransient(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		isTransientMessage(err)
}

// afterOpen tightens the database files. SQLite creates them with the process
// umask, and results can contain personal data (NFR §4).
func (sqliteDialect) afterOpen(opts Options) error {
	// The WAL and shared-memory files carry the same data as the database.
	for _, p := range []string{opts.Path, opts.Path + "-wal", opts.Path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}

		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("setting permissions on %s: %w", p, err)
		}
	}

	return nil
}
