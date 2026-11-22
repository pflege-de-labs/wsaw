package model_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 4.15, AC4: the schema forbids unknown properties, so every key a text
// capture writes must be declared, with the right required keys.
func TestTheSchemaDeclaresEverythingATextCaptureWrites(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../docs/result.schema.json")
	if err != nil {
		t.Fatalf("reading the published schema: %v", err)
	}

	var root schemaNode
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decoding the published schema: %v", err)
	}

	if root.Properties["text"].Ref != "#/$defs/textCapture" {
		t.Fatalf("text is not declared as a textCapture: %+v", root.Properties["text"])
	}

	full := model.TextCapture{
		Selector: "main", Unavailable: "x", Ref: "text/ab", Bytes: 3, Length: 1, SHA256: "s", TruncatedFrom: 2,
		Fragments: []model.TextFragment{{Href: "#a", Found: true}},
	}
	stored := model.TextArtifact{Text: "t", Links: []model.TextLink{{Text: "a", Href: "/a", Target: "https://site.test/a"}}}

	for def, v := range map[string]any{
		"textCapture": full, "textArtifact": stored, "textLink": stored.Links[0], "textFragment": full.Fragments[0],
	} {
		node, ok := root.Defs[def]
		if !ok {
			t.Errorf("$defs.%s is missing", def)

			continue
		}

		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}

		var keys map[string]json.RawMessage
		if err := json.Unmarshal(b, &keys); err != nil {
			t.Fatal(err)
		}

		for k := range keys {
			if _, ok := node.Properties[k]; !ok {
				t.Errorf("%s writes %q, which the schema does not declare", def, k)
			}
		}

		for _, k := range node.Required {
			if _, ok := keys[k]; !ok {
				t.Errorf("%s: the schema requires %q, which a capture does not write", def, k)
			}
		}
	}

	// A text that could not be read still writes the required keys.
	b, _ := json.Marshal(model.TextCapture{Selector: "main", Unavailable: "selector matched nothing"})

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}

	for _, k := range root.Defs["textCapture"].Required {
		if _, ok := keys[k]; !ok {
			t.Errorf("an unavailable text omits required %q", k)
		}
	}

	if model.SchemaVersion < "2.5" {
		t.Errorf("SchemaVersion = %s; text is what 2.5 added", model.SchemaVersion)
	}
}
