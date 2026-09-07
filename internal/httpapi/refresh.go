package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Auto-refresh of the interface (Story 5.16).
//
// The dashboard is used two ways. Someone opens it to check a target, and
// someone leaves it on a second monitor or a wall display. The second use is
// what this is for: a page that last told the truth an hour ago is worse than
// no page, because it looks current.
//
// The interval is a property of the person looking rather than of the
// deployment, so it lives in a cookie and in the URL rather than in wsaw's
// configuration. Configuration sets the default a fresh browser gets; one
// viewer's wall display must not change what anybody else sees (Tenet 15).

// refreshCookie holds a viewer's chosen interval in seconds, with 0 meaning
// off.
const refreshCookie = "wsaw_refresh"

// labelOff is how the interface spells "do not refresh", both when it renders
// the setting and when it reads one back.
const labelOff = "off"

// refreshFloor is the shortest interval the interface will accept.
//
// Every refresh re-renders the whole dashboard, which reads the latest result
// of every target and consent mode. A one-second interval across two hundred
// targets is a denial of service against one's own daemon (Story 5.16, AC8).
const refreshFloor = 5 * time.Second

// refreshCeiling bounds the other end, so a mistyped value cannot leave a
// page that claims it will refresh and effectively never does.
const refreshCeiling = 24 * time.Hour

// refreshWhileRunningInterval is what a page falls back to while a scan is in
// flight and the viewer has expressed no preference (Story 5.12).
const refreshWhileRunningInterval = 10 * time.Second

// refreshChoices are the intervals the control offers. A fixed list rather
// than a free-text field: the floor is then obvious from the options instead
// of being a rejection message.
var refreshChoices = []time.Duration{
	0,
	10 * time.Second,
	30 * time.Second,
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
}

// refreshSetting is what the interface decided to do about refreshing, and
// why. The reason is rendered, because a page that reloads silently invites
// the reader to trust whatever is on it (AC5).
type refreshSetting struct {
	// Interval is the effective interval; zero means no auto-refresh.
	Interval time.Duration

	// Chosen is the viewer's own choice, if they made one. It is separate
	// from Interval because "off, because I said so" and "off, because
	// nothing is happening" are different states.
	Chosen    time.Duration
	HasChosen bool

	// FromURL marks an interval that came from the query string, which is not
	// remembered: a wall display is pointed at a URL, and a link somebody
	// shares should not silently rewrite the recipient's preference.
	FromURL bool

	// HeldReason is why this particular page does not refresh whatever
	// interval is in force, and is empty on the pages that do (Story 5.18).
	// It is a sentence the page shows, because a page that behaves
	// differently from the one the reader came from has to say so rather than
	// leave them wondering whether refreshing is broken (AC2).
	HeldReason string
}

// hold turns refreshing off for one page and records why.
//
// The viewer's own choice is kept, so the control still shows what the
// interface is set to and can still be changed from here — a reader may set
// the interval from anywhere, it just does not apply to the page they are on
// (Story 5.18, AC5).
func (s refreshSetting) hold(reason string) refreshSetting {
	s.Interval = 0
	s.HeldReason = reason

	return s
}

// Held reports whether this page holds still by its own nature rather than
// because of the interval in force.
func (s refreshSetting) Held() bool { return s.HeldReason != "" }

// Seconds renders the interval for a meta refresh and for the enhancement
// script.
func (s refreshSetting) Seconds() int { return int(s.Interval.Seconds()) }

// Enabled reports whether the page will refresh itself.
func (s refreshSetting) Enabled() bool { return s.Interval > 0 }

// Label describes the setting in the words the page shows.
func (s refreshSetting) Label() string {
	if s.Interval <= 0 {
		return labelOff
	}

	return humaniseInterval(s.Interval)
}

