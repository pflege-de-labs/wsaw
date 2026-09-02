package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	// The pure-Go PostgreSQL driver, registered through its database/sql
	// shim. pgx rather than lib/pq because lib/pq is in maintenance mode, and
	// pure Go because CGo would end cross-compilation (Tenet 14).
	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresDialect stores results in PostgreSQL.
type postgresDialect struct{}

func (postgresDialect) name() string      { return DriverPostgres }
func (postgresDialect) sqlDriver() string { return "pgx" }

func (postgresDialect) dsn(opts Options) (string, error) {
	if !opts.DSN.IsSet() {
		return "", fmt.Errorf("store: the %s driver needs store.dsn", DriverPostgres)
	}

	return opts.DSN.Reveal(), nil
}

// tune sets pool limits. Unlike SQLite there is no single-writer constraint,
// so concurrent scans may write concurrently; the ceiling exists so that a
// wsaw with a large pool cannot exhaust a shared database's connections.
func (postgresDialect) tune(db *sql.DB, opts Options) {
	db.SetMaxOpenConns(opts.maxOpenConns())
	db.SetMaxIdleConns(opts.maxIdleConns())
	db.SetConnMaxLifetime(opts.connMaxLifetime())
}

func (postgresDialect) rebind(query string) string { return rebindDollar(query) }

func (postgresDialect) ddlIsTransactional() bool { return true }

func (postgresDialect) upsert(conflict, update []string) string {
	return upsertExcluded(conflict, update)
}

func (postgresDialect) pruneByCount() string { return pruneByCountPortable }

// migrations mirror the SQLite schema exactly, differing only where the type
// system does. started_at stays an integer count of nanoseconds rather than a
// timestamp type: it is the sort key every listing depends on, and a column
// that silently rounds it would reorder history.
func (postgresDialect) migrations() [][]string {
	return [][]string{
		{
			`create table if not exists results (
				target       text   not null,
				consent_mode text   not null,
				scan_id      text   not null,
				started_at   bigint not null,
				termination  text   not null,
				document     text   not null,
				primary key (target, consent_mode, scan_id)
			)`,

			`create index if not exists results_series
				on results (target, consent_mode, started_at desc, scan_id desc)`,

			`create table if not exists baselines (
				target       text   not null,
				consent_mode text   not null,
				scan_id      text   not null,
				approved_at  bigint not null,
				document     text   not null,
				primary key (target, consent_mode)
			)`,

			`create table if not exists audit (
				id       bigserial primary key,
				at       bigint not null,
				document text   not null
			)`,
		},
	}
}

// schemaVersionTable is where a server database records the applied schema
// version. SQLite has a counter of its own; these do not, and a table wsaw
// owns is preferable to a convention borrowed from a migration tool that is
// not a dependency.
const schemaVersionTable = `
	create table if not exists wsaw_schema_version (
		id      int not null primary key,
		version int not null
	)`

func (postgresDialect) schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	if _, err := db.ExecContext(ctx, schemaVersionTable); err != nil {
		return 0, err
	}

	var version int

	err := db.QueryRowContext(ctx, "select version from wsaw_schema_version where id = 1").Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No row yet means an empty database, which is version zero.
		return 0, nil
	case err != nil:
		return 0, err
	}

	return version, nil
}

func (postgresDialect) setSchemaVersion(ctx context.Context, ex execer, version int) error {
	_, err := ex.ExecContext(ctx, `
		insert into wsaw_schema_version (id, version) values (1, $1)
		on conflict (id) do update set version = excluded.version`, version)

	return err
}

// isTransient reads PostgreSQL's SQLSTATE rather than its message text,
// because the text is localised and the codes are not.
func (postgresDialect) isTransient(err error) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		// Class 08 — connection exception.
		case strings.HasPrefix(pgErr.Code, "08"):
			return true
		// Serialization failure and deadlock: the transaction can be replayed.
		case pgErr.Code == "40001", pgErr.Code == "40P01":
			return true
		// The server is shutting down, restarting, or refusing new work.
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03",
			pgErr.Code == "53300", pgErr.Code == "53400":
			return true
		}

		// Any other server-side error is the statement's own fault.
		return false
	}

	return isTransientMessage(err)
}

func (postgresDialect) afterOpen(Options) error { return nil }
