package diff_test

// Story 2.9, AC5: Web Storage is compared the way cookies are. A site that
// keeps its identifiers in localStorage changes nothing a cookie diff can
// see, and "no new cookies" would report that as no change at all.

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func storage(entries ...model.StorageEntry) func(*model.Result) {
	return func(r *model.Result) { r.Storage = entries }
}

func entry(origin, key string, area model.StorageArea, party model.Party) model.StorageEntry {
	return model.StorageEntry{
		Origin:      origin,
		Area:        area,
		Key:         key,
		Party:       party,
		ValueSHA256: "digest-" + key,
		ValueLength: len(key),
	}
}

func withStorage(mode model.ConsentMode, opts ...func(*model.Result)) *model.Result {
	r := result(mode)

	for _, o := range opts {
		o(r)
	}

	return r
}

func TestStorageKeyAppearingIsAChange(t *testing.T) {
	t.Parallel()

	before := withStorage(model.ConsentAccept)
	after := withStorage(model.ConsentAccept,
		storage(entry("https://example.com", "cookie-accepted", model.StorageLocal, model.FirstParty)))

	c := find(t, diff.Compare(before, after, diff.Options{}),
		diff.StorageAdded, "https://example.com|local:cookie-accepted")

	if c.Severity != diff.SeverityLow {
		t.Errorf("severity = %q, want low for a first-party storage key", c.Severity)
	}

	if c.Detail == "" {
		t.Error("no detail recorded for the new storage key")
	}
}

func TestThirdPartyStorageInRejectModeIsCritical(t *testing.T) {
	t.Parallel()

	// A third party that keeps an identifier in localStorage after a
	// rejection has done what a third-party cookie does, and is ranked with
	// it rather than below it.
	before := withStorage(model.ConsentReject)
	after := withStorage(model.ConsentReject,
		storage(entry("https://tracker.test", "ph_distinct_id", model.StorageLocal, model.ThirdParty)))

	c := find(t, diff.Compare(before, after, diff.Options{}),
		diff.StorageAdded, "https://tracker.test|local:ph_distinct_id")

	if c.Severity != diff.SeverityCritical {
		t.Errorf("severity = %q, want critical: a third party stored an identifier despite a rejection", c.Severity)
	}
}

func TestStorageKeyDisappearingIsReportedQuietly(t *testing.T) {
	t.Parallel()

	before := withStorage(model.ConsentAccept,
		storage(entry("https://example.com", "cart", model.StorageSession, model.FirstParty)))
	after := withStorage(model.ConsentAccept)

	c := find(t, diff.Compare(before, after, diff.Options{}),
		diff.StorageRemoved, "https://example.com|session:cart")

	if c.Severity != diff.SeverityInfo {
		t.Errorf("severity = %q, want info for a key that is no longer set", c.Severity)
	}
}

func TestUnchangedStorageProducesNoChange(t *testing.T) {
	t.Parallel()

	// Determinism is a tested property (Tenet 6): the same storage twice is
	// not a finding.
	e := entry("https://example.com", "cookie-accepted", model.StorageLocal, model.FirstParty)

	rep := diff.Compare(
		withStorage(model.ConsentReject, storage(e)),
		withStorage(model.ConsentReject, storage(e)),
		diff.Options{})

	for _, c := range rep.Changes {
		if c.Type == diff.StorageAdded || c.Type == diff.StorageRemoved {
			t.Errorf("unchanged storage produced %+v", c)
		}
	}
}
