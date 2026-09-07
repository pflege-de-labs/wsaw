package report_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// A body that storeBodies kept lives outside the result document, so an
// export that does not resolve the reference names bodies nothing in it can
// reach. These tests are about the export carrying them.

const scriptBody = "(function(){window.tracker='on';})();\n"

// errUnreadable stands in for a body the store cannot produce — evicted,
// deleted, or on a volume that went away.
var errUnreadable = errors.New("artifact not found")

func withStoredBody() *model.Result {
	res := fixture()

	for i := range res.Requests {
		if res.Requests[i].ResourceType == "script" {
			res.Requests[i].BodySHA256 = "abc123"
			res.Requests[i].BodyRef = "body/abc123"
			res.Requests[i].DecodedSize = int64(len(scriptBody))

			return res
		}
	}

	// The fixture has no script; give the first request a body instead, so
	// this helper cannot silently return a result with nothing to load.
	res.Requests[0].BodySHA256 = "abc123"
	res.Requests[0].BodyRef = "body/abc123"
	res.Requests[0].DecodedSize = int64(len(scriptBody))

	return res
}

func loadFixed(body string) report.BodyLoader {
	return func(string) ([]byte, error) { return []byte(body), nil }
}

func TestTheHARCarriesStoredBodies(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteHAR(&b, withStoredBody(), loadFixed(scriptBody)); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Log struct {
			Comment string `json:"comment"`
			Entries []struct {
				Response struct {
					Content struct {
						Text     string `json:"text"`
						Encoding string `json:"encoding"`
						Size     int64  `json:"size"`
						Comment  string `json:"comment"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}

	if err := json.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatalf("the HAR is not valid JSON: %v", err)
	}

	var found bool

	for _, e := range doc.Log.Entries {
		if e.Response.Content.Text == scriptBody {
			found = true

			if e.Response.Content.Encoding != "" {
				t.Errorf("a text body was encoded as %q; text is what a viewer shows and a diff compares",
					e.Response.Content.Encoding)
			}
		}
	}

	if !found {
		t.Fatalf("no entry carries the stored body:\n%s", b.String())
	}

	if !strings.Contains(doc.Log.Comment, "bodies are included") {
		t.Errorf("the HAR does not say that it carries bodies: %q", doc.Log.Comment)
	}
}

// Without a loader the HAR has no bodies — and has to say so, or a reader
// cannot tell "the site served nothing" from "wsaw did not keep it"
// (Tenet 5).
func TestAHARWithoutBodiesSaysSo(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteHAR(&b, withStoredBody(), nil); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(b.String(), scriptBody) {
		t.Error("a body appeared without a loader")
	}

	if !strings.Contains(b.String(), "not included in this export") {
		t.Error("the HAR does not disclose that bodies are missing")
	}
}

// A scan with body storage off is a different case again, and must not read
// as an export that dropped them.
func TestAHARForAScanWithNoStoredBodiesSaysThatInstead(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteHAR(&b, fixture(), loadFixed(scriptBody)); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(b.String(), "No response bodies were stored") {
		t.Error("the HAR does not distinguish a scan that stored no bodies from an export that omitted them")
	}
}

// Binary bodies have to survive, which JSON strings alone cannot do.
func TestABinaryBodyIsBase64Encoded(t *testing.T) {
	t.Parallel()

	binary := string([]byte{0x89, 'P', 'N', 'G', 0x00, 0x1a, 0xff})

	var b strings.Builder

	if err := report.WriteHAR(&b, withStoredBody(), loadFixed(binary)); err != nil {
		t.Fatal(err)
	}

	want := base64.StdEncoding.EncodeToString([]byte(binary))

	if !strings.Contains(b.String(), want) {
		t.Errorf("the binary body was not base64-encoded into the HAR:\n%s", b.String())
	}

	if !strings.Contains(b.String(), `"encoding": "base64"`) {
		t.Error("the HAR does not mark the body as base64, so a viewer would render bytes as text")
	}
}

// A truncated body must be visibly truncated. Reporting the stored length as
// the response's size would turn a size cap into a false fact about the site.
func TestATruncatedBodyIsDisclosed(t *testing.T) {
	t.Parallel()

	res := withStoredBody()

	for i := range res.Requests {
		if res.Requests[i].BodyRef != "" {
			res.Requests[i].DecodedSize = 10000
		}
	}

	var b strings.Builder

	if err := report.WriteHAR(&b, res, loadFixed(scriptBody)); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(b.String(), "truncated:") {
		t.Errorf("a body shorter than the response it came from is not disclosed:\n%s", b.String())
	}
}

// AC3 of Story 1.6, carried into the export: a body that cannot be read is
// reported, never silently absent.
func TestABodyThatCannotBeReadIsReported(t *testing.T) {
	t.Parallel()

	failing := func(string) ([]byte, error) { return nil, errUnreadable }

	out := report.WithBodies(withStoredBody(), failing)

	var reported bool

	for i := range out.Requests {
		if strings.Contains(out.Requests[i].BodyUnavailable, "could not be read") {
			reported = true
		}
	}

	if !reported {
		t.Error("a body that could not be loaded vanished instead of being reported")
	}
}

// The JSON export carries bodies as well: a downloaded result that names a
// body nobody can reach is not evidence of anything.
func TestTheJSONExportCarriesStoredBodies(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	if err := report.WriteJSON(&b, withStoredBody(), loadFixed(scriptBody)); err != nil {
		t.Fatal(err)
	}

	var res model.Result

	if err := json.Unmarshal([]byte(b.String()), &res); err != nil {
		t.Fatalf("the export is not valid JSON: %v", err)
	}

	var found bool

	for i := range res.Requests {
		if res.Requests[i].Body == scriptBody {
			found = true

			if res.Requests[i].BodyRef == "" {
				t.Error("the export dropped the reference; the body and where it is stored are both facts")
			}

			if res.Requests[i].BodyStoredSize != int64(len(scriptBody)) {
				t.Errorf("stored size = %d, want %d", res.Requests[i].BodyStoredSize, len(scriptBody))
			}
		}
	}

	if !found {
		t.Error("the JSON export carries no body")
	}
}

// Inlining must not mutate the caller's result. In the daemon that result is
// the stored document, and the notifier is about to look at it.
func TestInliningDoesNotTouchTheOriginal(t *testing.T) {
	t.Parallel()

	res := withStoredBody()

	out := report.WithBodies(res, loadFixed(scriptBody))

	if out == res {
		t.Fatal("WithBodies returned the same result rather than a copy")
	}

	for i := range res.Requests {
		if res.Requests[i].Body != "" {
			t.Error("the original result grew a body; anything else holding it would pay for that too")
		}
	}
}

// A result with no stored bodies needs no copy at all.
func TestWithBodiesIsAPassThroughWhenThereIsNothingToLoad(t *testing.T) {
	t.Parallel()

	res := fixture()

	if out := report.WithBodies(res, loadFixed(scriptBody)); out != res {
		t.Error("a result with no stored bodies was copied for nothing")
	}

	if out := report.WithBodies(res, nil); out != res {
		t.Error("a nil loader copied the result")
	}
}
