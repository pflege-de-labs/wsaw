//go:build objectstore && cloudblob

package objectstore

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob"

	"github.com/pflege-de-labs/wsaw/internal/browser"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/consent"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// TestAFullCycleAgainstAnObjectStore is Story 8.9, AC2: a real scan of a real
// page, stored in a real object store, pruned, and read back — for each kind of
// store, because the two put entirely different things in the bucket.
//
// It is one test rather than four because the point is the *cycle*. Each step
// alone is covered by the unit suites, which now run against MinIO too; what
// only this can show is that a scan's evidence survives being written by one
// step, indexed by another, partly deleted by a third and read by a fourth,
// with an S3 implementation underneath instead of a filesystem.
func TestAFullCycleAgainstAnObjectStore(t *testing.T) {
	needMinIO(t)

	kinds := map[string]string{
		"index in rows":            store.DriverSQLite,
		"index in the same bucket": store.DriverBlob,
	}

	for name, driver := range kinds {
		t.Run(name, func(t *testing.T) {
			runCycle(t, driver)
		})
	}
}

// runCycle is the cycle itself, for one kind of store.
func runCycle(t *testing.T, driver string) {
	t.Helper()

	scan := newScanner(t)

	dir := t.TempDir()

	opts := store.Options{
		// Every run gets its own subtree of the bucket. The bucket is not
		// created fresh — it is a service — so isolation is a prefix, and the
		// prefix is a query parameter gocloud applies before the driver sees
		// the URL, so nothing in wsaw knows it is in one.
		ArtifactDir: minioBucketURL(fmt.Sprintf("cycle-%s-%d/", driver, time.Now().UnixNano())),
		Driver:      driver,
		Logger:      slog.New(slog.DiscardHandler),
	}

	if driver == store.DriverSQLite {
		opts.Path = dir + "/wsaw.db"
	}

	st, err := store.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("opening a %s store on the object store: %v", driver, err)
	}

	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})

	// The startup probe an operator's deployment runs, against a bucket that
	// charges for it (Story 8.6, AC4).
	if err := st.ProbeArtifactBucket(t.Context()); err != nil {
		t.Fatalf("the object store failed its write probe: %v", err)
	}

	first := scan(t, st, "the first scan")
	second := scan(t, st, "the second scan")

	if first.ScanID == second.ScanID {
		t.Fatalf("two scans produced one scan ID (%s), so nothing below is testing two results", first.ScanID)
	}

	// --- what landed ------------------------------------------------------

	assertReadsBack(t, st, first)
	assertReadsBack(t, st, second)

	summaries, err := st.ListResults(first.Target, first.ConsentMode, 0)
	if err != nil {
		t.Fatalf("listing the history: %v", err)
	}

	if len(summaries) != 2 {
		t.Fatalf("the history holds %d results after two scans, want 2", len(summaries))
	}

	// Newest first, whichever store answered — the order the whole product
	// assumes and the one an S3 listing has to be made to produce.
	if summaries[0].ScanID != second.ScanID {
		t.Errorf("the newest result is %s, want %s", summaries[0].ScanID, second.ScanID)
	}

	// --- prune ------------------------------------------------------------

	// The older scan goes, and everything only it referenced goes with it
	// (Story 8.5, AC1). Both scans are minutes old at most, so the count limit
	// is what decides rather than an age.
	stats, err := st.Prune(t.Context(), time.Now(), store.Retention{MaxPerSeries: 1})
	if err != nil {
		t.Fatalf("pruning against the object store: %v", err)
	}

	if stats.ResultsDeleted != 1 {
		t.Errorf("the prune removed %d results, want 1", stats.ResultsDeleted)
	}

	if stats.ArtifactsDeleted == 0 {
		t.Error("the prune removed no objects at all, so the older scan's evidence is still in the bucket")
	}

	if stats.ArtifactsFailed != 0 || stats.IndexKeysFailed != 0 {
		t.Errorf("the object store refused %d deletes and %d index keys",
			stats.ArtifactsFailed, stats.IndexKeysFailed)
	}

	if stats.BytesFreed <= 0 {
		t.Errorf("BytesFreed = %d after deleting %d objects", stats.BytesFreed, stats.ArtifactsDeleted)
	}

	// --- read back --------------------------------------------------------

	// The survivor is whole: its document, and every screenshot and stored
	// body it names, are still fetchable from the bucket.
	assertReadsBack(t, st, second)

	if _, err := st.GetResult(first.Target, first.ConsentMode, first.ScanID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reading the pruned scan = %v, want ErrNotFound", err)
	}

	after, err := st.ListResults(first.Target, first.ConsentMode, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(after) != 1 || after[0].ScanID != second.ScanID {
		t.Errorf("the history after the prune is %+v, want only %s", after, second.ScanID)
	}

	// And the bucket agrees, asked directly rather than through the store: a
	// prune that reported deletions it did not make would pass every assertion
	// above.
	assertBucketHolds(t, opts.ArtifactDir, second, first)
}

