package httpapi

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/secret"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// This file is Story 5.32: one page that ranks every series by the storage
// it holds, plots how that has grown, and states what the last prune and
// sweep gave back.
//
// Every figure on it comes from the index alone — Storage and MonthlyStorage
// read the results and result_artifacts tables and the stored size the index
// records for each object, and LastMaintenanceRun/MaintenanceRuns read Story
// 4.11's receipt log — never from a bucket operation (AC10). The figures are
// what the bucket occupies: each object once, at its stored size. The
// page deletes nothing: no prune button, no sweep button, only the CLI
// commands named (AC11).

// storageSnapshotTTL is how long a computed snapshot is served before the
// next view recomputes it. The page's own figures come from an aggregate
// query over every stored result, and a wall display polling every few
// seconds must not turn that into a query every few seconds (AC10).
const storageSnapshotTTL = 5 * time.Minute

// storageCache holds the one snapshot the dashboard serves between
// recomputes, guarded for the handler's own concurrent requests.
type storageCache struct {
	mu         sync.Mutex
	data       storageData
	computedAt time.Time
}

// get returns the cached snapshot when it is fresh enough, or computes and
// caches a new one. force skips the freshness check — the "recompute now"
// link AC10 asks for.
func (c *storageCache) get(
	ctx context.Context, force bool, compute func(context.Context) (storageData, error),
) (storageData, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force && !c.computedAt.IsZero() && time.Since(c.computedAt) < storageSnapshotTTL {
		return c.data, c.computedAt, nil
	}

	data, err := compute(ctx)
	if err != nil {
		return storageData{}, time.Time{}, err
	}

	c.data, c.computedAt = data, time.Now()

	return c.data, c.computedAt, nil
}

// storageRow is one series' line in the ranked table (AC2).
type storageRow struct {
	Series store.Series `json:"series"`

	Count int `json:"count"`

	// DocumentBytes and ArtifactBytes are what the series' results record
	// about their own size, before packing, each result charged for every
	// artifact it references (Story 5.31, AC2); SharePercent is their total's
	// share of GrandTotalBytes. API 1.0's figures, kept as they were.
	DocumentBytes int64   `json:"documentBytes"`
	ArtifactBytes int64   `json:"artifactBytes"`
	SharePercent  float64 `json:"sharePercent"`

	// StoredDocumentBytes and StoredArtifactBytes are what the bucket stores
	// for the objects only this series references, at their stored size:
	// what deleting it would give back. StoredSharedBytes is what it
	// references that another series does too, which is in the shared row
	// instead. StoredSharePercent is the row's share of StoredTotalBytes.
	StoredDocumentBytes int64   `json:"storedDocumentBytes"`
	StoredArtifactBytes int64   `json:"storedArtifactBytes"`
	StoredSharedBytes   int64   `json:"storedSharedBytes"`
	StoredSharePercent  float64 `json:"storedSharePercent"`

	// UnmeasuredObjects counts the objects whose stored size the index has
	// not recorded yet, in none of this row's Stored figures.
	UnmeasuredObjects int `json:"unmeasuredObjects"`

	Oldest time.Time `json:"oldest,omitzero"`
	Newest time.Time `json:"newest,omitzero"`
}

// StoredTotalBytes is what the bucket stores for this series alone.
func (r storageRow) StoredTotalBytes() int64 { return r.StoredDocumentBytes + r.StoredArtifactBytes }

