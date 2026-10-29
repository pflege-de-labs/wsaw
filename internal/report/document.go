package report

import (
	"fmt"
	"html/template"
	"strings"
)

// The human-readable report is built once, as a list of blocks, and then
// rendered as Markdown or as the HTML the PDF is printed from (Story 5.39).
// One builder means a section added to the report reaches both formats, and
// it means page-controlled text stays text: it is a span's content, never
// markup that the renderer would interpret.

// Block kinds. Strings rather than an iota so the HTML template can compare
// them by name.
const (
	kindHeading = "heading"
	kindLines   = "lines"
	kindPara    = "para"
	kindQuote   = "quote"
	kindList    = "list"
	kindTable   = "table"

	// kindImage is a screenshot. Only the PDF has them; Markdown skips it.
	kindImage = "image"
)

// span is a run of text in one style.
type span struct {
	Text string
	Bold bool
	Code bool

	// Untrusted text gets its pipes and newlines escaped in Markdown, where
	// they would otherwise break the table or list it sits in.
	Untrusted bool

	// Exact text is never shortened in the PDF: a scan ID, a digest or a
	// path the reader has to type back is useless cut (Story 5.39, AC12).
	Exact bool

	// PDFText, when set, is what the PDF shows instead of Text, e.g. a byte
	// count in human units where the Markdown keeps the exact figure.
	PDFText string

	// Href makes the span a link in the PDF.
	Href string
}

// item is a list entry, with an optional nested list.
type item struct {
	Spans []span
	Sub   [][]span
}

// block is one heading, paragraph, quote, list or table.
type block struct {
	Kind string

	// Level is a heading's level, 1 to 3.
	Level int

	// Lines holds a paragraph (one line), a quote, or a block of hard-broken
	// lines.
	Lines [][]span

	Items []item

	Cols []column
	Rows [][][]span

	Src template.URL
	Alt string
}

// column is one table column.
type column struct {
	Head string

	// Right aligns the column to the right, for figures.
	Right bool

	// Wide columns hold long, free-form text and take the width the PDF has
	// left; the others are sized to their content and never wrap, so a short
	// word is never split into letters (Story 5.39, AC10).
	Wide bool

	// Lead puts this column, in the PDF, at the top of the cell of the column
	// before it rather than in a cell of its own (AC11).
	Lead bool
}

func narrow(head string) column { return column{Head: head} }
func wide(head string) column   { return column{Head: head, Wide: true} }
func figure(head string) column { return column{Head: head, Right: true} }

// document is a report as blocks.
type document struct {
	Blocks []block

	// Landscape is the index of the first block the PDF prints on landscape
	// pages, through to the end; zero means none, as the title never is
	// (Story 5.39, AC15).
	Landscape int
}

func text(s string) span { return span{Text: s} }
func bold(s string) span { return span{Text: s, Bold: true} }
func code(s string) span { return span{Text: s, Code: true} }

// untrusted marks page-controlled text the Markdown must escape.
func untrusted(s string) span { return span{Text: s, Untrusted: true} }

func textf(format string, args ...any) span { return text(fmt.Sprintf(format, args...)) }

func (d *document) heading(level int, s string) {
	d.Blocks = append(d.Blocks, block{Kind: kindHeading, Level: level, Lines: [][]span{{text(s)}}})
}

func (d *document) para(spans ...span) {
	d.Blocks = append(d.Blocks, block{Kind: kindPara, Lines: [][]span{spans}})
}

func (d *document) lines(lines ...[]span) {
	d.Blocks = append(d.Blocks, block{Kind: kindLines, Lines: lines})
}

func (d *document) quote(lines ...[]span) {
	d.Blocks = append(d.Blocks, block{Kind: kindQuote, Lines: lines})
}

func (d *document) list(items ...item) {
	if len(items) == 0 {
		return
	}

	d.Blocks = append(d.Blocks, block{Kind: kindList, Items: items})
}

// codeList is a list of code spans, the shape most of the report's host
// lists take.
func (d *document) codeList(values []string) {
	items := make([]item, 0, len(values))
	for _, v := range values {
		items = append(items, item{Spans: []span{code(v)}})
	}

	d.list(items...)
}

func (d *document) table(cols []column, rows [][][]span) {
	d.Blocks = append(d.Blocks, block{Kind: kindTable, Cols: cols, Rows: rows})
}

// landscape starts the PDF's landscape pages at the next block.
func (d *document) landscape() { d.Landscape = len(d.Blocks) }

func spansText(spans []span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.Text)
	}

	return b.String()
}

// markdown renders the document as Markdown.
func (d *document) markdown() string {
	var b strings.Builder

	for _, blk := range d.Blocks {
		switch blk.Kind {
		case kindHeading:
			b.WriteString(strings.Repeat("#", blk.Level) + " " + mdSpans(blk.Lines[0]) + "\n\n")

		case kindPara:
			b.WriteString(mdSpans(blk.Lines[0]) + "\n\n")

		case kindLines:
			b.WriteString(mdJoin(blk.Lines, "  \n") + "\n\n")

		case kindQuote:
			b.WriteString("> " + mdJoin(blk.Lines, "\n> ") + "\n\n")

		case kindList:
			for _, it := range blk.Items {
				b.WriteString("- " + mdSpans(it.Spans) + "\n")

				for _, sub := range it.Sub {
					b.WriteString("  - " + mdSpans(sub) + "\n")
				}
			}

			b.WriteString("\n")

		case kindTable:
			mdTable(&b, blk)
		}
	}

	return b.String()
}

func mdTable(b *strings.Builder, blk block) {
	heads := make([]string, len(blk.Cols))
	for i, c := range blk.Cols {
		heads[i] = c.Head
	}

	b.WriteString("| " + strings.Join(heads, " | ") + " |\n|")

	for _, c := range blk.Cols {
		if c.Right {
			b.WriteString("---:|")
		} else {
			b.WriteString("---|")
		}
	}

	b.WriteString("\n")

	for _, row := range blk.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = mdSpans(c)
		}

		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}

	b.WriteString("\n")
}

func mdJoin(lines [][]span, sep string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = mdSpans(l)
	}

	return strings.Join(out, sep)
}

func mdSpans(spans []span) string {
	var b strings.Builder

	for _, s := range spans {
		t := s.Text
		if s.Untrusted {
			t = escapePipes(t)
		}

		switch {
		case s.Bold:
			b.WriteString("**" + t + "**")
		case s.Code:
			b.WriteString("`" + t + "`")
		default:
			b.WriteString(t)
		}
	}

	return b.String()
}

// escapePipes keeps a detail string from breaking the Markdown table it sits
// in. Detail text can contain a URL from the scanned page, so it is untrusted.
func escapePipes(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")

	return s
}
