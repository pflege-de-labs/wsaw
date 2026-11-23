//go:build mistral

package textcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// verdictMapping is appended to each page prompt of 20 November 2026, which
// asked for German verdicts and "[führt zu: …]" markers, so that the prompt
// and wsaw's answer format do not contradict each other.
const verdictMapping = "\n\nDie Urteile „bestanden“, „nicht bestanden“ und „unsicher“ heißen im Antwortformat " +
	"passed, failed und uncertain. Ein Befund mit „[führt zu: …]“ ist deciding: true, ein „[informativ]“-Befund deciding: false."

// Level 2 of docs/testplan-mistral.md: the real pages of 20 November 2026.
// WSAW_TEST_MISTRAL_PAGES names a directory with one directory per page
// holding prompt.txt and text.txt; every check's record is written to
// results/<model>/<page>-<run>.json under it. Models run in parallel, the
// checks of one model one after another.
//
//	WSAW_TEST_MISTRAL_PAGES=<dir> WSAW_TEST_MISTRAL_MODELS=mistral-small-2603,ministral-8b-2512 \
//	WSAW_TEST_MISTRAL_KEY_FILE=$PWD/mistralai.credentials WSAW_TEST_MISTRAL_RUNS=3 \
//	go test -tags mistral ./internal/textcheck -run MistralPages -v -timeout 3h
func TestMistralPages(t *testing.T) {
	dir, models, keyFile := os.Getenv("WSAW_TEST_MISTRAL_PAGES"), os.Getenv("WSAW_TEST_MISTRAL_MODELS"), os.Getenv("WSAW_TEST_MISTRAL_KEY_FILE")
	if dir == "" || models == "" || keyFile == "" {
		t.Skip("set WSAW_TEST_MISTRAL_PAGES, WSAW_TEST_MISTRAL_MODELS and WSAW_TEST_MISTRAL_KEY_FILE")
	}

	runs, _ := strconv.Atoi(os.Getenv("WSAW_TEST_MISTRAL_RUNS"))
	runs = max(runs, 1)

	key, err := secret.Resolve("${file:" + keyFile + "}")
	if err != nil || !key.IsSet() {
		t.Fatalf("reading the key file: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var pages []string

	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "text.txt")); err == nil {
			pages = append(pages, e.Name())
		}
	}

	sort.Strings(pages)

	date := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup

	for _, modelID := range strings.Split(models, ",") {
		wg.Add(1)

		go func() {
			defer wg.Done()

			c := New(Settings{
				API: APIMistral, Key: key.Reveal(), Model: modelID,
				Timeout: 10 * time.Minute, Retries: 3, Backoff: 5 * time.Second,
				MaxOutputTokens: 16384, DailyRequests: 10000,
			})

			for run := 1; run <= runs; run++ {
				for _, page := range pages {
					prompt, err1 := os.ReadFile(filepath.Join(dir, page, "prompt.txt"))
					text, err2 := os.ReadFile(filepath.Join(dir, page, "text.txt"))

					if err1 != nil || err2 != nil {
						t.Errorf("%s: %v %v", page, err1, err2)

						continue
					}

					started := time.Now()
					got := c.Check(t.Context(), Request{
						Prompt: string(prompt) + verdictMapping, Text: string(text), TextSHA256: page,
						Date: date, MaxInputTokens: 100000,
					})
					took := time.Since(started)

					record := map[string]any{"model": modelID, "page": page, "run": run, "seconds": took.Seconds(), "check": got}

					out := filepath.Join(dir, "results", modelID, fmt.Sprintf("%s-%d.json", page, run))
					_ = os.MkdirAll(filepath.Dir(out), 0o700)

					b, _ := json.MarshalIndent(record, "", " ")
					if err := os.WriteFile(out, b, 0o600); err != nil {
						t.Errorf("%s: %v", out, err)
					}

					t.Logf("%s %s run %d: %s, verdict %q, not checked %q, tokens %d/%d",
						modelID, page, run, took.Round(time.Second), got.Verdict, got.NotChecked, got.InputTokens, got.OutputTokens)
				}
			}
		}()
	}

	wg.Wait()
}