// prunePanel is Story 5.32, AC7's panel for the last recorded prune run —
// read once from LastMaintenanceRun and never recomputed from the runs
// themselves.
type prunePanel struct {
	// Available is false when this store cannot report maintenance history
	// at all (not the *store.SQL these reads need). Found is false when it
	// can, but nothing has run yet; Reason says why (AC7).
	Available bool   `json:"available"`
	Found     bool   `json:"found"`
	Reason    string `json:"reason,omitempty"`

	Trigger    string    `json:"trigger,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Error      string    `json:"error,omitempty"`

	ResultsDeleted   int       `json:"resultsDeleted,omitempty"`
	ResultsKept      int       `json:"resultsKept,omitempty"`
	SeriesPruned     int       `json:"seriesPruned,omitempty"`
	OldestKept       time.Time `json:"oldestKept,omitzero"`
	ArtifactsDeleted int       `json:"artifactsDeleted,omitempty"`
	BytesFreed       int64     `json:"bytesFreed,omitempty"`
}

// Duration is how long the run took.
func (p prunePanel) Duration() time.Duration { return p.FinishedAt.Sub(p.StartedAt) }

// sweepPanel is AC7's panel for the last recorded sweep run: the same shape
// of facts as prunePanel, for what a bucket-wide listing found.
type sweepPanel struct {
	Available bool   `json:"available"`
	Found     bool   `json:"found"`
	Reason    string `json:"reason,omitempty"`

	Trigger    string    `json:"trigger,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Error      string    `json:"error,omitempty"`

	ArtifactsScanned   int   `json:"artifactsScanned,omitempty"`
	BytesScanned       int64 `json:"bytesScanned,omitempty"`
	ArtifactsDeleted   int   `json:"artifactsDeleted,omitempty"`
	BytesFreed         int64 `json:"bytesFreed,omitempty"`
	ArtifactsProtected int   `json:"artifactsProtected,omitempty"`
	ArtifactsFailed    int   `json:"artifactsFailed,omitempty"`
	ForeignObjects     int   `json:"foreignObjects,omitempty"`
}

// Duration is how long the run took.
func (p sweepPanel) Duration() time.Duration { return p.FinishedAt.Sub(p.StartedAt) }

// storageData is everything the storage dashboard shows, in both its HTML
// and its JSON form (AC12) — the HTML is derived from this, not the other
// way round (Tenet 16). The chart fields carry no JSON: an SVG string is not
// a figure a consumer could reproduce or disagree with, and the numbers
// behind every one of them are already elsewhere in this struct.
type storageData struct {
	// Available is false when this store cannot answer the dashboard's own
	// reads at all — every field below is then zero, and the page says so
	// rather than rendering an empty dashboard as though it were a store
	// that simply holds nothing (Tenet 5).
	Available bool `json:"available"`

	Driver           string `json:"driver"`
	ArtifactLocation string `json:"artifactLocation"`

	// GrandTotalBytes is the sum of every row's DocumentBytes and
	// ArtifactBytes: what the results record about their own size. It is
	// not what the bucket holds — shared evidence is in it once per result
	// that references it, and everything at its size before packing.
	GrandTotalBytes int64        `json:"grandTotalBytes"`
	Rows            []storageRow `json:"series"`

	// StoredTotalBytes is what the bucket holds for the results still
	// stored: each object once, at its stored size. It is every row's
	// StoredTotalBytes plus StoredSharedBytes, which is what more than one
	// series references, each object once, and its own row of the table;
	// StoredSharedPercent is that row's share.
	StoredTotalBytes    int64   `json:"storedTotalBytes"`
	StoredSharedBytes   int64   `json:"storedSharedBytes"`
	StoredSharedPercent float64 `json:"storedSharedPercent"`

	// UnmeasuredObjects counts the referenced objects whose stored size the
	// index has not recorded yet — what a store written before sizes were
	// recorded holds until its next sweep measures them. They are in no
	// Stored figure, and the page says so rather than reading low in silence.
	UnmeasuredObjects int `json:"unmeasuredObjects"`

	SeriesChart       template.HTML `json:"-"`
	GrowthChart       template.HTML `json:"-"`
	PruneBytesChart   template.HTML `json:"-"`
	PruneResultsChart template.HTML `json:"-"`

	LastPrune prunePanel `json:"lastPrune"`
	LastSweep sweepPanel `json:"lastSweep"`

	// SweepFoundExtra flags AC3's header note: the last sweep found
	// protected or foreign objects the grand total above does not include,
	// because the total is what the index knows is referenced, not what a
	// bucket-wide listing would find.
	SweepFoundExtra bool `json:"sweepFoundExtra"`

	SnapshotAt time.Time `json:"snapshotAt"`
}

