package textcheck

import (
	"encoding/json"
	"net/http"
)

// The APIs a check can be sent through. The answer format is the model's
// (Adapter); how a request travels is the API's (wire).
const (
	// APIMessages is the Anthropic Messages API and every server that offers
	// a compatible /v1/messages, such as Ollama (AC10).
	APIMessages = "anthropic"
	// APIMistral is Mistral AI's native chat completions API, which has no
	// Messages-compatible endpoint.
	APIMistral = "mistral"

	// DefaultMistralBaseURL is where APIMistral goes unless told otherwise.
	DefaultMistralBaseURL = "https://api.mistral.ai"

	// stopMaxTokens is the Messages stop reason for a reply cut at the output
	// limit; Mistral's "length" is read as it.
	stopMaxTokens = "max_tokens"
)

// wire is how a request is put on and taken off one API: the path, the
// headers, the request body and the reply. It is the second seam next to
// Adapter, and like it, wsaw code rather than configuration.
type wire interface {
	path() string
	headers(h http.Header, key, auth string)
	encode(r wireRequest) ([]byte, error)
	// decode reads a reply into the Messages shape the rest of the client
	// works on. A body it cannot read leaves the reply empty, which
	// interpret records with the body kept.
	decode(raw []byte) messagesResponse
}

// wireRequest is what every API is sent.
type wireRequest struct {
	Model     string
	MaxTokens int
	System    string
	User      string
	// JSON asks the API for its JSON mode, where it has one, because the
	// model's adapter wants a JSON answer.
	JSON bool
}

func wireFor(api string) wire {
	if api == APIMistral {
		return mistralWire{}
	}

	return messagesWire{}
}

// messagesWire speaks the common subset of the Anthropic Messages API.
type messagesWire struct{}

func (messagesWire) path() string { return "/v1/messages" }

func (messagesWire) headers(h http.Header, key, auth string) {
	h.Set("anthropic-version", anthropicVersion)

	switch {
	case key == "":
	case auth == AuthBearer:
		h.Set("Authorization", "Bearer "+key)
	default:
		h.Set("x-api-key", key)
	}
}

func (messagesWire) encode(r wireRequest) ([]byte, error) {
	return json.Marshal(messagesRequest{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		System:    r.System,
		Messages:  []message{{Role: "user", Content: r.User}},
	})
}

func (messagesWire) decode(raw []byte) messagesResponse {
	var reply messagesResponse
	_ = json.Unmarshal(raw, &reply) // what does not decode stays empty, and interpret says so

	return reply
}

// mistralWire speaks Mistral's /v1/chat/completions, read against the live
// API on 23 November 2026: content is a string, or with reasoning a list of
// "thinking" and "text" chunks; tokens are usage.prompt_tokens and
// usage.completion_tokens. It always sends the key as a bearer token, the
// only way Mistral takes it.
type mistralWire struct{}

func (mistralWire) path() string { return "/v1/chat/completions" }

func (mistralWire) headers(h http.Header, key, _ string) {
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
}

type mistralRequest struct {
	Model          string          `json:"model"`
	MaxTokens      int             `json:"max_tokens"`
	Temperature    float64         `json:"temperature"`
	Messages       []message       `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

func (mistralWire) encode(r wireRequest) ([]byte, error) {
	req := mistralRequest{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		Messages:  []message{{Role: "system", Content: r.System}, {Role: "user", Content: r.User}},
	}

	// JSON mode guarantees a parseable object, which is exactly what broke
	// on other models: a German quote closing a JSON string.
	if r.JSON {
		req.ResponseFormat = &responseFormat{Type: "json_object"}
	}

	return json.Marshal(req)
}

func (mistralWire) decode(raw []byte) messagesResponse {
	var v struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	var reply messagesResponse

	if json.Unmarshal(raw, &v) != nil || len(v.Choices) == 0 {
		return reply
	}

	choice := v.Choices[0]

	reply.Usage.InputTokens, reply.Usage.OutputTokens = v.Usage.PromptTokens, v.Usage.CompletionTokens

	reply.StopReason = choice.FinishReason
	if reply.StopReason == "length" {
		reply.StopReason = stopMaxTokens
	}

	var text string
	if json.Unmarshal(choice.Message.Content, &text) == nil {
		reply.Content = append(reply.Content, contentBlock{Type: "text", Text: text})

		return reply
	}

	// With reasoning: thinking chunks, which are discarded like a Messages
	// thinking block, and the answer in text chunks.
	var chunks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	if json.Unmarshal(choice.Message.Content, &chunks) == nil {
		for _, c := range chunks {
			reply.Content = append(reply.Content, contentBlock{Type: c.Type, Text: c.Text})
		}
	}

	return reply
}
