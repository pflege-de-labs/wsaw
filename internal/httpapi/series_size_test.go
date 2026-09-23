package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 5.31: what the history page and its API say about what a scan holds
// and what a series will cost.

// seedWithEvidence stores a result naming one screenshot, and returns its
// size in bytes.
func seedWithEvidence(t *testing.T, f *fixture, scanID string, at time.Time) (screenshotBytes int64) {
	t.Helper()

	screenshot := []byte("a screenshot for " + scanID)

	ref, err := f.store.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	f.seed(scanID, model.ConsentReject, at, func(r *model.Result) {
		r.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: ref, Bytes: int64(len(screenshot))}}
	})

	return int64(len(screenshot))
}

// TestSeriesPageShowsSizeColumnAndTotal: AC3 and AC5 — a row's size figure
// and the series total are both on the page.
func TestSeriesPageShowsSizeColumnAndTotal(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)

	now := time.Now()

	seedWithEvidence(t, f, "scan-a", now.Add(-time.Hour))
	seedWithEvidence(t, f, "scan-b", now)

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, ">Size<") {
		t.Error("the history table has no Size column header")
	}

	if !strings.Contains(html, "2 scans listed") {
		t.Errorf("series total not found; body:\n%s", html)
	}

	if strings.Contains(html, "size not recorded") {
		t.Error("both scans were stored with the bytes column present, so \"size not recorded\" should not appear")
	}
}

// TestSeriesPageEstimateNamesTheKeepPolicyAndItsCeiling: AC6 and AC8 — with a
// configured interval and an active Keep policy, the estimate shows a
// ceiling and names the policy rather than reporting unbounded growth.
func TestSeriesPageEstimateNamesTheKeepPolicyAndItsCeiling(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true}, nil, nil, func(d *httpapi.Deps) {
		d.Targets = func() []config.Resolved {
			return []config.Resolved{{Name: "site", Interval: 24 * time.Hour}}
		}
		d.Retention = func() (store.Retention, error) {
			return store.Retention{Keep: &store.Keep{Daily: 7}}, nil
		}
	})

	now := time.Now()

	for i := range 3 {
		seedWithEvidence(t, f, "scan-"+string(rune('a'+i)), now.Add(time.Duration(i)*time.Hour))
	}

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, "keep") {
		t.Errorf("estimate does not name the keep policy; body:\n%s", html)
	}

	if strings.Contains(html, "<strong>unbounded</strong>") {
		t.Error("an active retention policy is configured, so the estimate must not report unbounded growth")
	}
}

// TestSeriesPageEstimateIsUnboundedWithNoRetentionPolicy: AC8's other case —
// nothing bounds the series, and the page says so plainly.
func TestSeriesPageEstimateIsUnboundedWithNoRetentionPolicy(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true}, nil, nil, func(d *httpapi.Deps) {
		d.Targets = func() []config.Resolved {
			return []config.Resolved{{Name: "site", Interval: 24 * time.Hour}}
		}
	})

	now := time.Now()

	for i := range 3 {
		seedWithEvidence(t, f, "scan-"+string(rune('a'+i)), now.Add(time.Duration(i)*time.Hour))
	}

	html := body(t, f.get("/targets/site/reject", "Accept", "text/html"))

	if !strings.Contains(html, "<strong>unbounded</strong>") {
		t.Errorf("no retention policy is configured, so the estimate must say unbounded; body:\n%s", html)
	}
}

// TestAPIResultsIncludeDocumentAndArtifactBytes: AC10 — the summaries the
// JSON API already returns gain documentBytes and artifactBytes, additively.
func TestAPIResultsIncludeDocumentAndArtifactBytes(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	shotBytes := seedWithEvidence(t, f, "scan-a", time.Now())

	var decoded struct {
		Results []struct {
			ScanID                string `json:"scanId"`
			DocumentBytes         int64  `json:"documentBytes"`
			ArtifactBytes         int64  `json:"artifactBytes"`
			ArtifactBytesRecorded bool   `json:"artifactBytesRecorded"`
		} `json:"results"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/results/site/reject"))), &decoded); err != nil {
		t.Fatalf("decoding the results listing: %v", err)
	}

	if len(decoded.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(decoded.Results))
	}

	r := decoded.Results[0]

	if r.DocumentBytes <= 0 {
		t.Errorf("documentBytes = %d, want the stored document's own size", r.DocumentBytes)
	}

	if r.ArtifactBytes != shotBytes {
		t.Errorf("artifactBytes = %d, want %d", r.ArtifactBytes, shotBytes)
	}

	if !r.ArtifactBytesRecorded {
		t.Error("artifactBytesRecorded = false, want true")
	}
}
