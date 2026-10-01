package scanner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/report"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Story 1.11, AC17: a sampled scan with store: all, against a local fixture
// that exercises every kind of exchange a page has — a document, a script, a
// stylesheet, an image, a JSON fetch, a sendBeacon with a payload, a redirect
// and a 204 — keeps every body or says why not, and the HAR carries them.

const bodiesSiteHost = "bodies.test"

type bodiesSite struct {
	site, third *httptest.Server
}

func newBodiesSite(t *testing.T) *bodiesSite {
	t.Helper()

	third := http.NewServeMux()

	third.HandleFunc("/collect", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	})

	b := &bodiesSite{third: httptest.NewServer(third)}
	t.Cleanup(b.third.Close)

	site := http.NewServeMux()

	site.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write([]byte{0x00, 0x00, 0x01, 0x00})
	})

	site.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__app = 'v1';"))
	})

	site.HandleFunc("/style.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("h1 { color: rebeccapurple; }"))
	})

	site.HandleFunc("/moved.gif", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/pixel.gif", http.StatusFound)
	})

	site.HandleFunc("/pixel.gif", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
	})

	site.HandleFunc("/data.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"products":["a","b"]}`))
	})

	site.HandleFunc("/empty", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	site.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>bodies</title>
<link rel="stylesheet" href="/style.css">
<script src="/app.js"></script>
</head><body>
<h1>bodies fixture</h1>
<img src="/moved.gif" alt="">
<script>
fetch('/data.json').then(function (r) { return r.json(); });
fetch('/empty');
navigator.sendBeacon('http://%s/collect', JSON.stringify({event: 'pageview', visitor: 'v-42'}));
</script>
</body></html>`, thirdHost)
	})

	b.site = httptest.NewServer(site)
	t.Cleanup(b.site.Close)

	return b
}

func (b *bodiesSite) resolverRules() string {
	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		bodiesSiteHost, hostPort(b.site.URL), thirdHost, hostPort(b.third.URL))
}

func (b *bodiesSite) target(p config.BodyPolicy) config.Resolved {
	return config.Resolved{
		Name:         "bodies",
		URL:          "http://" + bodiesSiteHost + "/",
		ConsentModes: []model.ConsentMode{model.ConsentNone},
		IdleQuiet:    1500 * time.Millisecond,
		HardTimeout:  30 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  500,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
		Bodies:       p,
	}
}

var storeEverything = config.BodyPolicy{
	Store: model.BodyStoreAll, Ratio: 1, RatioWindow: time.Hour,
	RequestBodies: true, MaxBodyBytes: 1 << 20, MaxScanBytes: 8 << 20,
}

// newBodyScanner builds a scanner with a store, which is both where bodies go
// and the sampling ledger.
func newBodyScanner(t *testing.T, info browser.Info, rules string) (*scanner.Scanner, store.Store) {
	t.Helper()

	launch := browser.Options{
		Info: info, LaunchTimeout: 40 * time.Second, ProfileDir: t.TempDir(),
		ExtraArgs: []string{"host-resolver-rules=" + rules},
	}

	pool := browser.NewPool(browser.PoolOptions{Size: 1, Launch: launch})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing pool: %v", err)
		}
	})

	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{Path: dir + "/wsaw.db", ArtifactDir: dir + "/artifacts"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})

	normalizer, err := normalize.New(normalize.Rules{})
	if err != nil {
		t.Fatal(err)
	}

	consentRules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	s, err := scanner.New(scanner.Deps{Pool: pool, Store: st, Ledger: st, Rules: consentRules}, scanner.Options{
		Normalizer: normalizer, WsawVersion: "test", ChromeVersion: info.Version,
	})
	if err != nil {
		t.Fatal(err)
	}

	return s, st
}

func TestASampledScanStoresEveryBodyAndTheHARCarriesThem(t *testing.T) {
	info := requireChrome(t)

	site := newBodiesSite(t)
	s, st := newBodyScanner(t, info, site.resolverRules())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	out, err := s.Scan(ctx, site.target(storeEverything), model.ConsentNone)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	res := out.Result
	if !res.OK() {
		t.Fatalf("scan not OK: %s %s", res.Termination, res.Error)
	}

	bc := res.BodyCapture
	if bc == nil || !bc.Sampled || bc.Decision != model.BodyDecisionSampled || bc.ResponseBodiesStored == 0 {
		t.Fatalf("bodyCapture = %+v, want a sampled scan that stored bodies", bc)
	}

	byPath := requestsByPath(t, res)

	for _, path := range []string{"/", "/style.css", "/app.js", "/pixel.gif", "/data.json"} {
		if byPath[path].BodyRef == "" {
			t.Errorf("%s: no stored body (%q)", path, byPath[path].BodyUnavailable)
		}
	}

	// AC13: only fingerprinted types carry a digest.
	if byPath["/app.js"].BodySHA256 == "" {
		t.Error("the script lost its digest")
	}

	if byPath["/style.css"].BodySHA256 != "" {
		t.Error("a stylesheet stored under store: all has a digest; comparisons would start reading it")
	}

	if got := byPath["/moved.gif (redirect)"].BodyUnavailable; got != "no body: redirect" {
		t.Errorf("redirect hop: %q, want no body: redirect", got)
	}

	if got := byPath["/empty"].BodyUnavailable; got != "no body: status 204" {
		t.Errorf("204: %q, want no body: status 204", got)
	}

	beacon := byPath["/collect"]
	if beacon.RequestBodyRef == "" {
		t.Fatalf("the beacon's payload was not stored: %q", beacon.RequestBodyUnavailable)
	}

	payload, err := st.GetArtifact(beacon.RequestBodyRef)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(payload, []byte("v-42")) {
		t.Errorf("stored payload %q is not what the page sent", payload)
	}

	// AC14: the HAR carries the bodies and the payload.
	assertHARCarriesBodies(t, res, st)
}

// requestsByPath indexes a scan's network requests by path, a redirect hop
// marked as such, and checks AC11 on the way: every one has its body, or a
// reason.
func requestsByPath(t *testing.T, res *model.Result) map[string]model.Request {
	t.Helper()

	byPath := map[string]model.Request{}

	for _, req := range res.Requests {
		if req.NonNetwork {
			continue
		}

		if req.BodyRef == "" && req.BodyUnavailable == "" {
			t.Errorf("%s %s has neither a stored body nor a reason", req.ResourceType, req.URL)
		}

		key := req.URL
		if i := strings.Index(key, "://"); i >= 0 {
			key = key[i+3:]
			key = key[strings.Index(key, "/"):]
		}

		if req.RedirectTo != "" {
			key += " (redirect)"
		}

		byPath[key] = req
	}

	return byPath
}

// assertHARCarriesBodies writes the scan's HAR from the store and checks that
// a response body and the beacon's payload are in it.
func assertHARCarriesBodies(t *testing.T, res *model.Result, st store.Store) {
	t.Helper()

	var buf bytes.Buffer
	if err := report.WriteHAR(&buf, res, st.GetArtifact); err != nil {
		t.Fatal(err)
	}

	var har struct {
		Log struct {
			Entries []struct {
				Request struct {
					URL      string `json:"url"`
					PostData *struct {
						Text string `json:"text"`
					} `json:"postData"`
				} `json:"request"`
				Response struct {
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}

	if err := json.Unmarshal(buf.Bytes(), &har); err != nil {
		t.Fatalf("the HAR does not parse: %v", err)
	}

	var sawCSS, sawPayload bool

	for _, e := range har.Log.Entries {
		if strings.HasSuffix(e.Request.URL, "/style.css") && strings.Contains(e.Response.Content.Text, "rebeccapurple") {
			sawCSS = true
		}

		if strings.HasSuffix(e.Request.URL, "/collect") && e.Request.PostData != nil &&
			strings.Contains(e.Request.PostData.Text, "pageview") {
			sawPayload = true
		}
	}

	if !sawCSS {
		t.Error("the HAR does not carry the stylesheet's body")
	}

	if !sawPayload {
		t.Error("the HAR does not carry the beacon's postData")
	}
}

// AC13: storing more bodies changes nothing a comparison sees. A sampled and
// an unsampled scan of an unchanged site produce an empty diff in both orders,
// and the same confidence.
func TestSampledAndUnsampledScansDiffEmpty(t *testing.T) {
	info := requireChrome(t)

	site := newBodiesSite(t)
	s, _ := newBodyScanner(t, info, site.resolverRules())

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	unsampled := storeEverything
	unsampled.Store = model.BodyStoreNone

	var scores []int

	for i, p := range []config.BodyPolicy{storeEverything, unsampled, storeEverything} {
		out, err := s.Scan(ctx, site.target(p), model.ConsentNone)
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}

		scores = append(scores, out.Result.Confidence.Score)

		if i == 0 || out.Diff == nil {
			continue
		}

		for _, c := range out.Diff.Changes {
			t.Errorf("scan %d (sampled %v) reported %s %s: %s",
				i, out.Result.BodyCapture.Sampled, c.Type, c.Subject, c.Detail)
		}
	}

	if scores[0] != scores[1] || scores[1] != scores[2] {
		t.Errorf("confidence scores %v differ between sampled and unsampled scans", scores)
	}
}
