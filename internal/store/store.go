// Package store persists scan results, baselines, and evidence artifacts.
//
// What wsaw asks of a store is an interface, Store, declared in api.go
// (Tenet 12); how a store keeps its index is its own business. SQL is that
// interface implemented over database/sql, which makes the dialect the seam
// between databases rather than three implementations of everything: SQLite is
// the default, through a CGo-free driver so the single static binary still
// cross-compiles to every supported platform (Tenet 14), and PostgreSQL and
// MySQL are options for a deployment that already runs one (Story 4.7). What
// differs between the three is confined to dialect.go — the queries, the
// transactions and the decoding are one implementation.
//
// A result is stored as its JSON document in the artifact bucket, plus a row
// that references it and carries the few fields needed to index and list it
// (Story 8.2). The document is deliberately not shredded into normalised
// tables: the JSON schema is the product's interface (Tenet 16), and a second
// schema would drift from it. It is equally deliberately not a column: nothing
// in wsaw queries its contents, and a multi-megabyte payload on every row makes
// every backup and every replication stream pay for evidence nobody asked for.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrCorrupt marks stored evidence that is not what was written: a document
// whose bytes no longer match the digest recorded with them, or that no longer
// decodes.
//
// It is separate from ErrNotFound because the two call for different answers.
// Evidence that has been pruned is expected and is reported as absent (Story
// 5.17, AC3); evidence that is present and wrong is a fault in the bucket or in
// whatever else has been writing to it, and must never be returned as though it
// were the scan (Story 8.2, AC6).
var ErrCorrupt = errors.New("stored evidence is corrupt")

// ErrEvidenceGone marks a result whose row is present and whose document is
// not in the bucket any more.
//
// It is deliberately not ErrNotFound. While the document lived in the row the
// two could not be told apart, because a row without a document was a row that
// did not exist — but once the payload is in a bucket, "there was no such scan"
// and "the scan is indexed and its evidence has been deleted behind wsaw's
// back" are different facts, and the callers that skip a missing previous
// result would otherwise skip lost evidence just as quietly. A scan compared
// against nothing then reads exactly like the first scan of a site, which is
// the failed observation dressed as a clean result that Tenet 5 and Story 8.2,
// AC5 exist to prevent.
//
// It does not wrap ErrNotFound for that same reason: an errors.Is(err,
// ErrNotFound) that matched it would restore the silence.
var ErrEvidenceGone = errors.New("the stored evidence this result names is no longer in the artifact bucket")

// ErrIndexIncomplete marks a read the index could not finish answering: an
// object the index itself listed and then could not produce, or a fold that ran
// out of budget part way through.
//
// It exists for the store whose index is objects in a bucket, where "the
// history does not contain this" and "I could not see all of the history" are
// different facts that a single listing can produce either of (Story 8.10,
// AC6). The SQL stores cannot reach it: a query either answers or fails.
//
// It wraps neither ErrNotFound nor ErrEvidenceGone, so no caller reads it as
// "there is no previous scan" and none reads it as an evidence loss. What it
// buys today is a log line at Warn and a truthful HTTP status. It does not yet
// change the comparison a scanner produces — scanner.compare treats every error
// but ErrEvidenceGone the same way, and giving it a third answer is a change in
// internal/scanner and internal/diff that belongs with them.
var ErrIndexIncomplete = errors.New("the index could not be read completely")

