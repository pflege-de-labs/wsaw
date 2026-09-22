package httpapi

import (
	"strconv"
	"strings"
)

// What a tile says about the last scan.
//
// Not what the scan loaded — what it changed. Three absolute counts are the
// same three numbers on every render of a site that has not moved, so a board
// full of them asks the reader to diff by memory across every tile. The
// headline is therefore the deviation from the scan the series is compared
// against, and the absolutes move to the anchor's title and aria-label, where
// a reader asks for them rather than being handed them (Story 5.28).
//
// Two things this deliberately does not do. It does not call anything
// "unchanged" without saying what it is unchanged against: the baseline
// somebody approved and yesterday's scan that nobody has looked at are
// different claims (Tenet 5), so the base is named on the tile. And it does
// not turn an uncomparable pair, or a first-ever scan, into "no change" — both
// say what they are and print the absolutes, since with no base those counts
// are the only figures that are true.

// The minus sign is U+2212, not a hyphen: at tile weight a hyphen next to a
// tabular digit reads as punctuation rather than as a sign.
const minusSign = "−"

// The three units the headline prints, in the order it prints them. They are
// the tile's own abbreviations and have nothing to do with the phase marker
// the compare table writes as "pre" (ui.go, compareRow.Present) — same three
// letters, different fact, so they are deliberately not one constant.
const (
	unitRequests   = "req"
	unitThirdParty = "3p"
	unitPreConsent = "pre"
)

// deltaState is which of the headline's four shapes a row renders.
type deltaState int

const (
	// deltaNone is a series with no stored scan at all.
	deltaNone deltaState = iota
	// deltaFirst is a scan with nothing to compare against.
	deltaFirst
	// deltaIncomparable is a pair the differ refused (an errored or skipped
	// scan on either side).
	deltaIncomparable
	// deltaCompared is a real comparison, moved or not.
	deltaCompared
)

// deltaState decides which shape this row's headline takes.
func (r modeRow) deltaState() deltaState {
	switch {
	case r.Series.LastScan == nil:
		return deltaNone
	case r.Series.ComparedTo == nil:
		return deltaFirst
	case !r.Series.ComparedTo.Comparable:
		return deltaIncomparable
	default:
		return deltaCompared
	}
}

// HasScan reports whether there is a scan to describe at all. A series with
// none keeps the board's existing "no observation yet" line.
func (r modeRow) HasScan() bool {
	return r.deltaState() != deltaNone
}

// DeltaLabel is the tile's headline: how the shown scan differs from the scan
// it was compared against, as signed figures — "+7 req · +1 3p · −2 pre".
//
// A figure that did not move is left out; the line exists to carry movement,
// and "+0 req" is the reader's attention spent on nothing. A scan that moved
// nothing at all reads "no change", stated rather than left as an empty line
// for the reader to interpret. Where there is no base, the absolutes stand in,
// with StateLabel saying why.
func (r modeRow) DeltaLabel() string {
	if r.deltaState() != deltaCompared {
		return r.AbsoluteLabel()
	}

	last, base := r.Series.LastScan, r.Series.ComparedTo

	parts := make([]string, 0, 3)
	for _, f := range []struct {
		delta int
		unit  string
	}{
		{last.Requests - base.Requests, unitRequests},
		{last.ThirdPartyDomains - base.ThirdPartyDomains, unitThirdParty},
		{last.PreConsentDomains - base.PreConsentDomains, unitPreConsent},
	} {
		if f.delta != 0 {
			parts = append(parts, signed(f.delta)+" "+f.unit)
		}
	}

	if len(parts) == 0 {
		// Equal counts are not the same fact as an unchanged site: a tracker
		// swapped for another tracker moves no count at all. Where the diff
		// reports changes anyway, the tile says so rather than printing the
		// quiet line over a finding (Tenet 5).
		if r.Series.Severity.Rank() > 0 {
			return "changed, same counts"
		}

		return "no change"
	}

	return strings.Join(parts, " · ")
}

// NoChange reports the quiet state: a real comparison, no count moved, and
// no change found. It is what the headline is muted for, and it is
// deliberately not "the counts are equal" — see DeltaLabel.
func (r modeRow) NoChange() bool {
	return r.deltaState() == deltaCompared && r.DeltaLabel() == "no change"
}

