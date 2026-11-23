package textcheck

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The adapter seam: a model gets its own answer format, and the prompt
// digest follows it, so moving a model to another adapter checks its texts
// again (Story 4.16, AC3).
func TestEachModelGetsItsAdapter(t *testing.T) {
	t.Parallel()

	for modelID, want := range map[string]string{
		"qwen3.8:27b-mlx": "lines",
		"qwen3.8":         "lines",
		"claude-opus-5-5": "json",
		"gemma4:31b-mlx":  "lines",
		"gemma4:26b-mlx":  "lines",
		"qwen3.5:latest":  "json",
	} {
		if got := AdapterFor(modelID).Name(); got != want {
			t.Errorf("AdapterFor(%q) = %s, want %s", modelID, got, want)
		}
	}

	if PromptDigest("qwen3.8:27b-mlx", "p") == PromptDigest("claude-opus-5-5", "p") {
		t.Error("two answer formats share a prompt digest")
	}

	if PromptDigest("qwen3.8:27b-mlx", "p") != PromptDigest("gemma4:31b-mlx", "p") {
		t.Error("one answer format has two prompt digests")
	}
}

// The line adapter reads what a JSON string cannot hold, and refuses
// everything that is not exactly its format.
func TestTheLineAdapterParsesStrictly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, answer, verdict, err string
		deciding, findings         int
	}{
		{name: "German quotes", answer: "VERDICT: failed\nDECIDING: Unter „Registergericht\" steht „HRB\".\n\nINFORMATIVE: Link-Ziel weicht ab.",
			verdict: model.VerdictFailed, deciding: 1, findings: 2},
		{name: "in a code fence", answer: "```\nVERDICT: passed\n```", verdict: model.VerdictPassed},
		{name: "unknown verdict", answer: "VERDICT: ok", err: "unknown verdict"},
		{name: "unmarked finding", answer: "VERDICT: passed\nDas Impressum ist vollständig.", err: "not a verdict or a finding"},
		{name: "findings first", answer: "DECIDING: x\nVERDICT: failed", err: "no verdict on the first line"},
		{name: "empty finding", answer: "VERDICT: failed\nDECIDING:", err: "empty finding"},
		{name: "JSON", answer: `{"verdict":"passed","findings":[]}`, err: "no verdict on the first line"},
		{name: "nothing", answer: "  ", err: "no verdict"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			verdict, findings, err := lineAdapter{}.Parse(tt.answer)

			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || verdict != "" {
					t.Fatalf("got %q, %v; want error %q", verdict, err, tt.err)
				}

				return
			}

			deciding := 0

			for _, f := range findings {
				if f.Deciding {
					deciding++
				}
			}

			if err != nil || verdict != tt.verdict || len(findings) != tt.findings || deciding != tt.deciding {
				t.Fatalf("got %q, %+v, %v", verdict, findings, err)
			}
		})
	}
}

// Through the client: a qwen3.8 model is asked for lines and its lines are
// read; the same answer from a model on the JSON adapter is not a verdict.
func TestTheClientUsesTheModelsAdapter(t *testing.T) {
	t.Parallel()

	answer := "VERDICT: failed\nDECIDING: Unter „Registergericht\" steht „HRB\"."

	for modelID, wantVerdict := range map[string]string{"qwen3.8:27b-mlx": model.VerdictFailed, "claude-opus-5-5": ""} {
		f := &fakeModel{answer: reply(answer, "end_turn")}
		c := client(f.serve(t), func(s *Settings) { s.Model = modelID })

		got := c.Check(t.Context(), request)

		req, _ := f.last.Load().(messagesRequest)
		if asksLines := strings.Contains(req.System, "VERDICT:"); asksLines != (modelID == "qwen3.8:27b-mlx") {
			t.Errorf("%s: system prompt asks for lines = %v", modelID, asksLines)
		}

		if got.Verdict != wantVerdict || (wantVerdict == "") != (got.NotChecked != "") {
			t.Errorf("%s: verdict %q, not checked %q", modelID, got.Verdict, got.NotChecked)
		}

		if got.PromptSHA256 != PromptDigest(modelID, request.Prompt) {
			t.Errorf("%s: digest does not follow the adapter", modelID)
		}
	}
}