// SQL is a result store whose index is rows in a database — SQLite,
// PostgreSQL or MySQL, behind one set of queries (dialect.go). It is safe
// for concurrent use.
//
// It is named for how it keeps its index rather than for being "the store",
// because what a store has to do and how it records what it has done are
// different questions: the evidence itself has lived in a bucket since Story
// 8.2, and only the index is SQL.
type SQL struct {
	db *sql.DB
	d  dialect

	// retrier is the store's retry policy. A server database is reachable over
	// a network, so a dropped connection is an ordinary event rather than a
	// catastrophe (Story 4.7, AC7). Which failures are worth another attempt is
	// the dialect's judgement, so the policy is handed its predicate.
	retrier retrier

	// bucket holds the evidence: screenshots, stored bodies, and since Story
	// 8.2 the result documents themselves. It is the store's only route to
	// them, so where evidence lives — a local directory, or object storage —
	// is a URL rather than a second implementation of everything.
	bucket *bucket

	// log reports the one thing a store does that an operator has to be able
	// to watch: the migration that moves stored documents into the bucket
	// (Story 8.4, AC3). Everything else a store does is one short statement.
	log *slog.Logger

	// documents records what that migration did, so the command an operator
	// runs it from can report it rather than reconstruct it from the log.
	// Nil when this open applied no such migration.
	documents *DocumentMigration
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

	// ArtifactDir names where the evidence goes: screenshots, stored bodies,
	// and every result document. A path is a directory on local disk and
	// anything with a scheme is a bucket URL, which is the one place that
	// distinction is not made — configuration keeps the two apart as
	// store.artifactDir and store.artifactURL and resolves them to this single
	// location (Story 8.6, AC1). It is required, because a store that has
	// nowhere to put a document cannot record a scan.
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

	// Logger receives the store's own progress reporting. Opening a store is
	// otherwise silent, and deliberately so — but the migration that moves
	// every stored document into the artifact bucket can run for minutes on a
	// large store, and an operator watching an upgrade needs to see it advance
	// rather than guess whether it has hung (Story 8.4, AC3). Empty takes
	// slog.Default().
	Logger *slog.Logger

	// Version identifies the build, for the one object a store writes that
	// records who created it: the layout marker of the bucket index (Story
	// 8.10). It is provenance rather than behaviour — nothing reads it back —
	// and it exists because an operator looking at a bucket with no database
	// beside it has nothing else to tell them which wsaw laid it out. Empty
	// records that the build did not say.
	Version string

	// Now is the clock the store reads. It exists for the store whose index is
	// in the bucket, where a timestamp is not decoration: it orders every key
	// and it decides every grace period, so a test of ordering or of retention
	// that could not move the clock would have to wait for one (AGENTS §5).
	// Nil takes time.Now.
	//
	// The SQL stores ignore it. Their ordering is a column the database sorts
	// and their retention takes its "now" as an argument.
	Now func() time.Time

	// CheckpointGrace is how long a compaction checkpoint must have been
	// visible in the bucket before the loose index entries it covers may be
	// deleted (Story 8.10). It is here rather than in configuration for the
	// reason every other constant of that design is a constant — an operator
	// has no basis on which to tune it — and it is here at all because a test
	// of the rule cannot otherwise reach the far side of a day without
	// sleeping through one. Zero takes the default.
	//
	// **Nothing reads it yet.** The rule it feeds belongs to compaction, which
	// is the step of Story 8.10 that follows the index itself, so setting this
	// today changes no behaviour. It is declared with the store it belongs to
	// rather than with its consumer so that the option set an operator and a
	// test see does not change shape when compaction lands; the deviation is
	// recorded in the design's §7.6.
	CheckpointGrace time.Duration
}

// Location names the store the way a log line or a command's output should:
// the database file for SQLite, the driver and the endpoint for a server.
//
// It is exported because whoever resolves these options is also whoever prints
// them, and a DSN printed as configured would print a password with it (Story
// 4.7, AC6).
func (o *Options) Location() string { return o.describe() }

// ArtifactLocation names the artifact bucket the way a log line or a command's
// output should.
//
// It lives beside Location because a bucket URL can carry credentials in its
// userinfo — an S3-compatible endpoint pointed at MinIO is the ordinary case —
// and a printer reaching for the raw string would put them in a log. Keeping
// the redaction with the value is what stops the next printer having to
// remember (AGENTS §3.3).
func (o *Options) ArtifactLocation() string { return secret.RedactURL(o.ArtifactDir) }

