package httpapi

import (
	"fmt"
	"slices"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// textDiffContext is how many unchanged lines are kept around a change.
const textDiffContext = 3

// textDiffLine is one line of the scan page's text diff. Text is page data
// and is only ever rendered through html/template, escaped (Tenet 9).
type textDiffLine struct {
	// Class is "add", "del", "same" or "skip".
	Class string
	Mark  string
	Text  string
}

// textDiffFor is the line diff of the watched text, shown when the report
// has a text-changed change and both sides were read (Story 4.15, AC7).
// Unchanged stretches are folded to a line saying how long they are, so a
// cut at the end of a long statement shows the cut and not the statement.
func textDiffFor(rep *diff.Report, base, cur *model.Result) []textDiffLine {
	if rep == nil || base == nil || base.Text == nil || cur.Text == nil ||
		!slices.ContainsFunc(rep.Changes, func(c diff.Change) bool { return c.Type == diff.TextChanged }) {
		return nil
	}

	lines := diff.TextDiff(diff.TextWithLinks(base.Text), diff.TextWithLinks(cur.Text))

	keep := make([]bool, len(lines))

	for i, l := range lines {
		if l.Op == diff.LineSame {
			continue
		}

		for j := max(0, i-textDiffContext); j <= min(len(lines)-1, i+textDiffContext); j++ {
			keep[j] = true
		}
	}

	var out []textDiffLine

	for i := 0; i < len(lines); {
		if !keep[i] {
			j := i
			for j < len(lines) && !keep[j] {
				j++
			}

			out = append(out, textDiffLine{Class: "skip", Mark: "…", Text: fmt.Sprintf("%d unchanged line(s)", j-i)})
			i = j

			continue
		}

		l := lines[i]

		switch l.Op {
		case diff.LineAdded:
			out = append(out, textDiffLine{Class: "add", Mark: "+", Text: l.Text})
		case diff.LineRemoved:
			out = append(out, textDiffLine{Class: "del", Mark: "−", Text: l.Text})
		default:
			out = append(out, textDiffLine{Class: "same", Mark: " ", Text: l.Text})
		}

		i++
	}

	return out
}
