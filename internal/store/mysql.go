package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	// The pure-Go MySQL driver. Imported for its side effect of registering
	// itself, and by name for its error type, which carries the numbers that
	// decide what is worth retrying.
	//
	// This is the one MPL-2.0 dependency in the tree, and the only one, which
	// is a deliberate decision rather than an oversight: it is linked
	// unmodified, which MPL-2.0 §3.3 permits inside an MIT-licensed work.
	// Patching or vendoring it with changes would invalidate that, and the
	// permissively-licensed alternatives cost roughly ten further modules
	// including a complete SQL parser. NFR §9 records the reasoning; read it
	// before replacing this import.
	gomysql "github.com/go-sql-driver/mysql"
)

// mysqlDialect stores results in MySQL.
//
// It needs the most care of the three, because MySQL's defaults disagree with
// the other two in ways that would corrupt evidence quietly rather than fail:
// a case-insensitive collation would make two differently-cased target names
// the same series, TEXT tops out at 64 KiB where a result document does not,
// and a non-strict sql_mode truncates an over-long value instead of refusing
// it. Each of those is pinned below.
type mysqlDialect struct{}

func (mysqlDialect) name() string      { return DriverMySQL }
func (mysqlDialect) sqlDriver() string { return "mysql" }

func (mysqlDialect) dsn(opts Options) (string, error) {
	if !opts.DSN.IsSet() {
		return "", fmt.Errorf("store: the %s driver needs store.dsn", DriverMySQL)
	}

	dsn := opts.DSN.Reveal()

	// Strict mode is required, not preferred: without it MySQL truncates a
	// value that does not fit and reports success, which would shorten a
	// captured URL and change the finding. It is appended rather than
	// demanded of the operator because it is not a matter of taste.
	if !strings.Contains(dsn, "sql_mode") {
		dsn = appendDSNParam(dsn, "sql_mode", "'STRICT_ALL_TABLES'")
	}

	// Timestamps are stored as integers, so the session time zone cannot
	// affect them — but parseTime would still change how any future DATETIME
	// column behaves, so the setting is made explicit rather than inherited.
	if !strings.Contains(dsn, "parseTime") {
		dsn = appendDSNParam(dsn, "parseTime", "false")
	}

	return dsn, nil
}

func appendDSNParam(dsn, key, value string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	return dsn + sep + key + "=" + value
}

func (mysqlDialect) tune(db *sql.DB, opts Options) {
	db.SetMaxOpenConns(opts.maxOpenConns())
	db.SetMaxIdleConns(opts.maxIdleConns())
	// A bounded lifetime matters more here than for PostgreSQL: MySQL closes
	// idle connections itself after wait_timeout, and a pooled connection the
	// server has already dropped surfaces as a failed scan.
	db.SetConnMaxLifetime(opts.connMaxLifetime())
}

func (mysqlDialect) rebind(query string) string { return query }

// ddlIsTransactional is false: MySQL commits implicitly on DDL, so a
// migration cannot be rolled back. That is why every statement is written
// `if not exists` — a retry after a partial failure has to be able to
// continue rather than trip over what already succeeded.
func (mysqlDialect) ddlIsTransactional() bool { return false }

// upsert uses the row-alias form rather than the deprecated VALUES()
// function, which sets the floor at MySQL 8.0.19.
func (mysqlDialect) upsert(_, update []string) string {
	sets := make([]string, 0, len(update))
	for _, col := range update {
		sets = append(sets, col+" = new."+col)
	}

	return " as new on duplicate key update " + strings.Join(sets, ", ")
}

// pruneByCount cannot use the portable form. MySQL refuses to select from the
// table a DELETE targets (error 1093), and its optimizer merges a derived
// table back into the outer query unless something blocks it. A multi-table
// DELETE against a joined derived table is the form that does not depend on
// optimizer behaviour.
func (mysqlDialect) pruneByCount() string {
	return `
		delete r from results r
		join (
			select target, consent_mode, scan_id from (
				select target, consent_mode, scan_id, row_number() over (
					partition by target, consent_mode
					order by started_at desc, scan_id desc
				) as row_rank
				from results
			) as ranked
			where row_rank > ?
		) as doomed
		on  r.target       = doomed.target
		and r.consent_mode = doomed.consent_mode
		and r.scan_id      = doomed.scan_id`
}

