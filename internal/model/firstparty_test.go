package model_test

// Story 2.9, AC7: "no third parties before consent" is a narrower claim than
// it looks. A collector reverse-proxied onto the site's own domain is
// first-party by every rule wsaw applies, so the first-party hosts of a phase
// have to be answerable too.

import (
	"slices"
	"testing"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

func phaseResult() *model.Result {
	return &model.Result{
		SchemaVersion: model.SchemaVersion,
		Requests: []model.Request{
			{Host: "example.com", Domain: "example.com", Party: model.FirstParty, Phase: model.PhasePre},
			{Host: "hog.example.com", Domain: "example.com", Party: model.FirstParty, Phase: model.PhasePre},
			{Host: "example.com", Domain: "example.com", Party: model.FirstParty, Phase: model.PhasePost},
			{Host: "cdn.example.com", Domain: "example.com", Party: model.FirstParty, Phase: model.PhasePre,
				NonNetwork: true},
			{Host: "tracker.test", Domain: "tracker.test", Party: model.ThirdParty, Phase: model.PhasePre},
		},
	}
}

func TestFirstPartyHostsNamesTheSitesOwnHostsInAPhase(t *testing.T) {
	t.Parallel()

	got := phaseResult().FirstPartyHosts(model.PhasePre)

	want := []string{"example.com", "hog.example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("pre-consent first-party hosts = %v, want %v", got, want)
	}
}

func TestFirstPartyHostsSkipsNonNetworkAndThirdParties(t *testing.T) {
	t.Parallel()

	got := phaseResult().FirstPartyHosts("")

	if slices.Contains(got, "tracker.test") {
		t.Errorf("hosts = %v, want no third party in a first-party list", got)
	}

	if slices.Contains(got, "cdn.example.com") {
		t.Errorf("hosts = %v, want no data:/blob: request, which never left the browser", got)
	}
}

func TestFirstPartyHostsOfAPhaseWithNoneIsEmpty(t *testing.T) {
	t.Parallel()

	empty := &model.Result{SchemaVersion: model.SchemaVersion}

	if got := empty.FirstPartyHosts(model.PhasePre); len(got) != 0 {
		t.Errorf("hosts = %v, want none", got)
	}
}
