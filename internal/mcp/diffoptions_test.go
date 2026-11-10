package mcp_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/mcp"
)

// diff_scans ranks changes with the target's own settings, as the scan's
// notification and the web interface do, so the three never disagree about
// the same pair of scans.
func TestDiffScansUsesTheTargetsSeverityRules(t *testing.T) {
	t.Parallel()

	f := seed(t)

	var asked string

	opts := func(target string) diff.Options {
		asked = target

		return diff.Options{Severity: diff.SeverityRules{ThirdPartyHostRejectMode: diff.SeverityLow}}
	}

	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"diff_scans","arguments":` +
		`{"target":"site","consentMode":"reject","scanId":"scan-2","against":"previous"}}}`

	var out bytes.Buffer
	if err := mcp.New(f.st, nil, opts, "test", slog.New(slog.DiscardHandler)).
		Serve(t.Context(), strings.NewReader(req+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}

	var tr toolResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("decoding the tool result: %v", err)
	}

	var got struct {
		Rules  string
		Report struct {
			Changes []struct {
				Type     diff.ChangeType
				Subject  string
				Severity diff.Severity
			}
		}
	}
	decode(t, tr, &got)

	if asked != "site" || got.Rules != "target" {
		t.Errorf("settings asked for %q, rules %q; want site and target", asked, got.Rules)
	}

	for _, c := range got.Report.Changes {
		if c.Type == diff.HostAdded && c.Subject == "tracker.test" {
			if c.Severity != diff.SeverityLow {
				t.Errorf("host-added tracker.test ranks %s, want the target's own low", c.Severity)
			}

			return
		}
	}

	t.Fatalf("no host-added change for tracker.test in %+v", got.Report.Changes)
}
