package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/driver"
	"gocloud.dev/gcerrors"
)

// A bucket that lags, on a schedule (Story 8.10, §8.2).
//
// The store with no database is the one store whose correctness rests on what
// object storage does *not* promise: a key written a moment ago may be missing
// from the next listing, a listing may be served from a stale view, a write may
// land and report failure, a delete may report failure after succeeding. A
// local directory does none of that and neither does a memory bucket, so the
// shared suite cannot reach any of it — which is exactly why AC15 asks for a
// fake that can.
//
// Everything here is driven by an explicit schedule and never by timing: a test
// says which key is hidden and for how many listings, and no test waits for
// anything (AGENTS §5). That is what makes each of them deterministic and safe
// under t.Parallel.
//
// It reaches the store the way a provider does. The fake is a
// gocloud.dev/blob/driver.Bucket registered under the "lag" scheme, opened
// through store.Options.ArtifactDir like any other bucket URL, so every
// injected fault travels through the real blob.Bucket, the real bucket.go and
// the real bucket_index.go — including their retry policy and their gcerrors
// mapping. As with memblob's "mem://", the scheme is registered by a test file
// and by nothing else, so no shipped binary can resolve it.
//
// **It owns its storage rather than wrapping memblob**, which is where this
// deviates from §8.2's sketch. Wrapping cannot express two of the behaviours: a
// listing served from a pinned view has to show keys the storage no longer
// holds, and a hidden key has to be absent from a page without disturbing that
// page's token. Both need the listing built from a view the fake controls, and
// keeping a shadow copy of every object's listing facts beside memblob's own
// would be the same map kept twice.
//
// Two of §8.2's knobs are still not here and one arrived with compaction.
// failPage and corruptBody have no caller: a damaged body needs no injection,
// because writing bad bytes to the object is what the tests that want one
// already do, and a failed page needs a listing longer than one page, which
// nothing in this build constructs. setModTime is the third, futureModTime,
// generalised — it feeds the deletion rule whose only deleter is compaction,
// and compaction landed in step 13.

// lagScheme is what a URL has to say for openBucket to reach the fake. It is
// registered in this file and in no other, and in no non-test file at all.
const lagScheme = "lag"

// lagEveryListing hides a key until the test reveals it again, for the tests
// that care about a visible key set rather than about how many listings it took
// to become visible.
const lagEveryListing = math.MaxInt

// lagBuckets is where a fake waits between the test that builds it and the
// store that opens it, because a URL opener is handed a URL and nothing else.
// The name in the URL is the key, and every fake gets one nobody else has, so
// parallel tests cannot see each other's bucket.
var lagBuckets sync.Map

// lagNames mints those names. A counter rather than the test's name: subtests
// and table cases produce names with characters a URL host cannot carry.
var lagNames atomic.Uint64

// lagOpener resolves "lag://<name>" to the fake registered under that name.
type lagOpener struct{}

// OpenBucketURL hands out a new blob.Bucket over the same driver every time, so
// that two store handles on one URL are two handles on one bucket — which is
// the setup AC7's determinism is stated over.
func (lagOpener) OpenBucketURL(_ context.Context, u *url.URL) (*blob.Bucket, error) {
	found, ok := lagBuckets.Load(u.Host)
	if !ok {
		return nil, fmt.Errorf("no test bucket is registered as %q", u.Host)
	}

	fake, ok := found.(*lagBucket)
	if !ok {
		return nil, fmt.Errorf("what is registered as %q is not a test bucket", u.Host)
	}

	return blob.NewBucket(fake), nil
}

func init() {
	blob.DefaultURLMux().RegisterBucket(lagScheme, lagOpener{})
}

// lagOp names one kind of request, which is both what a fault is scheduled
// against and what AC11's counters count.
type lagOp int

const (
	lagList lagOp = iota
	lagGet
	lagAttributes
	lagPut
	lagDelete
)

func (o lagOp) String() string {
	switch o {
	case lagList:
		return "LIST"
	case lagGet:
		return "GET"
	case lagAttributes:
		return "ATTRIBUTES"
	case lagPut:
		return "PUT"
	case lagDelete:
		return "DELETE"
	default:
		return "unknown"
	}
}

// lagRequests is what one read or write path cost, in requests.
//
// Attributes is counted apart from Get because the two are priced apart and
// because the design's cost table distinguishes them: HasResult is one
// attribute read and no body, and a sweep is attribute reads and no bodies at
// all (§7.6 #5).
type lagRequests struct {
	List       int
	Get        int
	Attributes int
	Put        int
	Delete     int
}

// lagFaultMode says what a scheduled fault does to the operation it is attached
// to. Each of the four is a thing a real object store does and a local
// directory never does.
type lagFaultMode int

