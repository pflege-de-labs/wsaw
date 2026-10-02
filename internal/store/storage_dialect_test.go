package store_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// TestStorageAddsUpOnEveryDialect runs the storage dashboard's reads against
// whichever database the suite is pointed at (WSAW_TEST_STORE_DRIVER): the
// rows and the shared row add up to the total, the months add up to it too,
// and a sweep — which records every listed object's size again — leaves the
// total where the write path put it.
func TestStorageAddsUpOnEveryDialect(t *testing.T) {
	t.Parallel()

	s := openSQL(t)

	shared := bytes.Repeat([]byte("a script both series load "), 512)

	sharedRef, err := s.PutArtifact("body", shared)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	for i, mode := range []model.ConsentMode{model.ConsentReject, model.ConsentAccept} {
		own, err := s.PutArtifact("screenshot-before-consent", []byte("screenshot of "+string(mode)))
		if err != nil {
			t.Fatalf("PutArtifact: %v", err)
		}

		res := &model.Result{
			SchemaVersion: model.SchemaVersion,
			ScanID:        "scan-" + string(mode),
			Target:        "site",
			URL:           "https://example.com/",
			ConsentMode:   mode,
			StartedAt:     time.Now().Add(-time.Duration(i) * time.Hour),
			FinishedAt:    time.Now(),
			Termination:   model.TermIdle,
			Consent:       model.Consent{Outcome: model.OutcomeApplied},
			Screenshots:   []model.Artifact{{Kind: "screenshot-before-consent", Ref: own}},
			Requests: []model.Request{{
				URL: "https://cdn.test/lib.js", Domain: "cdn.test",
				Party: model.ThirdParty, Phase: model.PhasePost, BodyRef: sharedRef,
			}},
		}

		if err := s.PutResult(res); err != nil {
			t.Fatalf("PutResult: %v", err)
		}
	}

	check := func(when string) int64 {
		t.Helper()

		report, err := s.Storage(t.Context())
		if err != nil {
			t.Fatalf("%s: Storage: %v", when, err)
		}

		if len(report.Series) != 2 {
			t.Fatalf("%s: %d series, want 2", when, len(report.Series))
		}

		if report.Unmeasured != 0 {
			t.Errorf("%s: Unmeasured = %d, want 0", when, report.Unmeasured)
		}

		if report.StoredSharedBytes <= 0 {
			t.Errorf("%s: SharedBytes = %d, want the shared body's stored size", when, report.StoredSharedBytes)
		}

		sum := report.StoredSharedBytes
		for _, sd := range report.Series {
			sum += sd.StoredTotalBytes()

			if sd.StoredDocumentBytes <= 0 || sd.StoredArtifactBytes <= 0 {
				t.Errorf("%s: %s/%s document %d, artifacts %d, want both > 0",
					when, sd.Series.Target, sd.Series.Mode, sd.StoredDocumentBytes, sd.StoredArtifactBytes)
			}
		}

		if sum != report.StoredTotalBytes {
			t.Errorf("%s: rows plus shared = %d, want the total %d", when, sum, report.StoredTotalBytes)
		}

		months, err := s.MonthlyStorage(t.Context(), time.Now().AddDate(0, -1, 0))
		if err != nil {
			t.Fatalf("%s: MonthlyStorage: %v", when, err)
		}

		var monthly int64
		for _, m := range months {
			monthly += m.Bytes
		}

		if monthly != report.StoredTotalBytes {
			t.Errorf("%s: the months add up to %d, want the total %d", when, monthly, report.StoredTotalBytes)
		}

		return report.StoredTotalBytes
	}

	written := check("as written")

	if _, err := s.Sweep(t.Context(), store.TriggerCLI, time.Now(), store.SweepOptions{}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if swept := check("after a sweep"); swept != written {
		t.Errorf("the total moved from %d to %d across a sweep; the listing and the write path disagree on sizes",
			written, swept)
	}
}
