// Package logging configures structured logging.
//
// Every scan-scoped line carries the scan ID, target, and consent mode, so
// lines from concurrent scans can be separated in an aggregator (Story 6.4).
// Secrets are removed centrally rather than at each call site.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/martint17r/wsaw/internal/secret"
)

// Options configures the logger.
type Options struct {
	// Level is debug, info, warn or error.
	Level string
	// Format is json or text. JSON is the default because these logs are
	// meant for aggregators, not for reading in a terminal.
	Format string
	// Output receives the log lines.
	Output io.Writer
	// Secrets, when set, scrubs registered plaintexts from every attribute.
	Secrets *secret.Registry
}

// New builds a logger.
func New(opts Options) (*slog.Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	handlerOpts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler

	switch strings.ToLower(opts.Format) {
	case "", "json":
		handler = slog.NewJSONHandler(opts.Output, handlerOpts)
	case "text":
		handler = slog.NewTextHandler(opts.Output, handlerOpts)
	default:
		return nil, fmt.Errorf("unknown log format %q, use json or text", opts.Format)
	}

	if opts.Secrets != nil {
		handler = &scrubbingHandler{inner: handler, secrets: opts.Secrets}
	}

	return slog.New(handler), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
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
// library, which no type can guard. This is the backstop for that.
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