const (
	// lagNoFault is the zero value: the operation happens and is reported
	// truthfully.
	lagNoFault lagFaultMode = iota
	// lagFailBefore is the operation not happening, reported as a failure.
	lagFailBefore
	// lagFailAfter is the operation happening and reported as a failure — the
	// ambiguous case a retry has to survive (AC10).
	lagFailAfter
	// lagSkip is the operation not happening, reported as a success.
	lagSkip
)

// lagFault is one scheduled misbehaviour, spent as it is used.
//
// prefix is matched against the key the operation names, or for a listing
// against the prefix it lists, and an exact key is a prefix of itself. Prefixes
// rather than keys because two of the keys under test cannot be known in
// advance: an audit entry's key carries a nonce, and a decision's carries the
// digest of a body the test has not built yet.
type lagFault struct {
	op     lagOp
	prefix string
	mode   lagFaultMode
	code   gcerrors.ErrorCode
	left   int
}

// lagHide keeps one key out of the listings, which is the whole of what
// "eventually consistent" means to a reader of this store.
type lagHide struct {
	prefix string
	left   int
}

// lagObject is one stored object and everything the fake can be asked about it.
type lagObject struct {
	body        []byte
	contentType string
	modTime     time.Time
}

// lagInfo is what a listing reports about an object, kept separately so that a
// pinned view can go on reporting an object the bucket no longer holds.
type lagInfo struct {
	size    int64
	modTime time.Time
}

// lagBucket is the fake, and is a driver rather than a wrapper for the reason
// the file header gives.
type lagBucket struct {
	name string

	mu      sync.Mutex
	objects map[string]*lagObject

	// hides, faults and stale are the schedule. Nothing in here is time-based.
	hides  []lagHide
	faults []lagFault
	stale  int

	// pinned is the view the stale listings are served from, taken when the
	// test pinned it.
	pinned map[string]lagInfo

	// lying makes every conditional create succeed, which is what a provider
	// without a working IfNotExist looks like. It is here to prove that I1
	// comes from the key derivation and not from the condition (§8.2).
	lying bool

	// rewritable turns I1 off. See allowRewrites.
	rewritable bool

	counts lagRequests

	// bodiesRead is every key whose bytes a reader has actually fetched, in
	// order. The counters above say how many requests a path cost; this says
	// which objects it opened, which is the question Story 8.9, AC5 asks —
	// a listing may cost requests, and must never cost a document.
	bodiesRead []string

	// listed is every key a listing has reported, which is what the strict
	// deletion rule is checked against.
	listed map[string]struct{}

	// noted is what the strict rules caught, reported when the test ends.
	noted []string

	// expected suppresses the report at the end of the test, for the one test
	// that is about the strict rules themselves.
	expected bool
}

// newLagBucket builds a fake bucket and registers it for the duration of one
// test.
//
// The strict rules are on, and they are on by default rather than opted into
// because the two invariants they check — no key is ever written twice with
// different bytes (I1), and no index key is deleted that this run has not seen
// in a listing (I3) — are properties of every operation this store performs,
// not of one test. Reviewed once, they would hold in the paths somebody thought
// about; asserted on every lag test, they hold in the paths nobody did.
func newLagBucket(t *testing.T) *lagBucket {
	t.Helper()

	b := &lagBucket{
		name:    fmt.Sprintf("b%d", lagNames.Add(1)),
		objects: map[string]*lagObject{},
		listed:  map[string]struct{}{},
	}

	lagBuckets.Store(b.name, b)

	t.Cleanup(func() {
		lagBuckets.Delete(b.name)
		b.report(t)
	})

	return b
}

// url is what a store's ArtifactDir is set to in order to reach this bucket.
func (b *lagBucket) url() string { return lagScheme + "://" + b.name }

// report fails the test with whatever the strict rules caught.
func (b *lagBucket) report(t *testing.T) {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.expected || len(b.noted) == 0 {
		return
	}

	t.Errorf("the store broke a rule this index depends on:\n  %s", strings.Join(b.noted, "\n  "))
}

// --- the schedule ----------------------------------------------------------

// hideFromListings keeps every key under prefix out of the next listings that
// would otherwise have shown one, which is a write that has landed and is not
// visible yet.
//
// A listing that could not have shown the key spends none of the count, so the
// number a test writes down is the number of listings that are actually lied
// to rather than a count of everything the store happened to list in between.
func (b *lagBucket) hideFromListings(prefix string, listings int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.hides = append(b.hides, lagHide{prefix: prefix, left: listings})
}

// reveal ends the concealment of everything under prefix, so that a test can
// name the moment a key becomes visible instead of counting listings to it.
func (b *lagBucket) reveal(prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	kept := make([]lagHide, 0, len(b.hides))

	for _, hide := range b.hides {
		if !strings.HasPrefix(hide.prefix, prefix) {
			kept = append(kept, hide)
		}
	}

	b.hides = kept
}

