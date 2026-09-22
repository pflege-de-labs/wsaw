package httpapi

import (
	"slices"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// How the watchboard ranks a scan's changes (Story 5.30).
//
// diff.DefaultSeverityRules ranks for the notification and CI exit-code
// contract, where a third-party script appearing or changing under a stable
// URL is a supply-chain finding. A board asks a narrower question. A live site
// re-deploys its bundles under new hashed filenames most days, so asset-added,
// asset-removed and script-changed fire on nearly every series on nearly every
// scan, and a board where most tiles are marked is a board nobody reads.
//
// So the tile ranks the thing the board exists to answer — did a third party
// appear in a scan where the visitor had agreed to nothing — and leaves the
// count-shaped drift at info. Everything the rules do not name keeps the
// engine's own severity: a consent regression, a denied host, a degraded scan
// or a third-party cookie surviving a rejection is not count-shaped drift, and
// bleaching one of those out of the board would be the quiet lie Tenet 5
// forbids.
//
// This is the dashboard's ranking, not the product's. GET /api/v1/targets
// keeps reporting the diff engine's severity for the same comparison
// (Tenet 16) — the same split the "hosts only" toggle already makes.

// severityView selects how targetViews ranks a series' Severity.
type severityView struct {
	// HostsOnly narrows the ranking to hostChangeTypes, the dashboard's
	// per-viewer toggle (Story 5.23, AC8). It narrows what gets ranked; it
	// does not change how a change that survives it is ranked.
	HostsOnly bool

	// Tile applies the watchboard's own ranking instead of the diff engine's.
	// Only the HTML dashboard sets it (Story 5.30, AC7).
	Tile bool
}

// seriesSeverity condenses one comparison to the single severity a view shows.
//
// A report with no changes ranks info, exactly as diff.Report.MaxSeverity
// ranks it, so "nothing changed" keeps the value the board already renders.
func seriesSeverity(rep *diff.Report, mode model.ConsentMode, view severityView) diff.Severity {
	if !view.Tile {
		if view.HostsOnly {
			return rep.MaxSeverityOf(hostChangeTypes...)
		}

		return rep.MaxSeverity()
	}

	worst := diff.SeverityInfo

	for _, c := range rep.Changes {
		if view.HostsOnly && !slices.Contains(hostChangeTypes, c.Type) {
			continue
		}

		if s := tileSeverity(c, mode); s.Rank() > worst.Rank() {
			worst = s
		}
	}

	return worst
}

// tileSeverity ranks one change as the watchboard ranks it.
//
// mode is the series' consent mode rather than the change's own: they are the
// same scan's mode, and taking it from the caller keeps the ranking readable
// beside the tile it ranks.
func tileSeverity(c diff.Change, mode model.ConsentMode) diff.Severity {
	switch c.Type {
	// A request count moving, and an asset changing under a stable URL. The
	// tile already states the request delta as a signed number (Story 5.28,
	// AC1), which is the whole of what a reader does with it.
	case diff.AssetAdded, diff.AssetRemoved, diff.ScriptChanged:
		return diff.SeverityInfo

	// A site contacting fewer hosts than it did. A board that marks for that
	// teaches its reader to dismiss the marking.
	case diff.HostRemoved:
		return diff.SeverityInfo

	case diff.HostAdded:
		return tileHostAdded(c, mode)

	default:
		return c.Severity
	}
}

// tileHostAdded ranks a host this scan contacted and the base did not.
//
// The consent mode decides it, and the consent phase deliberately does not: in
// accept mode the visitor agreed to be tracked, so a new vendor is an expected
// consequence wherever in the page it fires, and the pre-consent fact stays on
// the tile as its "pre" figure and its is-pre marking rather than as a rank
// or a colour (Story 5.30, AC5).
//
// none ranks with reject, because "the visitor never agreed" and "the visitor
// refused" are both a third party contacted without consent.
func tileHostAdded(c diff.Change, mode model.ConsentMode) diff.Severity {
	if c.Party != model.ThirdParty {
		// Not named by the tile's rules, so it keeps the engine's rank.
		return c.Severity
	}

	if mode == model.ConsentAccept {
		return diff.SeverityInfo
	}

	return diff.SeverityCritical
}
