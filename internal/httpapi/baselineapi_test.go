package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/daemon"
	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// The read and delete halves of the baseline endpoint, and the schedule
// endpoint. Every other test reached these paths only unauthenticated or with
// a share token, where authentication answers before the handler does — so
// the handlers themselves went unexercised and nothing asserted what they
// return.

// approveViaAPI puts a baseline in place through the API, which is how an operator
// gets one, rather than by writing to the store behind the server's back.
func approveViaAPI(t *testing.T, f *fixture, payload string) {
	t.Helper()

	resp := f.do(http.MethodPost, "/api/v1/baseline/site/reject", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approving = %d: %s", resp.StatusCode, body(t, resp))
	}

	_ = resp.Body.Close()
}

func TestGetBaselineReturnsTheApprovedScan(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)
	approveViaAPI(t, f, `{"scanId":"scan-1","actor":"martin","note":"reviewed"}`)

	resp := f.get("/api/v1/baseline/site/reject")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body(t, resp))
	}

	var got store.Baseline
	if err := json.Unmarshal([]byte(body(t, resp)), &got); err != nil {
		t.Fatal(err)
	}

	if got.ScanID != "scan-1" {
		t.Errorf("scanId = %q, want scan-1", got.ScanID)
	}

	if got.Target != "site" || got.ConsentMode != model.ConsentReject {
		t.Errorf("baseline identifies %s/%s, want site/reject", got.Target, got.ConsentMode)
	}

	if got.ApprovedBy != "martin" || got.Note != "reviewed" {
		t.Errorf("approver and note = %q/%q, want martin/reviewed", got.ApprovedBy, got.Note)
	}

	// The approved result travels with the baseline. A baseline that only
	// named a scan would stop meaning anything once retention pruned it.
	if got.Result == nil || got.Result.ScanID != "scan-1" {
		t.Error("the baseline does not carry a copy of the approved result")
	}
}

func TestGetBaselineIsNotFoundBeforeAnyApproval(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)

	resp := f.get("/api/v1/baseline/site/reject")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", resp.StatusCode, body(t, resp))
	}
}

// A malformed consent mode must be refused before it reaches the store, on
// read and on delete alike.
func TestTheBaselineEndpointRejectsAnUnknownConsentMode(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		resp := f.do(method, "/api/v1/baseline/site/sideways", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s with an unknown mode = %d, want 400", method, resp.StatusCode)
		}

		if got := body(t, resp); !strings.Contains(got, "consent mode must be one of") {
			t.Errorf("%s error body = %s", method, got)
		}
	}
}

func TestDeletingABaselineRemovesItAndIsAudited(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), nil)
	approveViaAPI(t, f, `{"scanId":"scan-1"}`)

	resp := f.do(http.MethodDelete, "/api/v1/baseline/site/reject?actor=martin", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d: %s", resp.StatusCode, body(t, resp))
	}

	if got := body(t, resp); !strings.Contains(got, `"deleted": true`) {
		t.Errorf("delete body = %s", got)
	}

	// Gone, not merely reported gone.
	if resp := f.get("/api/v1/baseline/site/reject"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("baseline still readable after deletion: %d", resp.StatusCode)
	}

	// Withdrawing an approval changes approved state, so it is as auditable
	// as granting one.
	audit := body(t, f.get("/api/v1/audit"))
	if !strings.Contains(audit, "baseline-deleted") || !strings.Contains(audit, "martin") {
		t.Errorf("deletion was not audited against its actor: %s", audit)
	}
}

func TestDeletingABaselineIsRefusedInReadOnlyMode(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{ReadOnly: true}, nil)

	resp := f.do(http.MethodDelete, "/api/v1/baseline/site/reject", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete in read-only mode = %d, want 403", resp.StatusCode)
	}

	if got := body(t, resp); !strings.Contains(got, "read-only") {
		t.Errorf("error body = %s", got)
	}
}

// stubScanner satisfies the daemon's scanning dependency without a browser.
// The schedule is built when the daemon is constructed, so nothing here ever
// has to run.
type stubScanner struct{}

func (stubScanner) Scan(_ context.Context, _ config.Resolved, _ model.ConsentMode) (scanner.Outcome, error) {
	return scanner.Outcome{}, nil
}

// The schedule endpoint distinguishes a deployment with no scheduler from one
// whose scheduler has nothing to do. Reporting an empty job list for both
// would be the "empty means clean" mistake Tenet 5 forbids, so both are
// pinned.
func TestScheduleReportsAnEmptyListWithoutADaemon(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{}, nil)

	resp := f.get("/api/v1/schedule")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var got struct {
		Jobs []daemon.JobStatus `json:"jobs"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Jobs) != 0 {
		t.Errorf("jobs = %d, want none without a daemon", len(got.Jobs))
	}
}

func TestScheduleReportsTheDaemonsJobs(t *testing.T) {
	t.Parallel()

	targets := []config.Resolved{{
		Name:         "site",
		URL:          "https://example.com/",
		ConsentModes: []model.ConsentMode{model.ConsentReject, model.ConsentAccept},
		Interval:     time.Hour,
	}}

	d, err := daemon.New(stubScanner{}, nil, targets, daemon.Options{})
	if err != nil {
		t.Fatal(err)
	}

	f := newFixtureWith(t, httpapi.Options{}, nil, nil, func(deps *httpapi.Deps) {
		deps.Daemon = d
	})

	resp := f.get("/api/v1/schedule")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var got struct {
		Jobs []daemon.JobStatus `json:"jobs"`
	}

	if err := json.Unmarshal([]byte(body(t, resp)), &got); err != nil {
		t.Fatal(err)
	}

	// One job per target and consent mode, each with a next run: a schedule
	// that reported no next run would say nothing about whether wsaw is
	// still going to look.
	if len(got.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 (one per consent mode): %+v", len(got.Jobs), got.Jobs)
	}

	modes := map[model.ConsentMode]bool{}

	for _, j := range got.Jobs {
		if j.Target != "site" {
			t.Errorf("job target = %q, want site", j.Target)
		}

		if j.NextRun.IsZero() {
			t.Errorf("job %s/%s has no next run", j.Target, j.Mode)
		}

		modes[j.Mode] = true
	}

	if !modes[model.ConsentReject] || !modes[model.ConsentAccept] {
		t.Errorf("scheduled modes = %v, want reject and accept", modes)
	}
}
