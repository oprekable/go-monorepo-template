package logs

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/rs/zerolog"
)

func TestMapSlogLevelToZerolog(t *testing.T) {
	tests := []struct {
		name     string
		input    slog.Level
		expected zerolog.Level
	}{
		{"Debug", slog.LevelDebug, zerolog.DebugLevel},
		{"Info", slog.LevelInfo, zerolog.InfoLevel},
		{"Warn", slog.LevelWarn, zerolog.WarnLevel},
		{"Error", slog.LevelError, zerolog.ErrorLevel},
		{"Unknown", slog.Level(999), zerolog.ErrorLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapSlogLevelToZerolog(tt.input)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestNewZerologOtelLogger(t *testing.T) {
	t.Run("with explicit writer", func(t *testing.T) {
		var buf bytes.Buffer
		logger := NewZerologOtelLogger(LoggerConfig{
			ServiceName:   "test-svc",
			ZerologWriter: &buf,
			Level:         slog.LevelInfo,
		})

		if logger == nil {
			t.Fatal("Expected logger to be non-nil")
		}

		logger.Info("test message")
		if buf.Len() == 0 {
			t.Error("Expected log to be written to writer")
		}
	})

	t.Run("with nil writer", func(t *testing.T) {
		logger := NewZerologOtelLogger(LoggerConfig{
			ServiceName: "test-svc-stderr",
		})
		if logger == nil {
			t.Fatal("Expected logger to be non-nil")
		}
	})
}
