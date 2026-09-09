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
	// DriverBlob keeps the index as objects in the same bucket as the
	// evidence, so a deployment needs no database at all (Story 8.10). It is
	// the one driver in this list that is not a SQL dialect, which is why
	// dialectFor refuses it by name.
	DriverBlob = "blob"
)

// Drivers lists the supported drivers, for configuration validation and
// error messages.
//
// SQLite is first because it is the default, and blob is last because it is
// the one that gives up a database rather than choosing a different one.
func Drivers() []string { return []string{DriverSQLite, DriverPostgres, DriverMySQL, DriverBlob} }

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
	// alreadyApplied reports whether a schema statement failed because what
	// it asks for is already true. It exists for the dialect that cannot say
	// so in SQL: SQLite and PostgreSQL write `if exists`, MySQL has no such
	// form for dropping a column, and a migration it has already applied must
	// not become an error the next start cannot get past.
	alreadyApplied(err error) bool
	// hasColumn reports whether a table still carries a column. The document
	// migration asks before reading the column it is about to drop, because a
	// dialect whose DDL commits outside the transaction can have dropped it
	// and lost the version record (Story 8.4, AC2).
	hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error)
	// documentByteLength is the SQL expression for a stored document's size in
	// bytes. length() counts characters in SQLite and PostgreSQL and bytes in
	// MySQL, and a dry run that reported a number an operator cannot size a
	// bucket with would be worse than reporting none (Story 8.4, AC6).
	documentByteLength() string
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
	case DriverBlob:
		// Refused by name rather than falling through to "unknown driver",
		// because blob is a configured driver and the reason it has no dialect
		// is worth stating: it keeps no schema, so the commands that ask a
		// store about one — the document migration and its dry run — have
		// nothing here to report.
		//
		// The second sentence is in the message and not only in this comment
		// because of who reads it: an operator moving a deployment from
		// Postgres to the bucket runs the migration command first, and being
		// told there is no schema without being told what to do instead leaves
		// them at a dead end (AC16). It names no command, because the one that
		// does the work is Story 8.11's and a message that names a command
		// this binary does not have would be worse than a message that names
		// none.
		return nil, fmt.Errorf(
			"store: the %s driver keeps its index as objects in the artifact bucket rather than as rows, "+
				"so it has no SQL schema to migrate; moving a history between a database store and this "+
				"one is not a migration in either direction, because the documents are already in the "+
				"same bucket layout and the index is rebuilt from them rather than converted", DriverBlob,
		)
	default:
		return nil, fmt.Errorf(
			"store: unknown driver %q; supported drivers are %s",
			name, strings.Join(Drivers(), ", "),
		)
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
//
// resultUpdate is also what the insert itself is built from (resultInsert), so
// a column added here is written, overwritten, and bound in one place rather
// than in three that can disagree.
//
// It is split in two because the document migration writes one half and not
// the other (Story 8.4). resultScanned is what the scan itself recorded and
// what every listing has been ordered by since; resultDerived is everything
// that can be recomputed from the document, which is exactly what a migration
// deriving a summary from a moved document is entitled to overwrite.
// resultsTable and documentColumn are named rather than written inline
// because the document migration asks the database about them — whether the
// column is still there — as well as writing SQL that mentions them.
const (
	resultsTable   = "results"
	documentColumn = "document"

	// resultArtifactsTable records which artifacts each result names, so
	// retention can decide what to delete with a query rather than by reading
	// every document back out of the bucket (Story 8.5, AC2).
	resultArtifactsTable = "result_artifacts"

	// artifactClaimsTable records the artifacts a scan has stored but not
	// referenced yet, which is the only thing that tells a deduplicated write
	// from an object nobody wants (see claims.go).
	artifactClaimsTable = "artifact_claims"

	// refsIndexedColumn says whether that record is complete for a row. It is
	// on the results table rather than inferred from the presence of rows in
	// resultArtifactsTable, because "this result names no screenshots" and
	// "nobody has worked out what this result names" are different facts, and
	// only the second one makes deleting an artifact unsafe.
	refsIndexedColumn = "refs_indexed"
)

