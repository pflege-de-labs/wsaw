// Package share mints and verifies the signed links that let somebody read
// one scan result without being given wsaw's API token (Story 5.19).
//
// The token is a JWT, and it is written here rather than taken from a library
// for one reason: the security of this feature is almost entirely in what the
// verifier refuses, and the refusals are short enough to read in one sitting.
// A fixed algorithm, a signature checked before any claim is looked at, a
// constant-time comparison, and claims decoded strictly. A library would do
// the same thing behind a configuration surface where "accept these
// algorithms" is a thing one can get wrong.
//
// What a link grants is deliberately narrow: one target, one consent mode,
// one scan, read-only, until it expires.
//
// What it cannot do is be revoked. Nothing is stored, so there is no record
// to delete — verification is arithmetic on the token itself. That is why
// validity is short by default and why rotating the signing key, which
// invalidates every outstanding link at once, is the documented way to
// withdraw access in a hurry.
package share

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors a caller may want to tell apart. A person holding a link needs to
// know whether it has expired or was never valid; those are different facts
// and neither is worth hiding.
var (
	// ErrExpired means the token was genuine and its time has passed.
	ErrExpired = errors.New("this link has expired")
	// ErrInvalid means the token is not one wsaw issued, or has been altered.
	ErrInvalid = errors.New("this link is not valid")
	// ErrScope means the token is genuine but names a different result.
	ErrScope = errors.New("this link is for a different scan")
)

// The token's fixed shape. The algorithm is a constant rather than a field
// read from the token: trusting the algorithm a token names — "none" above
// all — is the oldest way to forge a JWT (Story 5.19, AC6).
const (
	algHS256 = "HS256"
	typJWT   = "JWT"

	// audience scopes a token to this feature, so one minted for anything
	// else wsaw might sign later cannot be replayed as a share link.
	audience = "wsaw-share"

	issuer = "wsaw"
)

// minKeyLength is the shortest signing key wsaw will use. HS256's security
// rests on the key alone, and a short one is guessable offline by anybody who
// holds a single link.
const minKeyLength = 32