// serveStaleListings pins the current key set and answers the next listings
// from it: keys written afterwards are missing and keys deleted afterwards are
// still there, which is a listing served from a replica that has not caught up.
func (b *lagBucket) serveStaleListings(listings int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pinned = make(map[string]lagInfo, len(b.objects))

	for key, object := range b.objects {
		b.pinned[key] = lagInfo{size: int64(len(object.body)), modTime: object.modTime}
	}

	b.stale = listings
}

// catchUpListings ends the pinned view, which is the replica catching up. It is
// a separate call rather than a count of listings because what a test about
// monotonicity wants to name is the moment the view changed, not how many
// requests it took to get there.
func (b *lagBucket) catchUpListings() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.stale = 0
}

// failNext makes the next operations on prefix fail without happening, with the
// code a provider uses for a fault another attempt could fix.
func (b *lagBucket) failNext(op lagOp, prefix string, times int) {
	b.schedule(lagFault{op: op, prefix: prefix, mode: lagFailBefore, code: gcerrors.Internal, left: times})
}

// missNext makes a GET of prefix report the object as absent while it is there,
// which is a provider with no read-after-write on that key.
func (b *lagBucket) missNext(prefix string, times int) {
	b.schedule(lagFault{op: lagGet, prefix: prefix, mode: lagFailBefore, code: gcerrors.NotFound, left: times})
}

// loseResponse lets the operation happen and reports it as failed, which is the
// ambiguous failure every idempotence claim in this design is about (AC10).
func (b *lagBucket) loseResponse(op lagOp, prefix string, times int) {
	b.schedule(lagFault{op: op, prefix: prefix, mode: lagFailAfter, code: gcerrors.Internal, left: times})
}

// ignoreDeletes reports a delete as done without doing it, which is the
// direction retention must survive: a key that comes back.
func (b *lagBucket) ignoreDeletes(prefix string, times int) {
	b.schedule(lagFault{op: lagDelete, prefix: prefix, mode: lagSkip, left: times})
}

// allowRewrites turns off I1, the rule that no key is written twice with
// different bytes.
//
// It has exactly one caller and it is not a lag test: the shared store suite,
// when it is being run against this bucket as its in-memory provider
// (Story 8.9, AC1). Three of those tests plant a tampered, truncated or
// undecodable object at a live content address on purpose — that is corruption
// arriving from outside the store, which is the thing they are about, and a
// fake cannot tell the test's hand from a bad key derivation.
//
// I3 stays on, and deliberately: nothing in the shared suite deletes an index
// key, so "no index key is deleted that this run has not seen in a listing"
// gains a few hundred more paths to hold in rather than losing one.
func (b *lagBucket) allowRewrites() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.rewritable = true
}

// acceptEveryConditionalWrite makes IfNotExist a lie, as it is on a provider
// that does not implement it.
func (b *lagBucket) acceptEveryConditionalWrite() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lying = true
}

// schedule adds one fault to the queue the operations draw from.
func (b *lagBucket) schedule(f lagFault) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.faults = append(b.faults, f)
}

// take spends the fault scheduled for one operation, if there is one.
//
// The caller holds the lock: a fault has to be spent in the same critical
// section as the operation it governs, or two goroutines racing on one key
// could both take the same one.
func (b *lagBucket) take(op lagOp, key string) (lagFaultMode, gcerrors.ErrorCode) {
	for i := range b.faults {
		fault := &b.faults[i]

		if fault.left <= 0 || fault.op != op || !strings.HasPrefix(key, fault.prefix) {
			continue
		}

		fault.left--

		return fault.mode, fault.code
	}

	return lagNoFault, gcerrors.OK
}

// --- what the test asks the fake afterwards --------------------------------

// requests is what has been asked of the bucket since it was built or since the
// counters were last forgotten.
func (b *lagBucket) requests() lagRequests {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.counts
}

// forgetRequests zeroes the counters and the record of what has been read, so
// that one call can be measured without the setup that had to happen first
// (AC11).
func (b *lagBucket) forgetRequests() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts = lagRequests{}
	b.bodiesRead = nil
}

// bodiesReadUnder is how many objects under prefix have had their bytes
// fetched since the counters were last forgotten.
func (b *lagBucket) bodiesReadUnder(prefix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0

	for _, key := range b.bodiesRead {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}

	return n
}

// keysUnder is every key the bucket really holds under prefix, sorted, whatever
// the listings have been told to show.
//
// It is the assertion side of every idempotence test: what a retry produced is
// a question about the bucket, and asking the store would be asking the code
// under test to mark its own work.
func (b *lagBucket) keysUnder(prefix string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var found []string

	for key := range b.objects {
		if strings.HasPrefix(key, prefix) {
			found = append(found, key)
		}
	}

	slices.Sort(found)

	return found
}

