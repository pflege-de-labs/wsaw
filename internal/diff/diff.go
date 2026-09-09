// Package diff turns two scan results into change events.
//
// Diffing is pure: it operates on stored results and never touches a browser
// (Tenet 3), so severity rules and allow lists can be re-applied to history
// without re-scanning.
package diff

import (
	"fmt"
	"net/url"
	"slices"
	"sort"

	"github.com/pflege-de-labs/wsaw/internal/classify"
	"github.com/pflege-de-labs/wsaw/internal/model"
)

// ChangeType names what changed.
type ChangeType string

// Change types.
const (
	HostAdded      ChangeType = "host-added"
	HostRemoved    ChangeType = "host-removed"
	AssetAdded     ChangeType = "asset-added"
	AssetRemoved   ChangeType = "asset-removed"
	ScriptChanged  ChangeType = "script-changed"
	CookieAdded    ChangeType = "cookie-added"
	CookieRemoved  ChangeType = "cookie-removed"
	StatusChanged  ChangeType = "status-changed"
	ConsentChanged ChangeType = "consent-changed"
	DeniedHost     ChangeType = "denied-host"
	ScanDegraded   ChangeType = "scan-degraded"
)

// Severity ranks a change for routing and for the CI exit-code contract.
type Severity string

// Severities, ordered.
const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var severityRank = map[Severity]int{
	SeverityInfo:     0,
	SeverityLow:      1,
	SeverityMedium:   2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// Rank returns the ordinal of a severity, for threshold comparisons.
func (s Severity) Rank() int { return severityRank[s] }

// AtLeast reports whether s is at least as severe as other.
func (s Severity) AtLeast(other Severity) bool { return s.Rank() >= other.Rank() }

// ParseSeverity converts a configured string into a Severity.
func ParseSeverity(s string) (Severity, error) {
	sev := Severity(s)
	if _, ok := severityRank[sev]; !ok {
		return "", fmt.Errorf("unknown severity %q, expected one of info, low, medium, high, critical", s)
	}

	return sev, nil
}

// Change is one difference between a baseline and a current result.
type Change struct {
	Type     ChangeType `json:"type"`
	Severity Severity   `json:"severity"`

	Target      string            `json:"target"`
	ConsentMode model.ConsentMode `json:"consentMode"`

	// Subject is what changed: a domain, a normalized asset URL, or a cookie
	// name scoped to its domain.
	Subject string `json:"subject"`
	// Domain is the registrable domain the change belongs to, for filtering
	// and allow-list matching.
	Domain string `json:"domain,omitempty"`
	// Party records whether the subject is first- or third-party, which is
	// the main driver of severity.
	Party model.Party `json:"party,omitempty"`

	// Phase records the consent phase for changes that have one. A new host
	// in the pre-interaction phase is a much stronger finding.
	Phase model.ConsentPhase `json:"phase,omitempty"`

	// Before and After carry the changed values, empty where not applicable.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`

	// Detail is a human-readable explanation.
	Detail string `json:"detail"`
}

// Report is the complete comparison of two results.
type Report struct {
	Target      string            `json:"target"`
	ConsentMode model.ConsentMode `json:"consentMode"`

	// BaselineScanID and CurrentScanID identify what was compared, so a
	// report is reproducible.
	BaselineScanID string `json:"baselineScanId"`
	CurrentScanID  string `json:"currentScanId"`

	Changes []Change `json:"changes"`

	// Suppressed counts changes hidden by allow lists, so a quiet report is
	// distinguishable from an over-configured one.
	Suppressed int `json:"suppressed"`

	// Comparable is false when the two results cannot be meaningfully
	// compared, in which case Changes holds only the reason.
	Comparable bool `json:"comparable"`
	// Reason explains an incomparable pair.
	Reason string `json:"reason,omitempty"`
}

// MaxSeverity returns the highest severity present, or info when there are no
// changes.
func (r *Report) MaxSeverity() Severity {
	highest := SeverityInfo

	for _, c := range r.Changes {
		if c.Severity.Rank() > highest.Rank() {
			highest = c.Severity
		}
	}

	return highest
}

// HasFindingsAtLeast reports whether any change meets a threshold, which is
// what the CI exit-code contract keys on.
func (r *Report) HasFindingsAtLeast(threshold Severity) bool {
	for _, c := range r.Changes {
		if c.Severity.AtLeast(threshold) {
			return true
		}
	}

	return false
}

// Compare produces a report. Results from different consent modes are never
// compared: the difference between modes is a finding in its own right, not a
// drift signal (Story 2.1).
func Compare(baseline, current *model.Result, opts Options) *Report {
	rep := &Report{
		Target:        current.Target,
		ConsentMode:   current.ConsentMode,
		CurrentScanID: current.ScanID,
	}

	if baseline != nil {
		rep.BaselineScanID = baseline.ScanID
	}

	rules := opts.withDefaults()

	if reason, ok := incomparable(baseline, current); !ok {
		rep.Reason = reason

		// A degraded scan is itself worth reporting: silence here would let a
		// broken watcher look like a clean site.
		if !current.OK() {
			rep.Changes = append(rep.Changes, Change{
				Type:        ScanDegraded,
				Severity:    rules.Severity.ScanDegraded,
				Target:      current.Target,
				ConsentMode: current.ConsentMode,
				Subject:     current.URL,
				Detail:      reason,
			})
		}

		return rep
	}

	rep.Comparable = true

	d := &differ{
		baseline: baseline,
		current:  current,
		rules:    rules,
		report:   rep,

		currentDegraded:  rules.degraded(current),
		baselineDegraded: rules.degraded(baseline),
		unobserved:       unobservedDomains(current),
	}

	d.compareHosts()
	d.compareAssets()
	d.compareScripts()
	d.compareCookies()
	d.compareStatuses()
	d.compareConsent()
	d.flagDeniedHosts()
	d.flagDegradation()

	sortChanges(rep.Changes)

	return rep
}

// ReasonEvidenceGone is the Reason a caller sets when the result this scan
// should have been compared against is still in the index and its document is
// no longer in the artifact bucket.
//
// It lives here, beside the reasons Compare produces itself, because a reader
// has to be able to tell it from "no baseline to compare against" — the two
// produce an identical report otherwise, and one of them means a change may
// have gone unreported. Compare cannot produce it: only whoever fetched the
// baseline knows why it is missing (Story 8.2, AC5; Tenet 5).
const ReasonEvidenceGone = "the result this scan should be compared against is recorded, " +
	"but its stored document is no longer in the artifact bucket"

// incomparable reports why two results must not be diffed.
func incomparable(baseline, current *model.Result) (string, bool) {
	switch {
	case current == nil:
		return "no current result", false

	case !current.OK():
		reason := current.Error
		if reason == "" {
			reason = string(current.Termination)
		}

		return "current scan did not produce a trustworthy asset list: " + reason, false

	case baseline == nil:
		return "no baseline to compare against", false

	case !baseline.OK():
		return "baseline scan did not produce a trustworthy asset list", false

	case baseline.ConsentMode != current.ConsentMode:
		return fmt.Sprintf("consent modes differ (%s vs %s); results are only comparable within one mode",
			baseline.ConsentMode, current.ConsentMode), false

	default:
		return "", true
	}
}

type differ struct {
	baseline *model.Result
	current  *model.Result
	rules    Options
	report   *Report

	// currentDegraded and baselineDegraded record whether each side lost
	// enough requests to capture failure that its asset list is incomplete.
	// They are computed once because every comparison consults them.
	currentDegraded  bool
	baselineDegraded bool

	// unobserved are the registrable domains the current scan did not fully
	// observe. It is consulted per change, not per scan, so a removal can be
	// withheld for a precise reason even when the scan as a whole is healthy.
	unobserved map[string]struct{}
}

func (d *differ) add(c Change) {
	c.Target = d.current.Target
	c.ConsentMode = d.current.ConsentMode

	if d.rules.Allow.Suppresses(c) {
		d.report.Suppressed++

		return
	}

	d.report.Changes = append(d.report.Changes, c)
}

type hostInfo struct {
	party model.Party
	phase model.ConsentPhase
	count int
}

// hosts indexes a result by registrable domain, remembering the strongest
// phase: if any request fired pre-consent, the host counts as pre-consent.
func hosts(r *model.Result) map[string]hostInfo {
	out := make(map[string]hostInfo)

	for i := range r.Requests {
		req := &r.Requests[i]
		if req.NonNetwork || req.Domain == "" {
			continue
		}

		info := out[req.Domain]
		info.party = req.Party
		info.count++

		if info.phase == "" || req.Phase == model.PhasePre {
			info.phase = req.Phase
		}

		out[req.Domain] = info
	}

	return out
}

func (d *differ) compareHosts() {
	before, after := hosts(d.baseline), hosts(d.current)

	for domain, info := range after {
		if _, existed := before[domain]; existed {
			continue
		}

		d.add(Change{
			Type:     HostAdded,
			Severity: d.rules.Severity.forHostAdded(d.current.ConsentMode, info.party, info.phase),
			Subject:  domain,
			Domain:   domain,
			Party:    info.party,
			Phase:    info.phase,
			After:    fmt.Sprintf("%d requests", info.count),
			Detail:   hostAddedDetail(domain, info, d.current.ConsentMode),
		})
	}

	for domain, info := range before {
		if _, still := after[domain]; still {
			continue
		}

		if d.removalUnprovable(domain, nil) {
			continue
		}

		d.add(Change{
			Type:     HostRemoved,
			Severity: d.rules.Severity.HostRemoved,
			Subject:  domain,
			Domain:   domain,
			Party:    info.party,
			Before:   fmt.Sprintf("%d requests", info.count),
			Detail:   fmt.Sprintf("%s is no longer contacted", domain),
		})
	}
}

func hostAddedDetail(domain string, info hostInfo, mode model.ConsentMode) string {
	switch {
	case info.party == model.ThirdParty && mode == model.ConsentReject && info.phase == model.PhasePre:
		return fmt.Sprintf("new third-party host %s is contacted before any consent interaction", domain)
	case info.party == model.ThirdParty && mode == model.ConsentReject:
		return fmt.Sprintf("new third-party host %s is contacted despite consent being rejected", domain)
	case info.party == model.ThirdParty && info.phase == model.PhasePre:
		return fmt.Sprintf("new third-party host %s is contacted before any consent interaction", domain)
	case info.party == model.ThirdParty:
		return fmt.Sprintf("new third-party host %s is contacted", domain)
	default:
		return fmt.Sprintf("new first-party host %s is contacted", domain)
	}
}

type assetInfo struct {
	url    string
	party  model.Party
	domain string
	phase  model.ConsentPhase
	typ    string
	status int
	digest string

	// identity and identityLabel carry a version the script publishes about
	// itself, when capture was configured to extract one. It is preferred
	// over the digest, because some scripts rewrite their bytes far more
	// often than their content changes.
	identity      string
	identityLabel string

	// initiators are the registrable domains of whatever caused this request.
	// They are what makes a missing asset attributable: if the script that
	// asked for it could not be fetched this time, its absence says nothing
	// about the site.
	initiators []string
}

func assets(r *model.Result) map[string]assetInfo {
	out := make(map[string]assetInfo)

	for i := range r.Requests {
		req := &r.Requests[i]
		if req.NormalizedURL == "" {
			continue
		}

		// The first observation wins, so a repeated asset does not flip
		// between phases between scans.
		if _, seen := out[req.NormalizedURL]; seen {
			continue
		}

		out[req.NormalizedURL] = assetInfo{
			url:    req.URL,
			party:  req.Party,
			domain: req.Domain,
			phase:  req.Phase,
			typ:    req.ResourceType,
			status: req.Status,
			digest: req.BodySHA256,

			identity:      req.BodyIdentity,
			identityLabel: req.BodyIdentityLabel,

			initiators: initiatorDomains(req),
		}
	}

	return out
}

func (d *differ) compareAssets() {
	before, after := assets(d.baseline), assets(d.current)

	for key, info := range after {
		if _, existed := before[key]; existed {
			continue
		}

		d.add(Change{
			Type:     AssetAdded,
			Severity: d.rules.Severity.forAssetAdded(info.party, info.typ),
			Subject:  key,
			Domain:   info.domain,
			Party:    info.party,
			Phase:    info.phase,
			After:    info.typ,
			Detail:   fmt.Sprintf("new %s asset loaded from %s", info.typ, info.domain),
		})
	}

	for key, info := range before {
		if _, still := after[key]; still {
			continue
		}

		if d.removalUnprovable(info.domain, info.initiators) {
			continue
		}

		d.add(Change{
			Type:     AssetRemoved,
			Severity: d.rules.Severity.AssetRemoved,
			Subject:  key,
			Domain:   info.domain,
			Party:    info.party,
			Before:   info.typ,
			Detail:   fmt.Sprintf("%s asset no longer loaded", info.typ),
		})
	}
}

// compareScripts is the supply-chain check: a third-party script whose
// content changed while its URL stayed the same.
func (d *differ) compareScripts() {
	before, after := assets(d.baseline), assets(d.current)

	for key, now := range after {
		was, existed := before[key]
		if !existed {
			continue
		}

		before, after, label, ok := scriptIdentity(was, now)
		if !ok || before == after {
			continue
		}

		detail := fmt.Sprintf("content of %s script %s changed while its URL stayed the same", now.party, key)
		if label != "" {
			detail = fmt.Sprintf("%s of %s script %s changed from %s to %s",
				label, now.party, key, before, after)
		}

		d.add(Change{
			Type:     ScriptChanged,
			Severity: d.rules.Severity.forScriptChanged(now.party),
			Subject:  key,
			Domain:   now.domain,
			Party:    now.party,
			Phase:    now.phase,
			Before:   before,
			After:    after,
			Detail:   detail,
		})
	}
}

// scriptIdentity decides what two observations of the same script URL should
// be compared on, and whether they can be compared at all.
//
// An extracted identity wins over the digest: it is the version the script
// declares, and it does not move when the server folds a rollout flag into
// the response. But an identity present on only one side is not a change —
// it means one scan could not read it — and comparing the digests instead
// would reintroduce exactly the noise the identity rule exists to remove. So
// once a rule is in play, both sides must carry a value, on the same footing
// as a missing digest: not comparable, and therefore not reported. Reporting
// "unchanged" would be the worse error, since this is the supply-chain check.
func scriptIdentity(was, now assetInfo) (before, after, label string, ok bool) {
	if was.identity != "" || now.identity != "" {
		if was.identity == "" || now.identity == "" {
			return "", "", "", false
		}

		label = now.identityLabel
		if label == "" {
			label = was.identityLabel
		}

		return was.identity, now.identity, label, true
	}

	if was.digest == "" || now.digest == "" {
		return "", "", "", false
	}

	return was.digest, now.digest, "", true
}

func cookieKey(c model.Cookie) string {
	return c.Domain + c.Path + "|" + c.Name
}

func (d *differ) compareCookies() {
	before := make(map[string]model.Cookie, len(d.baseline.Cookies))
	for _, c := range d.baseline.Cookies {
		before[cookieKey(c)] = c
	}

	after := make(map[string]model.Cookie, len(d.current.Cookies))
	for _, c := range d.current.Cookies {
		after[cookieKey(c)] = c
	}

	for key, c := range after {
		if _, existed := before[key]; existed {
			continue
		}

		d.add(Change{
			Type:     CookieAdded,
			Severity: d.rules.Severity.forCookieAdded(d.current.ConsentMode, c.Party),
			Subject:  key,
			Domain:   c.Domain,
			Party:    c.Party,
			After:    c.Name,
			Detail:   fmt.Sprintf("new %s cookie %q set for %s", c.Party, c.Name, c.Domain),
		})
	}

	for key, c := range before {
		if _, still := after[key]; still {
			continue
		}

		d.add(Change{
			Type:     CookieRemoved,
			Severity: d.rules.Severity.CookieRemoved,
			Subject:  key,
			Domain:   c.Domain,
			Party:    c.Party,
			Before:   c.Name,
			Detail:   fmt.Sprintf("cookie %q for %s is no longer set", c.Name, c.Domain),
		})
	}
}

func (d *differ) compareStatuses() {
	before, after := assets(d.baseline), assets(d.current)

	for key, now := range after {
		was, existed := before[key]
		if !existed || was.status == now.status || now.status == 0 || was.status == 0 {
			continue
		}

		d.add(Change{
			Type:     StatusChanged,
			Severity: d.rules.Severity.forStatusChanged(was.status, now.status),
			Subject:  key,
			Domain:   now.domain,
			Party:    now.party,
			Before:   fmt.Sprintf("%d", was.status),
			After:    fmt.Sprintf("%d", now.status),
			Detail:   fmt.Sprintf("status changed from %d to %d", was.status, now.status),
		})
	}
}

// compareConsent reports a change in how the banner was handled, which
// invalidates the comparability of everything else and must be visible.
func (d *differ) compareConsent() {
	was, now := d.baseline.Consent, d.current.Consent

	if was.Outcome == now.Outcome && was.CMP == now.CMP {
		return
	}

	severity := d.rules.Severity.ConsentChanged

	// Losing the ability to apply consent is worse than gaining it.
	if was.Outcome == model.OutcomeApplied && now.Outcome != model.OutcomeApplied {
		severity = d.rules.Severity.ConsentDegraded
	}

	detail := fmt.Sprintf("consent outcome changed from %s to %s", was.Outcome, now.Outcome)
	if was.CMP != now.CMP {
		detail += fmt.Sprintf(" (CMP %q to %q)", was.CMP, now.CMP)
	}

	if now.Reason != "" {
		detail += ": " + now.Reason
	}

	d.add(Change{
		Type:     ConsentChanged,
		Severity: severity,
		Subject:  string(now.Outcome),
		Before:   string(was.Outcome),
		After:    string(now.Outcome),
		Detail:   detail,
	})
}

// flagDeniedHosts reports hosts on the deny list regardless of the baseline:
// a known tracker is a finding the first time it appears and every time
// after, not only when it is new.
func (d *differ) flagDeniedHosts() {
	if d.rules.Deny.Empty() {
		return
	}

	for domain, info := range hosts(d.current) {
		if !d.rules.Deny.Matches(domain) {
			continue
		}

		d.report.Changes = append(d.report.Changes, Change{
			Type:        DeniedHost,
			Severity:    d.rules.Severity.DeniedHost,
			Target:      d.current.Target,
			ConsentMode: d.current.ConsentMode,
			Subject:     domain,
			Domain:      domain,
			Party:       info.party,
			Phase:       info.phase,
			After:       fmt.Sprintf("%d requests", info.count),
			Detail:      fmt.Sprintf("%s is on the deny list and was contacted", domain),
		})
	}
}

// degraded reports whether a result lost so many requests to incomplete
// observation that its asset list cannot be trusted to be complete.
//
// This is deliberately separate from Truncated. A truncated scan announces
// itself in its termination reason; a scan that goes quiet on schedule and
// terminates "idle" looks trustworthy while quietly missing every asset that
// a failed loader would have pulled in. The second case is the one that
// produced a week of phantom removals, and nothing in the termination reason
// showed it.
func (o Options) degraded(r *model.Result) bool {
	if r == nil {
		return false
	}

	return r.IncompleteRatio() >= o.DegradedFailureRatio
}

// unobservedDomains collects the registrable domains the current scan did not
// fully observe — a request that failed in transit, or one still in flight
// when capture stopped waiting — so a missing asset can be attributed to a
// specific gap rather than to the scan's overall health.
func unobservedDomains(r *model.Result) map[string]struct{} {
	out := make(map[string]struct{})

	for i := range r.Requests {
		if req := &r.Requests[i]; req.Incomplete() && req.Domain != "" {
			out[req.Domain] = struct{}{}
		}
	}

	return out
}

// initiatorDomains returns the registrable domains that caused a request.
// Chrome reports the immediate initiator and, where it can, the whole script
// stack; all of them matter, because any link in that chain failing is enough
// to stop the request happening at all.
func initiatorDomains(req *model.Request) []string {
	var out []string

	add := func(raw string) {
		if raw == "" {
			return
		}

		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return
		}

		domain := classify.RegistrableDomain(u.Hostname())
		if domain == "" || slices.Contains(out, domain) {
			return
		}

		out = append(out, domain)
	}

	add(req.Initiator.URL)

	for _, frame := range req.Initiator.Stack {
		add(frame)
	}

	return out
}

