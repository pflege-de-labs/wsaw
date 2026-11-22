package diff_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// The privacy statement the story was written for, complete and cut in the
// middle of a word with its last sections gone.
const (
	privacyFull = "Datenschutz\n1. Verantwortlicher\nweb care LBJ GmbH\n14. Dienste Dritter\n" +
		"YouTube erlaubt das kostenlose Einstellen von Videoclips und deren kostenfreie Ansicht.\n" +
		"15. Löschung\n16. Ihre Rechte\n17. Pflichten\n18. Automatisierte Entscheidungsfindung\nIm Einzelfall nicht."
	privacyCut = "Datenschutz\n1. Verantwortlicher\nweb care LBJ GmbH\n14. Dienste Dritter\n" +
		"YouTube erlaubt das kostenlose Einstellen von Videoclips und deren kostenfre"
)

func withText(r *model.Result, tc model.TextCapture) *model.Result {
	if tc.Selector == "" {
		tc.Selector = "main"
	}

	if tc.Unavailable == "" {
		tc.Length = len([]rune(tc.Text))
		tc.SHA256 = "sha-" + tc.Text
	}

	r.Text = &tc

	return r
}

func textOpts(checks *diff.TextChecks) diff.Options {
	return diff.Options{Text: checks}
}

func changesOf(rep *diff.Report, typ diff.ChangeType) []diff.Change {
	var out []diff.Change

	for _, c := range rep.Changes {
		if c.Type == typ {
			out = append(out, c)
		}
	}

	return out
}

func TestNormalizeText(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"a  b\n\n  c\t\td  \n":      "a b\nc d",
		" x  y":                     "x y",
		"line\r\nnext":              "line\nnext",
		"":                          "",
		"   \n\t\n":                 "",
		"Ihre Rechte\nIhre  Rechte": "Ihre Rechte\nIhre Rechte",
	} {
		if got := diff.NormalizeText(in); got != want {
			t.Errorf("NormalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Story 4.15, AC6 and AC7: whitespace is not a change, wording is, and the
// change carries sizes and digests but never the text (Tenet 9).
func TestTextChanged(t *testing.T) {
	t.Parallel()

	base := withText(result(model.ConsentReject), model.TextCapture{Text: "Ihre Rechte\nAuskunft und Löschung"})

	for _, tc := range []struct {
		name    string
		current model.TextCapture
		want    bool
		detail  string
	}{
		{"identical", model.TextCapture{Text: "Ihre Rechte\nAuskunft und Löschung"}, false, ""},
		{"whitespace only", model.TextCapture{Text: "  Ihre   Rechte\n\n Auskunft und Löschung  "}, false, ""},
		{"wording", model.TextCapture{Text: "Ihre Rechte\nAuskunft"}, true, "text 33 → 20 characters"},
		{"unreadable now", model.TextCapture{Unavailable: "the selector matched nothing"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cur := withText(result(model.ConsentReject), tc.current)
			got := changesOf(diff.Compare(base, cur, diff.Options{}), diff.TextChanged)

			if (len(got) == 1) != tc.want {
				t.Fatalf("text-changed = %+v, want reported %v", got, tc.want)
			}

			if !tc.want {
				return
			}

			c := got[0]
			if c.Severity != diff.SeverityMedium || c.Subject != "main" || c.Detail != tc.detail {
				t.Errorf("change = %+v", c)
			}

			if strings.Contains(c.Detail, "Rechte") {
				t.Errorf("the detail carries page text: %q", c.Detail)
			}

			if c.Before == "" || c.After == "" || c.Before == c.After {
				t.Errorf("digests = %q → %q", c.Before, c.After)
			}
		})
	}
}

// Story 4.15, AC14: a changed link target is a change even when no visible
// character changed; a link that only moved is not.
func TestTextLinkChanges(t *testing.T) {
	t.Parallel()

	contact := model.TextLink{Text: "Datenschutz-Kontakt", Href: "mailto:a@x.test", Target: "mailto:a@x.test"}
	press := model.TextLink{Text: "Presse", Href: "/presse/", Target: "https://x.test/presse/"}
	moved := contact
	moved.Target = "mailto:b@x.test"

	base := withText(result(model.ConsentNone), model.TextCapture{Text: "Kontakt", Links: []model.TextLink{contact, press}})

	same := withText(result(model.ConsentNone), model.TextCapture{Text: "Kontakt", Links: []model.TextLink{press, contact}})
	if got := changesOf(diff.Compare(base, same, diff.Options{}), diff.TextChanged); len(got) != 0 {
		t.Errorf("reordered links reported: %+v", got)
	}

	retarget := withText(result(model.ConsentNone), model.TextCapture{Text: "Kontakt", Links: []model.TextLink{moved, press}})

	got := changesOf(diff.Compare(base, retarget, diff.Options{}), diff.TextChanged)
	if len(got) != 1 || got[0].Detail != "text unchanged, 7 characters; 1 link(s) changed" {
		t.Fatalf("retargeted link = %+v", got)
	}
	// With both texts stored, the change names the artifacts (AC7).
	base.Text.Ref, retarget.Text.Ref = "text/a", "text/b"

	got = changesOf(diff.Compare(base, retarget, diff.Options{}), diff.TextChanged)
	if len(got) != 1 || !strings.HasSuffix(got[0].Detail, "; stored as text/a and text/b") {
		t.Fatalf("stored texts = %+v", got)
	}
}

// Story 4.15, AC8 and AC11: completeness needs no baseline, names every
// reason, and covers a cut text, a contents list longer than the body, a
// missing region and a short text.
func TestTextIncomplete(t *testing.T) {
	t.Parallel()

	checks := &diff.TextChecks{MustContain: []string{"18. Automatisierte Entscheidungsfindung"}, MinLength: 100, CheckAnchors: true}
	toc := []model.TextFragment{{Href: "#link14", Found: true}, {Href: "#link15"}, {Href: "#link16"}, {Href: "#link15"}}

	for _, tc := range []struct {
		name   string
		cur    *model.Result
		checks *diff.TextChecks
		want   string
	}{
		{"complete", withText(result(model.ConsentReject), model.TextCapture{Text: privacyFull}), checks, ""},
		{
			"cut in the middle of a word",
			withText(result(model.ConsentReject), model.TextCapture{Text: privacyCut, Fragments: toc}), checks,
			`missing "18. Automatisierte Entscheidungsfindung"; in-page links to nothing: "#link15", "#link16"`,
		},
		{
			"anchors alone, nothing written down",
			withText(result(model.ConsentReject), model.TextCapture{Text: privacyCut, Fragments: toc}),
			&diff.TextChecks{CheckAnchors: true}, `in-page links to nothing: "#link15", "#link16"`,
		},
		{
			"anchors off",
			withText(result(model.ConsentReject), model.TextCapture{Text: privacyCut, Fragments: toc}),
			&diff.TextChecks{}, "",
		},
		{
			"region absent",
			withText(result(model.ConsentReject), model.TextCapture{Unavailable: "the selector matched nothing"}), checks,
			"no text: the selector matched nothing",
		},
		{
			"too short",
			withText(result(model.ConsentReject), model.TextCapture{Text: "web care LBJ GmbH"}),
			&diff.TextChecks{MinLength: 100}, "17 characters, fewer than the minimum of 100",
		},
		{
			"imprint line removed",
			withText(result(model.ConsentNone), model.TextCapture{Text: "web care LBJ GmbH\nUSt-IdNr: DE276352378"}),
			&diff.TextChecks{MustContain: []string{"web care LBJ GmbH", "HRB  117601", "DE276352378"}}, `missing "HRB  117601"`,
		},
		{"no checks configured", withText(result(model.ConsentReject), model.TextCapture{Text: privacyCut}), nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// No baseline at all: the first scan must already say so.
			got := changesOf(diff.Compare(nil, tc.cur, textOpts(tc.checks)), diff.TextIncomplete)

			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("reported %+v", got)
				}

				return
			}

			if len(got) != 1 || got[0].Detail != tc.want || got[0].Severity != diff.SeverityHigh || got[0].Subject != "main" {
				t.Fatalf("text-incomplete = %+v, want detail %q", got, tc.want)
			}

			// And again against a baseline of the same cut text: approving a
			// cut text does not make it complete.
			again := changesOf(diff.Compare(tc.cur, tc.cur, textOpts(tc.checks)), diff.TextIncomplete)
			if len(again) != 1 {
				t.Errorf("against itself = %+v", again)
			}
		})
	}
}

