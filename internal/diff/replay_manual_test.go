//go:build replay

// Replay the operator's real stored scans through this package, to check the
// rules against the data that motivated them rather than against fixtures.
//
//	go test -tags replay ./internal/diff/ -run Replay -v
//
// Behind a build tag because it reads a local database that only exists on
// the machine the scans were taken on. Set WSAW_REPLAY_DB to point at one
// elsewhere; without it the platform state directory is used and the test
// skips when there is nothing to read.
package diff_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func load(t *testing.T, target, mode string) []*model.Result {
	t.Helper()

	path := os.Getenv("WSAW_REPLAY_DB")
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			t.Skip(err)
		}

		path = filepath.Join(dir, "wsaw", "wsaw.db")
	}

	if _, err := os.Stat(path); err != nil {
		t.Skipf("no local scan database (set WSAW_REPLAY_DB): %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Skip(err)
	}

	defer func() { _ = db.Close() }()

	rows, err := db.Query(
		`select document from results where target = ? and consent_mode = ? order by started_at asc`,
		target, mode)
	if err != nil {
		t.Skip(err)
	}

	defer func() { _ = rows.Close() }()

	var out []*model.Result

	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			t.Fatal(err)
		}

		var res model.Result
		if err := json.Unmarshal([]byte(doc), &res); err != nil {
			t.Fatal(err)
		}

		out = append(out, &res)
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

func tally(results []*model.Result, opts diff.Options) (noisy int, byType map[diff.ChangeType]int) {
	byType = map[diff.ChangeType]int{}

	for i := 1; i < len(results); i++ {
		rep := diff.Compare(results[i-1], results[i], opts)

		counted := 0

		for _, c := range rep.Changes {
			byType[c.Type]++

			if c.Type != diff.ScanDegraded {
				counted++
			}
		}

		if counted > 0 {
			noisy++
		}
	}

	return noisy, byType
}

func report(t *testing.T, label string, noisy, transitions int, byType map[diff.ChangeType]int) {
	t.Helper()

	kinds := make([]string, 0, len(byType))
	for k := range byType {
		kinds = append(kinds, string(k))
	}

	sort.Strings(kinds)

	t.Logf("%-28s %2d/%2d transitions with a non-degradation change", label, noisy, transitions)

	for _, k := range kinds {
		t.Logf("      %-16s %4d", k, byType[diff.ChangeType(k)])
	}
}

func TestReplayRealSeries(t *testing.T) {
	results := load(t, "pflege.de", "none")
	if len(results) < 2 {
		t.Skip("not enough stored scans")
	}

	t.Logf("%d scans of pflege.de/none", len(results))

	// The true "before": every request presented as though it had completed,
	// which is how these comparisons behaved before any of this existed.
	// Attribution is unconditional by design, so it cannot be switched off
	// through Options — the fixture has to be flattened instead.
	unruled := make([]*model.Result, len(results))
	for i, res := range results {
		flat := *res
		flat.Requests = stripIncomplete(res.Requests)
		unruled[i] = &flat
	}

	raw, rawTypes := tally(unruled, diff.Options{DegradedFailureRatio: 1.5})
	report(t, "no rules", raw, len(unruled)-1, rawTypes)

	off, offTypes := tally(results, diff.Options{DegradedFailureRatio: 1.5})
	report(t, "attribution only", off, len(results)-1, offTypes)

	on, onTypes := tally(results, diff.Options{})
	report(t, "attribution + 5% gate", on, len(results)-1, onTypes)

	// What survives, and whether the scan that produced it was clean. A
	// removal from a scan that lost nothing is a real observation; one from a
	// scan that lost requests is a gap the rules did not attribute.
	type survivor struct {
		subject string
		clean   int
		dirty   int
	}

	survivors := map[string]*survivor{}

	for i := 1; i < len(results); i++ {
		cur := results[i]
		lost := cur.IncompleteObservations()

		for _, c := range diff.Compare(results[i-1], cur, diff.Options{}).Changes {
			if c.Type != diff.AssetRemoved && c.Type != diff.HostRemoved {
				continue
			}

			sv := survivors[c.Subject]
			if sv == nil {
				sv = &survivor{subject: c.Subject}
				survivors[c.Subject] = sv
			}

			if lost == 0 {
				sv.clean++
			} else {
				sv.dirty++
			}
		}
	}

	list := make([]*survivor, 0, len(survivors))
	for _, sv := range survivors {
		list = append(list, sv)
	}

	sort.Slice(list, func(a, b int) bool {
		if list[a].clean+list[a].dirty != list[b].clean+list[b].dirty {
			return list[a].clean+list[a].dirty > list[b].clean+list[b].dirty
		}

		return list[a].subject < list[b].subject
	})

	cleanTotal, dirtyTotal := 0, 0
	for _, sv := range list {
		cleanTotal += sv.clean
		dirtyTotal += sv.dirty
	}

	t.Logf("surviving removals: %d from scans that lost nothing, %d from scans that lost requests",
		cleanTotal, dirtyTotal)

	for _, sv := range list[:min(20, len(list))] {
		t.Logf("      clean=%2d dirty=%2d  %s", sv.clean, sv.dirty, sv.subject)
	}

	if onTypes[diff.ScanDegraded] == 0 {
		t.Error("no scan was reported degraded, yet the series is full of transport failures")
	}

	// The rules exist to take the phantom removals out of this series. The
	// residue is the cache-buster churn a query-parameter rule handles, which
	// is a separate concern and is not configured here.
	const wantReduction = 0.8

	if before, after := rawTypes[diff.AssetRemoved], onTypes[diff.AssetRemoved]; before > 0 {
		got := 1 - float64(after)/float64(before)
		if got < wantReduction {
			t.Errorf("asset-removed fell only %.0f%% (%d to %d); want at least %.0f%%",
				got*100, before, after, wantReduction*100)
		}
	}

	if onTypes[diff.HostRemoved] > 0 {
		t.Errorf("%d host-removed survived; a host that stopped being contacted is only "+
			"provable from a scan that observed everything", onTypes[diff.HostRemoved])
	}

	// Additions must not be collateral: whatever the gate does to removals, a
	// host appearing has to keep being reported.
	if onTypes[diff.HostAdded] != rawTypes[diff.HostAdded] {
		t.Errorf("the rules changed host-added from %d to %d; additions must survive",
			rawTypes[diff.HostAdded], onTypes[diff.HostAdded])
	}

	if onTypes[diff.AssetAdded] != rawTypes[diff.AssetAdded] {
		t.Errorf("the rules changed asset-added from %d to %d; additions must survive",
			rawTypes[diff.AssetAdded], onTypes[diff.AssetAdded])
	}
}

