// Package container runs headless Chrome inside a container.
//
// wsaw renders pages it does not control and treats every one as hostile
// (Tenet 9). Without a container the only boundary between a compromised
// renderer and the host is the Chrome sandbox — a boundary the browser
// enforces on itself. A container adds one the operating system enforces, and
// takes the browser build out of the host's hands, which makes results
// comparable across machines (Story 1.8).
//
// Podman is preferred over Docker because it is daemonless and rootless by
// default: wsaw needs no privileged socket, and an escape lands as an
// unprivileged user rather than as root.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultImage is the browser image, pinned by digest.
//
// Pinned rather than floating because the browser is part of what a result
// means: a tag that moves would change capture behaviour silently between
// scans, and a diff would report the change as the site's (Tenet 6).
const DefaultImage = "docker.io/chromedp/headless-shell@sha256:2d349b544a1ea6b5b5fd7c0fe99215ff662339c57407ee2e8c0a11af93516b04"

// cdpPort is the port the browser image listens on inside the container.
const cdpPort = "9222"

// Labels identify containers wsaw started, so orphans can be found and
// removed. A leaked container is worse than a leaked process: it survives the
// restart that would have cleaned the process up.
const (
	labelManaged  = "io.wsaw.managed"
	labelOwnerPID = "io.wsaw.owner-pid"
)

// Kind names a container runtime.
type Kind string

// Runtimes, in order of preference.
const (
	// KindAuto detects a runtime, preferring podman.
	KindAuto Kind = "auto"
	// KindPodman is preferred: daemonless and rootless by default.
	KindPodman Kind = "podman"
	// KindDocker is the fallback.
	KindDocker Kind = "docker"
	// KindLocal runs the browser as a host process instead.
	KindLocal Kind = "local"
)

// Valid reports whether k names a runtime wsaw understands.
func (k Kind) Valid() bool {
	switch k {
	case "", KindAuto, KindPodman, KindDocker, KindLocal:
		return true
	default:
		return false
	}
}

// ErrNoRuntime is returned when no container runtime is usable.
var ErrNoRuntime = errors.New("no usable container runtime")

// Runtime is a detected, working container runtime.
type Runtime struct {
	Kind Kind
	Path string
	// Version is the runtime's own version, recorded for reproducibility.
	Version string
}

// String renders the runtime for logs and results.
func (r *Runtime) String() string {
	if r == nil {
		return string(KindLocal)
	}

	return string(r.Kind) + " " + r.Version
}

// Detect finds a usable runtime.
//
// A runtime that is installed but not working — podman without a running
// machine, docker without a reachable daemon — is treated as absent rather
// than as a fatal error, because the local browser is a perfectly good
// fallback and failing startup over it would be unhelpful.
func Detect(ctx context.Context, preference Kind) (*Runtime, error) {
	switch preference {
	case KindLocal:
		return nil, nil

	case KindPodman, KindDocker:
		r, err := probe(ctx, preference)
		if err != nil {
			// An explicit choice that does not work is an error: the operator
			// asked for something specific and silently doing otherwise would
			// hide it.
			return nil, fmt.Errorf("%w: %s was requested: %w", ErrNoRuntime, preference, err)
		}

		return r, nil

	default:
		for _, kind := range []Kind{KindPodman, KindDocker} {
			if r, err := probe(ctx, kind); err == nil {
				return r, nil
			}
		}

		return nil, nil
	}
}

func probe(ctx context.Context, kind Kind) (*Runtime, error) {
	path, err := exec.LookPath(string(kind))
	if err != nil {
		return nil, fmt.Errorf("not on PATH: %w", err)
	}

	const probeTimeout = 20 * time.Second

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// "version" alone only proves the client exists; the server side has to
	// answer too, or every scan would fail later instead of now.
	out, err := run(probeCtx, path, "info", "--format", "{{.Host.OS}}")
	if err != nil {
		out, err = run(probeCtx, path, "info", "--format", "{{.OSType}}")
		if err != nil {
			return nil, fmt.Errorf("runtime is installed but not responding: %w", err)
		}
	}

	_ = out

	version, err := run(probeCtx, path, "version", "--format", "{{.Client.Version}}")
	if err != nil {
		version = "unknown"
	}

	return &Runtime{Kind: kind, Path: path, Version: strings.TrimSpace(version)}, nil
}