// migrations mirror the SQLite schema, with the three MySQL-specific choices
// this dialect exists to make.
func (mysqlDialect) migrations() [][]string {
	return [][]string{
		{
			// varchar rather than text for key columns, because an index key
			// has a length limit; longtext for the document, because TEXT
			// holds 64 KiB and a result of a real page exceeds that.
			//
			// utf8mb4_bin, because MySQL's default collation is
			// case-insensitive and accent-insensitive: under it, targets
			// named "Site" and "site" would be one series, which neither
			// SQLite nor PostgreSQL would do. Evidence must not depend on
			// which database it landed in.
			`create table if not exists results (
				target       varchar(191) not null,
				consent_mode varchar(32)  not null,
				scan_id      varchar(64)  not null,
				started_at   bigint       not null,
				termination  varchar(32)  not null,
				document     longtext     not null,
				primary key (target, consent_mode, scan_id),
				key results_series (target, consent_mode, started_at desc, scan_id desc)
			) engine=InnoDB default charset=utf8mb4 collate=utf8mb4_bin`,

			`create table if not exists baselines (
				target       varchar(191) not null,
				consent_mode varchar(32)  not null,
				scan_id      varchar(64)  not null,
				approved_at  bigint       not null,
				document     longtext     not null,
				primary key (target, consent_mode)
			) engine=InnoDB default charset=utf8mb4 collate=utf8mb4_bin`,

			`create table if not exists audit (
				id       bigint   not null auto_increment primary key,
				at       bigint   not null,
				document longtext not null
			) engine=InnoDB default charset=utf8mb4 collate=utf8mb4_bin`,
		},

		// Version 2: the document reference and the summary columns. The
		// sqlite dialect carries the reasoning for the schema; what is
		// MySQL-specific is the shape of the statement.
		//
		// It is one ALTER TABLE rather than ten, because MySQL has no
		// `add column if not exists` and DDL here is not transactional: ten
		// statements could stop half way and leave a rerun tripping over the
		// columns that did apply. A single ALTER TABLE is atomic in MySQL 8.0
		// — the version this dialect already requires for its row-alias upsert
		// — so it either adds every column or none. The window that remains is
		// losing the version record after the ALTER committed, which the next
		// start meets as a duplicate-column error and alreadyApplied absorbs.
		//
		// varchar for the reference and the digest because their lengths are
		// known and bounded; the collation is the table's own utf8mb4_bin, so
		// a digest cannot match case-insensitively.
		//
		// Every column has a default, scan_error included. MySQL takes no
		// literal default on a long text column, so it gets the expression
		// form — available since 8.0.13, below the 8.0.19 this dialect already
		// requires for its row-alias upsert. Without one, adding a NOT NULL
		// column to a table that already has rows asks MySQL to invent a value
		// for each of them, which is exactly what the STRICT_ALL_TABLES this
		// dialect forces on itself (see dsn) refuses — so the upgrade would
		// fail on precisely the populated store it exists for.
		{
			`alter table results
				add column artifact_ref        varchar(128) not null default '',
				add column document_size       bigint       not null default 0,
				add column document_digest     varchar(64)  not null default '',
				add column duration_ns         bigint       not null default 0,
				add column scan_error          longtext     not null default (''),
				add column consent_outcome     varchar(32)  not null default '',
				add column consent_cmp         varchar(191) not null default '',
				add column requests            int          not null default 0,
				add column third_party_domains int          not null default 0,
				add column pre_consent_domains int          not null default 0`,
		},

		// Version 3 (Story 8.4): the document column goes, once every payload
		// it held is in the bucket. The sqlite dialect carries the reasoning
		// for the migration; what is MySQL's own is that it cannot write
		// `drop column if exists`, so the tolerance for a rerun lives in
		// alreadyApplied instead of in the statement.
		{
			`alter table results drop column document`,
		},
	}
}

// The two errors that mean "this statement's work is already done" for the
// statements this dialect cannot write conditionally.
const (
	// errCantDropField is MySQL's answer to dropping a column that is not
	// there, which migration 3 asks for.
	errCantDropField = 1091

	// errDupFieldName is its answer to adding one that is, which migration 2
	// asks for. Adding a column that already exists means the ALTER committed
	// and only the version record was lost — the same accident as the drop,
	// met one migration earlier.
	errDupFieldName = 1060
)

// alreadyApplied recognises a schema change this store has already made.
//
// MySQL commits DDL implicitly, so a migration can succeed and then lose the
// record that it did — a connection dropped between the ALTER and the version
// row. Without this, the next start would meet 1091 or 1060 and refuse to open
// a store whose schema is in fact correct, which is a store an operator cannot
// get back without hand-editing a version number.
//
// Both numbers are narrow: neither can be produced by a statement that failed
// to do its work, so accepting them skips nothing that still needs doing.
func (mysqlDialect) alreadyApplied(err error) bool {
	var myErr *gomysql.MySQLError

	if !errors.As(err, &myErr) {
		return false
	}

	return myErr.Number == errCantDropField || myErr.Number == errDupFieldName
}

func (mysqlDialect) hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	return countColumn(ctx, db, `
		select count(*) from information_schema.columns
		where table_schema = database() and table_name = ? and column_name = ?`,
		table, column)
}

// documentByteLength is MySQL's length(), which already counts bytes.
func (mysqlDialect) documentByteLength() string {
	return "length(" + documentColumn + ")"
}

func (mysqlDialect) schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	if _, err := db.ExecContext(ctx, schemaVersionTable+
		` engine=InnoDB default charset=utf8mb4 collate=utf8mb4_bin`); err != nil {
		return 0, err
	}

	var version int

	err := db.QueryRowContext(ctx, "select version from wsaw_schema_version where id = 1").Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}

	return version, nil
}

func (mysqlDialect) setSchemaVersion(ctx context.Context, ex execer, version int) error {
	_, err := ex.ExecContext(ctx, `
		insert into wsaw_schema_version (id, version) values (1, ?) as new
		on duplicate key update version = new.version`, version)

	return err
}

// transientMySQLErrors are the numbers worth another attempt: a lost or
// refused connection, a server going away, a deadlock or a lock-wait timeout.
var transientMySQLErrors = map[uint16]bool{
	1040: true, // too many connections
	1042: true, // can't get hostname
	1043: true, // bad handshake
	1053: true, // server shutdown in progress
	1205: true, // lock wait timeout exceeded
	1213: true, // deadlock found
	1290: true, // running with --read-only (a failover in progress)
	2002: true, // can't connect to local server
	2003: true, // can't connect to server
	2006: true, // server has gone away
	2013: true, // lost connection during query
}

func (mysqlDialect) isTransient(err error) bool {
	if err == nil {
		return false
	}

	var myErr *gomysql.MySQLError
	if errors.As(err, &myErr) {
		return transientMySQLErrors[myErr.Number]
	}

	return isTransientMessage(err)
}

func (mysqlDialect) afterOpen(Options) error { return nil }