// bodyOf is what the bucket holds under one key, for a test that has to read an
// object the store has no method for handing back.
func (b *lagBucket) bodyOf(key string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	object, ok := b.objects[key]
	if !ok {
		return nil, false
	}

	return slices.Clone(object.body), true
}

// forget drops an object without a request having been made, which is a bucket
// that lost bytes: a lifecycle rule that took more than it was meant to, a
// restore that was short, a person with a console. It is the only way to
// produce the state a key that lists and cannot be read stands for, because
// nothing in this store deletes those keys.
func (b *lagBucket) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.objects, key)
}

// setModTime rewrites the bucket's own timestamp for every key under prefix,
// which is the one thing about an object that no request of the store's can
// change and that compaction's every deletion is decided against.
//
// It is one knob where §8.2 named only futureModTime, because a general one has
// both callers: a checkpoint dated far enough in the past is what puts a
// deletion outside the grace period without a test waiting a day for it
// (AGENTS §5), and a checkpoint dated in the future is the provider clock that
// has to disable a deletion rather than enable one. Two names for one
// mechanism would be two things to keep in step.
//
// It does not count as a request, for the same reason forget does not: it is
// the bucket being a bucket rather than the store asking it for anything.
func (b *lagBucket) setModTime(prefix string, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for key, object := range b.objects {
		if strings.HasPrefix(key, prefix) {
			object.modTime = at
		}
	}

	for key, info := range b.pinned {
		if strings.HasPrefix(key, prefix) {
			info.modTime = at
			b.pinned[key] = info
		}
	}
}

// violations is what the strict rules caught, for the test that is about the
// rules themselves.
func (b *lagBucket) violations() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.noted)
}

// expectViolations stops the end-of-test report, and exists for exactly one
// caller: the test that proves the strict rules catch what they claim to and
// asserts them itself. Every other test leaves the report to fail it.
func (b *lagBucket) expectViolations() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.expected = true
}

// noteViolation records a broken invariant. The caller holds the lock.
func (b *lagBucket) noteViolation(format string, args ...any) {
	b.noted = append(b.noted, fmt.Sprintf(format, args...))
}

// --- the driver ------------------------------------------------------------

// lagError is a fault with a provider's error code on it, which is what
// bucket.go classifies a failure by (AC8) and so what an injected one has to
// carry to be classified at all.
type lagError struct {
	code gcerrors.ErrorCode
	msg  string
}

func (e *lagError) Error() string { return e.msg }

// fault is the failure a scheduled misbehaviour reports.
func (b *lagBucket) fault(op lagOp, key string, code gcerrors.ErrorCode) error {
	return &lagError{code: code, msg: fmt.Sprintf("the test bucket failed a %s of %q on purpose", op, key)}
}

// missing is what a key that is not there reports, which every provider owes
// as gcerrors.NotFound.
func (b *lagBucket) missing(key string) error {
	return &lagError{code: gcerrors.NotFound, msg: fmt.Sprintf("no object at %q", key)}
}

// ErrorCode reports what kind of failure this was, which is the one
// classification every gocloud driver owes its caller.
func (b *lagBucket) ErrorCode(err error) gcerrors.ErrorCode {
	var coded *lagError

	if errors.As(err, &coded) {
		return coded.code
	}

	return gcerrors.Unknown
}

// As exposes no driver-specific type. There is nothing underneath this one.
func (b *lagBucket) As(any) bool { return false }

// ErrorAs exposes no driver-specific error type, for the same reason.
func (b *lagBucket) ErrorAs(error, any) bool { return false }

// Close releases nothing, deliberately: two store handles on one URL share this
// driver, and one of them closing must not empty the bucket the other is
// reading (§7.6 #6 states AC7 over exactly that pair).
func (b *lagBucket) Close() error { return nil }

// Copy is not implemented because nothing in this store copies an object.
func (b *lagBucket) Copy(context.Context, string, string, *driver.CopyOptions) error {
	return &lagError{code: gcerrors.Unimplemented, msg: "the test bucket does not copy"}
}

// SignedURL is not implemented, which is what a provider with no signing
// reports and what ErrSigningUnsupported is derived from.
func (b *lagBucket) SignedURL(context.Context, string, *driver.SignedURLOptions) (string, error) {
	return "", &lagError{code: gcerrors.Unimplemented, msg: "the test bucket does not sign URLs"}
}

// Attributes reports what the bucket knows about one object without reading it.
func (b *lagBucket) Attributes(ctx context.Context, key string) (*driver.Attributes, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading the attributes of %s in the test bucket: %w", key, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts.Attributes++

	if mode, code := b.take(lagAttributes, key); mode == lagFailBefore {
		return nil, b.fault(lagAttributes, key, code)
	}

	object, ok := b.objects[key]
	if !ok {
		return nil, b.missing(key)
	}

	return &driver.Attributes{
		ContentType: object.contentType,
		ModTime:     object.modTime,
		Size:        int64(len(object.body)),
	}, nil
}

