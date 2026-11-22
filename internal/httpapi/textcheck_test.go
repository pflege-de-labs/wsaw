package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/httpapi"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 4.16, AC4-AC6: the scan page shows a model's verdict labelled as
// model output, with its findings escaped; a check that did not run says so
// and is never shown as passed.
func TestTheScanPageShowsTheTextCheckAsModelOutput(t *testing.T) {
	t.Parallel()

	f := newFixture(t, httpapi.Options{WebUI: true}, nil)
	f.seed("scan-1", model.ConsentReject, time.Now(), func(r *model.Result) {
		r.Text = &model.TextCapture{Selector: "main", Length: 9, Check: &model.TextCheck{
			Model: "test-model", Server: "api.example", CheckedAt: time.Now(), Verdict: model.VerdictFailed,
			Findings: []model.TextFinding{
				{Text: `Registergericht fehlt <script>alert(1)</script>`, Deciding: true},
				{Text: "Link-Ziel weicht ab.", Deciding: false},
			},
		}}
	})
	f.seed("scan-2", model.ConsentAccept, time.Now(), func(r *model.Result) {
		r.Text = &model.TextCapture{Selector: "main", Length: 9, Check: &model.TextCheck{
			NotChecked: "the model's answer did not parse: no verdict", Reply: "Bestanden <b>!</b>",
		}}
	})

	html := body(t, f.get("/results/site/reject/scan-1", "Accept", "text/html"))
	section := html[strings.Index(html, `id="text-check"`):]
	section = section[:strings.Index(section, "</section>")]

	for _, want := range []string{
		"model output", "<strong>failed</strong>, 1 deciding finding(s)", "test-model",
		"<strong>deciding</strong>: Registergericht fehlt &lt;script&gt;", "informative: Link-Ziel weicht ab.",
		"not a compliance result",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the text check section does not contain %q:\n%s", want, section)
		}
	}

	if strings.Contains(html, "<script>alert(1)") {
		t.Error("a finding was rendered as markup")
	}

	html = body(t, f.get("/results/site/accept/scan-2", "Accept", "text/html"))
	section = html[strings.Index(html, `id="text-check"`):]
	section = section[:strings.Index(section, "</section>")]

	if !strings.Contains(section, "Not checked.") || !strings.Contains(section, "Bestanden &lt;b&gt;") ||
		strings.Contains(section, "passed") {
		t.Errorf("unchecked section:\n%s", section)
	}
}