// Claims are what a share token asserts.
type Claims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`

	Target      string `json:"tgt"`
	ConsentMode string `json:"mode"`
	ScanID      string `json:"scan"`

	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// Expiry is when the token stops working.
func (c Claims) Expiry() time.Time { return time.Unix(c.ExpiresAt, 0).UTC() }

// header is the JWT header wsaw writes. It is also the only header it
// accepts.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Signer mints and verifies tokens.
type Signer struct {
	key []byte

	// validity and maxValidity bound how long a link lasts. The maximum is
	// enforced here rather than trusted from the caller, so no request can
	// mint a link that outlives the policy.
	validity    time.Duration
	maxValidity time.Duration

	// now is injectable so expiry is testable without sleeping.
	now func() time.Time
}

// Options configures a Signer.
type Options struct {
	Key         string
	Validity    time.Duration
	MaxValidity time.Duration

	// Now defaults to time.Now.
	Now func() time.Time
}

// New builds a Signer, refusing a configuration that cannot be safe.
func New(opts Options) (*Signer, error) {
	if len(opts.Key) < minKeyLength {
		return nil, fmt.Errorf(
			"share: the signing key must be at least %d characters; HS256's security is the key alone, "+
				"and a short one can be recovered offline from a single shared link",
			minKeyLength)
	}

	if opts.MaxValidity <= 0 {
		return nil, errors.New("share: a maximum validity is required")
	}

	if opts.Validity <= 0 {
		return nil, errors.New("share: a default validity is required")
	}

	if opts.Validity > opts.MaxValidity {
		return nil, fmt.Errorf("share: the default validity (%s) exceeds the maximum (%s)",
			opts.Validity, opts.MaxValidity)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Signer{
		key:         []byte(opts.Key),
		validity:    opts.Validity,
		maxValidity: opts.MaxValidity,
		now:         now,
	}, nil
}

// DefaultValidity is how long a link lasts when the caller does not say.
func (s *Signer) DefaultValidity() time.Duration { return s.validity }

// MaxValidity is the longest a link may last.
func (s *Signer) MaxValidity() time.Duration { return s.maxValidity }

// Mint issues a token for one result. A validity of zero takes the default,
// and anything above the maximum is clamped to it rather than refused: the
// operator gets a link, and the page tells them when it expires.
func (s *Signer) Mint(target, mode, scanID string, validity time.Duration) (string, Claims, error) {
	if target == "" || mode == "" || scanID == "" {
		return "", Claims{}, errors.New("share: a link needs a target, a consent mode and a scan")
	}

	if validity <= 0 {
		validity = s.validity
	}

	if validity > s.maxValidity {
		validity = s.maxValidity
	}

	now := s.now()

	claims := Claims{
		Issuer:      issuer,
		Audience:    audience,
		Target:      target,
		ConsentMode: mode,
		ScanID:      scanID,
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(validity).Unix(),
	}

	headerJSON, err := json.Marshal(header{Alg: algHS256, Typ: typJWT})
	if err != nil {
		return "", Claims{}, fmt.Errorf("share: encoding the token header: %w", err)
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", Claims{}, fmt.Errorf("share: encoding the token claims: %w", err)
	}

	signing := encode(headerJSON) + "." + encode(claimsJSON)

	return signing + "." + encode(s.sign(signing)), claims, nil
}

// Verify checks a token and reports what it grants.
//
// The order matters and is the point of the function: the signature is
// checked before any claim is read, so nothing a forged token asserts is ever
// acted on.
func (s *Signer) Verify(token string) (Claims, error) {
	headerPart, claimsPart, sigPart, ok := split(token)
	if !ok {
		return Claims{}, ErrInvalid
	}

	sig, err := decode(sigPart)
	if err != nil {
		return Claims{}, ErrInvalid
	}

	// Constant-time, so the signature cannot be recovered a byte at a time.
	if !hmac.Equal(sig, s.sign(headerPart+"."+claimsPart)) {
		return Claims{}, ErrInvalid
	}

	// Only now is anything the token says worth reading — and the header is
	// checked against the one algorithm wsaw uses rather than used to choose
	// one.
	headerJSON, err := decode(headerPart)
	if err != nil {
		return Claims{}, ErrInvalid
	}

	var h header

	if err := strictUnmarshal(headerJSON, &h); err != nil {
		return Claims{}, ErrInvalid
	}

	if h.Alg != algHS256 || h.Typ != typJWT {
		return Claims{}, ErrInvalid
	}

	claimsJSON, err := decode(claimsPart)
	if err != nil {
		return Claims{}, ErrInvalid
	}

	var claims Claims

	if err := strictUnmarshal(claimsJSON, &claims); err != nil {
		return Claims{}, ErrInvalid
	}

	if claims.Issuer != issuer || claims.Audience != audience {
		return Claims{}, ErrInvalid
	}

	if claims.Target == "" || claims.ConsentMode == "" || claims.ScanID == "" {
		return Claims{}, ErrInvalid
	}

	if claims.ExpiresAt <= 0 || !s.now().Before(claims.Expiry()) {
		// Reported as expiry rather than as invalidity: the holder needs to
		// know to ask for a new link, not to wonder whether wsaw is broken.
		return Claims{}, ErrExpired
	}

	return claims, nil
}

// Allows reports whether a verified token covers exactly this result.
//
// Kept separate from Verify so the scope check cannot be forgotten: a caller
// has to name the result it is about to serve.
func (c Claims) Allows(target, mode, scanID string) bool {
	return c.Target == target && c.ConsentMode == mode && c.ScanID == scanID
}

func (s *Signer) sign(signing string) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(signing))

	return mac.Sum(nil)
}

// split takes a token apart without allocating on a malformed one.
func split(token string) (headerPart, claimsPart, sigPart string, ok bool) {
	first := strings.IndexByte(token, '.')
	if first <= 0 {
		return "", "", "", false
	}

	rest := token[first+1:]

	second := strings.IndexByte(rest, '.')
	if second <= 0 {
		return "", "", "", false
	}

	sigPart = rest[second+1:]
	if sigPart == "" || strings.ContainsRune(sigPart, '.') {
		return "", "", "", false
	}

	return token[:first], rest[:second], sigPart, true
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// strictUnmarshal refuses unknown fields, so a token cannot carry claims wsaw
// does not understand and would therefore not check.
func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return err
	}

	// Exactly one JSON value, nothing appended.
	if dec.More() {
		return errors.New("share: trailing data in token segment")
	}

	return nil
}