// Spec describes the container to start.
type Spec struct {
	// Image is the browser image, pinned by digest.
	Image string

	// Memory caps the container, e.g. "1g". Empty leaves it to the runtime.
	Memory string
	// PidsLimit bounds runaway process creation inside the container.
	PidsLimit int

	// SHMSize is the size of /dev/shm inside the container, e.g. "1g".
	// Empty selects DefaultSHMSize.
	SHMSize string
	// FileDescriptors is the container's open-file limit. Zero selects
	// DefaultFileDescriptors.
	FileDescriptors int

	// ExtraArgs are additional runtime arguments for unusual environments.
	ExtraArgs []string
	// BrowserArgs are appended to the browser's own command line.
	BrowserArgs []string

	// StartupTimeout bounds how long the container may take to answer CDP.
	StartupTimeout time.Duration

	Logger *slog.Logger
}

func (s *Spec) image() string {
	if s.Image == "" {
		return DefaultImage
	}

	return s.Image
}

// DefaultSHMSize is the size of /dev/shm given to the browser container.
//
// Chrome keeps renderer shared memory in /dev/shm, and both podman and docker
// default it to 64 MB. That is not enough for a real page: when a site
// releases its images in one burst, allocations start failing and Chrome
// reports net::ERR_INSUFFICIENT_RESOURCES for requests that never opened a
// connection. Those requests then look like assets the site stopped loading,
// which is a finding wsaw would be inventing out of its own environment
// (Tenet 5). One gigabyte is what the Chrome team recommends for
// containerised runs, and it is cheap: a limit, not an allocation.
const DefaultSHMSize = "1g"

// DefaultFileDescriptors is the container's open-file limit.
//
// A page with a few hundred subresources needs a socket, and therefore a
// descriptor, for each one in flight. The common container default of 1024 is
// shared with everything else the browser has open, and running out produces
// the same phantom-removal failure mode as a small /dev/shm.
const DefaultFileDescriptors = 8192

func (s *Spec) shmSize() string {
	if s.SHMSize == "" {
		return DefaultSHMSize
	}

	return s.SHMSize
}

func (s *Spec) fileDescriptors() int {
	if s.FileDescriptors <= 0 {
		return DefaultFileDescriptors
	}

	return s.FileDescriptors
}

func (s *Spec) startupTimeout() time.Duration {
	if s.StartupTimeout > 0 {
		return s.StartupTimeout
	}

	return 90 * time.Second
}

func (s *Spec) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}

	return slog.Default()
}

// Instance is one running browser container.
type Instance struct {
	runtime *Runtime
	id      string
	name    string

	// endpoint is the CDP HTTP endpoint on the host.
	endpoint string

	// StartupDuration is how long the container took to answer, so the cost
	// of the isolation is measurable rather than assumed.
	StartupDuration time.Duration
}

// Endpoint returns the CDP endpoint to attach to.
func (i *Instance) Endpoint() string { return i.endpoint }

// ID returns the container identifier, for logs.
func (i *Instance) ID() string { return i.id }

// runArgs builds the runtime's command line. It is separate from Start so the
// arguments can be asserted without a runtime present: they carry decisions —
// the loopback publish, the resource limits — whose regression would be
// invisible in a passing scan.
func runArgs(name string, spec Spec) []string {
	args := []string{
		"run", "--detach", "--rm",
		"--name", name,

		// Never pull during a scan: a scan must not block on fetching a
		// browser, and an image that changed under us would change what a
		// result means (Tenet 14, Story 1.8 AC6).
		"--pull=never",

		// Published on loopback only. The browser's debugging port is a
		// remote-control interface for something rendering hostile content;
		// it must not be reachable from the network.
		"--publish", "127.0.0.1::" + cdpPort,

		"--label", labelManaged + "=true",
		"--label", labelOwnerPID + "=" + strconv.Itoa(os.Getpid()),
	}

	if spec.Memory != "" {
		args = append(args, "--memory", spec.Memory)
	}

	// Both of these exist to stop the browser starving in ways that look like
	// the site changing. See DefaultSHMSize.
	args = append(args, "--shm-size", spec.shmSize())

	fds := strconv.Itoa(spec.fileDescriptors())
	args = append(args, "--ulimit", "nofile="+fds+":"+fds)

	if spec.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(spec.PidsLimit))
	}

	args = append(args, spec.ExtraArgs...)
	args = append(args, spec.image())
	args = append(args, spec.BrowserArgs...)

	return args
}

