package httpapi

// The one-time login building blocks (Story 5.21), tested directly: a
// request cannot easily force a stale map entry or a specific remote
// address, and those are exactly the branches that decide whether a link can
// be replayed.

import (
	"testing"
	"time"
)

func TestOTPStoreSingleUse(t *testing.T) {
	t.Parallel()

	o := newOTPStore()

	token, ttl, err := o.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if ttl <= 0 {
		t.Fatalf("ttl = %v, want positive", ttl)
	}

	if !o.redeem(token) {
		t.Fatal("first redemption should succeed")
	}

	if o.redeem(token) {
		t.Fatal("second redemption of the same token should fail")
	}
}

func TestOTPStoreExpires(t *testing.T) {
	t.Parallel()

	o := newOTPStore()

	token, _, err := o.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Backdate the token's own expiry rather than sleeping past the real
	// TTL, so the test is instant and still exercises the real check.
	o.mu.Lock()
	o.tokens[token] = time.Now().Add(-time.Second)
	o.mu.Unlock()

	if o.redeem(token) {
		t.Fatal("an expired token should not redeem")
	}
}

func TestOTPStoreRejectsUnknownAndEmpty(t *testing.T) {
	t.Parallel()

	o := newOTPStore()

	if o.redeem("") {
		t.Error("an empty token should never redeem")
	}

	if o.redeem("never-issued") {
		t.Error("a token that was never issued should not redeem")
	}
}

func TestOTPStoreSweepDropsExpiredEntries(t *testing.T) {
	t.Parallel()

	o := newOTPStore()

	stale, _, err := o.issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	o.mu.Lock()
	o.tokens[stale] = time.Now().Add(-time.Minute)
	o.mu.Unlock()

	// A second issue sweeps the stale entry as a side effect.
	if _, _, err := o.issue(); err != nil {
		t.Fatalf("issue: %v", err)
	}

	o.mu.Lock()
	_, stillThere := o.tokens[stale]
	o.mu.Unlock()

	if stillThere {
		t.Error("an expired token should have been swept, not kept forever")
	}
}

func TestOTPLimiterBoundsAttemptsPerAddress(t *testing.T) {
	t.Parallel()

	l := newOTPLimiter()

	addr := "127.0.0.1:5555"

	for i := range otpRedeemMax {
		if !l.allow(addr) {
			t.Fatalf("attempt %d unexpectedly blocked", i)
		}
	}

	if l.allow(addr) {
		t.Error("an attempt beyond the limit should be blocked")
	}

	// A different address is unaffected by the first one's exhaustion.
	if !l.allow("10.0.0.9:1") {
		t.Error("a different address should not be rate-limited by another's attempts")
	}
}

// The login form's limiter is a second instance of the same sliding window
// with its own budget, so exhausting one does not touch the other — and a
// successful sign-in forgets the address's window entirely.
func TestLoginLimiterHasItsOwnBudgetAndForgetsOnSuccess(t *testing.T) {
	t.Parallel()

	login := newLoginLimiter()
	redeem := newOTPLimiter()
	addr := "127.0.0.1:5555"

	for range loginFailMax {
		if !login.allow(addr) {
			t.Fatal("an in-budget attempt was blocked")
		}
	}

	if login.allow(addr) {
		t.Error("an attempt beyond the login limit should be blocked")
	}

	// The one-time-link limiter shares nothing with the exhausted one.
	if !redeem.allow(addr) {
		t.Error("the link limiter was affected by the login limiter's exhaustion")
	}

	// Forgetting clears the window: the next attempt starts fresh.
	login.forget(addr)

	if !login.allow(addr) {
		t.Error("an attempt after a forgotten window was blocked")
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:12345", true},
		{"[::1]:12345", true},
		{"203.0.113.5:12345", false},
		{"not-an-address", false},
		{"", false},
	}

	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