// computeStorageData builds one snapshot. It never touches the bucket: every
// figure comes from Storage, MonthlyStorage and the maintenance run log, each
// an index-only read (AC10).
func (s *Server) computeStorageData(ctx context.Context) (storageData, error) {
	if s.deps.Storage == nil {
		return storageData{Available: false}, nil
	}

	data := storageData{
		Available: true,
		Driver:    s.deps.Store.Driver(),
		// Redacted here, where it is rendered, rather than trusted to arrive
		// that way: the page and the JSON are both built from this field, so
		// this is the one place that covers both (AC3, AC12).
		ArtifactLocation: secret.RedactURL(s.deps.ArtifactLocation),
	}

	report, err := s.deps.Storage(ctx)
	if err != nil {
		return storageData{}, fmt.Errorf("reading storage by series: %w", err)
	}

	series := report.Series

	// The page ranks by what is on disk; the API 1.0 figures ride along.
	sort.Slice(series, func(i, j int) bool { return series[i].StoredTotalBytes() > series[j].StoredTotalBytes() })

	data.GrandTotalBytes = report.TotalBytes
	data.StoredTotalBytes = report.StoredTotalBytes
	data.StoredSharedBytes = report.StoredSharedBytes
	data.StoredSharedPercent = sharePercent(report.StoredSharedBytes, report.StoredTotalBytes)
	data.UnmeasuredObjects = report.Unmeasured

	data.Rows = make([]storageRow, 0, len(series))

	barRows := make([]hBarRow, 0, len(series)+1)

	for _, sd := range series {
		data.Rows = append(data.Rows, storageRow{
			Series: sd.Series, Count: sd.Count,
			DocumentBytes: sd.DocumentBytes, ArtifactBytes: sd.ArtifactBytes,
			SharePercent:        sharePercent(sd.TotalBytes(), report.TotalBytes),
			StoredDocumentBytes: sd.StoredDocumentBytes, StoredArtifactBytes: sd.StoredArtifactBytes,
			StoredSharedBytes:  sd.StoredSharedBytes,
			StoredSharePercent: sharePercent(sd.StoredTotalBytes(), report.StoredTotalBytes),
			UnmeasuredObjects:  sd.Unmeasured,
			Oldest:             sd.Oldest, Newest: sd.Newest,
		})

		label := sd.Series.Target + " / " + string(sd.Series.Mode)
		barRows = append(barRows, hBarRow{
			Label: label,
			Detail: fmt.Sprintf("%s: document %s, artifacts %s",
				label, formatBytes(sd.StoredDocumentBytes), formatBytes(sd.StoredArtifactBytes)),
			Segments: []hBarSegment{
				{Bytes: sd.StoredDocumentBytes, Fill: "var(--accent)"},
				{Bytes: sd.StoredArtifactBytes, Fill: "var(--ok-partial)"},
			},
		})
	}

	// Drawn last, as the table's last row: it is not a series, and ranking it
	// among them would read as one.
	if report.StoredSharedBytes > 0 {
		barRows = append(barRows, hBarRow{
			Label:    sharedRowLabel,
			Detail:   fmt.Sprintf("%s: %s", sharedRowLabel, formatBytes(report.StoredSharedBytes)),
			Segments: []hBarSegment{{Bytes: report.StoredSharedBytes, Fill: "var(--muted)"}},
		})
	}

	data.SeriesChart = seriesBarSVG(barRows)
	data.GrowthChart = s.growthChart(ctx)
	data.PruneBytesChart, data.PruneResultsChart = s.pruneChart(ctx)
	data.LastPrune = s.prunePanel(ctx)
	data.LastSweep = s.sweepPanel(ctx)

	data.SweepFoundExtra = data.LastSweep.Found &&
		(data.LastSweep.ArtifactsProtected > 0 || data.LastSweep.ForeignObjects > 0)

	return data, nil
}

