package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/mcp"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// pngHeader is enough of a PNG for content sniffing to call it one.
var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

var epoch = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

func request(u, host, domain string, party model.Party, phase model.ConsentPhase) model.Request {
	return model.Request{
		URL: u, NormalizedURL: u, Method: "GET", ResourceType: "script",
		Host: host, Domain: domain, Party: party, Phase: phase, Status: 200,
	}
}

func result(id string, at time.Time, reqs ...model.Request) *model.Result {
	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        id,
		Target:        "site",
		URL:           "https://site.test/",
		ConsentMode:   model.ConsentReject,
		StartedAt:     at,
		FinishedAt:    at.Add(time.Second),
		Duration:      time.Second,
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied},
		Requests:      reqs,
	}
}

type fixture struct {
	st         store.Store
	bodyRef    string
	shotRef    string
	bigBodyLen int
}

// seed builds a real SQLite store in a temp directory: three scans of
// site/reject, the first approved as the baseline, the last one failed; and
// one scan of other/accept. No browser and no network are involved.
func seed(t *testing.T) fixture {
	t.Helper()

	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	body := strings.Repeat("console.log('é');\n", 10000)

	bodyRef, err := st.PutArtifact("body", []byte(body))
	if err != nil {
		t.Fatalf("storing a body: %v", err)
	}

	shotRef, err := st.PutArtifact("screenshot-before-consent", pngHeader)
	if err != nil {
		t.Fatalf("storing a screenshot: %v", err)
	}

	first := request("https://site.test/app.js", "site.test", "site.test", model.FirstParty, model.PhasePre)
	first.BodyRef = bodyRef
	tracker := request("https://tracker.test/t.js", "tracker.test", "tracker.test", model.ThirdParty, model.PhasePre)
	late := request("https://ads.test/a.js", "ads.test", "ads.test", model.ThirdParty, model.PhasePost)

	one := result("scan-1", epoch, first)
	one.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: shotRef}}
	two := result("scan-2", epoch.Add(time.Hour), first, tracker, late)
	three := result("scan-3", epoch.Add(2*time.Hour))
	three.Termination = model.TermError
	three.Error = "navigation failed"

	other := result("other-1", epoch, first)
	other.Target, other.ConsentMode = "other", model.ConsentAccept

	for _, r := range []*model.Result{one, two, three, other} {
		if err := st.PutResult(r); err != nil {
			t.Fatalf("storing %s: %v", r.ScanID, err)
		}
	}

	if _, err := st.SetBaseline("site", model.ConsentReject, "scan-1", "tester", ""); err != nil {
		t.Fatalf("approving the baseline: %v", err)
	}

	return fixture{st: st, bodyRef: bodyRef, shotRef: shotRef, bigBodyLen: len(body)}
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// exchange runs a whole session — every line of input, then end of file — and
// returns the responses in order.
func exchange(t *testing.T, st mcp.Store, lines ...string) []rpcResponse {
	t.Helper()

	var out bytes.Buffer

	srv := mcp.New(st, "test", slog.New(slog.DiscardHandler))
	if err := srv.Serve(t.Context(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resps []rpcResponse

	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 16<<20)

	for sc.Scan() {
		var r rpcResponse
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("response %q is not JSON: %v", sc.Text(), err)
		}

		resps = append(resps, r)
	}

	return resps
}

type toolResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func call(t *testing.T, st mcp.Store, name string, args any) toolResult {
	t.Helper()

	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}

	resps := exchange(t, st, string(req))
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("tools/call %s: want one result, got %+v", name, resps)
	}

	var tr toolResult
	if err := json.Unmarshal(resps[0].Result, &tr); err != nil {
		t.Fatalf("decoding the tool result: %v", err)
	}

	return tr
}

// decode reads the JSON in a tool result's first block.
func decode(t *testing.T, tr toolResult, v any) {
	t.Helper()

	if tr.IsError {
		t.Fatalf("tool failed: %s", tr.Content[0].Text)
	}

	if err := json.Unmarshal([]byte(tr.Content[0].Text), v); err != nil {
		t.Fatalf("decoding %q: %v", tr.Content[0].Text, err)
	}
}

