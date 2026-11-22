package model

import "time"

// TextCapture is what a scan read from the region a target's text.selector
// names (Story 4.15). The text and its links are stored verbatim as one
// content-addressed artifact (TextArtifact) named by Ref, and the result
// carries the reference, the digest and the length (AC4). Everything derived
// from the text — the normalized form, the diff, the completeness checks — is
// computed when results are compared and never stored in its place (Tenet 3,
// Tenet 4).
type TextCapture struct {
	// Selector is the CSS selector the text was read from.
	Selector string `json:"selector"`

	// Unavailable says why no text was read: the selector matched nothing,
	// the selector was invalid, or the read failed. Empty when Text holds
	// what the page showed (Tenet 5: no text is never an empty text).
	Unavailable string `json:"unavailable,omitempty"`

	// Ref names the artifact holding the text and its links, once stored.
	Ref string `json:"ref,omitempty"`

	// Bytes is the size of that artifact, for the storage accounting every
	// other artifact gets (Story 5.31).
	Bytes int64 `json:"bytes,omitempty"`

	// Text is the region's innerText as the browser rendered it. It lives in
	// the artifact, not the result document: the capture fills it, and the
	// comparison loads it back from Ref.
	Text string `json:"-"`

	// Length is the number of characters in Text.
	Length int `json:"length"`

	// SHA256 is the hex digest of Text, so two texts can be told apart
	// without reading either.
	SHA256 string `json:"sha256,omitempty"`

	// TruncatedFrom is the length in characters of the region's text when
	// wsaw cut it to text.maxBytes, and zero when it did not. A text wsaw
	// cut is never mistaken for one the site cut.
	TruncatedFrom int `json:"truncatedFrom,omitempty"`

	// Links are the region's links with targets, in document order (Story
	// 4.15, AC14), kept in the artifact with Text. Fragment-only and
	// javascript: links are in Fragments or left out.
	Links []TextLink `json:"-"`

	// Fragments are the region's in-page links (href="#x") and whether the
	// document had an element with that id or name when the text was read.
	Fragments []TextFragment `json:"fragments,omitempty"`

	// Check is what a model made of the text against the target's prompt
	// (Story 4.16), or nil when the target has no check. It is model output,
	// derived from the text, and never part of what makes two scans equal.
	Check *TextCheck `json:"check,omitempty"`
}

// The verdicts a text check can reach (Story 4.16, AC4).
const (
	VerdictPassed    = "passed"
	VerdictFailed    = "failed"
	VerdictUncertain = "uncertain"
)

// TextCheck is one model's reading of a watched text against a written
// prompt (Story 4.16, AC5). Either Verdict is set, or NotChecked says why
// there is none: no answer is never a pass (Tenet 5).
type TextCheck struct {
	// Model is the model ID the request named.
	Model string `json:"model"`
	// Server is the host of the Messages API that answered, so token counts
	// from a compatible server are not mistaken for Anthropic's (AC10).
	Server string `json:"server"`
	// PromptSHA256 and TextSHA256 identify what was read; together with
	// Model they decide whether an earlier check can be reused (AC3).
	PromptSHA256 string `json:"promptSha256"`
	TextSHA256   string `json:"textSha256"`
	// CheckedAt is when the model answered, or when the check was given up.
	CheckedAt time.Time `json:"checkedAt"`
	// Reused says the findings were made for an earlier scan of the same
	// text, prompt and model, at CheckedAt, and nothing was sent this time.
	Reused bool `json:"reused,omitempty"`

	// NotChecked says why there is no verdict: no key, a timeout, a refusal,
	// the daily cap, a text over the size limit, or a reply that did not
	// parse.
	NotChecked string `json:"notChecked,omitempty"`
	// Verdict is passed, failed or uncertain.
	Verdict string `json:"verdict,omitempty"`
	// Findings are the model's, each marked as deciding the verdict or
	// informative. Page-derived model output: render it escaped.
	Findings []TextFinding `json:"findings,omitempty"`
	// Reply is the model's raw answer, kept when it did not parse (AC4).
	Reply string `json:"reply,omitempty"`

	// InputTokens and OutputTokens are the server's own counts.
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
}

// Deciding counts the findings that decide the verdict.
func (c *TextCheck) Deciding() int {
	n := 0

	for _, f := range c.Findings {
		if f.Deciding {
			n++
		}
	}

	return n
}

// TextFinding is one finding of a text check.
type TextFinding struct {
	Text     string `json:"text"`
	Deciding bool   `json:"deciding"`
}

// TextArtifactKind is the artifact kind a TextArtifact is stored under.
const TextArtifactKind = "text"

// TextArtifact is the stored form of a watched text: what Ref names.
type TextArtifact struct {
	Text  string     `json:"text"`
	Links []TextLink `json:"links,omitempty"`
}

// TextLink is one link inside a watched region.
type TextLink struct {
	// Text is the link's rendered text.
	Text string `json:"text"`
	// Href is the href attribute as written in the page.
	Href string `json:"href"`
	// Target is Href resolved against the document's base URL.
	Target string `json:"target"`
}

// TextFragment is one in-page link inside a watched region.
type TextFragment struct {
	// Href is the attribute as written, starting with "#".
	Href string `json:"href"`
	// Found says an element with that id or name existed in the document.
	Found bool `json:"found"`
}
