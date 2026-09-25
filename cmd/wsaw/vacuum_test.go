package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/store"
)

// `wsaw store vacuum` (Story 4.13, AC1, AC4, AC8).

func lastVacuumReceipt(t *testing.T, configPath string) (store.MaintenanceRun, bool) {
	t.Helper()

	s, err := store.OpenSQL(t.Context(), store.Options{
		Path:        storePath(configPath),
		ArtifactDir: strings.TrimSuffix(storePath(configPath), "wsaw.db") + "artifacts",
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	defer func() { _ = s.Close() }()

	run, found, err := s.LastMaintenanceRun(t.Context(), store.MaintenanceKindVacuum)
	if err != nil {
		t.Fatalf("LastMaintenanceRun: %v", err)
	}

	return run, found
}

// TestCmdStoreVacuumRewritesTheFileAndRecordsACLIReceipt: a forced vacuum
// reports the file before and after and what it reclaimed, and leaves a
// receipt with the command's trigger.
func TestCmdStoreVacuumRewritesTheFileAndRecordsACLIReceipt(t *testing.T) {
	path := writeRetentionConfig(t, "")

	seedStore(t, storePath(path), "scan-1", "scan-2")

	var runErr error

	stdout, stderr := capture(t, func() {
		runErr = cmdStore(context.Background(), []string{"vacuum", "--config", path, "--force"})
	})

	if runErr != nil {
		t.Fatalf("store vacuum: %v (stderr=%q)", runErr, stderr)
	}

	for _, want := range []string{"file:", "free:", "after:", "reclaimed:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not report %q:\n%s", want, stdout)
		}
	}

	run, found := lastVacuumReceipt(t, path)
	if !found || run.Trigger != store.TriggerCLI || run.Error != "" {
		t.Errorf("receipt = %+v (found %v), want a clean cli vacuum", run, found)
	}
}

// TestCmdStoreVacuumSkipsBelowTheThreshold is AC4: without --force, a file
// with too little free is left alone, the command says why, and succeeds.
func TestCmdStoreVacuumSkipsBelowTheThreshold(t *testing.T) {
	path := writeRetentionConfig(t, "  vacuumMinFreeRatio: 1\n")

	seedStore(t, storePath(path), "scan-1")

	var runErr error

	stdout, stderr := capture(t, func() {
		runErr = cmdStore(context.Background(), []string{"vacuum", "--config", path})
	})

	if runErr != nil {
		t.Fatalf("store vacuum: %v (stderr=%q)", runErr, stderr)
	}

	if !strings.Contains(stdout, "skipped:") || !strings.Contains(stdout, "--force") {
		t.Errorf("stdout does not say the vacuum was skipped and how to force it:\n%s", stdout)
	}

	if run, _ := lastVacuumReceipt(t, path); !strings.Contains(string(run.Stats), `"outcome":"skipped"`) {
		t.Errorf("receipt stats = %s, want a skip", run.Stats)
	}
}

// TestCmdStoreVacuumDryRunWritesNothing is AC1's --dry-run: it reports what
// a vacuum would need and reclaim, and records no receipt.
func TestCmdStoreVacuumDryRunWritesNothing(t *testing.T) {
	path := writeRetentionConfig(t, "")

	seedStore(t, storePath(path), "scan-1")

	var runErr error

	stdout, stderr := capture(t, func() {
		runErr = cmdStore(context.Background(), []string{"vacuum", "--config", path, "--dry-run", "--force"})
	})

	if runErr != nil {
		t.Fatalf("store vacuum --dry-run: %v (stderr=%q)", runErr, stderr)
	}

	if !strings.Contains(stdout, "needs:") || !strings.Contains(stdout, "Nothing was written") {
		t.Errorf("stdout does not report the plan:\n%s", stdout)
	}

	if _, found := lastVacuumReceipt(t, path); found {
		t.Error("a dry run left a receipt")
	}
}
