//go:build objectstore && cloudblob

// Package objectstore runs wsaw against a real S3-compatible object store.
//
// Everything else in Epic 8 is tested against a directory on local disk, a
// bucket that only exists in memory, and a fake that misbehaves on a schedule.
// Each of those is a model of object storage written by the same people who
// wrote the code under test, so what none of them can find is the thing this
// package exists for: a place where MinIO — and behind it S3, and behind that
// every provider gocloud.dev/blob reaches — does not behave the way the model
// assumed. Listing order, pagination, modification times, error codes and
// delete semantics are all in that category (Story 8.9, AC2).
//
// It is behind a build tag because it needs a container runtime and pulls an
// image, which no `go test ./...` should do (Story 7.6, AC1):
//
//	make test-store-minio
//	go test -tags cloudblob,objectstore ./test/e2e/objectstore/
//
// The cloudblob tag is required rather than the S3 driver being imported here,
// so that what is under test is the driver registration a released binary has
// (Story 8.8, AC3) rather than one this test arranged for itself.
//
// # Why the gocloud types are in here and nowhere else outside the store
//
// Story 8.1, AC1 keeps every provider type inside internal/store. This package
// is the deliberate exception and it is the one place the exception is not a
// leak: its subject *is* the provider. A test asserting that MinIO's listing
// pages the way the index assumes, or that a delete of an absent key reports
// NotFound, has to speak to the bucket rather than through the store — asking
// the store would be asking the code under test whether it is right.
//
// # Why MinIO is a package of its own rather than a service in a stack
//
// AC2 says MinIO "joins the containerised end-to-end stack (Epic 7)". That
// stack does not exist: Stories 7.2 and 7.3 — the Compose file and the Podman
// pod — are still headed "(not implemented)", and test/e2e holds a fixture
// server and this package and nothing to join. So the substance of AC2 is here
// instead: a full scan → store → prune → read-back cycle against a real
// S3-compatible service, for both store kinds, skipping with a reason where no
// container runtime is present, gated on every CI push by `make
// test-store-minio`. What is outstanding is the stack membership alone, and it
// is outstanding on Epic 7 rather than on this epic. When 7.2 and 7.3 land,
// MinIO becomes a service in the Compose file and the pod manifest and this
// package stays as the standalone path — it starts its own container, which is
// what makes `go test -tags cloudblob,objectstore ./test/e2e/objectstore/`
// work on a laptop with no stack running.
package objectstore

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/container"
)

const (
	// minioImage is pinned by digest, and by the digest of the *manifest list*
	// rather than of one architecture's image, so the same line resolves on the
	// arm64 laptop this was written on and the amd64 runner CI uses.
	//
	// Pinned for the reason every image in this repository is pinned: an
	// upstream release must not be able to change what a test means, and a
	// provider-behaviour suite is precisely where a silent upgrade would be
	// mistaken for a regression in wsaw (Story 7.6, AC4).
	//
	// RELEASE.2025-09-07T16-13-09Z.
	minioImage = "quay.io/minio/minio@sha256:" +
		"14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"

	// minioContainer is what the container is called, so a leftover from an
	// interrupted run is found and removed rather than colliding.
	minioContainer = "wsaw-test-minio"

	// minioPort is deliberately not 9000: a developer running MinIO for their
	// own reasons must not have it emptied by a test run.
	minioPort = "59000"

	// bucketName is the bucket inside MinIO. It is created before the server
	// starts, by making the directory the single-drive backend keeps its
	// buckets in — which needs no client, no mc, and no bind mount from a host
	// whose temporary directory a container runtime on macOS cannot see.
	bucketName = "wsaw"

	// The credentials. Obvious fixtures, in the repository on purpose so that
	// nobody mistakes them for something to protect (Story 7.2, AC7).
	accessKey = "wsaw-test-access-key"
	secretKey = "wsaw-test-secret-key"
)

// envBucket is how `make test-store-minio` hands this package an object store
// it has already started, and is the same variable the store's own suite reads
// to decide where its evidence goes.
//
// One server for every run in that target rather than one per package: the
// store suite runs against it twice, once per kind of index, and starting a
// third container here would be a third image pull and a third wait for no
// gain.
const envBucket = "WSAW_TEST_ARTIFACT_BUCKET"

// bucketURL is where the tests below point a store, set by TestMain.
//
// The credentials are not in it: they reach the AWS SDK through the standard
// environment variables, which is how an operator supplies them too
// (Story 8.6, AC5), and a URL that carried them would put them in every log
// line and error message that names the bucket.
var bucketURL string

// skipReason is why there is no MinIO to test against, or empty.
//
// It is a value rather than a call to t.Skip inside TestMain because TestMain
// has no *testing.T: a skip decided there has to be carried to each test, and
// a test that skipped without saying why would be indistinguishable from one
// that quietly stopped covering anything.
var skipReason string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stop, err := startMinIO(ctx)
	if err != nil {
		skipReason = err.Error()
	}

	code := m.Run()

	if stop != nil {
		stop()
	}

	os.Exit(code)
}

