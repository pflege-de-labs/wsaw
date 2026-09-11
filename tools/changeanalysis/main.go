// Command changeanalysis replays the stored scan history and aggregates the
// changes wsaw would have reported, broken down by severity, change type and
// asset type.
//
// Changes are derived, not stored: the store keeps results, and a report is
// produced by comparing a result with the one before it. So the history is
// replayed here with the same comparison the scanner runs, against a
// read-only copy of the database.
//
//	go run ./tools/changeanalysis -db snap.db -config wsaw.yaml -json out.json
package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func main() {
	db := flag.String("db", "", "path to a wsaw sqlite database (a copy; opened read-only)")
	cfgPath := flag.String("config", "", "wsaw.yaml, for per-target allow/deny lists and severity overrides")
	flapWindow := flag.Duration("flap", 6*time.Hour, "flap suppression window; 0 disables")
	jsonOut := flag.String("json", "", "also write the aggregate as JSON to this path")
	csvOut := flag.String("csv", "", "also write every emitted change as CSV to this path")
	flag.Parse()

	if *db == "" {
		fail(fmt.Errorf("-db is required"))
	}

	opts, err := targetOptions(*cfgPath)
	if err != nil {
		fail(err)
	}

	series, err := load(*db)
	if err != nil {
		fail(err)
	}

	agg := replay(series, opts, *flapWindow)

	fmt.Print(agg.render())

	if *csvOut != "" {
		if err := agg.writeCSV(*csvOut); err != nil {
			fail(err)
		}
	}

	if *jsonOut != "" {
		b, err := json.MarshalIndent(agg, "", "  ")
		if err != nil {
			fail(err)
		}

		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "changeanalysis:", err)
	os.Exit(1)
}

// targetOptions reads the comparison options each target runs with, so the
// replay suppresses the same allow-listed changes the daemon does.
func targetOptions(path string) (map[string]diff.Options, error) {
	out := map[string]diff.Options{}

	if path == "" {
		return out, nil
	}

	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}

	targets, err := cfg.ResolveTargets(nil)
	if err != nil {
		return nil, err
	}

	for _, t := range targets {
		out[t.Name] = diff.Options{
			Allow:                t.Allow,
			Deny:                 t.Deny,
			Severity:             t.Severity,
			DegradedFailureRatio: cfg.Detection.DegradedFailureRatio,
		}
	}

	return out, nil
}

type seriesKey struct {
	target string
	mode   model.ConsentMode
}

