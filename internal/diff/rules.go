package diff

import (
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Options configures a comparison.
type Options struct {
	// Allow suppresses changes for expected parties, so review effort goes to
	// the unexpected.
	Allow HostList
	// Deny raises a finding whenever a matching host appears at all.
	Deny HostList
	// Severity assigns a rank to each kind of change.
	Severity SeverityRules
}

func (o Options) withDefaults() Options {
	out := o
	out.Severity = out.Severity.withDefaults()

	return out
}

// HostList matches registrable domains and hosts, with "*." wildcards.
type HostList struct {
	exact    map[string]struct{}
	suffixes []string
}

// NewHostList compiles patterns. A bare domain matches that domain and its
// subdomains, which is what operators mean when they write "example.com".
func NewHostList(patterns []string) HostList {
	l := HostList{exact: make(map[string]struct{}, len(patterns))}

	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}

		if strings.HasPrefix(p, "*.") {
			l.suffixes = append(l.suffixes, strings.TrimPrefix(p, "*."))

			continue
		}

		l.exact[p] = struct{}{}
		// A bare domain also covers its subdomains.
		l.suffixes = append(l.suffixes, p)
	}

	return l
}

// Empty reports whether the list matches nothing.
func (l HostList) Empty() bool { return len(l.exact) == 0 && len(l.suffixes) == 0 }

// Matches reports whether a host or domain is on the list.
func (l HostList) Matches(host string) bool {
	if host == "" {
		return false
	}

	host = strings.ToLower(host)

	if _, ok := l.exact[host]; ok {
		return true
	}

	// The registrable domain is checked too, so "tracker.test" covers
	// "pixel.tracker.test" even when only the full host was observed.
	if domain := classify.RegistrableDomain(host); domain != host {
		if _, ok := l.exact[domain]; ok {
			return true
		}
	}

	for _, suffix := range l.suffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}

	return false
}

// Suppresses reports whether an allow list hides a change. Only changes tied
// to a host are suppressible: a consent regression or a degraded scan is
// never hidden by an allow list, because those are about wsaw's own ability
// to observe rather than about an expected third party.
func (l HostList) Suppresses(c Change) bool {
	if l.Empty() || c.Domain == "" {
		return false
	}

	switch c.Type {
	case ConsentChanged, ScanDegraded, DeniedHost:
		return false
	default:
		return l.Matches(c.Domain)
	}
}

// SeverityRules assigns severities. The zero value is completed by
// withDefaults, so a partially configured rule set still behaves sensibly.
type SeverityRules struct {
	// Third-party host appearing, by consent mode and phase. These are the
	// findings the product exists to surface, so they default high.
	ThirdPartyHostRejectMode Severity `yaml:"thirdPartyHostRejectMode,omitempty"`
	ThirdPartyHostPreConsent Severity `yaml:"thirdPartyHostPreConsent,omitempty"`
	ThirdPartyHostAdded      Severity `yaml:"thirdPartyHostAdded,omitempty"`
	FirstPartyHostAdded      Severity `yaml:"firstPartyHostAdded,omitempty"`
	HostRemoved              Severity `yaml:"hostRemoved,omitempty"`

	ThirdPartyScriptAdded Severity `yaml:"thirdPartyScriptAdded,omitempty"`
	ThirdPartyAssetAdded  Severity `yaml:"thirdPartyAssetAdded,omitempty"`
	FirstPartyScriptAdded Severity `yaml:"firstPartyScriptAdded,omitempty"`
	FirstPartyAssetAdded  Severity `yaml:"firstPartyAssetAdded,omitempty"`
	AssetRemoved          Severity `yaml:"assetRemoved,omitempty"`

	ThirdPartyScriptChanged Severity `yaml:"thirdPartyScriptChanged,omitempty"`
	FirstPartyScriptChanged Severity `yaml:"firstPartyScriptChanged,omitempty"`

	ThirdPartyCookieRejectMode Severity `yaml:"thirdPartyCookieRejectMode,omitempty"`
	ThirdPartyCookieAdded      Severity `yaml:"thirdPartyCookieAdded,omitempty"`
	FirstPartyCookieAdded      Severity `yaml:"firstPartyCookieAdded,omitempty"`
	CookieRemoved              Severity `yaml:"cookieRemoved,omitempty"`

	StatusBecameError Severity `yaml:"statusBecameError,omitempty"`
	StatusChanged     Severity `yaml:"statusChanged,omitempty"`

	ConsentChanged  Severity `yaml:"consentChanged,omitempty"`
	ConsentDegraded Severity `yaml:"consentDegraded,omitempty"`

	DeniedHost   Severity `yaml:"deniedHost,omitempty"`
	ScanDegraded Severity `yaml:"scanDegraded,omitempty"`
}

