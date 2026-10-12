package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// This file holds the defaults an Options gets when a field is left unset, the
// retry policy, the deadline one operation runs under, and the envelope a
// stored result document is written and read back through.

// Defaults for an Options with the field left unset. They live together, and
// apart from the fields they default, so that "what does an unset MaxIdleConns
// mean" has one place to be answered.

func (o *Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}

	return slog.Default()
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

// retrier runs one store operation, trying again while the failure is
// transient. It exists because the thing a store talks to is reached over a
// network: a restart, a failover, a deadlock or a 503 from object storage is an
// ordinary event, and failing a scan's result on the first dropped packet would
// lose an observation for no good reason.
//
// A permanent failure — a constraint violation, a malformed statement, a
// missing table, a refused credential — is returned on the first attempt.
// Retrying one only makes the failure slower and hides its cause. Which
// failures those are is the one part that differs per store, so it arrives as
// a predicate rather than being decided here.
//
// The operations retried through it are idempotent by construction: the writes
// are upserts keyed by identity, or writes to a key derived from what is being
// written, and the deletes are by identity. The exception is an audit entry,
// which is an append: if a connection drops after the server committed but
// before wsaw heard so, a retry can write it twice. A duplicated audit line is
// visible and harmless; a lost approval record is neither, so this is the right
// way round.
//
// It is a value rather than an interface: there is one policy, configured three
// ways, and a second implementation of "try again" would be a second answer to
// a question already settled.
type retrier struct {
	// attempts is the total number of tries, not the number of retries. One
	// means the policy is off.
	attempts int
	// backoff is the delay before the second attempt; it doubles thereafter.
	backoff time.Duration
	// onRetry is called before each retry. A retry nobody can see is a
	// flapping dependency that looks healthy (Tenet 8). Optional.
	onRetry func(op string, attempt int, err error)
	// transient reports whether this failure is worth another attempt. Nil
	// means nothing is: every failure is returned as it arrives.
	transient func(err error) bool
}

// run performs one operation under the policy.
func (r retrier) run(ctx context.Context, op string, fn func(context.Context) error) error {
	var lastErr error

	for attempt := 1; attempt <= r.attempts; attempt++ {
		if attempt > 1 {
			if err := r.wait(ctx, attempt); err != nil {
				return err
			}
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}

		// A cancelled caller is not a broken dependency, and retrying its work
		// would only delay the shutdown it asked for.
		if ctx.Err() != nil {
			return err
		}

		if r.transient == nil || !r.transient(err) {
			return err
		}

		lastErr = err

		// Reported only when another attempt actually follows: a "retrying"
		// line after the last attempt would overstate what happened.
		if r.onRetry != nil && attempt < r.attempts {
			r.onRetry(op, attempt, err)
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", op, r.attempts, lastErr)
}

// wait sleeps before an attempt, with exponential backoff, and gives up as
// soon as the context does.
func (r retrier) wait(ctx context.Context, attempt int) error {
	delay := r.backoff * (1 << (attempt - 2))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// opTimeout bounds one store operation, database or bucket. Every call has a
// deadline, for the same reason every browser interaction does: nothing waits
// for ever.
const opTimeout = 30 * time.Second

// opCtx bounds a single store operation started from outside any caller's
// context — which is every exported method that reads or writes a result, none
// of which takes one.
func opCtx() (context.Context, context.CancelFunc) {
	return opCtxFrom(context.Background())
}

// opCtxFrom bounds one operation inside a longer-running one. The document
// migration is the only such caller: it may run for minutes in total, and each
// statement and bucket call within it still gets the same deadline every other
// store operation has.
func opCtxFrom(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, opTimeout)
}

// resultRef is what a result row carries in place of the document: where the
// bytes are, how many of them there should be, and what they must hash to.
//
// It is the whole of what the store has to record about a document: a row that
// recorded less could not tell truncated evidence from intact evidence, and a
// row that recorded more would be describing the document twice.
type resultRef struct {
	ref    string
	size   int64
	digest string
}

// documentDigest is what an index records for its document, and what a
// document read back out of the bucket is checked against.
func documentDigest(document []byte) string {
	sum := sha256.Sum256(document)

	return hex.EncodeToString(sum[:])
}

// decodeDocument turns the bytes of a stored document back into the result
// that was written, having first checked them against what the index recorded.
//
// Checked before decoding, and never after: JSON that parses is not evidence
// that the bytes are the ones written, and a result rebuilt from altered or
// truncated bytes would be reported as a scan rather than as damage (Tenet 5).
func decodeDocument(ref resultRef, document []byte) (*model.Result, error) {
	if err := verifyDocument(ref, document); err != nil {
		return nil, err
	}

	var res model.Result

	if err := json.Unmarshal(document, &res); err != nil {
		// The bytes are the ones that were written — the digest says so — and
		// they still do not decode, which leaves the stored evidence unusable
		// rather than absent.
		return nil, fmt.Errorf("decoding artifact %s: %w: %w", ref.ref, ErrCorrupt, err)
	}

	return &res, nil
}

// verifyDocument compares an artifact against the size and digest the index
// recorded when it was written. Size first, because it is free and catches the
// truncation that is the likeliest of the two.
func verifyDocument(ref resultRef, document []byte) error {
	if int64(len(document)) != ref.size {
		return fmt.Errorf("artifact %s holds %d bytes where the store recorded %d: %w",
			ref.ref, len(document), ref.size, ErrCorrupt)
	}

	if digest := documentDigest(document); digest != ref.digest {
		return fmt.Errorf("artifact %s hashes to %s where the store recorded %s: %w",
			ref.ref, digest, ref.digest, ErrCorrupt)
	}

	return nil
}
