// Package secret resolves indirect credential references and redacts secret
// values from anything wsaw emits.
//
// Credentials must never appear in logs, results, or API responses (NFR §4),
// so redaction is centralized here rather than reimplemented per call site.
package secret

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
)

// Redacted replaces every secret value in output.
const Redacted = "[REDACTED]"

// ErrUnresolved is returned when a reference cannot be resolved.
var ErrUnresolved = errors.New("unresolved secret reference")

// Value is a credential that resolves from an indirect reference and refuses
// to reveal itself through the fmt, JSON, or slog interfaces. Callers reach
// the plaintext only via Reveal, which makes leaks grep-able in review.
type Value struct {
	ref   string
	plain string
	set   bool
}

// Literal wraps an already-known plaintext secret.
func Literal(s string) Value {
	return Value{plain: s, set: s != ""}
}

// refPrefix starts an indirect reference. It is one constant, next to the
// resolver that parses it, because two packages used to carry their own copy
// and the copies decided whether a credential was registered for scrubbing —
// an answer that must not be able to differ from the resolver's.
const refPrefix = "${"

// IsReference reports whether a configured setting names a credential
// indirectly rather than carrying it.
//
// Callers use it to decide how to treat a value they have not resolved yet:
// configuration validation must not judge a reference's contents, and the
// startup path registers what a reference resolved to for scrubbing. Both
// questions are the same one, so it is answered here rather than by a prefix
// test copied into each of them.
func IsReference(setting string) bool {
	return strings.HasPrefix(setting, refPrefix)
}

// Resolve interprets a configured reference. Supported forms:
//
//	${env:NAME}      read from the environment
//	${file:/path}    read from a file, trailing newline trimmed
//	literal text     used as-is
//
// An empty input yields an unset Value, which is not an error: most
// credentials are optional.
func Resolve(ref string) (Value, error) {
	v := Value{ref: ref}

	switch {
	case ref == "":
		return v, nil

	case strings.HasPrefix(ref, refPrefix+"env:") && strings.HasSuffix(ref, "}"):
		name := ref[len("${env:") : len(ref)-1]
		if name == "" {
			return v, fmt.Errorf("%w: empty environment variable name", ErrUnresolved)
		}

		plain, ok := os.LookupEnv(name)
		if !ok {
			return v, fmt.Errorf("%w: environment variable %s is not set", ErrUnresolved, name)
		}

		v.plain, v.set = plain, plain != ""

	case strings.HasPrefix(ref, refPrefix+"file:") && strings.HasSuffix(ref, "}"):
		path := ref[len("${file:") : len(ref)-1]
		if path == "" {
			return v, fmt.Errorf("%w: empty file path", ErrUnresolved)
		}

		b, err := os.ReadFile(path) //nolint:gosec // path comes from operator config, not from a scanned page
		if err != nil {
			// The error carries the path but never the file contents.
			return v, fmt.Errorf("%w: reading %s: %w", ErrUnresolved, path, err)
		}

		plain := strings.TrimRight(string(b), "\r\n")
		v.plain, v.set = plain, plain != ""

	default:
		v.plain, v.set = ref, true
	}

	return v, nil
}

// IsSet reports whether a non-empty secret was resolved.
func (v Value) IsSet() bool { return v.set }

// Reveal returns the plaintext. Every call site is a potential leak; keep the
// value's lifetime as short as possible and never pass it to a logger.
func (v Value) Reveal() string { return v.plain }

// String implements fmt.Stringer with the redacted form, so accidental %v or
// %s formatting cannot leak the value.
func (v Value) String() string {
	if !v.set {
		return ""
	}

	return Redacted
}

// GoString implements fmt.GoStringer so %#v is also safe.
func (v Value) GoString() string { return v.String() }

// LogValue implements slog.LogValuer so structured logging is safe by
// default, in every handler. Returning a slog.Value rather than any means
// slog resolves it as a LogValuer instead of falling back to whichever
// stringer a given handler happens to try.
func (v Value) LogValue() slog.Value { return slog.StringValue(v.String()) }

// MarshalJSON emits the redacted form, so a Value embedded in an API response
// or a stored result cannot leak.
func (v Value) MarshalJSON() ([]byte, error) {
	if !v.set {
		return []byte(`""`), nil
	}

	return []byte(`"` + Redacted + `"`), nil
}

// RedactURL removes userinfo from a URL so proxy and webhook URLs can be
// logged and stored. An unparseable URL is reported as entirely redacted
// rather than echoed, since it may still contain a credential.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Redacted
	}

	// Userinfo is rebuilt by hand: url.Userinfo percent-encodes the redaction
	// marker, which makes redacted output harder to read than it needs to be.
	var userinfo string

	if u.User != nil {
		userinfo = u.User.Username()
		if _, hasPassword := u.User.Password(); hasPassword {
			userinfo += ":" + Redacted
		}

		userinfo += "@"

		u.User = nil
	}

	// Query strings on webhook URLs frequently carry tokens.
	if u.RawQuery != "" {
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""

		return u.Scheme + "://" + userinfo + u.Host + u.EscapedPath() + "?" + Redacted
	}

	if userinfo == "" {
		return u.String()
	}

	return u.Scheme + "://" + userinfo + u.Host + u.EscapedPath()
}

// Registry tracks resolved plaintexts so that arbitrary text — an error
// message from a third-party library, a page's response body — can be scrubbed
// before it reaches a log or a result.
type Registry struct {
	mu     sync.RWMutex
	values []string
}

// Add registers a plaintext for scrubbing. Empty and very short values are
// ignored: scrubbing a one-character secret would mangle unrelated output.
func (r *Registry) Add(v Value) {
	const minScrubLen = 4

	if !v.set || len(v.plain) < minScrubLen {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.values {
		if existing == v.plain {
			return
		}
	}

	r.values = append(r.values, v.plain)
}

// Scrub replaces every registered plaintext in s with the redacted marker.
func (r *Registry) Scrub(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, Redacted)
	}

	return s
}
