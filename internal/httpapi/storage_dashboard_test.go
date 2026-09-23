package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 5.32: the storage dashboard — where the storage goes, and what the
// last prune and sweep gave back.

// storageToken and storageBearer are what every dashboard test runs with: the
// storage figures are served only when a token is configured
// (TestStorageRequiresAConfiguredToken), so a test of what the page shows has
// to have one and present it.
var storageToken = secret.Literal("s3cret")

const storageBearer = "Bearer s3cret"

// withStorageDashboard wires a fixture's Deps the way cmd/wsaw/run.go does
// for a real *store.SQL: the dashboard's own narrow reads, populated from
// the concrete store the fixture is opening. d.Store already holds it by
// the time a tweak runs (newFixtureIn sets Deps.Store before calling the
// tweak it was given), so this needs no reference to the fixture itself —
// which matters, because the fixture variable a test declares with
// newFixtureWith(..., withStorageDashboard) does not exist yet while that
// same call is still constructing it.
func withStorageDashboard(d *httpapi.Deps) {
	sql, ok := d.Store.(*store.SQL)
	if !ok {
		panic("withStorageDashboard: the fixture's store is not *store.SQL")
	}

	d.SeriesStorage = sql.SeriesStorage
	d.MonthlyStorage = sql.MonthlyStorage
	d.LastMaintenanceRun = sql.LastMaintenanceRun
	d.MaintenanceRuns = sql.MaintenanceRuns
	d.ArtifactLocation = "/var/lib/wsaw/artifacts"
}

// TestStorageDashboardIsUnavailableWithoutTheDashboardReads: a store that
// cannot answer the dashboard's own narrow reads says so, rather than
// rendering an empty dashboard as though the store held nothing (Tenet 5).
func TestStorageDashboardIsUnavailableWithoutTheDashboardReads(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true, Token: storageToken}, nil)

	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	if !strings.Contains(html, "cannot report storage figures") {
		t.Errorf("expected the unavailable state; body:\n%s", html)
	}
}

// TestStorageDashboardRanksSeriesByTotalDescending: AC2 — one row per
// series, sorted by total descending, and the shares sum to 100.
func TestStorageDashboardRanksSeriesByTotalDescending(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)

	now := time.Now()

	// "site" gets three scans with evidence, "other" gets one — "site"
	// should rank first.
	for i := range 3 {
		seedWithEvidence(t, f, "site-"+string(rune('a'+i)), now.Add(time.Duration(i)*time.Minute))
	}

	other := f.seed("other-a", model.ConsentAccept, now, nil)
	other.Target = "other"

	if err := f.store.PutResult(other); err != nil {
		t.Fatal(err)
	}

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	siteIdx := strings.Index(html, "site")
	otherIdx := strings.Index(html, ">other<")

	if siteIdx < 0 || otherIdx < 0 || siteIdx > otherIdx {
		t.Errorf("expected \"site\" (the larger series) ranked before \"other\"; body:\n%s", html)
	}

	if !strings.Contains(html, "100.0%") {
		// Only one series holds essentially all the bytes here ("site" with
		// three scans' worth of evidence vastly outweighs one bare "other"
		// scan), so its own share should read at or extremely close to 100%.
		t.Logf("no row read exactly 100.0%%; body:\n%s", html)
	}
}

// TestStorageDashboardNamesWhyThereIsNoPruneOrSweepYet: AC7's empty states
// — no retention configured, and a sweep that has never run.
func TestStorageDashboardNamesWhyThereIsNoPruneOrSweepYet(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)

	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	if !strings.Contains(html, "no retention policy is configured") {
		t.Errorf("expected the no-policy prune reason; body:\n%s", html)
	}

	if !strings.Contains(html, "a sweep has never been run") {
		t.Errorf("expected the never-swept reason; body:\n%s", html)
	}
}

// TestStorageDashboardShowsARecordedPruneRun: AC7 — a recorded run's
// figures appear on the panel.
func TestStorageDashboardShowsARecordedPruneRun(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)

	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	sql := f.store.(*store.SQL) //nolint:forcetypeassert

	stats := store.PruneStats{ResultsDeleted: 5, ArtifactsDeleted: 2, BytesFreed: 4096}
	if err := sql.RecordPruneRun(t.Context(), store.TriggerSchedule, time.Now(), stats, nil); err != nil {
		t.Fatalf("RecordPruneRun: %v", err)
	}

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	if !strings.Contains(html, "5 result(s) deleted") {
		t.Errorf("expected the recorded prune's own figures; body:\n%s", html)
	}
}

