package model_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// schemaNode is the part of a JSON Schema object this test reads.
type schemaNode struct {
	Ref        string                `json:"$ref"`
	Required   []string              `json:"required"`
	Properties map[string]schemaNode `json:"properties"`
	Enum       []string              `json:"enum"`
	Items      *schemaNode           `json:"items"`
	Defs       map[string]schemaNode `json:"$defs"`
}

// Story 5.35, AC7: the published schema forbids unknown properties, so every
// key a scored result writes must be declared there or every new document
// fails validation. This checks exactly the confidence object's keys and
// values against the schema, with nothing but the standard library: wsaw has
// no JSON Schema validator and does not take a dependency for one test.
func TestTheSchemaDeclaresEverythingAConfidenceWrites(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../docs/result.schema.json")
	if err != nil {
		t.Fatalf("reading the published schema: %v", err)
	}

	var root schemaNode
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decoding the published schema: %v", err)
	}

	res := model.Result{
		SchemaVersion: model.SchemaVersion,
		Confidence: &model.Confidence{
			Score: 70,
			Band:  model.ConfidenceMedium,
			Reasons: []model.ConfidenceReason{
				{Signal: model.SignalTermination, Points: 30, Observed: "timeout"},
				{Signal: model.SignalFetches, Observed: "all 3 observed"},
				{Signal: model.SignalDuration, Observed: "2s", Reference: "median 2s over 5 earlier scans"},
				{Signal: model.SignalConsent, Observed: "applied", NotAssessed: "never, but every key is written here"},
			},
			DurationReference: &model.DurationReference{Median: 2 * time.Second, Scans: 5},
		},
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}

	prop, ok := root.Properties["confidence"]
	if !ok {
		t.Fatal("the schema's root declares no confidence property")
	}

	conf := resolve(t, root, prop)
	assertDeclared(t, root, "confidence", conf, doc["confidence"])

	var byKey map[string]json.RawMessage
	if err := json.Unmarshal(doc["confidence"], &byKey); err != nil {
		t.Fatal(err)
	}

	assertDeclared(t, root, "durationReference", conf.Properties["durationReference"], byKey["durationReference"])
	assertEnum(t, "band", conf.Properties["band"], byKey["band"])

	var reasons []json.RawMessage
	if err := json.Unmarshal(byKey["reasons"], &reasons); err != nil {
		t.Fatal(err)
	}

	item := resolve(t, root, *conf.Properties["reasons"].Items)

	for _, r := range reasons {
		assertDeclared(t, root, "reason", item, r)

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(r, &fields); err != nil {
			t.Fatal(err)
		}

		assertEnum(t, "signal", item.Properties["signal"], fields["signal"])
	}

	if model.SchemaVersion != "2.1" {
		t.Errorf("SchemaVersion = %s; the confidence object is what 2.1 added", model.SchemaVersion)
	}
}

func resolve(t *testing.T, root, n schemaNode) schemaNode {
	t.Helper()

	if n.Ref == "" {
		return n
	}

	const prefix = "#/$defs/"

	def, ok := root.Defs[n.Ref[len(prefix):]]
	if !ok {
		t.Fatalf("the schema references %s, which it does not define", n.Ref)
	}

	return def
}

func assertDeclared(t *testing.T, root schemaNode, what string, n schemaNode, value json.RawMessage) {
	t.Helper()

	n = resolve(t, root, n)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		t.Fatalf("%s is not an object: %s", what, value)
	}

	for key := range fields {
		if _, ok := n.Properties[key]; !ok {
			t.Errorf("%s writes %q, which the schema does not declare", what, key)
		}
	}

	for _, key := range n.Required {
		if _, ok := fields[key]; !ok {
			t.Errorf("%s omits %q, which the schema requires", what, key)
		}
	}
}

func assertEnum(t *testing.T, what string, n schemaNode, value json.RawMessage) {
	t.Helper()

	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		t.Fatalf("%s is not a string: %s", what, value)
	}

	if !slices.Contains(n.Enum, s) {
		t.Errorf("%s %q is not in the schema's enum %v", what, s, n.Enum)
	}
}

// Every band and signal the code can write is one the schema allows.
func TestTheSchemaEnumsMatchTheModel(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../docs/result.schema.json")
	if err != nil {
		t.Fatal(err)
	}

	var root schemaNode
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}

	bands := root.Defs["confidence"].Properties["band"].Enum
	for _, b := range []model.ConfidenceBand{
		model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow, model.ConfidenceNone,
	} {
		if !slices.Contains(bands, string(b)) {
			t.Errorf("band %s is missing from the schema", b)
		}
	}

	signals := root.Defs["confidenceReason"].Properties["signal"].Enum
	for _, s := range []model.ConfidenceSignal{
		model.SignalTermination, model.SignalFetches, model.SignalDuration, model.SignalConsent,
	} {
		if !slices.Contains(signals, string(s)) {
			t.Errorf("signal %s is missing from the schema", s)
		}
	}
}
