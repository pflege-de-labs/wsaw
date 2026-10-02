package store

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// These tests hold the storage dashboard's figures to what the bucket
// actually occupies on disk, measured by walking the local bucket.

// bucketBytes is what the local bucket under s occupies: the size of every
// artifact file in it, which is the figure the dashboard has to agree with.
func bucketBytes(t *testing.T, s *SQL) int64 {
	t.Helper()

	var total int64

	err := filepath.WalkDir(s.bucket.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		// fileblob keeps its own attribute file beside each object; it is the
		// driver's, not an artifact wsaw stored.
		if strings.HasSuffix(path, ".attrs") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		t.Fatalf("measuring the bucket: %v", err)
	}

	return total
}

// storedObjectBytes is the on-disk size of one artifact, under whichever
// spelling the bucket holds it.
func storedObjectBytes(t *testing.T, s *SQL, ref string) int64 {
	t.Helper()

	key, err := s.bucket.storedKey(t.Context(), ref)
	if err != nil {
		t.Fatalf("finding %s: %v", ref, err)
	}

	info, err := os.Stat(filepath.Join(s.bucket.dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("measuring %s: %v", ref, err)
	}

	return info.Size()
}

// documentStoredBytes is the on-disk size of a stored result's document.
func documentStoredBytes(t *testing.T, s *SQL, res *model.Result) int64 {
	t.Helper()

	var ref string

	q := `select artifact_ref from results where target = ? and consent_mode = ? and scan_id = ?`
	if err := s.db.QueryRowContext(t.Context(), s.q(q), res.Target, string(res.ConsentMode), res.ScanID).
		Scan(&ref); err != nil {
		t.Fatalf("reading the document reference of %s: %v", res.ScanID, err)
	}

	return storedObjectBytes(t, s, ref)
}

// withEvidence gives a result a screenshot and stored bodies, by reference,
// the way capture does: each stored first, then named.
func withEvidence(t *testing.T, s *SQL, res *model.Result, screenshot []byte, bodies ...[]byte) {
	t.Helper()

	ref, err := s.PutArtifact("screenshot-before-consent", screenshot)
	if err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	res.Screenshots = append(res.Screenshots, model.Artifact{
		Kind: "screenshot-before-consent", Ref: ref, Bytes: int64(len(screenshot)),
	})

	for i, body := range bodies {
		ref, err := s.PutArtifact(artifactKindBody, body)
		if err != nil {
			t.Fatalf("PutArtifact: %v", err)
		}

		res.Requests = append(res.Requests, model.Request{
			URL:    fmt.Sprintf("https://cdn.test/%d.js", i),
			Domain: "cdn.test", Party: model.ThirdParty, Phase: model.PhasePost,
			BodyRef: ref, BodyStoredSize: int64(len(body)),
		})
	}
}

// TestStorageChargesEachObjectOnceAtItsStoredSize is the regression for a
// dashboard that reported 7.0 GB for a 409 MB store: the per-series query
// joined every result to its reference rows and summed document_size once per
// row, so a scan naming 23 artifacts had its document counted 23 times, and a
// body shared by twenty scans was charged twenty times at its unpacked size.
// What the dashboard states must be what the bucket occupies.
func TestStorageChargesEachObjectOnceAtItsStoredSize(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	now := time.Now()
	shared := bytes.Repeat([]byte("shared library "), 4096)
	ownBodies := [][]byte{
		bytes.Repeat([]byte("first body "), 2048),
		bytes.Repeat([]byte("second body "), 2048),
		bytes.Repeat([]byte("third body "), 2048),
	}

	// Two scans of one series naming the same screenshot and the same bodies,
	// and a scan of another series that shares one body with them.
	first := storedResult("site-1", now.Add(-2*time.Hour))
	withEvidence(t, s, first, []byte("site screenshot"), append([][]byte{shared}, ownBodies...)...)

	second := storedResult("site-2", now.Add(-time.Hour))
	withEvidence(t, s, second, []byte("site screenshot"), append([][]byte{shared}, ownBodies...)...)

	other := storedResult("other-1", now)
	other.Target = "other"
	other.ConsentMode = model.ConsentAccept
	withEvidence(t, s, other, []byte("other screenshot"), shared)

	for _, res := range []*model.Result{first, second, other} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	report, err := s.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}

	if want := bucketBytes(t, s); report.StoredTotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d (what the bucket holds)", report.StoredTotalBytes, want)
	}

	if report.Unmeasured != 0 {
		t.Errorf("Unmeasured = %d, want 0 (every object was written by this store)", report.Unmeasured)
	}

	sharedBytes := storedObjectBytes(t, s, other.Requests[len(other.Requests)-1].BodyRef)
	if report.StoredSharedBytes != sharedBytes {
		t.Errorf("SharedBytes = %d, want %d (the one body both series name, once)", report.StoredSharedBytes, sharedBytes)
	}

	var rows int64
	for _, sd := range report.Series {
		rows += sd.StoredTotalBytes()
	}

	if rows+report.StoredSharedBytes != report.StoredTotalBytes {
		t.Errorf("rows (%d) + shared (%d) = %d, want the total %d",
			rows, report.StoredSharedBytes, rows+report.StoredSharedBytes, report.StoredTotalBytes)
	}

	var site SeriesStorage

	for _, sd := range report.Series {
		if sd.Series.Target == "site" {
			site = sd
		}
	}

	wantDocs := documentStoredBytes(t, s, first) + documentStoredBytes(t, s, second)
	if site.StoredDocumentBytes != wantDocs {
		t.Errorf("site DocumentBytes = %d, want %d (two documents, each once, as stored)", site.StoredDocumentBytes, wantDocs)
	}

	wantArtifacts := storedObjectBytes(t, s, first.Screenshots[0].Ref)
	for _, req := range first.Requests {
		if req.BodyRef != "" && req.BodyRef != other.Requests[len(other.Requests)-1].BodyRef {
			wantArtifacts += storedObjectBytes(t, s, req.BodyRef)
		}
	}

	if site.StoredArtifactBytes != wantArtifacts {
		t.Errorf("site ArtifactBytes = %d, want %d (one screenshot and three bodies, each once)",
			site.StoredArtifactBytes, wantArtifacts)
	}

	if site.StoredSharedBytes != sharedBytes {
		t.Errorf("site SharedBytes = %d, want %d", site.StoredSharedBytes, sharedBytes)
	}
}

