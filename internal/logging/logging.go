// Package logging configures structured logging.
//
// Every scan-scoped line carries the scan ID, target, and consent mode, so
// lines from concurrent scans can be separated in an aggregator (Story 6.4).
// Secrets are removed centrally rather than at each call site.
//
// The output format follows where the output is going rather than a flag an
// operator has to remember: readable on a terminal, JSON everywhere else
// (Story 6.9).
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// Format selects how records are rendered.
type Format string

// Formats.
const (
	// FormatAuto picks pretty on an interactive terminal and json otherwise.
	// It is the default, and it is a named value rather than hidden
	// behaviour so an operator can see it in the configuration.
	FormatAuto Format = "auto"
	// FormatJSON is the machine-readable format aggregators expect.
	FormatJSON Format = "json"
	// FormatText is slog's key=value text handler.
	FormatText Format = "text"
	// FormatPretty is the aligned, coloured format for a person watching.
	FormatPretty Format = "pretty"
)

// Valid reports whether f is a format wsaw understands.
func (f Format) Valid() bool {
	switch f {
	case "", FormatAuto, FormatJSON, FormatText, FormatPretty:
		return true
	default:
		return false
	}
}

// Options configures the logger.
type Options struct {
	// Level is debug, info, warn or error.
	Level string
	// Format is auto, pretty, json or text. Empty means auto.
	Format string
	// Output receives the log lines.
	Output io.Writer
	// Secrets, when set, scrubs registered plaintexts from every attribute.
	Secrets *secret.Registry

	// Color forces colour on or off. Nil detects it, which is what almost
	// every caller wants.
	Color *bool
}

// New builds a logger and reports the format it resolved to, so a caller can
// state it in a startup line rather than leaving an operator to guess
// (Story 6.9, AC8).
func New(opts Options) (*slog.Logger, Format, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, "", err
	}

	requested := Format(strings.ToLower(strings.TrimSpace(opts.Format)))
	if !requested.Valid() {
		return nil, "", fmt.Errorf("unknown log format %q, use auto, pretty, json or text", opts.Format)
	}

	if opts.Output == nil {
		opts.Output = os.Stderr
	}

	resolved := resolveFormat(requested, opts.Output)

	var handler slog.Handler

	switch resolved {
	case FormatPretty:
		handler = newPrettyHandler(opts.Output, level, useColor(opts, resolved))

	case FormatText:
		handler = slog.NewTextHandler(opts.Output, &slog.HandlerOptions{Level: level})

	default:
		handler = slog.NewJSONHandler(opts.Output, &slog.HandlerOptions{Level: level})
	}

	if opts.Secrets != nil {
		// Wraps whichever handler was chosen: redaction must not depend on
		// the format (AC6).
		handler = &scrubbingHandler{inner: handler, secrets: opts.Secrets}
	}

	return slog.New(handler), resolved, nil
}

// resolveFormat turns auto into a concrete choice.
//
// A piped, redirected, containerised or service-managed run is not a
// terminal, so it keeps machine-readable output with no configuration. That
// is the case where getting it wrong is expensive: an aggregator silently
// ingesting decorated text is much worse than a person seeing JSON.
func resolveFormat(requested Format, w io.Writer) Format {
	if requested != "" && requested != FormatAuto {
		return requested
	}

	if isTerminal(w) {
		return FormatPretty
	}

	return FormatJSON
}

// isTerminal reports whether w is an interactive terminal.
//
// Detected through the character-device bit rather than with a dependency:
// wsaw targets Linux and macOS, where that is exactly what distinguishes a
// tty from a pipe or a file (Tenet 18, standard library first).
func isTerminal(w io.Writer) bool {
	f, ok := w.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return false
	}

	fi, err := f.Stat()
	if err != nil {
		return false
	}

	return fi.Mode()&os.ModeCharDevice != 0
}

// useColor decides whether to emit ANSI codes.
func useColor(opts Options, resolved Format) bool {
	if resolved != FormatPretty {
		return false
	}

	if opts.Color != nil {
		return *opts.Color
	}

	// The widely honoured opt-out, and the terminal that cannot render it.
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return false
	}

	switch os.Getenv("TERM") {
	case "", "dumb":
		return false
	}

	return isTerminal(opts.Output)
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q, use debug, info, warn or error", s)
	}
}

// scrubbingHandler removes registered secrets from string attributes.
//
// slog.LogValuer already protects a secret.Value that is logged directly, but
// a secret can also reach a log inside an error string from a third-party
// library, which no type can guard. This is the backstop for that, and it
// wraps every format equally.
type scrubbingHandler struct {
	inner   slog.Handler
	secrets *secret.Registry
}

func (h *scrubbingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *scrubbingHandler) Handle(ctx context.Context, rec slog.Record) error {
	scrubbed := slog.NewRecord(rec.Time, rec.Level, h.secrets.Scrub(rec.Message), rec.PC)

	rec.Attrs(func(a slog.Attr) bool {
		scrubbed.AddAttrs(h.scrubAttr(a))

		return true
	})

	if err := h.inner.Handle(ctx, scrubbed); err != nil {
		return fmt.Errorf("writing log record: %w", err)
	}

	return nil
}

func (h *scrubbingHandler) scrubAttr(a slog.Attr) slog.Attr {
	// Resolved first, so a LogValuer's output is scrubbed too rather than
	// slipping past as an opaque value.
	a.Value = a.Value.Resolve()

	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.secrets.Scrub(a.Value.String()))

	case slog.KindAny:
		// Errors are the most common carrier of an accidentally leaked
		// credential, since a library may embed a URL or header in its text.
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, h.secrets.Scrub(err.Error()))
		}

		if s, ok := a.Value.Any().(fmt.Stringer); ok {
			return slog.String(a.Key, h.secrets.Scrub(s.String()))
		}

		return a

	case slog.KindGroup:
		attrs := a.Value.Group()
		out := make([]any, 0, len(attrs))

		for _, inner := range attrs {
			out = append(out, h.scrubAttr(inner))
		}

		return slog.Group(a.Key, out...)

	default:
		return a
	}
}

func (h *scrubbingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		scrubbed = append(scrubbed, h.scrubAttr(a))
	}

	return &scrubbingHandler{inner: h.inner.WithAttrs(scrubbed), secrets: h.secrets}
}

func (h *scrubbingHandler) WithGroup(name string) slog.Handler {
	return &scrubbingHandler{inner: h.inner.WithGroup(name), secrets: h.secrets}
}
