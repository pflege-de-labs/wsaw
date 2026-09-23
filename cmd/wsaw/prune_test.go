package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// writeRetentionConfig writes a configuration whose store block the test
// controls, which writeConfig cannot do: it appends a store block of its own.
func writeRetentionConfig(t *testing.T, storeBlock string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.yaml")

	body := oneTargetConfig + "\nstore:\n  path: " + filepath.Join(dir, "wsaw.db") + "\n" + storeBlock

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	return path
}

func storedScanIDs(t *testing.T, path string) []string {
	t.Helper()

	st, err := store.Open(t.Context(), store.Options{Path: path, ArtifactDir: filepath.Join(filepath.Dir(path), "artifacts")})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	defer func() { _ = st.Close() }()

	got, err := st.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("listing results: %v", err)
	}

	out := make([]string, 0, len(got))

	for _, g := range got {
		out = append(out, g.ScanID)
	}

	return out
}

// TestCmdPruneDryRunDeletesNothingAndSaysWhy is the reason the command
// exists: a policy change is retroactive, so an operator has to be able to
// read the consequence before it happens.
func TestCmdPruneDryRunDeletesNothingAndSaysWhy(t *testing.T) {
	path := writeRetentionConfig(t, "  keep:\n    last: 1\n")

	seedStore(t, storePath(path), "scan-1", "scan-2", "scan-3")

	var runErr error

	stdout, stderr := capture(t, func() {
		runErr = cmdPrune(context.Background(), []string{"--config", path, "--dry-run", "--all"})
	})

	if runErr != nil {
		t.Fatalf("cmdPrune: %v (stderr=%q)", runErr, stderr)
	}

	if !strings.Contains(stdout, "would delete 2 result(s)") {
		t.Errorf("stdout = %q, want the count it would delete", stdout)
	}

	if !strings.Contains(stdout, "keep (last)") {
		t.Errorf("stdout = %q, want the rule that kept the survivor", stdout)
	}

	if got := storedScanIDs(t, storePath(path)); len(got) != 3 {
		t.Errorf("a dry run left %v, want all three scans", got)
	}
}

func TestCmdPruneAppliesTheConfiguredPolicy(t *testing.T) {
	path := writeRetentionConfig(t, "  keep:\n    last: 1\n")

	seedStore(t, storePath(path), "scan-1", "scan-2", "scan-3")

	var runErr error

	stdout, stderr := capture(t, func() {
		runErr = cmdPrune(context.Background(), []string{"--config", path})
	})

	if runErr != nil {
		t.Fatalf("cmdPrune: %v (stderr=%q)", runErr, stderr)
	}

	if !strings.Contains(stdout, "deleted 2 result(s)") {
		t.Errorf("stdout = %q, want what it deleted", stdout)
	}

	got := storedScanIDs(t, storePath(path))
	if len(got) != 1 || got[0] != "scan-3" {
		t.Errorf("kept %v, want only the newest scan", got)
	}
}

// TestCmdPruneRecordsACLITriggeredReceipt: "wsaw prune" is a CLI invocation,
// not the daemon's scheduled loop, and Story 4.11's receipt log needs to say
// which one ran (AC7).
func TestCmdPruneRecordsACLITriggeredReceipt(t *testing.T) {
	path := writeRetentionConfig(t, "  keep:\n    last: 1\n")

	seedStore(t, storePath(path), "scan-1", "scan-2")

	if err := cmdPrune(context.Background(), []string{"--config", path}); err != nil {
		t.Fatalf("cmdPrune: %v", err)
	}

	s, err := store.OpenSQL(t.Context(),
		store.Options{Path: storePath(path), ArtifactDir: filepath.Join(filepath.Dir(storePath(path)), "artifacts")})
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}

	defer func() { _ = s.Close() }()

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindPrune)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	if !found {
		t.Fatal("wsaw prune left no receipt")
	}

	if run.Trigger != store.TriggerCLI {
		t.Errorf("Trigger = %q, want %q", run.Trigger, store.TriggerCLI)
	}
}

// TestCmdPruneRefusesWithoutAPolicy: deleting nothing while reporting success
// would leave an operator believing retention is running when it is not.
func TestCmdPruneRefusesWithoutAPolicy(t *testing.T) {
	path := writeRetentionConfig(t, "  maxPerSeries: 0\n")

	seedStore(t, storePath(path), "scan-1")

	err := cmdPrune(context.Background(), []string{"--config", path})
	if err == nil {
		t.Fatal("cmdPrune accepted a configuration with no retention")
	}

	if !strings.Contains(err.Error(), "store.keep") {
		t.Errorf("error = %v, want it to name the setting that is missing", err)
	}
}