// Start runs a browser container and waits for it to answer CDP.
func (r *Runtime) Start(ctx context.Context, spec Spec) (*Instance, error) {
	started := time.Now()

	name := "wsaw-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	args := runArgs(name, spec)

	startCtx, cancel := context.WithTimeout(ctx, spec.startupTimeout())
	defer cancel()

	out, err := run(startCtx, r.Path, args...)
	if err != nil {
		if isImageMissing(err.Error() + out) {
			return nil, fmt.Errorf(
				"browser image %s is not present locally and wsaw does not pull during a scan; fetch it with: %s pull %s",
				spec.image(), r.Kind, spec.image())
		}

		return nil, fmt.Errorf("starting browser container: %w", err)
	}

	inst := &Instance{runtime: r, id: strings.TrimSpace(out), name: name}

	endpoint, err := r.waitForCDP(startCtx, inst, spec)
	if err != nil {
		// Never leave a container behind because startup failed.
		_ = inst.Remove(context.WithoutCancel(ctx))

		return nil, err
	}

	inst.endpoint = endpoint
	inst.StartupDuration = time.Since(started)

	spec.logger().Debug("browser container started",
		"runtime", string(r.Kind),
		"container", shortID(inst.id),
		"endpoint", endpoint,
		"startup", inst.StartupDuration.String(),
	)

	return inst, nil
}

// waitForCDP polls until the browser answers, or the budget runs out.
func (r *Runtime) waitForCDP(ctx context.Context, inst *Instance, spec Spec) (string, error) {
	port, err := r.publishedPort(ctx, inst)
	if err != nil {
		return "", err
	}

	endpoint := "http://127.0.0.1:" + port

	client := &http.Client{Timeout: 3 * time.Second}

	const poll = 200 * time.Millisecond

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var lastErr error

	for {
		if ok, err := probeCDP(ctx, client, endpoint); ok {
			return endpoint, nil
		} else if err != nil {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("browser container did not answer CDP at %s within the startup budget: %w",
				endpoint, errors.Join(ctx.Err(), lastErr))
		case <-ticker.C:
		}
	}
}