func TestInitializeNegotiatesTheVersion(t *testing.T) {
	t.Parallel()

	f := seed(t)

	cases := []struct{ asked, want string }{
		{asked: "2025-06-18", want: "2025-06-18"},
		{asked: "2024-11-05", want: "2024-11-05"},
		{asked: "1999-01-01", want: "2025-11-25"},
		{asked: "", want: "2025-11-25"},
	}

	for _, tc := range cases {
		resps := exchange(t, f.st,
			`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"`+tc.asked+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

		var got struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct{ Name string }
			Instructions    string
		}
		if err := json.Unmarshal(resps[0].Result, &got); err != nil {
			t.Fatal(err)
		}

		if got.ProtocolVersion != tc.want {
			t.Errorf("asked %q: got version %q, want %q", tc.asked, got.ProtocolVersion, tc.want)
		}

		if _, ok := got.Capabilities["tools"]; !ok || got.ServerInfo.Name != "wsaw" {
			t.Errorf("initialize result = %+v", got)
		}

		if !strings.Contains(got.Instructions, "untrusted") {
			t.Errorf("instructions do not warn about page content: %q", got.Instructions)
		}

		if string(resps[0].ID) != "7" {
			t.Errorf("id = %s, want 7", resps[0].ID)
		}
	}
}

func TestProtocolErrorsAndNotifications(t *testing.T) {
	t.Parallel()

	f := seed(t)

	resps := exchange(t, f.st,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		``,
		`{"jsonrpc":"2.0","id":"a","method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{not json`,
		`[{"jsonrpc":"2.0","id":3,"method":"ping"}]`,
		`{"id":4,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_everything"}}`,
	)

	wantCodes := []int{0, -32601, -32700, -32600, -32600, -32602}
	if len(resps) != len(wantCodes) {
		t.Fatalf("got %d responses, want %d (a notification or blank line was answered?): %+v",
			len(resps), len(wantCodes), resps)
	}

	if string(resps[0].ID) != `"a"` || string(resps[0].Result) != "{}" {
		t.Errorf("ping = %+v", resps[0])
	}

	for i, want := range wantCodes[1:] {
		r := resps[i+1]
		if r.Error == nil || r.Error.Code != want {
			t.Errorf("response %d: want error %d, got %+v", i+1, want, r)
		}
	}

	if string(resps[2].ID) != "null" {
		t.Errorf("a parse error must carry a null id, got %s", resps[2].ID)
	}
}

func TestToolsAreAllReadOnly(t *testing.T) {
	t.Parallel()

	f := seed(t)
	resps := exchange(t, f.st, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var got struct {
		Tools []struct {
			Name        string
			InputSchema map[string]any
			Annotations struct{ ReadOnlyHint, OpenWorldHint bool }
		}
	}
	if err := json.Unmarshal(resps[0].Result, &got); err != nil {
		t.Fatal(err)
	}

	want := []string{"list_series", "list_scans", "get_scan", "list_requests", "diff_scans", "get_artifact"}
	if len(got.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(got.Tools), len(want))
	}

	for i, tl := range got.Tools {
		if tl.Name != want[i] {
			t.Errorf("tool %d = %s, want %s", i, tl.Name, want[i])
		}

		if !tl.Annotations.ReadOnlyHint || tl.Annotations.OpenWorldHint {
			t.Errorf("%s is not annotated read-only and closed-world", tl.Name)
		}

		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s input schema is not an object", tl.Name)
		}
	}
}

func TestListSeries(t *testing.T) {
	t.Parallel()

	f := seed(t)

	var got struct {
		Series []struct {
			Target      string
			ConsentMode string
			HasBaseline bool
		}
	}
	decode(t, call(t, f.st, "list_series", map[string]any{}), &got)

	if len(got.Series) != 2 {
		t.Fatalf("series = %+v", got.Series)
	}

	if got.Series[0].Target != "other" || got.Series[0].HasBaseline {
		t.Errorf("first series = %+v", got.Series[0])
	}

	if got.Series[1].Target != "site" || !got.Series[1].HasBaseline {
		t.Errorf("second series = %+v", got.Series[1])
	}

	decode(t, call(t, f.st, "list_series", map[string]any{"filter": "sit"}), &got)

	if len(got.Series) != 1 || got.Series[0].Target != "site" {
		t.Errorf("filtered series = %+v", got.Series)
	}
}

func TestListScansIsNewestFirstAndBounded(t *testing.T) {
	t.Parallel()

	f := seed(t)

	var got struct {
		Scans []struct{ ScanID string }
		Limit int
	}
	decode(t, call(t, f.st, "list_scans", map[string]any{"target": "site", "consentMode": "reject", "limit": 2}), &got)

	if len(got.Scans) != 2 || got.Scans[0].ScanID != "scan-3" || got.Scans[1].ScanID != "scan-2" || got.Limit != 2 {
		t.Errorf("scans = %+v", got)
	}
}

