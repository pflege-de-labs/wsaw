package httpapi

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/store"
)

// Serving evidence, wherever it is kept (Story 8.7).
//
// Since Story 8.2 an artifact is an object in a bucket, which may be a
// directory on local disk or somebody's object storage. This file is where
// that stops mattering to a reader: the same route, the same headers, and the
// same account of evidence that has been pruned, whether the bytes come off a
// disk or over a network.
//
// Three things follow from the bucket that did not follow from the directory,
// and all three live here. The object is streamed rather than read into
// memory, because a result document runs to tens of megabytes (AC1). The
// response carries a size and a validator, so a screenshot on a result page
// is fetched once rather than on every reload (AC4) — content addressing
// makes that validator exactly correct. And where the provider can sign a URL
// and the operator has asked for it, wsaw may hand the reader a link to the
// bucket instead of copying the bytes through itself (AC3).

// handleArtifact serves a stored body or screenshot.
//
// Without this route a stored artifact was unreachable: capture wrote it, the
// result named it, and nothing could read it back. The reference is validated
// by the store, which confines it to the artifact bucket (Tenet 9).
//
// How it comes back depends on what it is, and the distinction is the whole
// of Story 5.17's AC5. A response body *is* the scanned page's bytes, so it
// is served as an opaque attachment and never as anything a browser will
// render — rendering it on wsaw's own origin would hand a hostile page a
// same-origin script context. A screenshot is a PNG that Chrome produced
// under wsaw's control: the page influenced its pixels, not its bytes, so it
// may be shown as an image. As an image and nothing else.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if ref == "" {
		writeJSONError(w, http.StatusBadRequest, "an artifact reference is required")

		return
	}

	s.serveArtifact(w, r, ref)
}

// serveArtifact writes one stored artifact with the headers its kind
// deserves. It is shared by the authenticated route and by the share-link
// route, so a shared reader cannot be served bytes under weaker headers than
// an operator would get.
func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, ref string) {
	// The zero time: this request was authenticated by something that does
	// not expire on its own, which is the API token.
	s.serveArtifactUntil(w, r, ref, time.Time{})
}

// serveArtifactUntil serves an artifact to a reader whose access ends at a
// known moment — a share link's expiry — which is the ceiling on any signed
// URL issued to them (AC3).
func (s *Server) serveArtifactUntil(w http.ResponseWriter, r *http.Request, ref string, accessUntil time.Time) {
	// A conditional answer and a redirect both need to know what the object
	// is, and neither needs a byte of it, so one attributes call covers both.
	// A request that wants neither goes straight at the object and takes its
	// attributes from the read it was making anyway: against a bucket that
	// charges per request, a HEAD before every GET is a bill.
	if wantsConditional(r) || s.redirectsArtifacts() {
		info, err := s.deps.Store.StatArtifact(r.Context(), ref)
		if err != nil {
			// Evidence that has been pruned answers as pruned rather than as a
			// fault (AC5, Tenet 5); the store distinguishes the two and
			// writeStoreError renders that distinction.
			writeStoreError(w, err)

			return
		}

		if s.answerNotModified(w, r, info) {
			return
		}

		// Guarded again at the call, because the branch above is also entered
		// by a conditional request against a wsaw that redirects nothing.
		if s.redirectsArtifacts() && s.redirectToBucket(w, r, ref, accessUntil) {
			return
		}
	}

	s.streamArtifact(w, r, ref)
}

