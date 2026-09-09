package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gocloud.dev/gcerrors"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The failure modes of a bucket, tested rather than assumed (Story 8.9, AC3).
//
// A directory on local disk fails in two ways — it is not there, or the
// permission bits say no — and a real object store fails in many more: a write
// refused by a policy, a request that lands and reports failure anyway, a key
// that a lifecycle rule took away, an object whose bytes are not the ones its
// key names. Story 8.2 requires that each of those is *recorded* and not fatal,
// which is a promise nothing can check by inspection.
//
// So each is injected here, through the same fake bucket the eventual
// consistency tests use (Story 8.10, AC15) — a gocloud driver, so a fault
// travels through the real blob.Bucket, the real retry policy and the real
// gcerrors classification — and every test runs against whichever store the
// suite is configured for, because "reported and not fatal" is a promise of the
// seam and not of one implementation.
//
// The tests here are the ones a directory cannot host. What a lost, tampered or
// truncated object does to a read is asserted for every bucket by the shared
// suite in store_test.go, which no longer reaches the evidence with os.ReadFile
// (AC1).

// faulty opens a store whose artifact bucket is a fake that can be told to
// misbehave, and returns both.
//
// Retrying is off. Every fault below is either permanent — a refusal is a
// policy, and trying it three times only makes the failure slower — or is meant
// to be seen exactly once, and a retrier in the middle would decide how many
// times a test's schedule is spent.
func faulty(t *testing.T) (*lagBucket, store.Store) {
	t.Helper()

	fake := newLagBucket(t)

	opts := storeOptions(t)
	opts.ArtifactDir = fake.url()
	opts.MaxAttempts = 1

	return fake, openAt(t, opts)
}

// refuse schedules a refusal: the operation does not happen, and the provider
// says so with a code no retry would help with.
func refuse(fake *lagBucket, op lagOp, prefix string) {
	fake.schedule(lagFault{
		op: op, prefix: prefix, mode: lagFailBefore,
		code: gcerrors.PermissionDenied, left: 1,
	})
}

// TestAScanWhoseDocumentIsRefusedIsNotRecordedAsStored is the write half of
// AC3, and the one that decides whether wsaw can be trusted at all.
//
// A bucket that refuses the write must not leave a store claiming to hold the
// scan. Story 8.2, AC4 puts the bucket first and the index second for exactly
// this reason: the failure this order produces is an unreferenced object, which
// the sweep collects, and never an index entry pointing at nothing, which reads
// as a corrupt result (Tenet 5 — a failed observation must not look like a
// clean one).
func TestAScanWhoseDocumentIsRefusedIsNotRecordedAsStored(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	refuse(fake, lagPut, "result/")

	err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject))
	if err == nil {
		t.Fatal("a scan whose document the bucket refused was stored anyway")
	}

	// Reported, and reported as what it was: an operator reading this line has
	// to be able to tell a bucket that said no from a database that did.
	if !containsAll(err.Error(), "scan-1") {
		t.Errorf("the failure does not name the scan: %v", err)
	}

	if found, err := s.HasResult("site", model.ConsentReject, "scan-1"); err != nil || found {
		t.Errorf("HasResult = %v, %v after a refused write, want false: the index must not name a document that was never written",
			found, err)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetResult after a refused write = %v, want ErrNotFound", err)
	}

	// And the store still works: one refused write is one lost scan, never a
	// store that has to be restarted.
	if err := s.PutResult(result("scan-2", time.Now(), model.ConsentReject)); err != nil {
		t.Fatalf("the store did not survive a refused write: %v", err)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-2"); err != nil {
		t.Errorf("the scan after the refused one is not readable: %v", err)
	}
}

