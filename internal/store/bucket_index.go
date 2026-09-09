package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// This file is the index's door onto the same bucket the evidence lives in
// (Story 8.10). It is a second door and not a widening of the first: bucket.go
// admits exactly "<kind>/<sha256hex>" and nothing else, and the value of that
// whitelist is that it has to be right about one shape. Relaxing it so index
// keys could pass would trade a check that is provably right for a check that
// is approximately right, on the one code path a crafted artifact reference
// reaches (Tenet 9).
//
// What it deliberately does not duplicate is everything underneath the
// validation. The retry policy, the classification of which provider errors
// are worth another attempt, the per-request deadline and the translation of
// gcerrors into this package's sentinels all belong to bucket.do and are
// borrowed here, because a second copy of them would be a second answer to
// "is this failure transient" that drifts from the first (AC8).
//
// The other difference from the artifact door is the write: an artifact key
// that already exists holds those exact bytes, so bucket.write treats losing
// the conditional-create race as the no-op it is. Index keys are not
// content-addressed in general — a byid key is named after a scan, not after
// its body — so swallowing that here would hide a scan ID reused for different
// content. putIndex reports which of the two happened and lets its caller
// decide.

const (
	// indexListPageSize bounds one listing round trip of the index. A thousand
	// is the maximum every provider gocloud reaches will return in one
	// response, so it is the fewest requests a listing can cost.
	//
	// It is separate from artifactListPageSize, which stays at 256: an
	// artifact listing is a retention sweep holding objects in memory one page
	// at a time, while an index listing is a read on the critical path of a
	// page render, where the round trip dominates and a larger page is
	// strictly cheaper.
	indexListPageSize = 1000

	// indexContentType is what every index object is written as, including the
	// zero-byte markers.
	//
	// One type for the whole index, and the same one artifacts get, because no
	// index object is ever served to a browser or signed into a URL — the HTTP
	// layer has no route that reaches one — so the only thing a content type
	// could do here is describe a tombstone as a document it is not.
	indexContentType = artifactContentType
)

// putOutcome says which of the two ways a conditional create succeeded.
//
// It is a returned value rather than a swallowed distinction because two
// callers have to tell the cases apart: PutResult, so that a scan ID reused
// for different content is refused rather than quietly shadowed, and
// compaction, so that re-running an interrupted pass is reported as the no-op
// it is instead of as fresh work (AC10).
type putOutcome int

const (
	// putNothing is the zero value and means no write was attempted or none
	// completed. It exists so that a caller reading the outcome without
	// reading the error learns nothing rather than something false.
	putNothing putOutcome = iota
	// putCreated means this call wrote the key.
	putCreated
	// putExisted means the key was already there. The bytes are not compared:
	// finding out would cost a GET on every write, and invariant I1 — that two
	// distinct facts derive two distinct keys — is what makes the comparison
	// unnecessary.
	putExisted
)

// indexObject is one key a listing of the index reported, with what the bucket
// said about it.
//
// It carries the same three facts artifactObject does and is a separate type
// on purpose: the two key spaces are disjoint by construction (see indexRoot),
// and a distinct type is what stops an index key from reaching a function that
// expects an artifact reference, or the reverse, without the compiler saying
// so.
//
// modTime is not decoration. It is the bucket's own clock, and compaction's
// rule for when a covered entry may be deleted is expressed against it rather
// than against the host's, so that there is one clock in the decision and a
// timestamp in the future disables a deletion instead of enabling it.
type indexObject struct {
	key     string
	size    int64
	modTime time.Time
}

// indexCursor says where a listing resumes.
//
// The zero value starts one, and a cursor a completed listing handed back is
// spent: passing it to listIndexPage again is refused rather than served. The
// two states are separate fields on purpose. gocloud's own page token spells
// "start here" and "nothing left" as two different values — FirstPageToken and
// the empty slice — precisely because a single value for both makes a
// straightforward `for` loop restart the listing forever, and a wrapper that
// mapped an empty token back onto "start" would hand that bug straight back.
// Keeping it a struct also keeps the blob package out of every caller.
type indexCursor struct {
	token []byte
	spent bool
}

// more reports whether the listing this cursor came from has pages left, and
// is the condition a paging loop is written against.
func (c indexCursor) more() bool { return !c.spent }

