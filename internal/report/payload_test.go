package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// Story 1.11, AC14: the HAR carries request payloads, and says what the scan's
// sampling decision was.

type harDoc struct {
	Log struct {
		Comment string `json:"comment"`
		Entries []struct {
			Request struct {
				URL      string `json:"url"`
				BodySize int64  `json:"bodySize"`
				PostData *struct {
					MimeType string `json:"mimeType"`
					Text     string `json:"text"`
					Comment  string `json:"comment"`
					Params   []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"params"`
				} `json:"postData"`
			} `json:"request"`
		} `json:"entries"`
	} `json:"log"`
}

func writeHAR(t *testing.T, res *model.Result, load report.BodyLoader) harDoc {
	t.Helper()

	var b bytes.Buffer
	if err := report.WriteHAR(&b, res, load); err != nil {
		t.Fatal(err)
	}

	var doc harDoc
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("the HAR does not parse: %v", err)
	}

	return doc
}

func payloadResult() *model.Result {
	return &model.Result{
		Target: "site", URL: "https://example.com/", StartedAt: time.Unix(0, 0),
		BodyCapture: &model.BodyCapture{
			Store: model.BodyStoreAll, Ratio: 0.05, RatioWindow: 168 * time.Hour,
			Sampled: true, Decision: model.BodyDecisionSampled, WindowScans: 40, WindowSampled: 2,
		},
		Requests: []model.Request{
			{
				URL: "https://tracker.test/collect", Method: "POST",
				RequestBodyRef: "body/form", RequestBodySize: 13,
				RequestBodyMimeType: "application/x-www-form-urlencoded; charset=UTF-8",
			},
			{
				URL: "https://tracker.test/bin", Method: "POST",
				RequestBodyRef: "body/bin", RequestBodySize: 3, RequestBodyMimeType: "application/octet-stream",
			},
			{
				URL: "https://tracker.test/gone", Method: "POST",
				RequestBodyRef: "body/missing", RequestBodyMimeType: "text/plain",
			},
		},
	}
}

func payloadLoader(ref string) ([]byte, error) {
	switch ref {
	case "body/form":
		return []byte("uid=v-42&ev=pv"), nil
	case "body/bin":
		return []byte{0x00, 0xff, 0x01}, nil
	default:
		return nil, errors.New("not in the bucket")
	}
}

func TestTheHARCarriesRequestPayloads(t *testing.T) {
	t.Parallel()

	doc := writeHAR(t, payloadResult(), payloadLoader)

	form := doc.Log.Entries[0].Request
	if form.PostData == nil || form.PostData.Text != "uid=v-42&ev=pv" {
		t.Fatalf("form payload = %+v", form.PostData)
	}

	if form.BodySize != 13 {
		t.Errorf("request bodySize = %d, want the payload's size", form.BodySize)
	}

	if len(form.PostData.Params) != 2 || form.PostData.Params[0].Name != "ev" || form.PostData.Params[1].Value != "v-42" {
		t.Errorf("form params = %+v, want ev and uid, sorted", form.PostData.Params)
	}

	bin := doc.Log.Entries[1].Request.PostData
	if bin == nil || bin.Text != "AP8B" || !strings.Contains(bin.Comment, "base64") {
		t.Errorf("binary payload = %+v, want base64 and a comment saying so", bin)
	}

	gone := doc.Log.Entries[2].Request.PostData
	if gone == nil || !strings.Contains(gone.Comment, "could not be read") {
		t.Errorf("a payload the bucket lost = %+v, want the loss stated", gone)
	}

	if !strings.Contains(doc.Log.Comment, "in the body sample") || !strings.Contains(doc.Log.Comment, "2 of 40 scans") {
		t.Errorf("the HAR comment does not state the sampling decision: %q", doc.Log.Comment)
	}
}

// A scan outside the sample says so, so a HAR without bodies is not read as
// a broken export.
func TestTheHARSaysAScanWasNotSampled(t *testing.T) {
	t.Parallel()

	res := payloadResult()
	res.Requests = nil
	res.BodyCapture.Sampled = false
	res.BodyCapture.Decision = model.BodyDecisionNotSampled

	doc := writeHAR(t, res, payloadLoader)

	if !strings.Contains(doc.Log.Comment, "not in the body sample") || !strings.Contains(doc.Log.Comment, "by configuration") {
		t.Errorf("comment = %q", doc.Log.Comment)
	}
}

// The export copy carries the payload; the stored document does not grow.
func TestWithBodiesInlinesPayloadsIntoACopy(t *testing.T) {
	t.Parallel()

	res := payloadResult()
	out := report.WithBodies(res, payloadLoader)

	if out.Requests[0].RequestBody != "uid=v-42&ev=pv" {
		t.Errorf("exported payload = %q", out.Requests[0].RequestBody)
	}

	if res.Requests[0].RequestBody != "" {
		t.Error("inlining a payload changed the stored document")
	}
}