// assertReadsBack reads one scan back through the store, evidence included.
func assertReadsBack(t *testing.T, st store.Store, want *model.Result) {
	t.Helper()

	got, err := st.GetResult(want.Target, want.ConsentMode, want.ScanID)
	if err != nil {
		t.Fatalf("reading %s back from the object store: %v", want.ScanID, err)
	}

	if got.ScanID != want.ScanID || len(got.Requests) != len(want.Requests) {
		t.Errorf("%s read back as %s with %d requests, want %d",
			want.ScanID, got.ScanID, len(got.Requests), len(want.Requests))
	}

	if len(got.Screenshots) == 0 {
		t.Fatalf("%s has no screenshot, so this test is not covering artifacts", want.ScanID)
	}

	for _, shot := range got.Screenshots {
		data, err := st.GetArtifact(shot.Ref)
		if err != nil {
			t.Errorf("%s: the screenshot at %s is not readable: %v", want.ScanID, shot.Ref, err)

			continue
		}

		if int64(len(data)) != shot.Bytes {
			t.Errorf("%s: the screenshot at %s is %d bytes, the result says %d",
				want.ScanID, shot.Ref, len(data), shot.Bytes)
		}
	}

	// A stored body is the other kind of evidence, and the one served through
	// a stream rather than read whole (Story 8.7).
	for _, req := range got.Requests {
		if req.BodyRef == "" {
			continue
		}

		r, err := st.OpenArtifact(t.Context(), req.BodyRef)
		if err != nil {
			t.Errorf("%s: the stored body at %s did not open: %v", want.ScanID, req.BodyRef, err)

			continue
		}

		read, err := io.Copy(io.Discard, r)
		_ = r.Close()

		if err != nil {
			t.Errorf("%s: streaming the stored body at %s: %v", want.ScanID, req.BodyRef, err)
		}

		if read != r.Size {
			t.Errorf("%s: the stored body at %s streamed %d bytes, its attributes said %d",
				want.ScanID, req.BodyRef, read, r.Size)
		}
	}
}

// assertBucketHolds asks the object store itself what is left, so the prune's
// own report is not the only evidence that it deleted anything.
func assertBucketHolds(t *testing.T, location string, kept, pruned *model.Result) {
	t.Helper()

	bucket, err := blob.OpenBucket(t.Context(), location)
	if err != nil {
		t.Fatalf("opening the bucket to check it: %v", err)
	}

	defer func() { _ = bucket.Close() }()

	for _, shot := range kept.Screenshots {
		found, err := bucket.Exists(t.Context(), shot.Ref)
		if err != nil || !found {
			t.Errorf("the surviving scan's screenshot %s is not in the bucket: %v", shot.Ref, err)
		}
	}

	// The pruned scan's screenshot is gone — unless the two scans captured
	// identical bytes, in which case it is one object and the survivor still
	// names it (Story 8.5, AC1). The fixture is deterministic, so that is the
	// normal outcome and the assertion has to allow for it.
	for _, shot := range pruned.Screenshots {
		if sharedWith(kept, shot.Ref) {
			continue
		}

		found, err := bucket.Exists(t.Context(), shot.Ref)
		if err != nil {
			t.Errorf("asking the bucket about %s: %v", shot.Ref, err)

			continue
		}

		if found {
			t.Errorf("the pruned scan's screenshot %s is still in the bucket", shot.Ref)
		}
	}
}