// indexPage is one page of a listing: what it found, and how to continue.
//
// A page at a time and not a callback, unlike bucket.list, because the index's
// reads are bounded by what they want rather than by what exists: a fold that
// needs the newest few entries reads one page and stops, and that is the
// difference between a page render costing one request and costing a listing
// of the whole history (AC11).
type indexPage struct {
	objects []indexObject

	// next resumes the listing, and reports itself spent when nothing remains.
	next indexCursor
}

// putIndex creates one index key and never rewrites one.
//
// Every index write in this store goes through it, which is where invariant I1
// — no index object is ever overwritten, appended to, or deleted as part of an
// ordinary write (AC3) — is enforced for the providers that honour the
// condition. For a provider that does not, I1 still holds, because it is a
// property of the key derivation and not of the condition: two distinct facts
// derive two distinct keys, and two writers of the same fact write the same
// bytes to the same key.
func (b *bucket) putIndex(ctx context.Context, key string, body []byte) (putOutcome, error) {
	if err := validateIndexKey(key); err != nil {
		return putNothing, err
	}

	outcome := putNothing

	if err := b.do(ctx, "writing an index object", func(ctx context.Context) error {
		// Reset per attempt, so a retry cannot inherit the verdict of the
		// attempt before it.
		outcome = putNothing

		written, err := b.writeIndex(ctx, key, body)
		if err != nil {
			return err
		}

		outcome = written

		return nil
	}); err != nil {
		return putNothing, indexError("writing", key, err)
	}

	return outcome, nil
}

// writeIndex performs one attempt at creating an index key.
//
// Atomicity comes from the driver, exactly as it does for an artifact:
// fileblob renames a temporary file into place on Close and the cloud drivers
// publish an object only when the upload completes, so an interrupted write
// leaves no key rather than a half-written one. A reader of this index
// therefore never sees a torn object, which is what lets every read be a fold
// over whatever is currently visible.
func (b *bucket) writeIndex(ctx context.Context, key string, body []byte) (putOutcome, error) {
	w, err := b.b.NewWriter(ctx, key, &blob.WriterOptions{
		ContentType: indexContentType,
		IfNotExist:  true,
		BeforeWrite: restrictLocalFilePermissions,
	})
	if err != nil {
		return putNothing, err
	}

	if _, err := w.Write(body); err != nil {
		// Close abandons the partial write. Its error is dropped because the
		// one worth reporting is the one from Write, which is the failure that
		// actually happened.
		_ = w.Close()

		return putNothing, err
	}

	if err := w.Close(); err != nil {
		if gcerrors.Code(err) == gcerrors.FailedPrecondition {
			// Somebody wrote this key first. That is a fact about the bucket
			// and not a failure, and saying which of the two happened is the
			// whole reason this function returns an outcome.
			return putExisted, nil
		}

		return putNothing, err
	}

	return putCreated, nil
}

// getIndex reads one index object whole.
//
// Whole rather than streamed, because index objects are small by design — an
// entry is about 450 bytes and a checkpoint is capped — and because every
// caller decodes what it reads rather than forwarding it. The artifacts, which
// are the large objects, stream through bucket.newReader instead.
func (b *bucket) getIndex(ctx context.Context, key string) ([]byte, error) {
	if err := validateIndexKey(key); err != nil {
		return nil, err
	}

	var body []byte

	if err := b.do(ctx, "reading an index object", func(ctx context.Context) error {
		read, err := b.b.ReadAll(ctx, key)
		if err != nil {
			return err
		}

		body = read

		return nil
	}); err != nil {
		return nil, indexError("reading", key, err)
	}

	return body, nil
}

// statIndex reports what the bucket knows about one index object without
// reading it.
//
// It exists for the two questions that need an object's age rather than its
// contents: whether a checkpoint has been visible long enough for what it
// covers to be deleted, and whether a tombstone is old enough to collect.
// Both are answered from the bucket's modification time, and paying a GET for
// a timestamp the bucket will report in a HEAD would be a request per object
// on the one path that walks many of them.
func (b *bucket) statIndex(ctx context.Context, key string) (indexObject, error) {
	if err := validateIndexKey(key); err != nil {
		return indexObject{}, err
	}

	obj := indexObject{key: key}

	if err := b.do(ctx, "reading index object attributes", func(ctx context.Context) error {
		attrs, err := b.b.Attributes(ctx, key)
		if err != nil {
			return err
		}

		obj.size, obj.modTime = attrs.Size, attrs.ModTime

		return nil
	}); err != nil {
		return indexObject{}, indexError("reading", key, err)
	}

	return obj, nil
}

