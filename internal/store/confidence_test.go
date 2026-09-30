package store_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func scored(id string, at time.Time, score int, band model.ConfidenceBand) *model.Result {
	res := result(id, at, model.ConsentReject)
	res.Confidence = &model.Confidence{
		Score: score,
		Band:  band,
		Reasons: []model.ConfidenceReason{
			{Signal: model.SignalTermination, Observed: "the page went idle"},
			{Signal: model.SignalDuration, Points: 100 - score, Observed: "2s", Reference: "median 10s over 7 earlier scans"},
		},
		DurationReference: &model.DurationReference{Median: 10 * time.Second, Scans: 7},
	}

	return res
}

// Story 5.35, AC10: the listing carries the score and band the document
// holds, copied rather than computed, and a document without one — written
// before schema 2.1 — lists as not computed, absent from the JSON rather
// than a zero.
func TestTheListingCarriesTheDocumentsConfidence(t *testing.T) {
	t.Parallel()

	s := open(t)
	at := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)

	old := result("scan-old", at, model.ConsentReject)
	if err := s.PutResult(old); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	if err := s.PutResult(scored("scan-new", at.Add(time.Hour), 85, model.ConfidenceMedium)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	sums, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil || len(sums) != 2 {
		t.Fatalf("ListResults = %d summaries, %v", len(sums), err)
	}

	if got := sums[0]; got.ConfidenceScore == nil || *got.ConfidenceScore != 85 || got.ConfidenceBand != model.ConfidenceMedium {
		t.Errorf("scored summary = %v %q, want 85 medium", got.ConfidenceScore, got.ConfidenceBand)
	}

	if got := sums[1]; got.ConfidenceScore != nil || got.ConfidenceBand != "" {
		t.Errorf("an unscored document listed as %d %q, want not computed", *got.ConfidenceScore, got.ConfidenceBand)
	}

	encoded, err := json.Marshal(sums[1])
	if err != nil {
		t.Fatalf("encoding a summary: %v", err)
	}

	if strings.Contains(string(encoded), "confidence") {
		t.Errorf("a summary with no score encodes one: %s", encoded)
	}
}