// A scan that never loaded the page is degraded; it is not also an
// incomplete text.
func TestTextIncompleteNotForAFailedScan(t *testing.T) {
	t.Parallel()

	cur := withText(result(model.ConsentReject), model.TextCapture{Unavailable: "not read: the page did not load"})
	cur.Termination = model.TermError
	cur.Error = "navigating: net::ERR_NAME_NOT_RESOLVED"

	rep := diff.Compare(nil, cur, textOpts(&diff.TextChecks{CheckAnchors: true}))
	if got := changesOf(rep, diff.TextIncomplete); len(got) != 0 {
		t.Errorf("text-incomplete on a failed scan: %+v", got)
	}

	if got := changesOf(rep, diff.ScanDegraded); len(got) != 1 {
		t.Errorf("scan-degraded = %+v", got)
	}
}

// Story 4.15, AC8: an incomplete text keeps being reported, scan after scan.
func TestFlapSuppressorKeepsIncompleteText(t *testing.T) {
	t.Parallel()

	f := diff.NewFlapSuppressor(time.Hour)
	c := diff.Change{Type: diff.TextIncomplete, Target: "site", Subject: "main"}
	now := time.Now()

	for i := range 3 {
		emit, _ := f.Filter(now.Add(time.Duration(i)*time.Minute), []diff.Change{c})
		if len(emit) != 1 {
			t.Fatalf("scan %d: incomplete text suppressed", i)
		}
	}
}

func TestTextDiff(t *testing.T) {
	t.Parallel()

	render := func(lines []diff.TextLine) string {
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(string(l.Op) + l.Text + "\n")
		}

		return b.String()
	}

	cut := render(diff.TextDiff("a\nb\nc\nd", "a\nb\nc kurz"))
	if cut != " a\n b\n-c\n-d\n+c kurz\n" {
		t.Errorf("cut text:\n%s", cut)
	}

	middle := render(diff.TextDiff("a\nb\nc\nd\ne", "a\nx\nc\nd\ny\ne"))
	if middle != " a\n-b\n+x\n c\n d\n+y\n e\n" {
		t.Errorf("middle:\n%s", middle)
	}

	if got := render(diff.TextDiff("same  text", "same text")); got != " same text\n" {
		t.Errorf("whitespace:\n%s", got)
	}

	// Beyond the work bound the middle is shown removed, then added.
	var a, b []string
	for i := range 2100 {
		a = append(a, "a"+strings.Repeat("x", i%7)+string(rune('A'+i%26)))
		b = append(b, "b"+strings.Repeat("y", i%5)+string(rune('a'+i%26)))
	}

	big := diff.TextDiff(strings.Join(a, "\n"), strings.Join(b, "\n"))
	if len(big) != len(a)+len(b) || big[0].Op != diff.LineRemoved || big[len(big)-1].Op != diff.LineAdded {
		t.Errorf("large diff: %d lines, first %c, last %c", len(big), big[0].Op, big[len(big)-1].Op)
	}
}
