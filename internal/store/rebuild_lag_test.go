package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The rebuild against a bucket that behaves the way object storage does
// (Story 8.11, AC13).
//
// The shared suite in rebuild_test.go runs against every store and against a
// bucket that answers every request correctly, which is where the recovery
// itself is specified. What is here is the two things only this store, on a
// bucket that can lose a write or serve a listing that has not caught up, can
// get into — and both of them are states Story 8.10 deliberately left for this
// command to be the answer to.

// TestARebuildRestoresTheEntryAnInterruptedWriteNeverWrote is Story 8.10, AC6
// met from the other end.
//
// PutResult writes the document first and the index objects after it, so a
// process interrupted in between leaves a document that is in the bucket and in
// no listing. Story 8.10 made that state survivable — the sweep is forbidden
// from collecting a result document, precisely so that the evidence is still
// there to derive from — and left the deriving to this command. This is it: the
// scan is invisible before the rebuild and in its history afterwards, and
// nothing had to be scanned again.
func TestARebuildRestoresTheEntryAnInterruptedWriteNeverWrote(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	// The object addressed by the scan ID never lands, so neither does the
	// entry after it: the document is in the bucket and nothing points at it.
	fake.failNext(lagPut, siteByIDDir, blobRetryAttempts)

	interrupted := result("scan-2", siteStart().Add(time.Minute), model.ConsentReject)

	if err := s.PutResult(interrupted); err == nil {
		t.Fatal("a PutResult whose index write kept failing reported success")
	}

	if got := listedScanIDs(t, s); len(got) != 1 || got[0] != "scan-1" {
		t.Fatalf("the listing shows %v, want only the scan whose write finished", got)
	}

	// A verify sees the document with no entry, and says so rather than
	// reporting a history that is one scan short.
	drifted, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if !errors.Is(err, store.ErrIndexDrift) {
		t.Fatalf("a verify over an interrupted write reported %v, want ErrIndexDrift", err)
	}

	if drifted.EntriesAdded != 1 {
		t.Errorf("the verify found %d documents with no entry, want 1: %+v",
			drifted.EntriesAdded, drifted.DriftList)
	}

	stats, err := s.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	if stats.EntriesAdded != 1 || stats.EntriesUnchanged != 1 {
		t.Errorf("the rebuild added %d and left %d unchanged, want 1 and 1",
			stats.EntriesAdded, stats.EntriesUnchanged)
	}

	if got := listedScanIDs(t, s); len(got) != 2 || !slices.Contains(got, "scan-2") {
		t.Errorf("the listing after the rebuild shows %v, want the interrupted scan in it", got)
	}

	if _, err := s.GetResult("site", model.ConsentReject, "scan-2"); err != nil {
		t.Errorf("the restored scan reads back as %v", err)
	}

	// And the second run has nothing left to do.
	again, err := s.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("the second rebuild: %v", err)
	}

	if again.Writes != 0 {
		t.Errorf("the second rebuild issued %d writes, want none", again.Writes)
	}
}

// TestVerifyFindsTheApprovalWhoseAuditEntryNeverLanded is the gap Story 8.10
// documented and deferred here, and the reason AC10 exists in the words the
// README already uses.
//
// The approval itself is never at risk: the decision and the audit entry that
// explains it are one object under one key. What can be missing is the copy
// filed under audit/ that the log view is read through, and it is re-created by
// the next decision for that target — so a target that takes no further
// decision keeps the gap until something goes looking. Healing it on every read
// of the log would mean listing every target's decisions on every render, to
// repair something that never endangers the record; finding it here costs one
// listing and one check per decision, on a command that is run deliberately.
func TestVerifyFindsTheApprovalWhoseAuditEntryNeverLanded(t *testing.T) {
	t.Parallel()

	fake := newLagBucket(t)
	s := lagStoreAt(t, fake, siteStart)

	if err := s.PutResult(result("scan-1", siteStart(), model.ConsentReject)); err != nil {
		t.Fatal(err)
	}

	fake.failNext(lagPut, auditPrefix, blobRetryAttempts)

	if _, err := s.SetBaseline("site", model.ConsentReject, "scan-1", "eva", "agreed"); err != nil {
		t.Fatalf("an approval whose audit pointer could not be written reported %v, "+
			"want the approval it had already committed", err)
	}

	if pointers := fake.keysUnder(auditPrefix); len(pointers) != 0 {
		t.Fatalf("the audit prefix holds %v, want the pointer that failed to be missing", pointers)
	}

	stats, err := s.RebuildIndex(t.Context(), store.RebuildOptions{Mode: store.RebuildVerify})
	if !errors.Is(err, store.ErrIndexDrift) {
		t.Fatalf("a verify over a decision with no audit entry reported %v, want ErrIndexDrift", err)
	}

	found := false

	for _, drift := range stats.DriftList {
		if strings.Contains(drift.Kind, "audit log") {
			found = true
		}
	}

	if !found {
		t.Errorf("the verify reported %+v, want the missing audit entry among it", stats.DriftList)
	}

	// The approval itself is intact and is reported as something no rebuild
	// could have produced (AC6).
	if stats.Unrecoverable.Baselines != 1 {
		t.Errorf("the verify counted %d baseline decisions, want the approval",
			stats.Unrecoverable.Baselines)
	}

	if _, err := s.GetBaseline("site", model.ConsentReject); err != nil {
		t.Errorf("the approval reads back as %v; a verify must change nothing", err)
	}
}
