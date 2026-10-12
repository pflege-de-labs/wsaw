package diff

import (
	"cmp"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/normalize"
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

	// DegradedFailureRatio is the share of a scan's network requests that may
	// fail for capture reasons before the scan is treated as degraded.
	//
	// Zero selects DefaultDegradedFailureRatio. A value above 1 can never be
	// reached and so disables the check, for an operator who would rather see
	// the raw comparison.
	DegradedFailureRatio float64

	// Normalizer, when set, re-derives every asset's comparison key from its
	// raw URL under the current rules instead of using the key stored at
	// capture (Tenet 4). A rule change then applies to history, and the
	// first scan after one is not a wall of assets removed and re-added under
	// new keys. Nil keeps the stored keys.
	Normalizer *normalize.Normalizer
}

// DefaultDegradedFailureRatio is the share of lost requests above which a
// scan's asset list is no longer trusted for removals.
//
// Five percent sits between the two behaviours observed in practice: a site
// whose homepage releases its images in one burst loses ten to thirteen
// percent of its requests inside a resource-constrained browser container,
// while sites that spread the same work lose well under one percent. The
// point is to separate a capture environment that is failing from one with
// the occasional unlucky request, not to pick a number that sounds strict.
const DefaultDegradedFailureRatio = 0.05

func (o Options) withDefaults() Options {
	out := o
	out.Severity = out.Severity.withDefaults()

	if out.DegradedFailureRatio <= 0 {
		out.DegradedFailureRatio = DefaultDegradedFailureRatio
	}

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

	ThirdPartyStorageRejectMode Severity `yaml:"thirdPartyStorageRejectMode,omitempty"`
	ThirdPartyStorageAdded      Severity `yaml:"thirdPartyStorageAdded,omitempty"`
	FirstPartyStorageAdded      Severity `yaml:"firstPartyStorageAdded,omitempty"`
	StorageRemoved              Severity `yaml:"storageRemoved,omitempty"`

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

		// Web Storage is ranked with cookies, not below them: a third party
		// that keeps an identifier in localStorage after a rejection has done
		// the same thing as one that sets a cookie, and the storage is the
		// half a cookie-only view misses (Story 2.9).
		ThirdPartyStorageRejectMode: SeverityCritical,
		ThirdPartyStorageAdded:      SeverityMedium,
		FirstPartyStorageAdded:      SeverityLow,
		StorageRemoved:              SeverityInfo,

		StatusBecameError: SeverityMedium,
		StatusChanged:     SeverityLow,

		ConsentChanged:  SeverityMedium,
		ConsentDegraded: SeverityHigh,

		DeniedHost:   SeverityCritical,
		ScanDegraded: SeverityMedium,
	}
}

func (s SeverityRules) withDefaults() SeverityRules {
	return s.Over(DefaultSeverityRules())
}

// Over returns s with every unset rule taken from base, so layers of
// configuration stack without restating which fields exist.
func (s SeverityRules) Over(base SeverityRules) SeverityRules {
	from := base.named()
	for i, r := range s.named() {
		*r.sev = cmp.Or(*r.sev, *from[i].sev)
	}

	return s
}

// Each calls fn with every rule and its YAML key, so validation can name the
// field an operator got wrong.
func (s SeverityRules) Each(fn func(key string, sev Severity)) {
	for _, r := range s.named() {
		fn(r.key, *r.sev)
	}
}

type namedSeverity struct {
	key string
	sev *Severity
}

// named is the one list of rules that Over and Each walk. Both have value
// receivers, so writing through these pointers never reaches a caller's value.
func (s *SeverityRules) named() []namedSeverity {
	return []namedSeverity{
		{"thirdPartyHostRejectMode", &s.ThirdPartyHostRejectMode},
		{"thirdPartyHostPreConsent", &s.ThirdPartyHostPreConsent},
		{"thirdPartyHostAdded", &s.ThirdPartyHostAdded},
		{"firstPartyHostAdded", &s.FirstPartyHostAdded},
		{"hostRemoved", &s.HostRemoved},
		{"thirdPartyScriptAdded", &s.ThirdPartyScriptAdded},
		{"thirdPartyAssetAdded", &s.ThirdPartyAssetAdded},
		{"firstPartyScriptAdded", &s.FirstPartyScriptAdded},
		{"firstPartyAssetAdded", &s.FirstPartyAssetAdded},
		{"assetRemoved", &s.AssetRemoved},
		{"thirdPartyScriptChanged", &s.ThirdPartyScriptChanged},
		{"firstPartyScriptChanged", &s.FirstPartyScriptChanged},
		{"thirdPartyCookieRejectMode", &s.ThirdPartyCookieRejectMode},
		{"thirdPartyCookieAdded", &s.ThirdPartyCookieAdded},
		{"firstPartyCookieAdded", &s.FirstPartyCookieAdded},
		{"cookieRemoved", &s.CookieRemoved},
		{"thirdPartyStorageRejectMode", &s.ThirdPartyStorageRejectMode},
		{"thirdPartyStorageAdded", &s.ThirdPartyStorageAdded},
		{"firstPartyStorageAdded", &s.FirstPartyStorageAdded},
		{"storageRemoved", &s.StorageRemoved},
		{"statusBecameError", &s.StatusBecameError},
		{"statusChanged", &s.StatusChanged},
		{"consentChanged", &s.ConsentChanged},
		{"consentDegraded", &s.ConsentDegraded},
		{"deniedHost", &s.DeniedHost},
		{"scanDegraded", &s.ScanDegraded},
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

func (s SeverityRules) forStorageAdded(mode model.ConsentMode, party model.Party) Severity {
	if party != model.ThirdParty {
		return s.FirstPartyStorageAdded
	}

	if mode == model.ConsentReject {
		return s.ThirdPartyStorageRejectMode
	}

	return s.ThirdPartyStorageAdded
}

func (s SeverityRules) forStatusChanged(before, after int) Severity {
	if after >= 400 && before < 400 {
		return s.StatusBecameError
	}

	return s.StatusChanged
}
