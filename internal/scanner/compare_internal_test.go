package scanner

import (
	"slices"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/diff"
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

// Story 4.15, AC4 and AC8: the scan stores its watched text as an artifact,
// the comparison behind the notification reads the base's back from there,
// and it applies the target's own text checks — so text-incomplete and
// text-changed reach the notifier, not only the scan page.
func TestTheNotificationComparesTheStoredText(t *testing.T) {
	t.Parallel()

	st := &comparisonStore{}
	s := &Scanner{deps: Deps{Store: st}}
	target := config.Resolved{Name: "site", TextChecks: &diff.TextChecks{MustContain: []string{"HRB 12345"}}}

	withText := func(id, text string) *model.Result {
		res := cleanResult()
		res.ScanID = id
		res.Text = &model.TextCapture{Selector: "main", Text: text, Length: len(text), SHA256: id}
		s.storeText(res, quiet())

		return res
	}

	before := withText("scan-0", "Impressum\nHRB 12345")
	if before.Text.Ref == "" || before.Text.Bytes == 0 || before.Text.Unavailable != "" {
		t.Fatalf("text not stored: %+v", before.Text)
	}

	// The base comes back the way the store hands it out: without the text.
	stored := *before.Text
	stored.Text, stored.Links = "", nil
	base := *before
	base.Text = &stored
	st.previous = &base

	rep := s.compare(target, withText("scan-1", "Impressum"), quiet())

	var types []diff.ChangeType
	for _, c := range rep.Changes {
		types = append(types, c.Type)
	}

	if !slices.Contains(types, diff.TextChanged) || !slices.Contains(types, diff.TextIncomplete) {
		t.Errorf("changes = %v; want text-changed and text-incomplete", types)
	}
}
