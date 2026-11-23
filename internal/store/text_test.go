package store

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 4.15, AC4: a watched text is its own content-addressed artifact. The
// result names it, retention counts it as the result's, and a comparison
// loads it back; one that has gone is recorded as unavailable, never as an
// empty text (Tenet 5).
func TestAWatchedTextIsAnArtifactTheResultNames(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	data, err := json.Marshal(model.TextArtifact{
		Text:  "Impressum\nHRB 12345",
		Links: []model.TextLink{{Text: "E-Mail", Href: "mailto:a@b.test", Target: "mailto:a@b.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ref, err := s.PutArtifact(model.TextArtifactKind, data)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	if !strings.HasPrefix(ref, "text/") {
		t.Fatalf("ref = %q, want a text/ reference", ref)
	}

	res := &model.Result{Text: &model.TextCapture{Selector: "main", Ref: ref, Bytes: int64(len(data)), Length: 19}}

	refs := artifactRefsOf(res, "")
	if !slices.Contains(refs, artifactRefSize{ref: ref, bytes: int64(len(data))}) {
		t.Errorf("retention does not count the text: %+v", refs)
	}

	// The document does not carry the text, only the reference.
	doc, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(doc), "HRB 12345") || !strings.Contains(string(doc), ref) {
		t.Errorf("document = %s; want the reference and not the text", doc)
	}

	var back model.Result
	if err := json.Unmarshal(doc, &back); err != nil {
		t.Fatal(err)
	}

	LoadText(s, &back)

	if back.Text.Text != "Impressum\nHRB 12345" || len(back.Text.Links) != 1 || back.Text.Unavailable != "" {
		t.Errorf("loaded %+v", back.Text)
	}

	gone := &model.Result{Text: &model.TextCapture{Selector: "main", Ref: "text/" + strings.Repeat("0", 64)}}
	LoadText(s, gone)

	if gone.Text.Unavailable == "" || gone.Text.Text != "" {
		t.Errorf("a missing artifact left %+v; want it recorded as unavailable", gone.Text)
	}
}
