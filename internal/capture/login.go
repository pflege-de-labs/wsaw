package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// FormLogin is a login form wsaw posts before navigating, for a site behind a
// password page rather than HTTP authentication — a Shopify storefront
// password, for one.
type FormLogin struct {
	// URL is where the form is posted.
	URL string
	// Fields are the form's fields. Every value is treated as a secret.
	Fields map[string]secret.Value
}

// logIn posts the login form outside the browser and gives the browser the
// cookies the response set.
//
// The exchange is deliberately not part of the observation: the scan is of
// the page behind the form, and loading the form in the browser would record
// the login page's own assets, cookies and storage as if the protected page
// had produced them.
func (s *session) logIn() error {
	if s.opts.FormLogin == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(s.runCtx, s.opts.NavTimeout)
	defer cancel()

	cookies, err := s.postLogin(ctx)
	if err != nil {
		return fmt.Errorf("logging in at %s: %w", s.opts.FormLogin.URL, err)
	}

	if err := chromedp.Do(ctx, call(network.SetCookies, network.SetCookiesParams{Cookies: cookies})); err != nil {
		return fmt.Errorf("logging in at %s: handing cookies to the browser: %w", s.opts.FormLogin.URL, err)
	}

	return nil
}

func (s *session) postLogin(ctx context.Context) ([]*network.CookieParam, error) {
	login := s.opts.FormLogin

	form := url.Values{}
	for name, value := range login.Fields {
		form.Set(name, value.Reveal())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, login.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// The same headers the browser sends, so a site behind basic auth as well
	// as a login form, or one that keys its session on the user agent, sees
	// one visitor rather than two.
	for name, value := range s.headers() {
		req.Header.Set(name, fmt.Sprint(value))
	}

	if s.opts.UserAgent != "" {
		req.Header.Set("User-Agent", s.opts.UserAgent)
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport is not an *http.Transport")
	}

	transport = transport.Clone()

	if s.opts.Proxy != "" {
		proxy, err := url.Parse(s.opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}

		transport.Proxy = http.ProxyURL(proxy)
	}

	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		// The response to the post is what carries the session cookie; where
		// it redirects to is the browser's to follow, not this client's.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }() // a read-only body; nothing is lost if closing fails

	// Drained, not read: the body of a login response says nothing wsaw
	// trusts, and a failed drain costs only connection reuse.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// A refused password is not reliably an error status — Shopify answers
	// 200 and sets cookies either way — so success is judged after
	// navigating, by checkLoggedIn.
	var cookies []*network.CookieParam

	for _, c := range resp.Cookies() {
		cookies = append(cookies, cookieParam(login.URL, c))
	}

	if len(cookies) == 0 {
		return nil, errors.New("the response set no cookie, so there is no session to carry into the scan")
	}

	return cookies, nil
}

// cookieParam scopes a cookie the way the browser would have, had it made the
// request itself: to the login URL's host unless the cookie names a domain,
// so it is never sent to a third party.
func cookieParam(loginURL string, c *http.Cookie) *network.CookieParam {
	p := &network.CookieParam{
		Name:     c.Name,
		Value:    c.Value,
		URL:      loginURL,
		Domain:   c.Domain,
		Path:     c.Path,
		Secure:   c.Secure,
		HTTPOnly: c.HttpOnly,
	}

	if p.Path == "" {
		p.Path = "/"
	}

	switch c.SameSite {
	case http.SameSiteLaxMode:
		p.SameSite = network.CookieSameSiteLax
	case http.SameSiteStrictMode:
		p.SameSite = network.CookieSameSiteStrict
	case http.SameSiteNoneMode:
		p.SameSite = network.CookieSameSiteNone
	case http.SameSiteDefaultMode:
	}

	return p
}

// checkLoggedIn fails the scan when navigation ended on the login page: the
// page behind the form was never seen, and recording the login page in its
// place would be a clean-looking result of the wrong page (Tenet 5).
func (s *session) checkLoggedIn() error {
	if s.opts.FormLogin == nil {
		return nil
	}

	const locationTimeout = 5 * time.Second

	ctx, cancel := context.WithTimeout(s.runCtx, locationTimeout)
	defer cancel()

	final, err := chromedp.Run(ctx, chromedp.Location())
	if err != nil {
		return fmt.Errorf("checking the login held: %w", err)
	}

	if samePage(final, s.opts.FormLogin.URL) {
		return fmt.Errorf("login was not accepted: navigating to %s ended on the login page %s", s.opts.URL, final)
	}

	return nil
}

func formLoginURL(l *FormLogin) string {
	if l == nil {
		return ""
	}

	return l.URL
}

// samePage reports whether two URLs name the same page, ignoring query and
// fragment.
func samePage(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)

	if errA != nil || errB != nil {
		return false
	}

	return strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/")
}
