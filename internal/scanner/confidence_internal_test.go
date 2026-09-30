package scanner

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// durationStore is a ResultStore that answers the duration query and records
// what was stored, which is all persist asks of it.
type durationStore struct {
	durations []time.Duration
	err       error

	asked struct {
		target string
		mode   model.ConsentMode
		before time.Time
		limit  int
	}
	stored *model.Result
}

func (d *durationStore) PutArtifact(string, []byte) (string, error) { return "", nil }

func (d *durationStore) PutResult(res *model.Result) error {
	d.stored = res

	return nil
}

func (d *durationStore) GetBaseline(string, model.ConsentMode) (*store.Baseline, error) {
	return nil, store.ErrNotFound
}

func (d *durationStore) PreviousResult(string, model.ConsentMode, string) (*model.Result, error) {
	return nil, store.ErrNotFound
}

func (d *durationStore) RecentCleanDurations(
	target string, mode model.ConsentMode, before time.Time, limit int,
) ([]time.Duration, error) {
	d.asked.target, d.asked.mode, d.asked.before, d.asked.limit = target, mode, before, limit

	return d.durations, d.err
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func cleanResult() *model.Result {
	return &model.Result{
		ScanID:      "scan-1",
		Target:      "site",
		ConsentMode: model.ConsentReject,
		StartedAt:   time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC),
		Duration:    2 * time.Second,
		Termination: model.TermIdle,
		Consent:     model.Consent{Outcome: model.OutcomeApplied},
		Requests:    []model.Request{{URL: "https://example.com/", Status: 200}},
	}
}

// Story 5.35, AC7: the score is written with the document, together with the
// median it was judged against, and the history is asked for this series,
// before this scan, and no more of it than the score uses.
func TestPersistStoresTheScoreWithTheMedianItUsed(t *testing.T) {
	t.Parallel()

	st := &durationStore{}
	for range 12 {
		st.durations = append(st.durations, 10*time.Second)
	}

	s := &Scanner{deps: Deps{Store: st}}
	res := cleanResult()

	s.persist(t.Context(), quiet(), res)

	if st.stored == nil || st.stored.Confidence == nil {
		t.Fatalf("the stored result carries no confidence: %+v", st.stored)
	}

	c := st.stored.Confidence

	// 2s against a 10s median is below a quarter.
	if c.Score != 70 || c.Band != model.ConfidenceMedium {
		t.Errorf("score = %d %s, want 70 medium for a scan a fifth of its usual length", c.Score, c.Band)
	}

	if c.DurationReference == nil || c.DurationReference.Median != 10*time.Second || c.DurationReference.Scans != 10 {
		t.Errorf("duration reference = %+v, want the 10s median over 10 scans", c.DurationReference)
	}

	if st.asked.target != "site" || st.asked.mode != model.ConsentReject ||
		!st.asked.before.Equal(res.StartedAt) || st.asked.limit != 10 {
		t.Errorf("durations were asked for %+v, want this series before this scan, at most 10", st.asked)
	}
}

func TestPersistScoresEvenWhenTheHistoryCannotBeRead(t *testing.T) {
	t.Parallel()

	st := &durationStore{err: errors.New("database is locked")}
	s := &Scanner{deps: Deps{Store: st}}

	s.persist(t.Context(), quiet(), cleanResult())

	c := st.stored.Confidence
	if c == nil || c.Score != 100 || c.Reasons[2].NotAssessed == "" {
		t.Errorf("confidence = %+v, want every other signal counted and duration not assessed", c)
	}
}

// A one-shot scan with no store still reports how far it can be trusted.
func TestPersistScoresAResultWithNowhereToStoreIt(t *testing.T) {
	t.Parallel()

	res := cleanResult()
	(&Scanner{}).persist(t.Context(), quiet(), res)

	if res.Confidence == nil || res.Confidence.Reasons[2].NotAssessed == "" {
		t.Errorf("confidence = %+v, want a score with duration not assessed", res.Confidence)
	}
}

func TestPersistScoresAFailedScanAsNoObservation(t *testing.T) {
	t.Parallel()

	st := &durationStore{}
	res := cleanResult()
	res.Termination, res.Error = model.TermError, "navigation failed"

	(&Scanner{deps: Deps{Store: st}}).persist(t.Context(), quiet(), res)

	if c := st.stored.Confidence; c == nil || c.Band != model.ConfidenceNone || c.Score != 0 {
		t.Errorf("confidence = %+v, want 0 none", c)
	}

	if !st.asked.before.IsZero() {
		t.Error("a failed scan read the series history it cannot use")
	}
}
