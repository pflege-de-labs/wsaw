# Story 8.10 — A store with no database: the index in the bucket

**Final implementable specification.** Written against the tree at
`/Users/martin/pflege.de/wsaw/.claude/worktrees/epic-blob-store` (branch
`feat/epic-8-blob-store`, HEAD `332eb92`). Nothing in the repository was
modified while writing it. `internal/store`, `internal/config` and
`internal/httpapi` are being edited concurrently, so this document names
shapes and contracts, never line numbers, and the one change it needs inside
`internal/store/bucket.go` is deliberately routed into a new file instead.

Every objection raised by the two red-team passes is either fixed here or
answered in **§11**, which records the calls made and why. There are no open
questions left for the implementer.

---

## 1. What is being built and why

A third kind of store, `blob`, beside `sqlite`, `postgres` and `mysql`. It
keeps its index as objects in the same bucket as the evidence, so a wsaw
deployment can be one binary and one bucket with no database to run, back up
or fail over.

A bucket is not a database and is not pretended into one. There are no
transactions, no cross-key atomicity, and no portable compare-and-swap. A key
written a moment ago may not appear in the next listing, and a listing may be
served from a stale view. The design is therefore **append-only**: every write
goes to a key nobody else writes and nothing rewrites, and every read is a
fold over what is currently visible.

Four invariants carry the whole design. Everything below is a consequence of
one of them.

* **I1 — No rewrite.** No index key is ever written twice with different
  bytes. Two facts that differ produce two keys.
* **I2 — No key from an unescaped caller value.** A target is a URL and a
  consent mode is a short string; neither ever reaches a key verbatim
  (Tenet 9).
* **I3 — No deletion without positive observation.** A key is deleted only
  when *this run* observed, in a listing taken now, the thing that makes it
  redundant — never because a listing failed to show something.
* **I4 — A body is a pure function of its fact.** Re-running an interrupted
  write produces the identical key with the identical bytes (AC10).

What it buys is the removal of the database. What it costs is stated in §7
and in the README text of §10.

---

## 2. The Go seam

### 2.1 Shapes

Three interfaces at three real boundaries. The wide one exists because
`store.Open` dispatches on `Options.Driver` and needs a return type, and
because AC2 names "the same store interface" as the deliverable. The two
narrow ones exist because AGENTS §4 defines an interface where it is
*consumed*, and because they document authority: the HTTP layer cannot write
a result, the scanner cannot approve a baseline or prune.

```go
// internal/store/api.go  (new file)

// Store is the result store seam. Tenet 12 names the result store as one of
// the boundaries that gets an interface, and this story is the second
// implementation it was reserved for: the SQL stores keep their index in
// rows, the blob store keeps it in objects in the same bucket as the
// evidence.
//
// It is declared here rather than at a consumer because no single consumer
// uses all of it: app and cmd hold a whole store and hand narrowed views to
// the scanner and the HTTP server, each of which declares its own.
//
// DocumentMigration is deliberately absent. It is the Story 8.4 row-to-bucket
// migration, which the bucket index has no counterpart for (AC16); the one
// caller that needs it opens a SQL store explicitly.
type Store interface {
	Driver() string
	Ping(ctx context.Context) error
	ProbeArtifactBucket(ctx context.Context) error
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
	OpenArtifact(ref string) (io.ReadCloser, int64, error)
	StatArtifact(ref string) (int64, error)

	Prune(ctx context.Context, now time.Time, r Retention) (PruneStats, error)
	PlanPrune(ctx context.Context, now time.Time, r Retention) (PruneStats, error)
	Sweep(ctx context.Context, now time.Time) (SweepStats, error)
	PlanSweep(ctx context.Context, now time.Time) (SweepStats, error)
}

var (
	_ Store = (*SQL)(nil)
	_ Store = (*Blob)(nil)
)

// Open creates or opens the configured store. SQLite remains the default
// (AC1); a deployment that does not ask for the bucket index does not get it.
func Open(ctx context.Context, opts Options) (Store, error) {
	if opts.Driver == DriverBlob {
		return OpenBlob(ctx, opts)
	}

	return OpenSQL(ctx, opts)
}
```

`HasBaseline` is the one method added to the contract. It exists because the
targets page asks "does a baseline exist for this target and mode" once per
series and today answers it by fetching the whole approved result — a full
copy of a scan document, per series, per page render. Against SQLite that is
a wasted row read; against a bucket it is 600 multi-megabyte GETs to render a
dashboard. SQL implements it as `select 1 from baselines where …`; the blob
store implements it as a listing with no GET at all (§6.5). It is a narrowing
of an existing question, not a second code path, so AC2 holds.

```go
// internal/scanner/scanner.go — 4 methods; exactly what a scan needs.
// A scanner that could prune or approve a baseline would carry more
// authority than a scan has.
type ResultStore interface {
	PutArtifact(kind string, data []byte) (string, error)
	PutResult(res *model.Result) error
	GetBaseline(target string, mode model.ConsentMode) (*store.Baseline, error)
	PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error)
}
// Deps.Store becomes ResultStore. The four call sites do not change.
```

```go
// internal/httpapi/server.go — 14 methods, verified against every store call
// site in api.go, ui.go and sharing.go. PutResult and PutArtifact are
// excluded on purpose: this interface triggers scans, it does not record
// them.
type Store interface {
	Ping(ctx context.Context) error

	ListResults(target string, mode model.ConsentMode, limit int) ([]store.Summary, error)
	HasResult(target string, mode model.ConsentMode, scanID string) (bool, error)
	GetResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error)
	LatestResult(target string, mode model.ConsentMode) (*model.Result, error)
	PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error)

	GetBaseline(target string, mode model.ConsentMode) (*store.Baseline, error)
	HasBaseline(target string, mode model.ConsentMode) (bool, error)
	SetBaseline(target string, mode model.ConsentMode, scanID, approvedBy, note string) (*store.Baseline, error)
	DeleteBaseline(target string, mode model.ConsentMode, actor string) error
	Audit(limit int) ([]store.AuditEntry, error)

	GetArtifact(ref string) ([]byte, error)
	OpenArtifact(ref string) (io.ReadCloser, int64, error)
	StatArtifact(ref string) (int64, error)
}
```

