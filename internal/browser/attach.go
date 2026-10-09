package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

const (
	// dialTimeout bounds opening the CDP websocket.
	dialTimeout = 10 * time.Second

	// discoverTimeout bounds asking an endpoint for its websocket address.
	discoverTimeout = 20 * time.Second

	// maxVersionBody bounds the /json/version answer, a few hundred bytes from
	// any real browser.
	maxVersionBody = 1 << 20
)

// dialer connects to the websocket of a browser wsaw started, through the
// event tap.
func (t *eventTap) dialer(ctx context.Context, wsURL string) (chromedp.Transport, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := remote.DialContext(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to the browser: %w", err)
	}

	return t.wrap(conn), nil
}

// attacher is a chromedp allocator for a browser wsaw did not start: a remote
// endpoint or a container. It does what the allocator of chromedp's remote
// module does, except that the connection goes through the event tap, which
// that allocator has no way to add.
type attacher struct {
	endpoint string
	tap      *eventTap

	wg sync.WaitGroup
}

func newAttacher(endpoint string, tap *eventTap) *attacher {
	return &attacher{endpoint: endpoint, tap: tap}
}

// Allocate connects to the browser. It satisfies chromedp.Allocator.
func (a *attacher) Allocate(ctx context.Context, opts ...chromedp.BrowserOption) (*chromedp.Browser, error) {
	if chromedp.FromContext(ctx) == nil {
		return nil, chromedp.ErrInvalidContext
	}

	wsURL, err := debuggerURL(ctx, a.endpoint)
	if err != nil {
		return nil, fmt.Errorf("finding the browser's websocket address: %w", err)
	}

	// The connection gets a lifetime of its own, so the tabs can be closed
	// over it after ctx ends and before it goes.
	connCtx, closeConn := context.WithCancel(context.WithoutCancel(ctx))

	a.wg.Go(func() {
		<-ctx.Done()

		// Best effort: the connection is closed next either way, and a remote
		// browser outlives it.
		_ = chromedp.Cancel(ctx)

		closeConn()
	})

	dialCtx, cancelDial := context.WithTimeout(connCtx, dialTimeout)
	defer cancelDial()

	conn, err := remote.DialContext(dialCtx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to the browser: %w", err)
	}

	return chromedp.NewBrowserTransport(connCtx, a.tap.wrap(conn), opts...)
}

// Wait blocks until the connection is released. It satisfies
// chromedp.Allocator.
func (a *attacher) Wait() { a.wg.Wait() }

// Attaches tells chromedp the browser runs already, so cancelling a context
// closes its tab and not the browser. It satisfies chromedp.Attacher.
func (a *attacher) Attaches() bool { return true }

// debuggerURL turns an endpoint into the browser's websocket address. A
// websocket address is used as it is; for anything else, such as
// http://127.0.0.1:9222, the browser is asked through /json/version. Chrome
// only answers a host given as an IP address or "localhost", so a name is
// resolved first.
func debuggerURL(ctx context.Context, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}

	if u.Host, err = hostByIP(ctx, u.Host); err != nil {
		return "", err
	}

	if strings.Contains(u.Path, "/devtools/browser/") {
		return u.String(), nil
	}

	u.Scheme = "http"
	u.Path = "/json/version"
	u.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }() // a read-only body; nothing is lost if closing fails

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", u.Redacted(), resp.Status)
	}

	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVersionBody)).Decode(&version); err != nil {
		return "", fmt.Errorf("reading %s: %w", u.Redacted(), err)
	}

	if version.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("%s named no websocket address", u.Redacted())
	}

	return version.WebSocketDebuggerURL, nil
}

// hostByIP replaces the host name in hostport with one of its IP addresses.
func hostByIP(ctx context.Context, hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", err
	}

	if host == "localhost" || net.ParseIP(host) != nil {
		return hostport, nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}

	if len(addrs) == 0 {
		return "", fmt.Errorf("%s has no address", host)
	}

	return net.JoinHostPort(addrs[0].IP.String(), port), nil
}