// streamArtifact copies the object to the client.
//
// It is streamed rather than buffered. Evidence lives in a bucket and a result
// document runs to tens of megabytes, so reading it whole before the first
// byte reaches the client would make the daemon's memory track the size of the
// largest artifact times the number of readers (Story 8.1, AC7; AC1).
//
// The reader is closed on every path — a header that could not be written, a
// copy that failed half way, a client that hung up — because it holds a bucket
// connection and the deadline that bounds it (AC6).
func (s *Server) streamArtifact(w http.ResponseWriter, r *http.Request, ref string) {
	body, err := s.deps.Store.OpenArtifact(r.Context(), ref)
	if err != nil {
		writeStoreError(w, err)

		return
	}

	defer func() {
		if err := body.Close(); err != nil {
			s.deps.Logger.Error("closing an artifact reader", "error", err)
		}
	}()

	// Only the magic bytes are pulled ahead of the copy. A screenshot is served
	// as an image and a body never is, and that decision has to be made before
	// the headers go out — but it needs eight bytes, not the whole object.
	head := bufio.NewReaderSize(body, len(pngMagic))

	magic, err := head.Peek(len(pngMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		writeStoreError(w, err)

		return
	}

	// Two independent conditions, deliberately. The kind comes from wsaw's own
	// code rather than from a page, and the magic bytes are the file itself:
	// requiring both means a response body cannot be served as an image even
	// if some future caller passed a reference that claimed to be one.
	inline := !wantsDownload(r) && isScreenshotRef(ref) && isPNG(magic)

	// Written from what the bucket reported rather than from what is copied,
	// so the interface can show a size and a browser can cache instead of
	// re-fetching megabytes (Story 5.17, AC4).
	if body.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(body.Size, 10))
	}

	setArtifactValidators(w, body.ArtifactInfo)

	w.Header().Set("X-Content-Type-Options", "nosniff")

	if inline {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `inline; filename="`+safeArtifactFilename(ref, "png")+`"`)
		// An image and nothing else: no script, no styles, no subresources,
		// whatever a browser might otherwise try to do with these bytes.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; sandbox")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+safeArtifactFilename(ref, "bin")+`"`)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	}

	// #nosec G705 -- a body's bytes are page-controlled, which is why the
	// headers above make them inert: an opaque type, an attachment
	// disposition, nosniff, and a sandbox policy. A screenshot is wsaw's own
	// PNG and is served as an image with an equally strict policy. The
	// analyser sees the taint and not the mitigation; the tests in
	// bodies_test.go and screenshots_test.go see the mitigation.
	if _, err := io.Copy(w, head); err != nil {
		// The status and the headers are already gone, so there is nothing to
		// report to the client; the log line is what tells an operator that a
		// download died half way rather than completing.
		s.deps.Logger.Error("writing artifact", "error", err)
	}
}

// artifactCacheControl is what a browser may do with evidence it has been
// given.
//
// "no-cache" is not "do not store": it lets the browser keep the image and
// requires it to ask before showing it again, which is what turns a reload of
// a result page into a handful of 304s instead of a handful of megabytes
// (AC4). "private" keeps it out of any cache shared between people, because a
// screenshot of a page can carry personal data (Tenet 19) and because
// revalidating with wsaw is what keeps wsaw in charge of who may see it.
const artifactCacheControl = "private, no-cache"

// setArtifactValidators writes what a client needs to cache this artifact and
// to ask about it next time.
func setArtifactValidators(w http.ResponseWriter, info store.ArtifactInfo) {
	if etag := artifactETag(info); etag != "" {
		w.Header().Set("ETag", etag)
	}

	if !info.ModTime.IsZero() {
		w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}

	w.Header().Set("Cache-Control", artifactCacheControl)
}

// artifactETag builds a strong entity tag for a stored artifact.
//
// Strong, and trivially so: an artifact's key *is* the SHA-256 of its bytes,
// so an object under that key cannot come to hold anything else — content
// addressing makes rewriting it either a no-op or a different key. The one
// thing a validator can get wrong, claiming a stale copy is current, is
// therefore impossible here, which is why this is a strong tag rather than the
// weak one a size-and-date guess would deserve (AC4).
func artifactETag(info store.ArtifactInfo) string {
	if info.Digest == "" {
		return ""
	}

	return `"sha256:` + info.Digest + `"`
}

