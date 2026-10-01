package metrics_test

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/metrics"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Story 1.11, AC16: what sampled scans stored, and could not, is countable,
// with a label set that cannot grow.
func TestBodyCounters(t *testing.T) {
	t.Parallel()

	r := metrics.New("test")

	empty := render(t, r)
	for _, line := range []string{
		`wsaw_bodies_stored_total{kind="response"} 0`,
		`wsaw_bodies_stored_total{kind="request"} 0`,
		`wsaw_bodies_unavailable_total{reason="budget-exhausted"} 0`,
	} {
		if !strings.Contains(empty, line) {
			t.Errorf("a fresh registry does not render %q", line)
		}
	}

	if strings.Contains(empty, `reason="no-body"`) {
		t.Error("an exchange without a body is counted as one the scan failed to keep")
	}

	res := result(true)
	res.BodyCapture = &model.BodyCapture{Sampled: true}
	res.Requests = []model.Request{
		{BodyRef: "body/a", BodyStoredSize: 100},
		{BodyRef: "body/b", BodyStoredSize: 50, RequestBodyRef: "body/c", RequestBodySize: 7},
		{BodyUnavailable: "scan body budget exhausted"},
		{BodyUnavailable: "no body: redirect"},
		{RequestBodyUnavailable: "body not retrievable: gone"},
	}

	r.ScanFinished("site", model.ConsentReject, res)

	out := render(t, r)

	for _, line := range []string{
		`wsaw_bodies_stored_total{kind="response"} 2`,
		`wsaw_bodies_stored_total{kind="request"} 1`,
		`wsaw_body_bytes_stored_total{kind="response"} 150`,
		`wsaw_body_bytes_stored_total{kind="request"} 7`,
		`wsaw_bodies_unavailable_total{reason="budget-exhausted"} 1`,
		`wsaw_bodies_unavailable_total{reason="not-retrievable"} 1`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("metrics do not contain %q", line)
		}
	}

	if strings.Contains(out, `wsaw_bodies_stored_total{kind="response",target`) {
		t.Error("the body counters carry a per-target label")
	}
}
