package model

import (
	"sort"
)

// HostSummary aggregates the requests of one registrable domain, which is the
// unit compliance review actually cares about.
type HostSummary struct {
	Domain string   `json:"domain"`
	Hosts  []string `json:"hosts"`
	Party  Party    `json:"party"`

	Requests int   `json:"requests"`
	Bytes    int64 `json:"bytes"`

	// PreConsent counts requests observed before the consent interaction.
	PreConsent int `json:"preConsent"`

	// ResourceTypes lists the distinct resource types seen, sorted.
	ResourceTypes []string `json:"resourceTypes"`

	Failed int `json:"failed"`
}

// HostSummaries aggregates requests by registrable domain, sorted third-party
// first, then by request count descending, then by domain for determinism.
func (r *Result) HostSummaries() []HostSummary {
	type acc struct {
		s     HostSummary
		hosts map[string]struct{}
		types map[string]struct{}
	}

	byDomain := make(map[string]*acc)

	for i := range r.Requests {
		req := &r.Requests[i]
		if req.NonNetwork {
			continue
		}

		key := req.Domain
		if key == "" {
			key = req.Host
		}

		a, ok := byDomain[key]
		if !ok {
			a = &acc{
				s:     HostSummary{Domain: key, Party: req.Party},
				hosts: make(map[string]struct{}),
				types: make(map[string]struct{}),
			}
			byDomain[key] = a
		}

		a.s.Requests++
		a.s.Bytes += req.TransferSize

		if req.Phase == PhasePre {
			a.s.PreConsent++
		}

		if req.Failed || req.Blocked {
			a.s.Failed++
		}

		if req.Host != "" {
			a.hosts[req.Host] = struct{}{}
		}

		if req.ResourceType != "" {
			a.types[req.ResourceType] = struct{}{}
		}
	}

	out := make([]HostSummary, 0, len(byDomain))

	for _, a := range byDomain {
		a.s.Hosts = sortedKeys(a.hosts)
		a.s.ResourceTypes = sortedKeys(a.types)
		out = append(out, a.s)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Party != out[j].Party {
			return out[i].Party == ThirdParty
		}

		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}

		return out[i].Domain < out[j].Domain
	})

	return out
}

// CountsByResourceType returns request counts per resource type.
func (r *Result) CountsByResourceType() map[string]int {
	out := make(map[string]int)

	for i := range r.Requests {
		t := r.Requests[i].ResourceType
		if t == "" {
			t = "other"
		}

		out[t]++
	}

	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
