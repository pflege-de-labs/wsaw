package consent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// errFrameOutOfProcess reports a frame the browser renders in a process of
// its own. Site isolation puts a frame from another site there, and its
// document is reachable only through a separate debugging session.
var errFrameOutOfProcess = errors.New(
	"the frame is rendered in another process (typically because it is served from another site), which frame steps cannot reach")

// frameWorldName names the isolated world wsaw creates inside a frame, so it
// is recognizable in the browser's own tooling.
const frameWorldName = "wsaw-consent"

// runFrameStep runs a Click or WaitFor inside the frame named by step.Frame.
func (h *handler) runFrameStep(ctx context.Context, step Action) error {
	if step.WaitFor != "" {
		return h.waitForInFrame(ctx, step.Frame, step.WaitFor)
	}

	var out clickResult

	expr := fmt.Sprintf("window.__wsawClick(%s)", jsString(step.Click))

	if err := h.evalInFrame(ctx, step.Frame, expr, &out); err != nil {
		return fmt.Errorf("clicking %s in frame %s: %w", step.Click, step.Frame, err)
	}

	if !out.Clicked {
		return fmt.Errorf("clicking %s in frame %s: %s", step.Click, step.Frame, out.Reason)
	}

	return nil
}

// waitForInFrame polls until selector matches inside the frame. The frame
// itself may not exist yet, or be replaced while loading, so it is located
// again on every attempt; only a frame that cannot be reached at all ends
// the wait early.
func (h *handler) waitForInFrame(ctx context.Context, frameSel, selector string) error {
	const poll = 200 * time.Millisecond

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	expr := fmt.Sprintf("!!window.__wsawQuery(%s)", jsString(selector))

	for {
		var found bool

		err := h.evalInFrame(ctx, frameSel, expr, &found)
		if err == nil && found {
			return nil
		}

		if errors.Is(err, errFrameOutOfProcess) {
			return fmt.Errorf("waiting for %s in frame %s: %w", selector, frameSel, err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s in frame %s: %w", selector, frameSel, ctx.Err())
		case <-ticker.C:
		}
	}
}

// simulateClickInFrame is simulateClick for a frame step.
func (h *handler) simulateClickInFrame(ctx context.Context, frameSel, selector string) bool {
	stepCtx, cancel := context.WithTimeout(ctx, h.opts.StepTimeout)
	defer cancel()

	var out clickResult

	expr := fmt.Sprintf("window.__wsawSimulateClick(%s)", jsString(selector))

	if err := h.evalInFrame(stepCtx, frameSel, expr, &out); err != nil {
		return false
	}

	return out.Clicked
}

// evalInFrame evaluates expr inside the frame whose element matches frameSel
// in the top document, with the helpers injected there, and decodes the
// result into out. It runs in an isolated world: the frame's DOM is shared,
// so a click reaches the page's own listeners, but the page's scripts never
// see wsaw's helpers and wsaw never calls theirs.
func (h *handler) evalInFrame(ctx context.Context, frameSel, expr string, out any) error {
	h.ensureHelpers(ctx)

	return chromedp.Do(ctx, chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		frameID, err := h.locateFrame(ctx, t, frameSel)
		if err != nil {
			return err
		}

		world, err := cdp.Call(ctx, t, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{
			FrameID:   frameID,
			WorldName: frameWorldName,
		})
		if err != nil {
			return fmt.Errorf("entering frame: %w", err)
		}

		if err := evalInWorld(ctx, t, world.ExecutionContextID, helperScript, nil); err != nil {
			return fmt.Errorf("preparing frame: %w", err)
		}

		return evalInWorld(ctx, t, world.ExecutionContextID, expr, out)
	}))
}

// locateFrame resolves frameSel in the top document to the frame its element
// displays.
func (h *handler) locateFrame(ctx context.Context, s cdp.Session, frameSel string) (cdp.FrameID, error) {
	res, err := cdp.Call(ctx, s, runtime.Evaluate, runtime.EvaluateParams{
		Expression: fmt.Sprintf("window.__wsawQuery(%s)", jsString(frameSel)),
	})
	if err != nil {
		return "", fmt.Errorf("finding frame %s: %w", frameSel, err)
	}

	if res.ExceptionDetails != nil {
		return "", fmt.Errorf("finding frame %s: %w", frameSel, &chromedp.ExceptionError{ExceptionDetails: res.ExceptionDetails})
	}

	obj := res.Result
	if obj == nil || obj.ObjectID == "" {
		return "", fmt.Errorf("no frame matched %s", frameSel)
	}

	defer func() {
		_, err := cdp.Call(ctx, s, runtime.ReleaseObject, runtime.ReleaseObjectParams{ObjectID: obj.ObjectID})
		if err != nil {
			h.opts.Logger.Debug("releasing frame element failed", "error", err)
		}
	}()

	described, err := cdp.Call(ctx, s, dom.DescribeNode, dom.DescribeNodeParams{ObjectID: obj.ObjectID})
	if err != nil {
		return "", fmt.Errorf("describing frame %s: %w", frameSel, err)
	}

	node := described.Node
	if node == nil {
		return "", fmt.Errorf("describing frame %s: the browser returned no node", frameSel)
	}

	if name := strings.ToUpper(node.NodeName); name != "IFRAME" && name != "FRAME" {
		return "", fmt.Errorf("%s matches a <%s>, not a frame", frameSel, strings.ToLower(node.NodeName))
	}

	if node.ContentDocument == nil {
		return "", fmt.Errorf("frame %s: %w", frameSel, errFrameOutOfProcess)
	}

	if node.ContentDocument.FrameID != "" {
		return node.ContentDocument.FrameID, nil
	}

	return node.FrameID, nil
}

func evalInWorld(ctx context.Context, s cdp.Session, world runtime.ExecutionContextID, expr string, out any) error {
	evaluated, err := cdp.Call(ctx, s, runtime.Evaluate, runtime.EvaluateParams{
		Expression:    expr,
		ContextID:     world,
		ReturnByValue: new(true),
		AwaitPromise:  new(true),
	})
	if err != nil {
		return err
	}

	if evaluated.ExceptionDetails != nil {
		return &chromedp.ExceptionError{ExceptionDetails: evaluated.ExceptionDetails}
	}

	res := evaluated.Result
	if out == nil || res == nil || len(res.Value) == 0 {
		return nil
	}

	if err := json.Unmarshal(res.Value, out); err != nil {
		return fmt.Errorf("decoding frame result: %w", err)
	}

	return nil
}
