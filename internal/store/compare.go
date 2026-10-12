package store

import (
	"errors"
	"fmt"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// ComparisonReader is the part of a store that choosing what a scan is
// compared against reads.
type ComparisonReader interface {
	GetBaseline(target string, mode model.ConsentMode) (*Baseline, error)
	PreviousResult(target string, mode model.ConsentMode, scanID string) (*model.Result, error)
}

// Comparison is what a scan is measured against by default.
type Comparison struct {
	// Base is the result to compare with, or nil when there is none.
	Base *model.Result

	// Baseline is the series' approved baseline, or nil when it has none.
	Baseline *Baseline

	// OwnBaseline says the scan is that baseline. Comparing it with itself
	// would report "no changes", which is true, says nothing, and reads
	// exactly like a scan that genuinely changed nothing (Tenet 5) — so such
	// a scan is compared with the previous stored scan instead, as a series
	// without a baseline is (Story 5.36, AC5).
	OwnBaseline bool

	// EvidenceGone says the previous result is indexed but its document has
	// left the artifact bucket. A nil Base then means a comparison was owed
	// and could not be made, not that there is nothing earlier (Story 8.2,
	// AC5; Tenet 5).
	EvidenceGone bool
}

// ComparisonFor picks what res is compared against: the approved baseline,
// falling back to the previous scan — and to the previous scan also when res
// is itself the baseline. Only a read failure other than "not found" or
// "evidence gone" is an error.
func ComparisonFor(src ComparisonReader, res *model.Result) (Comparison, error) {
	var c Comparison

	b, err := src.GetBaseline(res.Target, res.ConsentMode)

	switch {
	case err == nil:
		c.Baseline = b
		c.OwnBaseline = b.ScanID == res.ScanID

	case !errors.Is(err, ErrNotFound):
		return c, fmt.Errorf("reading the baseline of %s/%s: %w", res.Target, res.ConsentMode, err)
	}

	if c.Baseline != nil && !c.OwnBaseline {
		c.Base = c.Baseline.Result

		return c, nil
	}

	prev, err := src.PreviousResult(res.Target, res.ConsentMode, res.ScanID)

	switch {
	case err == nil:
		c.Base = prev

	case errors.Is(err, ErrEvidenceGone):
		c.EvidenceGone = true

	case !errors.Is(err, ErrNotFound):
		return c, fmt.Errorf("reading the scan before %s: %w", res.ScanID, err)
	}

	return c, nil
}

// Report diffs res against c.Base, with the reason a nil Base has when it is
// not the plain "no baseline" case Compare already explains.
func (c Comparison) Report(res *model.Result, opts diff.Options) *diff.Report {
	rep := diff.Compare(c.Base, res, opts)

	switch {
	case c.EvidenceGone:
		rep.Reason = diff.ReasonEvidenceGone

	case c.OwnBaseline && c.Base == nil:
		// Compare would say there is no baseline, which is the one thing
		// that is not true of this scan.
		rep.Reason = diff.ReasonFirstBaseline
	}

	return rep
}