// sharedWith reports whether the kept result names the same object.
func sharedWith(kept *model.Result, ref string) bool {
	for _, shot := range kept.Screenshots {
		if shot.Ref == ref {
			return true
		}
	}

	for _, req := range kept.Requests {
		if req.BodyRef == ref {
			return true
		}
	}

	return false
}

// newScanner builds a real scanner over a local fixture site and returns a
// function that scans it once.
//
// A real capture rather than a hand-built result, because what this suite is
// for is the whole path: a screenshot is bytes Chrome produced, a stored body
// is bytes a server sent, and a document is what the scanner made of them. A
// fabricated result would exercise the bucket and not the product.
func newScanner(t *testing.T) func(*testing.T, store.Store, string) *model.Result {
	t.Helper()

	info, err := browser.Discover(t.Context(), os.Getenv("WSAW_CHROME_PATH"))
	if err != nil {
		t.Skipf("no usable Chrome: %v", err)
	}

	site := newSite(t)

	pool := browser.NewPool(browser.PoolOptions{
		Size: 1,
		Launch: browser.Options{
			Info:          info,
			LaunchTimeout: 60 * time.Second,
			ProfileDir:    t.TempDir(),
			ExtraArgs:     []string{"host-resolver-rules=" + site.resolverRules()},
		},
	})

	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the browser pool: %v", err)
		}
	})

	normalizer, err := normalize.New(normalize.Rules{DropQueryParams: normalize.DefaultDropQueryParams})
	if err != nil {
		t.Fatal(err)
	}

	rules, err := consent.LoadBuiltinRules()
	if err != nil {
		t.Fatal(err)
	}

	return func(t *testing.T, st store.Store, what string) *model.Result {
		t.Helper()

		s, err := scanner.New(
			scanner.Deps{Pool: pool, Store: st, Rules: rules},
			scanner.Options{Normalizer: normalizer, AllowHeuristicConsent: true, WsawVersion: "objectstore-e2e"},
		)
		if err != nil {
			t.Fatal(err)
		}

		out, err := s.Scan(t.Context(), site.target(), model.ConsentReject)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}

		if out.Result == nil {
			t.Fatalf("%s produced no result at all", what)
		}

		return out.Result
	}
}

// site is the page the scans capture: one first-party origin, one third party,
// a screenshot to store and a body worth storing.
type site struct {
	main  *httptest.Server
	third *httptest.Server
}

const (
	siteHost  = "objectstore-site.test"
	thirdHost = "objectstore-third.test"
)

func newSite(t *testing.T) *site {
	t.Helper()

	s := &site{}

	thirdMux := http.NewServeMux()
	thirdMux.HandleFunc("/t.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__t = 1;"))
	})

	s.third = httptest.NewServer(thirdMux)
	t.Cleanup(s.third.Close)

	mainMux := http.NewServeMux()
	mainMux.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte("window.__app = true;"))
	})
	mainMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>objectstore</title><script src="/app.js"></script>
<script src="http://%s/t.js"></script></head>
<body><h1>objectstore</h1><p>A page whose evidence goes to a bucket.</p></body></html>`, thirdHost)
	})

	s.main = httptest.NewServer(mainMux)
	t.Cleanup(s.main.Close)

	return s
}

func (s *site) resolverRules() string {
	strip := func(u string) string { return strings.TrimPrefix(u, "http://") }

	return fmt.Sprintf("MAP %s %s, MAP %s %s",
		siteHost, strip(s.main.URL), thirdHost, strip(s.third.URL))
}

// target asks for the evidence this suite is about: screenshots and stored
// bodies, which are the artifacts a bucket holds beside the document.
func (s *site) target() config.Resolved {
	return config.Resolved{
		Name:         "objectstore",
		URL:          "http://" + siteHost + "/",
		ConsentModes: []model.ConsentMode{model.ConsentReject},
		IdleQuiet:    750 * time.Millisecond,
		HardTimeout:  30 * time.Second,
		NavTimeout:   20 * time.Second,
		MaxRequests:  200,
		MaxBytes:     1 << 20,
		Robots:       config.RobotsIgnore,
		Screenshots:  true,
		StoreBodies:  true,
	}
}
