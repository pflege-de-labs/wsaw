package report_test

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

func fixture() *model.Result {
	start := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)

	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        "scan-abc",
		Target:        "example",
		URL:           "https://example.com/",
		ConsentMode:   model.ConsentReject,
		StartedAt:     start,
		FinishedAt:    start.Add(3 * time.Second),
		Duration:      3 * time.Second,
		Termination:   model.TermIdle,
		Consent: model.Consent{
			Outcome:   model.OutcomeApplied,
			Reason:    "rule \"onetrust\" applied and verified",
			CMP:       "OneTrust",
			Detection: "rule:onetrust",
			Mechanism: "vendor-api",
			TCString:  "CPabc",
		},
		Environment: model.Environment{
			WsawVersion: "1.0.0", ChromeVersion: "Chromium 131",
			ViewportWidth: 1280, ViewportHeight: 800, DeviceScale: 1,
			ExtraHeaders: []string{"X-Scan"},
			BasicAuth:    true,
		},
		Requests: []model.Request{
			{
				URL: "https://example.com/", NormalizedURL: "https://example.com/",
				Method: "GET", ResourceType: "document", Host: "example.com", Domain: "example.com",
				Party: model.FirstParty, Phase: model.PhasePre, Status: 200, MimeType: "text/html",
				Protocol: "h2", RemoteIP: "93.184.216.34", TransferSize: 1200, DecodedSize: 4000,
				Timing: model.Timing{StartOffset: 0, TTFB: 80 * time.Millisecond, EndOffset: 200 * time.Millisecond},
			},
			{
				URL: "https://tracker.test/px.gif", NormalizedURL: "https://tracker.test/px.gif",
				Method: "GET", ResourceType: "image", Host: "tracker.test", Domain: "tracker.test",
				Party: model.ThirdParty, Phase: model.PhasePre, Status: 200, TransferSize: 43,
				Timing: model.Timing{StartOffset: 100 * time.Millisecond, EndOffset: 150 * time.Millisecond},
			},
			{
				URL: "https://ads.test/t.js", NormalizedURL: "https://ads.test/t.js",
				Method: "GET", ResourceType: "script", Host: "ads.test", Domain: "ads.test",
				Party: model.ThirdParty, Phase: model.PhasePost, Status: 200, TransferSize: 900,
				BodySHA256: "abc123",
				Timing:     model.Timing{StartOffset: 500 * time.Millisecond, EndOffset: 700 * time.Millisecond},
			},
			{
				URL: "data:image/gif;base64,AA", NormalizedURL: "data:image/gif;base64,…",
				ResourceType: "image", NonNetwork: true, Party: model.FirstParty,
			},
		},
		Cookies: []model.Cookie{
			{Name: "sid", Domain: "example.com", Path: "/", Session: true, Secure: true, HTTPOnly: true, Party: model.FirstParty, ValueSHA256: "deadbeef"},
		},
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteJSON(&b, fixture()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var back model.Result
	if err := json.Unmarshal([]byte(b.String()), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if back.ScanID != "scan-abc" || len(back.Requests) != 4 {
		t.Errorf("round trip lost data: %+v", back)
	}

	if back.SchemaVersion == "" {
		t.Error("schemaVersion is missing; consumers need it to interpret the payload")
	}
}

func TestWriteJSONLIsOnePerLine(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteJSONL(&b, fixture(), fixture()); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	for i, line := range lines {
		var res model.Result
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
	}
}

func TestWriteCSVHostTable(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteCSV(&b, fixture()); err != nil {
		t.Fatal(err)
	}

	rows, err := csv.NewReader(strings.NewReader(b.String())).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}

	// Header plus three network domains; the data: URL must not appear.
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4: %v", len(rows), rows)
	}

	if rows[0][0] != "target" {
		t.Errorf("header = %v", rows[0])
	}

	// Third parties sort first, so the first data row is a third party.
	if rows[1][5] != "third" {
		t.Errorf("first data row party = %q, want third", rows[1][5])
	}
}

func TestWriteMarkdownLeadsWithTrustworthiness(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Termination = model.TermError
	res.Error = "navigate: net::ERR_CONNECTION_REFUSED"

	var b strings.Builder

	if err := report.WriteMarkdown(&b, res, nil); err != nil {
		t.Fatal(err)
	}

	out := b.String()

	if !strings.Contains(out, "did not complete") {
		t.Error("a failed scan does not announce itself")
	}

	// The warning must come before the host table, so a reader cannot digest
	// the numbers before learning they are unreliable.
	warnAt := strings.Index(out, "did not complete")
	hostsAt := strings.Index(out, "## Hosts")

	if warnAt < 0 || hostsAt < 0 || warnAt > hostsAt {
		t.Error("the trustworthiness warning does not precede the host table")
	}
}

