package consent

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// DetectionLoader names a CMP found by its loader in the request list rather
// than on the page (Story 2.11).
const DetectionLoader = "loader"

// sha256HexLen is the length of a hex SHA-256 digest.
const sha256HexLen = 64

// checkLoaders refuses a malformed loader or stub at load, rather than as a
// rule that silently never matches mid-scan (Story 2.11, AC1).
func checkLoaders(r Rule) error {
	for _, l := range r.Loaders {
		u, err := url.Parse(l)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("loader %q is not an absolute http(s) URL", l)
		}
	}

	if len(r.Stubs) != 0 && len(r.Loaders) == 0 {
		return errors.New("stubs are matched against loader requests, so a rule with stubs must name its loaders")
	}

	for _, s := range r.Stubs {
		if !isSHA256Hex(s.SHA256) {
			return fmt.Errorf("stub sha256 %q is not 64 lowercase hex characters", s.SHA256)
		}

		if s.Means != StubBot {
			return fmt.Errorf("stub %s means %q; the only defined meaning is %q", s.SHA256, s.Means, StubBot)
		}
	}

	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// CheckLoaders turns a "no CMP detected" outcome into a failure when the page
// requested a known CMP's loader: the vendor's script was there and its CMP
// never started, which is a failure to observe the consent state, not evidence
// that the site asks for none (Story 2.11, Tenet 5).
//
// It is a pure derivation over the recorded requests and the rule pack, so it
// can be recomputed for a stored result (Tenet 4). Any other outcome, and a
// not-needed outcome that was not a detection (consent mode none, a scan that
// did not run), is returned unchanged.
func (s *RuleSet) CheckLoaders(c model.Consent, host, domain string, requests []model.Request) model.Consent {
	if c.Outcome != model.OutcomeNotNeeded || c.Kind != model.CMPKindNone {
		return c
	}

	for _, r := range s.Candidates(host, domain) {
		if req, loader, ok := r.loaderRequest(requests); ok {
			return r.loaderFailure(c, req, loader)
		}
	}

	return c
}

// loaderRequest finds the first recorded request for one of the rule's
// loaders, and the loader it matched.
func (r *Rule) loaderRequest(requests []model.Request) (model.Request, string, bool) {
	for _, req := range requests {
		for _, l := range r.Loaders {
			if strings.HasPrefix(req.URL, l) {
				return req, l, true
			}
		}
	}

	return model.Request{}, "", false
}

// loaderFailure records the finding. The reason quotes only rule-pack data and
// the browser's own status, never the page's URL, which may carry a vendor API
// key and is page-controlled (NFR §4).
func (r *Rule) loaderFailure(c model.Consent, req model.Request, loader string) model.Consent {
	vendor := cmp.Or(r.Vendor, r.Name)

	c.Outcome = model.OutcomeFailed
	c.Kind = model.CMPKindVendor
	c.CMP = vendor
	c.Detection = DetectionLoader
	c.LoaderRequestID = req.RequestID

	// Only a body the browser actually received can be the vendor's stub.
	if stub, ok := r.stubFor(req.BodySHA256); ok && !req.Failed && req.BlockedReason == "" {
		c.BotDetected = true
		c.Reason = fmt.Sprintf("%s disabled itself for this browser: its loader %s returned the stub it serves to bots%s; "+
			"the recorded traffic is pre-consent traffic. A per-target proxy the vendor does not take for a bot avoids this",
			vendor, loader, saying(stub))

		return c
	}

	c.Reason = fmt.Sprintf("%s's loader %s was requested (%s) but the CMP never started; "+
		"the recorded traffic is pre-consent traffic", vendor, loader, describeLoad(req))

	return c
}

func (r *Rule) stubFor(digest string) (Stub, bool) {
	for _, s := range r.Stubs {
		if digest != "" && s.SHA256 == digest && s.Means == StubBot {
			return s, true
		}
	}

	return Stub{}, false
}

func saying(s Stub) string {
	if s.Says == "" {
		return ""
	}

	return fmt.Sprintf(" (%q)", s.Says)
}

func describeLoad(req model.Request) string {
	switch {
	case req.BlockedReason != "":
		return "blocked: " + req.BlockedReason
	case req.Failed:
		return "failed: " + cmp.Or(req.FailureReason, "no reason given")
	default:
		return fmt.Sprintf("status %d, %d bytes", req.Status, req.DecodedSize)
	}
}