// TestAnAmbiguousWriteLeavesGarbageAndNotAHalfResult is the harder half: the
// object landed and the response was lost, which is the failure a retry cannot
// distinguish from a write that never happened.
//
// What must come out of it is the state Story 8.2, AC4 chose: an unreferenced
// object, collectable by the sweep, and no index entry. The write is then run
// again, as a scheduler would run the scan again, and the second attempt has to
// produce one object and one entry rather than a duplicate of either — content
// addressing is what makes that free (Story 8.10, AC10).
func TestAnAmbiguousWriteLeavesGarbageAndNotAHalfResult(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	// The write happens; the report of it does not arrive.
	fake.loseResponse(lagPut, "result/", 1)

	res := result("scan-1", time.Now(), model.ConsentReject)
	if err := s.PutResult(res); err == nil {
		t.Fatal("a write whose response was lost was reported as successful")
	}

	documents := fake.keysUnder("result/")
	if len(documents) != 1 {
		t.Fatalf("the bucket holds %d documents after the ambiguous write, want 1: %v", len(documents), documents)
	}

	if found, err := s.HasResult("site", model.ConsentReject, "scan-1"); err != nil || found {
		t.Errorf("HasResult = %v, %v, want false: the object is there and nothing references it yet", found, err)
	}

	// The same scan, stored again.
	if err := s.PutResult(res); err != nil {
		t.Fatalf("re-storing the scan after an ambiguous write: %v", err)
	}

	if again := fake.keysUnder("result/"); len(again) != 1 {
		t.Errorf("the retry left %d documents, want 1: an identical document is one object", len(again))
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("the re-stored scan is not readable: %v", err)
	}

	if got.ScanID != "scan-1" {
		t.Errorf("read back scan %q, want scan-1", got.ScanID)
	}

	// One entry, not two. Listing is the question a duplicate would show up in.
	summaries, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(summaries) != 1 {
		t.Errorf("the history holds %d results after one scan stored twice, want 1", len(summaries))
	}
}

// TestAScreenshotTheBucketRefusesIsReportedToTheScanner is the same refusal on
// the artifacts a capture writes before the document.
//
// PutArtifact is what the scanner calls with a screenshot or a stored body in
// hand, and a bucket that refuses one has to say so: a capture that quietly
// dropped its screenshot and let the scan finish would be the worst bug this
// product can have (AGENTS §3.4).
func TestAScreenshotTheBucketRefusesIsReportedToTheScanner(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	refuse(fake, lagPut, "screenshot-before-consent/")

	ref, err := s.PutArtifact("screenshot-before-consent", []byte("a screenshot nobody will see"))
	if err == nil {
		t.Fatalf("a screenshot the bucket refused was reported as stored at %s", ref)
	}

	if keys := fake.keysUnder("screenshot-before-consent/"); len(keys) != 0 {
		t.Errorf("the bucket holds %v after a refused write, want nothing", keys)
	}

	// The next one works, and is not poisoned by the refusal.
	if _, err := s.PutArtifact("screenshot-before-consent", []byte("a screenshot that lands")); err != nil {
		t.Errorf("the store did not survive a refused artifact write: %v", err)
	}
}

// TestEvidenceDeletedBehindTheStoresBackIsReportedNotInferred is AC3's third
// case, on the artifacts rather than on the document: a lifecycle rule, a
// restore that was short, or a person with a console takes a screenshot away.
//
// The result stays readable and goes on naming the screenshot, and the read of
// the screenshot itself reports that it is gone. Absence of evidence is
// recorded, never inferred (Tenet 5) — a store that dropped the reference
// instead would leave a result that looks as though nothing was ever captured.
func TestEvidenceDeletedBehindTheStoresBackIsReportedNotInferred(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	shot, body := withEvidence(t, s, "scan-1", time.Now(),
		[]byte("a screenshot the bucket will lose"), []byte("a body the bucket will lose"))

	// Not a delete: the object is simply not there any more, which is what a
	// bucket that lost bytes looks like from outside.
	fake.forget(shot)

	res, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("a lost screenshot made the result unreadable: %v", err)
	}

	if len(res.Screenshots) != 1 || res.Screenshots[0].Ref != shot {
		t.Errorf("the result stopped naming the screenshot it captured: %+v", res.Screenshots)
	}

	if _, err := s.GetArtifact(shot); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the lost screenshot = %v, want ErrNotFound", err)
	}

	if _, err := s.StatArtifact(t.Context(), shot); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("stat of the lost screenshot = %v, want ErrNotFound", err)
	}

	// The evidence beside it is untouched, which is the "one lost object is one
	// lost object" half of Story 8.2, AC5.
	if _, err := s.GetArtifact(body); err != nil {
		t.Errorf("the stored body went with the screenshot: %v", err)
	}
}

