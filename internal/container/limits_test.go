package container

import (
	"slices"
	"strings"
	"testing"
)

// argValue returns the value following flag in an argument list.
func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()

	i := slices.Index(args, flag)
	if i < 0 || i == len(args)-1 {
		t.Fatalf("%s is not in the argument list: %s", flag, strings.Join(args, " "))
	}

	return args[i+1]
}

// TestBrowserGetsEnoughSharedMemoryByDefault is the fix for a failure mode
// that looked like the site changing: Chrome keeps renderer shared memory in
// /dev/shm, podman and docker default it to 64 MB, and a page that releases
// its images in one burst then loses requests to
// net::ERR_INSUFFICIENT_RESOURCES before they open a connection. The assets
// those requests would have loaded go missing, and a diff reports them as
// assets the site stopped loading.
func TestBrowserGetsEnoughSharedMemoryByDefault(t *testing.T) {
	t.Parallel()

	args := runArgs("wsaw-test", Spec{})

	if got := argValue(t, args, "--shm-size"); got != DefaultSHMSize {
		t.Errorf("--shm-size = %q, want %q", got, DefaultSHMSize)
	}

	// The limit is set on both the soft and the hard side, or the soft one
	// stays at the runtime default and nothing changes.
	want := "nofile=8192:8192"
	if got := argValue(t, args, "--ulimit"); got != want {
		t.Errorf("--ulimit = %q, want %q", got, want)
	}
}

func TestContainerLimitsAreOverridable(t *testing.T) {
	t.Parallel()

	args := runArgs("wsaw-test", Spec{SHMSize: "512m", FileDescriptors: 4096})

	if got := argValue(t, args, "--shm-size"); got != "512m" {
		t.Errorf("--shm-size = %q, want the configured 512m", got)
	}

	if got := argValue(t, args, "--ulimit"); got != "nofile=4096:4096" {
		t.Errorf("--ulimit = %q, want nofile=4096:4096", got)
	}
}

// TestRunArgsKeepsTheBoundaryDecisions guards the parts of the command line
// that are security decisions rather than tuning.
func TestRunArgsKeepsTheBoundaryDecisions(t *testing.T) {
	t.Parallel()

	args := runArgs("wsaw-test", Spec{PidsLimit: 512, Memory: "1g"})

	if got := argValue(t, args, "--publish"); got != "127.0.0.1::"+cdpPort {
		t.Errorf("--publish = %q; the debugging port must stay on loopback", got)
	}

	if !slices.Contains(args, "--pull=never") {
		t.Error("--pull=never is missing; a scan must not block on fetching a browser")
	}

	if got := argValue(t, args, "--pids-limit"); got != "512" {
		t.Errorf("--pids-limit = %q, want 512", got)
	}

	if got := argValue(t, args, "--memory"); got != "1g" {
		t.Errorf("--memory = %q, want 1g", got)
	}
}

// TestBrowserArgsComeAfterTheImage because anything before it is read by the
// runtime and anything after it by the browser.
func TestBrowserArgsComeAfterTheImage(t *testing.T) {
	t.Parallel()

	args := runArgs("wsaw-test", Spec{
		Image:       "example.test/browser:1",
		ExtraArgs:   []string{"--network", "host"},
		BrowserArgs: []string{"--disable-gpu"},
	})

	image := slices.Index(args, "example.test/browser:1")
	if image < 0 {
		t.Fatalf("the image is not in the argument list: %s", strings.Join(args, " "))
	}

	if runtimeArg := slices.Index(args, "--network"); runtimeArg > image {
		t.Error("extraArgs landed after the image, where the browser would read them")
	}

	if browserArg := slices.Index(args, "--disable-gpu"); browserArg < image {
		t.Error("browserArgs landed before the image, where the runtime would read them")
	}
}