// TestReplayGTMIdentityOnRealBodies checks the rule end to end on the stored
// container bodies: the extraction, and what the diff then does with it.
func TestReplayGTMIdentityOnRealBodies(t *testing.T) {
	results := load(t, "pflege.de", "none")
	if len(results) < 2 {
		t.Skip("not enough stored scans")
	}

	artifacts := os.Getenv("WSAW_REPLAY_ARTIFACTS")
	if artifacts == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			t.Skip(err)
		}

		artifacts = filepath.Join(dir, "wsaw", "artifacts")
	}

	extract := regexp.MustCompile(`"version":"(\d+)"`)

	// Fill in the identity the way capture now would, from the stored body.
	filled := 0

	for _, res := range results {
		for i := range res.Requests {
			req := &res.Requests[i]
			if req.BodyRef == "" || !regexp.MustCompile(`googletagmanager\.com/gtm\.js`).MatchString(req.URL) {
				continue
			}

			body, err := os.ReadFile(filepath.Join(artifacts, req.BodyRef))
			if err != nil {
				continue
			}

			if m := extract.FindSubmatch(body); m != nil {
				req.BodyIdentity = string(m[1])
				req.BodyIdentityLabel = "GTM container version"
				filled++
			}
		}
	}

	if filled == 0 {
		t.Skip("no stored gtm.js bodies to read")
	}

	t.Logf("extracted a container version from %d stored gtm.js bodies", filled)

	// Retention prunes stored bodies, so only some scans could be backfilled.
	// A pair where neither side carries an identity still falls back to the
	// digest, which is correct — so the two paths are counted separately.
	gtm := regexp.MustCompile(`gtm\.js`)
	isVersion := regexp.MustCompile(`^\d+→\d+$`)

	count := func(withIdentity bool) (total, viaIdentity int, versions map[string]bool) {
		versions = map[string]bool{}

		for i := 1; i < len(results); i++ {
			before, after := *results[i-1], *results[i]

			if !withIdentity {
				before.Requests = stripIdentity(before.Requests)
				after.Requests = stripIdentity(after.Requests)
			}

			for _, c := range diff.Compare(&before, &after, diff.Options{}).Changes {
				if c.Type != diff.ScriptChanged || !gtm.MatchString(c.Subject) {
					continue
				}

				total++

				// The label in the detail is what marks the identity path.
				if strings.Contains(c.Detail, "GTM container version") {
					viaIdentity++
					versions[c.Before+"→"+c.After] = true
				}
			}
		}

		return total, viaIdentity, versions
	}

	byDigest, _, _ := count(false)
	total, viaIdentity, versions := count(true)

	t.Logf("gtm.js script-changed, digest only:        %d", byDigest)
	t.Logf("gtm.js script-changed, identity in play:   %d (%d of them via the identity)",
		total, viaIdentity)

	for v := range versions {
		t.Logf("      container %s", v)
	}

	if total >= byDigest {
		t.Errorf("the identity rule did not reduce gtm.js churn: %d with it vs %d without",
			total, byDigest)
	}

	if viaIdentity == 0 {
		t.Error("no change came through the identity path, so the rule was never exercised")
	}

	// Every change the identity path produced must be a version transition —
	// that is the whole claim. Digest-path changes on un-backfilled pairs are
	// expected and are not asserted on.
	for v := range versions {
		if !isVersion.MatchString(v) {
			t.Errorf("an identity change is not a version transition: %q", v)
		}
	}
}

// stripIncomplete presents every request as though it had completed, to
// recover the comparison as it behaved before incomplete observations were
// taken into account.
func stripIncomplete(reqs []model.Request) []model.Request {
	out := make([]model.Request, len(reqs))
	copy(out, reqs)

	for i := range out {
		req := &out[i]

		// Clearing the failure and giving the request an end offset is enough
		// to defeat both halves of Incomplete(). The status is left alone, so
		// this does not invent observations the scan never made.
		req.Failed = false
		req.FailureReason = ""

		if req.Timing.EndOffset == 0 {
			req.Timing.EndOffset = req.Timing.StartOffset + 1
		}
	}

	return out
}

func stripIdentity(reqs []model.Request) []model.Request {
	out := make([]model.Request, len(reqs))
	copy(out, reqs)

	for i := range out {
		out[i].BodyIdentity = ""
		out[i].BodyIdentityLabel = ""
	}

	return out
}
