package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"sync"
	"time"
)

// One-time browser sign-in (Story 5.21).
//
// "wsaw ui" already holds the standing API token — it read it from the same
// config file the daemon did — so making an operator retype it into a login
// form buys nothing. Instead the CLI spends that token once, out of band,
// to ask the daemon for a token that is good for exactly one thing: setting
// the session cookie in whatever browser opens the link next. That token
// never carries the standing credential, is good for one redemption, expires
// in seconds, and is refused from anywhere but loopback regardless of how
// the interface itself is exposed — because unlike the standing token, it
// has no signature to verify and nothing to check it against but its own
// short existence in memory.

// oneTimeLoginTTL bounds how long a minted sign-in link stays redeemable.
// Short on purpose: the link exists only to cover the time between the CLI
// minting it and the browser it opens making one request.
const oneTimeLoginTTL = 30 * time.Second

// otpStore holds one-time login tokens in memory. A restart invalidates every
// outstanding link, which is fine: they are seconds old at most, minted by a
// CLI that can simply ask again.
type otpStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newOTPStore() *otpStore {
	return &otpStore{tokens: make(map[string]time.Time)}
}

// issue mints a random, single-use token and returns it with its lifetime.
func (o *otpStore) issue() (string, time.Duration, error) {
	var raw [32]byte

	if _, err := rand.Read(raw[:]); err != nil {
		return "", 0, err
	}

	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := time.Now()

	o.mu.Lock()
	defer o.mu.Unlock()

	o.sweep(now)
	o.tokens[token] = now.Add(oneTimeLoginTTL)

	return token, oneTimeLoginTTL, nil
}

// redeem consumes token if it is known and not expired. Either way the token
// is gone afterwards: a second redemption, of a used token or a stale one,
// always fails the same way an expired share link does (Story 5.19, AC7) —
// explicitly, not as an unexplained rejection.
func (o *otpStore) redeem(token string) bool {
	if token == "" {
		return false
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	expiry, ok := o.tokens[token]
	delete(o.tokens, token)

	return ok && time.Now().Before(expiry)
}

// sweep drops expired tokens so a long-running daemon's map does not grow
// with every mint that a browser never redeemed.
func (o *otpStore) sweep(now time.Time) {
	for token, expiry := range o.tokens {
		if now.After(expiry) {
			delete(o.tokens, token)
		}
	}
}

// otpRedeemWindow and otpRedeemMax bound redemption attempts per source
// address. The token's own entropy already makes guessing infeasible; this
// is the belt to that suspenders, and it costs a legitimate caller nothing —
// one browser, one request.
const (
	otpRedeemWindow = time.Minute
	otpRedeemMax    = 20
)

// loginFailWindow and loginFailMax bound wrong-token submissions to the
// login form per source address. The form is the one endpoint reachable
// without any prior credential, so guessing the standing token there is
// bounded by more than the token's own entropy — the same reasoning as the
// one-time-link limiter above. Only failures are counted: a successful
// login costs no window entry, so signing in cannot lock a legitimate
// caller out.
const (
	loginFailWindow = time.Minute
	loginFailMax    = 20
)

// otpLimiter is a plain per-address sliding window.
type otpLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time

	// window and max are the limiter's own policy, set at construction so
	// the one-time-link limiter and the login-form limiter can differ
	// without the sliding-window logic being written twice.
	window time.Duration
	max    int
}

func newOTPLimiter() *otpLimiter {
	return &otpLimiter{
		hits:   make(map[string][]time.Time),
		window: otpRedeemWindow,
		max:    otpRedeemMax,
	}
}

// newLoginLimiter bounds failed login-form submissions per source address.
func newLoginLimiter() *otpLimiter {
	return &otpLimiter{
		hits:   make(map[string][]time.Time),
		window: loginFailWindow,
		max:    loginFailMax,
	}
}

func (l *otpLimiter) allow(remoteAddr string) bool {
	host := hostOf(remoteAddr)
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	kept := l.hits[host][:0]

	for _, t := range l.hits[host] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= l.max {
		l.hits[host] = kept

		return false
	}

	l.hits[host] = append(kept, now)

	return true
}

// forget drops an address's window. A successful login is not a failure,
// so it must not count towards the login form's budget even though the
// check ran before the comparison could know the answer.
func (l *otpLimiter) forget(remoteAddr string) {
	host := hostOf(remoteAddr)

	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.hits, host)
}

func hostOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}

	return host
}

// isLoopback reports whether remoteAddr — an http.Request.RemoteAddr — is the
// local machine. The redemption endpoint trusts nothing but this: it has no
// signature and no CSRF token, so it is never reachable from anywhere the
// interface's own "Listen" configuration was not asked to expose.
func isLoopback(remoteAddr string) bool {
	ip := net.ParseIP(hostOf(remoteAddr))

	return ip != nil && ip.IsLoopback()
}

type loginTokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expiresIn"`
}

// handleMintLoginToken issues a one-time login token to a caller that already
// holds the standing API token — normally "wsaw ui", authenticating exactly
// as any other API client does (Story 5.11, AC2). It grants nothing the
// caller could not already do with that token; it only lets a browser start
// a session without ever seeing it.
func (s *Server) handleMintLoginToken(w http.ResponseWriter, _ *http.Request) {
	if !s.opts.Token.IsSet() {
		writeJSONError(w, http.StatusBadRequest, "no token is configured; there is no session to start")

		return
	}

	token, ttl, err := s.otp.issue()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "minting a sign-in token failed")

		return
	}

	writeJSON(w, http.StatusOK, loginTokenResponse{Token: token, ExpiresIn: int(ttl.Seconds())})
}

// handleRedeemLoginToken is the other half: it never carries the standing
// token, works only from loopback whatever "Listen" is configured to, and
// consumes its token whether or not the attempt succeeds.
func (s *Server) handleRedeemLoginToken(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Token.IsSet() {
		http.Redirect(w, r, "/", http.StatusSeeOther)

		return
	}

	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "one-time sign-in is only available from the local machine", http.StatusForbidden)

		return
	}

	if !s.loginLimiter.allow(r.RemoteAddr) {
		http.Error(w, "too many sign-in attempts", http.StatusTooManyRequests)

		return
	}

	if !s.otp.redeem(r.PathValue("token")) {
		s.uiRedirectError(w, r, "/login", "this sign-in link has expired or was already used")

		return
	}

	s.setSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