// sharedRowLabel names the row that holds what several series share.
const sharedRowLabel = "shared by several series"

// sharePercent is part's share of whole, out of 100, and zero for an empty
// whole.
func sharePercent(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}

	return float64(part) / float64(whole) * 100
}

// growthChart is AC5's diagram: stored bytes by month for the last twelve
// months, from the scans still stored.
func (s *Server) growthChart(ctx context.Context) template.HTML {
	if s.deps.MonthlyStorage == nil {
		return ""
	}

	since := time.Now().AddDate(0, -11, 0)

	months, err := s.deps.MonthlyStorage(ctx, since)
	if err != nil {
		s.deps.Logger.Warn("could not read storage by month for the storage dashboard", "error", err)

		return ""
	}

	labels := make([]string, len(months))
	values := make([]int64, len(months))

	for i, m := range months {
		labels[i] = m.Month.Format("Jan")
		values[i] = m.Bytes
	}

	return vBarChart("Stored bytes by month — what is still stored, each object in the month first stored",
		labels, values, formatBytes)
}

// pruneRunsChartLimit is how many of the newest prune runs the chart shows.
const pruneRunsChartLimit = 12

// pruneChart is AC6's diagram: one bar per recorded prune run, in two charts
// sharing an x-axis. A run that freed nothing still draws a zero-height bar,
// because a run of runs that freed nothing is the finding.
func (s *Server) pruneChart(ctx context.Context) (bytesChart, resultsChart template.HTML) {
	if s.deps.MaintenanceRuns == nil {
		return "", ""
	}

	runs, err := s.deps.MaintenanceRuns(ctx, store.MaintenanceKindPrune, pruneRunsChartLimit)
	if err != nil {
		s.deps.Logger.Warn("could not read prune runs for the storage dashboard", "error", err)

		return "", ""
	}

	if len(runs) == 0 {
		return "", ""
	}

	// Newest first from the store; the chart reads left to right, oldest
	// first.
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}

	labels := make([]string, len(runs))
	bytesV := make([]int64, len(runs))
	resultsV := make([]int64, len(runs))

	for i, run := range runs {
		labels[i] = run.FinishedAt.Format("Jan 2")

		stats, err := run.PruneStats()
		if err != nil {
			s.deps.Logger.Warn("a recorded prune run's stats could not be decoded", "error", err)

			continue
		}

		bytesV[i] = stats.BytesFreed
		resultsV[i] = int64(stats.ResultsDeleted)
	}

	bytesChart = vBarChart("Bytes freed by prune run, newest last", labels, bytesV, formatBytes)
	resultsChart = vBarChart("Results deleted by prune run, newest last", labels, resultsV,
		func(n int64) string { return fmt.Sprintf("%d", n) })

	return bytesChart, resultsChart
}

// prunePanel reads AC7's prune panel, naming why there is nothing to show
// when there is nothing to show.
func (s *Server) prunePanel(ctx context.Context) prunePanel {
	if s.deps.LastMaintenanceRun == nil {
		return prunePanel{Available: false}
	}

	run, found, err := s.deps.LastMaintenanceRun(ctx, store.MaintenanceKindPrune)
	if err != nil {
		return prunePanel{Available: true, Reason: "the last prune run could not be read: " + err.Error()}
	}

	if !found {
		return prunePanel{Available: true, Reason: s.noPruneReason(ctx)}
	}

	stats, err := run.PruneStats()
	if err != nil {
		return prunePanel{Available: true, Reason: "the last prune's stats could not be decoded: " + err.Error()}
	}

	return prunePanel{
		Available: true, Found: true,
		Trigger: run.Trigger, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Error: run.Error,
		ResultsDeleted: stats.ResultsDeleted, ResultsKept: stats.ResultsKept, SeriesPruned: stats.SeriesPruned,
		OldestKept: stats.OldestKept, ArtifactsDeleted: stats.ArtifactsDeleted, BytesFreed: stats.BytesFreed,
	}
}

