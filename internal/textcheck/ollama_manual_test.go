//go:build ollama

package textcheck

import (
	"cmp"
	"os"
	"testing"
	"time"
)

// The check against a real model on a local Ollama server (Story 4.16,
// AC10). It needs a server and a model on this host and takes minutes, so it
// is behind a build tag that CI never sets, and still skips without a model:
//
//	WSAW_TEST_OLLAMA_MODEL=qwen3.8:27b-mlx go test -tags ollama ./internal/textcheck -run Ollama -v
//
// WSAW_TEST_OLLAMA_URL overrides http://localhost:11434. Nothing leaves this
// host (AGENTS.md §3.7). The cases are runLiveCases'.
func TestAgainstOllama(t *testing.T) {
	modelID := os.Getenv("WSAW_TEST_OLLAMA_MODEL")
	if modelID == "" {
		t.Skip("set WSAW_TEST_OLLAMA_MODEL to run against a local Ollama server")
	}

	base := cmp.Or(os.Getenv("WSAW_TEST_OLLAMA_URL"), "http://localhost:11434")

	runLiveCases(t, New(Settings{
		BaseURL: base,
		Auth:    AuthBearer,
		Model:   modelID,
		Timeout: 10 * time.Minute,
		Retries: 0,
	}), serverOf(base))
}