// removalUnprovable reports whether "this is gone" can be stated at all.
//
// The asymmetry is the point. A request that is missing may mean the site
// stopped making it, or may mean wsaw failed to observe it. A request that is
// *present*, on the other hand, can only mean the site made it. So removals
// need a healthy scan to be trustworthy and additions never do: a false
// positive on an addition costs someone a look, while a false negative hides
// a tracker that fired without consent.
//
// There are two ways a removal becomes unprovable, and both are needed. The
// blunt one is a scan that lost so much that nothing about its asset list can
// be trusted. The precise one is attribution: the loss is *not* confined to
// the hosts that failed, because a script that could not be fetched silences
// every request it would have made — a consent banner that never loads takes
// its whole second stage with it, on a different host. Checking the domain
// and the initiator chain catches that case while leaving a healthy scan's
// removals alone, which a ratio on its own cannot do.
func (d *differ) removalUnprovable(domain string, initiators []string) bool {
	unprovable := d.currentDegraded

	if !unprovable {
		if _, missed := d.unobserved[domain]; missed {
			unprovable = true
		}
	}

	if !unprovable {
		for _, initiator := range initiators {
			if _, missed := d.unobserved[initiator]; missed {
				unprovable = true

				break
			}
		}
	}

	if !unprovable {
		return false
	}

	d.report.Suppressed++

	return true
}