// TestStorageCountsUnmeasuredObjectsUntilASweepMeasuresThem: a store written
// before sizes were recorded must not read as holding nothing. Its objects are
// counted as unmeasured, and the next sweep records their sizes from its
// listing and drops the record of an object the bucket no longer holds.
func TestStorageCountsUnmeasuredObjectsUntilASweepMeasuresThem(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	res := storedResult("old-1", time.Now().Add(-time.Hour))
	withEvidence(t, s, res, []byte("screenshot"), []byte("body"))

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	// What a store upgraded into this version looks like: no size recorded
	// for anything, and a record left for an object that has since gone.
	if _, err := s.db.ExecContext(t.Context(), `delete from `+artifactSizesTable); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(t.Context(), s.q(`insert into `+artifactSizesTable+
		` (artifact_ref, stored_bytes, measured_at) values (?, ?, ?)`),
		artifactKindBody+refSeparator+strings.Repeat("0", 64), 999, 1); err != nil {
		t.Fatal(err)
	}

	before, err := s.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}

	if before.Unmeasured != 3 {
		t.Errorf("Unmeasured = %d, want 3 (the document, the screenshot and the body)", before.Unmeasured)
	}

	if before.StoredTotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0: an unmeasured object is not counted at a guessed size", before.StoredTotalBytes)
	}

	if _, err := s.Sweep(t.Context(), TriggerCLI, time.Now(), SweepOptions{}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	after, err := s.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}

	if after.Unmeasured != 0 {
		t.Errorf("after the sweep, Unmeasured = %d, want 0", after.Unmeasured)
	}

	if want := bucketBytes(t, s); after.StoredTotalBytes != want {
		t.Errorf("after the sweep, TotalBytes = %d, want %d (what the bucket holds)", after.StoredTotalBytes, want)
	}

	var rows int
	if err := s.db.QueryRowContext(t.Context(), `select count(*) from `+artifactSizesTable).Scan(&rows); err != nil {
		t.Fatal(err)
	}

	if rows != 3 {
		t.Errorf("%d size records after the sweep, want 3: the record of an object the bucket "+
			"no longer holds must go", rows)
	}
}

// TestPlanSweepRecordsNoSizes: a dry run writes nothing (Story 8.5, AC6),
// sizes included.
func TestPlanSweepRecordsNoSizes(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	if err := s.PutResult(storedResult("one", time.Now())); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(t.Context(), `delete from `+artifactSizesTable); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PlanSweep(t.Context(), time.Now(), SweepOptions{}); err != nil {
		t.Fatalf("PlanSweep: %v", err)
	}

	report, err := s.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}

	if report.Unmeasured != 1 {
		t.Errorf("Unmeasured = %d after a plan, want 1: a plan must not record sizes", report.Unmeasured)
	}
}