// needMinIO skips a test where there is no object store to run it against, with
// the reason in full.
func needMinIO(t *testing.T) {
	t.Helper()

	if skipReason != "" {
		t.Skip("no S3-compatible object store to test against: " + skipReason)
	}
}

// startMinIO brings up the object store and returns how to take it down, or
// adopts one that is already running.
//
// The runtime is detected the way wsaw detects one for the browser — podman
// first, then docker, and neither being usable is an absence rather than an
// error (Story 1.8) — so a developer with no container runtime gets a skip with
// a reason, and CI, which has one, gets the test.
func startMinIO(ctx context.Context) (func(), error) {
	// Already provided, by the Makefile target that runs the store suite
	// against the same server. Nothing to start and nothing to take down; the
	// credentials are in the environment for the same reason the URL is.
	if configured := os.Getenv(envBucket); strings.HasPrefix(configured, "s3://") {
		bucketURL = configured

		return nil, nil
	}

	rt, err := container.Detect(ctx, container.KindAuto)
	if err != nil {
		return nil, fmt.Errorf("detecting a container runtime: %w", err)
	}

	if rt == nil {
		return nil, fmt.Errorf("%w: neither podman nor docker is usable here", container.ErrNoRuntime)
	}

	// A container left behind by an interrupted run holds the port and may
	// hold another run's objects, so it goes before this one starts.
	_, _ = run(ctx, rt.Path, "rm", "-f", minioContainer)

	// The bucket is a directory on the drive, created before the server reads
	// it. MinIO's single-drive backend lists the directories under its data
	// path as buckets, so this is a bucket that exists at startup without a
	// client having to create one.
	start := []string{
		"run", "-d", "--rm", "--name", minioContainer,
		"-p", "127.0.0.1:" + minioPort + ":9000",
		"-e", "MINIO_ROOT_USER=" + accessKey,
		"-e", "MINIO_ROOT_PASSWORD=" + secretKey,
		"--entrypoint", "/bin/sh",
		minioImage,
		"-c", "mkdir -p /data/" + bucketName + " && exec minio server /data --address :9000",
	}

	if out, err := run(ctx, rt.Path, start...); err != nil {
		return nil, fmt.Errorf("starting %s: %w: %s", minioImage, err, out)
	}

	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		_, _ = run(stopCtx, rt.Path, "rm", "-f", minioContainer)
	}

	if err := waitForMinIO(ctx); err != nil {
		logs, _ := run(ctx, rt.Path, "logs", minioContainer)
		stop()

		return nil, fmt.Errorf("%w: %s", err, logs)
	}

	// The credential chain the S3 driver resolves is the process environment,
	// which is what a deployment gives it too. Set here rather than with
	// t.Setenv because the store is opened by several tests and one of them may
	// well be the first.
	if err := os.Setenv("AWS_ACCESS_KEY_ID", accessKey); err != nil {
		stop()

		return nil, fmt.Errorf("setting the access key: %w", err)
	}

	if err := os.Setenv("AWS_SECRET_ACCESS_KEY", secretKey); err != nil {
		stop()

		return nil, fmt.Errorf("setting the secret key: %w", err)
	}

	bucketURL = defaultBucketURL

	return stop, nil
}

// minioBucketURL names the bucket the tests write to, optionally a subtree of
// it.
//
// The subtree is how two tests, or two runs, share one bucket: gocloud applies
// a "prefix" parameter before the driver sees the URL, so nothing in wsaw
// learns that its bucket is part of a larger one. A bucket is a service and is
// not created fresh per test, so isolation has to be a prefix.
func minioBucketURL(prefix string) string {
	u := bucketURL
	if u == "" {
		u = defaultBucketURL
	}

	if prefix != "" {
		u += "&prefix=" + prefix
	}

	return u
}

// defaultBucketURL is the server this package starts for itself.
//
// use_path_style is what makes an S3 URL work against anything that is not AWS:
// the virtual-host form puts the bucket in the hostname, and "wsaw.127.0.0.1"
// resolves nowhere. disable_https is because the fixture speaks plain HTTP on
// loopback, and it is the one setting here that a deployment must never copy.
const defaultBucketURL = "s3://" + bucketName +
	"?endpoint=http://127.0.0.1:" + minioPort +
	"&region=us-east-1&use_path_style=true&disable_https=true"

// waitForMinIO waits for the server to answer its own health endpoint.
//
// Polling rather than sleeping for a fixed time: a race that usually passes is
// worse than one that always fails, and how long an image takes to start is a
// property of the machine rather than of this test (AGENTS §5).
func waitForMinIO(ctx context.Context) error {
	deadline, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://127.0.0.1:" + minioPort + "/minio/health/live"

	for {
		req, err := http.NewRequestWithContext(deadline, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("asking MinIO whether it is up: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-deadline.Done():
			return fmt.Errorf("MinIO did not become healthy on port %s: %w", minioPort, deadline.Err())
		case <-tick.C:
		}
	}
}

// run executes one runtime command and returns its combined output.
func run(ctx context.Context, path string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w", path, strings.Join(args, " "), err)
	}

	return strings.TrimSpace(string(out)), nil
}
