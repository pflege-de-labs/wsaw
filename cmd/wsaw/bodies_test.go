package main

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 1.11, AC8: --bodies forces this run's sample, in a mode it names.
func TestParseForcedBodies(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]model.BodyStore{
		"":       "",
		"hashed": model.BodyStoreHashed,
		"all":    model.BodyStoreAll,
	} {
		got, err := parseForcedBodies(value)
		if err != nil || got != want {
			t.Errorf("parseForcedBodies(%q) = %q, %v; want %q", value, got, err, want)
		}
	}

	for _, bad := range []string{"none", "everything", "ALL"} {
		if _, err := parseForcedBodies(bad); err == nil {
			t.Errorf("parseForcedBodies(%q) accepted a mode that does not exist", bad)
		}
	}
}