// A failed scan's score of 0 is a score, not an absence.
func TestAZeroScoreIsStoredAsAScore(t *testing.T) {
	t.Parallel()

	s := open(t)

	if err := s.PutResult(scored("scan-1", time.Now().UTC(), 0, model.ConfidenceNone)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	sums, err := s.ListResults("site", model.ConsentReject, 1)
	if err != nil || len(sums) != 1 {
		t.Fatalf("ListResults = %v, %v", sums, err)
	}

	if got := sums[0]; got.ConfidenceScore == nil || *got.ConfidenceScore != 0 || got.ConfidenceBand != model.ConfidenceNone {
		t.Errorf("summary = %v %q, want 0 none", got.ConfidenceScore, got.ConfidenceBand)
	}
}

// Story 5.35, AC7: the document is the record, so the reasons and the median
// come back exactly as they were written.
func TestTheDocumentKeepsTheReasonsAndTheMedian(t *testing.T) {
	t.Parallel()

	s := open(t)
	want := scored("scan-1", time.Now().UTC(), 70, model.ConfidenceMedium)

	if err := s.PutResult(want); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	got, err := s.GetResult("site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}

	if !reflect.DeepEqual(got.Confidence, want.Confidence) {
		t.Errorf("confidence read back as %+v, want %+v", got.Confidence, want.Confidence)
	}
}

// Story 5.35, AC5: only earlier clean scans of the same series are the
// reference, newest first and bounded.
func TestRecentCleanDurations(t *testing.T) {
	t.Parallel()

	s := open(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	put := func(id string, offset time.Duration, d time.Duration, mutate func(*model.Result)) {
		t.Helper()

		res := result(id, at.Add(offset), model.ConsentReject)
		res.Duration = d

		if mutate != nil {
			mutate(res)
		}

		if err := s.PutResult(res); err != nil {
			t.Fatalf("PutResult %s: %v", id, err)
		}
	}

	put("oldest", 1*time.Hour, 1*time.Second, nil)
	put("older", 2*time.Hour, 2*time.Second, nil)
	put("timeout", 3*time.Hour, 60*time.Second, func(r *model.Result) { r.Termination = model.TermTimeout })
	put("errored", 4*time.Hour, 90*time.Second, func(r *model.Result) {
		r.Termination = model.TermError
		r.Error = "crashed"
	})
	put("idle-with-error", 5*time.Hour, 70*time.Second, func(r *model.Result) { r.Error = "crashed late" })
	put("newer", 6*time.Hour, 3*time.Second, nil)
	put("other-mode", 7*time.Hour, 80*time.Second, func(r *model.Result) { r.ConsentMode = model.ConsentAccept })
	put("after", 9*time.Hour, 4*time.Second, nil)

	got, err := s.RecentCleanDurations("site", model.ConsentReject, at.Add(8*time.Hour), 10)
	if err != nil {
		t.Fatalf("RecentCleanDurations: %v", err)
	}

	want := []time.Duration{3 * time.Second, 2 * time.Second, 1 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("durations = %v, want %v: only earlier idle scans without an error, of this series, newest first", got, want)
	}

	limited, err := s.RecentCleanDurations("site", model.ConsentReject, at.Add(8*time.Hour), 2)
	if err != nil || !reflect.DeepEqual(limited, want[:2]) {
		t.Errorf("limited durations = %v, %v, want %v", limited, err, want[:2])
	}
}

// Story 5.35, AC10: a store at version 8 gains the columns, its rows read as
// not computed, and new scans are scored — in whichever dialect runs this.
func TestUpgradingToTheConfidenceColumnsKeepsEveryRow(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	at := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)

	fresh, err := store.OpenSQL(t.Context(), opts)
	if err != nil {
		t.Fatalf("creating the store to take back: %v", err)
	}

	if err := fresh.PutResult(result("scan-before", at, model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	db := rawDB(t, opts)

	for _, stmt := range []string{
		`alter table results drop column confidence_score`,
		`alter table results drop column confidence_band`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("taking the store back to version 8: %s: %v", stmt, err)
		}
	}

	rewindSchemaVersion(t, db, opts, 8)

	s, err := store.OpenSQL(t.Context(), opts)
	if err != nil {
		t.Fatalf("migrating a version-8 store: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	if err := s.PutResult(scored("scan-after", at.Add(time.Hour), 91, model.ConfidenceHigh)); err != nil {
		t.Fatalf("PutResult after the migration: %v", err)
	}

	sums, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil || len(sums) != 2 {
		t.Fatalf("ListResults = %d, %v", len(sums), err)
	}

	if sums[0].ConfidenceScore == nil || *sums[0].ConfidenceScore != 91 {
		t.Errorf("the scan stored after the migration lists as %v, want 91", sums[0].ConfidenceScore)
	}

	if sums[1].ConfidenceScore != nil {
		t.Errorf("the row from before the migration lists a score of %d, want not computed", *sums[1].ConfidenceScore)
	}

	if got, want := recordedSchemaVersion(t, opts), freshVersion(t); got != want {
		t.Errorf("the migrated store records schema version %d, want %d", got, want)
	}
}

// Story 5.35, AC10: the index columns are a copy of the document, so a
// rebuild from the bucket restores the stored score rather than a new one.
func TestARebuildRestoresTheStoredConfidence(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)

	if err := first.PutResult(scored("scan-1", at, 64, model.ConfidenceMedium)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	if err := first.PutResult(result("scan-0", at.Add(-time.Hour), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	second := openAt(t, lostIndex(t, opts))

	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{}); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	sums, err := second.ListResults("site", model.ConsentReject, 0)
	if err != nil || len(sums) != 2 {
		t.Fatalf("ListResults after rebuild = %d, %v", len(sums), err)
	}

	if got := sums[0]; got.ConfidenceScore == nil || *got.ConfidenceScore != 64 || got.ConfidenceBand != model.ConfidenceMedium {
		t.Errorf("rebuilt summary = %v %q, want 64 medium", got.ConfidenceScore, got.ConfidenceBand)
	}

	if got := sums[1]; got.ConfidenceScore != nil {
		t.Errorf("a rebuilt pre-2.1 document gained a score: %d", *got.ConfidenceScore)
	}

	again, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("second RebuildIndex: %v", err)
	}

	if again.EntriesAdded != 0 || again.EntriesRefreshed != 0 {
		t.Errorf("a second rebuild added %d and refreshed %d entries; the confidence columns must already agree",
			again.EntriesAdded, again.EntriesRefreshed)
	}
}