// describe names the store for a log line or an error, without revealing a
// DSN's credentials.
func (o *Options) describe() string {
	if o.Driver == "" || o.Driver == DriverSQLite {
		return o.Path
	}

	if o.Driver == DriverBlob {
		// There is no second location to name: the index is objects in the
		// same bucket as the evidence, so the bucket is the store. The driver
		// is kept in front of it so a log line still says which kind of store
		// this is rather than printing the artifact location twice with no
		// explanation.
		return DriverBlob + " " + o.ArtifactLocation()
	}

	if o.DSN.IsSet() {
		return o.Driver + " " + secret.RedactURL(o.DSN.Reveal())
	}

	return o.Driver
}

// OpenSQL creates or opens a SQL store, bringing its schema up to date.
//
// The schema work is deliberately the second half: connect() has already
// reached the database and the artifact bucket by then, so a migration that
// has to move documents into the bucket (Story 8.4) fails against an
// unreachable one before it has changed a single row.
//
// It takes a context because that schema work is the one part of opening a
// store that is not instantaneous: migration 3 writes one artifact per stored
// result, and a hundred thousand of them is minutes. Every statement and every
// bucket call inside it carries its own deadline, so nothing waits for ever —
// but a deadline is not a cancellation, and an operator pressing Ctrl-C, or a
// service manager sending SIGTERM during a slow start, has to be able to stop
// the run rather than watch it go on uploading (Story 8.4, AC3).
func OpenSQL(ctx context.Context, opts Options) (*SQL, error) {
	s, err := connect(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Derived from the caller's context rather than manufactured from
	// Background: the per-operation deadlines bound each step, and the caller's
	// cancellation is what bounds the whole run.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.migrate(ctx); err != nil {
		s.closeAfterFailedOpen()

		return nil, err
	}

	if err := s.d.afterOpen(opts); err != nil {
		s.closeAfterFailedOpen()

		return nil, err
	}

	return s, nil
}

