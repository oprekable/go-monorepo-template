package logs

import (
	"context"
	"log/slog"
)

// FanoutHandler broadcasts slog records to multiple handlers.
// It implements [slog.Handler] and delegates each log record to all
// underlying handlers. This is useful when you need to send logs to
// multiple destinations simultaneously (e.g., console + OpenTelemetry).
type FanoutHandler struct {
	handlers []slog.Handler
}

// NewFanoutHandler creates a new FanoutHandler that broadcasts log records
// to all provided handlers. At least one handler must be provided.
func NewFanoutHandler(handlers ...slog.Handler) *FanoutHandler {
	return &FanoutHandler{handlers: handlers}
}

// Enabled reports whether any of the underlying handlers is enabled
// for the given level. This ensures that if at least one handler
// wants the record, it will be processed.
func (h *FanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle broadcasts the log record to all enabled underlying handlers.
// Each handler receives a clone of the record to prevent side effects.
// Returns the first error encountered, but continues processing all handlers.
func (h *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			if err := handler.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// WithAttrs returns a new FanoutHandler whose underlying handlers
// all have the given attributes pre-applied.
func (h *FanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		cloned[i] = handler.WithAttrs(attrs)
	}
	return &FanoutHandler{handlers: cloned}
}

// WithGroup returns a new FanoutHandler whose underlying handlers
// all have the given group name pre-applied.
func (h *FanoutHandler) WithGroup(name string) slog.Handler {
	cloned := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		cloned[i] = handler.WithGroup(name)
	}
	return &FanoutHandler{handlers: cloned}
}