// TestPruneForgetsTheSizeOfWhatItDeletes: a pruned result's evidence leaves
// the bucket, and its size record goes with it.
func TestPruneForgetsTheSizeOfWhatItDeletes(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	older := storedResult("older", time.Now().Add(-time.Hour))
	withEvidence(t, s, older, []byte("old screenshot"))

	newer := storedResult("newer", time.Now())
	withEvidence(t, s, newer, []byte("new screenshot"))

	for _, res := range []*model.Result{older, newer} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.Prune(t.Context(), TriggerCLI, time.Now(), Retention{MaxPerSeries: 1}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var rows int
	if err := s.db.QueryRowContext(t.Context(), `select count(*) from `+artifactSizesTable).Scan(&rows); err != nil {
		t.Fatal(err)
	}

	if rows != 2 {
		t.Errorf("%d size records after the prune, want 2 (the newer scan's document and screenshot)", rows)
	}
}

// TestMonthlyStorageCountsASharedObjectOnce: an object two months' scans both
// name is in the earlier month only, so the months add up to what the bucket
// holds rather than to more.
func TestMonthlyStorageCountsASharedObjectOnce(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.UTC)
	lastMonth := thisMonth.AddDate(0, -1, 0)
	body := bytes.Repeat([]byte("unchanged script "), 1024)

	older := storedResult("older", lastMonth)
	withEvidence(t, s, older, []byte("screenshot"), body)

	newer := storedResult("newer", thisMonth)
	withEvidence(t, s, newer, []byte("screenshot"), body)

	for _, res := range []*model.Result{older, newer} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	months, err := s.MonthlyStorage(t.Context(), lastMonth)
	if err != nil {
		t.Fatalf("MonthlyStorage: %v", err)
	}

	var total int64
	for _, m := range months {
		total += m.Bytes
	}

	if want := bucketBytes(t, s); total != want {
		t.Errorf("the months add up to %d, want %d (what the bucket holds)", total, want)
	}

	if want := documentStoredBytes(t, s, newer); months[len(months)-1].Bytes != want {
		t.Errorf("this month = %d, want %d: only the newer scan's own document is new this month",
			months[len(months)-1].Bytes, want)
	}
}

// TestListResultsReportsStoredSizes is the regression for a history page that
// showed each scan at its unpacked size: a document read as 238 KB while the
// bucket held 25 KB of it, and bodies the same. A scan's figures are what its
// objects occupy on disk.
func TestListResultsReportsStoredSizes(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	res := storedResult("one", time.Now())
	withEvidence(t, s, res, []byte("screenshot"), bytes.Repeat([]byte("compressible body "), 4096))

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}

	if want := documentStoredBytes(t, s, res); got[0].StoredDocumentBytes != want {
		t.Errorf("DocumentBytes = %d, want %d (the document as stored)", got[0].StoredDocumentBytes, want)
	}

	want := storedObjectBytes(t, s, res.Screenshots[0].Ref) +
		storedObjectBytes(t, s, res.Requests[len(res.Requests)-1].BodyRef)
	if got[0].StoredArtifactBytes != want {
		t.Errorf("ArtifactBytes = %d, want %d (the screenshot and the body as stored)", got[0].StoredArtifactBytes, want)
	}

	if total := got[0].StoredDocumentBytes + got[0].StoredArtifactBytes; total != bucketBytes(t, s) {
		t.Errorf("the one scan holds %d, want %d (everything in the bucket is its)", total, bucketBytes(t, s))
	}
}

// TestListResultsFlagsAPartlyMeasuredScan is the regression for scans whose
// screenshot had a recorded size and whose bodies did not: their figure was
// shown as known, and simply too small. Any unmeasured object makes the
// scan's figures incomplete, and the listing says how many.
func TestListResultsFlagsAPartlyMeasuredScan(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	res := storedResult("partly", time.Now())
	withEvidence(t, s, res, []byte("screenshot"), []byte("body one"), []byte("body two"))

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(t.Context(), s.q(`delete from `+artifactSizesTable+` where artifact_ref = ?`),
		res.Requests[len(res.Requests)-1].BodyRef); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	if got[0].UnmeasuredObjects != 1 {
		t.Errorf("UnmeasuredObjects = %d, want 1", got[0].UnmeasuredObjects)
	}

	if got[0].StoredArtifactBytes <= 0 {
		t.Errorf("StoredArtifactBytes = %d, want the measured screenshot and body", got[0].StoredArtifactBytes)
	}
}

