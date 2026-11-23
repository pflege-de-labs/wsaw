//go:build mistral

package textcheck

import (
	"os"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// The check against Mistral AI's API (docs/testplan-mistral.md). Unlike the
// Ollama test this sends the fixtures — synthetic imprints, no scanned page —
// to a third party and is billed, so it is behind a build tag CI never sets
// and skips without a model and a key file:
//
//	WSAW_TEST_MISTRAL_MODEL=mistral-small-latest \
//	WSAW_TEST_MISTRAL_KEY_FILE=$PWD/mistralai.credentials \
//	go test -tags mistral ./internal/textcheck -run Mistral -v
//
// The key is read through the same reference wsaw's configuration uses and
// is never logged; the record of every check is free of it (AC8).
func TestAgainstMistral(t *testing.T) {
	modelID, keyFile := os.Getenv("WSAW_TEST_MISTRAL_MODEL"), os.Getenv("WSAW_TEST_MISTRAL_KEY_FILE")
	if modelID == "" || keyFile == "" {
		t.Skip("set WSAW_TEST_MISTRAL_MODEL and WSAW_TEST_MISTRAL_KEY_FILE to run against Mistral AI")
	}

	key, err := secret.Resolve("${file:" + keyFile + "}")
	if err != nil || !key.IsSet() {
		t.Fatalf("reading the key file: %v", err)
	}

	runLiveCases(t, New(Settings{
		API:     APIMistral,
		Key:     key.Reveal(),
		Model:   modelID,
		Timeout: 5 * time.Minute,
		Retries: 1,
	}), "api.mistral.ai")
}
