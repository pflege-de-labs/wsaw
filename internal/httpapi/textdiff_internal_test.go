package httpapi

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 4.15, AC7: the scan page shows a cut text as the cut, with the long
// unchanged start folded, and only when the report has a text change.
func TestTextDiffForFoldsTheUnchangedPart(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := range 40 {
		lines = append(lines, "Abschnitt "+strings.Repeat("x", i))
	}

	full := strings.Join(append(lines, "15. Löschung", "16. Ihre Rechte"), "\n")
	cut := strings.Join(append(lines, "15. Lösch"), "\n")

	base := &model.Result{Text: &model.TextCapture{Text: full}}
	cur := &model.Result{Text: &model.TextCapture{Text: cut}}
	changed := &diff.Report{Changes: []diff.Change{{Type: diff.TextChanged}}}

	got := textDiffFor(changed, base, cur)

	var b strings.Builder
	for _, l := range got {
		b.WriteString(l.Class + ":" + l.Text + "\n")
	}

	want := "skip:37 unchanged line(s)\n" +
		"same:Abschnitt " + strings.Repeat("x", 37) + "\n" +
		"same:Abschnitt " + strings.Repeat("x", 38) + "\n" +
		"same:Abschnitt " + strings.Repeat("x", 39) + "\n" +
		"del:15. Löschung\ndel:16. Ihre Rechte\nadd:15. Lösch\n"
	if b.String() != want {
		t.Errorf("diff:\n%s\nwant:\n%s", b.String(), want)
	}

	if textDiffFor(&diff.Report{}, base, cur) != nil {
		t.Error("a diff without a text change shows the text")
	}

	if textDiffFor(changed, nil, cur) != nil {
		t.Error("a diff with no base shows the text")
	}
}