// NewRangeReader reads part of an object.
func (b *lagBucket) NewRangeReader(
	ctx context.Context, key string, offset, length int64, opts *driver.ReaderOptions,
) (driver.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading %s from the test bucket: %w", key, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts.Get++

	if mode, code := b.take(lagGet, key); mode == lagFailBefore {
		return nil, b.fault(lagGet, key, code)
	}

	object, ok := b.objects[key]
	if !ok {
		return nil, b.missing(key)
	}

	b.bodiesRead = append(b.bodiesRead, key)

	if opts.BeforeRead != nil {
		if err := opts.BeforeRead(func(any) bool { return false }); err != nil {
			return nil, err
		}
	}

	body := object.body[min(offset, int64(len(object.body))):]
	if length >= 0 && length < int64(len(body)) {
		body = body[:length]
	}

	return &lagReader{
		r: bytes.NewReader(body),
		attrs: driver.ReaderAttributes{
			ContentType: object.contentType,
			ModTime:     object.modTime,
			Size:        int64(len(object.body)),
		},
	}, nil
}

// lagReader hands back the bytes of one range.
type lagReader struct {
	r     *bytes.Reader
	attrs driver.ReaderAttributes
}

// Read hands back the range's bytes. The error is returned unwrapped because
// io.EOF is the loop condition every reader is written against, and a wrapped
// one would not be recognised by the code doing the reading.
func (r *lagReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func (r *lagReader) Close() error { return nil }

func (r *lagReader) Attributes() *driver.ReaderAttributes { return &r.attrs }

func (r *lagReader) As(any) bool { return false }

// NewTypedWriter buffers a write; everything that can go wrong with it happens
// at Close, which is where the request is.
func (b *lagBucket) NewTypedWriter(
	ctx context.Context, key, contentType string, opts *driver.WriterOptions,
) (driver.Writer, error) {
	if key == "" {
		return nil, &lagError{code: gcerrors.InvalidArgument, msg: "the test bucket was given an empty key"}
	}

	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(func(any) bool { return false }); err != nil {
			return nil, err
		}
	}

	return &lagWriter{
		b: b,
		// The driver interface puts no context on Write or Close, so a driver
		// has to carry the one it was opened with in order to honour a
		// cancelled write at all; memblob does the same. It is the one place
		// this file keeps a context in a struct.
		ctx:         ctx,
		key:         key,
		contentType: contentType,
		ifNotExist:  opts.IfNotExist,
	}, nil
}

// lagWriter is one buffered write.
type lagWriter struct {
	b           *lagBucket
	ctx         context.Context
	key         string
	contentType string
	ifNotExist  bool
	body        bytes.Buffer
}

func (w *lagWriter) Write(p []byte) (int, error) {
	n, err := w.body.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffering a write to the test bucket: %w", err)
	}

	return n, nil
}

// Close is the request: it counts, it obeys the schedule, it checks the rewrite
// rule, and only then does it store anything.
func (w *lagWriter) Close() error {
	if err := w.ctx.Err(); err != nil {
		return fmt.Errorf("writing %s to the test bucket: %w", w.key, err)
	}

	b := w.b

	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts.Put++

	mode, code := b.take(lagPut, w.key)

	switch mode {
	case lagFailBefore:
		return b.fault(lagPut, w.key, code)

	case lagSkip:
		// Reported as done and not done: the write-side twin of the delete
		// that reports success without deleting.
		return nil

	case lagNoFault, lagFailAfter:
	}

	body := w.body.Bytes()

	b.checkRewrite(w.key, body)

	if _, present := b.objects[w.key]; present && w.ifNotExist && !b.lying {
		return &lagError{
			code: gcerrors.FailedPrecondition,
			msg:  fmt.Sprintf("an object already exists at %q", w.key),
		}
	}

	b.objects[w.key] = &lagObject{
		body:        slices.Clone(body),
		contentType: w.contentType,
		// The bucket's own clock, which is what compaction and retention
		// compare against rather than the host's.
		modTime: time.Now(),
	}

	if mode == lagFailAfter {
		return b.fault(lagPut, w.key, code)
	}

	return nil
}

// checkRewrite is invariant I1, checked on the attempt rather than on the
// outcome. The caller holds the lock.
//
// A conditional create refuses the second write, so a key derived from the
// wrong thing would be caught by nothing: the provider would quietly keep the
// first bytes and the store would carry on believing it had recorded the
// second fact. What has to hold is that no two distinct facts ever derive one
// key, and that is a property of the derivation, which is what an attempt
// shows.
func (b *lagBucket) checkRewrite(key string, body []byte) {
	if b.rewritable {
		return
	}

	existing, present := b.objects[key]
	if !present || bytes.Equal(existing.body, body) {
		return
	}

	b.noteViolation("%q was written twice with different bytes (%d then %d)",
		key, len(existing.body), len(body))
}

