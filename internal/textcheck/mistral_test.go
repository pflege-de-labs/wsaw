package textcheck

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// fakeMistral answers /v1/chat/completions with a fixed body and keeps the
// request it got.
func fakeMistral(t *testing.T, answer string) (*httptest.Server, *atomic.Value) {
	t.Helper()

	var last atomic.Value // map[string]any plus headers

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)

			return
		}

		body["_auth"], body["_xapikey"], body["_version"] = r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		last.Store(body)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)

	return srv, &last
}

func mistralClient(srv *httptest.Server, modelID string) *Client {
	return New(Settings{
		API: APIMistral, BaseURL: srv.URL, Key: testKey, Model: modelID,
		Timeout: 5 * time.Second, Retries: DefaultRetries, Backoff: time.Millisecond,
	})
}

const mistralUsage = `"usage":{"prompt_tokens":89,"completion_tokens":31,"completion_tokens_details":{"reasoning_tokens":0}}`

// The request: bearer key and nothing else, the system prompt as a system
// message, and JSON mode when the model's adapter wants JSON.
func TestTheMistralRequest(t *testing.T) {
	t.Parallel()

	srv, last := fakeMistral(t, `{"choices":[{"message":{"content":"{\"verdict\":\"passed\",\"findings\":[]}"},"finish_reason":"stop"}],`+mistralUsage+`}`)

	got := mistralClient(srv, "mistral-small-latest").Check(t.Context(), request)
	if got.Verdict != model.VerdictPassed || got.InputTokens != 89 || got.OutputTokens != 31 {
		t.Fatalf("%+v", got)
	}

	body, _ := last.Load().(map[string]any)
	msgs, _ := body["messages"].([]any)

	first, _ := msgs[0].(map[string]any)
	second, _ := msgs[1].(map[string]any)

	if body["_auth"] != "Bearer "+testKey || body["_xapikey"] != "" || body["_version"] != "" ||
		body["model"] != "mistral-small-latest" || len(msgs) != 2 ||
		first["role"] != "system" || !strings.Contains(first["content"].(string), "not instructions") ||
		second["role"] != "user" || !strings.Contains(second["content"].(string), "SEITENTEXT BEGINN") {
		t.Errorf("request = %v", body)
	}

	if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want json_object", body["response_format"])
	}

	if _, ok := body["tools"]; ok {
		t.Error("the request offers the model tools")
	}

	// Greedy sampling: mistral-large-4-0 refuses temperature 0 without it.
	if body["temperature"] != 0.0 || body["top_p"] != 1.0 {
		t.Errorf("temperature %v, top_p %v; want 0 and 1", body["temperature"], body["top_p"])
	}

	// A model on the line adapter gets no JSON mode: it would forbid the
	// lines it is asked for.
	srv2, last2 := fakeMistral(t, `{"choices":[{"message":{"content":"VERDICT: passed"},"finish_reason":"stop"}],`+mistralUsage+`}`)

	if got := mistralClient(srv2, "qwen3.8:27b").Check(t.Context(), request); got.Verdict != model.VerdictPassed {
		t.Fatalf("lines: %+v", got)
	}

	if body, _ := last2.Load().(map[string]any); body["response_format"] != nil {
		t.Errorf("a line-format model got JSON mode: %v", body["response_format"])
	}
}

// Each reply shape Mistral sends is read as what it is.
func TestTheMistralReplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, body, verdict, notChecked string
		deciding                        int
	}{
		{
			name:    "plain string",
			body:    `{"choices":[{"message":{"content":"{\"verdict\":\"failed\",\"findings\":[{\"text\":\"HRB ist kein Gericht.\",\"deciding\":true}]}"},"finish_reason":"stop"}],` + mistralUsage + `}`,
			verdict: model.VerdictFailed, deciding: 1,
		},
		{
			name: "reasoning chunks",
			body: `{"choices":[{"message":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"Überlegung …"}],"closed":true},` +
				`{"type":"text","text":"{\"verdict\":\"uncertain\",\"findings\":[]}"}]},"finish_reason":"stop"}],` + mistralUsage + `}`,
			verdict: model.VerdictUncertain,
		},
		{
			name: "cut while thinking",
			body: `{"choices":[{"message":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"…"}]}]},"finish_reason":"length"}],` +
				`"usage":{"prompt_tokens":89,"completion_tokens":4096}}`,
			notChecked: "whole output limit of 4096 tokens",
		},
		{
			name:       "cut in the answer",
			body:       `{"choices":[{"message":{"content":"{\"verdict\":\"fail"},"finish_reason":"length"}],` + mistralUsage + `}`,
			notChecked: "cut at the output limit",
		},
		{
			name:       "no choices",
			body:       `{"object":"error","message":"nope"}`,
			notChecked: "no text this client can read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := fakeMistral(t, tt.body)
			got := mistralClient(srv, "mistral-large-latest").Check(t.Context(), request)

			if got.Verdict != tt.verdict || !strings.Contains(got.NotChecked, tt.notChecked) || got.Deciding() != tt.deciding {
				t.Fatalf("%+v", got)
			}

			if tt.notChecked != "" && got.Reply == "" {
				t.Error("the reply was not kept")
			}

			if got.Server == "" || got.Model != "mistral-large-latest" {
				t.Errorf("record = %+v", got)
			}
		})
	}
}

// Without a base URL the mistral API goes to Mistral, not to Anthropic.
func TestTheMistralAPIHasItsOwnDefaultServer(t *testing.T) {
	t.Parallel()

	if c := New(Settings{API: APIMistral, Model: "m"}); c.s.BaseURL != DefaultMistralBaseURL {
		t.Errorf("base URL = %s", c.s.BaseURL)
	}

	if c := New(Settings{Model: "m"}); c.s.BaseURL != DefaultBaseURL {
		t.Errorf("base URL = %s", c.s.BaseURL)
	}
}