// wantsConditional reports whether the client offered a copy for wsaw to
// validate rather than asking for the bytes outright.
func wantsConditional(r *http.Request) bool {
	return r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != ""
}

// answerNotModified tells a client that the copy it already holds is the
// artifact, and reports whether it did.
//
// The response carries the validators and nothing else: a 304 has no body, and
// the headers that describe one — the content type, the disposition, the
// policy that renders it inert — describe bytes that are not being sent.
func (s *Server) answerNotModified(w http.ResponseWriter, r *http.Request, info store.ArtifactInfo) bool {
	if !artifactUnchanged(r, info) {
		return false
	}

	setArtifactValidators(w, info)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotModified)

	return true
}

// artifactUnchanged evaluates the request's preconditions against the stored
// object.
func artifactUnchanged(r *http.Request, info store.ArtifactInfo) bool {
	// An entity tag wins where both are offered, as RFC 9110 requires: the tag
	// is exact, while a date is a comparison at one-second resolution against
	// a clock that was never wsaw's.
	if match := r.Header.Get("If-None-Match"); match != "" {
		return etagMatches(match, artifactETag(info))
	}

	if since := r.Header.Get("If-Modified-Since"); since != "" {
		at, err := http.ParseTime(since)
		if err != nil {
			// An unparseable date is ignored rather than refused, which is what
			// a cache validator failing should cost: one full response.
			return false
		}

		// Truncated to the second because that is all an HTTP date carries; an
		// object written 300ms after the date the client holds would otherwise
		// look modified for ever.
		return !info.ModTime.Truncate(time.Second).After(at)
	}

	return false
}

// etagMatches reports whether one of the tags the client offered is this
// artifact's.
//
// The comparison is the weak one, as RFC 9110 specifies for If-None-Match: a
// client that stored the tag as weak — a proxy may have made it so — is
// asking about the same bytes, and content addressing means it really is the
// same bytes.
func etagMatches(header, etag string) bool {
	if etag == "" {
		return false
	}

	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)

		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}

	return false
}

// redirectsArtifacts reports whether this wsaw hands readers a bucket URL
// instead of the bytes.
//
// Three conditions, all of them off by default: the operator asked for it, a
// lifetime is configured, and the bucket has not already been found unable to
// sign. The last one is what makes the fallback cost one attempt per process
// rather than one per request (AC3).
func (s *Server) redirectsArtifacts() bool {
	return s.opts.SignedArtifactURLs &&
		s.opts.SignedArtifactURLTTL > 0 &&
		!s.noSigning.Load()
}

// redirectToBucket answers with a signed URL rather than with the object, and
// reports whether it did.
//
// Every refusal falls back to serving the bytes. A redirect is an
// optimisation the operator opted into; a bucket that will not sign, for
// whatever reason, is not a reason to fail a request wsaw can answer itself.
//
// What a redirect gives up, besides access control, is wsaw's own response
// headers — and the property they exist for survives anyway, because it was
// established when the object was written. Every artifact is stored as
// application/octet-stream (Story 8.1), so a provider hands it over as an
// opaque download rather than as something a browser will render, and it does
// so from the bucket's origin rather than from wsaw's, where a hostile page's
// bytes would have a same-origin context to play with.
func (s *Server) redirectToBucket(w http.ResponseWriter, r *http.Request, ref string, accessUntil time.Time) bool {
	ttl := s.signedURLLifetime(accessUntil)
	if ttl <= 0 {
		// The credential that authorised this request is on its last seconds.
		// Signing for less than a second is not worth a round trip, and
		// signing for longer is precisely what AC3 forbids.
		return false
	}

	signed, err := s.deps.Store.SignArtifactURL(r.Context(), ref, ttl)
	if err != nil {
		s.reportUnsignedBucket(err)

		return false
	}

	// Not cached, at any hop: the Location stops working when the signature
	// expires, and a cached redirect would send a reader to a URL the bucket
	// has begun refusing. The artifact's own validators are deliberately
	// absent — this response is not the artifact.
	w.Header().Set("Cache-Control", "no-store")

	// Found rather than a permanent redirect, for the same reason: where this
	// artifact can be fetched from is true for the next few minutes only.
	//
	// #nosec G710 -- the destination is not caller-controlled. The provider
	// builds it from the endpoint wsaw was configured with, and the only part
	// of the request that reaches it is the object key, which the store has
	// already reduced to exactly "<kind>/<sha256 hex>" — a whitelist rather
	// than an escape test (Tenet 9). The analyser sees a path parameter
	// arriving at a redirect and cannot see the validation a package away;
	// bodies_test.go's traversal cases see it.
	http.Redirect(w, r, signed, http.StatusFound)

	return true
}

