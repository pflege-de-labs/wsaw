package container_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/martint17r/wsaw/internal/container"
)

// Runtime detection and container lifecycle are tested against a real runtime
// where one exists, and skipped with a reason where none does, so the
// container path is covered rather than assumed (Story 1.8, AC11).

func TestKindValid(t *testing.T) {
	t.Parallel()

	for _, k := range []container.Kind{"", container.KindAuto, container.KindPodman, container.KindDocker, container.KindLocal} {
		if !k.Valid() {
			t.Errorf("Kind(%q).Valid() = false", k)
		}
	}

	for _, k := range []container.Kind{"lxc", "kubernetes", "yes"} {
		if k.Valid() {
			t.Errorf("Kind(%q).Valid() = true, want false", k)
		}
	}
}

// TestDetectLocalIsAlwaysNil: asking for the local browser must never go
// looking for a runtime.
func TestDetectLocalIsAlwaysNil(t *testing.T) {
	t.Parallel()

	rt, err := container.Detect(context.Background(), container.KindLocal)
	if err != nil {
		t.Fatalf("Detect(local): %v", err)
	}

	if rt != nil {
		t.Errorf("Detect(local) returned a runtime: %+v", rt)
	}
}

// TestDetectAutoNeverFails is the behaviour that keeps wsaw usable on a
// machine without a runtime: auto falls back to the local browser rather than
// refusing to start.
func TestDetectAutoNeverFails(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := container.Detect(ctx, container.KindAuto)
	if err != nil {
		t.Fatalf("Detect(auto) returned an error rather than falling back: %v", err)
	}

	if rt == nil {
		t.Skip("no container runtime on this machine; auto correctly fell back to local")
	}

	if rt.Kind != container.KindPodman && rt.Kind != container.KindDocker {
		t.Errorf("Kind = %q, want podman or docker", rt.Kind)
	}

	if rt.Path == "" {
		t.Error("no path recorded for the detected runtime")
	}
}

// TestDetectAutoPrefersPodman encodes the preference and its reason: podman is
// daemonless and rootless by default.
func TestDetectAutoPrefersPodman(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not installed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := container.Detect(ctx, container.KindAuto)
	if err != nil || rt == nil {
		t.Skipf("no usable runtime: %v", err)
	}

	if rt.Kind != container.KindPodman {
		t.Errorf("Kind = %q, want podman when podman is available", rt.Kind)
	}
}

// TestDetectExplicitMissingRuntimeIsAnError: an operator who names a runtime
// must be told when it is unusable, not silently given something else.
func TestDetectExplicitMissingRuntimeIsAnError(t *testing.T) {
	// Not parallel: t.Setenv is forbidden in a parallel test.

	// A PATH with nothing on it, so the runtime cannot be found.
	t.Setenv("PATH", t.TempDir())

	_, err := container.Detect(context.Background(), container.KindPodman)
	if err == nil {
		t.Fatal("Detect returned no error for an explicitly requested, absent runtime")
	}

	if !errors.Is(err, container.ErrNoRuntime) {
		t.Errorf("err = %v, want ErrNoRuntime", err)
	}

	if !strings.Contains(err.Error(), "podman") {
		t.Errorf("error does not name the requested runtime: %v", err)
	}
}

// requireRuntime skips unless a runtime and the browser image are both
// present. The image is deliberately not pulled: a test must not download
// half a gigabyte, and wsaw itself never pulls implicitly either.
func requireRuntime(t *testing.T) *container.Runtime {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rt, err := container.Detect(ctx, container.KindAuto)
	if err != nil || rt == nil {
		t.Skipf("no container runtime available: %v", err)
	}

	if _, err := rt.BrowserVersion(ctx, ""); err != nil {
		t.Skipf("browser image not present locally: %v", err)
	}

	return rt
}

func TestBrowserVersionReportsTheImagesBrowser(t *testing.T) {
	rt := requireRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	version, err := rt.BrowserVersion(ctx, "")
	if err != nil {
		t.Fatalf("BrowserVersion: %v", err)
	}

	// The image's entrypoint echoes its command line first, so this also
	// checks that the last line is what gets reported.
	if !strings.Contains(strings.ToLower(version), "chrom") {
		t.Errorf("version = %q, want a Chrome or Chromium version", version)
	}

	if strings.Contains(version, "exec ") {
		t.Errorf("version = %q; the entrypoint's own output leaked into it", version)
	}
}