func probeCDP(ctx context.Context, client *http.Client, endpoint string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}

	var v struct {
		Browser string `json:"Browser"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return false, err
	}

	return v.Browser != "", nil
}

// publishedPort asks the runtime which host port the CDP port landed on.
func (r *Runtime) publishedPort(ctx context.Context, inst *Instance) (string, error) {
	out, err := run(ctx, r.Path, "port", inst.id, cdpPort+"/tcp")
	if err != nil {
		return "", fmt.Errorf("reading the published port of container %s: %w", shortID(inst.id), err)
	}

	// Output is one or more lines of "0.0.0.0:40289" or "127.0.0.1:40289".
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])

	idx := strings.LastIndex(line, ":")
	if idx < 0 || idx == len(line)-1 {
		return "", fmt.Errorf("could not parse published port from %q", out)
	}

	return line[idx+1:], nil
}

// BrowserVersion reports the browser version in the image, which doubles as a
// check that the image is present and runnable.
func (r *Runtime) BrowserVersion(ctx context.Context, image string) (string, error) {
	if image == "" {
		image = DefaultImage
	}

	const versionTimeout = 60 * time.Second

	versionCtx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	out, err := run(versionCtx, r.Path, "run", "--rm", "--pull=never", image, "--version")
	if err != nil {
		if isImageMissing(err.Error() + out) {
			return "", fmt.Errorf(
				"browser image %s is not present locally; fetch it with: %s pull %s",
				image, r.Kind, image)
		}

		return "", fmt.Errorf("reading the browser version from %s: %w", image, err)
	}

	// The image's entrypoint echoes its command line before the version, so
	// the last non-empty line is the answer.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line, nil
		}
	}

	return "", fmt.Errorf("no version reported by %s", image)
}

// Remove stops and deletes the container. It is safe to call more than once.
func (i *Instance) Remove(ctx context.Context) error {
	if i == nil || i.id == "" {
		return nil
	}

	const removeTimeout = 30 * time.Second

	rmCtx, cancel := context.WithTimeout(ctx, removeTimeout)
	defer cancel()

	if _, err := run(rmCtx, i.runtime.Path, "rm", "--force", i.id); err != nil {
		// Already gone is success, not failure: the container was started
		// with --rm and may have exited on its own.
		if isNoSuchContainer(err.Error()) {
			return nil
		}

		return fmt.Errorf("removing browser container %s: %w", shortID(i.id), err)
	}

	return nil
}

// Reap removes containers left behind by an earlier wsaw that did not shut
// down cleanly, and reports how many it removed.
//
// Only containers whose owning process is gone are touched, so a second wsaw
// running alongside this one is never disturbed.
func (r *Runtime) Reap(ctx context.Context, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}

	const reapTimeout = 30 * time.Second

	reapCtx, cancel := context.WithTimeout(ctx, reapTimeout)
	defer cancel()

	out, err := run(reapCtx, r.Path,
		"ps", "--all", "--quiet",
		"--filter", "label="+labelManaged+"=true")
	if err != nil {
		return 0, fmt.Errorf("listing wsaw containers: %w", err)
	}

	removed := 0

	for _, id := range strings.Fields(out) {
		owner, err := run(reapCtx, r.Path, "inspect", id,
			"--format", "{{index .Config.Labels \""+labelOwnerPID+"\"}}")
		if err != nil {
			continue
		}

		// Removal requires positively establishing that the owner is gone.
		// An owner that cannot be read or parsed is left alone: reaping
		// something wsaw cannot attribute risks killing the browser of
		// another wsaw running alongside this one, which is a far worse
		// outcome than leaving one container behind.
		pid, convErr := strconv.Atoi(strings.TrimSpace(owner))
		if convErr != nil || pid <= 0 {
			logger.Debug("leaving a wsaw container alone: its owner could not be determined",
				"container", shortID(id), "owner", strings.TrimSpace(owner))

			continue
		}

		if processAlive(pid) {
			// Another wsaw is using it.
			continue
		}

		if _, err := run(reapCtx, r.Path, "rm", "--force", id); err != nil {
			logger.Warn("could not remove an orphaned browser container",
				"container", shortID(id), "error", err)

			continue
		}

		removed++

		logger.Info("removed an orphaned browser container",
			"container", shortID(id), "owner_pid", strings.TrimSpace(owner))
	}

	return removed, nil
}

// processAlive reports whether a pid is running.
//
// Signal 0 performs the existence and permission checks without delivering
// anything. It must be syscall.Signal(0) and not a nil signal: os.Process
// rejects a nil signal outright, which would make every process look dead and
// turn reaping into an indiscriminate cleanup of other instances' browsers.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = proc.Signal(syscall.Signal(0))

	// EPERM means the process exists but belongs to another user, which still
	// counts as alive.
	return err == nil || errors.Is(err, syscall.EPERM)
}

func run(ctx context.Context, path string, args ...string) (string, error) {
	// #nosec G204 -- path comes from exec.LookPath over a fixed runtime list,
	// and arguments are built here rather than taken from a scanned page.
	cmd := exec.CommandContext(ctx, path, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s",
			path, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

func isImageMissing(s string) bool {
	s = strings.ToLower(s)

	for _, marker := range []string{
		"no such image", "image not known", "unable to find image",
		"manifest unknown", "not present", "pull=never",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}

	return false
}

func isNoSuchContainer(s string) bool {
	s = strings.ToLower(s)

	return strings.Contains(s, "no such container") || strings.Contains(s, "not found")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}