// noPruneReason tells "no retention policy is configured" apart from "a
// policy is configured and has not yet pruned" (AC7).
func (s *Server) noPruneReason(ctx context.Context) string {
	if s.deps.Retention == nil {
		return "no retention policy is configured"
	}

	r, err := s.deps.Retention()
	if err != nil {
		return "the retention policy could not be read: " + err.Error()
	}

	if !r.Active() {
		return "no retention policy is configured"
	}

	_ = ctx // reserved: a future reading of the schedule belongs here, not a bucket call

	return "a retention policy is configured, but no prune has run yet"
}

// sweepPanel reads AC7's sweep panel. A sweep has never been run is an
// expected and named state, not an error: the daemon's first scheduled sweep
// comes shortly after it starts (Story 4.12), and a store may have been
// configured with scheduled sweeping off.
func (s *Server) sweepPanel(ctx context.Context) sweepPanel {
	if s.deps.LastMaintenanceRun == nil {
		return sweepPanel{Available: false}
	}

	run, found, err := s.deps.LastMaintenanceRun(ctx, store.MaintenanceKindSweep)
	if err != nil {
		return sweepPanel{Available: true, Reason: "the last sweep run could not be read: " + err.Error()}
	}

	if !found {
		return sweepPanel{Available: true,
			Reason: `a sweep has never been run (the daemon sweeps on a schedule unless store.sweep is false, ` +
				`and "wsaw store sweep" runs one now)`}
	}

	stats, err := run.SweepStats()
	if err != nil {
		return sweepPanel{Available: true, Reason: "the last sweep's stats could not be decoded: " + err.Error()}
	}

	return sweepPanel{
		Available: true, Found: true,
		Trigger: run.Trigger, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, Error: run.Error,
		ArtifactsScanned: stats.ArtifactsScanned, BytesScanned: stats.BytesScanned,
		ArtifactsDeleted: stats.ArtifactsDeleted, BytesFreed: stats.BytesFreed,
		ArtifactsProtected: stats.ArtifactsProtected, ArtifactsFailed: stats.ArtifactsFailed,
		ForeignObjects: stats.ForeignObjects,
	}
}

// handleUIStorage serves the dashboard (AC1), behind the same session as the
// rest of the interface, and only when there is a token for that session to
// be made from (storageAllowed).
func (s *Server) handleUIStorage(w http.ResponseWriter, r *http.Request) {
	if ok, reason := s.storageAllowed(); !ok {
		s.uiError(w, r, http.StatusForbidden, reason)

		return
	}

	force := r.URL.Query().Get("recompute") == "1"

	data, computedAt, err := s.storage.get(r.Context(), force, s.computeStorageData)
	if err != nil {
		s.uiError(w, r, http.StatusInternalServerError, err.Error())

		return
	}

	data.SnapshotAt = computedAt

	s.render(w, r, "storage.html", "Storage", data)
}

// handleStorageAPI serves AC12's JSON form of the same snapshot the HTML
// page renders — additive to the API, no version bump (Tenet 16, AGENTS.md
// §8).
func (s *Server) handleStorageAPI(w http.ResponseWriter, r *http.Request) {
	if ok, reason := s.storageAllowed(); !ok {
		writeJSONError(w, http.StatusForbidden, reason)

		return
	}

	force := r.URL.Query().Get("recompute") == "1"

	data, computedAt, err := s.storage.get(r.Context(), force, s.computeStorageData)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	data.SnapshotAt = computedAt

	writeJSON(w, http.StatusOK, data)
}
