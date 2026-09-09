package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
)

// examplePath is the shipped annotated configuration, from this package's
// directory. The README's quick start is "cp wsaw.example.yaml wsaw.yaml", so
// it is the first file most operators will ever edit.
const examplePath = "../../wsaw.example.yaml"

// TestTheShippedExampleIsValid keeps the annotated configuration honest.
//
// Nothing else did. wsaw.example.yaml documents every setting and is copied by
// the README's quick start, but no build, test or CI job had ever parsed it, so
// a validation rule added for one setting could — and did — leave the shipped
// example rejecting itself with an error the reader had done nothing to cause.
// Story 8.6, AC8 asks the example and the README not to drift apart; this is
// the cheaper half of that, which is the example not drifting away from the
// code.
//
// Parse rather than Load, because Parse is the whole of the structural check
// and resolves no secret. The example's ${env:…} and ${file:…} references are
// meant to be unresolvable here — they name an operator's own environment — so
// resolving them is not something a test can assert, and not what rots.
func TestTheShippedExampleIsValid(t *testing.T) {
	t.Parallel()

	b, err := os.ReadFile(filepath.Clean(examplePath))
	if err != nil {
		t.Fatalf("reading the shipped example: %v", err)
	}

	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatalf("the shipped example does not validate, so `wsaw config --check` on a copy of it fails: %v", err)
	}

	// A guard against the assertion above passing for the wrong reason: an
	// empty or truncated file parses cleanly and would prove nothing.
	if len(cfg.Targets) == 0 {
		t.Fatal("the shipped example parsed but has no targets, so it is not the file this test thinks it is")
	}
}