// TestListResultsKeepsTheRecordedSizes: documentBytes, artifactBytes and
// artifactBytesRecorded keep API 1.0's meaning — the sizes the result recorded
// about itself, before packing — beside the stored figures that are new.
func TestListResultsKeepsTheRecordedSizes(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	screenshot := []byte("screenshot")
	body := bytes.Repeat([]byte("compressible body "), 4096)

	res := storedResult("one", time.Now())
	withEvidence(t, s, res, screenshot, body)

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	var documentSize int64
	if err := s.db.QueryRowContext(t.Context(), `select document_size from results`).Scan(&documentSize); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	if got[0].DocumentBytes != documentSize {
		t.Errorf("DocumentBytes = %d, want document_size %d", got[0].DocumentBytes, documentSize)
	}

	if want := int64(len(screenshot) + len(body)); got[0].ArtifactBytes != want {
		t.Errorf("ArtifactBytes = %d, want %d (the recorded sizes)", got[0].ArtifactBytes, want)
	}

	if !got[0].ArtifactBytesRecorded {
		t.Error("ArtifactBytesRecorded = false, want true")
	}

	if got[0].StoredDocumentBytes >= got[0].DocumentBytes {
		t.Errorf("StoredDocumentBytes = %d, want less than the unpacked %d: the bucket compresses documents",
			got[0].StoredDocumentBytes, got[0].DocumentBytes)
	}
}

// TestStorageKeepsTheRecordedSizes: the per-series documentBytes and
// artifactBytes keep API 1.0's meaning, without its fault — the document
// counted once per result, not once per artifact its result names.
func TestStorageKeepsTheRecordedSizes(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	res := storedResult("one", time.Now())
	withEvidence(t, s, res, []byte("screenshot"), []byte("body one"), []byte("body two"), []byte("body three"))

	if err := s.PutResult(res); err != nil {
		t.Fatal(err)
	}

	listed, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	report, err := s.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}

	sd := report.Series[0]

	if sd.DocumentBytes != listed[0].DocumentBytes {
		t.Errorf("DocumentBytes = %d, want %d: one result's document, once", sd.DocumentBytes, listed[0].DocumentBytes)
	}

	if sd.ArtifactBytes != listed[0].ArtifactBytes {
		t.Errorf("ArtifactBytes = %d, want %d", sd.ArtifactBytes, listed[0].ArtifactBytes)
	}

	if report.TotalBytes != sd.TotalBytes() {
		t.Errorf("TotalBytes = %d, want the one row's %d", report.TotalBytes, sd.TotalBytes())
	}
}

// TestListResultsReportsWhatEachScanAdded: evidence unchanged since an earlier
// scan of the series is stored once, so a scan adds only what no earlier scan
// stored. That holds however few scans are listed, because "earlier" is
// every stored scan, not every listed one.
func TestListResultsReportsWhatEachScanAdded(t *testing.T) {
	t.Parallel()

	s, _ := openCounted(t)

	body := bytes.Repeat([]byte("unchanged script "), 2048)

	first := storedResult("first", time.Now().Add(-time.Hour))
	withEvidence(t, s, first, []byte("unchanged screenshot"), body)

	second := storedResult("second", time.Now())
	withEvidence(t, s, second, []byte("unchanged screenshot"), body)

	for _, res := range []*model.Result{first, second} {
		if err := s.PutResult(res); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListResults("site", model.ConsentReject, 0)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	byID := map[string]Summary{}
	for _, sm := range all {
		byID[sm.ScanID] = sm
	}

	if got, want := byID["first"].StoredNewBytes, bucketBytes(t, s)-documentStoredBytes(t, s, second); got != want {
		t.Errorf("first NewBytes = %d, want %d (everything but the second scan's document)", got, want)
	}

	if got, want := byID["second"].StoredNewBytes, documentStoredBytes(t, s, second); got != want {
		t.Errorf("second NewBytes = %d, want %d (its own document only)", got, want)
	}

	if byID["second"].StoredArtifactBytes != byID["first"].StoredArtifactBytes {
		t.Errorf("second ArtifactBytes = %d, want %d: a scan is still charged for what it references",
			byID["second"].StoredArtifactBytes, byID["first"].StoredArtifactBytes)
	}

	newest, err := s.ListResults("site", model.ConsentReject, 1)
	if err != nil {
		t.Fatalf("ListResults: %v", err)
	}

	if got, want := newest[0].StoredNewBytes, documentStoredBytes(t, s, second); got != want {
		t.Errorf("listed alone, second NewBytes = %d, want %d: the unlisted first scan still stored the rest",
			got, want)
	}
}
