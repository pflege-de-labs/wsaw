package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPRefusesAMissingStoreAndCreatesNothing(t *testing.T) {
	t.Parallel()

	cfg := writeConfig(t, oneTargetConfig)
	db := storePath(cfg)

	var out, errOut bytes.Buffer

	err := serveMCP(t.Context(), []string{"--config", cfg}, strings.NewReader(""), &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("serveMCP = %v, want a refusal naming the missing database", err)
	}

	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Errorf("the database %s was created by a read-only command (stat: %v)", db, statErr)
	}

	if _, statErr := os.Stat(filepath.Join(filepath.Dir(db), "artifacts")); !os.IsNotExist(statErr) {
		t.Errorf("the artifact directory was created by a read-only command (stat: %v)", statErr)
	}

	if out.Len() != 0 {
		t.Errorf("stdout carries the protocol and nothing else, got %q", out.String())
	}
}

func TestMCPAnswersFromTheConfiguredStore(t *testing.T) {
	t.Parallel()

	cfg := writeConfig(t, oneTargetConfig)
	seedStore(t, storePath(cfg), "scan-1")

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_scans","arguments":{"target":"site","consentMode":"reject"}}}`,
	}, "\n") + "\n")

	var out, errOut bytes.Buffer

	if err := serveMCP(t.Context(), []string{"--config", cfg}, in, &out, &errOut); err != nil {
		t.Fatalf("serveMCP: %v\nstderr: %s", err, errOut.String())
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses on stdout, got %d: %q", len(lines), out.String())
	}

	if !strings.Contains(lines[0], `"protocolVersion":"2025-06-18"`) {
		t.Errorf("initialize = %s", lines[0])
	}

	if !strings.Contains(lines[1], `scan-1`) || strings.Contains(lines[1], `"isError":true`) {
		t.Errorf("list_scans = %s", lines[1])
	}
}