// connect performs every part of opening a store except its schema: the
// dialect, the directories, the connection, the bucket and the retry policy.
//
// It is separate because describing a pending migration without applying it
// (PlanDocumentMigration) needs exactly this much and must touch the schema no
// more than a reader would — a dry run that migrated on the way to reporting
// what a migration would do would be no dry run at all.
//
// For SQLite, parent directories are created with restrictive permissions,
// since results can contain personal data (NFR §4).
func connect(ctx context.Context, opts Options) (*SQL, error) {
	d, err := dialectFor(opts.Driver)
	if err != nil {
		return nil, err
	}

	if opts.ArtifactDir == "" {
		// Refused here rather than defaulted, and refused at startup rather
		// than on the first scan (Tenet 15). Resolving "somewhere sensible
		// beside the database" is the configuration layer's job, which knows
		// where the state directory is; a store that guessed would guess the
		// process's working directory, and retention would later sweep it.
		return nil, errors.New(
			"store: an artifact location is required: a scan's result document is stored there. " +
				"Set store.artifactDir for a directory on local disk, or store.artifactURL for a bucket",
		)
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

	ctx, cancel := context.WithTimeout(ctx, opts.timeout()+5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("opening store %s: %w", opts.describe(), err)
	}

	s := &SQL{
		db: db,
		d:  d,
		retrier: retrier{
			attempts:  opts.maxAttempts(),
			backoff:   opts.retryBackoff(),
			onRetry:   opts.OnRetry,
			transient: d.isTransient,
		},
		log: opts.logger(),
	}

	// Opened before the schema is touched, for the reason OpenSQL gives.
	b, err := openBucket(ctx, opts.ArtifactDir)
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	// The bucket runs on the store's retry policy rather than one of its own,
	// so `maxAttempts` and `retryBackoff` mean the same thing for evidence as
	// they do for rows (Story 8.1, AC8).
	b.setRetry(s.retry)
	s.bucket = b

	return s, nil
}

// closeAfterFailedOpen releases what OpenSQL has already acquired. The close
// errors are dropped on purpose: the caller is about to be told why the store
// could not be opened, and a failure to tidy up would only obscure it.
func (s *SQL) closeAfterFailedOpen() {
	_ = s.bucket.close()
	_ = s.db.Close()
}

// Driver reports which database this store is using, for logs and for the
// tests that must run against every dialect.
func (s *SQL) Driver() string { return s.d.name() }

// retry runs one database operation under this store's policy, and is also
// what the artifact bucket borrows, so evidence and rows are retried by the
// same rules (Story 8.1, AC8). The policy itself is in shared.go: it is not a
// property of SQL.
func (s *SQL) retry(ctx context.Context, op string, fn func(context.Context) error) error {
	return s.retrier.run(ctx, op, fn)
}

// q rewrites a query written with ? placeholders into the dialect's form.
// Every query in this package is written once, with ?, and passed through
// here — so a query cannot be correct for one database and malformed for
// another.
func (s *SQL) q(query string) string { return s.d.rebind(query) }

// migrate brings the schema up to date, and refuses to run against a newer
// one. A binary that does not understand the schema must not write to it.
//
// The mechanism is identical for every dialect: the same numbered migrations,
// applied in the same order, recorded wherever that dialect records them.
func (s *SQL) migrate(ctx context.Context) error {
	migrations := s.d.migrations()

	current, err := s.d.schemaVersion(ctx, s.db)
	if err != nil {
		return fmt.Errorf("reading the store's schema version: %w", err)
	}

	if current > len(migrations) {
		return fmt.Errorf(
			"the store was written by a newer wsaw (schema version %d, this build understands %d); upgrade wsaw or point it at a different store",
			current, len(migrations),
		)
	}

	for v := current; v < len(migrations); v++ {
		version := v + 1

		if err := s.prepareMigration(ctx, version); err != nil {
			return err
		}

		if err := s.applyMigration(ctx, migrations[v], version); err != nil {
			return err
		}
	}

	// The one piece of schema work that is not a statement and does not belong
	// to a single version: recording which artifacts each stored result names
	// (Story 8.5). Migration 4 creates the table, but filling it means reading
	// documents out of the bucket, so it is finished here — and attempted again
	// on every start, because a store whose references are incomplete is a
	// store retention cannot safely reclaim anything from. It reports its own
	// per-row failures rather than returning them.
	return s.indexArtifactReferences(ctx)
}

// prepareMigration runs the part of a numbered migration that SQL cannot
// express, before that version's statements are applied.
//
// Only one version has such a part, and it is the reason this hook exists:
// version 3 moves every stored document into the artifact bucket, which is a
// network write per row, and the statement that follows it drops the column
// those documents live in. The data therefore has to be safely elsewhere
// before the DDL runs — and it cannot run inside the DDL's transaction, which
// would hold a schema lock open across a round trip to object storage and
// could not roll the objects back anyway (Story 8.4, AC1 and AC3).
func (s *SQL) prepareMigration(ctx context.Context, version int) error {
	if version != schemaDocumentsInBucket {
		return nil
	}

	return s.moveDocumentsToBucket(ctx)
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
func (s *SQL) applyMigration(ctx context.Context, statements []string, version int) error {
	if !s.d.ddlIsTransactional() {
		for _, stmt := range statements {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				if s.d.alreadyApplied(err) {
					// The statement's work is already done — this dialect
					// committed it on a previous run and then lost the version
					// record. Continuing is what makes that recoverable
					// without hand-editing the schema version.
					s.log.Info("schema migration statement was already applied",
						"version", version, "error", err)

					continue
				}

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

// Ping reports whether the store is reachable — both halves of it.
//
// For SQLite the database half is nearly free and nearly always true. For a
// server database it is the difference between a wsaw that is working and one
// that is scanning into a void, which readiness has to be able to tell apart:
// a watcher that cannot record what it saw is not watching (Tenet 8).
//
// The artifact bucket is checked for the same reason and through the same
// call, because since Story 8.2 it holds the result document itself: a
// reachable database with an unreachable bucket cannot store a scan either,
// and readiness that only asked about rows would report a wsaw as ready when
// every scan it ran was about to fail (Story 8.6, AC5). One call rather than
// two, so no caller can check one half and forget the other.
// Both halves are wrapped in a sentinel naming which half it was. Readiness is
// served on an endpoint that has no authentication when no API token is
// configured, so what goes in the response body has to be a reason class while
// the detail — which names the bucket, or the directory an unmounted volume
// used to be — goes to the log (Tenet 19).
func (s *SQL) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: the %s store: %w", ErrDatabaseUnreachable, s.d.name(), err)
	}

	if err := s.bucket.reachable(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrBucketUnreachable, err)
	}

	return nil
}

// The two halves of a store that readiness has to tell apart, and the only
// part of a failed Ping that is safe to publish unauthenticated.
//
// They are sentinels rather than strings so a caller matches them with
// errors.Is instead of reading a message, and exported because the reason a
// readiness endpoint gives is a public interface (Story 8.6, AC5).
var (
	// ErrDatabaseUnreachable marks a store whose database did not answer.
	ErrDatabaseUnreachable = errors.New("the result database is not reachable")

	// ErrBucketUnreachable marks a store whose artifact bucket did not answer.
	// A wsaw in that state can no more record a scan than one with no
	// database: since Story 8.2 the result document itself lives there.
	ErrBucketUnreachable = errors.New("the artifact bucket is not reachable")
)

// ProbeArtifactBucket writes a small object to the artifact bucket and removes
// it again.
//
// It is what turns "the bucket is misconfigured" from a scan failure hours
// later into a startup failure naming the bucket (Story 8.6, AC4, Tenet 15).
// Reachability is not enough to establish: a bucket that lists happily and
// refuses writes — a read-only policy, an expired credential with read scope,
// a full disk — looks healthy to every check short of a write.
//
// The write costs one request and one delete per start, which is the price of
// finding out now rather than after a scan has been run and cannot be stored.
func (s *SQL) ProbeArtifactBucket(ctx context.Context) error {
	return s.bucket.probeWritable(ctx)
}

// Close releases the database and the artifact bucket.
//
// Both are released even when the first fails: leaving the bucket open because
// the database would not close leaks exactly the handles this method exists to
// give back.
func (s *SQL) Close() error {
	bucketErr := s.bucket.close()

	if err := s.db.Close(); err != nil {
		return errors.Join(fmt.Errorf("closing store: %w", err), bucketErr)
	}

	return bucketErr
}

// artifactKindResult is the bucket prefix result documents are stored under,
// beside the screenshots and bodies the same scan produced.
const artifactKindResult = "result"

// documentMovedToBucket is what the migration leaves in a row's document
// column once its payload is in the bucket, for the moment between that row
// being moved and the column being dropped. The empty string cannot be
// mistaken for a stored document, since every document an older wsaw wrote is
// JSON — which is what makes it the marker the migration selects the rows it
// still has to move by, and what makes an interrupted run resumable to the row
// (Story 8.4, AC2).
const documentMovedToBucket = ""

// refColumns are the columns a resultRef is read from, in scan order.
const refColumns = `artifact_ref, document_size, document_digest`

// PutResult stores a result, replacing any earlier record of the same scan.
//
// The document goes to the bucket first and the row second (Story 8.2, AC4).
// The reverse order looks equally reasonable until it is interrupted: a row
// written before its document points at nothing, and a reader cannot tell that
// from evidence somebody deleted, so the result reads as corrupt. Interrupting
// this order instead leaves an artifact nobody references, which costs storage
// until the sweep collects it (Story 8.5) and misleads no one. The write is
// also idempotent in both halves — the artifact is content-addressed and the
// row is an upsert — so a retried PutResult produces one result, not two.
//
// The bucket write is deliberately not inside a database transaction (AC7).
// A transaction that spans it would hold a database lock open across a network
// round trip to object storage, and it could not roll the object back anyway:
// what is written to a bucket stays written.
func (s *SQL) PutResult(res *model.Result) error {
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

	ctx, cancel := opCtx()
	defer cancel()

	ref, err := s.putDocument(ctx, document)
	if err != nil {
		return fmt.Errorf("storing result %s: %w", res.ScanID, err)
	}

	// The summary columns come from the same summarize() the interface renders
	// a listing with, so a listed summary and a computed one cannot disagree
	// (Story 8.3, AC4).
	args := resultArgs(summarize(res), ref)

	// Which artifacts this result names, worked out here where the decoded
	// result is in hand, so retention never has to read a document back out of
	// the bucket to find out (Story 8.5, AC2).
	key := resultRowKey{target: res.Target, mode: string(res.ConsentMode), scanID: res.ScanID}
	refs := artifactRefsOf(res, ref.ref)

	err = s.retry(ctx, "storing a result", func(ctx context.Context) error {
		return s.putResultTx(ctx, key, args, refs)
	})
	if err != nil {
		return fmt.Errorf("storing result %s: %w", res.ScanID, err)
	}

	return nil
}

// putDocument writes the document to the bucket and describes it for the row.
//
// The digest is computed here rather than taken from the reference the bucket
// returns. They agree today, because artifacts are content-addressed, but the
// key layout is the bucket's business: the row records what the document must
// hash to, independently of how it is addressed.
func (s *SQL) putDocument(ctx context.Context, document []byte) (resultRef, error) {
	digest := documentDigest(document)

	ref, err := s.bucket.put(ctx, artifactKindResult, document)
	if err != nil {
		return resultRef{}, err
	}

	return resultRef{
		ref:    ref,
		size:   int64(len(document)),
		digest: digest,
	}, nil
}

// resultInsert builds the insert from the same column lists the upsert
// overwrites, so a column cannot be added to one and forgotten in the other.
func resultInsert() string {
	columns := make([]string, 0, len(resultKey)+len(resultUpdate))
	columns = append(columns, resultKey...)
	columns = append(columns, resultUpdate...)

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ")

	return "insert into results (" + strings.Join(columns, ", ") + ") values (" +
		placeholders + ")"
}

// resultArgs binds a row in the order resultInsert names its columns. The two
// are read together, and TestResultArgsMatchTheInsertColumns fails if they stop
// agreeing on how many there are.
func resultArgs(sum Summary, ref resultRef) []any {
	return append(resultKeyArgs(sum), resultUpdateArgs(sum, ref)...)
}

// resultKeyArgs binds the columns of resultKey, which identify the row.
func resultKeyArgs(sum Summary) []any {
	return []any{sum.Target, string(sum.ConsentMode), sum.ScanID}
}

// resultUpdateArgs binds the columns of resultUpdate — everything a row carries
// that is not its identity.
func resultUpdateArgs(sum Summary, ref resultRef) []any {
	args := append(
		[]any{sum.StartedAt.UnixNano(), string(sum.Termination)},
		resultDerivedArgs(sum, ref)...,
	)

	// resultReferenced, bound here rather than in resultDerivedArgs: a row
	// this store writes has its artifact references recorded in the same
	// transaction, while a row the document migration rewrites does not, and
	// binding it with the derived columns would claim otherwise.
	return append(args, refsRecorded)
}

// resultDerivedArgs binds the columns of resultDerived: where the document is,
// and the summary read out of it.
//
// It is separate because the document migration writes exactly these columns
// and none of the others (Story 8.4). Binding them from the same function the
// insert uses is what makes a migrated row column-for-column what a freshly
// written one would be, and keeps a summary column added later from being
// written on a new scan but skipped on a migrated one.
func resultDerivedArgs(sum Summary, ref resultRef) []any {
	return []any{
		ref.ref, ref.size, ref.digest,
		int64(sum.Duration), sum.Error, string(sum.ConsentOutcome), sum.ConsentCMP,
		sum.Requests, sum.ThirdPartyDomains, sum.PreConsentDomains,
	}
}

// Series identifies one target-and-mode stream.
type Series struct {
	Target string            `json:"target"`
	Mode   model.ConsentMode `json:"consentMode"`
}

// Series lists every stored series, sorted for deterministic output.
func (s *SQL) Series() ([]Series, error) {
	ctx, cancel := opCtx()
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

// summaryColumns are the materialised summary, in the order scanSummary reads
// them. Together they are everything Summary holds except the target and the
// consent mode, which the caller already knows because it asked for them.
const summaryColumns = `scan_id, started_at, duration_ns, termination, scan_error,
	consent_outcome, consent_cmp, requests, third_party_domains,
	pre_consent_domains, artifact_ref`

// underivedSummary explains a row that names no document. Reporting it beats
// presenting a summary of zeroes, which would read as a scan that saw nothing
// (Tenet 5).
//
// No wsaw writes such a row: an insert carries the reference, and migration 3
// refuses to finish while a row is still without one (Story 8.4, AC4). One that
// exists anyway was edited outside wsaw, and the listing says so rather than
// inventing what the scan found.
const underivedSummary = "this result names no stored document, so its summary could not be derived"

// ListResults returns summaries newest first, at most limit entries. A limit
// of zero means all.
//
// It is answered entirely from the database and performs no bucket operation
// at all (Story 8.3, AC2). The summary was small, fixed and already computed
// when the result was written, and fetching fifty documents from object storage
// to render fifty lines would be fifty round trips before the page appeared.
//
// The columns cannot drift from the documents they describe, because both come
// from summarize() at write time (AC4). Where a derivation itself changes, the
// repair is to recompute every summary from the stored documents — the
// documents are the record and the columns are a view of them (Tenet 4) — and
// that operation is Story 8.11's rebuild, which is not built yet. Until it is,
// changing summarize() leaves rows written before the change carrying the old
// definition.
func (s *SQL) ListResults(target string, mode model.ConsentMode, limit int) ([]Summary, error) {
	ctx, cancel := opCtx()
	defer cancel()

	q := `select ` + summaryColumns + ` from results
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
			sm, err := scanSummary(rows, target, mode)
			if err != nil {
				return err
			}

			out = append(out, sm)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("listing results for %s/%s: %w", target, mode, err)
	}

	return out, nil
}

// scanSummary reads one row of summaryColumns.
//
// A row whose artifact reference is empty is one an older wsaw wrote and the
// document migration has not reached yet. It is reported as such and kept in
// the listing rather than dropped from it: one row that cannot be summarised
// must neither hide the rest of a target's history nor vanish from it (Story
// 8.2, AC5).
func scanSummary(rows *sql.Rows, target string, mode model.ConsentMode) (Summary, error) {
	var (
		sm                    Summary
		startedAt, durationNS int64
		termination, outcome  string
		ref                   string
	)

	if err := rows.Scan(&sm.ScanID, &startedAt, &durationNS, &termination, &sm.Error,
		&outcome, &sm.ConsentCMP, &sm.Requests, &sm.ThirdPartyDomains,
		&sm.PreConsentDomains, &ref); err != nil {
		return Summary{}, err
	}

	sm.Target = target
	sm.ConsentMode = mode
	// UTC because the row records an instant, not the offset the scanning host
	// happened to be in; the instant is what every comparison and every
	// ordering uses.
	sm.StartedAt = time.Unix(0, startedAt).UTC()
	sm.Duration = time.Duration(durationNS)
	sm.Termination = model.TerminationReason(termination)
	sm.ConsentOutcome = model.ConsentOutcome(outcome)

	if ref == "" {
		sm.Error = underivedSummary
	}

	return sm, nil
}

// HasResult reports whether a result is still stored, without reading it.
//
// A stored document runs to megabytes, and the interface asks this question
// to decide whether it may link to a result — a page must not load the whole
// record to find out that it can offer an anchor to it. It stays a row lookup
// now that the document is in the bucket, where reading it would also cost a
// request (Story 8.3, AC3).
func (s *SQL) HasResult(target string, mode model.ConsentMode, scanID string) (bool, error) {
	ctx, cancel := opCtx()
	defer cancel()

	const q = `select 1 from results where target = ? and consent_mode = ? and scan_id = ?`

	var found bool

	err := s.retry(ctx, "checking for a result", func(ctx context.Context) error {
		found = false

		var one int

		err := s.db.QueryRowContext(ctx, s.q(q), target, string(mode), scanID).Scan(&one)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}

		found = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("checking for result %s in %s/%s: %w", scanID, target, mode, err)
	}

	return found, nil
}

// GetResult returns one result by scan ID.
func (s *SQL) GetResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	ctx, cancel := opCtx()
	defer cancel()

	return s.resultByID(ctx, target, mode, scanID)
}

// resultByID reads one result within a caller's context, so the operations
// that need a result before doing something else — approving a baseline — do
// not each rebuild the query and its errors.
func (s *SQL) resultByID(
	ctx context.Context,
	target string,
	mode model.ConsentMode,
	scanID string,
) (*model.Result, error) {
	q := `select ` + refColumns + ` from results where target = ? and consent_mode = ? and scan_id = ?`

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
func (s *SQL) LatestResult(target string, mode model.ConsentMode) (*model.Result, error) {
	ctx, cancel := opCtx()
	defer cancel()

	q := `select ` + refColumns + ` from results
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

// PreviousResult returns the most recent result before the given scan ID that
// is worth comparing against.
//
// A failed or skipped scan is skipped over. It stays in the history — it
// happened, and hiding it would be the quiet lie Tenet 5 forbids — but it is
// not an observation of the site, so diffing against it would report the
// whole site as new or as gone. That matters most for a retried scan: without
// this, a successful retry would be compared against the failure it replaced
// and manufacture a finding out of its own recovery (Story 3.8, AC6).
func (s *SQL) PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error) {
	ctx, cancel := opCtx()
	defer cancel()

	// A row-value comparison against the anchor scan, so results sharing a
	// start time still order deterministically — the same ordering the index
	// and every listing use.
	q := `select ` + refColumns + ` from results
		where target = ? and consent_mode = ?
		  and termination not in (?, ?)
		  and (started_at, scan_id) <
		      (select started_at, scan_id from results
		       where target = ? and consent_mode = ? and scan_id = ?)
		order by started_at desc, scan_id desc
		limit 1`

	res, err := s.queryResult(ctx, q,
		target, string(mode),
		string(model.TermError), string(model.TermSkipped),
		target, string(mode), scanID)
	if err != nil {
		return nil, fmt.Errorf("reading result before %s: %w", scanID, err)
	}

	if res == nil {
		return nil, fmt.Errorf("result before %s: %w", scanID, ErrNotFound)
	}

	return res, nil
}

// queryResult runs a query returning one row of refColumns and reads the
// document it references, or returns nil when there is no such row.
func (s *SQL) queryResult(ctx context.Context, query string, args ...any) (*model.Result, error) {
	var ref resultRef

	err := s.retry(ctx, "reading a result", func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, s.q(query), args...).
			Scan(&ref.ref, &ref.size, &ref.digest)
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}

	return s.readDocument(ctx, ref)
}

// readDocument reads the document one row references.
//
// What it is answerable for is the one case only a SQL index has: a row an
// older wsaw wrote, whose document is still in the column and has not been
// moved into the bucket yet (Story 8.4). There is nothing there to read, and
// that is absence rather than corruption. Everything after that — the digest
// and size check, and telling lost evidence from a corrupt reference — is the
// same judgement for every kind of index and is made once, on the bucket.
func (s *SQL) readDocument(ctx context.Context, ref resultRef) (*model.Result, error) {
	if ref.ref == "" {
		return nil, fmt.Errorf("this result's document has not been moved to the artifact bucket: %w", ErrNotFound)
	}

	return s.bucket.resultDocument(ctx, ref)
}