`app.App.Store` is typed `store.Store`. Structural typing does the rest: it
is assignable to both narrow interfaces, so `cmd/wsaw/run.go`'s
`Store: a.Store` compiles unchanged and `app` acquires no import of
`httpapi`. Concrete types are `store.SQL` (today's struct, renamed) and
`store.Blob`. `store.OpenSQL` stays exported for the two callers that
legitimately need SQL: `wsaw store migrate` and the SQL-pinned half of the
store suite.

### 2.2 Two decisions inside the seam

**No `ctx` on the read/write methods.** AGENTS §4 wants it, and against
object storage the argument is stronger than it was against SQLite. Adding it
touches roughly fifty call sites across `api.go`, `ui.go`, `sharing.go`,
`scanner.go`, `app.go`, `scan.go`, `share.go` and their tests — precisely the
"no caller acquires a second code path" AC2 forbids. It is its own story
across all four drivers, and it is recorded as such in §12.

`opTimeout` is **already 30 s** for every store and bucket call, and
`Options.timeout()` is SQLite's `busy_timeout` and never reaches a bucket. So
there is nothing to change there, and the real problem is different: one 30 s
deadline for a fold that may issue a thousand retried requests is a single
cliff. `Blob` therefore uses a **two-level budget**: `opTimeout` (30 s)
remains the per-request deadline applied to each individual bucket call, and
each exported method additionally carries an overall budget of
`blobOpBudget = 120 s` within which the whole fold, including pagination and
hydration, must finish. Exceeding the budget returns a wrapped
`context.DeadlineExceeded` naming the operation and the number of requests
issued — never a short answer (§6.7).

**The typed-nil trap.** `httpapi` and `scanner` both test `deps.Store == nil`.
Once the field is an interface a typed nil passes that check and panics on
call. `Open` returns `nil, err` on failure and never a non-nil interface
wrapping a nil pointer; each `Deps.Store` field gets a one-line comment saying
the zero value is the "no store" case and that a typed nil is a programmer
bug.

### 2.3 File-by-file change list for consumers

| File | Change |
|---|---|
| `internal/store/store.go` | `type Store struct` → `type SQL struct`; receiver renames; `Open` → `OpenSQL`; `describe()` gains a `blob` case returning `"blob " + ArtifactLocation()` |
| `internal/store/{baseline,documents,references,retention,dialect}.go` | receiver renames only, plus the two fixes in §9 |
| `internal/store/api.go` | **new**: the `Store` interface, the `Open` factory, the assertions |
| `internal/app/app.go` | `Store store.Store`; `adoptStore(st store.Store)`; a `blob` branch in `StoreOptions` that resolves no DSN and no pool limits — ~30 lines |
| `internal/scanner/scanner.go` | `+ ResultStore` declaration, `Deps.Store` retyped — ~12 lines; **4 call sites unchanged** |
| `internal/httpapi/server.go` | `+ Store` declaration, `Deps.Store` retyped, the nil-check comment — ~22 lines |
| `internal/httpapi/api.go` | one call site: the targets-page existence check moves from `GetBaseline` to `HasBaseline` — 3 lines. The other 27 store call sites are unchanged |
| `internal/httpapi/{ui,sharing}.go` | **0 lines** |
| `cmd/wsaw/store.go` | `maintenance.store` becomes a four-method `retentionStore` declared beside it, **not** `store.Store` as this row said — a prune and a sweep have no business being able to store a result or approve a baseline, which is the same argument `scanner.ResultStore` makes, and two of three consumers narrowing while the third took the whole store would have made the rule look optional; `storeMigrateApply` and `PlanDocumentMigration` use `OpenSQL` and refuse `blob` (the message names no command — see §7.6 #11) — ~30 lines |
| `cmd/wsaw/{run,scan,share,config}.go` | **0 lines** |
| `internal/config/config.go` | `IsServerStore()` narrowed to postgres/mysql; `IsBucketStore()` added — ~12 lines |
| `internal/config/validate.go` | a `blob` branch: no `dsn`, no `path`, no pool settings; an artifact location required — ~25 lines |
| `internal/store/dialect.go` | `DriverBlob = "blob"`, added to `Drivers()`; `dialectFor` refuses it by name with a message saying it is not a SQL dialect |
| `internal/httpapi/httpapi_test.go` | field type — 1 line |
| `internal/store/store_test.go` | `open(t)` return type; a `blob` case in `storeOptions`; 19 `OpenSQL`; the new `documentRefFor` helper; `TestDriverIsReported` — ~50 lines |
| `internal/store/retention_test.go` | three helper signatures (`withEvidence`, `assertStored`, `assertGone`) take `store.Store`; one `OpenSQL` — ~5 lines |
| `internal/store/{documents,readiness,result,bucket,dialect}_test.go` | `OpenSQL` where they open a store — these are SQL-specific by construction |
| `internal/soak/soak_test.go`, `internal/scanner/screenshots_test.go`, every other `internal/httpapi/*_test.go` | **0 lines** |

Consumer test churn is one line. That number is the argument that the seam is
in the right place.

---

## 3. Package layout

All new files are in `internal/store`.

| File | ~lines | Contents |
|---|---|---|
| `api.go` | 160 | The `Store` interface and its doc comment; the `Open` factory; `var _ Store = …` assertions |
| `shared.go` | 200 (≈160 moved) | `retrier`, `opCtx`/`opCtxFrom`, `decodeDocument`, `documentDigest`, `summarize` access, `Options` defaulting, `Options.Now`, `Options.CheckpointGrace` |
| `bucket_index.go` | 220 | The index door onto the existing bucket: `putIndex` (tri-state), `getIndex`, `statIndex`, `deleteIndex`, `listIndexPage`, `validateIndexKey`. Shares `bucket.do`, the retry policy and the `gcerrors` mapping. **No edit to `bucket.go`.** |
| `blobkeys.go` | 260 | The §4 grammar: `tk`, `mk`, `sk`, `tm`, `inv`, `stamp`, `did`, `gen`, every key constructor and parser. Pure, no I/O — this is where the interesting invariants live and where a key bug is caught by the fast suite |
| `blob.go` | 360 | The `Blob` type, `OpenBlob`, the layout probe, `Ping`, `ProbeArtifactBucket`, `Close`, `Driver`, the four artifact methods delegating to `bucket`, the write overlay and the immutable-object cache |
| `blobfold.go` | 460 | `foldKeys`, `hydrate`, the checkpoint union, tombstone application, the tie-break; `PutResult`, `ListResults`, `HasResult`, `GetResult`, `LatestResult`, `PreviousResult`, `Series` |
| `blobbaseline.go` | 320 | `SetBaseline`, `GetBaseline`, `HasBaseline`, `DeleteBaseline`, `Audit`, `RecordAudit`, the decision fold, the audit-pointer self-heal |
| `blobprune.go` | 300 | `Prune`, `PlanPrune`, `Sweep`, `PlanSweep`, tombstones, the `ref/` reverse index, the ordered merge join |
| `blobcompact.go` | 280 | Checkpoint write, the checkpoint union, the I3 deletion rule, tombstone GC, the triggers |
| `lagbucket_test.go` | 320 | The `lag://` driver wrapper and its knobs (§8.2) |
| `blobkeys_test.go` | 240 | Table-driven and fuzz key-grammar tests |
| `blob_test.go` | 800 | The blob-only tests of §8.3 |

Roughly 2,560 production lines and 1,360 test lines of new code.

---

## 4. Key grammar

### 4.1 Root, and why it is provably disjoint

```
_wsaw/…
```

Artifacts are `<kind>/<sha256hex>` at the bucket root, and `validKind` accepts
only `[a-z0-9]` with an internal `-`. A root segment beginning with `_` is
therefore disjoint from every artifact key that exists or can ever be
written, with no reserved-word list to maintain. (`index/` would **not** be
disjoint: `index` is a legal kind.)

### 4.2 The listing-order invariant

> **No directory that a listing walks may contain both objects and
> sub-directories.**

`fileblob` lists through `filepath.WalkDir`, which sorts per directory: with
mixed content, `"x/a"` comes before `"x-b"` on disk and after it on S3. Every
prefix in §4.5 satisfies the invariant — `series/<tk>/` holds only `<mk>`
directories, `series/<tk>/<mk>/` holds only objects, `ref/<kind>/` holds only
digest directories, `ref/<kind>/<digest>/` holds only owner objects — so
`fileblob` and S3 return identical order for every listing this store makes.
This is a correctness constraint, it is asserted by a test (§8.3 #13), and it
is the reason the series marker is one segment rather than two.

### 4.3 Component encoders (I2)

Three caller-supplied values reach a key and none of them appears verbatim.
Every encoder is total, injective, and produces only `[a-z0-9-]` plus the
fixed separators.

```
tk(target) = hex(sha256(target))[:32] + "-" + slug(target, 48)
mk(mode)   = mode                            if mode ∈ {none, reject, accept}
           = "_" + hex(sha256(mode))[:32]    otherwise
sk(scanID) = scanID                          if scanID matches ^[a-z0-9][a-z0-9-]{0,63}$
           = "_" + hex(sha256(scanID))[:32]  otherwise
tm(term)   = term                            if term matches ^[a-z][a-z0-9-]{0,23}$
           = "_" + hex(sha256(term))[:16]    otherwise

slug(s,n): lowercase; every byte outside [a-z0-9] → "-"; collapse runs of "-";
           trim leading and trailing "-"; truncate to n bytes; trim again;
           "x" if the result is empty. Never contains "_", ".", or "/".
```

`_` is the escape marker and no literal alphabet contains it, so escaped and
literal forms are disjoint and every encoder is injective. `tk` is parsed
positionally: the first 32 bytes are the identity and everything after the
first `-` is decoration that cannot affect it.

**The target is always hashed**, for four reasons each of which is
sufficient on its own: case-folding filesystems (`TestTargetNamesAreCaseSensitive`
would fail on APFS with a literal segment, because it stores `"Site"` and
`"site"` as distinct series), the 255-byte path-component limit against a
2,000-byte URL, the 1,024-byte S3 key limit, and NFC/NFD normalisation. 128
bits and not 64, because a collision merges two targets' histories, which is a
correctness failure, and 32 bytes is free.

**The field separator is `.`**, and no component alphabet contains `.`, so
every key splits unambiguously — which matters as soon as a termination is
`request-cap` and a scan ID is `scan-1`.

### 4.4 Ordering

```
clampNano(t) = min(max(t.UnixNano(), 0), math.MaxInt64)   // 1970-01-01 … 2262-04-11
inv(t)       = fmt.Sprintf("%019d", uint64(math.MaxInt64 - clampNano(t)))
stamp(t)     = time.Unix(0, clampNano(t)).UTC().Format("20060102T150405Z")
```

* **Resolution 1 ns**, required rather than chosen:
  `TestOrderingSurvivesSubMillisecondStarts` puts three results one
  microsecond apart and asserts strict newest-first.
* **Range** 1970-01-01 to 2262-04-11. Out of range is clamped, never dropped,
  and logged at `Warn` with `scan_id`, `target`, `consent_mode`.
* **Fixed width 19** — `MaxInt64` is 19 digits — so a later field can never
  influence ordering across different instants.
* `blob.ListOptions` in v0.46.0 offers neither reverse listing nor
  `StartAfter`, so inversion is the only way to get newest-first out of a
  listing (AC4).
* `stamp` is decoration placed after `inv`. It is derived from the **clamped**
  value, so it is constant whenever `inv` is and can never reorder anything;
  it exists so a human reading a bucket browser sees `20260908T104512Z` and
  not only a 19-digit number.

Verified for `2026-09-08T10:45:12.123456789Z`:

```
inv                 7434507724731319018    stamp 20260908T104512Z
+1 µs (newer)       7434507724731318018    ← smaller, sorts first  ✓
−24 h (older)       7434594124731319018    ← larger,  sorts later  ✓
```

**Ties are resolved in the fold, not in the key.** SQL orders
`started_at desc, scan_id desc`. Entries sharing an `inv` form a contiguous
run in the listing; the fold reads the run to completion and sorts it by
`<sk>` **descending**. Two reasons this is not encoded in the key:
`scanner.newScanID()`'s entropy-failure fallback is `"scan-"` plus decimal
nanoseconds and is not fixed-width, so a key-encoded descending tie-break is
not sound; and a byte-complemented scan ID destroys the one property that
makes a bucket index worth debugging. A run longer than
`maxTieRun = 1000` is reported as a fault (`ErrCorrupt`) rather than sorted,
because a thousand scans starting in the same nanosecond is not a history.

Because `<sk>` is the only field that varies inside a run, the entry key puts
`<sk>` **before** `<tm>`. Putting `<tm>` first would order a run by
termination and then by scan ID, which is not what SQL does and which no
current test would catch.

### 4.5 The complete grammar

```
_wsaw/index/layout/<8-digit>.json
_wsaw/index/v1/targets/<tk>.<mk>
_wsaw/index/v1/series/<tk>/<mk>/d.<inv>.<sk>                  (tombstone, zero bytes)
_wsaw/index/v1/series/<tk>/<mk>/k.<inv>.<gen>                 (checkpoint)
_wsaw/index/v1/series/<tk>/<mk>/r.<inv>.<stamp>.<sk>.<tm>     (entry)
_wsaw/index/v1/byid/<tk>/<mk>/<sk>
_wsaw/index/v1/baseline/<tk>/<mk>/<inv>.<stamp>.<op>.<did>    (the decision object)
_wsaw/index/v1/audit/<inv>.<stamp>.<did>
_wsaw/index/v1/auditckpt/<inv>.<gen>
_wsaw/index/v1/ref/<kind>/<digest>/<owner>                    (zero bytes)
_wsaw/index/v1/rebuild/<id>                                   (written by Story 8.11)
```

* `<op>` ∈ `{approve, revoke}`.
* `<did>` = `hex(sha256(canonical decision body))[:32]`.
* `<gen>` = `hex(sha256(checkpoint body))[:32]`. 128 bits for both: a
  collision at the same `inv` silently discards a compliance decision or keeps
  the wrong history, which is the same argument that made `tk` 128 bits, and
  the layout has ~700 bytes of headroom.
* `<owner>` = `r.<tk>.<mk>.<sk>` for a result, `d.<did>` for a baseline
  decision, `t.<inv>` for a **take** — the record that `PutArtifact` stored
  these bytes for a scan whose result is not in the index yet, which is this
  store's answer to `claims.go`'s row and is added by §7.6 #1. The tags sort
  `d` < `r` < `t`, so the pins that answer "does anything reference this" come
  before every take. A take is not a reference: it expires, and a pin written
  after it releases it.

**Everything a series fold needs is in one directory.** `d` < `k` < `r`, so a
single listing of `series/<tk>/<mk>/` returns the tombstones first, then the
checkpoints (newest first), then the entries (newest first). A fold that
wants the newest *n* entries reads one page and stops; it has seen every
tombstone and every checkpoint that could affect its answer, **from one
listing**, which is what makes AC7's determinism a property of a single
visible set rather than of three listings that may disagree. `byid/` is a
separate prefix because it is point-addressed and never listed by a fold —
putting it in the same directory would put ten thousand keys in front of
every listing.

**Literal examples.** Target `https://www.pflege.de/pflegeberatung`, mode
`reject`, scan `scan-9f1c0b2d3e4f5a6b7c8d9e0f`, started
`2026-09-08T10:45:12.123456789Z`, termination `completed`:

```
_wsaw/index/layout/00000001.json

_wsaw/index/v1/targets/c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung.reject

_wsaw/index/v1/series/c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung/reject/
    r.7434507724731319018.20260908T104512Z.scan-9f1c0b2d3e4f5a6b7c8d9e0f.completed

_wsaw/index/v1/series/c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung/reject/
    k.7434594124731319018.3f21ab90cc7e1d55a0be47c1e2d3f905

_wsaw/index/v1/series/c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung/reject/
    d.7434594124731319018.scan-3a7b19ce55d0f412

_wsaw/index/v1/byid/c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung/reject/
    scan-9f1c0b2d3e4f5a6b7c8d9e0f

_wsaw/index/v1/baseline/c58abfa3d591f80b9c63801fb2c9e681-…/reject/
    7434507724731319018.20260908T104512Z.approve.ec77eeb0cd4a92113b8f2a6d40915cc7

_wsaw/index/v1/audit/7434507724731319018.20260908T104512Z.ec77eeb0cd4a92113b8f2a6d40915cc7

_wsaw/index/v1/ref/screenshot-after-consent/9d2c…64hex…/
    r.c58abfa3d591f80b9c63801fb2c9e681-https-www-pflege-de-pflegeberatung.reject.scan-9f1c0b2d3e4f5a6b7c8d9e0f
```

Escaped forms — the case-sensitivity case from the existing suite, and a
hostile mode:

```
…/series/fa7955814e32aed3a240ee46fcd053dd-site/reject/…       ← target "Site"
…/series/fbae041b02c41ed0fd8a4efb039bc780-site/reject/…       ← target "site"
…/series/<tk>/_4c9f…32hex…/…                                  ← mode "reject/ALL?"
```

Longest key ≈ 250 bytes. **Longest path component is the `ref/` owner**, at
`2 + 81 + 1 + 33 + 1 + 64 ≈ 182` bytes — inside the 255-byte filesystem limit
and far inside S3's 1,024-byte key limit. That is the number that decides
whether the layout is filesystem-safe, and a fuzz test asserts it (§8.3 #12).

### 4.6 What is in the key and what is in the body

The rule: **the key carries what selects; the body carries what displays.**

`<tm>` is in the key because `PreviousResult` must skip `error` and `skipped`
scans. With it in the key, a series with two hundred consecutive failures
still costs one listing and two GETs. Free-form strings — `Summary.Error`,
the CMP name — cannot go in a key and stay in the body.

```jsonc
// the entry object, and the byid object: byte-identical bodies, ~450 B
{ "layout": 1,
  "summary":  { …exactly store.Summary, produced by the same summarize()… },
  "document": { "ref": "result/<sha256>", "size": 481920, "digest": "<sha256>" },
  "refs":     ["result/<sha>", "screenshot-after-consent/<sha>", "body/<sha>"]
}
```

The duplication is deliberate: each object is self-describing, which is what
makes the bucket debuggable and what lets Story 8.11's rebuild produce both
from one document without keeping two derivations in step. The entry key is
recomputable from the body (`summary.startedAt`, `summary.scanId`,
`summary.termination`), so no back-pointer field is needed and none is
written. `refs` in the body is the index-object answer to Story 8.5 AC2 —
"which artifacts does this result reference" costs zero extra requests.

---

## 5. Constants

All are named constants with the reasoning in the comment, not configuration,
because an operator has no basis on which to tune them. The two exceptions
are `Options.Now` and `Options.CheckpointGrace`, which exist so tests need no
`time.Sleep` (AGENTS §5).

| Name | Value | Why |
|---|---|---|
| `indexListPageSize` | 1000 | The provider maximum; separate from the existing `artifactListPageSize = 256`, which stays as it is for artifacts |
| `blobMaxHydrate` | 1000 | Matches the HTTP API's own `?limit=` clamp; the cap on bodies fetched for one call |
| `blobHydrateConcurrency` | 16 | Bounded fan-out; AGENTS §4 forbids unbounded goroutines |
| `blobOpBudget` | 120 s | The overall budget for one exported method; `opTimeout` (30 s) still bounds each individual request |
| `compactAfter` | 1000 | Loose entries in a series directory before compaction runs |
| `compactKeep` | 200 | Newest entries left loose, so the hot path never reads a checkpoint |
| `checkpointMaxBytes` | 4 MiB | One checkpoint object's cap |
| `checkpointMaxEntries` | 20000 | The other cap |
| `Options.CheckpointGrace` | 24 h | How long a checkpoint must have been visible before what it covers may be deleted |
| `tombstoneGrace` | 7 days | How long a tombstone must have existed before it may be collected |
| `unreferencedArtifactGrace` | 24 h (existing) | Now also applied to **prune's** artifact deletion, not only to the sweep |
| `overlayTTL` / `overlayMax` | 15 min / 4096 keys | The read-your-writes overlay |
| `immutableCacheBytes` | 16 MiB | LRU over immutable object bodies |
| `maxTieRun` | 1000 | Entries sharing one nanosecond beyond which the fold reports corruption |

---

## 6. Operations

Notation: **W** = `putIndex`, **G** = `getIndex`, **L** = `listIndexPage`,
**D** = `deleteIndex`.

### 6.1 `putIndex` and the tri-state

`bucket.write` maps `gcerrors.FailedPrecondition` from `IfNotExist` to a
silent `nil`, which is exactly right for artifacts, because an artifact key
that exists already holds those bytes. Index keys are **not** content-addressed
in general, so the swallow is wrong for them.

```go
type putOutcome int
const (
	putCreated putOutcome = iota // this call wrote the key
	putExisted                   // the key was already there; bytes not compared
)

// putIndex creates one index key, never rewriting one. Every index write in
// this store goes through it (I1). The outcome is returned rather than
// swallowed because two callers need to tell the cases apart: PutResult, so a
// scan ID reused for different content is refused rather than shadowed, and
// compaction, so a re-run of an interrupted pass is reported as the no-op it
// is (AC10).
func (b *Blob) putIndex(ctx context.Context, key string, body []byte) (putOutcome, error)
```

`fileblob` and `memblob` both honour `IfNotExist`, so the local and the test
bucket exercise the real path. A provider that answers `Unimplemented` for the
condition is still safe, because **I1 is enforced by key derivation, not by
the condition**: two distinct facts derive two distinct keys, and two writers
of the same fact write the same bytes. The `ifNotExistIsALie` test knob
(§8.2) asserts exactly that.

`validateIndexKey` is a separate whitelist from `validateRef`, matching the
§4.5 grammar. Relaxing `validateRef` would be a mistake: its whole value is
that it has to be right about exactly one shape.

### 6.2 `PutResult`

Write order, and every step is `IfNotExist`:

| # | key | why here |
|---|---|---|
| 1 | `result/<sha256>` via the existing `bucket.put` | Story 8.2 AC4: bucket first |
| 2 | `ref/<kind>/<digest>/r.<tk>.<mk>.<sk>` for each artifact the result names, **including the document** | the pin exists before the result is visible, so a live result never has an unpinned artifact |
| 3 | `byid/<tk>/<mk>/<sk>` | must precede 4 |
| 4 | `series/<tk>/<mk>/r.<inv>.<stamp>.<sk>.<tm>` | **the commit point** |
| 5 | `targets/<tk>.<mk>` | once per series per process |

Reader behaviour on each partial set:

* **1 only** — invisible to every query. Not "deleted", *not yet there*
  (AC6). Never collected: the blob sweep does not collect `result/` objects
  at all (§7.4), and Story 8.11 re-derives its entry.
* **1+2** — as above, plus ref markers whose owner does not exist. Collected
  by the sweep's dangling-owner pass (§7.4), which is the analogue of SQL's
  `sweepDangling`.
* **1+2+3** — `GetResult` and `HasResult` answer; `ListResults` does not show
  it yet. Tolerable and self-healing.
* **1+2+3+4, no 5** — complete for reading; the series is one write behind in
  `Series()` and heals on the next `PutResult` or rebuild.
* **A visible entry with no `byid`** cannot happen, and that ordering is the
  point. The reverse order would put a row in the interface that 404s when
  clicked, which is the one visibly-wrong state.

**Step 3 uses the tri-state.** If `byid/<tk>/<mk>/<sk>` already exists naming
a different document, `PutResult` returns an error naming both rather than
growing two histories for one scan ID. This is the one deliberate behaviour
divergence from SQL, which upserts. No test in the current suite exercises
it, and production cannot produce it — `newScanID()` mints a fresh ID per
scan, retries included. It is documented in the package comment. If a shared
test is ever written asserting SQL's overwrite, that is a genuine divergence
for a human to settle (AGENTS §12), not something to paper over with a
rewrite AC3 forbids.

**The compaction trigger** fires at the end of `PutResult` when this process's
in-memory per-series write counter crosses `compactAfter` (§7.2). It runs
synchronously inside the op budget; a failure is logged and is **not** fatal,
because compaction is an optimisation, not a fact.

### 6.3 The fold — `foldKeys` and `hydrate`

This is the single read path, and it is split in two because everything a
read needs in order to *select* is already in the key.

```go
// foldKeys returns the selected entry keys for one series, newest first,
// reading no bodies at all. It is one listing of series/<tk>/<mk>/ that
// yields the tombstones, the checkpoints and the loose entries together, so
// the answer is a function of one visible key set (AC7).
func (b *Blob) foldKeys(ctx context.Context, tk, mk string, want int, skipFailed bool,
	before *entryKey) ([]entryKey, error)

// hydrate reads the bodies of exactly the keys the caller is returning,
// with bounded concurrency, honouring the immutable-object cache and the
// recent-write overlay.
func (b *Blob) hydrate(ctx context.Context, keys []entryKey) ([]entryBody, error)
```

`foldKeys` in steps:

1. **L** `series/<tk>/<mk>/`, page 1. It returns, in order: every `d.` key
   (tombstones), every `k.` key (checkpoints, newest first), then `r.` keys
   (entries, newest first). Continue paging only while more entries are still
   wanted.
2. Build the tombstoned scan-ID set from the `d.` keys.
3. Walk the `r.` keys, skipping tombstoned scan IDs and — when `skipFailed` —
   those whose `<tm>` is `error` or `skipped`. Stop as soon as `want` is
   satisfied, **after completing the current equal-`inv` run**.
4. If `want` is still not satisfied, **G** every `k.` checkpoint observed in
   step 1 and **union** their `entries` arrays, deduped by scan ID. Apply the
   union's own `tombstoned` array as well. Checkpoints are unioned, never
   chained: completeness follows from one listing rather than from a graph
   that two concurrent compactors can fork.
5. Dedupe by scan ID — a covered entry whose loose key has not been deleted
   yet appears twice — keeping the smallest key. Sort each equal-`inv` run by
   `<sk>` descending.

A listed `k.` key whose GET returns NotFound is a hard error
(`ErrIndexIncomplete`, §6.7), never a short answer. Checkpoints are never
deleted by any operation in this store, so a missing one means the bucket
lost an object or a human removed it.

A listed `r.` key whose GET returns NotFound during `hydrate` is **skipped,
counted, and logged at Debug**. It is the ordinary consequence of a
concurrent prune: the entry key and its body are the same object, written and
deleted by the same operation, so list-behind-delete is expected there. This
rule is deliberately different from the baseline rule in §6.5, and the reason
is written into both: a baseline decision object is never deleted, so its
absence really is corruption.

### 6.4 The result read paths

| method | path | LIST | GET |
|---|---|---|---|
| `ListResults(t,m,n)` | `foldKeys(want=min(n, blobMaxHydrate))` + `hydrate` | 1 (2 if the series directory exceeds one page) | n |
| `LatestResult` | `foldKeys(want=1)`, `hydrate` 1, then the document | 1 | 2 |
| `GetResult` | **G** `byid/<tk>/<mk>/<sk>`, then the document. No fold | 0 | 2 |
| `HasResult` | `statIndex` on `byid`. No listing, no document | 0 | 1 |
| `PreviousResult` | **G** `byid` for the anchor's key fields, `foldKeys(want=1, skipFailed, before=anchor)`, **G** the candidate's `byid`, then the document | ⌈k/1000⌉ | 3 |
| `Series()` | **L** `targets/`, **G** each marker (cached forever), sort in memory by literal target then mode | ⌈S/1000⌉ | S |

`ListResults` reads no result document. Story 8.3 AC2 says "listing N results
performs zero bucket reads", which cannot hold verbatim in a store whose
index *is* the bucket; the property that matters and that is tested is that
**a listing reads no result document**. The existing
`TestListingNeedsNoDocumentAtAll` asserts precisely that, by deleting the
`result/` prefix and requiring the summaries to still come back, and it
passes against `Blob` unchanged. The strict zero-count assertion lives in
`TestListingPerformsNoBucketOperations`, which is hardcoded SQLite in package
`store` and stays SQL-only. **No edit to the epic document is needed**, and
none should be made.

`Series()` must sort in memory because `<tk>` orders by digest while SQL
orders by target. The ≤600 marker bodies are immutable and cached, so a warm
`Series()` is one listing.

`PreviousResult` gets one extra safety step: after choosing a candidate, it
re-lists the key range between the candidate and the anchor once. If the
visible set changed, it retries once and logs at `Warn`. This turns the
failure mode of §8's F-case — a lagging listing skipping a scan written by
another process — from silent into observed most of the time. It cannot be
eliminated; see §10.

### 6.5 Baselines and the audit log

**One authoritative object per decision, in the baseline prefix, with the
ordering and the operation in its key:**

```
_wsaw/index/v1/baseline/<tk>/<mk>/<inv>.<stamp>.<op>.<did>
```

```jsonc
{ "layout": 1,
  "op": "approve",                              // or "revoke"
  "nonce": "3f8c1a90b7d24e55",                  // 64 bits, drawn once above the retry loop
  "audit": {                                    // exactly store.AuditEntry
    "at": "2026-09-08T10:45:12.123456789Z",
    "actor": "eva@example.com",
    "action": "baseline-approved",
    "target": "https://www.pflege.de/pflegeberatung",
    "consentMode": "reject",
    "subject": "scan-9f1c0b2d3e4f5a6b7c8d9e0f",
    "note": "post-CMP-upgrade state"
  },
  "baseline": { …exactly store.Baseline, Result included by value… },  // absent when op is revoke
  "refs": ["result/<sha>", "screenshot-after-consent/<sha>"]           // absent when op is revoke
}
```

**The approved result is embedded by value, exactly as SQL embeds it.** The
existing comment on `store.Baseline` states the reason: "Result is a copy of
the approved scan. It is stored by value so that retention pruning of old
results cannot silently invalidate a baseline." Referencing the document
instead would give the blob store a `GetBaseline` path that can return
`ErrEvidenceGone`, which `scanner.compare` turns into
`diff.ReasonEvidenceGone` on **every subsequent scan of that target** — a
visible difference in compliance output between store kinds, which is
precisely what AC2 exists to exclude. Approvals are human-generated and rare;
the duplicated bytes are bounded and are the price of parity.

The decision object still writes `ref/<kind>/<digest>/d.<did>` markers for
every artifact the copy names, because the copy's screenshots and stored
bodies live in the bucket and must be pinned. That is the exact analogue of
SQL's `liveOwners` union.

**Write order, identical for approve and revoke:**

1. Read the result (`GetResult`) and validate it as SQL does — a scan that is
   not `OK()` cannot be a baseline. *(approve only)*
2. **L** `baseline/<tk>/<mk>/` — one listing, which gives the current
   decision, the newest `inv`, and the keys the self-heal in step 6 needs.
3. Compute `at = max(Options.Now(), newestDecisionAt + 1ns)`, the nonce, the
   canonical body and `<did>`. All of it **once, above the retry loop**, so a
   replay writes identical bytes at an identical key (AC10, I4).
4. **W** `ref/<kind>/<digest>/d.<did>` for every ref. *(approve only)*
5. **W** `baseline/<tk>/<mk>/<inv>.<stamp>.<op>.<did>` — **the commit point.**
   The decision is now in effect and fully recorded, in one object: this is
   AC9's "one write carries both" satisfied literally, and a partial write
   leaves neither the approval nor its audit entry.
6. **W** `audit/<inv>.<stamp>.<did>`, body = the `audit` sub-object alone
   (~250 B), a *derived pointer* into the decision. Then re-issue, with
   `IfNotExist`, any audit pointer missing for the other decisions this
   series' listing showed in step 2 — the self-heal, bounded by the number of
   decisions in one series.

Both orders are the same and both fail safe. For **approve**, the object
missing means the previous state stands — an approval that did not land does
not silence anything. For **revoke**, the object *is* the effective state, so
a revoke that lands is immediately in effect and cannot leave findings
silenced against a withdrawn approval; only the derived audit pointer can lag,
and it heals. There is no ordering asymmetry to remember and no direction in
which the store is fail-unsafe.

**Reads:**

* `HasBaseline` — **L** `baseline/<tk>/<mk>/` page 1. The newest key's `<op>`
  answers it. **Zero GETs.**
* `GetBaseline` — the same listing; `revoke` → `ErrNotFound`; `approve` →
  **G** the object → return its `baseline`. A listed key whose object is
  missing is **`ErrCorrupt`**, never `ErrNotFound`: nothing in this store
  deletes a decision object, so absence there is corruption, and inferring
  "no baseline" from it would be the quiet lie Tenet 5 forbids.
* The fold (AC8): newest = smallest `inv`; ties at equal `inv` broken by
  `<did>` ascending, which is a content digest and therefore host-independent.
  Every decision, winner and loser, remains visible in the audit log.
* `Audit(n)` — **L** `audit/` page `n`, **G** each (small). Falls through to
  `auditckpt/` when `n` exceeds the loose pointers.
* `RecordAudit(e)` — one **W** to `audit/<inv>.<stamp>.<did>`, where `<did>`
  hashes the canonical entry **including a nonce drawn once above the retry
  loop**. Without the nonce two identical actions in one clock tick — certain
  under a pinned test clock, possible on a coarse-resolution timer — would
  collapse to one key, and deleting an audit record because it resembled
  another one is not acceptable.

**The audit prefix holds only derived pointers for baseline decisions, and
authoritative bodies for plain `RecordAudit` entries.** Audit compaction may
therefore fold and delete `audit/` keys freely under the normal checkpoint
rule (§7.2) without ever endangering a baseline. The arrow points from audit
to nothing; the decision object is self-contained.

**Ordering divergence, resolved.** SQL's `Audit` orders by `id desc`, the
autoincrement, i.e. insertion order, while `RecordAudit` accepts a
caller-supplied `At`. The blob store orders by inverted `At`. Rather than
document a divergence, **this story changes SQL's query to
`order by at desc, id desc`** — a one-line change with a shared test. If the
suite is the specification of what a store does, the two must agree.

### 6.6 The overlay and the cache

**Neither of these was built.** §7.6 #6 records the decision and its
consequences — every read of the store is a fold over what the bucket currently
shows, AC7's determinism is global rather than process-local, read-your-writes
is not promised to the writing process either, and §8.3 test 1 and §10's README
bullet were both rewritten because of it. The section is kept as designed
because the argument below is what a later step would have to answer to add
either mechanism, and because the deletion-recording requirement is the part
that would be easiest to get wrong.

Two mechanisms, both safe by construction under I1.

**The recent-write overlay** is a bounded map from index key to
present-or-absent, holding what *this process* wrote or deleted in the last
`overlayTTL`, evicted by age and by `overlayMax`. Every fold unions the
present entries in and removes the absent ones. It **must record deletions**:
without that, a process that prunes a result it wrote two minutes earlier
resurrects it into its own listing — deterministic on any deployment with
`maxPerSeries` set and a scan interval under fifteen minutes.

This is not inference. It is evidence this process produced, whose write or
delete returned success. It closes AC6's window for the single-node
deployment AC13 defines. It does make AC7's determinism *process-local*, and
that is stated in the README: a just-written result is visible immediately to
the writing process and within the provider's convergence window to any
other. Blob-only tests observe the bucket without the overlay by opening a
second `Blob` handle on the same location; no test knob is needed.

**The immutable-object cache** is a 16 MiB LRU over object bodies: entries,
checkpoints, target markers and decision objects. Safe because no key is ever
rewritten, so a cached body can never be stale. **Listings are never cached.**

### 6.7 Errors

| condition | error |
|---|---|
| no such entry in a verified-complete fold | `ErrNotFound` (unchanged) |
| the document an entry names is gone | `ErrEvidenceGone` (unchanged) |
| a decision object listed but absent; a digest mismatch; an undecodable body on a path that cannot skip | `ErrCorrupt` (unchanged) |
| a listed checkpoint whose object is absent; the overall budget exceeded mid-fold | **`ErrIndexIncomplete`** (new sentinel) |

`ErrIndexIncomplete` wraps neither `ErrNotFound` nor `ErrEvidenceGone`, so no
caller reads it as "there is no previous scan" and none reads it as an
evidence loss.

**A correction to a claim made in earlier drafts, which must not survive into
the code comments.** `scanner.compare` sets `baseline = nil` for *every*
error from `baselineFor`; the only sentinel that changes the produced report
is `ErrEvidenceGone`. So returning `ErrIndexIncomplete` rather than
`ErrNotFound` changes the log level (`Warn` instead of silence) and nothing
else today: the scan still produces a first-ever-scan-shaped report. Giving
the scanner and `internal/diff` a third reason for "the index could not
answer" is a real change in two packages outside this story's scope. **The
call: 8.10 does not change `internal/scanner` or `internal/diff`.** The gap
is recorded in §12 as the follow-up story it is. What the sentinel buys today
is a distinguishable log line and a correct HTTP status, and that is the
honest description to put in its doc comment.

---

## 7. Compaction and retention

### 7.1 Cost per operation

| operation | LIST | GET | PUT | DELETE |
|---|---|---|---|---|
| `ListResults(n)` | 1–2 | n | | |
| `LatestResult` | 1 | 2 | | |
| `GetResult` | 0 | 2 | | |
| `HasResult` | 0 | 1 (attributes) | | |
| `PreviousResult` (anchor at depth k) | ⌈k/1000⌉ + 1 confirm | 3 | | |
| `Series()` (S series) | ⌈S/1000⌉ | S | | |
| `GetBaseline` | 1 | 1 | | |
| `HasBaseline` | 1 | 0 | | |
| `Audit(n)` | 1 | n | | |
| `PutResult` | 0 | 0 | 3 + one per artifact (+1 the first time in a series) | 0 |
| `SetBaseline` | 1 | 2 | 2 + one per artifact | 0 |
| `DeleteBaseline` | 1 | 0 | 2 | 0 |
| `RecordAudit` | 0 | 0 | 1 | 0 |
| full walk of a 10,000-scan series | 1–2 | 12 checkpoints + 200 loose | | |
| `Prune`, per series | 1 (+1 GET per checkpoint) | only for clamped entries | 1 tombstone per pruned result | 2 + refs + artifacts per pruned result |
| `Sweep` (A artifacts, R ref markers) | ⌈A/1000⌉ + ⌈R/1000⌉ | 0 | 0 | per collected |

**Read this table with §7.6 beside it.** Eight of its rows are wrong against
the implementation, and every one of them is corrected in §7.6 rather than
here, so that what was designed and what was built stay distinguishable:
`PutResult` and the take marker `PutArtifact` now writes (§7.6 #1), `Prune`
and `Sweep`, which gained a delete per stale take (#1) and, for the sweep, an
attribute read per pinned artifact (#5), `RecordAudit` and `Audit(n)` (#7),
`PreviousResult` (#8), and `SetBaseline`/`DeleteBaseline` (#7 and #9).
**Where the two disagree, §7.6 is what the code does and what step 12's
request-count assertions (AC11) must be written against.**

Two consequences worth stating plainly.

**Prune by age decides from `inv`**, which is a total function of `StartedAt`,
so pruning by age is LIST-only and reads no bodies. The one exception is an
entry whose `inv` sits at a clamp sentinel; those get a body GET so the
recorded `StartedAt` decides. Prune by count decides from the fold's position
and reads no bodies at all.

**`Sweep` is an ordered merge join, not one listing per artifact.**
`ref/<kind>/<digest>/<owner>` and `<kind>/<digest>` both sort by
`(kind, digest)`, and §4.2 guarantees both listings come back in the same
order on every provider. So the sweep streams the two listings per kind side
by side in O(1) memory. The naive form — one listing per artifact — would be
twelve million requests on the reference deployment below.

### 7.2 Compaction

Three prefixes grow without bound: `series/<tk>/<mk>/`, `audit/`, and slowly
`baseline/<tk>/<mk>/` (which is left alone — human-generated volume).

* **Threshold:** more than `compactAfter` loose `r.` keys in a series
  directory, or more than `compactAfter` loose keys under `audit/`.
* **Keep:** the newest `compactKeep` stay loose, so the hot path never reads a
  checkpoint.
* **Writes** one object at `series/<tk>/<mk>/k.<inv of the newest covered
  entry>.<gen>`:

```jsonc
{ "layout": 1, "kind": "checkpoint",
  "coversFrom": "<oldest covered entry key>",   // documentation, never a predicate
  "coversTo":   "<newest covered entry key>",   // documentation, never a predicate
  "count": 800,
  "entries": [ …entry bodies, in key order… ],
  "tombstoned": ["scan-…"] }
```

No wall-clock field, so the bytes — and therefore `<gen>` and the key — are a
pure function of what was folded. An interrupted compaction that re-runs
writes the identical object and the partial attempt is a no-op (I4, AC10).

* **Delta, never cumulative.** A checkpoint covers only the entries between
  the previous watermark and its own. Cumulative checkpoints rewrite all
  history on every compaction, which is O(n²) bytes. Ten thousand scans in a
  series is about twelve checkpoints. Cap at `checkpointMaxBytes` /
  `checkpointMaxEntries`.
* **No `prev` chain.** Completeness comes from listing the whole
  `series/<tk>/<mk>/` prefix and unioning every visible `k.` object. A chain
  is a graph that two concurrent compactors can fork, and a fork strands
  every entry in the orphaned branch after its loose keys are deleted. A
  union cannot fork; two concurrent compactors just produce two checkpoints
  that overlap, and the union dedupes by scan ID.
* **Checkpoints are never deleted**, by any operation in this store.
* **Trigger, with no new background loop** (AGENTS §4): an in-memory
  per-series write counter checked at the end of `PutResult`, and any fold
  that had to walk more than `compactAfter` loose keys to answer — which has
  already read every body it needs, so the compaction is one PUT of what is
  in hand. It deliberately does *not* hang off `Prune`, because `PruneLoop`
  returns immediately when neither `maxAge` nor `maxPerSeries` is set, and a
  blob store with retention off would then never compact.

**Deletion of covered keys — invariant I3 in full:**

> A loose entry key `K` is deleted only if a checkpoint `C` exists such that
> `C.entries` contains `K`'s **scan ID**; `C` is visible in a listing taken
> *now*, in this run; and `C`'s **bucket-reported `ModTime`** is older than
> `Options.CheckpointGrace`. If `ModTime` is unavailable or in the future,
> nothing is deleted and the run logs it.

Deletion is by **membership, never by key range**. A range predicate deletes
any key that sorts inside the covered span, including one written after the
compactor's listing was taken and therefore absent from `entries` — which
loses the entry permanently, with a single process and one stale listing.
`coversFrom`/`coversTo` stay in the body as documentation and are never a
predicate.

Using the **bucket's** clock rather than the host's keeps the one
time-dependent decision in the design off the local clock entirely: there is
one clock, the provider's, and a `ModTime` in the future disables deletion
rather than enabling it. 24 hours is not derived from a published convergence
window — S3 has been strongly consistent for LIST since December 2020, but
`gocloud.dev/blob` also reaches GCS, Azure, MinIO and Ceph RGW, and the last
two publish nothing. It is chosen to be far beyond any plausible window, and
the only cost of being generous is deferring the deletion of 450-byte
objects.

**Tombstone collection.** The same pass deletes a `d.<inv>.<sk>` key when all
three hold: no `r.` key for that scan ID is visible in the listing taken now;
that scan ID is in no visible checkpoint's `entries`; and the tombstone's
bucket `ModTime` is older than `tombstoneGrace`. Tombstoned scan IDs covered
by a checkpoint are folded into the next checkpoint's `tombstoned` array
first, so they can be dropped under the ordinary I3 rule.

If `Sweep` and `Prune` never run, index keys accumulate: reads get slower,
correctness is untouched. That is exactly AC17's "a history whose listing gets
slower as it grows unless compaction is left on".

### 7.3 `Prune`

Per doomed result, in this order:

1. **W** `series/<tk>/<mk>/d.<inv>.<sk>`, the tombstone — **always,
   unconditionally, first.** Not "if the entry is inside a checkpoint": that
   is a branch on a listing, and a stale listing that hides the checkpoint
   skips the tombstone and leaves the pruned result in every listing for the
   life of the checkpoint, while `PruneStats.ResultsDeleted` counted it. One
   zero-byte PUT per pruned result buys the property that a prune's effect
   does not depend on a listing being fresh.
2. **D** `series/<tk>/<mk>/r.<inv>.<stamp>.<sk>.<tm>` if it is loose. A
   failure here is harmless; the tombstone already hides it.
3. **D** `byid/<tk>/<mk>/<sk>`. **Before** the refs and the artifacts, so a
   reader that loses the race gets a clean `ErrNotFound` rather than the
   `ErrEvidenceGone` the interface renders as lost evidence — Story 5.17 AC3
   exists to keep "pruned" and "broken" apart. A `byid` delete that keeps
   failing is retried, then counted in `PruneStats.IndexKeysFailed` (new,
   `omitempty`) and logged as a key that is **still reachable by scan ID**,
   because folding it into `ArtifactsFailed` would read as "some bytes were
   not reclaimed" when it is in fact a retention policy that left the record
   fetchable through a share link.
4. **D** `ref/<kind>/<digest>/r.<tk>.<mk>.<sk>` for each artifact it named.
5. For each of those artifacts: **L** `ref/<kind>/<digest>/` page size 1.
   Delete the artifact only if the prefix is empty **and** the artifact's own
   bucket `ModTime` is older than `unreferencedArtifactGrace`. The grace is
   the part that matters: content addressing means a scan finishing right now
   can write the identical screenshot bytes as a no-op and land its ref marker
   moments later, so an empty prefix alone is not evidence that nothing needs
   the object. Deletion is decided by reference and by age, never by age
   alone (Story 8.5 AC1).

Before the per-result loop, once per series:

* **Positive baseline protection.** The prune already folds the series, so it
  also lists `baseline/<tk>/<mk>/`, reads the current decision, and unions its
  `refs` into a protected set for this run. This restores SQL's `liveOwners`
  guarantee by a positive check rather than by the absence of a marker, which
  a stale listing can fake.
* **Reaping superseded pins.** Every `d.<did'>` marker whose `<did'>` is not
  the current decision's may be deleted, provided the current decision was
  observed in *this* run and the marker is older than
  `unreferencedArtifactGrace`. Without this, every superseded and every
  revoked baseline would pin its result document and every screenshot it named
  for the life of the bucket, and "retention deletes what it stops
  referencing" would be false. A revoked series has no current decision, so
  all its `d.` pins are reaped — which is exactly what SQL does when
  `DeleteBaseline` drops the row.
* **Audit-pointer self-heal**, as in §6.5 step 6.

After the loop, if the series has no entries and no checkpoints left,
**D** `targets/<tk>.<mk>`. SQL's `Series()` is a `select distinct` over
`results`, so a fully expired series disappears from it; without this the
dashboard would render decommissioned targets forever. `PutResult` re-creates
the marker with `IfNotExist`, so the race is harmless.

Every step is idempotent — deleting an absent key is a no-op — so an
interrupted prune resumes by running again. A failed delete does not fail the
prune (Story 8.5 AC4): logged, counted, left for the next pass.
`PlanPrune` writes nothing at all, including no tombstone and no self-heal.

### 7.4 `Sweep`

Two passes, both ordered merge joins, plus two blob-specific rules.

1. **Dangling owners.** Stream `ref/<kind>/` and check each owner: an
   `r.<tk>.<mk>.<sk>` owner whose `byid/<tk>/<mk>/<sk>` is absent, and a
   `d.<did>` owner whose decision object is absent, are leftovers of an
   interrupted write or an interrupted prune. They are deleted after
   `unreferencedArtifactGrace`. This is the analogue of SQL's `sweepDangling`.
2. **Unreferenced artifacts.** Stream `<kind>/` against `ref/<kind>/` and
   collect what has no marker, subject to the existing grace.
3. **`result/` objects are never collected.** AC6 says a document visible
   without its index entry "is not silently lost (Story 8.5's sweep must not
   collect it)", and that is unconditional — a 24-hour grace protects it for
   a day and then deletes it, which does not meet the AC. A result document
   is self-describing: it decodes to a `model.Result` carrying its scan ID,
   target and mode, so it is a rebuild candidate and not garbage. It is
   reported as `SweepStats.ResultsWithoutEntry` (new, `omitempty`) and left
   in place for `wsaw store rebuild-index`. Screenshots and stored bodies are
   *not* self-describing and keep the ordinary grace-based collection.
4. **A rebuild in progress suppresses collection entirely.** If any key exists
   under `_wsaw/index/v1/rebuild/`, the sweep sets
   `SweepStats.UnknownReferences` to the number of such markers and collects
   nothing. This is the blob analogue of SQL's `resultsWithUnknownRefs`
   safety valve, which has no counterpart here because "I have no ref marker
   for this artifact" and "I have not derived the ref markers yet" are the
   same observation. Without it, a rebuild interrupted at 40 % followed by a
   sweep deletes the evidence of the other 60 %. Story 8.10 implements the
   check; Story 8.11 writes the marker.
5. **Orphaned `byid` keys.** A `byid` key with no `r.` entry, no tombstone and
   no checkpoint membership is a prune leftover; it is deleted after
   `tombstoneGrace`. Without this pass nothing ever cleans up a `byid` whose
   delete failed in step 3 of §7.3, and the result stays fetchable by scan ID
   indefinitely.

### 7.5 Cost at scale

Reference deployment: 200 targets × 3 consent modes × 10,000 scans = 600
series, 6,000,000 results. Steady state: ~6 M `byid` + ~12 M `ref` markers +
~120 K loose entries + ~7 K checkpoints ≈ 18 M index objects, alongside the
documents and screenshots.

S3 us-east-1 (LIST/PUT $0.005 per 1,000; GET $0.0004 per 1,000):

| | requests | cost | wall clock |
|---|---|---|---|
| `ListResults(100)` — a target page | 1 LIST + 100 GET | $0.000045 | ~180 ms at 16-way |
| Dashboard render: 600 × `ListResults(…,1)` + 600 × `HasBaseline` | ~1,200 LIST + 600 GET | $0.0062 | ~3 s cold, ~1 s warm |
| `GetResult` + document | 2 GET | $0.0000008 | ~60 ms |
| Full walk of one 10,000-scan series | 1 LIST + 212 GET | $0.00013 | ~0.5 s |
| `Prune`, per result actually removed | ~9 requests | $0.00004 | |
| A daily prune removing 60,000 results | ~540,000 | $2.60 | ~10 min at 8-way |
| `Sweep` over the whole store | ~22,000 LIST | $0.11 | ~2 min at 32-way |
| Story 8.11 rebuild: list everything | ~24,000 LIST | $0.12 | ~2 min |
| Story 8.11 rebuild: read every document | 6,000,000 GET | $2.40 + egress | hours |

**On a local filesystem the picture is materially worse, and the operator
needs to know it.** `fileblob` writes a `.attrs` sidecar beside every object,
so 18 M index objects become 36 M files and 36 M inodes. A 400 GB ext4 with
the default inode ratio has about 25 M inodes, so you exhaust inodes before
space, and 450-byte objects rounded to two 4 KiB blocks give roughly 20×
allocation amplification. Per-directory load is fine — a series directory
holding thousands of files is single-digit milliseconds on ext4 `dir_index`
and APFS — it is the whole-filesystem accounting that fails. **At this scale
on local disk, SQLite is the right store.** That sentence goes in the README.

### 7.6 Amendments made while implementing §6 and §7 (step 11)

Eleven things in §6 and §7.1 to §7.5 above are wrong against the tree they were
written for, and the implementation deviates from them deliberately. They are
recorded here because §8 and step 12 are written against §6 and §7: an
assertion written from the sections above and not from this one will fail
against the code, and weakening it to match would be the worst available
outcome for AC11.

1. **The prune needs a take record, and §7.3's `ModTime` grace cannot stand in
   for one.** §7.3 step 5 deletes an artifact when its pin prefix is empty and
   *the artifact's own* `ModTime` is past the grace. That fails the shared suite
   in both directions at once, and no combination of the two facts can pass it:
   `TestPruningDeletesTheArtifactsItStopsReferencing` requires a screenshot
   written seconds ago to be collected when the result naming it expires, while
   `TestRetentionKeepsAnArtifactARunningScanHasJustTaken` requires an identical
   screenshot to be kept because a second scan re-captured the same bytes — and
   a re-capture of unchanged bytes writes **nothing at all** to the bucket, so
   the two states are indistinguishable from outside. The SQL stores tell them
   apart with the claim row of `claims.go`, and
   `TestSweepCollectsAnUnreferencedArtifactOnlyAfterTheGracePeriod` needs the
   same record for a third reason: without it a store that has stored one
   artifact and no result has an *empty index* and the sweep refuses.

   So `PutArtifact` writes one zero-byte **take marker**,
   `ref/<kind>/<digest>/t.<inv>`, and retention honours it: a take protects the
   artifact while it is younger than `unreferencedArtifactGrace` **and newer
   than every pin on that artifact**. The second half is the release SQL
   performs by deleting the claim row, expressed without a delete on an ordinary
   write path (AC3): a pin written after a take is the result the take was
   waiting for. Both times come from the bucket's own clock. A document is not
   taken — it is written by `putDocument`, named by exactly one result, and no
   other scan can content-address to it — which is also what SQL does. The
   owner tag is `"t"` so that takes sort after `d.` and `r.`. Stale takes are
   reaped by prune and sweep, as `forgetStaleClaims` is.

   **Cost:** `PutArtifact` gains one PUT (§7.1 listed none), and the prune and
   sweep gain one DELETE per stale take. A SQL store pays one row upsert for the
   same fact.

2. **A pin is deleted after the artifact, not before (§7.3 steps 4 and 5 swap).**
   The pins are the work list, exactly as the reference rows are in SQL: a pin
   removed before an artifact the bucket then refuses to delete leaves that
   object with nothing pointing at it, and the next sweep finds it young and
   unreferenced and protects it by age — a refused delete turned into a leak.
   `TestADeletionFailureDoesNotFailThePrune` requires the next sweep to collect
   it. Consequently the pin listing of step 5 is also taken **before** any pin
   is deleted, which is what lets the take-release comparison see the pin times
   this prune is removing; that is safe because a scan writes its take before
   the pin that will release it, so anything that could add a pin after the
   listing is already in it as a take.

3. **The sweep's grace applies only to an artifact no pin has ever covered.**
   §7.4 gives the dangling-owner pass a grace of its own; SQL's `collectDangling`
   has none, and `TestADeletionFailureDoesNotFailThePrune` fails with one. A pin
   — live or dangling — is proof that a stored result named these bytes, so the
   object's age says nothing; an object with no pin at all is what an
   interrupted write looks like, and that is where the grace belongs. The
   interrupted-`PutResult` case §7.4 was protecting is covered by the take
   instead, which is written before the pin.

4. **`result/` objects are protected unless `AllowEmptyIndex` is set.** §7.4 #3
   says never, unconditionally.
   `TestASweepRefusesAnIndexThatKnowsNothing` requires the override to collect an
   orphaned document, and it must: the rule rests on the premise that there is an
   index to rebuild from, and the override is the operator stating there is not.
   Counted as `SweepStats.ResultsWithoutEntry` either way.

5. **The sweep costs one attribute read per artifact that carries a pin**, not
   the zero GETs §7.1 and §7.5 claim. §7.4 #1 requires each owner to be checked
   and there is no listing that answers it: pins arrive ordered by artifact
   digest and `byid/` by series, so the two cannot be joined. The check is lazy —
   `fate` stops at the first pin that survives — so the healthy case is one read
   per artifact. Decision pins cost nothing: every visible decision digest is
   read once per run, because `d.<did>` is not a key. Corrected figures for the
   reference deployment: about 47,000 LIST (the artifact side pages at
   `artifactListPageSize` = 256, not 1,000) and about 6 M attribute reads, so
   roughly **$2.60 rather than $0.11** for a whole-store sweep. It stays an
   operator-invoked command and not a timer.

6. **§6.6's recent-write overlay and immutable-object cache are not built**,
   and their absence is the design decision rather than an omission. Without an
   overlay, every read of this store is a fold over what the bucket currently
   shows: the answer is a function of one visible key set and of nothing
   process-local. Three consequences, all of them recorded in `blob.go`'s
   package prose as well.

   * **AC7's determinism becomes global rather than per process.** Two handles
     on one bucket give the same answer, which is a stronger property than the
     one §6.6 offered and a simpler one to test — there is no "in-process view"
     for a test to have to reach around.
   * **The hazard §6.6 spends most of its own argument on disappears.** A
     remembered write cannot resurrect a key a prune has since deleted, because
     nothing is remembered.
   * **What it costs is read-your-writes.** Against a provider whose listings
     lag, a `PutResult` followed by a `ListResults` in the *same* process can
     omit the scan just written. It is reported as "not there yet" and never as
     deleted (§6.7). §10's README bullet is corrected accordingly: the claim
     that "the process that wrote it always sees it" is **struck**, because the
     store does not provide it.

   **§8.3 test 1 must therefore be rewritten** before step 12 implements it. It
   is specified as "in-process the overlay returns it; through a second `Blob`
   handle on the same bucket `ListResults` returns the older results", and there
   is no such asymmetry to observe. What it should assert instead is tolerance:
   a write whose key the listing hides is absent from the fold, absent
   identically through either handle, never reported as an evidence loss on any
   path, still readable by scan ID through `byid`, and present in every later
   read once the key becomes visible — which is AC6's actual promise. A body cache can be added later without a
   layout change, because no key is ever rewritten.

7. **§7.1's audit and baseline rows are wrong about their listings.**

   * **`RecordAudit` is 1+ LIST + 1 PUT**, not 0 LIST. `freeAuditInstant`
     probes for a free nanosecond, one `listIndex` of the ordering field's own
     prefix per probe, up to `auditInstantProbes` = 64 — one listing in the
     ordinary case, more only when entries genuinely share a nanosecond. The
     reason is in that function's doc comment and it is not optional: the shared
     suite requires two entries recorded at one instant to come back in
     recording order, and a bucket offers nothing but the key to get an order
     from.
   * **`Audit(n)` is 1 LIST + n GET, plus a second LIST whenever the loose keys
     do not fill the answer** — which is every call against a log shorter than
     the limit, i.e. every call in a small deployment. The second listing is
     `noAuditCheckpoints`, and it is what turns "the log has nothing more" into
     `ErrIndexIncomplete` when what it actually has is a compaction this build
     cannot read (§6.7, #10 below).
   * **`SetBaseline` and `DeleteBaseline` pay up to `auditHealProbes` = 8
     attribute reads** beyond the GETs the table shows, for the self-heal of #9.
     The count is bounded by that constant and not by the size of the series;
     the healthy case pays eight `statIndex` calls that all find the pointer
     present.

8. **`PreviousResult`'s confirmation re-folds the whole prefix**, where §6.4
   re-lists only the key range between the candidate and the anchor. The cost
   is 2⌈k/1000⌉ listings for an anchor k scans deep, not ⌈k/1000⌉ + 1, and a
   third fold on disagreement. It is deliberate, and `previousOf`'s doc comment
   carries the argument: the narrow form can only see a scan that appeared
   *between* the candidate and the anchor, while the case that matters is a
   stale listing view omitting a scan **newer** than the candidate — the entry
   that would have been the answer. Only a fold from the same starting key
   produces two answers that are comparable at all. Strictly safer than the
   specified form and strictly more expensive; the figure above is what step 12
   asserts.

9. **The audit pointer heals on the next decision of its series, not on the
   next read**, which is a deviation from AC9's wording and not from its
   substance. The decision object holds the approval *and* its audit entry in
   one body under one key, so a partial write leaves neither and the record
   itself is never at risk; what a process that died between the two objects
   leaves behind is a derived pointer under `audit/` that `Audit()` reads.
   `healAuditPointers` repairs it from the listing the next decision of that
   series has already made, bounded to the newest `auditHealProbes` = 8
   decisions, because that is where the gap can be — a pointer is missing only
   when a process died at the head of the series.

   `Audit()` does **not** heal and does not report the gap, and the alternative
   was rejected on cost: healing on read means listing every series' baseline
   prefix on every read of the log, which is the one read path that has no
   series to scope it. So a series that takes no further decision keeps its gap,
   and finding it is Story 8.11's `--verify`. **The exposure, stated plainly so
   that it is a decision on the record: an approval whose pointer was lost is in
   force and correct and readable through `GetBaseline`, and is missing from
   `Audit()` until either another decision of that target is taken or a verify
   pass runs.**

10. **`Options.CheckpointGrace` is declared and nothing reads it.** The rule it
    feeds is I3's deletion rule, whose only deleter is compaction — step 13 —
    so in this build setting it changes no behaviour. It is declared with the
    store it belongs to rather than with its consumer so that the option set an
    operator and a test see does not change shape when compaction lands, and
    both declarations say in prose that the rule is not in force. A reader must
    not take the field's presence for the rule's presence, and a step-13 author
    must not take the assignment in `OpenBlob` for a wired grace.

    The same is true of two constructors: `auditCheckpointAt` (step 13 writes
    audit checkpoints; this build only detects that one exists and refuses the
    read) and `rebuildKey` (Story 8.11 writes rebuild markers; this build only
    honours them by collecting nothing while one is visible). Both say so on
    their doc comments.

11. **`dialectFor`'s refusal of the blob driver names no command**, where §2.3
    asked for "a message pointing at `wsaw store rebuild-index`". That command
    is Story 8.11's and this binary does not have it, and a message naming a
    command an operator cannot run is worse than one naming none. The message
    carries the direction instead — the documents are already in the same bucket
    layout in all four stores, so moving a history between a database store and
    this one is not a migration in either direction and the index is rebuilt
    from the documents rather than converted — which is the half of AC16 an
    operator at a dead end actually needs.

Two shared tests also had to become driver-aware, and both are recorded beside
the assertion. `TestRetentionRefusesToRunWithoutTheArtifactBucket` asserts that
the history outlives the bucket, which a store whose index *is* the bucket
cannot promise; it asserts instead that the store reports itself unreachable
rather than reporting an empty history. `TestASweepLeavesWhatWsawDidNotWrite`
plants four foreign objects, two of which are shaped like a bucket index, and the
bucket-index store counts one fewer stray than the SQL stores do. **Not because
it judges the fourth object safe** — an earlier draft of both this paragraph and
the test's own comment said that, and it was never true. The blob sweep walks the
evidence plus exactly the two index prefixes it needs work lists for, `ref/` and
`byid/`, and never lists `audit/` at all, so the planted audit entry is out of
its reach rather than inside it and cleared; the planted pin is inside its reach
and is counted as a stray, because "owner" is no owner the key grammar spells.
Same number, and the reason matters to whoever later grows the sweep a pass over
`audit/`. Both stores leave all four objects exactly where they are, which is
what the criterion is about.

---

## 8. Test plan

### 8.1 The existing suite, unchanged (AC15, Story 8.9 AC1)

`store_test.go`'s `open(t)` is already parameterised on
`WSAW_TEST_STORE_DRIVER`, and it is used **36 times: 28 in `store_test.go`
and 8 in `retention_test.go`**. Those 36 are the shared specification and
they must all run against `blob`. Changes:

```go
// open(t) returns store.Store instead of *store.Store — one line.

// storeOptions(t) gains:
case store.DriverBlob:
    opts.Driver = driver          // ArtifactDir is already set; the bucket is the store

// retention_test.go: withEvidence, assertStored, assertGone take store.Store.
```

plus a `blob` row in `TestDriverIsReported`. The eight in `retention_test.go`
are the hardest ones for this store and are where commits 10 and 11 land:
`TestPruningKeepsWhatABaselineNames`,
`TestSweepCollectsAnUnreferencedArtifactOnlyAfterTheGracePeriod`,
`TestADeletionFailureDoesNotFailThePrune`,
`TestASharedArtifactSurvivesThePruneOfOneResult`, and four more.

The tests that use `sqliteOptions(dir)` are SQL-specific by construction —
pragmas, schema version, migration idempotence, newer-schema refusal, file
permissions, the `hasColumn` / `resultRow` / `documentPath` helpers — and
change only `store.Open` → `store.OpenSQL`.

**Five of them are behavioural rather than SQL-specific**, and four cannot be
lifted as they stand because they call `resultRow(t, dir, scanID)`, which
opens `dir/wsaw.db` and reads three columns:
`TestResultDocumentIsStoredInTheBucket`,
`TestDigestMismatchIsCorruptionNotEvidence`,
`TestTruncatedDocumentIsCorruption`,
`TestAMissingDocumentLeavesTheRestReadable`. To lift them, add one
driver-agnostic helper and switch those four to it:

```go
// documentRefFor finds the stored document of one scan by reading the
// artifact directory, so a test can assert on it without knowing whether the
// store keeps its index in rows or in objects.
func documentRefFor(t *testing.T, artifactDir, scanID string) (ref string, size int64, digest string)
```

It walks `<artifactDir>/result/`, decodes each object, and returns the one
whose `scanID` matches — with its size and its SHA-256, which is what the
three columns held. `TestListingNeedsNoDocumentAtAll` needs no helper: it
removes the whole `result/` directory and is liftable as it is.

```make
.PHONY: test-store-blob
test-store-blob:
	WSAW_TEST_STORE_DRIVER=blob go test -count=1 ./internal/store/
```

Unlike Postgres and MySQL this needs no container, so it joins the default CI
job as well as `test-store-all`.

### 8.2 The fake bucket

A test-only `gocloud.dev/blob/driver.Bucket` wrapping `memblob`, registered
under a `lag://` scheme in a `package store` test file. `bucket_test.go`
already sets that precedent for `mem://`: neither is reachable from the
shipped binary, not because of a denylist (there is none; the comment in
`bucket.go` overstates it) but because `memblob` is registered only by the
test file and is not linked into the binary. The same mechanism covers
`lag://`. Wrapping at the driver level means every injected fault travels
through the real `blob.Bucket` and the real `internal/store/bucket.go` path.
Internal and external test files link into one binary, so the black-box
`store_test` suite reaches it through `ArtifactDir: "lag://…"`.

| knob | injects | failure mode it tests |
|---|---|---|
| `hideFromList(key, n)` | a written key invisible to the next *n* listings | a just-written key absent from a listing |
| `listSnapshot(age)` | listings served from a pinned snapshot | a stale listing view |
| `visibleAfter(key, n)` | per-key counters, so B appears before A | out-of-order visibility |
| `getMisses(key, n)` | `NewRangeReader` returns NotFound for a just-written key | no read-after-write on GET |
| `dropResponse(key)` | the write lands, the call returns an error | ambiguous failure → retry idempotence |
| `ifNotExistIsALie` | both racing conditional writers report success | I1 comes from key derivation, not from the condition |
| `deleteBehindListing(key)` | a deleted key still listed | prune order, compaction grace |
| `failWrite(pattern)` | a PUT that fails at a chosen key | interrupted compaction, interrupted prune |
| `failPage(n)` | one page of a multi-page listing fails once | the token discipline in `bucket.list` |
| `corruptBody(key)` | a truncated or garbage body | a bad entry is skipped and reported, never fatal |
| `futureModTime(key)` | `ModTime` ahead of the local clock | the grace check disables deletion rather than enabling it |
| `count(op)` | per-operation request counters and a `t.Cleanup` assertion helper | **AC11** |
| **`strict` (default on)** | fails any write to an existing key with different bytes, and any delete of a key this run has not listed | **I1 and I3, enforced on every blob test rather than reviewed once** |

Plus `Options.Now` for every clock scenario and `Options.CheckpointGrace` for
compaction timing. **No test sleeps** (AGENTS §5).

### 8.3 The blob-only tests

1. **Delayed listing omits a just-written key.** *Rewritten by §7.6 #6: the
   overlay this was drafted against does not exist, so there is no in-process
   view to contrast with a second handle's, and the original wording cannot
   pass.* What to assert instead is tolerance. With the entry key hidden from
   listings, `ListResults` returns the older results with no error and never
   reports the scan as removed; the answer is byte-identical through the
   writing handle and through a second `Blob` handle on the same bucket, which
   is the stronger property the missing overlay buys; `GetResult` still
   succeeds, because it reads `byid` and not a listing; and once the key
   becomes visible the scan is in every fold. Assert that the error in the
   missing case is not `ErrNotFound` anywhere a caller would read it as
   "deleted".
2. **Stale listing view.** Two reads over one pinned snapshot return
   byte-identical answers, the same `LatestResult` and the same
   `PreviousResult` (AC7).
3. **Out-of-order visibility, property-style.** Write N entries, reveal them
   in several shuffled orders, assert every fold over the same visible set is
   identical.
4. **Two concurrent writers of one logical fact.** (a) Two goroutines call
   `SetBaseline` with identical arguments and an identical injected clock →
   exactly one decision object and one audit pointer, byte-identical, under
   `-race`. (b) Different scan IDs → two decisions, a deterministic winner,
   and **both in `Audit()`** (AC8).
5. **Revoke then re-approve under a pinned clock.** Three operations produce
   three distinct keys with strictly increasing `inv`, thanks to the monotonic
   bump, and `GetBaseline` returns the re-approval. Without the bump this is
   the ambiguous case, so it is a regression test for it.
6. **Two identical `RecordAudit` calls in one clock tick** produce two audit
   entries, not one. The nonce test.
7. **Interrupted compaction.** Kill after the checkpoint is written and before
   the deletes: the fold before and after is identical with no duplicates.
   Re-run: identical `<gen>`, no second checkpoint. Then assert the deleter
   refuses inside the grace, refuses when the covering checkpoint is not
   re-observed, refuses on a future `ModTime`, and refuses to delete a key
   that is not a *member* of a checkpoint even when it sorts inside its range.
8. **Two concurrent compactors.** Both write checkpoints from different
   listings; the union of all visible checkpoints plus the loose entries is
   complete and duplicate-free, and no entry is stranded after both delete
   what they covered.
9. **A pruned result inside a checkpoint** stays out of every listing (the
   tombstone), the tombstone survives the grace, and it disappears once the
   next checkpoint absorbs it.
10. **Prune under a stale listing.** With the checkpoint hidden from the
    prune's listing, the tombstone is still written and the result still
    leaves every fold.
11. **Prune does not delete a freshly re-written artifact.** Prune scan-OLD
    while a concurrent scan re-writes the same content-addressed screenshot;
    the artifact survives because of the **take marker** of §7.6 #1 and its
    grace, not because of the artifact's own `ModTime` — a re-capture of
    unchanged bytes writes no artifact object at all, so the object's age
    cannot be the reason. Include the release direction as well: a pin written
    after the take ends the protection.
12. **Prune does not delete a baseline's evidence** under a stale ref listing,
    and **does** delete it after the baseline is revoked. Approve → revoke →
    prune → the approved scan's screenshot **is** collected.
13. **`GetBaseline` after a revoke costs zero GETs**, and a decision object
    that is listed but absent is `ErrCorrupt`, not `ErrNotFound`.
14. **A repair pass over an entry that never appeared.** Write only the
    document; assert the sweep does not collect it (ever, not merely inside
    the grace), then run the rebuild path and assert entry, `byid`, refs and
    target marker all appear and the result reads back byte-identically.
15. **A rebuild marker suppresses all collection**, and its removal restores
    the sweep.
16. **A listed checkpoint whose object is missing** is `ErrIndexIncomplete`,
    not `ErrNotFound`, on every read path.
17. **Request counts per read path**, asserted against the §7.1 table **as
    corrected by §7.6** — eight of its rows are wrong and #7 and #8 are the two
    that bite this test: `RecordAudit` issues a listing before its PUT,
    `Audit(n)` issues a second listing whenever the loose keys do not fill the
    answer, `SetBaseline` pays up to eight attribute reads for the audit-pointer
    heal, and `PreviousResult` folds the whole prefix twice. Assert the
    corrected numbers; do not soften an assertion to whatever the code happens
    to do, which is the one failure mode an AC11 request-count test has. Still
    true and still worth asserting: `LatestResult` reads no checkpoint,
    `ListResults` reads no result document, `HasBaseline` reads no object, and
    `Sweep` issues no GET of a body — though §7.6 #5 makes it one *attribute*
    read per pinned artifact, which is not the zero the table claims.
18. **Backwards clock and two hosts disagreeing.** Both scans present, both
    reachable by ID, `Series` unchanged, deterministic order, a `Warn` naming
    the non-monotonic insert, and prune-by-age using the recorded `StartedAt`.
19. **Key-grammar property and fuzz tests** over `tk`/`mk`/`sk`/`tm`: `"Site"`
    vs `"site"`, a 3,000-byte URL, NFC/NFD pairs, `../`, `%2F`, a NUL byte, a
    backslash, RTL overrides, the empty string, punctuation only. Assert
    injectivity, an output alphabet within `[a-z0-9.-]` plus the `_` marker,
    every path component ≤ 200 bytes, every key ≤ 900 bytes, every key under
    `_wsaw/`, and that a hostile scan ID passed to `GetResult` yields
    `ErrNotFound` without reaching the bucket.
20. **Ordering equivalence across providers.** Build the same 1,000 keys of
    every form, list them through `fileblob` and through `memblob`, assert
    identical order. This is the guard on the `WalkDir` hazard and the test
    that fails if someone nests a listed directory.
21. **Layout version:** a newer `layout/` object refuses `OpenBlob` with the
    same message shape a newer SQL schema gets.
22. **`_wsaw/` is never collected by a sweep**, asserted for *both* store
    kinds against a shared bucket.

---

## 9. Two existing defects this design depends on

**1. The bucket cannot carry index keys.** `validateRef` and `validatePrefix`
accept only `<kind>/<sha256hex>`, which is the right whitelist for artifacts
and the wrong one for `_wsaw/…`. The index gets its own separately-validated
door — `putIndex`, `getIndex`, `statIndex`, `deleteIndex`, `listIndexPage` —
sharing `bucket.do`, the retry policy and the `gcerrors` mapping. **It goes in
a new `internal/store/bucket_index.go`, not into `bucket.go`**, so this work
does not collide with the workflow currently editing that file.

**2. `Sweep` tries to delete the index, and will for ever.** `sweepBucket`
lists prefix `""` — every key, and `validatePrefix("")` returns nil — and
hands everything unreferenced to `removeArtifact`, where `validateRef`
refuses it. So `_wsaw/…` keys survive by accident while being counted as
orphans and producing a logged delete failure on every sweep. This is not
hypothetical: AC16 explicitly permits a bucket that once held a blob index and
is later served by a SQLite deployment. **Fix in both implementations:**
`sweepBucket` skips keys that are not valid artifact references, counts them
separately, and says so. A few lines, and it is the difference between "the
sweep ignores what it does not own" and "the sweep tries to delete the index
every night".

**Recorded while implementing step 4: this defect was already fixed before this
branch began.** `sweepBucket` carries the `isArtifactRef` guard and
`SweepStats.ForeignObjects` as of `eee6414` (Stories 8.5–8.7), so step 4 of §11
had nothing to change in `retention.go`; what it actually contributed is the
extension of `TestASweepLeavesWhatWsawDidNotWrite` with two objects shaped like
a bucket index, which is the case AC16 permits and which the original test did
not cover. Two consequences. A reviewer looking for the fix as a commit on this
branch will not find one, and should not conclude the step was skipped. And the
README line promised above has no premise: no released version of wsaw ever
logged those delete failures, because the sweep and the guard shipped in the same
epic — so **step 14 does not owe that line**, and writing it would tell operators
about a failure mode they have never seen.

---

## 10. README text (AC13, AC16, AC17)

> **The `blob` store: one binary, one bucket, no database.**
>
> wsaw keeps its index as objects in the same bucket as the evidence. There is
> nothing to run, patch or fail over besides the bucket itself. It exists for a
> deployment — typically Kubernetes against object storage — where a database
> is an entire piece of infrastructure kept alive to hold a few thousand small
> records.
>
> **What it costs.**
>
> - **No ad-hoc queries.** It answers exactly the questions wsaw asks — a
>   target's history newest-first, one scan by ID, the scan before a given one,
>   the baseline for a target and mode, the audit log, and retention — because
>   its key layout was built for those. There is no SQL, no reporting tool, and
>   no way to ask a new question without a new key. If you expect to query your
>   scan history yourself, use SQLite or Postgres.
> - **Every read costs requests and latency.** Listing one page of results is
>   one listing request plus one small read per row; opening a result is two
>   reads. A page that renders instantly against SQLite takes a couple of
>   hundred milliseconds here, and every read is a line on your bill. Retention
>   pruning walks each target's history: run it daily, not hourly.
> - **A history gets slower as it grows unless compaction is left on.**
>   Compaction folds old index entries into checkpoint objects and removes what
>   they cover 24 hours later. Turning it off corrupts nothing; it makes long
>   listings and pruning steadily more expensive.
> - **It is still single-node.** Two wsaw instances against one bucket will not
>   corrupt the index — nothing is ever overwritten — but they will duplicate
>   every scheduled scan, and between one approving a baseline and the other
>   seeing it they will disagree about what is expected. This store removes the
>   database, not the constraint in Story 4.7, AC8.
> - **A just-written result may not appear in a listing immediately — not even
>   to the process that wrote it.** Object storage does not promise that, and
>   wsaw does not paper over it: every read is a listing of what the bucket
>   currently shows, with no remembered writes on the side. A result in that
>   window is reported as "not there yet" and never as "deleted", and it is
>   reported that way identically to every process, so two wsaw instances never
>   disagree about a history because one of them wrote part of it. The
>   consequence worth knowing: if a scan is written by `wsaw scan` while the
>   daemon is running, the daemon's next comparison for that target may be
>   against the scan *before* it rather than against it, for as long as the
>   provider takes to converge. wsaw logs when it detects this. It is one more
>   reason Story 4.7's single-node constraint stands.
> - **Two operators approving different baselines at the same moment both
>   succeed.** The later recorded timestamp wins, deterministically, and the
>   other approval stays visible in the audit log rather than vanishing.
> - **The `_wsaw/` prefix is the one thing you still have to back up.** Scan
>   documents can rebuild the result index, but approvals, the audit log and
>   share tokens are decisions, not properties of a scan, and nothing can
>   re-derive them. Turn on bucket versioning, or back that prefix up.
> - **A bucket lifecycle rule will delete evidence behind wsaw's back.** Index
>   objects are small and old; a rule that expires or tiers by age will destroy
>   history. Exclude the `_wsaw/` prefix from any lifecycle policy.
> - **`wsaw store sweep --allow-empty-index` removes the one protection that
>   stands between an orphaned scan document and deletion.** Ordinarily a sweep
>   never collects a result document, even when no index entry points at it: a
>   document decodes to the scan it records, so it is something an index rebuild
>   can recover from rather than garbage. The override says "there is no index
>   to rebuild from", and with it those documents are collected. A listing that
>   is merely lagging looks exactly like an index that is genuinely absent from
>   out here, so use the flag only after a rebuild has been attempted, and never
>   as a way to make a refused sweep run.
> - **A local directory is the wrong home for it at scale.** The local file
>   bucket writes two files per index object. A store with millions of results
>   will exhaust inodes and waste block space long before it fills the disk. On
>   local disk, SQLite is the right answer; the bucket index is for object
>   storage, or for a small deployment that values one binary above all else.
> - **Object storage needs the cloud build.** The default binary links the
>   local file driver only; reaching S3, GCS or Azure needs a build made with
>   `-tags cloudblob`. The release artefacts say which is which.
> - **There is no migration to or from the SQL stores, in either direction.**
>   All four keep documents in the same bucket layout, so what does not port is
>   the index. The supported way across is to point the new store at the same
>   bucket and run `wsaw store rebuild-index`.
>
> **Choose SQLite** if you want one file, the fastest reads, and the ability to
> inspect your history with any SQL tool. **Choose `blob`** if you would rather
> have no local state at all and can pay per-request latency for it.

A matching one-liner goes in the SQLite section pointing the other way, so
AC17's "an operator should be able to choose from that paragraph" works in
both directions.

**Two lines of the text above must not ship ahead of the code they describe**
(step 14 writes the README; steps 12 and 13 are what make these true).

* The compaction bullet — "a history gets slower as it grows unless compaction
  is left on", and the sentence about checkpoints being removed 24 hours later
  — describes step 13. This build reads and unions every checkpoint it can see
  and writes none, so until step 13 lands there is nothing to leave on and
  nothing to turn off. Either step 13 lands first or the bullet says that a
  history simply gets slower as it grows.
* The read-your-writes bullet as **corrected above**, not as originally
  drafted: §7.6 #6 struck the promise that the writing process always sees its
  own write, because §6.6's overlay was not built and that guarantee is not one
  this store makes.

**Configuration.** `store.driver: blob`, with no `path` and no `dsn`; an
artifact location (`store.artifactURL` or `store.artifactDir`) is required —
it is the store. `store.Drivers()` gains `DriverBlob`, which makes
`validateStoreDriver` accept the name for free; the branch after it must
refuse `dsn`, `path` and the pool settings for `blob`.
`config.Store.IsServerStore()` currently means "not sqlite" and drives both
the DSN requirement and `app.StoreOptions`; it is narrowed to postgres/mysql
and `IsBucketStore()` is added beside it.

**Startup (AC14).** `_wsaw/index/layout/<8-digit>.json`, append-only; the
effective layout is the highest present number.

```json
{"layout":1,"createdAt":"2026-09-08T10:45:12Z","writtenBy":"wsaw 0.9.2"}
```

`OpenBlob` probes the bucket exactly as Story 8.6 AC4 does
(`bucket.probeWritable`), then **probes the layout by GET, not by LIST**:
`getIndex` on `00000001.json`, `00000002.json`, … until NotFound. A stale
listing against a store already at layout 2 would return empty, let a v1
binary write `00000001.json` and conclude layout 1 — the refusal AC14 demands,
defeated by the exact consistency property this whole design refuses to trust.
GET is read-after-write consistent on every provider `gocloud.dev/blob`
reaches, so the probe is bounded and never depends on a listing. If the layout
one above the highest known exists, the binary refuses to start, with the same
message shape `TestNewerSchemaIsRefused` asserts, naming the bucket through
`secret.RedactURL`. An empty `layout/` gets `00000001.json` written with
`IfNotExist`; a race is a harmless no-op.

---

## 11. Implementation order

Each step leaves the tree compiling and the suite green.

1. **`refactor(store): extract the shared retry, context and document helpers`**
   — a pure move into `shared.go`.
2. **`refactor(store): name the SQL store what it is`** — `Store` → `SQL`,
   `Open` → `OpenSQL`. Mechanical (`gofmt -r`), no behaviour change.
3. **`refactor(store): make the result store an interface (Tenet 12)`** — add
   `api.go` and the `Open` factory; add `HasBaseline` to `SQL`; retype
   `App.Store`, `maintenance.store`, `httpapi.Deps.Store`,
   `scanner.Deps.Store`; move `migrate` to `OpenSQL`; switch the targets-page
   existence check to `HasBaseline`. **Steps 1–3 are the whole refactor, are
   independently mergeable, and contain no blob code** — which matters given
   that `internal/store`, `internal/config` and `internal/httpapi` are being
   edited concurrently. Land them after that work.
4. **`fix(store): the sweep ignores keys it does not own`** — §9 defect 2,
   with its test, for both kinds. **Already done before this branch**: the
   guard and its counter shipped with Stories 8.5–8.7, so this step is the test
   extension only. See the note in §9.
5. **`fix(store): order the audit log by when it happened`** — SQL's `Audit`
   becomes `order by at desc, id desc`, with a test that a back-dated
   `RecordAudit` lands where its timestamp says.
6. **`feat(store): an index door onto the bucket`** — `bucket_index.go` with
   the tri-state `putIndex`, plus tests.
7. **`feat(store): the bucket index key grammar (Story 8.10)`** —
   `blobkeys.go` with its table and fuzz tests. Pure, fast, no I/O.
8. **`feat(store): open a store whose index is in the bucket`** — `blob.go`,
   the GET-based layout probe, the artifact methods, `Driver()`,
   `ProbeArtifactBucket`; the config and `Drivers()` changes; `Options.Now`
   and `Options.CheckpointGrace`. `WSAW_TEST_STORE_DRIVER=blob` now runs and
   fails loudly on unimplemented methods.
9. **`feat(store): results in the bucket index`** — `blobfold.go`. The shared
   result tests pass under `blob`.
10. **`feat(store): baselines and the audit log in the bucket index`** —
    `blobbaseline.go`. The shared baseline and audit tests pass.
11. **`feat(store): retention against the bucket index`** — `blobprune.go`,
    including the merge-join sweep. All 36 shared tests green under `blob`.
12. **`test(store): a bucket that lags, and what only this store can get
    wrong`** — `lagbucket_test.go` and `blob_test.go` (§8.3), including the
    strict-mode default and the AC11 request-count assertions.
13. **`feat(store): compaction and checkpoints for the bucket index`** —
    `blobcompact.go` plus its interrupted-compaction, concurrent-compactor
    and grace-period tests.
14. **`docs: the bucket-index store, what it is for and what it costs
    (Story 8.10)`** — README and `wsaw.example.yaml`. **The Makefile target and
    the CI step moved up into step 11**, deliberately: `make test-store-blob`
    and the CI step that runs it are what make the 191 shared assertions of
    AC15 a gate rather than something that happens to pass when somebody
    remembers to set `WSAW_TEST_STORE_DRIVER`. Leaving them until last would
    have left every gate in the repository green while the blob store went
    unexercised, which is the one deferred item whose absence creates a false
    green now instead of later. The step needs no container, unlike the
    Postgres and MySQL ones. Step 14 keeps the prose, and §10 says which two of
    its bullets must not ship ahead of steps 12 and 13.

---

## 12. Out of scope, and what Story 8.11 picks up

**Explicitly out of scope for 8.10**, each recorded as a follow-up rather than
left to be discovered:

* **`ctx` on the store interface's read and write methods.** ~50 call sites
  across five packages; its own story, across all four drivers. `Blob` uses
  `opCtx` plus the `blobOpBudget` in the meantime.
* **A `diff.Reason` for "the index could not answer".** `scanner.compare`
  today produces the same first-ever-scan-shaped report for every error but
  `ErrEvidenceGone`. `ErrIndexIncomplete` exists and is logged; making the
  *report* say so is a change in `internal/scanner` and `internal/diff` and
  belongs with them.
* **A head-snapshot object** that would turn `ListResults(100)` into 1 LIST +
  1 GET. It is a fourth object kind with a fold of its own, it is purely
  derived, and it can be added later with **no layout change**: the listing
  stays authoritative for *which* entries exist and the snapshot is only a
  cache of bodies, so a snapshot that lost a concurrent write costs one extra
  GET and never a wrong answer. Ship it when a measured page-load number asks
  for it.
* **Tuning the compaction constants.** 1,000 / 200 / 4 MiB / 20,000 / 24 h are
  reasoned choices, not measurements; Story 8.9's soak run is where a real
  number would come from. They are named constants, not configuration.
* **Multi-node operation.** AC13; Story 4.7 AC8 still holds.
* **Migration between the SQL stores and this one.** AC16 refuses it in both
  directions.

**Story 8.11 picks up:**

* `wsaw store rebuild-index` for all four store kinds, rebuilding entries,
  `byid` objects, ref markers and target markers from the `result/` prefix
  alone. Both the entry and the `byid` body are byte-identical and derived by
  the same `summarize()` the write path uses, so a rebuilt index and a written
  one cannot disagree (8.11 AC2).
* Writing and clearing the `_wsaw/index/v1/rebuild/<id>` marker that §7.4
  already honours (8.11 AC12).
* Re-deriving the entry for a document written before an interrupted
  `PutResult` — the state §6.2 leaves and §7.4 refuses to collect (AC6).
* `--verify`, which is where the two gaps this store cannot close on a read
  path get reported: an audit pointer missing for a decision object, and a
  ref marker missing for an artifact a result names (8.11 AC10).
* Reporting what a rebuild **cannot** restore: baselines, approvals, the audit
  log and share tokens are decisions, not properties of a scan (8.11 AC6).
  In this store those live under `_wsaw/index/v1/baseline/` and
  `_wsaw/index/v1/audit/`, which is why §10's README text tells the operator
  that this one prefix still needs versioning or a backup.
