package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// TestListingReadsNoDocumentWhicheverStoreKeepsTheIndex is Story 8.3, AC2
// carried to the store the criterion cannot be stated over unchanged
// (Story 8.9, AC5).
//
// "Listing performs zero bucket operations" is the SQL store's promise, and it
// is asserted as a count of zero by TestListingPerformsNoBucketOperations,
// beside the implementation where the bucket can be seen. It cannot hold where
// the index *is* the bucket: a listing there is one prefix listing plus one
// small entry object per summary, which is what Story 8.10, §6.4 chose and what
// TestWhatEachReadPathCostsInRequests pins down request by request.
//
// What both stores owe, and what this asserts for whichever one the suite is
// configured for, is that a listing never opens a *document*. That is the
// regression the story is about: a summary is a few hundred bytes and a scan's
// document runs to megabytes, so a listing that read one per row would turn a
// page of history into a wait and a bill on every provider — and would look
// perfectly fast on the local disk it was written on.
func TestListingReadsNoDocumentWhicheverStoreKeepsTheIndex(t *testing.T) {
	t.Parallel()

	fake, s := faulty(t)

	base := time.Now().Add(-time.Hour)

	const scans = 5

	for i := range scans {
		id := fmt.Sprintf("scan-%d", i)
		if err := s.PutResult(result(id, base.Add(time.Duration(i)*time.Minute), model.ConsentReject)); err != nil {
			t.Fatal(err)
		}
	}

	fake.forgetRequests()

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil || len(got) != scans {
		t.Fatalf("ListResults = %d summaries, %v", len(got), err)
	}

	if found, err := s.HasResult("site", model.ConsentReject, "scan-0"); err != nil || !found {
		t.Fatalf("HasResult = %v, %v", found, err)
	}

	if series, err := s.Series(); err != nil || len(series) != 1 {
		t.Fatalf("Series = %v, %v", series, err)
	}

	if n := fake.bodiesReadUnder("result/"); n != 0 {
		t.Errorf("listing %d results opened %d documents, want none", scans, n)
	}

	// The summaries are the ones that were written, so "read no document" is
	// not "answered from nothing".
	for _, sm := range got {
		if sm.Requests != 1 || sm.ThirdPartyDomains != 1 {
			t.Errorf("%s: the summary is not the one that was stored: %+v", sm.ScanID, sm)
		}
	}

	// And the count is of something: reading a result does open one.
	if _, err := s.GetResult("site", model.ConsentReject, "scan-0"); err != nil {
		t.Fatal(err)
	}

	if n := fake.bodiesReadUnder("result/"); n == 0 {
		t.Error("reading a result opened no document, so the count above proves nothing")
	}
}
