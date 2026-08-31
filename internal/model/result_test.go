package model_test

import (
	"testing"

	"github.com/martint17r/wsaw/internal/model"
)

func TestConsentModeValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode model.ConsentMode
		want bool
	}{
		{model.ConsentNone, true},
		{model.ConsentReject, true},
		{model.ConsentAccept, true},
		{model.ConsentMode("accept-all"), false},
		{model.ConsentMode(""), false},
	}

	for _, tc := range tests {
		if got := tc.mode.Valid(); got != tc.want {
			t.Errorf("ConsentMode(%q).Valid() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestResultOK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		res  model.Result
		want bool
	}{
		{
			name: "idle scan is ok",
			res:  model.Result{Termination: model.TermIdle},
			want: true,
		},
		{
			name: "timeout is still a usable result",
			res:  model.Result{Termination: model.TermTimeout},
			want: true,
		},
		{
			name: "error is not ok",
			res:  model.Result{Termination: model.TermError, Error: "navigate: boom"},
			want: false,
		},
		{
			name: "skipped is not ok",
			res:  model.Result{Termination: model.TermSkipped},
			want: false,
		},
		{
			name: "error string alone disqualifies",
			res:  model.Result{Termination: model.TermIdle, Error: "partial capture"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.res.OK(); got != tc.want {
				t.Errorf("OK() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResultTruncated(t *testing.T) {
	t.Parallel()

	truncating := []model.TerminationReason{
		model.TermTimeout,
		model.TermRequestCap,
		model.TermByteCap,
	}

	for _, term := range truncating {
		res := model.Result{Termination: term}
		if !res.Truncated() {
			t.Errorf("Truncated() = false for %q, want true", term)
		}
	}

	if (&model.Result{Termination: model.TermIdle}).Truncated() {
		t.Error("Truncated() = true for idle, want false")
	}
}

func fixture() *model.Result {
	return &model.Result{
		Requests: []model.Request{
			{URL: "https://example.com/", Host: "example.com", Domain: "example.com", Party: model.FirstParty, ResourceType: "document", Phase: model.PhasePre, TransferSize: 100},
			{URL: "https://cdn.example.com/a.js", Host: "cdn.example.com", Domain: "example.com", Party: model.FirstParty, ResourceType: "script", Phase: model.PhasePre, TransferSize: 200},
			{URL: "https://tracker.test/px.gif", Host: "tracker.test", Domain: "tracker.test", Party: model.ThirdParty, ResourceType: "image", Phase: model.PhasePre, TransferSize: 43},
			{URL: "https://ads.test/t.js", Host: "ads.test", Domain: "ads.test", Party: model.ThirdParty, ResourceType: "script", Phase: model.PhasePost, TransferSize: 900},
			{URL: "https://ads.test/beacon", Host: "ads.test", Domain: "ads.test", Party: model.ThirdParty, ResourceType: "ping", Phase: model.PhasePost, TransferSize: 10, Failed: true},
			{URL: "data:image/gif;base64,AA", NonNetwork: true, ResourceType: "image", Party: model.FirstParty},
		},
	}
}

func TestThirdPartyDomains(t *testing.T) {
	t.Parallel()

	res := fixture()

	all := res.ThirdPartyDomains("")
	if want := []string{"ads.test", "tracker.test"}; !equal(all, want) {
		t.Errorf("all phases = %v, want %v", all, want)
	}

	pre := res.ThirdPartyDomains(model.PhasePre)
	if want := []string{"tracker.test"}; !equal(pre, want) {
		t.Errorf("pre-consent = %v, want %v", pre, want)
	}
}

func TestHostSummariesOrderingAndCounts(t *testing.T) {
	t.Parallel()

	got := fixture().HostSummaries()

	// Third parties sort first, then by request count descending.
	want := []string{"ads.test", "tracker.test", "example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %d summaries, want %d: %+v", len(got), len(want), got)
	}

	for i := range want {
		if got[i].Domain != want[i] {
			t.Errorf("summary[%d].Domain = %q, want %q", i, got[i].Domain, want[i])
		}
	}

	ads := got[0]
	if ads.Requests != 2 {
		t.Errorf("ads.test requests = %d, want 2", ads.Requests)
	}

	if ads.Failed != 1 {
		t.Errorf("ads.test failed = %d, want 1", ads.Failed)
	}

	if ads.Bytes != 910 {
		t.Errorf("ads.test bytes = %d, want 910", ads.Bytes)
	}

	if !equal(ads.ResourceTypes, []string{"ping", "script"}) {
		t.Errorf("ads.test resource types = %v", ads.ResourceTypes)
	}

	first := got[2]
	if !equal(first.Hosts, []string{"cdn.example.com", "example.com"}) {
		t.Errorf("example.com hosts = %v", first.Hosts)
	}

	if first.Party != model.FirstParty {
		t.Errorf("example.com party = %q, want first", first.Party)
	}
}

func TestHostSummariesExcludesNonNetwork(t *testing.T) {
	t.Parallel()

	for _, s := range fixture().HostSummaries() {
		if s.Domain == "" {
			t.Error("non-network request leaked into host summaries")
		}
	}
}

func TestCountsByResourceType(t *testing.T) {
	t.Parallel()

	got := fixture().CountsByResourceType()

	want := map[string]int{"document": 1, "script": 2, "image": 2, "ping": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("count[%q] = %d, want %d", k, got[k], v)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
