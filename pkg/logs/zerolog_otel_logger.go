package logs

import (
	"io"
	"log/slog"
	"os"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// LoggerConfig holds configuration for creating a logger with
// zerolog backend and OpenTelemetry bridge.
type LoggerConfig struct {
	ZerologWriter io.Writer
	ServiceName   string
	Level         slog.Level
}

// NewZerologOtelLogger creates a [*slog.Logger] with the following handler stack:
//
//	TraceHandler → FanoutHandler → [zerolog.SlogHandler, otelslog.Handler]
//
// The TraceHandler injects trace_id and span_id from the context.
// The FanoutHandler broadcasts each log record to both:
//   - zerolog: high-performance structured JSON output to the configured writer
//   - otelslog: OpenTelemetry log bridge that sends logs to the OTel Collector
//
// The caller must have initialized an OpenTelemetry LoggerProvider and set it
// as the global provider before calling this function. If the LoggerProvider is
// not configured, the otelslog handler will use a no-op provider.
func NewZerologOtelLogger(cfg LoggerConfig) *slog.Logger {
	writer := cfg.ZerologWriter
	if writer == nil {
		writer = os.Stderr
	}

	// Create zerolog logger and its slog handler.
	zl := zerolog.New(writer).With().Timestamp().Logger()
	zerologLevel := mapSlogLevelToZerolog(cfg.Level)
	zl = zl.Level(zerologLevel)
	zerologHandler := zerolog.NewSlogHandler(zl)

	// Create OpenTelemetry slog bridge handler.
	otelHandler := otelslog.NewHandler(cfg.ServiceName)

	// Fan out to both handlers.
	fanout := NewFanoutHandler(zerologHandler, otelHandler)

	// Wrap with TraceHandler for trace_id/span_id injection.
	traceHandler := NewTraceHandler(fanout)

	return slog.New(traceHandler)
}

// mapSlogLevelToZerolog converts a [slog.Level] to the equivalent [zerolog.Level].
func mapSlogLevelToZerolog(level slog.Level) zerolog.Level {
	switch {
	case level <= slog.LevelDebug:
		return zerolog.DebugLevel
	case level <= slog.LevelInfo:
		return zerolog.InfoLevel
	case level <= slog.LevelWarn:
		return zerolog.WarnLevel
	default:
		return zerolog.ErrorLevel
	}
}
