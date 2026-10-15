package store_test

import (
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

func botBlocked(id string, at time.Time) *model.Result {
	res := result(id, at, model.ConsentReject)
	res.Consent = model.Consent{
		Outcome:         model.OutcomeFailed,
		Kind:            model.CMPKindVendor,
		CMP:             "CCM19",
		LoaderRequestID: "7.2",
		BotDetected:     true,
	}

	return res
}

// Story 2.11, AC5: the dashboard marks a tile from the listing alone, so the
// listing carries the bot finding, and a rebuild restores it from the
// document without then disagreeing with it.
func TestTheListingCarriesTheBotFinding(t *testing.T) {
	t.Parallel()

	opts := storeOptions(t)
	first := openAt(t, opts)
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

	if err := first.PutResult(botBlocked("scan-bot", at)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	if err := first.PutResult(result("scan-plain", at.Add(-time.Hour), model.ConsentReject)); err != nil {
		t.Fatalf("PutResult: %v", err)
	}

	check := func(s store.Store, when string) {
		t.Helper()

		sums, err := s.ListResults("site", model.ConsentReject, 0)
		if err != nil || len(sums) != 2 {
			t.Fatalf("ListResults %s = %d, %v", when, len(sums), err)
		}

		if !sums[0].ConsentBotDetected {
			t.Errorf("%s: the bot-blocked scan lists without its finding", when)
		}

		if sums[1].ConsentBotDetected {
			t.Errorf("%s: an ordinary scan lists as bot-blocked", when)
		}
	}

	check(first, "as written")

	second := openAt(t, lostIndex(t, opts))

	if _, err := second.RebuildIndex(t.Context(), store.RebuildOptions{}); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	check(second, "after a rebuild")

	again, err := second.RebuildIndex(t.Context(), store.RebuildOptions{})
	if err != nil {
		t.Fatalf("second RebuildIndex: %v", err)
	}

	if again.EntriesAdded != 0 || again.EntriesRefreshed != 0 {
		t.Errorf("a second rebuild added %d and refreshed %d entries; the bot column must already agree",
			again.EntriesAdded, again.EntriesRefreshed)
	}
}
