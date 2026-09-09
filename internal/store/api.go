package store

import (
	"context"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Store is the result store seam: everything wsaw asks of the place its scans
// are recorded, with nothing in it about how that place records them.
//
// Tenet 12 names the result store as one of the boundaries that gets an
// interface, and this is the story it was reserved for: how the index is kept
// is a real choice, not a speculative one. SQL keeps it in rows in SQLite,
// PostgreSQL or MySQL and is its only implementation today; the second — the
// same index kept as objects in the bucket that already holds the evidence, so
// that a deployment needs no database at all — is what the rest of Story 8.10
// adds behind this interface. What a store has to answer is the same either
// way, and this is the list.
//
// It is declared in the package that implements it rather than at a consumer,
// which AGENTS §4 otherwise asks for, because no single consumer uses all of
// it: app and cmd hold a whole store and hand narrowed views to the scanner
// and the HTTP server, each of which declares the interface it actually needs
// (scanner.ResultStore, httpapi.Store). Those are the consumer-side
// interfaces; this one is the seam's own contract, and the assertion below is
// what keeps an implementation honest about it.
//
// Two absences are deliberate:
//
// DocumentMigration is not here. It reports what the Story 8.4 row-to-bucket
// migration moved, which is a fact about a schema only a SQL store has; the
// one command that asks opens a SQL store explicitly.
//
// No method takes a context except the ones that already did. Adding one to
// the read and write methods is worth doing and is not this story: it is
// roughly fifty call sites across five packages, and every store operation
// already carries the same deadline internally (opCtx). Where an operation can
// run long enough for an operator to want to interrupt it — opening, pruning,
// sweeping, streaming an artifact — the context is there.
type Store interface {
	// Driver reports which kind of store this is, for logs and for the tests
	// that run the same suite against every one of them.
	Driver() string
	// Ping reports whether both halves of the store — its index and its
	// artifact bucket — are reachable, which is what readiness asks.
	Ping(ctx context.Context) error
	// ProbeArtifactBucket establishes at startup that the bucket can be
	// written to, rather than at the first scan that cannot be stored.
	ProbeArtifactBucket(ctx context.Context) error
	// Close releases the index and the bucket.
	Close() error

	PutResult(res *model.Result) error
	ListResults(target string, mode model.ConsentMode, limit int) ([]Summary, error)
	HasResult(target string, mode model.ConsentMode, scanID string) (bool, error)
	GetResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error)
	LatestResult(target string, mode model.ConsentMode) (*model.Result, error)
	PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error)
	Series() ([]Series, error)

	GetBaseline(target string, mode model.ConsentMode) (*Baseline, error)
	HasBaseline(target string, mode model.ConsentMode) (bool, error)
	SetBaseline(target string, mode model.ConsentMode, scanID, approvedBy, note string) (*Baseline, error)
	DeleteBaseline(target string, mode model.ConsentMode, actor string) error
	Audit(limit int) ([]AuditEntry, error)
	RecordAudit(e AuditEntry) error

	PutArtifact(kind string, data []byte) (string, error)
	GetArtifact(ref string) ([]byte, error)
	OpenArtifact(ctx context.Context, ref string) (*ArtifactReader, error)
	StatArtifact(ctx context.Context, ref string) (ArtifactInfo, error)
	SignArtifactURL(ctx context.Context, ref string, ttl time.Duration) (string, error)

	// RebuildIndex derives this store's index from the documents in its
	// bucket, reports what doing so would change, or verifies the two against
	// each other (Story 8.11). It is on the seam because it is the supported
	// way between store kinds as well as the recovery procedure for an index
	// that was lost, and both of those are questions about the store an
	// operator has rather than about the one they had.
	RebuildIndex(ctx context.Context, opts RebuildOptions) (RebuildStats, error)

	Prune(ctx context.Context, now time.Time, r Retention) (PruneStats, error)
	PlanPrune(ctx context.Context, now time.Time, r Retention) (PruneStats, error)
	Sweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error)
	PlanSweep(ctx context.Context, now time.Time, opts SweepOptions) (SweepStats, error)
}

// The compile-time proof that the seam is a description of what exists rather
// than an aspiration. A method added to the interface without an implementation
// fails the build here, next to the contract, instead of at whichever consumer
// happened to be assigned a store first.
var (
	_ Store = (*SQL)(nil)
	_ Store = (*Blob)(nil)
)

// Open creates or opens the store the options name.
//
// It exists so that a consumer asks for "the store" and gets whichever one is
// configured, rather than each of them naming a constructor and so pinning
// itself to one kind. Three of the four drivers keep their index in rows and
// are opened by OpenSQL; blob keeps it as objects in the artifact bucket and is
// opened by OpenBlob (Story 8.10, AC1). Which one a deployment gets is the one
// line of configuration that says so, and SQLite is still what it gets by
// saying nothing.
//
// The error path returns a literal nil rather than the failed store, which
// matters more than it looks: a (*SQL)(nil) returned as a Store is an interface
// value that is not nil, and every consumer that checks `Store == nil` for "no
// store configured" would sail past it and panic on the first call.
func Open(ctx context.Context, opts Options) (Store, error) {
	if opts.Driver == DriverBlob {
		s, err := OpenBlob(ctx, opts)
		if err != nil {
			return nil, err
		}

		return s, nil
	}

	s, err := OpenSQL(ctx, opts)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// OpenForInspection opens the configured store without bringing its schema up
// to date, and refuses one that is behind.
//
// It exists for the two modes of `wsaw store rebuild-index` that promise to
// change nothing. Opening a SQL store the ordinary way applies its migrations,
// and one of those migrations moves every stored document out of the database
// and into the bucket and then drops the column — irreversible, minutes long,
// and a network write per row (Story 8.4). An operator who does not know what
// state their store is in and reaches for the safe-looking `--verify` would get
// the whole one-way upgrade and then read "mode: verify" and "Nothing was
// written." That is the surprise the rebuild's own reasoning says this command
// must not have, in the one command somebody runs when they are not sure what
// is safe.
//
// A store that is behind is refused with the command that upgrades it rather
// than upgraded here. What it is not is silently tolerated: a verify against a
// schema this build does not recognise would be comparing an index it cannot
// read against a bucket, and reporting the result as agreement or drift would
// be a made-up answer either way (Tenet 5).
//
// It is "no more than a reader would" and not literally nothing: reading the
// schema version creates the one-row bookkeeping table on PostgreSQL and MySQL,
// which is the same idempotent DDL every other read of it performs, and opening
// the bucket-index store writes its layout-version object if the bucket has
// none (Story 8.10, AC14). Neither touches a scan, a row or a document.
func OpenForInspection(ctx context.Context, opts Options) (Store, error) {
	if opts.Driver == DriverBlob {
		s, err := OpenBlob(ctx, opts)
		if err != nil {
			return nil, err
		}

		return s, nil
	}

	s, err := openSQLAtItsOwnSchema(ctx, opts)
	if err != nil {
		return nil, err
	}

	return s, nil
}
