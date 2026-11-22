package model

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
