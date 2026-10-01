package mcp_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// diff_scans must compare under the configured rules, the way the scan's
// own diff did, or a reader asking the LLM sees churn the scan did not
// report.
func TestDiffScansKeysAssetsUnderTheConfiguredRules(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	gtag := func(gtm string) model.Request {
		return request("https://www.googletagmanager.com/gtag/js?id=G-1&gtm="+gtm,
			"www.googletagmanager.com", "googletagmanager.com", model.ThirdParty, model.PhasePost)
	}

	for _, r := range []*model.Result{
		result("scan-1", epoch, gtag("4e69t1")),
		result("scan-2", epoch.Add(time.Hour), gtag("4e6a01")),
	} {
		if err := st.PutResult(r); err != nil {
			t.Fatalf("storing %s: %v", r.ScanID, err)
		}
	}

	n, err := normalize.New(normalize.Rules{QueryRules: normalize.DefaultQueryRules})
	if err != nil {
		t.Fatal(err)
	}

	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "diff_scans", "arguments": map[string]any{
			"target": "site", "consentMode": "reject", "scanId": "scan-2", "against": "previous",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	resps := exchangeWith(t, st, n, string(req))
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("want one result, got %+v", resps)
	}

	var res toolResult
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Report struct {
			Comparable bool
			Changes    []struct{ Type, Subject string }
		}
	}

	decode(t, res, &got)

	if !got.Report.Comparable || len(got.Report.Changes) != 0 {
		t.Errorf("report = %+v; a beacon's per-visit parameter alone must not be a change", got.Report)
	}
}
