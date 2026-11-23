package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

const (
	// textTimeout bounds reading the text (Tenet 1). It has its own budget,
	// like the cookie jar, because the run's may be spent by now.
	textTimeout = 15 * time.Second

	// maxTextLinks bounds the links recorded from one region.
	maxTextLinks = 5000

	// textWorldName names the isolated world the text is read in.
	textWorldName = "wsaw-text"
)

// textScript reads the region in an isolated world: the DOM is the page's,
// the JavaScript globals are not, so a page that redefines
// Element.prototype.innerText or querySelector cannot change what wsaw reads
// (Tenet 9). It returns the rendered text, cut to max UTF-16 units, the
// region's links and its in-page links with whether their target exists.
const textScript = `(function (sel, max, maxLinks) {
  var el;
  try { el = document.querySelector(sel); } catch (e) {
    return {error: 'invalid selector: ' + String((e && e.message) || e)};
  }
  if (!el) { return {missing: true}; }
  var text = el.innerText || '';
  var full = text.length > max ? Array.from(text).length : 0;
  if (text.length > max) { text = text.slice(0, max); }
  var links = [], frags = [], capped = false;
  var as = el.querySelectorAll('a[href]');
  for (var i = 0; i < as.length; i++) {
    if (links.length + frags.length >= maxLinks) { capped = true; break; }
    var a = as[i], raw = a.getAttribute('href') || '', r = raw.trim();
    if (r === '' || r === '#') { continue; }
    if (r.charAt(0) === '#') {
      var id = r.slice(1);
      try { id = decodeURIComponent(id); } catch (e) {}
      frags.push({href: r, found: !!(document.getElementById(id) || document.getElementsByName(id).length)});
      continue;
    }
    if (/^javascript:/i.test(r)) { continue; }
    links.push({text: (a.innerText || '').trim(), href: raw, target: a.href});
  }
  return {text: text, full: full, links: links, fragments: frags, capped: capped};
})`

// textRead is what textScript returns.
type textRead struct {
	Error     string               `json:"error"`
	Missing   bool                 `json:"missing"`
	Text      string               `json:"text"`
	Full      int                  `json:"full"`
	Links     []model.TextLink     `json:"links"`
	Fragments []model.TextFragment `json:"fragments"`
	Capped    bool                 `json:"capped"`
}

// collectText reads the configured region, once, after the page settled and
// consent handling finished (Story 4.15, AC3). A text that could not be read
// is recorded with its reason and never as an empty text (Tenet 5).
func (s *session) collectText(scanFailed bool) {
	region := s.opts.Text
	if region == nil {
		return
	}

	tc := &model.TextCapture{Selector: region.Selector}
	s.res.Text = tc

	if scanFailed {
		tc.Unavailable = "not read: the page did not load"

		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.runCtx), textTimeout)
	defer cancel()

	var got textRead

	err := chromedp.Do(ctx, chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		tree, err := cdp.Call(ctx, t, page.GetFrameTree, cdp.Empty{})
		if err != nil {
			return fmt.Errorf("reading the frame tree: %w", err)
		}

		world, err := cdp.Call(ctx, t, page.CreateIsolatedWorld, page.CreateIsolatedWorldParams{
			FrameID:   tree.FrameTree.Frame.ID,
			WorldName: textWorldName,
		})
		if err != nil {
			return fmt.Errorf("entering the page: %w", err)
		}

		sel, err := json.Marshal(region.Selector)
		if err != nil {
			return fmt.Errorf("encoding the selector: %w", err)
		}

		evaluated, err := cdp.Call(ctx, t, runtime.Evaluate, runtime.EvaluateParams{
			Expression:    fmt.Sprintf("%s(%s, %d, %d)", textScript, sel, region.MaxBytes, maxTextLinks),
			ContextID:     world.ExecutionContextID,
			ReturnByValue: new(true),
		})
		if err != nil {
			return err
		}

		if evaluated.ExceptionDetails != nil {
			return &chromedp.ExceptionError{ExceptionDetails: evaluated.ExceptionDetails}
		}

		if evaluated.Result == nil || len(evaluated.Result.Value) == 0 {
			return fmt.Errorf("the browser returned no value")
		}

		return json.Unmarshal(evaluated.Result.Value, &got)
	}))

	switch {
	case err != nil:
		tc.Unavailable = "the text could not be read: " + s.scrub(err.Error())
	case got.Error != "":
		tc.Unavailable = s.scrub(got.Error)
	case got.Missing:
		tc.Unavailable = "the selector matched nothing"
	default:
		s.fillText(tc, got, region.MaxBytes)
	}
}

// fillText records what was read. Configured secrets are scrubbed from the
// text and the links, as from every other string in a result (AGENTS §3):
// a page behind a login can echo the user name it was given.
func (s *session) fillText(tc *model.TextCapture, got textRead, maxBytes int) {
	text := got.Text
	cut := got.Full > 0

	if len(text) > maxBytes {
		text = truncateUTF8(text, maxBytes)
		cut = true
	}

	tc.Text = s.scrub(text)
	tc.Length = utf8.RuneCountInString(tc.Text)
	sum := sha256.Sum256([]byte(tc.Text))
	tc.SHA256 = hex.EncodeToString(sum[:])

	if cut {
		tc.TruncatedFrom = max(got.Full, utf8.RuneCountInString(got.Text))
		s.res.Warnings = append(s.res.Warnings, fmt.Sprintf(
			"the watched text was cut by wsaw at text.maxBytes (%d bytes); the region had %d characters",
			maxBytes, tc.TruncatedFrom))
	}

	for _, l := range got.Links {
		tc.Links = append(tc.Links, model.TextLink{Text: s.scrub(l.Text), Href: s.scrub(l.Href), Target: s.scrub(l.Target)})
	}

	tc.Fragments = got.Fragments

	if got.Capped {
		s.res.Warnings = append(s.res.Warnings, fmt.Sprintf(
			"the watched region's links were recorded up to the cap of %d", maxTextLinks))
	}
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