// TestMissingImageFailsWithThePullCommand is AC6: a scan must never block on
// fetching a browser, so an absent image is a startup failure that says how
// to fix it.
func TestMissingImageFailsWithThePullCommand(t *testing.T) {
	rt := requireRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, err := rt.BrowserVersion(ctx, "localhost/wsaw-does-not-exist:never")
	if err == nil {
		t.Fatal("a missing image was accepted")
	}

	if !strings.Contains(err.Error(), "pull") {
		t.Errorf("error does not tell the operator how to fetch the image: %v", err)
	}
}

func TestStartAndRemove(t *testing.T) {
	rt := requireRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	inst, err := rt.Start(ctx, container.Spec{StartupTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Removed even if an assertion below fails, so a failing test never
	// leaves a container behind.
	defer func() {
		if err := inst.Remove(context.Background()); err != nil {
			t.Errorf("Remove: %v", err)
		}
	}()

	if !strings.HasPrefix(inst.Endpoint(), "http://127.0.0.1:") {
		t.Errorf("endpoint = %q, want a loopback address: the debugging port must not be reachable from the network",
			inst.Endpoint())
	}

	if inst.ID() == "" {
		t.Error("no container id recorded")
	}

	if inst.StartupDuration <= 0 {
		t.Error("startup was not measured; its cost must be visible (AC8)")
	}

	t.Logf("container %s ready in %s at %s", inst.ID()[:12], inst.StartupDuration, inst.Endpoint())
}

// TestRemoveIsIdempotent matters because cleanup runs on several paths —
// normal exit, cancellation, panic — and must not fail on the second call.
func TestRemoveIsIdempotent(t *testing.T) {
	rt := requireRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	inst, err := rt.Start(ctx, container.Spec{StartupTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := inst.Remove(ctx); err != nil {
		t.Fatalf("first Remove: %v", err)
	}

	if err := inst.Remove(ctx); err != nil {
		t.Errorf("second Remove: %v, want nil for an already-removed container", err)
	}
}

// TestStartLeavesNothingBehindOnFailure: a container that never answers must
// not be left running.
func TestStartLeavesNothingBehindOnFailure(t *testing.T) {
	rt := requireRuntime(t)

	before := managedContainers(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A browser argument that makes the process exit immediately, so CDP is
	// never answered and startup fails.
	_, err := rt.Start(ctx, container.Spec{
		StartupTimeout: 15 * time.Second,
		BrowserArgs:    []string{"--version"},
	})
	if err == nil {
		t.Fatal("Start succeeded against a browser that exits immediately")
	}

	after := managedContainers(t, rt)

	if after > before {
		t.Errorf("containers grew from %d to %d after a failed start: one was left behind", before, after)
	}
}

func TestReapLeavesLiveContainersAlone(t *testing.T) {
	rt := requireRuntime(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	inst, err := rt.Start(ctx, container.Spec{StartupTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	defer func() { _ = inst.Remove(context.Background()) }()

	// This container is owned by this very process, so reaping must not touch
	// it — otherwise a second wsaw would kill the first one's browsers.
	if _, err := rt.Reap(ctx, nil); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if !containerExists(t, rt, inst.ID()) {
		t.Error("Reap removed a container belonging to a live process")
	}
}

func managedContainers(t *testing.T, rt *container.Runtime) int {
	t.Helper()

	out, err := exec.Command(rt.Path, "ps", "--all", "--quiet",
		"--filter", "label=io.wsaw.managed=true").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}

	return len(strings.Fields(string(out)))
}

func containerExists(t *testing.T, rt *container.Runtime, id string) bool {
	t.Helper()

	out, err := exec.Command(rt.Path, "ps", "--all", "--quiet", "--filter", "id="+id).Output()
	if err != nil {
		return false
	}

	return len(strings.Fields(string(out))) > 0
}