// flagDegradation reports a scan whose asset list is incomplete, so that
// "fewer assets than last time" is never mistaken for an improvement.
//
// Both sides are reported. A degraded current scan invalidates removals; a
// degraded baseline invalidates additions, which are still reported rather
// than suppressed, so the reader needs to be told why an addition may be an
// artefact of the comparison.
func (d *differ) flagDegradation() {
	if d.current.Truncated() {
		d.reportDegraded(string(d.current.Termination),
			fmt.Sprintf("scan stopped early (%s); the asset list may be incomplete and differences may be artefacts",
				d.current.Termination))
	}

	if d.currentDegraded {
		reason, _ := d.current.TopIncompleteReason()
		d.reportDegraded(reason, fmt.Sprintf(
			"%d of this scan's requests produced no observation (%.1f%%, mostly %s); "+
				"assets those requests would have loaded are missing for reasons the site did not choose, "+
				"so removals are not reported for this comparison",
			d.current.IncompleteObservations(), d.current.IncompleteRatio()*100, reason))
	}

	if d.baselineDegraded {
		reason, _ := d.baseline.TopIncompleteReason()
		d.reportDegraded(reason, fmt.Sprintf(
			"%d of the baseline scan's requests produced no observation (%.1f%%, mostly %s); "+
				"an asset reported as new here may simply have been missed last time",
			d.baseline.IncompleteObservations(), d.baseline.IncompleteRatio()*100, reason))
	}
}

func (d *differ) reportDegraded(subject, detail string) {
	d.report.Changes = append(d.report.Changes, Change{
		Type:        ScanDegraded,
		Severity:    d.rules.Severity.ScanDegraded,
		Target:      d.current.Target,
		ConsentMode: d.current.ConsentMode,
		Subject:     subject,
		Detail:      detail,
	})
}

// sortChanges orders deterministically: severity first so the worst is at the
// top, then type and subject so two runs produce identical output.
func sortChanges(c []Change) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Severity.Rank() != c[j].Severity.Rank() {
			return c[i].Severity.Rank() > c[j].Severity.Rank()
		}

		if c[i].Type != c[j].Type {
			return c[i].Type < c[j].Type
		}

		return c[i].Subject < c[j].Subject
	})
}
