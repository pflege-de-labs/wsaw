package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// prettyHandler renders one record per line for a person reading a terminal.
//
// It is a peer of the JSON handler, not a lesser one: every attribute is
// rendered, nothing is summarised away, and it sits underneath the same
// scrubbing wrapper. A format meant for humans is exactly where a credential
// would end up in a scrollback buffer, so it must not become a way around
// redaction (Story 6.9, AC5 and AC6).
type prettyHandler struct {
	level slog.Leveler
	color bool

	// mu is shared between derived handlers so that concurrent scans cannot
	// interleave half-written lines.
	mu *sync.Mutex
	w  io.Writer

	// preformatted holds attributes contributed by WithAttrs, already
	// rendered with their group prefix. Scan-scoped fields arrive this way,
	// which is also why they appear first on the line.
	preformatted []string
	groups       []string
}

func newPrettyHandler(w io.Writer, level slog.Leveler, color bool) *prettyHandler {
	return &prettyHandler{level: level, color: color, mu: &sync.Mutex{}, w: w}
}

// ANSI codes. Colour is decoration only: the level is always written as text
// as well, so a log that loses its colour loses no meaning (AC4).
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiBold   = "\x1b[1m"
)

func (h *prettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *prettyHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := make([]string, 0, len(h.preformatted)+rec.NumAttrs())
	attrs = append(attrs, h.preformatted...)

	prefix := groupPrefix(h.groups)

	rec.Attrs(func(a slog.Attr) bool {
		attrs = h.appendAttr(attrs, prefix, a)

		return true
	})

	var b strings.Builder

	stamp := rec.Time
	if stamp.IsZero() {
		stamp = time.Now()
	}

	b.WriteString(h.paint(ansiDim, stamp.Format("15:04:05.000")))
	b.WriteByte(' ')
	b.WriteString(h.level5(rec.Level))
	b.WriteByte(' ')

	// The message is padded so that attributes line up across records, which
	// is what makes a stream of them scannable. A long message pushes its own
	// attributes out rather than truncating.
	const msgWidth = 34

	msg := rec.Message
	b.WriteString(msg)

	if len(attrs) > 0 {
		if pad := msgWidth - len([]rune(msg)); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}

		b.WriteByte(' ')
		b.WriteString(strings.Join(attrs, " "))
	}

	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, err := io.WriteString(h.w, b.String()); err != nil {
		return fmt.Errorf("writing log record: %w", err)
	}

	return nil
}

// level5 renders the level at a fixed width so the message column aligns.
func (h *prettyHandler) level5(l slog.Level) string {
	var (
		text  string
		color string
	)

	switch {
	case l < slog.LevelInfo:
		text, color = "DEBUG", ansiDim
	case l < slog.LevelWarn:
		text, color = "INFO ", ansiGreen
	case l < slog.LevelError:
		text, color = "WARN ", ansiYellow
	default:
		text, color = "ERROR", ansiBold+ansiRed
	}

	return h.paint(color, text)
}

func (h *prettyHandler) appendAttr(dst []string, prefix string, a slog.Attr) []string {
	// Resolve runs LogValuer, which is how a secret renders as its redacted
	// form rather than its contents.
	a.Value = a.Value.Resolve()

	if a.Equal(slog.Attr{}) {
		return dst
	}

	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return dst
		}

		inner := prefix
		if a.Key != "" {
			inner = prefix + a.Key + "."
		}

		for _, sub := range group {
			dst = h.appendAttr(dst, inner, sub)
		}

		return dst
	}

	return append(dst, h.paint(ansiBlue, prefix+a.Key)+h.paint(ansiDim, "=")+formatValue(a.Value))
}

// formatValue renders a value on exactly one line.
//
// Anything containing a newline, a quote, or whitespace is quoted, which
// turns an embedded newline into an escape. One record therefore always
// occupies one line and cannot be mistaken for several (AC7) — which matters
// because captured error text routinely contains newlines from a page.
func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quoteIfNeeded(v.String())

	case slog.KindTime:
		return v.Time().Format(time.RFC3339)

	case slog.KindDuration:
		return v.Duration().String()

	case slog.KindAny:
		switch t := v.Any().(type) {
		case error:
			return quoteIfNeeded(t.Error())
		case fmt.Stringer:
			return quoteIfNeeded(t.String())
		default:
			return quoteIfNeeded(fmt.Sprint(v.Any()))
		}

	default:
		return quoteIfNeeded(v.String())
	}
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}

	if strings.ContainsFunc(s, func(r rune) bool {
		return r <= ' ' || r == '"' || r == '=' || r == 0x7f
	}) {
		return strconv.Quote(s)
	}

	return s
}

func (h *prettyHandler) paint(color, s string) string {
	if !h.color || color == "" {
		return s
	}

	return color + s + ansiReset
}

func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	out := h.clone()

	prefix := groupPrefix(h.groups)
	for _, a := range attrs {
		out.preformatted = h.appendAttr(out.preformatted, prefix, a)
	}

	return out
}

func (h *prettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	out := h.clone()
	out.groups = append(out.groups, name)

	return out
}

func (h *prettyHandler) clone() *prettyHandler {
	return &prettyHandler{
		level: h.level,
		color: h.color,
		// The mutex and writer are shared, not copied: derived handlers must
		// serialize against each other.
		mu:           h.mu,
		w:            h.w,
		preformatted: append([]string(nil), h.preformatted...),
		groups:       append([]string(nil), h.groups...),
	}
}

func groupPrefix(groups []string) string {
	if len(groups) == 0 {
		return ""
	}

	return strings.Join(groups, ".") + "."
}