// signedURLLifetime is how long a redirect issued now may be honoured.
//
// The configured lifetime, cut to what is left of the reader's own access. A
// share link that expires in thirty seconds must not be convertible into ten
// minutes of access to the evidence it names, because the redirect cannot be
// withdrawn once it has been handed over (AC3).
func (s *Server) signedURLLifetime(accessUntil time.Time) time.Duration {
	ttl := s.opts.SignedArtifactURLTTL

	if accessUntil.IsZero() {
		return ttl
	}

	if remaining := time.Until(accessUntil); remaining < ttl {
		return remaining
	}

	return ttl
}

// reportUnsignedBucket records, once, that redirects are not happening.
//
// Once, because the answer does not change while the process runs: a provider
// either signs or does not. Logging it per request would put a line in the
// operator's log for every screenshot on every page view, which is how a
// warning worth reading becomes noise (AC3).
func (s *Server) reportUnsignedBucket(err error) {
	unsupported := errors.Is(err, store.ErrSigningUnsupported)

	if unsupported {
		// Remembered, so nothing tries again: this is a property of the
		// provider, not a passing failure.
		if !s.noSigning.CompareAndSwap(false, true) {
			return
		}

		s.deps.Logger.Warn(
			"the artifact bucket cannot sign URLs, so artifacts are served through wsaw instead; "+
				"remove store.artifactSignedURLs, or point it at a provider that signs",
			"error", err,
		)

		return
	}

	// Anything else is a bucket that answered badly rather than a provider
	// without the feature, and it is not remembered: the next request may well
	// succeed. It is a warning rather than an error because the reader still
	// gets their evidence, from wsaw.
	if s.signingWarned.CompareAndSwap(false, true) {
		s.deps.Logger.Warn("an artifact URL could not be signed, so the artifact was served through wsaw",
			"error", err)
	}
}

// wantsDownload reports whether the caller asked for the file rather than a
// rendering of it, so a screenshot can still be saved as evidence.
func wantsDownload(r *http.Request) bool {
	switch r.URL.Query().Get("download") {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// isScreenshotRef reports whether a reference names a screenshot. The kind is
// the first path segment and is written by wsaw, never by a scanned page.
func isScreenshotRef(ref string) bool {
	kind, _, ok := strings.Cut(ref, "/")

	return ok && strings.HasPrefix(kind, "screenshot")
}

// pngMagic is the signature every PNG starts with, and the only part of an
// artifact streamArtifact has to look at before it chooses headers.
const pngMagic = "\x89PNG\r\n\x1a\n"

// isPNG checks the file's own magic bytes, so what is served as an image is
// an image regardless of what its reference claimed.
func isPNG(data []byte) bool {
	return bytes.HasPrefix(data, []byte(pngMagic))
}

// safeArtifactFilename builds a download name from a reference. References
// are content-addressed — a kind and a hex digest — but the value still
// arrives from the request, so it is reduced to characters that cannot break
// the header.
func safeArtifactFilename(ref, ext string) string {
	out := make([]rune, 0, len(ref))

	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}

	return "wsaw-" + string(out) + "." + ext
}
