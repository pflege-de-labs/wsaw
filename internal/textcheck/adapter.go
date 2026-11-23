package textcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Adapter is how one family of models is asked for its answer and how that
// answer is read (Story 4.16, AC4). Models differ in which answer shape they
// keep reliably — one writes clean JSON, another breaks JSON strings with
// German quotes — so the shape is chosen per model, while what is sent, the
// markers and the rule that only a parsed verdict counts stay wsaw's.
//
// An adapter is wsaw code, not configuration: a parser for hostile model
// output is not something an operator should have to write. A new one is a
// type here and a line in adapters.
type Adapter interface {
	// Name identifies the adapter in errors and logs.
	Name() string
	// Format is the part of wsaw's instruction that fixes the answer's
	// shape. It is part of the prompt digest, so a changed format is a
	// changed prompt and texts are checked again.
	Format() string
	// Parse reads the model's answer strictly into a verdict and findings.
	// Anything not in the asked-for shape is an error, never a verdict.
	Parse(answer string) (verdict string, findings []model.TextFinding, err error)
}

// adapters maps model ID prefixes to the adapter for them; the first match
// wins, and a model none matches gets the JSON adapter.
var adapters = []struct {
	prefix  string
	adapter Adapter
}{
	{"qwen3.8", lineAdapter{}},
	{"gemma4", lineAdapter{}},
}

// AdapterFor is the adapter wsaw uses for a model ID.
func AdapterFor(modelID string) Adapter {
	for _, a := range adapters {
		if strings.HasPrefix(modelID, a.prefix) {
			return a.adapter
		}
	}

	return jsonAdapter{}
}

// jsonAdapter asks for one JSON object. It is the default: Claude models
// keep it reliably, and it is the shape a client expects.
type jsonAdapter struct{}

func (jsonAdapter) Name() string { return "json" }

func (jsonAdapter) Format() string {
	return `Answer with exactly one JSON object and nothing else, in this shape:
{"verdict": "passed" | "failed" | "uncertain", "findings": [{"text": "<one finding>", "deciding": true | false}]}
A finding is deciding when it leads to the verdict on its own, and informative otherwise. Write the findings in the language of the instructions.
The answer must be valid JSON: inside a finding, quote with typographic quotes such as „…“ or with single quotes, never with a straight double quote.`
}

// Parse reads the one JSON object. A code fence around it is tolerated,
// because models add one; anything else around it, an unknown field, an
// unknown verdict or an unmarked or empty finding is refused.
func (jsonAdapter) Parse(s string) (string, []model.TextFinding, error) {
	s = strings.TrimSpace(s)
	if inner, ok := strings.CutPrefix(s, "```json"); ok {
		s = inner
	} else {
		s = strings.TrimPrefix(s, "```")
	}

	s = strings.TrimSpace(strings.TrimSuffix(s, "```"))

	var v struct {
		Verdict  *string `json:"verdict"`
		Findings []struct {
			Text     string `json:"text"`
			Deciding *bool  `json:"deciding"`
		} `json:"findings"`
	}

	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&v); err != nil {
		return "", nil, err
	}

	if dec.More() {
		return "", nil, errors.New("more than one JSON value")
	}

	if v.Verdict == nil {
		return "", nil, errors.New("no verdict")
	}

	if err := knownVerdict(*v.Verdict); err != nil {
		return "", nil, err
	}

	findings := make([]model.TextFinding, 0, len(v.Findings))

	for i, f := range v.Findings {
		if strings.TrimSpace(f.Text) == "" || f.Deciding == nil {
			return "", nil, fmt.Errorf("finding %d has no text or is not marked deciding or informative", i+1)
		}

		findings = append(findings, model.TextFinding{Text: f.Text, Deciding: *f.Deciding})
	}

	return *v.Verdict, findings, nil
}

// knownVerdict refuses a verdict outside the three wsaw reports.
func knownVerdict(v string) error {
	switch v {
	case model.VerdictPassed, model.VerdictFailed, model.VerdictUncertain:
		return nil
	default:
		return fmt.Errorf("unknown verdict %q", short(v, 40))
	}
}
