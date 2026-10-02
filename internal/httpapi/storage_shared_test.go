package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// TestStorageDashboardStatesSharedEvidenceOnce: the regression for a page
// that reported 7.0 GB for a 409 MB store. Evidence two series both name is
// shown once, in its own row, and the rows add up to the grand total rather
// than to more than the bucket holds.
func TestStorageDashboardStatesSharedEvidenceOnce(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)

	shot := []byte("the same screenshot in two series")

	ref, err := f.store.PutArtifact("screenshot-before-consent", shot)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	withShot := func(r *model.Result) {
		r.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: ref, Bytes: int64(len(shot))}}
	}

	f.seed("reject-a", model.ConsentReject, time.Now(), withShot)
	f.seed("accept-a", model.ConsentAccept, time.Now(), withShot)

	var decoded struct {
		StoredTotalBytes  int64 `json:"storedTotalBytes"`
		StoredSharedBytes int64 `json:"storedSharedBytes"`
		UnmeasuredObjects int   `json:"unmeasuredObjects"`
		Series            []struct {
			StoredDocumentBytes int64 `json:"storedDocumentBytes"`
			StoredArtifactBytes int64 `json:"storedArtifactBytes"`
			StoredSharedBytes   int64 `json:"storedSharedBytes"`
		} `json:"series"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/storage", "Authorization", storageBearer))), &decoded); err != nil {
		t.Fatalf("decoding /api/v1/storage: %v", err)
	}

	if decoded.StoredSharedBytes <= 0 {
		t.Fatalf("sharedBytes = %d, want the shared screenshot's size", decoded.StoredSharedBytes)
	}

	sum := decoded.StoredSharedBytes

	for _, row := range decoded.Series {
		sum += row.StoredDocumentBytes + row.StoredArtifactBytes

		if row.StoredSharedBytes != decoded.StoredSharedBytes {
			t.Errorf("a row's sharedBytes = %d, want %d (the one screenshot both name)", row.StoredSharedBytes, decoded.StoredSharedBytes)
		}

		if row.StoredArtifactBytes != 0 {
			t.Errorf("a row's artifactBytes = %d, want 0: its one artifact is shared, so it is in the shared row",
				row.StoredArtifactBytes)
		}
	}

	if sum != decoded.StoredTotalBytes {
		t.Errorf("rows plus shared = %d, want the grand total %d", sum, decoded.StoredTotalBytes)
	}

	if decoded.UnmeasuredObjects != 0 {
		t.Errorf("unmeasuredObjects = %d, want 0", decoded.UnmeasuredObjects)
	}

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	if !strings.Contains(html, "Shared by several series") {
		t.Errorf("expected the shared row; body:\n%s", html)
	}

	if strings.Contains(html, "no recorded size yet") {
		t.Errorf("every object is measured, so the unmeasured warning must not appear; body:\n%s", html)
	}
}