// load reads every stored result, grouped into series and ordered the way the
// store orders them.
func load(path string) (map[seriesKey][]*model.Result, error) {
	conn, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	rows, err := conn.Query(`select document from results
		order by target, consent_mode, started_at, scan_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[seriesKey][]*model.Result{}

	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}

		var res model.Result
		if err := json.Unmarshal([]byte(doc), &res); err != nil {
			return nil, err
		}

		k := seriesKey{target: res.Target, mode: res.ConsentMode}
		out[k] = append(out[k], &res)
	}

	return out, rows.Err()
}

// comparison is one replayed diff, kept with its timestamp so the whole set
// can be flap-suppressed in the order the daemon saw them.
type comparison struct {
	at       time.Time
	key      seriesKey
	report   *diff.Report
	baseline *model.Result
	current  *model.Result
}

func replay(series map[seriesKey][]*model.Result, opts map[string]diff.Options, flap time.Duration) *aggregate {
	agg := newAggregate(flap)

	var all []comparison

	for key, results := range series {
		agg.Scans += len(results)

		// The scanner compares against the previous *usable* result: an
		// errored or skipped scan is never a baseline, because its empty
		// asset list would report the whole site as removed.
		var prev *model.Result

		for _, res := range results {
			if prev != nil {
				rep := diff.Compare(prev, res, opts[key.target])
				all = append(all, comparison{at: res.StartedAt, key: key, report: rep, baseline: prev, current: res})
			} else {
				agg.SeriesFirstScans++
			}

			if res.Termination != model.TermError && res.Termination != model.TermSkipped {
				prev = res
			}
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}

		return all[i].current.ScanID < all[j].current.ScanID
	})

	suppressor := diff.NewFlapSuppressor(flap)

	for _, c := range all {
		agg.Comparisons++

		if c.report == nil {
			continue
		}

		if !c.report.Comparable {
			agg.Incomparable++
			agg.IncomparableReasons[c.report.Reason]++
		}

		emit, suppressed := suppressor.Filter(c.at, c.report.Changes)
		agg.Suppressed += suppressed
		agg.AllowSuppressed += c.report.Suppressed
		agg.Raw += len(c.report.Changes)

		if len(emit) > 0 {
			agg.ComparisonsWithChanges++
		}

		for _, ch := range c.report.Changes {
			agg.RawBySeverity[string(ch.Severity)]++
		}

		for _, ch := range emit {
			agg.count(c, ch)
		}
	}

	return agg
}

type window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type aggregate struct {
	FlapWindow string `json:"flapWindow"`
	Window     window `json:"window"`

	Scans                  int `json:"scans"`
	SeriesFirstScans       int `json:"seriesFirstScans"`
	Comparisons            int `json:"comparisons"`
	ComparisonsWithChanges int `json:"comparisonsWithChanges"`
	Incomparable           int `json:"incomparable"`
	Raw                    int `json:"rawChanges"`
	Suppressed             int `json:"flapSuppressed"`
	AllowSuppressed        int `json:"allowListSuppressed"`
	Emitted                int `json:"emittedChanges"`

	IncomparableReasons map[string]int `json:"incomparableReasons"`
	RawBySeverity       map[string]int `json:"rawBySeverity"`

	BySeverity    map[string]int            `json:"bySeverity"`
	ByType        map[string]int            `json:"byType"`
	ByAssetType   map[string]int            `json:"byAssetType"`
	ByParty       map[string]int            `json:"byParty"`
	ByMode        map[string]int            `json:"byConsentMode"`
	ByTarget      map[string]int            `json:"byTarget"`
	TypeSeverity  map[string]map[string]int `json:"typeBySeverity"`
	AssetSeverity map[string]map[string]int `json:"assetTypeBySeverity"`
	TargetSever   map[string]map[string]int `json:"targetBySeverity"`
	ModeSeverity  map[string]map[string]int `json:"consentModeBySeverity"`
	TopDomains    map[string]int            `json:"byDomain"`
	TopSubjects   map[string]int            `json:"bySubject"`

	first, last time.Time

	// rows keeps every emitted change for the CSV dump, so a breakdown can be
	// re-cut without replaying the history again.
	rows [][]string
}

func (a *aggregate) writeCSV(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)

	header := []string{"at", "target", "consentMode", "type", "severity", "assetType", "party", "phase", "domain", "subject", "before", "after", "detail"}
	if err := w.Write(header); err != nil {
		return err
	}

	if err := w.WriteAll(a.rows); err != nil {
		return err
	}

	w.Flush()

	return w.Error()
}

func newAggregate(flap time.Duration) *aggregate {
	return &aggregate{
		FlapWindow:          flap.String(),
		IncomparableReasons: map[string]int{},
		RawBySeverity:       map[string]int{},
		BySeverity:          map[string]int{},
		ByType:              map[string]int{},
		ByAssetType:         map[string]int{},
		ByParty:             map[string]int{},
		ByMode:              map[string]int{},
		ByTarget:            map[string]int{},
		TypeSeverity:        map[string]map[string]int{},
		AssetSeverity:       map[string]map[string]int{},
		TargetSever:         map[string]map[string]int{},
		ModeSeverity:        map[string]map[string]int{},
		TopDomains:          map[string]int{},
		TopSubjects:         map[string]int{},
	}
}

func (a *aggregate) count(c comparison, ch diff.Change) {
	a.Emitted++

	if a.first.IsZero() || c.at.Before(a.first) {
		a.first = c.at
	}

	if c.at.After(a.last) {
		a.last = c.at
	}

	sev := string(ch.Severity)
	typ := string(ch.Type)
	asset := assetType(c, ch)

	a.BySeverity[sev]++
	a.ByType[typ]++
	a.ByAssetType[asset]++
	a.ByMode[string(ch.ConsentMode)]++
	a.ByTarget[ch.Target]++

	party := string(ch.Party)
	if party == "" {
		party = "n/a"
	}

	a.ByParty[party]++

	bump(a.TypeSeverity, typ, sev)
	bump(a.AssetSeverity, asset, sev)
	bump(a.TargetSever, ch.Target, sev)
	bump(a.ModeSeverity, string(ch.ConsentMode), sev)

	if ch.Domain != "" {
		a.TopDomains[ch.Domain]++
	}

	if ch.Subject != "" {
		a.TopSubjects[trim(ch.Subject, 110)]++
	}

	a.rows = append(a.rows, []string{
		c.at.UTC().Format(time.RFC3339), ch.Target, string(ch.ConsentMode), typ, sev,
		asset, party, string(ch.Phase), ch.Domain, ch.Subject, ch.Before, ch.After, ch.Detail,
	})
}

func bump(m map[string]map[string]int, outer, inner string) {
	if m[outer] == nil {
		m[outer] = map[string]int{}
	}

	m[outer][inner]++
}

// assetType names the kind of thing that changed. Asset- and script-level
// changes carry a normalized URL, so the resource type is looked up in the
// result that observed it — the current one for an addition, the baseline for
// a removal. Host, cookie and scan-level changes have no asset behind them
// and are reported as their own kind.
func assetType(c comparison, ch diff.Change) string {
	switch ch.Type {
	case diff.HostAdded, diff.HostRemoved, diff.DeniedHost:
		return "(host)"
	case diff.CookieAdded, diff.CookieRemoved:
		return "(cookie)"
	case diff.ConsentChanged:
		return "(consent)"
	case diff.ScanDegraded:
		return "(scan)"
	}

	if t := resourceType(c.current, ch.Subject); t != "" {
		return t
	}

	if t := resourceType(c.baseline, ch.Subject); t != "" {
		return t
	}

	return "unknown"
}

func resourceType(res *model.Result, normalized string) string {
	if res == nil {
		return ""
	}

	for i := range res.Requests {
		if res.Requests[i].NormalizedURL == normalized {
			if t := res.Requests[i].ResourceType; t != "" {
				return strings.ToLower(t)
			}

			return "other"
		}
	}

	return ""
}

var severities = []string{"critical", "high", "medium", "low", "info"}

func (a *aggregate) render() string {
	var b strings.Builder

	if !a.first.IsZero() {
		a.Window.From = a.first.UTC().Format(time.RFC3339)
		a.Window.To = a.last.UTC().Format(time.RFC3339)
	}

	fmt.Fprintf(&b, "scans %d, comparisons %d (first-in-series %d, incomparable %d)\n",
		a.Scans, a.Comparisons, a.SeriesFirstScans, a.Incomparable)
	fmt.Fprintf(&b, "changes raw %d, allow-list suppressed %d, flap-suppressed %d (window %s), emitted %d, in %d of %d comparisons\n",
		a.Raw, a.AllowSuppressed, a.Suppressed, a.FlapWindow, a.Emitted, a.ComparisonsWithChanges, a.Comparisons)
	fmt.Fprintf(&b, "window %s .. %s\n\n", a.Window.From, a.Window.To)

	section(&b, "by severity", a.BySeverity, a.Emitted)
	section(&b, "by change type", a.ByType, a.Emitted)
	section(&b, "by asset type", a.ByAssetType, a.Emitted)
	section(&b, "by party", a.ByParty, a.Emitted)
	section(&b, "by consent mode", a.ByMode, a.Emitted)
	section(&b, "by target", a.ByTarget, a.Emitted)
	section(&b, "incomparable reasons", a.IncomparableReasons, a.Incomparable)

	matrix(&b, "change type x severity", a.TypeSeverity)
	matrix(&b, "asset type x severity", a.AssetSeverity)
	matrix(&b, "target x severity", a.TargetSever)
	matrix(&b, "consent mode x severity", a.ModeSeverity)

	topN(&b, "top domains", a.TopDomains, 25)
	topN(&b, "top subjects", a.TopSubjects, 25)

	return b.String()
}

func section(b *strings.Builder, title string, m map[string]int, tot int) {
	if len(m) == 0 {
		return
	}

	fmt.Fprintf(b, "%s\n", title)

	for _, e := range sorted(m) {
		share := 0.0
		if tot > 0 {
			share = 100 * float64(e.v) / float64(tot)
		}

		fmt.Fprintf(b, "  %-28s %6d  %5.1f%%\n", e.k, e.v, share)
	}

	fmt.Fprintln(b)
}

func matrix(b *strings.Builder, title string, m map[string]map[string]int) {
	if len(m) == 0 {
		return
	}

	fmt.Fprintf(b, "%s\n", title)
	fmt.Fprintf(b, "  %-28s", "")

	for _, s := range severities {
		fmt.Fprintf(b, "%9s", s)
	}

	fmt.Fprintf(b, "%9s\n", "total")

	rows := make([]string, 0, len(m))
	for k := range m {
		rows = append(rows, k)
	}

	sort.Slice(rows, func(i, j int) bool { return total(m[rows[i]]) > total(m[rows[j]]) })

	for _, r := range rows {
		fmt.Fprintf(b, "  %-28s", r)

		for _, s := range severities {
			fmt.Fprintf(b, "%9d", m[r][s])
		}

		fmt.Fprintf(b, "%9d\n", total(m[r]))
	}

	fmt.Fprintln(b)
}

func topN(b *strings.Builder, title string, m map[string]int, n int) {
	if len(m) == 0 {
		return
	}

	fmt.Fprintf(b, "%s (%d distinct)\n", title, len(m))

	for i, e := range sorted(m) {
		if i >= n {
			break
		}

		fmt.Fprintf(b, "  %-110s %6d\n", e.k, e.v)
	}

	fmt.Fprintln(b)
}

type kv struct {
	k string
	v int
}

func sorted(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}

		return out[i].k < out[j].k
	})

	return out
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}

	return n
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n-1] + "…"
}
