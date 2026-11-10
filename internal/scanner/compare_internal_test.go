package scanner

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// comparisonStore answers the two reads choosing a comparison base makes.
type comparisonStore struct {
	durationStore

	baseline *store.Baseline
	previous *model.Result
}

func (c *comparisonStore) GetBaseline(string, model.ConsentMode) (*store.Baseline, error) {
	if c.baseline == nil {
		return nil, store.ErrNotFound
	}

	return c.baseline, nil
}

func (c *comparisonStore) PreviousResult(string, model.ConsentMode, string) (*model.Result, error) {
	if c.previous == nil {
		return nil, store.ErrNotFound
	}

	return c.previous, nil
}

func withThirdParty(scanID string, domains ...string) *model.Result {
	res := cleanResult()
	res.ScanID = scanID

	for _, d := range domains {
		res.Requests = append(res.Requests, model.Request{
			URL: "https://" + d + "/t.js", Domain: d, Party: model.ThirdParty, Status: 200,
		})
	}

	return res
}

// The notification compares against what the scan's page compares against
// (store.ComparisonFor): the approved baseline when there is one, else the
// previous scan. A notification measured against another scan than the page
// it links to reports changes and severities the page does not show.
func TestCompareUsesTheSameBaseAsTheScanPage(t *testing.T) {
	t.Parallel()

	approved := withThirdParty("scan-approved", "tracker.example")
	previous := withThirdParty("scan-previous")

	tests := []struct {
		name     string
		st       *comparisonStore
		wantBase string
	}{
		{
			name:     "approved baseline wins over the previous scan",
			st:       &comparisonStore{baseline: &store.Baseline{ScanID: approved.ScanID, Result: approved}, previous: previous},
			wantBase: approved.ScanID,
		},
		{
			name:     "previous scan without a baseline",
			st:       &comparisonStore{previous: previous},
			wantBase: previous.ScanID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &Scanner{deps: Deps{Store: tt.st}}
			res := withThirdParty("scan-1", "tracker.example")

			rep := s.compare(config.Resolved{Name: "site"}, res, quiet())
			if rep == nil {
				t.Fatal("compare returned no report")
			}

			if rep.BaselineScanID != tt.wantBase {
				t.Errorf("compared against %q, want %q", rep.BaselineScanID, tt.wantBase)
			}
		})
	}
}
