package capture

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

func TestTruncateUTF8NeverSplitsACharacter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"Löschung", 2, "L"},
		{"Löschung", 3, "Lö"},
		{"abc", 10, "abc"},
		{"€€", 4, "€"},
		{"", 3, ""},
	} {
		got := truncateUTF8(tc.in, tc.n)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// Story 4.15, AC5: a text over text.maxBytes is cut by wsaw, and the result
// says so and how long the region was, so a text wsaw cut is never mistaken
// for one the site cut.
func TestATextOverTheCapSaysWsawCutIt(t *testing.T) {
	t.Parallel()

	s := &session{res: &model.Result{}}
	tc := &model.TextCapture{Selector: "main"}

	s.fillText(tc, textRead{Text: strings.Repeat("ä", 10)}, 7)

	if tc.Text != "äää" || tc.Length != 3 || tc.TruncatedFrom != 10 {
		t.Fatalf("capture = %+v", tc)
	}

	if len(s.res.Warnings) != 1 || !strings.Contains(s.res.Warnings[0], "cut by wsaw") {
		t.Errorf("warnings = %q", s.res.Warnings)
	}

	under := &model.TextCapture{Selector: "main"}
	s.fillText(under, textRead{Text: "Impressum", Links: []model.TextLink{{Text: "Mail", Href: "mailto:a@x.test", Target: "mailto:a@x.test"}}}, 100)

	if under.TruncatedFrom != 0 || under.Length != 9 || under.SHA256 == "" || len(under.Links) != 1 {
		t.Errorf("capture under the cap = %+v", under)
	}
}
