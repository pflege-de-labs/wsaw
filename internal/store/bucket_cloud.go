//go:build cloudblob

package store

// The cloud drivers are a build-time opt-in, and this file is the whole of
// that opt-in: importing it registers the "s3", "gs" and "azblob" schemes
// with gocloud's URL mux, which is all openBucket needs to reach them.
//
// They are separated from the default build because of what they weigh. The
// released darwin/arm64 binary is 25.9 MB by default and 53.9 MB with these
// three imports — more, in SDKs, than the whole of the rest of wsaw, for a
// deployment that stores its evidence on a local disk. A single static binary
// is a promise wsaw makes (Tenet 14), and doubling it for a capability most
// installations do not use is not the way to keep it. So the default build
// stays lean and this build is a named, documented release artifact
// (Story 8.8, AC3). All four platforms are tabulated in
// docs/dependency-review-gocloud.md, and dist/SIZES carries the figures for a
// given release.
//
// The imports are blank because nothing in wsaw touches a provider type: the
// bucket is the only seam, and it speaks gocloud's interface (Story 8.1, AC1).
// Registration is also the only thing that happens here — no credential chain
// is resolved and no network call is made until openBucket is given a URL
// with one of these schemes.
//
//	go build -tags cloudblob ./cmd/wsaw
import (
	_ "gocloud.dev/blob/azureblob" // the "azblob" scheme
	_ "gocloud.dev/blob/gcsblob"   // the "gs" scheme
	_ "gocloud.dev/blob/s3blob"    // the "s3" scheme
)
