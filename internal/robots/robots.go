// Package robots decides whether a URL may be scanned under a site's
// robots.txt.
//
// Politeness is configurable per target (NFR §8): scanning one's own
// properties argues for ignoring robots.txt, scanning someone else's argues
// for respecting it. wsaw makes that an explicit choice rather than a hidden
// default.
package robots

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// UserAgent is the token wsaw matches against in robots.txt. A site owner can
// address wsaw specifically rather than only through the wildcard group.
const UserAgent = "wsaw"

// Decision records whether a URL may be fetched and why, so a skip is always
// explainable rather than a silent no-op.
type Decision struct {
	Allowed bool
	// Reason explains the decision in operator-readable terms.
	Reason string
	// Fetched is false when robots.txt could not be read, in which case the
	// fallback applied.
	Fetched bool
}

// Group is one user-agent block of a robots.txt file.
type group struct {
	agents   []string
	allow    []string
	disallow []string
}

// Rules is a parsed robots.txt.
type Rules struct {
	groups []group
	// crawlDelay is recorded but not enforced here; the scheduler owns pacing.
	crawlDelay time.Duration
}

// Parse reads a robots.txt body. Unparseable lines are skipped, matching what
// crawlers do in practice: a malformed file must not make a site unscannable
// or, worse, appear to allow everything.
func Parse(r io.Reader) *Rules {
	rules := &Rules{}

	var current *group

	// A blank line ends a group, so consecutive user-agent lines share rules.
	sawRule := false

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()

		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			if current == nil || sawRule {
				rules.groups = append(rules.groups, group{})
				current = &rules.groups[len(rules.groups)-1]
				sawRule = false
			}

			current.agents = append(current.agents, strings.ToLower(value))

		case "disallow":
			if current == nil {
				continue
			}

			sawRule = true
			current.disallow = append(current.disallow, value)

		case "allow":
			if current == nil {
				continue
			}

			sawRule = true
			current.allow = append(current.allow, value)

		case "crawl-delay":
			if d, err := time.ParseDuration(value + "s"); err == nil {
				rules.crawlDelay = d
			}
		}
	}

	return rules
}

// CrawlDelay returns the declared crawl delay, or zero.
func (r *Rules) CrawlDelay() time.Duration {
	if r == nil {
		return 0
	}

	return r.crawlDelay
}

// Allowed reports whether path may be fetched by the given agent.
//
// Matching follows the usual convention: the most specific matching group
// wins, and within a group the longest matching rule wins, with Allow beating
// Disallow on an equal-length tie.
func (r *Rules) Allowed(agent, path string) bool {
	if r == nil || len(r.groups) == 0 {
		return true
	}

	agent = strings.ToLower(agent)

	g := r.groupFor(agent)
	if g == nil {
		return true
	}

	if path == "" {
		path = "/"
	}

	bestAllow := longestMatch(g.allow, path)
	bestDisallow := longestMatch(g.disallow, path)

	if bestDisallow < 0 {
		return true
	}

	// An empty Disallow value means "allow everything" by convention, which
	// longestMatch reports as a zero-length match.
	if bestDisallow == 0 {
		return true
	}

	return bestAllow >= bestDisallow
}

// groupFor picks the most specific matching group: an exact agent match wins
// over the wildcard, which is how a site addresses one crawler specifically.
func (r *Rules) groupFor(agent string) *group {
	var wildcard *group

	for i := range r.groups {
		for _, a := range r.groups[i].agents {
			if a == agent {
				return &r.groups[i]
			}

			if a == "*" {
				wildcard = &r.groups[i]
			}
		}
	}

	return wildcard
}

// longestMatch returns the length of the longest matching rule, or -1.
func longestMatch(patterns []string, path string) int {
	best := -1

	for _, p := range patterns {
		if p == "" {
			if best < 0 {
				best = 0
			}

			continue
		}

		if matchPattern(p, path) && len(p) > best {
			best = len(p)
		}
	}

	return best
}

