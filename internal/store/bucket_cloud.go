//go:build cloudblob

package store

// The cloud drivers are a build-time opt-in, and this file is the whole of
// that opt-in: importing it registers the "s3", "gs" and "azblob" schemes
// with gocloud's URL mux, which is all openBucket needs to reach them.
//
// They are separated from the default build because of what they weigh. The
// baseline binary is 30.6 MB; fileblob takes it to 37.0 MB; adding
// S3 alone takes it to 47.9 MB, and all three to 77.4 MB — two and a half
// times the tool, in SDKs, for a deployment that stores its evidence on a
// local disk. A single static binary is a promise wsaw makes (Tenet 14), and
// doubling it for a capability most installations do not use is not the way
// to keep it. So the default build stays lean and this build is a named,
// documented release artifact (Story 8.8, AC3).
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