// Delete removes one object.
func (b *lagBucket) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("deleting %s from the test bucket: %w", key, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts.Delete++

	mode, code := b.take(lagDelete, key)

	switch mode {
	case lagFailBefore:
		return b.fault(lagDelete, key, code)

	case lagSkip:
		// Reported as done and not done, which is the delete a retention pass
		// has to be able to run again.
		return nil

	case lagNoFault, lagFailAfter:
	}

	if _, present := b.objects[key]; !present {
		return b.missing(key)
	}

	b.checkDeletion(key)
	delete(b.objects, key)

	if mode == lagFailAfter {
		return b.fault(lagDelete, key, code)
	}

	return nil
}

// checkDeletion is invariant I3's precondition: nothing in the index is deleted
// that this run has not seen in a listing. The caller holds the lock.
//
// Deletion here always follows observation — compaction deletes what a
// checkpoint it has just listed covers, and prune deletes the pins and entries
// its own listing produced — so a delete of a key nobody listed is a deletion
// decided from something other than what the bucket currently shows, which is
// the one way this index can lose an object it still needs.
//
// It is scoped to the index root because the rule is about index objects. The
// startup write probe (Story 8.6, AC4) deliberately deletes an artifact key it
// never listed, and that is a probe cleaning up after itself rather than
// retention making a decision.
//
// The rebuild marker is exempt for the same reason (Story 8.11, AC12). It is a
// lease rather than a record: the process that wrote it deletes exactly the key
// it wrote, by name, and nothing folds it or reads it as history. Requiring a
// listing first would be worse than useless — a listing that has not caught up
// would leave the marker behind for ever, and a marker left behind stops every
// later sweep from collecting anything at all.
func (b *lagBucket) checkDeletion(key string) {
	if !strings.HasPrefix(key, "_wsaw/") || strings.HasPrefix(key, "_wsaw/index/v1/rebuild/") {
		return
	}

	if _, seen := b.listed[key]; !seen {
		b.noteViolation("%q was deleted without ever having been listed", key)
	}
}

// ListPaged is where the lag lives.
func (b *lagBucket) ListPaged(ctx context.Context, opts *driver.ListOptions) (*driver.ListPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("listing the test bucket: %w", err)
	}

	if opts.BeforeList != nil {
		if err := opts.BeforeList(func(any) bool { return false }); err != nil {
			return nil, err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts.List++

	if mode, code := b.take(lagList, opts.Prefix); mode == lagFailBefore {
		return nil, b.fault(lagList, opts.Prefix, code)
	}

	return b.page(opts, b.collapse(opts, b.visible(opts.Prefix))), nil
}

// visible is the key set this listing is answered from: the pinned view if one
// is being served, otherwise what the bucket holds, minus whatever is hidden.
// The caller holds the lock.
func (b *lagBucket) visible(prefix string) []string {
	view := make(map[string]struct{}, len(b.objects))

	if b.stale > 0 {
		b.stale--

		for key := range b.pinned {
			view[key] = struct{}{}
		}
	} else {
		for key := range b.objects {
			view[key] = struct{}{}
		}
	}

	keys := make([]string, 0, len(view))

	for key := range view {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	slices.Sort(keys)

	return b.applyHides(keys)
}

// applyHides drops the hidden keys and spends one turn of each rule that hid
// something. The caller holds the lock.
func (b *lagBucket) applyHides(keys []string) []string {
	for i := range b.hides {
		hide := &b.hides[i]

		if hide.left <= 0 {
			continue
		}

		kept := make([]string, 0, len(keys))
		hidden := false

		for _, key := range keys {
			if strings.HasPrefix(key, hide.prefix) {
				hidden = true

				continue
			}

			kept = append(kept, key)
		}

		if hidden {
			hide.left--
			keys = kept
		}
	}

	return keys
}

// collapse turns the visible keys into list objects, folding a "directory" into
// one entry when the caller asked for a delimiter — which the retention sweep
// does, to discover which kinds a bucket holds without listing them.
//
// It is memblob's algorithm rather than a second reading of the same rule,
// because a listing that grouped differently from every real provider would
// make this fake the thing under test. The caller holds the lock.
func (b *lagBucket) collapse(opts *driver.ListOptions, keys []string) []*driver.ListObject {
	objects := make([]*driver.ListObject, 0, len(keys))
	lastDir := ""

	for _, key := range keys {
		if opts.Delimiter != "" {
			rest := strings.TrimPrefix(key, opts.Prefix)

			if i := strings.Index(rest, opts.Delimiter); i != -1 {
				dir := opts.Prefix + rest[:i+len(opts.Delimiter)]
				if dir == lastDir {
					continue
				}

				lastDir = dir

				objects = append(objects, &driver.ListObject{Key: dir, IsDir: true})

				continue
			}
		}

		b.listed[key] = struct{}{}

		info := b.info(key)
		objects = append(objects, &driver.ListObject{Key: key, Size: info.size, ModTime: info.modTime})
	}

	return objects
}

// info is what a listing reports about one key, taken from the pinned view when
// the object itself has gone. The caller holds the lock.
func (b *lagBucket) info(key string) lagInfo {
	if object, ok := b.objects[key]; ok {
		return lagInfo{size: int64(len(object.body)), modTime: object.modTime}
	}

	return b.pinned[key]
}

// page cuts one page out of the listing and says where the next one starts.
//
// The filtering happens before the paging and not after, which matters: a page
// short of its size because keys were hidden from it would make blob.ListPage
// ask again to fill it, and one logical listing would spend two turns of the
// schedule. The caller holds the lock.
func (b *lagBucket) page(opts *driver.ListOptions, objects []*driver.ListObject) *driver.ListPage {
	if token := string(opts.PageToken); token != "" {
		objects = slices.DeleteFunc(objects, func(o *driver.ListObject) bool { return o.Key <= token })
	}

	size := opts.PageSize
	if size <= 0 || size > len(objects) {
		size = len(objects)
	}

	page := &driver.ListPage{Objects: objects[:size]}

	// A token is handed back only when there is genuinely another page: an
	// empty page with a token is an infinite loop in every paging caller.
	if size < len(objects) {
		page.NextPageToken = []byte(objects[size-1].Key)
	}

	return page
}

// --- the fake's own tests --------------------------------------------------

// lagHandle opens a plain bucket handle on a fake, for the tests that are about
// the fake rather than about the store.
func lagHandle(t *testing.T, fake *lagBucket) *blob.Bucket {
	t.Helper()

	handle, err := blob.OpenBucket(t.Context(), fake.url())
	if err != nil {
		t.Fatalf("opening the test bucket: %v", err)
	}

	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Errorf("closing the test bucket: %v", err)
		}
	})

	return handle
}

