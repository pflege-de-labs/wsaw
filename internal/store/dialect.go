package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Driver names, as they appear in configuration.
const (
	// DriverSQLite is the default: an embedded file, no server to run.
	DriverSQLite = "sqlite"
	// DriverPostgres stores results in PostgreSQL.
	DriverPostgres = "postgres"
	// DriverMySQL stores results in MySQL.
	DriverMySQL = "mysql"
)

// Drivers lists the supported drivers, for configuration validation and
// error messages.
func Drivers() []string { return []string{DriverSQLite, DriverPostgres, DriverMySQL} }

// execer is the part of *sql.DB and *sql.Tx that schema work needs, so a
// migration can run inside a transaction or outside one without two code
// paths.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dialect isolates exactly the parts of the store's SQL that differ between
// databases: placeholder style, the schema itself, where the schema version
// lives, the upsert form, and the delete-by-identity form retention uses.
//
// Everything else — every query, every scan, every transaction boundary — is
// one implementation. A second copy of the query set per database would drift,
// and the drift would be silent until a deployment on the less-used database
// disagreed about what a result is.
//
// There is deliberately no JSON accessor here. Story 4.6 chose to derive
// summaries by parsing the stored document in Go rather than in SQL, so
// nothing in the store depends on `json_extract`, `->>` or `JSON_EXTRACT`.
// That decision is what makes this seam small.
type dialect interface {
	// name is the configured driver name.
	name() string
	// sqlDriver is the database/sql driver to open.
	sqlDriver() string
	// dsn builds the connection string.
	dsn(opts Options) (string, error)
	// tune applies the connection-pool settings this database needs.
	tune(db *sql.DB, opts Options)
	// rebind converts a query written with ? placeholders into this
	// dialect's form.
	rebind(query string) string
	// migrations are the schema statements, one slice per version. Index plus
	// one is the schema version, and every dialect must have the same number.
	migrations() [][]string
	// schemaVersion reads the applied schema version, creating whatever it is
	// recorded in if that does not exist yet.
	schemaVersion(ctx context.Context, db *sql.DB) (int, error)
	// setSchemaVersion records a newly applied version.
	setSchemaVersion(ctx context.Context, ex execer, version int) error
	// ddlIsTransactional reports whether schema changes roll back with a
	// transaction. MySQL commits implicitly on DDL, so they do not.
	ddlIsTransactional() bool
	// upsert builds the tail of an insert that overwrites an existing row.
	upsert(conflict, update []string) string
	// pruneByCount deletes all but the newest keep results per series.
	pruneByCount() string
	// isTransient reports whether an error is worth another attempt: a
	// dropped connection, a restarted server, a deadlock. A constraint
	// violation or a malformed statement is not, and retrying one only makes
	// the failure slower.
	isTransient(err error) bool
	// afterOpen is dialect-specific work once the connection is live.
	afterOpen(opts Options) error
}

func dialectFor(name string) (dialect, error) {
	switch name {
	case "", DriverSQLite:
		return sqliteDialect{}, nil
	case DriverPostgres:
		return postgresDialect{}, nil
	case DriverMySQL:
		return mysqlDialect{}, nil
	default:
		return nil, fmt.Errorf(
			"store: unknown driver %q; supported drivers are %s",
			name, strings.Join(Drivers(), ", "))
	}
}

// rebindDollar rewrites ? placeholders as $1, $2, … for PostgreSQL.
//
// Placeholders inside a single-quoted literal are left alone. No query in this
// package contains one, but a rewriter that only works for the queries that
// exist today is a trap for the query somebody adds tomorrow.
func rebindDollar(query string) string {
	var (
		b        strings.Builder
		n        int
		inString bool
	)

	b.Grow(len(query) + 8)

	for i := range len(query) {
		c := query[i]

		switch {
		case c == '\'':
			// A doubled quote is an escaped quote inside a literal, which
			// leaves the state unchanged either way.
			inString = !inString

			b.WriteByte(c)

		case c == '?' && !inString:
			n++

			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))

		default:
			b.WriteByte(c)
		}
	}

	return b.String()
}

// transientMessages are the connection-level failures every driver expresses
// as text rather than as a code. They are matched as a fallback, after each
// dialect has had a chance to recognise its own error codes.
var transientMessages = []string{
	"connection refused",
	"connection reset",
	"broken pipe",
	"use of closed network connection",
	"unexpected eof",
	"server closed",
	"bad connection",
	"i/o timeout",
	"no such host",
}

// isTransientMessage is the shared fallback. It is deliberately conservative:
// a false positive here turns a permanent failure into a slow permanent
// failure, so only unambiguous connection wording qualifies.
func isTransientMessage(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, driver.ErrBadConn) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	msg := strings.ToLower(err.Error())

	for _, m := range transientMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}

	return false
}

// upsertExcluded builds the ON CONFLICT form that SQLite and PostgreSQL share.
func upsertExcluded(conflict, update []string) string {
	sets := make([]string, 0, len(update))
	for _, col := range update {
		sets = append(sets, col+" = excluded."+col)
	}

	return " on conflict (" + strings.Join(conflict, ", ") + ") do update set " +
		strings.Join(sets, ", ")
}

// The columns the two upserts in this package overwrite. They are named here
// so the insert and its conflict clause cannot fall out of step.
var (
	resultKey      = []string{"target", "consent_mode", "scan_id"}
	resultUpdate   = []string{"started_at", "termination", "document"}
	baselineKey    = []string{"target", "consent_mode"}
	baselineUpdate = []string{"scan_id", "approved_at", "document"}
)

// pruneByCountPortable deletes all but the newest keep results per series,
// identified by primary key rather than by any physical row identifier.
//
// SQLite has rowid and PostgreSQL has ctid, but neither is the same concept
// and MySQL has neither. The primary key is the identity the schema already
// declares, so ranking on it works everywhere and needs no dialect at all.
const pruneByCountPortable = `
	delete from results where (target, consent_mode, scan_id) in (
		select target, consent_mode, scan_id from (
			select target, consent_mode, scan_id, row_number() over (
				partition by target, consent_mode
				order by started_at desc, scan_id desc
			) as row_rank
			from results
		) as ranked
		where row_rank > ?
	)`
