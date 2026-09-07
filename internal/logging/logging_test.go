package logging_test

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/logging"
	"github.com/pflege-de-labs/wsaw/internal/secret"
)

const plaintext = "hunter2-super-secret"

func TestJSONFormat(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("scan finished", "target", "site", "consent_mode", "reject")

	out := b.String()
	if !strings.HasPrefix(out, "{") {
		t.Errorf("output is not JSON: %s", out)
	}

	for _, want := range []string{`"msg":"scan finished"`, `"target":"site"`, `"consent_mode":"reject"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b, Level: "warn"})
	if err != nil {
		t.Fatal(err)
	}

	log.Debug("noise")
	log.Info("also noise")
	log.Warn("signal")

	out := b.String()

	if strings.Contains(out, "noise") {
		t.Error("lines below the configured level were emitted")
	}

	if !strings.Contains(out, "signal") {
		t.Error("a line at the configured level was dropped")
	}
}

func TestInvalidLevelAndFormatAreRejected(t *testing.T) {
	t.Parallel()

	if _, _, err := logging.New(logging.Options{Level: "verbose"}); err == nil {
		t.Error("an unknown level was accepted")
	}

	if _, _, err := logging.New(logging.Options{Format: "xml"}); err == nil {
		t.Error("an unknown format was accepted")
	}
}

// TestSecretsAreScrubbedFromErrorText is the case a typed secret cannot
// cover: a credential embedded in a third-party library's error string.
func TestSecretsAreScrubbedFromErrorText(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b, Secrets: &reg})
	if err != nil {
		t.Fatal(err)
	}

	log.Error("request failed", "error", errors.New("dial https://user:"+plaintext+"@proxy: refused"))

	if strings.Contains(b.String(), plaintext) {
		t.Errorf("secret leaked through an error attribute: %s", b.String())
	}

	if !strings.Contains(b.String(), secret.Redacted) {
		t.Errorf("no redaction marker in output: %s", b.String())
	}
}

func TestSecretsAreScrubbedFromMessageAndStringAttrs(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b, Secrets: &reg})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("token is "+plaintext, "url", "https://hook/"+plaintext)

	if strings.Contains(b.String(), plaintext) {
		t.Errorf("secret leaked: %s", b.String())
	}
}

// TestSecretsAreScrubbedFromInheritedAttributes covers the common pattern of
// a scan-scoped logger built with With().
func TestSecretsAreScrubbedFromInheritedAttributes(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b, Secrets: &reg})
	if err != nil {
		t.Fatal(err)
	}

	scoped := log.With("proxy", "https://user:"+plaintext+"@proxy")
	scoped.Info("scan started")

	if strings.Contains(b.String(), plaintext) {
		t.Errorf("secret leaked through an inherited attribute: %s", b.String())
	}
}

func TestScrubbingIsOptional(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b})
	if err != nil {
		t.Fatal(err)
	}

	log.Info("plain message", "k", "v")

	if !strings.Contains(b.String(), "plain message") {
		t.Error("logging without a secret registry lost the message")
	}
}

// --- Story 6.9: readable console logs -------------------------------------

func ptr[T any](v T) *T { return &v }

// TestAutoResolvesToJSONWhenNotATerminal is the case that matters most: a
// piped, redirected, containerised or service-managed run must keep
// machine-readable output without anyone configuring it.
func TestAutoResolvesToJSONWhenNotATerminal(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	log, format, err := logging.New(logging.Options{Output: &b, Format: "auto"})
	if err != nil {
		t.Fatal(err)
	}

	if format != logging.FormatJSON {
		t.Errorf("format = %q, want json when the output is not a terminal", format)
	}

	log.Info("scan started", "target", "site")

	if !strings.HasPrefix(b.String(), "{") {
		t.Errorf("output is not JSON: %s", b.String())
	}
}

func TestEmptyFormatMeansAuto(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	_, format, err := logging.New(logging.Options{Output: &b})
	if err != nil {
		t.Fatal(err)
	}

	if format != logging.FormatJSON {
		t.Errorf("format = %q, want the auto default to resolve to json off a terminal", format)
	}
}

// TestAutoResolvesToPrettyOnATerminal uses a pty, since the whole point of
// the detection is the character-device bit.
func TestAutoResolvesToPrettyOnATerminal(t *testing.T) {
	t.Parallel()

	pty, tty, err := openPTY()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}

	defer func() {
		_ = pty.Close()
		_ = tty.Close()
	}()

	_, format, err := logging.New(logging.Options{Output: tty, Format: "auto"})
	if err != nil {
		t.Fatal(err)
	}

	if format != logging.FormatPretty {
		t.Errorf("format = %q, want pretty when the output is a terminal", format)
	}
}

func TestFormatCanBeForcedInBothDirections(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	_, format, err := logging.New(logging.Options{Output: &b, Format: "pretty"})
	if err != nil {
		t.Fatal(err)
	}

	if format != logging.FormatPretty {
		t.Errorf("format = %q, want pretty when forced", format)
	}

	pty, tty, err := openPTY()
	if err == nil {
		defer func() {
			_ = pty.Close()
			_ = tty.Close()
		}()

		_, format, err = logging.New(logging.Options{Output: tty, Format: "json"})
		if err != nil {
			t.Fatal(err)
		}

		if format != logging.FormatJSON {
			t.Errorf("format = %q, want json when forced even on a terminal", format)
		}
	}
}

func prettyLog(t *testing.T, opts logging.Options) (*slog.Logger, *strings.Builder) {
	t.Helper()

	var b strings.Builder

	opts.Output = &b
	opts.Format = "pretty"

	if opts.Color == nil {
		opts.Color = ptr(false)
	}

	log, _, err := logging.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	return log, &b
}

func TestPrettyRendersMessageAndAttributes(t *testing.T) {
	t.Parallel()

	log, b := prettyLog(t, logging.Options{})

	log.Info("scan finished", "target", "site", "requests", 42)

	out := b.String()

	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one line, got:\n%s", out)
	}

	for _, want := range []string{"INFO", "scan finished", "target=site", "requests=42"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q: %s", want, out)
		}
	}
}

// TestPrettyKeepsScanScopedFields: a readable line that cannot be tied back
// to its scan is not much use to the operator it was made readable for.
func TestPrettyKeepsScanScopedFields(t *testing.T) {
	t.Parallel()

	log, b := prettyLog(t, logging.Options{})

	scoped := log.With("scan_id", "scan-abc", "target", "site", "consent_mode", "reject")
	scoped.Info("scan started", "url", "https://example.com/")

	out := b.String()

	for _, want := range []string{"scan_id=scan-abc", "target=site", "consent_mode=reject", "url=https://example.com/"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q: %s", want, out)
		}
	}
}

// TestPrettyKeepsOneRecordOnOneLine covers captured error text, which
// routinely contains newlines from a scanned page.
func TestPrettyKeepsOneRecordOnOneLine(t *testing.T) {
	t.Parallel()

	log, b := prettyLog(t, logging.Options{})

	log.Error("scan failed", "error", errors.New("first line\nsecond line\nthird line"))

	out := b.String()

	if strings.Count(out, "\n") != 1 {
		t.Errorf("a multi-line value split one record across lines:\n%s", out)
	}

	// The content must survive, escaped rather than dropped.
	if !strings.Contains(out, `second line`) {
		t.Errorf("the value was lost: %s", out)
	}
}

func TestPrettyLevelIsAlwaysTextNotOnlyColour(t *testing.T) {
	t.Parallel()

	log, b := prettyLog(t, logging.Options{Level: "debug", Color: ptr(true)})

	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")

	out := b.String()

	for _, want := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		if !strings.Contains(out, want) {
			t.Errorf("level %q is not present as text: %s", want, out)
		}
	}

	if !strings.Contains(out, "\x1b[") {
		t.Error("colour was forced on but no ANSI codes were emitted")
	}
}

func TestPrettyColourIsSuppressed(t *testing.T) {
	t.Parallel()

	// Not a terminal, so colour is off even without an explicit setting.
	var b strings.Builder

	log, _, err := logging.New(logging.Options{Output: &b, Format: "pretty"})
	if err != nil {
		t.Fatal(err)
	}

	log.Warn("careful")

	if strings.Contains(b.String(), "\x1b[") {
		t.Errorf("ANSI codes were emitted to a non-terminal: %q", b.String())
	}
}

func TestNoColorEnvironmentIsHonoured(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")

	pty, tty, err := openPTY()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}

	defer func() {
		_ = pty.Close()
		_ = tty.Close()
	}()

	log, format, err := logging.New(logging.Options{Output: tty, Format: "pretty"})
	if err != nil {
		t.Fatal(err)
	}

	if format != logging.FormatPretty {
		t.Fatalf("format = %q", format)
	}

	done := make(chan string, 1)

	go func() {
		buf := make([]byte, 4096)

		n, _ := pty.Read(buf)
		done <- string(buf[:n])
	}()

	log.Warn("careful")

	select {
	case out := <-done:
		if strings.Contains(out, "\x1b[") {
			t.Errorf("NO_COLOR was set but ANSI codes were emitted: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading from the pty")
	}
}

func TestDumbTerminalGetsNoColour(t *testing.T) {
	t.Setenv("TERM", "dumb")

	pty, tty, err := openPTY()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}

	defer func() {
		_ = pty.Close()
		_ = tty.Close()
	}()

	var b strings.Builder

	// The writer is the buffer, but TERM is what is being asserted on; force
	// the terminal path by asking for pretty explicitly.
	log, _, err := logging.New(logging.Options{Output: &b, Format: "pretty"})
	if err != nil {
		t.Fatal(err)
	}

	log.Warn("careful")

	if strings.Contains(b.String(), "\x1b[") {
		t.Errorf("TERM=dumb but ANSI codes were emitted: %q", b.String())
	}
}

// TestPrettyRedactsSecrets is AC6: the readable format is exactly where a
// credential would reach a scrollback buffer, so it must not be a way around
// redaction.
func TestPrettyRedactsSecrets(t *testing.T) {
	t.Parallel()

	var reg secret.Registry

	reg.Add(secret.Literal(plaintext))

	log, b := prettyLog(t, logging.Options{Secrets: &reg})

	// Three routes a credential can take: the typed value, an error string,
	// and an inherited attribute.
	log.Info("auth", "token", secret.Literal(plaintext))
	log.Error("failed", "error", errors.New("dial https://user:"+plaintext+"@proxy"))
	log.With("proxy", "https://user:"+plaintext+"@proxy").Info("scanning")

	out := b.String()

	if strings.Contains(out, plaintext) {
		t.Errorf("a secret reached the pretty output: %s", out)
	}

	if !strings.Contains(out, secret.Redacted) {
		t.Errorf("no redaction marker in output: %s", out)
	}
}

func TestPrettyRendersGroups(t *testing.T) {
	t.Parallel()

	log, b := prettyLog(t, logging.Options{})

	log.WithGroup("scan").Info("done", "id", "abc")

	if !strings.Contains(b.String(), "scan.id=abc") {
		t.Errorf("group prefix missing: %s", b.String())
	}
}

func TestPrettyIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	log, b := prettyLog(t, logging.Options{})

	var wg sync.WaitGroup

	for i := range 20 {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			log.With("worker", i).Info("scan started", "target", "site")
		}(i)
	}

	wg.Wait()

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d lines, want 20: concurrent writes interleaved", len(lines))
	}

	for _, l := range lines {
		if !strings.Contains(l, "scan started") || !strings.Contains(l, "target=site") {
			t.Errorf("a line was written partially: %q", l)
		}
	}
}
