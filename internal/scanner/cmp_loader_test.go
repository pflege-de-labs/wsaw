package scanner_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// botStubBody is what the fixture "CMP origin" serves in place of its loader,
// shaped like the stub CCM19 serves to clients it takes for bots.
const botStubBody = "'use strict';console.info(\"fixture CMP disabled for bots\");\n"

// TestACMPThatRefusesToRunIsNotReportedAsNoCMP is Story 2.11's end-to-end
// case: the page requests a known CMP's loader, the loader comes back as the
// vendor's bot stub, and no banner ever appears. The scan must say the CMP
// refused to run for this browser, never that the page has no CMP.
func TestACMPThatRefusesToRunIsNotReportedAsNoCMP(t *testing.T) {
	info := requireChrome(t)

	cmpOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(botStubBody))
	}))
	defer cmpOrigin.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte{0, 0, 1, 0})

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>cmp loader fixture</title>
<script src="%s/app.js?apiKey=fixture-key"></script></head>
<body><h1>A page whose CMP never starts</h1></body></html>`, cmpOrigin.URL)
	}))
	defer site.Close()

	digest := sha256.Sum256([]byte(botStubBody))
	rules := loaderRules(t, cmpOrigin.URL+"/app.js", hex.EncodeToString(digest[:]))

	s, closePool := newScannerWithRules(t, info, rules)
	defer closePool()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, config.Resolved{
		Name:         "cmp-loader",
		URL:          site.URL + "/",
		ConsentModes: []model.ConsentMode{model.ConsentAccept},
		IdleQuiet:    time.Second,
		HardTimeout:  40 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  100,
		Robots:       config.RobotsIgnore,
	}, model.ConsentAccept)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	c := out.Result.Consent

	if c.Outcome != model.OutcomeFailed || c.Kind != model.CMPKindVendor || c.CMP != "Fixture CMP" {
		t.Fatalf("consent = %s/%s/%q (%s), want failed/vendor/Fixture CMP", c.Outcome, c.Kind, c.CMP, c.Reason)
	}

	if !c.BotDetected {
		t.Errorf("botDetected is false; reason %q", c.Reason)
	}

	if !strings.Contains(c.Reason, `"fixture CMP disabled for bots"`) {
		t.Errorf("the reason does not quote the stub: %q", c.Reason)
	}

	if strings.Contains(c.Reason, "fixture-key") {
		t.Errorf("the reason quotes the page's loader URL, API key included: %q", c.Reason)
	}

	var found bool

	for _, req := range out.Result.Requests {
		if req.RequestID == c.LoaderRequestID {
			found = strings.HasPrefix(req.URL, cmpOrigin.URL+"/app.js")
		}
	}

	if !found {
		t.Errorf("loaderRequestId %q does not name the loader's request", c.LoaderRequestID)
	}

	if out.Result.Confidence == nil || out.Result.Confidence.Band == model.ConfidenceHigh {
		t.Errorf("a scan whose CMP never ran is scored %+v", out.Result.Confidence)
	}
}

func loaderRules(t *testing.T, loader, stub string) *consent.RuleSet {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rules.yaml")
	pack := fmt.Sprintf(`version: 1
rules:
  - name: fixture-cmp
    vendor: Fixture CMP
    detect: "!!window.FixtureCMP"
    accept:
      - eval: "window.FixtureCMP.accept()"
    loaders:
      - %s
    stubs:
      - sha256: %s
        says: fixture CMP disabled for bots
        means: bot
`, loader, stub)

	if err := os.WriteFile(path, []byte(pack), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, err := consent.LoadRuleFiles(path)
	if err != nil {
		t.Fatalf("LoadRuleFiles: %v", err)
	}

	return rules
}
