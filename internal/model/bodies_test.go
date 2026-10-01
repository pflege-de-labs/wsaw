package model_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 1.11, AC4: the sampling rule, as a pure function of the window counts.
func TestSampleWanted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		ratio          float64
		scans, sampled int
		want           bool
	}{
		{"ratio zero never samples", 0, 1, 0, false},
		{"ratio one always samples", 1, 50, 50, true},
		{"first scan of an empty window is sampled", 0.05, 1, 0, true},
		{"one sampled scan covers the next nineteen", 0.05, 20, 1, false},
		{"the twenty-first scan calls for a second", 0.05, 21, 1, true},
		{"an integer product does not round past itself", 0.05, 40, 2, false},
		{"half samples every other scan", 0.5, 2, 1, false},
		{"half samples the third", 0.5, 3, 1, true},
		{"a tiny ratio still gets one per window", 0.0001, 3, 0, true},
		{"and only one", 0.0001, 3, 1, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := model.SampleWanted(c.ratio, c.scans, c.sampled); got != c.want {
				t.Errorf("SampleWanted(%v, %d, %d) = %v, want %v", c.ratio, c.scans, c.sampled, got, c.want)
			}
		})
	}
}

// Sampling spaces scans out rather than clustering them: an hourly series at
// 0.05 over a week samples ceil(0.05 × 168) = 9 of its 168 scans, never two
// in a row once the first is taken.
func TestSampleWantedSpacesAFullWindow(t *testing.T) {
	t.Parallel()

	var (
		decisions []bool
		sampled   int
	)

	for scan := 1; scan <= 168; scan++ {
		take := model.SampleWanted(0.05, scan, sampled)
		if take {
			sampled++
		}

		decisions = append(decisions, take)
	}

	if sampled != 9 {
		t.Errorf("sampled %d of 168, want 9", sampled)
	}

	for i := 1; i < len(decisions); i++ {
		if decisions[i] && decisions[i-1] {
			t.Errorf("scans %d and %d were both sampled; a ratio of 0.05 must not cluster", i, i+1)
		}
	}
}

func TestBodyReasonCode(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"no body: redirect":                         "no-body",
		"body not retrievable: No resource with id": "not-retrievable",
		"body larger than the 8388608 byte cap":     "too-large",
		"scan body budget exhausted":                "budget-exhausted",
		"scan ended before the body was read":       "scan-ended",
		"body queue full":                           "queue-full",
		"storing the body failed: disk full":        "store-failed",
		"stored body could not be read: gone":       "other",
	}

	for text, want := range cases {
		if got := model.BodyReasonCode(text); got != want {
			t.Errorf("BodyReasonCode(%q) = %q, want %q", text, got, want)
		}

		if !slices.Contains(model.BodyReasonCodes(), want) {
			t.Errorf("%q is not in BodyReasonCodes", want)
		}
	}
}

func TestBodyCaptureInherited(t *testing.T) {
	t.Parallel()

	var none *model.BodyCapture
	if none.Inherited("a") {
		t.Error("a nil capture cannot be inherited")
	}

	bc := &model.BodyCapture{DecidedBy: "first"}
	if bc.Inherited("first") {
		t.Error("the attempt that decided did not inherit")
	}

	if !bc.Inherited("second") {
		t.Error("a later attempt inherited the decision")
	}
}

// Story 1.11, AC6: every key a body capture and a request payload write is
// declared by the published schema, which forbids unknown properties.
func TestTheSchemaDeclaresEverythingABodyCaptureWrites(t *testing.T) {
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
		BodyCapture: &model.BodyCapture{
			Store: model.BodyStoreAll, Ratio: 0.05, RatioWindow: 168 * time.Hour, RequestBodies: true,
			Sampled: true, Forced: true, Decision: model.BodyDecisionForced, DecidedBy: "scan-1",
			WindowScans: 20, WindowSampled: 1, MaxBodyBytes: 1, MaxScanBytes: 2,
			ResponseBodiesStored: 3, ResponseBodyBytes: 4, RequestBodiesStored: 5, RequestBodyBytes: 6,
			BodiesUnavailable: 7, BudgetExhausted: true,
		},
		Requests: []model.Request{{
			RequestBodyRef: "body/aa", RequestBodySHA256: "aa", RequestBodySize: 2,
			RequestBodyMimeType: "application/json", RequestBodyUnavailable: "no body",
			RequestBodyRedacted: true, RequestBody: "{}", RequestBodyEncoding: "base64",
		}},
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}

	assertDeclared(t, root, "bodyCapture", root.Properties["bodyCapture"], doc["bodyCapture"])

	var byKey map[string]json.RawMessage
	if err := json.Unmarshal(doc["bodyCapture"], &byKey); err != nil {
		t.Fatal(err)
	}

	bc := resolve(t, root, root.Properties["bodyCapture"])
	assertEnum(t, "store", bc.Properties["store"], byKey["store"])
	assertEnum(t, "decision", bc.Properties["decision"], byKey["decision"])

	var requests []json.RawMessage
	if err := json.Unmarshal(doc["requests"], &requests); err != nil {
		t.Fatal(err)
	}

	request := root.Defs["request"]
	for key := range mustObject(t, requests[0]) {
		if _, ok := request.Properties[key]; !ok {
			t.Errorf("a request writes %q, which the schema does not declare", key)
		}
	}

	for _, d := range []model.BodyDecision{
		model.BodyDecisionDisabled, model.BodyDecisionSampled, model.BodyDecisionNotSampled,
		model.BodyDecisionForced, model.BodyDecisionLedgerUnavailable, model.BodyDecisionDisabledByReload,
	} {
		if !slices.Contains(bc.Properties["decision"].Enum, string(d)) {
			t.Errorf("decision %s is missing from the schema", d)
		}
	}

	if model.SchemaVersion < "2.2" {
		t.Errorf("SchemaVersion = %s; bodyCapture is what 2.2 added", model.SchemaVersion)
	}
}

func mustObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()

	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not an object: %s", raw)
	}

	return out
}