// deleteIndex removes one index key.
//
// Deletion is not part of any ordinary write (AC3): the only callers are
// compaction, collecting keys a durably visible checkpoint has absorbed, and
// prune, collecting a tombstone nothing can still need. A key that is already
// gone is reported as ErrNotFound so that "somebody else collected it" can be
// treated as the success it is rather than as a failure to explain.
func (b *bucket) deleteIndex(ctx context.Context, key string) error {
	if err := validateIndexKey(key); err != nil {
		return err
	}

	if err := b.do(ctx, "deleting an index object", func(ctx context.Context) error {
		return b.b.Delete(ctx, key)
	}); err != nil {
		return indexError("deleting", key, err)
	}

	return nil
}

// listIndexPage returns one page of the keys under prefix, in the order the
// provider sorts them.
//
// The zero cursor starts the listing and the cursor a page carries continues
// it; a spent cursor is refused, because serving it would silently restart a
// listing a caller believed it had finished. limit asks for at most that many
// keys and is clamped to indexListPageSize, so a fold that wants twenty
// entries pays for twenty and not for a thousand.
//
// The cursor only advances on a successful page. A failed page leaves the
// caller's cursor pointing at the page still to be fetched, which is what
// makes a retried listing resume rather than skip.
//
// **The order is lexicographic over the whole key on every cloud provider, and
// on fileblob it is lexicographic only where the grammar earns it.** fileblob
// walks the directory tree and does not sort what the walk produces, so two
// sibling directories where one name is a proper prefix of the other come back
// in the walk's order and not the key's. §4.2's invariant, and the rule stated
// with indexRefPrefix in blobkeys.go, are what keep every prefix this store
// lists out of that case; a caller inventing a new prefix to list is the one
// who has to check it still holds.
func (b *bucket) listIndexPage(ctx context.Context, prefix string, cursor indexCursor, limit int) (indexPage, error) {
	if err := validateIndexPrefix(prefix); err != nil {
		return indexPage{}, err
	}

	if !cursor.more() {
		return indexPage{}, fmt.Errorf("continuing a finished listing of %q: %w",
			truncateForMessage(prefix), errInvalidIndexKey)
	}

	if limit <= 0 || limit > indexListPageSize {
		limit = indexListPageSize
	}

	token := cursor.token
	if len(token) == 0 {
		token = blob.FirstPageToken
	}

	var (
		objects []*blob.ListObject
		next    []byte
	)

	if err := b.do(ctx, "listing index objects", func(ctx context.Context) error {
		found, nextToken, err := b.b.ListPage(ctx, token, limit, &blob.ListOptions{Prefix: prefix})
		if err != nil {
			return err
		}

		objects, next = found, nextToken

		return nil
	}); err != nil {
		return indexPage{}, fmt.Errorf("listing index objects under %q in bucket %s: %w",
			truncateForMessage(prefix), b, err)
	}

	page := indexPage{
		objects: make([]indexObject, 0, len(objects)),
		next:    indexCursor{token: next, spent: len(next) == 0},
	}

	for _, obj := range objects {
		// A directory placeholder appears only when a listing sets a
		// delimiter, which this one does not; skipped anyway, because a
		// caller parsing one as a key would be parsing a name no writer wrote.
		if obj.IsDir {
			continue
		}

		page.objects = append(page.objects, indexObject{key: obj.Key, size: obj.Size, modTime: obj.ModTime})
	}

	return page, nil
}

