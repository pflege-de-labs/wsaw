package textcheck

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// lineAdapter asks for one line per item. It exists for qwen3.8, which
// closes German quotes („…") with a straight double quote and so breaks
// every JSON string that quotes the page — on both runs of the imprint
// fixture on 22 November 2026, even when told not to. A line survives that.
// gemma4 uses it too: asked for JSON, gemma4:26b spent its whole output
// limit reasoning on two of three fixtures and never answered, while on lines
// it answered all three in about 25 seconds each.
type lineAdapter struct{}

func (lineAdapter) Name() string { return "lines" }

func (lineAdapter) Format() string {
	return `Answer in exactly this format and nothing else: a first line with the verdict, then one line per finding.
VERDICT: passed | failed | uncertain
DECIDING: <a finding that leads to the verdict on its own>
INFORMATIVE: <a finding that does not>
Each finding is one line. Write the findings in the language of the instructions.`
}

// Parse reads the lines. A code fence around the answer is tolerated; a
// first line that is not the verdict, any other line, an unknown verdict or
// an empty finding is refused.
func (lineAdapter) Parse(s string) (string, []model.TextFinding, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s, "```"), "```"))

	var (
		verdict  string
		findings []model.TextFinding
	)

	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		value = strings.TrimSpace(value)

		switch {
		case !ok:
			return "", nil, fmt.Errorf("line %d is not a verdict or a finding", i+1)
		case verdict == "" && key == "VERDICT":
			if err := knownVerdict(value); err != nil {
				return "", nil, err
			}

			verdict = value
		case verdict == "":
			return "", nil, errors.New("no verdict on the first line")
		case key == "DECIDING" || key == "INFORMATIVE":
			if value == "" {
				return "", nil, fmt.Errorf("line %d is an empty finding", i+1)
			}

			findings = append(findings, model.TextFinding{Text: value, Deciding: key == "DECIDING"})
		default:
			return "", nil, fmt.Errorf("line %d is not a verdict or a finding", i+1)
		}
	}

	if verdict == "" {
		return "", nil, errors.New("no verdict")
	}

	return verdict, findings, nil
}
