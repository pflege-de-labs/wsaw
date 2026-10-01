package diff_test

import (
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
)

func beaconNormalizer(t *testing.T) *normalize.Normalizer {
	t.Helper()

	n, err := normalize.New(normalize.Rules{QueryRules: normalize.DefaultQueryRules})
	if err != nil {
		t.Fatal(err)
	}

	return n
}

// keyed returns a request whose stored key is its raw URL, the way a scan
// without query rules recorded it.
func keyed(url string) model.Request {
	return req(url, "googletagmanager.com", model.ThirdParty)
}

func assetChanges(rep *diff.Report) []diff.Change {
	var out []diff.Change

	for _, c := range rep.Changes {
		switch c.Type {
		case diff.AssetAdded, diff.AssetRemoved:
			out = append(out, c)
		}
	}

	return out
}

// Two visits to an unchanged page differ only in a beacon's per-visit
// parameters. Under the rules they must compare as unchanged (Tenet 6).
func TestBeaconParametersAloneDoNotChangeTheDiff(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, keyed("https://www.googletagmanager.com/gtag/js?id=G-1&cx=c&gtm=4e69t1"))
	b := result(model.ConsentAccept, keyed("https://www.googletagmanager.com/gtag/js?id=G-1&cx=c&gtm=4e6a01"))

	if got := assetChanges(diff.Compare(a, b, diff.Options{})); len(got) != 2 {
		t.Fatalf("without a normalizer: %d asset changes, want the stored keys' 2 (precondition)", len(got))
	}

	if got := assetChanges(diff.Compare(a, b, diff.Options{Normalizer: beaconNormalizer(t)})); len(got) != 0 {
		t.Errorf("with the rules: %d asset changes, want none: %+v", len(got), got)
	}
}

// A rule change must not make the first scan after it report every
// re-keyed asset as removed and added again: the baseline was keyed under
// the old rules, and both sides are keyed again under the new ones.
func TestRuleChangeDoesNotBurstAgainstAnOlderBaseline(t *testing.T) {
	t.Parallel()

	n := beaconNormalizer(t)

	old := keyed("https://www.googletagmanager.com/gtag/js?id=G-1&cx=c&gtm=4e69t1")

	cur := keyed("https://www.googletagmanager.com/gtag/js?id=G-1&cx=c&gtm=4e69t1")
	cur.NormalizedURL = n.Key(cur.URL, cur.Party)

	rep := diff.Compare(result(model.ConsentAccept, old), result(model.ConsentAccept, cur), diff.Options{Normalizer: n})

	if got := assetChanges(rep); len(got) != 0 {
		t.Errorf("%d asset changes after a rule change, want none: %+v", len(got), got)
	}
}

// What the rules keep is still compared: a new measurement ID is a new
// asset, and is reported.
func TestBeaconIdentityChangeIsStillReported(t *testing.T) {
	t.Parallel()

	a := result(model.ConsentAccept, keyed("https://www.googletagmanager.com/gtag/js?id=G-1&gtm=a"))
	b := result(model.ConsentAccept, keyed("https://www.googletagmanager.com/gtag/js?id=G-2&gtm=b"))

	rep := diff.Compare(a, b, diff.Options{Normalizer: beaconNormalizer(t)})

	find(t, rep, diff.AssetAdded, "https://www.googletagmanager.com/gtag/js?id=G-2")
	find(t, rep, diff.AssetRemoved, "https://www.googletagmanager.com/gtag/js?id=G-1")
}

// A data: request is stored with its URL truncated, so it cannot be keyed
// again from that URL; its stored key is used on both sides.
func TestRekeyLeavesDataURLsOnTheirStoredKey(t *testing.T) {
	t.Parallel()

	img := func(url string) model.Request {
		r := req(url, "", model.FirstParty, typ("image"))
		r.NormalizedURL = "data:image/png;base64,…"
		r.NonNetwork = true

		return r
	}

	a := result(model.ConsentAccept, img("data:image/png;base64,AAAA…"))
	b := result(model.ConsentAccept, img("data:image/png;base64,BBBB…"))

	if got := assetChanges(diff.Compare(a, b, diff.Options{Normalizer: beaconNormalizer(t)})); len(got) != 0 {
		t.Errorf("%d asset changes for one data: image, want none: %+v", len(got), got)
	}
}
