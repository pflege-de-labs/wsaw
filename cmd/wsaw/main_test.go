package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// capture runs fn with the process's standard streams redirected and returns
// what each received.
//
// The commands write to os.Stdout and os.Stderr directly, and deliberately so:
// the split between them is part of the contract — `wsaw share` prints the link
// on stdout so it can be piped, and everything else on stderr. Threading a
// writer through every command to make it testable would move that contract
// out of the place where it is documented, so the test swaps the streams
// instead. Doing that mutates package-level state, so no test that calls this
// may run in parallel.
func capture(t *testing.T, fn func()) (string, string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe for stdout: %v", err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe for stderr: %v", err)
	}

	originalOut, originalErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	var (
		outBuf, errBuf bytes.Buffer
		wg             sync.WaitGroup
	)

	// The readers run concurrently: a markdown report is larger than the pipe
	// buffer, so draining only after fn returned would deadlock.
	wg.Add(2)

	go func() {
		defer wg.Done()

		_, _ = io.Copy(&outBuf, outR)
	}()

	go func() {
		defer wg.Done()

		_, _ = io.Copy(&errBuf, errR)
	}()

	func() {
		defer func() {
			os.Stdout, os.Stderr = originalOut, originalErr

			_ = outW.Close()
			_ = errW.Close()
		}()

		fn()
	}()

	wg.Wait()

	_ = outR.Close()
	_ = errR.Close()

	return outBuf.String(), errBuf.String()
}

// writeConfig writes a configuration file into a fresh temporary directory and
// returns its path. The store lives in the same directory, so no test ever
// touches the real state directory in the developer's home.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.yaml")

	full := body + "\nstore:\n  path: " + filepath.Join(dir, "wsaw.db") + "\n"

	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	return path
}

// oneTargetConfig is the smallest configuration that resolves to something
// scannable.
const oneTargetConfig = `targets:
  - name: site
    url: https://example.com/
    consentModes: [reject]
`

func TestRunWithoutArgumentsExplainsItselfAndFails(t *testing.T) {
	var code int

	stdout, stderr := capture(t, func() {
		code = run(t.Context(), nil)
	})

	if code != exitOperational {
		t.Errorf("exit code = %d, want %d", code, exitOperational)
	}

	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("usage did not reach stderr; got %q", stderr)
	}

	// Usage for a failed invocation is diagnostic output, so it must not
	// pollute a pipeline reading stdout.
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	var code int

	_, stderr := capture(t, func() {
		code = run(t.Context(), []string{"frobnicate"})
	})

	if code != exitOperational {
		t.Errorf("exit code = %d, want %d", code, exitOperational)
	}

	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("stderr does not name the command: %q", stderr)
	}
}

func TestRunPrintsHelpOnStdout(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var code int

			stdout, _ := capture(t, func() {
				code = run(t.Context(), []string{arg})
			})

			if code != exitOK {
				t.Errorf("exit code = %d, want %d", code, exitOK)
			}

			// Asked-for help is the output, so it belongs on stdout.
			if !strings.Contains(stdout, "Commands:") {
				t.Errorf("help did not reach stdout; got %q", stdout)
			}
		})
	}
}

func TestRunPrintsBuildInformation(t *testing.T) {
	var code int

	stdout, _ := capture(t, func() {
		code = run(t.Context(), []string{"version"})
	})

	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}

	if !strings.Contains(stdout, "wsaw "+version) {
		t.Errorf("stdout = %q, want it to name the version %q", stdout, version)
	}
}

// TestRunDispatchesEveryCommand checks the switch, not the commands: each case
// is given arguments that fail inside the command it should reach, so the
// error text proves which one ran. A command word silently falling through to
// "unknown command" is exactly the regression this catches.
func TestRunDispatchesEveryCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "run", args: []string{"run", "--config", missing}, want: missing},
		{name: "daemon", args: []string{"daemon", "--config", missing}, want: missing},
		{name: "scan", args: []string{"scan", "--fail-on", "nonsense"}, want: "--fail-on"},
		{name: "debug", args: []string{cmdNameDebug}, want: "usage: wsaw debug"},
		{name: "rules", args: []string{"rules"}, want: "usage: wsaw rules"},
		{name: "config", args: []string{"config", "--config", missing}, want: missing},
		{name: "share", args: []string{"share"}, want: "--target is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var code int

			_, stderr := capture(t, func() {
				code = run(t.Context(), tc.args)
			})

			if code != exitOperational {
				t.Errorf("exit code = %d, want %d", code, exitOperational)
			}

			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.want)
			}
		})
	}
}