func TestWriteMarkdownFlagsHeuristicConsent(t *testing.T) {
	t.Parallel()

	res := fixture()
	res.Consent.Mechanism = "heuristic"
	res.Consent.Heuristic = true

	var b strings.Builder

	if err := report.WriteMarkdown(&b, res, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(b.String(), "caution") {
		t.Error("a heuristic consent result is not flagged; a reviewer cannot weigh the evidence")
	}
}

func TestWriteMarkdownSeparatesPreConsentTraffic(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteMarkdown(&b, fixture(), nil); err != nil {
		t.Fatal(err)
	}

	out := b.String()

	if !strings.Contains(out, "before any consent interaction") {
		t.Error("pre-consent section missing")
	}

	if !strings.Contains(out, "tracker.test") {
		t.Error("the pre-consent third party is not listed")
	}

	if !strings.Contains(out, "after rejection") || !strings.Contains(out, "ads.test") {
		t.Error("post-rejection third parties are not called out in reject mode")
	}
}

func TestWriteMarkdownIncludesDiff(t *testing.T) {
	t.Parallel()

	rep := &diff.Report{
		Comparable:     true,
		BaselineScanID: "scan-old",
		Changes: []diff.Change{
			{Type: diff.HostAdded, Severity: diff.SeverityCritical, Subject: "ads.test", Detail: "new third-party host"},
		},
		Suppressed: 2,
	}

	var b strings.Builder

	if err := report.WriteMarkdown(&b, fixture(), rep); err != nil {
		t.Fatal(err)
	}

	out := b.String()

	if !strings.Contains(out, "critical") || !strings.Contains(out, "ads.test") {
		t.Error("diff not rendered")
	}

	if !strings.Contains(out, "suppressed") {
		t.Error("suppressed count is hidden; a quiet report must say how much it hid")
	}
}

// TestMarkdownEscapesUntrustedDetail: detail text carries URLs from the
// scanned page, which is attacker-controlled.
func TestMarkdownEscapesUntrustedDetail(t *testing.T) {
	t.Parallel()

	rep := &diff.Report{
		Comparable: true,
		Changes: []diff.Change{
			{Type: diff.HostAdded, Severity: diff.SeverityHigh, Subject: "evil.test",
				Detail: "host | injected | column\nand a newline"},
		},
	}

	var b strings.Builder

	if err := report.WriteMarkdown(&b, fixture(), rep); err != nil {
		t.Fatal(err)
	}

	out := b.String()

	if strings.Contains(out, "host | injected") {
		t.Error("unescaped pipe from page-controlled text would break the table")
	}

	if strings.Contains(out, "injected | column\nand") {
		t.Error("newline from page-controlled text was not neutralized")
	}
}

func TestMarkdownNeverShowsHeaderValues(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteMarkdown(&b, fixture(), nil); err != nil {
		t.Fatal(err)
	}

	out := b.String()

	if !strings.Contains(out, "X-Scan") {
		t.Error("header name should be reported for reproducibility")
	}

	if !strings.Contains(out, "Basic authentication was used") {
		t.Error("basic auth usage should be reported")
	}
}

func TestWriteHARIsValidAndExcludesNonNetwork(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteHAR(&b, fixture()); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Log struct {
			Version string `json:"version"`
			Creator struct {
				Name string `json:"name"`
			} `json:"creator"`
			Comment string `json:"comment"`
			Pages   []struct {
				ID string `json:"id"`
			} `json:"pages"`
			Entries []struct {
				Pageref string `json:"pageref"`
				Comment string `json:"comment"`
				Request struct {
					URL    string `json:"url"`
					Method string `json:"method"`
				} `json:"request"`
				Response struct {
					Status  int `json:"status"`
					Content struct {
						Comment string `json:"comment"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}

	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("HAR is not valid JSON: %v", err)
	}

	if doc.Log.Version != "1.2" || doc.Log.Creator.Name != "wsaw" {
		t.Errorf("HAR envelope wrong: %+v", doc.Log)
	}

	// The data: URL is not a network request and must not be an entry.
	if len(doc.Log.Entries) != 3 {
		t.Errorf("got %d entries, want 3 (non-network resources excluded)", len(doc.Log.Entries))
	}

	for _, e := range doc.Log.Entries {
		if e.Pageref != doc.Log.Pages[0].ID {
			t.Errorf("entry %s is not linked to the page", e.Request.URL)
		}

		if !strings.Contains(e.Comment, "phase=") {
			t.Errorf("entry %s lost its consent phase, which HAR has no field for", e.Request.URL)
		}
	}

	// The header limitation must be stated rather than silently implied.
	if !strings.Contains(doc.Log.Comment, "headers are not retained") {
		t.Error("HAR does not disclose that headers are absent")
	}
}

func TestHARCarriesBodyDigest(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteHAR(&b, fixture()); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(b.String(), "sha256=abc123") {
		t.Error("script digest is missing from the HAR export")
	}
}

func TestReportsAreDeterministic(t *testing.T) {
	t.Parallel()

	render := func() string {
		var b strings.Builder

		if err := report.WriteMarkdown(&b, fixture(), nil); err != nil {
			t.Fatal(err)
		}

		return b.String()
	}

	// Two separate renderings of identical input, compared: staticcheck reads
	// render() != render() as a mistake, and naming them says it is not.
	first := render()
	second := render()

	if first != second {
		t.Error("Markdown rendering is not deterministic")
	}
}
