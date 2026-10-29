package report_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/report"
)

// The whole path, template to Chrome, on a fixture result. Skipped, saying
// why, where there is no Chrome (Tenet 13).
func TestPDFRendersInChrome(t *testing.T) {
	t.Parallel()

	if os.Getenv("WSAW_SKIP_BROWSER_TESTS") != "" {
		t.Skip("skipping browser test: WSAW_SKIP_BROWSER_TESTS is set")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	info, err := browser.Discover(ctx, os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("skipping browser test: no usable Chrome found (%v)", err)
	}

	pool := browser.NewPool(browser.PoolOptions{
		Size:   1,
		Launch: browser.Options{Info: info, ProfileDir: t.TempDir(), LaunchTimeout: 40 * time.Second},
	})

	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the pool: %v", err)
		}
	})

	res := fixture()
	res.Confidence = &model.Confidence{Score: 59, Band: model.ConfidenceLow, Reasons: []model.ConfidenceReason{
		{Signal: model.SignalFetches, Points: 41, Observed: "8 of 114 requests not observed"},
	}}
	res.Screenshots = []model.Artifact{{Kind: "screenshot-before-consent", Ref: "screenshot/1"}}

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4" +
		"\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

	var out bytes.Buffer

	err = report.WritePDF(t.Context(), &out, browser.NewPrinter(pool, 0), res, nil, report.PDFOptions{
		Version:     "test",
		Screenshots: func(string) ([]byte, error) { return png, nil },
	})
	if err != nil {
		t.Fatalf("WritePDF: %v", err)
	}

	if !bytes.HasPrefix(out.Bytes(), []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF-")
	}
}