// validateIndexKey refuses anything that is not a key the grammar in
// blobkeys.go defines.
//
// It is a whitelist, and a separate one from validateRef, for the reason
// stated at the top of this file. Three checks, each with its own job:
//
// The root check is the one that makes the index and the evidence able to
// share a bucket. Every index key begins with "_wsaw/" and no artifact key can
// ever begin with an underscore, because validKind is a closed list of kinds
// spelled in [a-z0-9-] — so a write through this door cannot land on an
// artifact, and a write through the artifact door cannot land on the index,
// whichever way either grammar grows. That is a proof about the two alphabets
// rather than a list of reserved names somebody has to remember to update.
//
// The size checks are for the providers rather than for the grammar. No key
// the grammar produces comes near either bound; they are here so that a later
// production which does is refused at the door, rather than discovered as a
// write that succeeds against S3 and fails against a local disk whose
// filesystem caps a path component at 255 bytes.
//
// The grammar check is by parsing, which is what keeps the door and the
// readers in agreement: a key this admits is a key some parser will read back.
func validateIndexKey(key string) error {
	if len(key) > maxIndexKeyBytes {
		return fmt.Errorf("index key %q is %d bytes, over the %d this store writes: %w",
			truncateForMessage(key), len(key), maxIndexKeyBytes, errInvalidIndexKey)
	}

	if !strings.HasPrefix(key, indexRoot) {
		return indexKeyError(key)
	}

	for _, segment := range strings.Split(key, refSeparator) {
		if len(segment) > maxIndexComponentBytes {
			return fmt.Errorf("index key %q has a %d-byte path component, over the %d this store writes: %w",
				truncateForMessage(key), len(segment), maxIndexComponentBytes, errInvalidIndexKey)
		}
	}

	if !wellFormedIndexKey(key) {
		return indexKeyError(key)
	}

	return nil
}

// validateIndexPrefix accepts what a listing of the index may be narrowed to.
//
// A prefix cannot be parsed the way a key can — it is half of one — so this
// checks the three things that still hold for every prefix of every
// production. It is inside the index root, so a listing can never enumerate
// the evidence or another tool's objects. Every byte of it is one the grammar
// can produce: lowercase, digits, the three separators, and the two uppercase
// letters that appear in a readable timestamp and nowhere else. And no segment
// of it is "." or ".." or empty.
//
// That last check is not redundant with the byte set, because "." is a byte
// the grammar does produce — it is the field separator — and a prefix is the
// one place in this file where a caller's string is handed to a provider
// without being parsed first. fileblob resolves the directory part of a prefix
// against the bucket root before it filters, and while it does guard its own
// root afterwards, a door whose safety depends on a provider's internal check
// is a door that is safe on one provider (Tenet 9). No production has an empty
// segment either, so a doubled separator is refused with the rest of it; a
// single trailing separator is how a directory prefix is spelled and is the
// one empty segment allowed.
func validateIndexPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, indexRoot) {
		return fmt.Errorf("index prefix %q is not inside %q: %w",
			truncateForMessage(prefix), indexRoot, errInvalidIndexKey)
	}

	if len(prefix) > maxIndexKeyBytes {
		return fmt.Errorf("index prefix %q is %d bytes, over the %d this store writes: %w",
			truncateForMessage(prefix), len(prefix), maxIndexKeyBytes, errInvalidIndexKey)
	}

	for i := range len(prefix) {
		if !indexKeyByte(prefix[i]) {
			return fmt.Errorf("index prefix %q holds a byte this store never writes into a key: %w",
				truncateForMessage(prefix), errInvalidIndexKey)
		}
	}

	segments := strings.Split(strings.TrimSuffix(prefix, refSeparator), refSeparator)
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("index prefix %q has a path segment no key of this store has: %w",
				truncateForMessage(prefix), errInvalidIndexKey)
		}
	}

	return nil
}

// indexKeyByte reports whether c is a byte the key grammar can produce.
func indexKeyByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '-', c == '_', c == '/':
		return true
	case c == 'T', c == 'Z':
		// The two literals in a readable timestamp, and the only uppercase
		// bytes anywhere in the grammar — every component encoder lowercases.
		return true
	default:
		return false
	}
}

// indexError gives a bucket failure on an index object the shape the rest of
// the store already understands.
//
// A missing key is ErrNotFound, as it is for an artifact, so that a caller can
// tell an object that is not there from a bucket that is broken. Whether "not
// there" then means "not yet" or "never" is the reader's judgement and depends
// on which key it asked for; this layer does not guess (AC6).
func indexError(verb, key string, err error) error {
	if gcerrors.Code(err) == gcerrors.NotFound {
		return fmt.Errorf("index object %s: %w", key, ErrNotFound)
	}

	return fmt.Errorf("%s index object %s: %w", verb, key, err)
}
