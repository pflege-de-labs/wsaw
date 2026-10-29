package browser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Printing defaults (Story 5.39, AC6).
const (
	// DefaultPrintTimeout bounds one print, waiting for a browser included.
	DefaultPrintTimeout = 30 * time.Second

	// maxConcurrentPrints keeps reports from crowding scans out of the pool.
	maxConcurrentPrints = 2
)

// ErrPrinterBusy is returned when no print slot or browser came free before
// the print's deadline. A caller answers it with "try again later".
var ErrPrinterBusy = errors.New("no browser was free to print the report")

// Printer prints HTML documents to PDF in browsers borrowed from a pool.
type Printer struct {
	pool    *Pool
	timeout time.Duration
	slots   chan struct{}
}

// NewPrinter returns a printer that borrows browsers from pool. A zero
// timeout means DefaultPrintTimeout.
func NewPrinter(pool *Pool, timeout time.Duration) *Printer {
	if timeout <= 0 {
		timeout = DefaultPrintTimeout
	}

	return &Printer{pool: pool, timeout: timeout, slots: make(chan struct{}, maxConcurrentPrints)}
}

// PrintPDF prints html, a self-contained document, as an A4 PDF with footer
// as every page's footer.
//
// The document is untrusted: it carries text the scanned page chose. It is
// printed in a browser context of its own, with JavaScript disabled and
// every request the document makes failed before it leaves the browser, so
// nothing in it can make wsaw contact anyone. The context is disposed of on
// every path.
func (p *Printer) PrintPDF(ctx context.Context, html []byte, footer string) (pdf []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrPrinterBusy, ctx.Err())
	}

	lease, err := p.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", ErrPrinterBusy, err)
		}

		return nil, err
	}

	// A browser that failed a print, or panicked during one, is replaced
	// rather than handed to the next scan.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("printing the report panicked: %v", r)
		}

		if err != nil {
			lease.Discard("pdf print failed")
		} else {
			lease.Release()
		}
	}()

	tab, closeTab, err := lease.Browser.NewScanContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening a tab to print in: %w", err)
	}
	defer closeTab()

	return printIn(tab, html, footer)
}

func printIn(tab context.Context, html []byte, footer string) ([]byte, error) {
	if err := Listen(tab, func(ev any) {
		if e, ok := ev.(*fetch.EventRequestPaused); ok {
			// Ignored: the request stays paused and never leaves the browser
			// either way, and the tab is disposed of when the print ends.
			_, _ = chromedp.Call(tab, fetch.FailRequest, fetch.FailRequestParams{
				RequestID: e.RequestID, ErrorReason: network.ErrorReasonBlockedByClient,
			})
		}
	}); err != nil {
		return nil, fmt.Errorf("listening to the print tab: %w", err)
	}

	if _, err := chromedp.Call(tab, emulation.SetScriptExecutionDisabled,
		emulation.SetScriptExecutionDisabledParams{Value: true}); err != nil {
		return nil, fmt.Errorf("disabling JavaScript: %w", err)
	}

	if _, err := chromedp.Call(tab, fetch.Enable,
		fetch.EnableParams{Patterns: []*fetch.RequestPattern{{URLPattern: "*"}}}); err != nil {
		return nil, fmt.Errorf("blocking requests: %w", err)
	}

	tree, err := chromedp.Call(tab, page.GetFrameTree, cdp.Empty{})
	if err != nil || tree.FrameTree == nil || tree.FrameTree.Frame == nil {
		return nil, fmt.Errorf("finding the print tab's frame: %w", errors.Join(err, errors.New("no frame tree")))
	}

	// Set as the about:blank tab's content rather than navigated to, so the
	// document itself is not a request either.
	if _, err := chromedp.Call(tab, page.SetDocumentContent, page.SetDocumentContentParams{
		FrameID: tree.FrameTree.Frame.ID, HTML: string(html),
	}); err != nil {
		return nil, fmt.Errorf("loading the report: %w", err)
	}

	res, err := chromedp.Call(tab, page.PrintToPDF, page.PrintToPDFParams{
		DisplayHeaderFooter: new(true),
		PrintBackground:     new(true),
		PreferCSSPageSize:   new(true),
		PaperWidth:          8.27, // A4, for a printer that ignores @page
		PaperHeight:         11.69,
		MarginBottom:        new(0.7),
		HeaderTemplate:      "<span></span>",
		FooterTemplate:      footer,
	})
	if err != nil {
		return nil, fmt.Errorf("printing: %w", err)
	}

	return res.Data, nil
}