// refreshFor decides the interval for one request.
//
// Precedence is deliberate: the URL beats the cookie because pointing a
// screen at an address should need no further setup, and an explicit choice
// of "off" beats the automatic reload a running scan would otherwise trigger
// — the reader asked for the page to hold still, the running scan is visible
// on it, and a manual reload is one keystroke away (AC7).
func (s *Server) refreshFor(r *http.Request, running int) refreshSetting {
	out := refreshSetting{}

	if raw := r.URL.Query().Get("refresh"); raw != "" {
		if d, ok := parseRefresh(raw); ok {
			out.Chosen = d
			out.HasChosen = true
			out.FromURL = true
			out.Interval = d

			return out
		}
	}

	if c, err := r.Cookie(refreshCookie); err == nil {
		if d, ok := parseRefresh(c.Value); ok {
			out.Chosen = d
			out.HasChosen = true
			out.Interval = d

			return out
		}
	}

	// No preference. A page with a scan in flight reloads so the pending row
	// does not sit there for ever (Story 5.12); otherwise the deployment's
	// default applies, which is what makes a wall display right the first
	// time it is opened (AC4).
	switch {
	case running > 0:
		out.Interval = refreshWhileRunningInterval
	default:
		out.Interval = s.opts.RefreshDefault
	}

	return out
}

// parseRefresh reads an interval from a form value, a cookie, or a query
// parameter. It accepts plain seconds and Go durations, because "30" and
// "30s" are both what somebody typing a URL would expect.
func parseRefresh(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)

	switch strings.ToLower(raw) {
	case "":
		return 0, false
	case labelOff, "0", "none":
		return 0, true
	}

	if n, err := strconv.Atoi(raw); err == nil {
		return clampRefresh(time.Duration(n) * time.Second), true
	}

	if d, err := time.ParseDuration(raw); err == nil {
		return clampRefresh(d), true
	}

	return 0, false
}

// clampRefresh holds an interval inside the documented bounds rather than
// refusing it. A viewer who asks for one second gets the floor and can see
// what they got, which is more useful than an error page.
func clampRefresh(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d < refreshFloor:
		return refreshFloor
	case d > refreshCeiling:
		return refreshCeiling
	default:
		return d
	}
}

// handleUIRefresh stores a viewer's choice and sends them back where they
// were.
//
// A POST rather than a link, so it is a deliberate state change and carries
// the same CSRF protection as any other write (Story 5.11, AC6) — and a
// redirect afterwards, so the reload that follows is a GET of the page rather
// than a repeat of this submission (AC9).
func (s *Server) handleUIRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}

	interval, ok := parseRefresh(r.FormValue("interval"))
	if !ok {
		s.uiRedirectError(w, r, r.FormValue("return"), "that is not an interval wsaw understands")

		return
	}

	// A session cookie: it is a display preference, not an identity, and it
	// carries nothing worth protecting beyond being same-site.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // no secret, and Secure follows TLS as elsewhere
		Name:     refreshCookie,
		Value:    strconv.Itoa(int(interval.Seconds())),
		Path:     "/",
		HttpOnly: false, // the enhancement script reads it to show the setting
		Secure:   s.opts.TLSCert != "",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})

	// Back to the page they were on, with the flash parameters dropped: the
	// refresh that follows must not keep re-showing a message about something
	// that happened once (AC9).
	//
	// #nosec G710 -- safeLocal reduces the destination to a path on this
	// origin; the taint analyser cannot follow the sanitiser, but
	// TestTheReturnPathCannotLeaveThisOrigin can. Same treatment as
	// uiRedirectOK.
	http.Redirect(w, r, safeLocal(r.FormValue("return")), http.StatusSeeOther)
}

// refreshTarget is the URL a refresh should load.
//
// It is the current path without the flash parameters. A meta refresh
// re-requests whatever URL it is given, so refreshing the URL as it stands
// would re-show "Baseline approved" every thirty seconds for as long as the
// tab stayed open (AC9).
func refreshTarget(r *http.Request) string {
	q := r.URL.Query()

	q.Del("ok")
	q.Del("err")

	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}

	return u.String()
}

// humaniseInterval renders a duration the way the page says it.
func humaniseInterval(d time.Duration) string {
	switch {
	case d <= 0:
		return labelOff
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d%time.Minute == 0 && d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return d.String()
	}
}
