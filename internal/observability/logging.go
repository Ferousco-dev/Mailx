// Package observability provides MailX's structured logging, Prometheus
// metrics, and operator HTTP listener. It only OBSERVES: PostgreSQL, Redis,
// and the SMTP path remain the sole sources of truth, and nothing here may
// change their behavior (a failing log handler or metric is swallowed).
//
// Privacy contract: callers log stable IDs and bounded categories only —
// never bodies, MIME, addresses, domains, URLs, secrets, signatures, or
// remote response text.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger builds the process logger. level is debug|info|warn|error and
// format is json|text (case-insensitive; empty selects info / json).
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid MAILX_LOG_LEVEL %q (want debug, info, warn, or error)", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("invalid MAILX_LOG_FORMAT %q (want json or text)", format)
	}
	return slog.New(safeHandler{h}), nil
}

// Discard returns a logger that drops everything; components use it until a
// real logger is injected.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// safeHandler guarantees a panicking or failing sink can never propagate
// into the operation being observed.
type safeHandler struct{ slog.Handler }

func (h safeHandler) Handle(ctx context.Context, r slog.Record) (err error) {
	defer func() {
		if recover() != nil {
			err = nil
		}
	}()
	return h.Handler.Handle(ctx, r)
}

func (h safeHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return safeHandler{h.Handler.WithAttrs(a)}
}
func (h safeHandler) WithGroup(n string) slog.Handler { return safeHandler{h.Handler.WithGroup(n)} }