// The states refsIndexedColumn holds.
//
// They are separated because they call for different behaviour and because
// retrying costs a bucket read per row. An older wsaw's row starts at
// refsUnknown and the backfill works through them; a row whose document is
// gone or will not decode is marked refsUnderivable so that the next start
// does not read it again, and neither state lets a sweep conclude that a
// screenshot or a body is unreferenced.
const (
	refsUnknown     = 0
	refsRecorded    = 1
	refsUnderivable = 2
)

var (
	resultKey = []string{"target", "consent_mode", "scan_id"}

	// resultScanned belongs to the row rather than to the document: the
	// instant the scan started and how it ended.
	resultScanned = []string{"started_at", "termination"}

	// resultDerived is everything a row carries that comes out of the
	// document: where the document is and what it must hash to (Story 8.2,
	// AC2), and the summary, materialised so that a listing needs no document
	// at all (Story 8.3, AC1).
	resultDerived = []string{
		"artifact_ref", "document_size", "document_digest",
		"duration_ns", "scan_error", "consent_outcome", "consent_cmp",
		"requests", "third_party_domains", "pre_consent_domains",
	}

	// resultReferenced is written by a scan's own insert and by nothing else.
	// The document migration deliberately leaves it at its default, because
	// moving a document into the bucket does not tell anyone which screenshots
	// and bodies that document names — working that out is the backfill's job
	// (Story 8.5, AC2).
	resultReferenced = []string{refsIndexedColumn}

	resultUpdate = append(append(append([]string{}, resultScanned...), resultDerived...), resultReferenced...)

	baselineKey    = []string{"target", "consent_mode"}
	baselineUpdate = []string{"scan_id", "approved_at", "document"}
)

// countColumn runs a catalogue query that answers "is this column there?" as
// a count, which every dialect can express and none of them can get subtly
// wrong the way a row-or-no-row scan can.
func countColumn(ctx context.Context, db *sql.DB, query string, args ...any) (bool, error) {
	var n int

	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return false, err
	}

	return n > 0, nil
}

// rankedResults numbers every result within its series, newest first, by the
// same ordering every listing uses — so "the newest N" means the same thing to
// retention as it does to the interface.
//
// It is one constant because three statements are built from it: the portable
// count-based delete, MySQL's own, and the select a dry run lists the doomed
// results with (Story 8.5, AC6). Written out three times, a dry run could
// eventually report something other than what the prune would remove.
const rankedResults = `
		select target, consent_mode, scan_id, started_at, row_number() over (
			partition by target, consent_mode
			order by started_at desc, scan_id desc
		) as row_rank
		from results`

// expiredByCount selects the results a count-based retention would remove.
const expiredByCount = `
	select target, consent_mode, scan_id, started_at from (` + rankedResults + `
	) as ranked
	where row_rank > ?`

// expiredByAgePredicate is what "too old to keep" means. It is one constant
// because two statements are built from it — the delete a prune runs and the
// select a dry run lists — and a boundary edited in one of two copies would
// leave the dry run describing a different set from the prune it claims to
// describe (Story 8.5, AC6).
const expiredByAgePredicate = ` where started_at < ?`

// expiredByAge selects the results an age-based retention would remove.
const expiredByAge = `
	select target, consent_mode, scan_id, started_at from ` + resultsTable + expiredByAgePredicate

// pruneByCountPortable deletes all but the newest keep results per series,
// identified by primary key rather than by any physical row identifier.
//
// SQLite has rowid and PostgreSQL has ctid, but neither is the same concept
// and MySQL has neither. The primary key is the identity the schema already
// declares, so ranking on it works everywhere and needs no dialect at all.
const pruneByCountPortable = `
	delete from results where (target, consent_mode, scan_id) in (
		select target, consent_mode, scan_id from (` + rankedResults + `
		) as ranked
		where row_rank > ?
	)`