// AbsoluteLabel is the shown scan's own three counts, as Story 5.25 AC1's
// headline printed them. It is what a tile with nothing to compare against
// shows, and what the anchor's title carries in every state.
func (r modeRow) AbsoluteLabel() string {
	if r.Series.LastScan == nil {
		return ""
	}

	s := r.Series.LastScan

	return strconv.Itoa(s.Requests) + " " + unitRequests + " · " +
		strconv.Itoa(s.ThirdPartyDomains) + " " + unitThirdParty + " · " +
		strconv.Itoa(s.PreConsentDomains) + " " + unitPreConsent
}

// StateLabel is the qualifier beside the headline: what the figures are
// measured against, or why they are not measured against anything.
//
// It is never omitted for a comparison, because a deviation without its base
// is not a fact a reader can act on: "no change" against an approved baseline
// means the site is as somebody signed it off, and "no change" against the
// previous scan means only that two unreviewed scans agree.
func (r modeRow) StateLabel() string {
	switch r.deltaState() {
	case deltaNone:
		return ""
	case deltaFirst:
		return "first scan"
	case deltaIncomparable:
		return "not comparable"
	default:
		if r.Series.ComparedTo.Base == BaseApproved {
			return "vs baseline"
		}

		return "vs last scan"
	}
}

// ScanAria is the anchor's accessible name and its title: the deviation the
// tile shows, then both scans' absolute figures, so the numbers the headline
// no longer prints are still one hover or one screen reader away (Story 5.28,
// AC5).
func (r modeRow) ScanAria(target string) string {
	last := r.Series.LastScan
	if last == nil {
		return ""
	}

	var b strings.Builder

	b.WriteString("Open the last scan of ")
	b.WriteString(target)
	b.WriteString(" in ")
	b.WriteString(string(r.Series.Mode))
	b.WriteString(" mode, ")
	b.WriteString(last.ScanID)
	b.WriteString(": ")
	b.WriteString(r.DeltaLabel())

	if s := r.StateLabel(); s != "" {
		b.WriteString(" ")
		b.WriteString(s)
	}

	b.WriteString("; this scan ")
	b.WriteString(counts(last.Requests, last.ThirdPartyDomains, last.PreConsentDomains))

	if base := r.Series.ComparedTo; base != nil {
		b.WriteString("; ")

		if base.Base == BaseApproved {
			b.WriteString("baseline ")
		} else {
			b.WriteString("previous scan ")
		}

		b.WriteString(base.ScanID)
		b.WriteString(" ")
		b.WriteString(counts(base.Requests, base.ThirdPartyDomains, base.PreConsentDomains))
	}

	return b.String()
}

// counts writes the three figures out in words, for a reader who is hearing
// the tile rather than looking at it.
func counts(requests, thirdParty, preConsent int) string {
	return strconv.Itoa(requests) + " requests, " +
		strconv.Itoa(thirdParty) + " third-party domains, " +
		strconv.Itoa(preConsent) + " pre-consent"
}

// signed renders a delta with an explicit sign, always: a bare "7" on a line
// of deviations invites reading it as a total.
func signed(n int) string {
	if n < 0 {
		return minusSign + strconv.Itoa(-n)
	}

	return "+" + strconv.Itoa(n)
}

// ScanClass is the headline anchor's class list: the tile's hit area, plus
// the severity the headline is coloured by.
//
// Colour follows the severity of what changed, and nothing else. It used to
// follow the shown scan's absolute pre-consent count, which marked a tile
// orange for a site that had not moved at all — the reader was handed a
// warning colour for a figure that was the same on the previous render and
// the one before that. A count-shaped deviation ("+3 req") ranks info and now
// reads as quietly as "no change" does, because a board that colours for
// every asset hash is a board nobody reads (Story 5.30's reasoning applied to
// the headline's colour).
//
// The pre-consent fact keeps its is-pre class — it is the hook that marks
// which tiles contacted a third party before consent — but the fact itself is
// carried where it belongs, in the headline's own "pre" figure and in the
// scan's accessible name, not in a colour the reader cannot act on.
func (r modeRow) ScanClass() string {
	class := "watch-scan"

	if s := r.Series.Severity; s != "" {
		class += " sev-" + string(s)
	}

	if last := r.Series.LastScan; last != nil && last.PreConsentDomains > 0 {
		class += " is-pre"
	}

	if r.NoChange() {
		class += " is-quiet"
	}

	return class
}
