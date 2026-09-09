//go:build objectstore && !cloudblob

// Package objectstore needs the S3 driver, which is behind the cloudblob build
// tag. See minio_test.go for what the package is.
package objectstore

import "testing"

// TestTheCloudDriversAreLinkedIn fails a run that asked for this suite and did
// not ask for the driver it needs.
//
// Without this file the package would compile to nothing under `-tags
// objectstore` alone and the run would report "ok, no test files" — a green
// line for a suite that did not exist. A build tag that can silently remove a
// gate is worse than one that cannot be used by mistake.
func TestTheCloudDriversAreLinkedIn(t *testing.T) {
	t.Fatal("this suite needs the S3 driver, which is behind the cloudblob build tag: " +
		"run `make test-store-minio`, or `go test -tags cloudblob,objectstore ./test/e2e/objectstore/`")
}
