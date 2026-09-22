package capture

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// parseDataURIHeader splits a data: URI into its declared media type and
// payload, without decoding the payload. ok is false for anything that is
// not a well-formed data: URI — malformed input is left exactly as it was
// captured rather than guessed at (Tenet 5).
func parseDataURIHeader(raw string) (mime, payload string, isBase64, ok bool) {
	const prefix = "data:"

	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return "", "", false, false
	}

	rest := raw[len(prefix):]

	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false, false
	}

	meta := rest[:comma]
	payload = rest[comma+1:]

	if meta != "" {
		parts := strings.Split(meta, ";")

		if strings.EqualFold(parts[len(parts)-1], "base64") {
			isBase64 = true
			parts = parts[:len(parts)-1]
		}

		if len(parts) > 0 && parts[0] != "" {
			mime = strings.Join(parts, ";")
		}
	}

	return mime, payload, isBase64, true
}

// dataURIUpperBound estimates a data: URI payload's decoded size from its
// encoded text alone, without decoding it: exact for base64 (accounting for
// padding), a safe upper bound for percent-encoded text. It lets a payload
// over the body cap be recognised as such before the cost of decoding it is
// paid.
func dataURIUpperBound(payload string, isBase64 bool) int {
	if !isBase64 {
		return len(payload)
	}

	n := len(payload)

	pad := 0
	for i := n - 1; i >= 0 && i >= n-2 && payload[i] == '='; i-- {
		pad++
	}

	size := (n/4)*3 - pad
	if size < 0 {
		return 0
	}

	return size
}

// decodeDataURIPayload decodes a data: URI's payload. Base64 is tried with
// standard padding first and falls back to the unpadded variant some pages
// emit; a non-base64 payload is percent-decoded text.
func decodeDataURIPayload(payload string, isBase64 bool) ([]byte, error) {
	if !isBase64 {
		decoded, err := url.QueryUnescape(payload)
		if err != nil {
			return nil, err
		}

		return []byte(decoded), nil
	}

	if data, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return data, nil
	}

	return base64.RawStdEncoding.DecodeString(payload)
}

// truncatedDataURI is the bounded stand-in stored for a data: URI's URL once
// its payload has been moved to a body artifact (Story 1.9). It keeps the
// scheme and declared media type — the part worth comparing — and replaces
// the payload with its size, so that it was truncated is never silent
// (Tenet 5). decoded is false when the payload was too large to be decoded
// at all, in which case size is an upper bound rather than an exact count.
func truncatedDataURI(mime string, isBase64 bool, size int, decoded bool) string {
	enc := ""
	if isBase64 {
		enc = ";base64"
	}

	note := fmt.Sprintf("%d bytes", size)
	if !decoded {
		note = fmt.Sprintf("over %d bytes, not decoded", size)
	}

	return fmt.Sprintf("data:%s%s,<truncated: %s>", mime, enc, note)
}