// TestAPruneSurvivesABucketThatRefusesToDelete is Story 8.5, AC4 driven by a
// provider's refusal rather than by a directory's permission bits, so that it
// holds for a bucket that has no permission bits.
//
// The prune must finish, count the refusal, and leave the key for the next
// sweep: the references are the work list, so forgetting them before the object
// is gone would turn a refused delete into a permanent leak.
func TestAPruneSurvivesABucketThatRefusesToDelete(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	old := time.Now().Add(-30 * 24 * time.Hour)
	shot, _ := withEvidence(t, s, "scan-old", old, []byte("undeletable"), []byte("a body"))

	refuse(fake, lagDelete, shot)

	stats, err := s.Prune(t.Context(), time.Now(), store.Retention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("a refused deletion failed the whole prune: %v", err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("ResultsDeleted = %d, want 1: retention still has to remove the result", stats.ResultsDeleted)
	}

	if stats.ArtifactsFailed != 1 {
		t.Errorf("ArtifactsFailed = %d, want 1", stats.ArtifactsFailed)
	}

	assertStored(t, s, shot, "the artifact the bucket would not delete")

	// The next sweep finds the same key, because the refusal left it on the
	// work list rather than forgetting it.
	swept, err := s.Sweep(t.Context(), time.Now().Add(48*time.Hour), store.SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if swept.ArtifactsDeleted != 1 {
		t.Errorf("the sweep after a refused delete removed %d artifacts, want 1", swept.ArtifactsDeleted)
	}

	assertGone(t, s, shot, "the artifact the sweep came back for")
}

// TestAPingReportsABucketThatWillNotAnswer is the readiness half: a store whose
// bucket answers with a failure is not ready, whatever the index says.
//
// It is the same promise TestPingCoversTheArtifactBucket makes about a
// directory that has gone away, made without a directory — which is what an
// operator running against object storage actually has.
func TestAPingReportsABucketThatWillNotAnswer(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("a healthy store does not answer a ping: %v", err)
	}

	// Every listing fails, which is what a bucket behind a broken endpoint or
	// an expired credential looks like.
	fake.failNext(lagList, "", 100)

	if err := s.Ping(t.Context()); err == nil {
		t.Error("a store whose bucket answers nothing reported itself ready")
	}
}

// TestAVerifySurvivesABucketThatFailsAListing is the rebuild's half of the same
// rule (Story 8.11, AC7 and Story 8.9, AC3): a walk that cannot be completed is
// reported as a failure, and never as a verify that found nothing wrong.
//
// The dangerous answer here is a clean one. A verify that returned "no drift"
// because it could not list the bucket would tell an operator their index is
// sound at the exact moment it cannot be checked.
func TestAVerifySurvivesABucketThatFailsAListing(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	if err := s.PutResult(result("scan-1", time.Now(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	fake.failNext(lagList, "", 100)

	stats, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if err == nil {
		t.Fatalf("a verify that could not list the bucket reported %+v and no error", stats)
	}

	if errors.Is(err, store.ErrIndexDrift) {
		t.Errorf("a bucket that would not answer was reported as index drift: %v", err)
	}
}

// containsAll reports whether s contains every one of the substrings.
func containsAll(s string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}

	return true
}
