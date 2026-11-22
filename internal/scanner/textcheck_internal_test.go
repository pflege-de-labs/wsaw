package scanner

import (
	"context"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/textcheck"
)

// countingChecker answers every check with a verdict and keeps what it was
// asked.
type countingChecker struct {
	calls []textcheck.Request
}

func (c *countingChecker) Model() string { return "test-model" }

func (c *countingChecker) Check(_ context.Context, req textcheck.Request) model.TextCheck {
	c.calls = append(c.calls, req)

	return model.TextCheck{
		Model: "test-model", PromptSHA256: textcheck.PromptDigest(req.Prompt), TextSHA256: req.TextSHA256,
		Verdict: model.VerdictFailed, Findings: []model.TextFinding{{Text: "f", Deciding: true}},
	}
}

// Story 4.16, AC3 and AC8: the first text is sent, an unchanged one is not —
// the earlier findings are carried over and marked reused — and a changed
// prompt or a changed text is sent again. What is sent is the text with its
// link targets.
func TestAnUnchangedTextIsNotSentAgain(t *testing.T) {
	t.Parallel()

	st := &comparisonStore{}
	chk := &countingChecker{}
	s := &Scanner{deps: Deps{Store: st, TextChecker: chk}}
	target := config.Resolved{Name: "site", ModelCheck: &config.ModelCheck{Prompt: "Prüfe."}}

	scan := func(id, text string, target config.Resolved) *model.Result {
		res := cleanResult()
		res.ScanID = id
		res.Text = &model.TextCapture{Selector: "main", Text: text, SHA256: "sha-" + text,
			Links: []model.TextLink{{Text: "Mail", Target: "mailto:a@b.test"}}}
		s.checkText(t.Context(), target, res, quiet())
		st.previous = res

		return res
	}

	first := scan("scan-1", "Impressum", target)
	if len(chk.calls) != 1 || first.Text.Check.Reused || first.Text.Check.Verdict != model.VerdictFailed ||
		!strings.Contains(chk.calls[0].Text, "[Mail](mailto:a@b.test)") {
		t.Fatalf("first scan: %d calls, %+v", len(chk.calls), first.Text.Check)
	}

	again := scan("scan-2", "Impressum", target)
	if len(chk.calls) != 1 || !again.Text.Check.Reused || again.Text.Check.Verdict != model.VerdictFailed {
		t.Fatalf("unchanged text: %d calls, %+v", len(chk.calls), again.Text.Check)
	}

	reworded := target
	reworded.ModelCheck = &config.ModelCheck{Prompt: "Prüfe genauer."}

	if scan("scan-3", "Impressum", reworded); len(chk.calls) != 2 {
		t.Fatalf("changed prompt: %d calls", len(chk.calls))
	}

	if scan("scan-4", "Impressum geändert", reworded); len(chk.calls) != 3 {
		t.Fatalf("changed text: %d calls", len(chk.calls))
	}
}

// AC5: a check that cannot run is recorded with its reason, and a target
// without a check is left alone.
func TestACheckThatCannotRunSaysWhy(t *testing.T) {
	t.Parallel()

	target := config.Resolved{Name: "site", ModelCheck: &config.ModelCheck{Prompt: "Prüfe."}}

	res := cleanResult()
	res.Text = &model.TextCapture{Selector: "main", Text: "Impressum", SHA256: "x"}
	(&Scanner{}).checkText(t.Context(), target, res, quiet())

	if c := res.Text.Check; c == nil || !strings.Contains(c.NotChecked, "no model API") || c.Verdict != "" {
		t.Errorf("no checker: %+v", c)
	}

	res = cleanResult()
	res.Text = &model.TextCapture{Selector: "main", Unavailable: "the selector matched nothing"}
	(&Scanner{deps: Deps{TextChecker: &countingChecker{}}}).checkText(t.Context(), target, res, quiet())

	if c := res.Text.Check; c == nil || !strings.Contains(c.NotChecked, "selector matched nothing") {
		t.Errorf("no text: %+v", c)
	}

	res = cleanResult()
	res.Text = &model.TextCapture{Selector: "main", Text: "Impressum"}
	(&Scanner{deps: Deps{TextChecker: &countingChecker{}}}).checkText(t.Context(), config.Resolved{Name: "site"}, res, quiet())

	if res.Text.Check != nil {
		t.Errorf("a target without a check was checked: %+v", res.Text.Check)
	}
}