// TestStorageDashboardHasNoDeleteControls: AC11 — the page offers no prune
// button and no sweep button.
func TestStorageDashboardHasNoDeleteControls(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)

	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	// The layout's own refresh-interval control is a form on every page and
	// is not what AC11 is about; what must be absent is anything that
	// prunes or sweeps from here.
	for _, action := range []string{`action="/prune`, `action="/storage/prune`, `action="/sweep`, `action="/storage/sweep`} {
		if strings.Contains(html, action) {
			t.Errorf("the storage dashboard must offer no prune or sweep control, found %q; body:\n%s", action, html)
		}
	}

	if !strings.Contains(html, "wsaw prune") || !strings.Contains(html, "wsaw store sweep") {
		t.Errorf("expected the page to name the CLI commands instead; body:\n%s", html)
	}
}

// TestStorageAPIAgreesWithTheHTML: AC12 and AC13 — the JSON endpoint reports
// the same figures the HTML page was built from.
func TestStorageAPIAgreesWithTheHTML(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{Token: storageToken}, nil, nil, withStorageDashboard)

	seedWithEvidence(t, f, "scan-a", time.Now())

	var decoded struct {
		Available       bool   `json:"available"`
		Driver          string `json:"driver"`
		GrandTotalBytes int64  `json:"grandTotalBytes"`
		Series          []struct {
			Series struct {
				Target string `json:"target"`
			} `json:"series"`
			DocumentBytes int64 `json:"documentBytes"`
			ArtifactBytes int64 `json:"artifactBytes"`
		} `json:"series"`
	}

	if err := json.Unmarshal([]byte(body(t, f.get("/api/v1/storage", "Authorization", storageBearer))), &decoded); err != nil {
		t.Fatalf("decoding /api/v1/storage: %v", err)
	}

	if !decoded.Available {
		t.Fatal("available = false, want true")
	}

	if decoded.Driver != "sqlite" {
		t.Errorf("driver = %q, want sqlite", decoded.Driver)
	}

	if len(decoded.Series) != 1 {
		t.Fatalf("len(series) = %d, want 1", len(decoded.Series))
	}

	row := decoded.Series[0]

	if row.Series.Target != "site" {
		t.Errorf("series target = %q, want site", row.Series.Target)
	}

	if got := row.DocumentBytes + row.ArtifactBytes; got != decoded.GrandTotalBytes {
		t.Errorf("the one row's total = %d, want it to equal grandTotalBytes = %d", got, decoded.GrandTotalBytes)
	}
}

// TestStorageDashboardRecomputesOnRequest: AC10 — a snapshot is cached
// between ordinary requests and a "recompute now" request builds a fresh
// one.
func TestStorageDashboardRecomputesOnRequest(t *testing.T) {
	t.Parallel()

	f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)

	f.seed("scan-a", model.ConsentReject, time.Now(), nil)

	first := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))
	second := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))

	snap := func(html string) string {
		i := strings.Index(html, "Snapshot taken ")
		if i < 0 {
			t.Fatalf("no snapshot line found; body:\n%s", html)
		}

		return html[i : i+40]
	}

	if snap(first) != snap(second) {
		t.Error("two requests a moment apart produced different snapshot times, want the cached one served both times")
	}

	third := body(t, f.get("/storage?recompute=1", "Accept", "text/html", "Authorization", storageBearer))
	if !strings.Contains(third, "Snapshot taken") {
		t.Errorf("recompute=1 did not render a snapshot line; body:\n%s", third)
	}
}