func TestGetScan(t *testing.T) {
	t.Parallel()

	f := seed(t)

	type view struct {
		OK                bool
		Warning           string
		RequestCount      int
		ThirdPartyDomains struct{ All, PreInteraction, PostInteraction []string }
		Result            map[string]json.RawMessage
	}

	var got view
	decode(t, call(t, f.st, "get_scan", map[string]any{"target": "site", "consentMode": "reject", "scanId": "scan-2"}), &got)

	if !got.OK || got.Warning != "" || got.RequestCount != 3 {
		t.Errorf("scan-2 = %+v", got)
	}

	if _, ok := got.Result["requests"]; ok {
		t.Error("get_scan returned the request list; it must leave it to list_requests")
	}

	if strings.Join(got.ThirdPartyDomains.PreInteraction, ",") != "tracker.test" ||
		strings.Join(got.ThirdPartyDomains.PostInteraction, ",") != "ads.test" {
		t.Errorf("third-party domains = %+v", got.ThirdPartyDomains)
	}

	// latest is the failed scan, and it must say so rather than look empty.
	got = view{}
	decode(t, call(t, f.st, "get_scan", map[string]any{"target": "site", "consentMode": "reject"}), &got)

	if got.OK || !strings.Contains(got.Warning, "incomplete") || got.RequestCount != 0 {
		t.Errorf("failed latest scan = %+v", got)
	}

	got = view{}
	decode(t, call(t, f.st, "get_scan", map[string]any{"target": "site", "consentMode": "reject", "scanId": "baseline"}), &got)

	if string(got.Result["scanId"]) != `"scan-1"` {
		t.Errorf("baseline scan = %s", got.Result["scanId"])
	}
}

func TestToolErrorsNameWhatIsWrong(t *testing.T) {
	t.Parallel()

	f := seed(t)

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"missing scan", "get_scan", map[string]any{"target": "site", "consentMode": "reject", "scanId": "nope"}, "pruned"},
		{"no baseline", "get_scan", map[string]any{"target": "other", "consentMode": "accept", "scanId": "baseline"}, `"baseline"`},
		{"bad mode", "list_scans", map[string]any{"target": "site", "consentMode": "maybe"}, "consentMode"},
		{"no target", "get_scan", map[string]any{"consentMode": "reject"}, "target is required"},
		{"unknown argument", "list_requests", map[string]any{"target": "site", "consentMode": "reject", "partie": "third"}, "partie"},
		{"missing artifact", "get_artifact", map[string]any{"ref": "body/" + strings.Repeat("0", 64)}, "not stored"},
		{"path in ref", "get_artifact", map[string]any{"ref": "../../etc/passwd"}, "artifact"},
		{"missing ref", "get_artifact", map[string]any{}, "ref is required"},
	}

	for _, tc := range cases {
		tr := call(t, f.st, tc.tool, tc.args)
		if !tr.IsError || !strings.Contains(tr.Content[0].Text, tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %+v", tc.name, tc.want, tr)
		}
	}
}

func TestListRequestsPagesAndFilters(t *testing.T) {
	t.Parallel()

	f := seed(t)

	type page struct {
		TotalInScan, Matching, Offset, Returned int
		More                                    bool
		Requests                                []struct{ URL, BodyRef string }
	}

	var got page
	decode(t, call(t, f.st, "list_requests",
		map[string]any{"target": "site", "consentMode": "reject", "scanId": "scan-2", "limit": 2}), &got)

	if got.TotalInScan != 3 || got.Matching != 3 || got.Returned != 2 || !got.More {
		t.Errorf("first page = %+v", got)
	}

	if got.Requests[0].BodyRef != f.bodyRef {
		t.Errorf("bodyRef = %q, want %q", got.Requests[0].BodyRef, f.bodyRef)
	}

	got = page{}
	decode(t, call(t, f.st, "list_requests",
		map[string]any{"target": "site", "consentMode": "reject", "scanId": "scan-2", "offset": 2}), &got)

	if got.Returned != 1 || got.More || got.Requests[0].URL != "https://ads.test/a.js" {
		t.Errorf("second page = %+v", got)
	}

	got = page{}
	decode(t, call(t, f.st, "list_requests", map[string]any{
		"target": "site", "consentMode": "reject", "scanId": "scan-2",
		"party": "third", "phase": "pre-interaction",
	}), &got)

	if got.Matching != 1 || got.Requests[0].URL != "https://tracker.test/t.js" {
		t.Errorf("filtered = %+v", got)
	}

	got = page{}
	decode(t, call(t, f.st, "list_requests",
		map[string]any{"target": "site", "consentMode": "reject", "scanId": "scan-2", "offset": 50}), &got)

	if got.Returned != 0 || got.Requests == nil {
		t.Errorf("past the end = %+v; want an empty list, not null", got)
	}
}