// DefaultSeverityRules encodes the product's judgement about what matters.
// The reasoning: anything that indicates tracking despite a rejection, or a
// third-party script changing under a stable URL, is a compliance or
// supply-chain finding. Cosmetic drift is not.
func DefaultSeverityRules() SeverityRules {
	return SeverityRules{
		ThirdPartyHostRejectMode: SeverityCritical,
		ThirdPartyHostPreConsent: SeverityHigh,
		ThirdPartyHostAdded:      SeverityHigh,
		FirstPartyHostAdded:      SeverityLow,
		HostRemoved:              SeverityInfo,

		ThirdPartyScriptAdded: SeverityHigh,
		ThirdPartyAssetAdded:  SeverityMedium,
		FirstPartyScriptAdded: SeverityLow,
		FirstPartyAssetAdded:  SeverityInfo,
		AssetRemoved:          SeverityInfo,

		ThirdPartyScriptChanged: SeverityHigh,
		FirstPartyScriptChanged: SeverityLow,

		ThirdPartyCookieRejectMode: SeverityCritical,
		ThirdPartyCookieAdded:      SeverityMedium,
		FirstPartyCookieAdded:      SeverityLow,
		CookieRemoved:              SeverityInfo,

		StatusBecameError: SeverityMedium,
		StatusChanged:     SeverityLow,

		ConsentChanged:  SeverityMedium,
		ConsentDegraded: SeverityHigh,

		DeniedHost:   SeverityCritical,
		ScanDegraded: SeverityMedium,
	}
}

func (s SeverityRules) withDefaults() SeverityRules {
	defaults := DefaultSeverityRules()

	fill := func(configured, fallback Severity) Severity {
		if configured == "" {
			return fallback
		}

		return configured
	}

	return SeverityRules{
		ThirdPartyHostRejectMode: fill(s.ThirdPartyHostRejectMode, defaults.ThirdPartyHostRejectMode),
		ThirdPartyHostPreConsent: fill(s.ThirdPartyHostPreConsent, defaults.ThirdPartyHostPreConsent),
		ThirdPartyHostAdded:      fill(s.ThirdPartyHostAdded, defaults.ThirdPartyHostAdded),
		FirstPartyHostAdded:      fill(s.FirstPartyHostAdded, defaults.FirstPartyHostAdded),
		HostRemoved:              fill(s.HostRemoved, defaults.HostRemoved),

		ThirdPartyScriptAdded: fill(s.ThirdPartyScriptAdded, defaults.ThirdPartyScriptAdded),
		ThirdPartyAssetAdded:  fill(s.ThirdPartyAssetAdded, defaults.ThirdPartyAssetAdded),
		FirstPartyScriptAdded: fill(s.FirstPartyScriptAdded, defaults.FirstPartyScriptAdded),
		FirstPartyAssetAdded:  fill(s.FirstPartyAssetAdded, defaults.FirstPartyAssetAdded),
		AssetRemoved:          fill(s.AssetRemoved, defaults.AssetRemoved),

		ThirdPartyScriptChanged: fill(s.ThirdPartyScriptChanged, defaults.ThirdPartyScriptChanged),
		FirstPartyScriptChanged: fill(s.FirstPartyScriptChanged, defaults.FirstPartyScriptChanged),

		ThirdPartyCookieRejectMode: fill(s.ThirdPartyCookieRejectMode, defaults.ThirdPartyCookieRejectMode),
		ThirdPartyCookieAdded:      fill(s.ThirdPartyCookieAdded, defaults.ThirdPartyCookieAdded),
		FirstPartyCookieAdded:      fill(s.FirstPartyCookieAdded, defaults.FirstPartyCookieAdded),
		CookieRemoved:              fill(s.CookieRemoved, defaults.CookieRemoved),

		StatusBecameError: fill(s.StatusBecameError, defaults.StatusBecameError),
		StatusChanged:     fill(s.StatusChanged, defaults.StatusChanged),

		ConsentChanged:  fill(s.ConsentChanged, defaults.ConsentChanged),
		ConsentDegraded: fill(s.ConsentDegraded, defaults.ConsentDegraded),

		DeniedHost:   fill(s.DeniedHost, defaults.DeniedHost),
		ScanDegraded: fill(s.ScanDegraded, defaults.ScanDegraded),
	}
}

func (s SeverityRules) forHostAdded(mode model.ConsentMode, party model.Party, phase model.ConsentPhase) Severity {
	if party != model.ThirdParty {
		return s.FirstPartyHostAdded
	}

	// Tracking that survives an explicit rejection is the strongest signal
	// wsaw can produce, so it outranks everything else.
	if mode == model.ConsentReject {
		return s.ThirdPartyHostRejectMode
	}

	if phase == model.PhasePre {
		return s.ThirdPartyHostPreConsent
	}

	return s.ThirdPartyHostAdded
}

func (s SeverityRules) forAssetAdded(party model.Party, resourceType string) Severity {
	isScript := resourceType == "script"

	switch {
	case party == model.ThirdParty && isScript:
		return s.ThirdPartyScriptAdded
	case party == model.ThirdParty:
		return s.ThirdPartyAssetAdded
	case isScript:
		return s.FirstPartyScriptAdded
	default:
		return s.FirstPartyAssetAdded
	}
}

func (s SeverityRules) forScriptChanged(party model.Party) Severity {
	if party == model.ThirdParty {
		return s.ThirdPartyScriptChanged
	}

	return s.FirstPartyScriptChanged
}

func (s SeverityRules) forCookieAdded(mode model.ConsentMode, party model.Party) Severity {
	if party != model.ThirdParty {
		return s.FirstPartyCookieAdded
	}

	if mode == model.ConsentReject {
		return s.ThirdPartyCookieRejectMode
	}

	return s.ThirdPartyCookieAdded
}

func (s SeverityRules) forStatusChanged(before, after int) Severity {
	if after >= 400 && before < 400 {
		return s.StatusBecameError
	}

	return s.StatusChanged
}