// lagWrite puts one object through a bucket handle.
func lagWrite(t *testing.T, handle *blob.Bucket, key, body string) {
	t.Helper()

	if err := handle.WriteAll(t.Context(), key, []byte(body), &blob.WriterOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		t.Fatalf("writing %s: %v", key, err)
	}
}

// lagList lists a prefix through a bucket handle, in one page.
func lagListing(t *testing.T, handle *blob.Bucket, prefix string) []string {
	t.Helper()

	objects, _, err := handle.ListPage(t.Context(), blob.FirstPageToken, 100, &blob.ListOptions{Prefix: prefix})
	if err != nil {
		t.Fatalf("listing %s: %v", prefix, err)
	}

	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
	}

	return keys
}

// TestTheTestBucketHidesAKeyForExactlyTheListingsItWasGiven is the fake's own
// specification for the behaviour every eventual-consistency test below rests
// on. A schedule that were off by one listing would make each of those tests
// assert something other than what it says it asserts.
func TestTheTestBucketHidesAKeyForExactlyTheListingsItWasGiven(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	handle := lagHandle(t, fake)

	lagWrite(t, handle, "a", "first")
	lagWrite(t, handle, "b", "second")

	fake.hideFromListings("b", 2)

	for i := range 3 {
		want := []string{"a"}
		if i == 2 {
			want = []string{"a", "b"}
		}

		if got := lagListing(t, handle, ""); !slices.Equal(got, want) {
			t.Errorf("listing %d showed %v, want %v", i+1, got, want)
		}
	}

	// Hidden from the listings and never from a read, which is the whole shape
	// of the failure: the object is there and only the listing is behind.
	if body, err := handle.ReadAll(t.Context(), "b"); err != nil || string(body) != "second" {
		t.Errorf("reading a hidden key gave %q, %v", body, err)
	}
}

// TestTheTestBucketServesAPinnedViewToAStaleListing pins the other half of what
// a lagging provider does: not a key that has not arrived, but a whole listing
// answered from a replica that is behind.
//
// Both directions matter and both are asserted, because a stale view is not
// simply a shorter one: it is missing what has been written since, and it still
// shows what has been deleted since.
func TestTheTestBucketServesAPinnedViewToAStaleListing(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	handle := lagHandle(t, fake)

	lagWrite(t, handle, "a", "first")

	fake.serveStaleListings(2)

	lagWrite(t, handle, "b", "second")

	if err := handle.Delete(t.Context(), "a"); err != nil {
		t.Fatalf("deleting a: %v", err)
	}

	for i := range 2 {
		if got := lagListing(t, handle, ""); !slices.Equal(got, []string{"a"}) {
			t.Errorf("stale listing %d showed %v, want the pinned view [a]", i+1, got)
		}
	}

	if got := lagListing(t, handle, ""); !slices.Equal(got, []string{"b"}) {
		t.Errorf("the listing after the pinned view showed %v, want [b]", got)
	}
}