// matchPattern supports the "*" and "$" extensions that every major crawler
// honours, since real robots.txt files rely on them.
func matchPattern(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}

	parts := strings.Split(pattern, "*")

	pos := 0

	for i, part := range parts {
		if part == "" {
			continue
		}

		if i == 0 {
			if !strings.HasPrefix(path[pos:], part) {
				return false
			}

			pos += len(part)

			continue
		}

		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}

		pos += idx + len(part)
	}

	if anchored {
		// With a trailing wildcard before "$" any suffix is fine.
		if len(parts) > 1 && parts[len(parts)-1] == "" {
			return true
		}

		return pos == len(path)
	}

	return true
}

// Checker fetches and caches robots.txt per origin.
type Checker struct {
	client *http.Client
	ttl    time.Duration

	// FallbackAllow decides what happens when robots.txt cannot be fetched.
	// Defaulting to allow matches crawler convention: an unreachable
	// robots.txt must not make a site permanently unscannable.
	fallbackAllow bool

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	rules   *Rules
	fetched time.Time
	err     error
}

// Options configures a Checker.
type Options struct {
	Client *http.Client
	// TTL bounds how long a fetched robots.txt is reused.
	TTL time.Duration
	// FallbackAllow decides the outcome when robots.txt cannot be fetched.
	FallbackAllow bool
}

// NewChecker creates a Checker.
func NewChecker(opts Options) *Checker {
	c := &Checker{
		client:        opts.Client,
		ttl:           opts.TTL,
		fallbackAllow: opts.FallbackAllow,
		cache:         make(map[string]cacheEntry),
	}

	if c.client == nil {
		c.client = &http.Client{Timeout: 10 * time.Second}
	}

	if c.ttl <= 0 {
		c.ttl = time.Hour
	}

	return c
}

// Check decides whether rawURL may be scanned.
func (c *Checker) Check(ctx context.Context, rawURL, agent string) Decision {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Decision{Allowed: false, Reason: "target URL could not be parsed: " + err.Error()}
	}

	rules, fetched, err := c.rulesFor(ctx, u)
	if err != nil {
		if c.fallbackAllow {
			return Decision{
				Allowed: true,
				Reason:  "robots.txt could not be fetched (" + err.Error() + "); the configured fallback allows scanning",
			}
		}

		return Decision{
			Allowed: false,
			Reason:  "robots.txt could not be fetched (" + err.Error() + "); the configured fallback refuses scanning",
		}
	}

	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	if rules.Allowed(agent, path) {
		return Decision{Allowed: true, Fetched: fetched, Reason: "allowed by robots.txt"}
	}

	return Decision{
		Allowed: false,
		Fetched: fetched,
		Reason:  fmt.Sprintf("robots.txt disallows %s for user-agent %q", path, agent),
	}
}

func (c *Checker) rulesFor(ctx context.Context, u *url.URL) (*Rules, bool, error) {
	origin := u.Scheme + "://" + u.Host

	c.mu.Lock()

	if e, ok := c.cache[origin]; ok && time.Since(e.fetched) < c.ttl {
		c.mu.Unlock()

		if e.err != nil {
			return nil, false, e.err
		}

		return e.rules, true, nil
	}

	c.mu.Unlock()

	rules, err := c.fetch(ctx, origin)

	c.mu.Lock()
	c.cache[origin] = cacheEntry{rules: rules, fetched: time.Now(), err: err}
	c.mu.Unlock()

	if err != nil {
		return nil, false, err
	}

	return rules, true, nil
}

func (c *Checker) fetch(ctx context.Context, origin string) (*Rules, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// No robots.txt means no restrictions, which is the convention.
		return &Rules{}, nil

	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// Bounded read: a hostile server must not be able to exhaust memory
	// through robots.txt.
	return Parse(io.LimitReader(resp.Body, 1<<20)), nil
}
