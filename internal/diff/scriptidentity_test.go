package diff_test

import (
	"strings"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/diff"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

func identity(label, value string) func(*model.Request) {
	return func(r *model.Request) {
		r.BodyIdentityLabel = label
		r.BodyIdentity = value
	}
}

const gtmURL = "https://www.googletagmanager.com/gtm.js?id=GTM-X"

// TestScriptIdentityBeatsTheDigest is the false positive this removes: the
// digest moves because the server folded a rollout flag into the response,
// while the container version — the thing an operator would act on — did not.
func TestScriptIdentityBeatsTheDigest(t *testing.T) {
	t.Parallel()

	before := result(model.ConsentNone,
		req(gtmURL, "googletagmanager.com", model.ThirdParty,
			digest("aaaa"), identity("GTM container version", "231")))

	after := result(model.ConsentNone,
		req(gtmURL, "googletagmanager.com", model.ThirdParty,
			digest("bbbb"), identity("GTM container version", "231")))

	rep := diff.Compare(before, after, diff.Options{})

	for _, c := range rep.Changes {
		if c.Type == diff.ScriptChanged {
			t.Errorf("a digest change was reported despite an unchanged identity: %+v", c)
		}
	}
}

// TestScriptIdentityChangeIsReportedWithItsLabel keeps the real publish, and
// says what moved rather than printing two hashes.
func TestScriptIdentityChangeIsReportedWithItsLabel(t *testing.T) {
	t.Parallel()

	before := result(model.ConsentNone,
		req(gtmURL, "googletagmanager.com", model.ThirdParty,
			digest("aaaa"), identity("GTM container version", "231")))

	after := result(model.ConsentNone,
		req(gtmURL, "googletagmanager.com", model.ThirdParty,
			digest("aaaa"), identity("GTM container version", "232")))

	rep := diff.Compare(before, after, diff.Options{})

	change := find(t, rep, diff.ScriptChanged, gtmURL)

	if change.Before != "231" || change.After != "232" {
		t.Errorf("reported %q → %q, want 231 → 232", change.Before, change.After)
	}

	if !strings.Contains(change.Detail, "GTM container version") {
		t.Errorf("detail does not name what changed: %q", change.Detail)
	}
}

// TestScriptIdentityOnOneSideOnlyIsNotAChange guards the fallback. If one
// scan could not read the identity, dropping back to the digest would
// reintroduce exactly the noise the rule exists to remove — and calling it
// "changed" would be inventing a supply-chain finding.
func TestScriptIdentityOnOneSideOnlyIsNotAChange(t *testing.T) {
	t.Parallel()

	before := result(model.ConsentNone,
		req(gtmURL, "googletagmanager.com", model.ThirdParty,
			digest("aaaa"), identity("GTM container version", "231")))

	after := result(model.ConsentNone,
		req(gtmURL, "googletagmanager.com", model.ThirdParty, digest("bbbb")))

	rep := diff.Compare(before, after, diff.Options{})

	for _, c := range rep.Changes {
		if c.Type == diff.ScriptChanged {
			t.Errorf("an unreadable identity was reported as a change: %+v", c)
		}
	}
}

// TestDigestStillGovernsScriptsWithoutAnIdentityRule keeps the supply-chain
// check intact for every script no rule applies to.
func TestDigestStillGovernsScriptsWithoutAnIdentityRule(t *testing.T) {
	t.Parallel()

	const url = "https://cdn.test/widget.js"

	before := result(model.ConsentNone,
		req(url, "cdn.test", model.ThirdParty, digest("aaaa")))

	after := result(model.ConsentNone,
		req(url, "cdn.test", model.ThirdParty, digest("bbbb")))

	change := find(t, diff.Compare(before, after, diff.Options{}), diff.ScriptChanged, url)

	if change.Before != "aaaa" || change.After != "bbbb" {
		t.Errorf("reported %q → %q, want the digests", change.Before, change.After)
	}
}
