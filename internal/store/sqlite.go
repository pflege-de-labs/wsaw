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

		// Version 2 (Stories 8.2 and 8.3): the document moves to the artifact
		// bucket, and the summary a listing needs becomes columns.
		//
		// A row now carries where the document is, how big it is and what it
		// must hash to, so a reader can tell a pruned document from a corrupt
		// one without trusting the bucket (Story 8.2, AC2 and AC6). It also
		// carries the summary, because a listing that had to fetch fifty
		// documents to render fifty lines would be fifty round trips to object
		// storage (Story 8.3, AC2).
		//
		// The document column is deliberately left in place and left NOT NULL.
		// Rows written from here on put the empty string in it — never a
		// second copy of the payload, which is the failure Story 8.2 exists to
		// avoid — and the rows an older wsaw wrote keep theirs, because that is
		// what the data migration reads when it moves them into the bucket and
		// then drops the column (Story 8.4, AC1). Relaxing the column to NULL
		// instead would cost SQLite a full table rebuild here, which is exactly
		// the bulk data movement that story owns.
		//
		// Every column has a default so that it can be added to a table that
		// already has rows. Nothing this build writes relies on the default:
		// a row wsaw writes sets all of them, and an artifact_ref that is still
		// empty marks a row whose document has not been moved yet, which the
		// read paths report as such rather than presenting a summary of zeroes.
		{
			`alter table results add column artifact_ref        text    not null default ''`,
			`alter table results add column document_size       integer not null default 0`,
			`alter table results add column document_digest     text    not null default ''`,
			`alter table results add column duration_ns         integer not null default 0`,
			`alter table results add column scan_error          text    not null default ''`,
			`alter table results add column consent_outcome     text    not null default ''`,
			`alter table results add column consent_cmp         text    not null default ''`,
			`alter table results add column requests            integer not null default 0`,
			`alter table results add column third_party_domains integer not null default 0`,
			`alter table results add column pre_consent_domains integer not null default 0`,
		},

		// Version 3 (Story 8.4): the document column goes, once every payload
		// it held is in the bucket.
		//
		// The move itself is not a statement, because it is not something SQL
		// can express: it is a bucket write and a row update per row, in
		// bounded batches, and it happens in prepareMigration before this runs
		// (Story 8.4, AC1 and AC3). By the time the column is dropped, every
		// document it held has been written to the bucket and referenced from
		// its own row — and if any row could not be moved, the migration stops
		// before this statement rather than dropping evidence it failed to
		// copy (AC4).
		//
		// SQLite rewrites the table for a dropped column, which is the one
		// expensive part of this upgrade and the reason a backup taken
		// beforehand is the documented rollback: forward is the only direction
		// (AC8).
		{
			`alter table results drop column document`,
		},

		// Version 4 (Story 8.5): which artifacts each result names becomes a
		// table, so retention can work out what a deletion orphans without
		// reading a single document back out of the bucket (AC2).
		//
		// A row per (result, artifact) rather than a list column, because the
		// question retention asks is "does any surviving result still name
		// this key?" — an indexed equality lookup against one column, not a
		// substring search through a list nothing can index. Artifacts are
		// content-addressed, so two scans that captured identical bytes share
		// one object and the same reference appears under both of them; that
		// sharing is the whole reason deletion has to be decided by reference
		// rather than by age (AC1).
		//
		// The document's own reference is recorded here too, even though the
		// results row already carries it in artifact_ref. The duplication is
		// deliberate: it makes one table the complete answer to "what does
		// this result name", so the sweep is one query per page of bucket keys
		// instead of two against tables with different shapes. Both are
		// written from the same list in one transaction, so they cannot drift.
		//
		// There is deliberately no foreign key to results. A row here is meant
		// to outlive the result it belonged to: after retention deletes the
		// result, the rows left behind are precisely the list of artifacts
		// that may now be unreferenced, which is what makes an interrupted
		// prune resumable instead of a silent leak (AC3, AC4). A cascade would
		// delete exactly the work list.
		{
			`create table if not exists result_artifacts (
				target       text not null,
				consent_mode text not null,
				scan_id      text not null,
				artifact_ref text not null,
				primary key (target, consent_mode, scan_id, artifact_ref)
			) strict`,

			// The index the reference check is: without it, deciding whether
			// one key is still referenced would scan the table once per
			// artifact considered for deletion.
			`create index if not exists result_artifacts_ref
				on result_artifacts (artifact_ref)`,

			`alter table results add column refs_indexed integer not null default 0`,
		},

		// Version 5 (Story 8.5): the two things version 4 left retention
		// unable to decide.
		//
		// The first is what an in-flight scan has taken. Artifacts are
		// content-addressed, so a scan that captures an unchanged asset stores
		// nothing — the key is already there — and it holds no reference to it
		// until its row commits, minutes later. Between those two moments the
		// only evidence that anyone wants those bytes is this table: the write
		// records a claim, the result that lands releases it, and retention
		// treats a claim younger than the grace period as a reference it
		// cannot see yet (AC3). The object's own modification time cannot
		// answer that question, because a deduplicated write does not change
		// it — it is still the time of the first scan that stored those bytes,
		// possibly months ago.
		//
		// The second is every existing row's own document reference. Version 4
		// created the reference table empty and left it to be filled by a
		// backfill that reads each document out of the bucket, which is
		// best-effort by design: a store the backfill has not reached yet has
		// no row here at all, and a sweep would then find the document of a
		// live result unreferenced and collect it. Where a document lives is
		// the one reference a row already knows, in artifact_ref, so it is
		// recorded in SQL — before any bucket read, for every row, in one
		// statement — and the sweep's exemption for result documents becomes
		// true rather than nearly true (Tenet 5).
		{
			`create table if not exists artifact_claims (
				artifact_ref text    not null primary key,
				claimed_at   integer not null
			) strict`,

			// Duplicates are ignored rather than prevented: a row this wsaw
			// wrote already has its document reference here, and a rerun after
			// a lost version record must be a no-op.
			`insert into result_artifacts (target, consent_mode, scan_id, artifact_ref)
				select target, consent_mode, scan_id, artifact_ref
				  from results
				 where artifact_ref <> ''
				    on conflict do nothing`,
		},
	}
}

// alreadyApplied is always false: SQLite's DDL is transactional here, so a
// statement either applied with its version record or did neither.
func (sqliteDialect) alreadyApplied(error) bool { return false }

// hasColumn reads SQLite's own table description. pragma_table_info is the
// table-valued form of the pragma, so the table name is a parameter rather
// than something interpolated into a statement.
func (sqliteDialect) hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	return countColumn(ctx, db,
		`select count(*) from pragma_table_info(?) where name = ?`, table, column)
}

// documentByteLength casts to a blob first: SQLite's length() counts
// characters for text, and a document of multi-byte characters would be
// reported smaller than the bucket will have to hold.
func (sqliteDialect) documentByteLength() string {
	return "length(cast(" + documentColumn + " as blob))"
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