// TestTheTestBucketFailsAnOperationTheWayAProviderDoes pins the two failure
// shapes a retry has to tell apart, and pins that both carry a code
// bucket.go's classifier reads.
func TestTheTestBucketFailsAnOperationTheWayAProviderDoes(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	handle := lagHandle(t, fake)

	fake.failNext(lagPut, "a", 2)

	for i := range 2 {
		err := handle.WriteAll(t.Context(), "a", []byte("first"), nil)
		if err == nil {
			t.Fatalf("write %d succeeded, want the scheduled failure", i+1)
		}

		if code := gcerrors.Code(err); code != gcerrors.Internal {
			t.Errorf("the failure came back as %v, want a code worth retrying", code)
		}
	}

	if _, present := fake.bodyOf("a"); present {
		t.Error("a write that failed before it happened left an object behind")
	}

	lagWrite(t, handle, "a", "first")

	// The other shape: it happened and the caller was told it did not.
	fake.loseResponse(lagDelete, "a", 1)

	if err := handle.Delete(t.Context(), "a"); err == nil {
		t.Fatal("the delete reported success, want the lost response")
	}

	if _, present := fake.bodyOf("a"); present {
		t.Error("the delete reported failure and did not happen; it must happen")
	}
}

// TestTheTestBucketCountsWhatWasAskedOfIt is the floor under every AC11
// assertion: a counter that missed a request would turn a read path that got
// more expensive into a test that still passed.
func TestTheTestBucketCountsWhatWasAskedOfIt(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	handle := lagHandle(t, fake)

	lagWrite(t, handle, "a", "first")
	fake.forgetRequests()

	if _, err := handle.ReadAll(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}

	if _, err := handle.Attributes(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}

	lagListing(t, handle, "")
	lagWrite(t, handle, "b", "second")

	if err := handle.Delete(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}

	want := lagRequests{List: 1, Get: 1, Attributes: 1, Put: 1, Delete: 1}
	if got := fake.requests(); got != want {
		t.Errorf("the bucket counted %+v, want %+v", got, want)
	}
}

// TestTheTestBucketRefusesToLetARewriteGoUnnoticed is the strict default (§8.2).
//
// The two rules it enforces are the two this index cannot survive being wrong
// about, and neither of them shows up as a failure at the time: a key written
// twice with different bytes is refused by the conditional create and the
// second fact is silently lost, and a key deleted without having been listed is
// a deletion decided from something other than what the bucket shows. Both are
// on by default on every lag test, which is the point — a rule that had to be
// switched on would hold only where somebody thought to switch it on.
func TestTheTestBucketRefusesToLetARewriteGoUnnoticed(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	fake.expectViolations()

	handle := lagHandle(t, fake)

	lagWrite(t, handle, "_wsaw/index/v1/a", "first")

	// The same bytes again is the retry every write here is designed to be, and
	// is not a violation.
	lagWrite(t, handle, "_wsaw/index/v1/a", "first")

	if got := fake.violations(); len(got) != 0 {
		t.Errorf("rewriting a key with its own bytes was reported as %v", got)
	}

	lagWrite(t, handle, "_wsaw/index/v1/a", "second")

	if err := handle.Delete(t.Context(), "_wsaw/index/v1/a"); err != nil {
		t.Fatal(err)
	}

	got := fake.violations()
	if len(got) != 2 {
		t.Fatalf("the strict rules reported %v, want one rewrite and one unlisted deletion", got)
	}

	if !strings.Contains(got[0], "different bytes") {
		t.Errorf("the first violation is %q, want the rewrite", got[0])
	}

	if !strings.Contains(got[1], "listed") {
		t.Errorf("the second violation is %q, want the unlisted deletion", got[1])
	}
}

// TestTheTestBucketAllowsADeleteOfAKeyItListed is the other half of the
// deletion rule, and the reason the rule is worth having: the deletes this
// store actually makes are all of keys a listing produced, so a rule that
// caught them too would have to be switched off and would then catch nothing.
func TestTheTestBucketAllowsADeleteOfAKeyItListed(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	handle := lagHandle(t, fake)

	lagWrite(t, handle, "_wsaw/index/v1/a", "first")
	lagListing(t, handle, "_wsaw/")

	if err := handle.Delete(t.Context(), "_wsaw/index/v1/a"); err != nil {
		t.Fatal(err)
	}

	if got := fake.violations(); len(got) != 0 {
		t.Errorf("deleting a listed key was reported as %v", got)
	}
}
