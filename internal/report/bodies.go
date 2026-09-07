package report

import (
	"encoding/base64"
	"unicode/utf8"

	"github.com/pflege-de-labs/wsaw/internal/model"
)

// Stored response bodies in exports.
//
// A body that `storeBodies` captured lives outside the result document, as a
// content-addressed file, and the document carries only its reference
// (Story 4.6, AC9). That is the right shape for storage: a result is read
// whole on every listing, and a JavaScript bundle inlined into it would be
// paid for on every page of history.
//
// It is the wrong shape for an export. A HAR without response bodies cannot
// show them in DevTools, which is most of why one exports a HAR, and a
// downloaded result that names a body nobody can reach is not evidence of
// anything. So exports resolve the reference and carry the bytes, while the
// stored document stays lean.

// BodyLoader resolves a stored artifact reference to its bytes. It is a
// function so the exporters do not depend on the store (Tenet 12), and nil
// means "no bodies available", which the exports state rather than imply.
type BodyLoader func(ref string) ([]byte, error)

// WithBodies returns a copy of res whose requests carry their stored response
// bodies inline.
//
// A copy, because the caller's result is usually the stored document and
// mutating it would put megabytes into whatever else holds a reference to it
// — including, in the daemon, the copy the notifier is about to look at.
//
// Requests are copied shallowly on purpose: only the body fields are added,
// and everything else is shared with the original, which is immutable in
// practice.
func WithBodies(res *model.Result, load BodyLoader) *model.Result {
	if res == nil || load == nil {
		return res
	}

	var (
		out    *model.Result
		copied bool
	)

	for i := range res.Requests {
		req := &res.Requests[i]
		if req.BodyRef == "" {
			continue
		}

		body, err := load(req.BodyRef)
		if err != nil {
			// A body that cannot be read is reported in place rather than
			// omitted silently: "no body here" and "the body could not be
			// loaded" are different facts (Tenet 5).
			if !copied {
				out, copied = shallowCopy(res), true
			}

			out.Requests[i].BodyUnavailable = "stored body could not be read: " + err.Error()

			continue
		}

		if !copied {
			out, copied = shallowCopy(res), true
		}

		text, encoding := encodeBody(body)

		out.Requests[i].Body = text
		out.Requests[i].BodyEncoding = encoding
		out.Requests[i].BodyStoredSize = int64(len(body))
	}

	if !copied {
		return res
	}

	return out
}

// shallowCopy duplicates the result and its request slice, so inlining bodies
// cannot be seen by anything else holding the original.
func shallowCopy(res *model.Result) *model.Result {
	out := *res
	out.Requests = append([]model.Request(nil), res.Requests...)

	return &out
}

// encodeBody decides how a body travels in JSON and in HAR.
//
// Text is far more useful than base64 — it is what a person reads and what a
// diff of two exports would show — so it is preferred whenever the bytes
// really are text. A body containing a NUL byte or invalid UTF-8 is not, and
// forcing it into a JSON string would either fail to encode or silently
// replace the offending bytes with U+FFFD, which would corrupt the evidence.
func encodeBody(body []byte) (string, string) {
	if isText(body) {
		return string(body), ""
	}

	return base64.StdEncoding.EncodeToString(body), "base64"
}

func isText(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}

	for _, b := range body {
		// A NUL byte is legal UTF-8 and a reliable sign of a binary payload.
		// Other control bytes appear in minified text often enough that
		// rejecting them would push ordinary scripts into base64.
		if b == 0x00 {
			return false
		}
	}

	return true
}