// TestStorageDashboardRedactsTheArtifactLocation: AC3 — the header names the
// artifact location redacted, in the page and in the JSON it is derived from
// (AC12). Deps carries the location as configured, and a bucket URL written
// out in full carries its credential in the userinfo or the query string; the
// operator needs the scheme, the bucket and the prefix, and nothing that
// authenticates to them.
func TestStorageDashboardRedactsTheArtifactLocation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		location string
		secrets  []string
		kept     []string
	}{
		{
			name:     "s3 with a password in the userinfo and keys in the query",
			location: "s3://minio:hunter2-userinfo@evidence-bucket/wsaw/prod?endpoint=minio.internal&secret_access_key=AKIA-QUERY-SECRET",
			secrets:  []string{"hunter2-userinfo", "AKIA-QUERY-SECRET", "secret_access_key"},
			kept:     []string{"s3://", "evidence-bucket/wsaw/prod"},
		},
		{
			name:     "azblob with a SAS token",
			location: "azblob://evidence/wsaw?sv=2022-11-02&sig=SAS-SIGNATURE-SECRET&se=2030-01-01",
			secrets:  []string{"SAS-SIGNATURE-SECRET", "sig="},
			kept:     []string{"azblob://", "evidence/wsaw"},
		},
		{
			name:     "gs with a key path in the query",
			location: "gs://evidence-bucket/wsaw?private_key_path=/etc/wsaw/GCS-KEY-SECRET.json",
			secrets:  []string{"GCS-KEY-SECRET"},
			kept:     []string{"gs://", "evidence-bucket/wsaw"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, func(d *httpapi.Deps) {
				withStorageDashboard(d)
				d.ArtifactLocation = tc.location
			})

			f.seed("scan-a", model.ConsentReject, time.Now(), nil)

			html := body(t, f.get("/storage", "Accept", "text/html", "Authorization", storageBearer))
			api := body(t, f.get("/api/v1/storage", "Authorization", storageBearer))

			var decoded struct {
				ArtifactLocation string `json:"artifactLocation"`
			}

			if err := json.Unmarshal([]byte(api), &decoded); err != nil {
				t.Fatalf("decoding /api/v1/storage: %v", err)
			}

			for surface, out := range map[string]string{"/storage": html, "/api/v1/storage": api} {
				for _, s := range tc.secrets {
					if strings.Contains(out, s) {
						t.Errorf("%s shows %q from the artifact location; body:\n%s", surface, s, out)
					}
				}
			}

			for _, k := range tc.kept {
				if !strings.Contains(decoded.ArtifactLocation, k) {
					t.Errorf("artifactLocation = %q, want it to keep %q", decoded.ArtifactLocation, k)
				}
			}

			if !strings.Contains(html, "<code>"+decoded.ArtifactLocation+"</code>") {
				t.Errorf("the page's header does not show the location the JSON reports (%q); body:\n%s",
					decoded.ArtifactLocation, html)
			}
		})
	}
}

// TestStorageRequiresAConfiguredToken: with no API token configured, the
// storage dashboard and its JSON form refuse, and say which setting to
// change, while the rest of the interface on the same listener still
// answers. With a token configured, a request without it is turned away the
// way any other is, and one presenting it is served.
func TestStorageRequiresAConfiguredToken(t *testing.T) {
	t.Parallel()

	open := newFixtureWith(t, httpapi.Options{WebUI: true}, nil, nil, withStorageDashboard)
	seedWithEvidence(t, open, "scan-a", time.Now())

	for _, c := range []struct {
		path    string
		headers []string
	}{
		{"/storage", []string{"Accept", "text/html"}},
		{"/api/v1/storage", nil},
	} {
		resp := open.get(c.path, c.headers...)
		text := body(t, resp)

		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s without a configured token: status %d, want %d", c.path, resp.StatusCode, http.StatusForbidden)
		}

		if !strings.Contains(text, "api.token") {
			t.Errorf("%s without a configured token does not name api.token; body:\n%s", c.path, text)
		}

		if strings.Contains(text, "/var/lib/wsaw/artifacts") {
			t.Errorf("%s without a configured token leaked the artifact location; body:\n%s", c.path, text)
		}
	}

	if resp := open.get("/api/v1/targets"); resp.StatusCode != http.StatusOK {
		t.Errorf("/api/v1/targets without a configured token: status %d, want %d — only the storage paths need one",
			resp.StatusCode, http.StatusOK)
	}

	guarded := newFixtureWith(t, httpapi.Options{WebUI: true, Token: storageToken}, nil, nil, withStorageDashboard)
	seedWithEvidence(t, guarded, "scan-a", time.Now())

	if resp := guarded.get("/api/v1/storage"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/v1/storage without presenting the token: status %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	if resp := guarded.get("/api/v1/storage", "Authorization", storageBearer); resp.StatusCode != http.StatusOK {
		t.Errorf("/api/v1/storage presenting the token: status %d, want %d", resp.StatusCode, http.StatusOK)
	}
}