func TestDiffScans(t *testing.T) {
	t.Parallel()

	f := seed(t)

	type report struct {
		Against string
		Rules   string
		Report  struct {
			BaselineScanID string
			Comparable     bool
			Reason         string
			Changes        []struct{ Type, Subject string }
		}
	}

	var got report
	decode(t, call(t, f.st, "diff_scans",
		map[string]any{"target": "site", "consentMode": "reject", "scanId": "scan-2", "against": "previous"}), &got)

	if got.Against != "previous" || got.Report.BaselineScanID != "scan-1" || !got.Report.Comparable {
		t.Errorf("against previous = %+v", got)
	}

	subjects := ""
	for _, c := range got.Report.Changes {
		subjects += c.Subject + " "
	}

	if !strings.Contains(subjects, "tracker.test") || !strings.Contains(subjects, "ads.test") {
		t.Errorf("new third parties missing from %q", subjects)
	}

	// With "against" omitted, an approved baseline is what is compared to.
	got = report{}
	decode(t, call(t, f.st, "diff_scans",
		map[string]any{"target": "site", "consentMode": "reject", "scanId": "scan-2"}), &got)

	if got.Against != "baseline" || got.Report.BaselineScanID != "scan-1" {
		t.Errorf("default basis = %+v", got)
	}

	// A first scan with no baseline has nothing to compare to, and says so.
	got = report{}
	decode(t, call(t, f.st, "diff_scans", map[string]any{"target": "other", "consentMode": "accept"}), &got)

	if got.Against != "previous" || got.Report.Comparable || got.Report.Reason == "" {
		t.Errorf("first scan = %+v", got)
	}
}

func TestGetArtifactBody(t *testing.T) {
	t.Parallel()

	f := seed(t)

	type meta struct {
		Bytes     int64
		Returned  int
		Truncated bool
		Encoding  string
		Untrusted string
	}

	// 1001 bytes lands inside the two-byte "é" of the repeated line.
	tr := call(t, f.st, "get_artifact", map[string]any{"ref": f.bodyRef, "maxBytes": 1001})

	var m meta
	decode(t, tr, &m)

	if !m.Truncated || m.Bytes != int64(f.bigBodyLen) || m.Encoding != "text" || m.Untrusted == "" {
		t.Errorf("meta = %+v", m)
	}

	if len(tr.Content) != 2 || len(tr.Content[1].Text) != m.Returned || m.Returned > 1001 {
		t.Fatalf("body block = %d bytes, meta says %d", len(tr.Content[1].Text), m.Returned)
	}

	if !strings.HasPrefix(tr.Content[1].Text, "console.log('é');") {
		t.Errorf("body starts %q", tr.Content[1].Text[:20])
	}

	tr = call(t, f.st, "get_artifact", map[string]any{"ref": f.bodyRef, "maxBytes": 1 << 20})
	m = meta{}
	decode(t, tr, &m)

	if m.Truncated || m.Returned != f.bigBodyLen {
		t.Errorf("whole body = %+v", m)
	}
}

func TestGetArtifactScreenshotIsAnImage(t *testing.T) {
	t.Parallel()

	f := seed(t)
	tr := call(t, f.st, "get_artifact", map[string]any{"ref": f.shotRef})

	if tr.IsError || len(tr.Content) != 2 || tr.Content[1].Type != "image" || tr.Content[1].MimeType != "image/png" {
		t.Fatalf("screenshot = %+v", tr)
	}

	data, err := base64.StdEncoding.DecodeString(tr.Content[1].Data)
	if err != nil || !bytes.Equal(data, pngHeader) {
		t.Errorf("image data = %q, %v", data, err)
	}
}

// panicking is a store whose series listing panics, standing in for any bug
// a tool might hit.
type panicking struct{ store.Store }

func (panicking) Series() ([]store.Series, error) { panic("boom") }

func TestAPanickingToolDoesNotEndTheSession(t *testing.T) {
	t.Parallel()

	f := seed(t)

	resps := exchange(t, panicking{f.st},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_series"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)

	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}

	var tr toolResult
	if err := json.Unmarshal(resps[0].Result, &tr); err != nil || !tr.IsError {
		t.Errorf("panicking tool = %s, %v", resps[0].Result, err)
	}

	if string(resps[1].Result) != "{}" {
		t.Errorf("ping after the panic = %+v", resps[1])
	}
}

func TestServeStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	f := seed(t)

	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- mcp.New(f.st, "test", slog.New(slog.DiscardHandler)).Serve(ctx, in, io.Discard)
	}()

	cancel()

	if err := <-done; err == nil {
		t.Error("Serve returned nil after cancellation; want the context's error")
	}
}
