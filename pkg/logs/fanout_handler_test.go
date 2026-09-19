package logs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var fanoutTestTime = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

func TestFanoutHandler_Handle(t *testing.T) {
	type args struct {
		ctx    context.Context
		record slog.Record
	}

	tests := []struct {
		name        string
		wantBuf1    string
		wantBuf2    string
		args        args
		wantHandled bool
	}{
		{
			name: "Ok - broadcasts to all handlers",
			args: args{
				ctx:    context.Background(),
				record: slog.NewRecord(fanoutTestTime, slog.LevelInfo, "test message", 0),
			},
			wantBuf1:    `"test message"`,
			wantBuf2:    `"test message"`,
			wantHandled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf1 := &bytes.Buffer{}
			buf2 := &bytes.Buffer{}

			h1 := slog.NewJSONHandler(buf1, &slog.HandlerOptions{})
			h2 := slog.NewJSONHandler(buf2, &slog.HandlerOptions{})

			fanout := NewFanoutHandler(h1, h2)
			err := fanout.Handle(tt.args.ctx, tt.args.record)
			if err != nil {
				t.Fatalf("Handle() unexpected error: %v", err)
			}

			if !strings.Contains(buf1.String(), tt.wantBuf1) {
				t.Errorf("handler 1 output = %v, want contains %v", buf1.String(), tt.wantBuf1)
			}
			if !strings.Contains(buf2.String(), tt.wantBuf2) {
				t.Errorf("handler 2 output = %v, want contains %v", buf2.String(), tt.wantBuf2)
			}
		})
	}
}

func TestFanoutHandler_Enabled(t *testing.T) {
	tests := []struct {
		name  string
		level slog.Level
		want  bool
	}{
		{
			name:  "Ok - Info enabled when one handler accepts Info",
			level: slog.LevelInfo,
			want:  true,
		},
		{
			name:  "Ok - Debug disabled when all handlers require Info",
			level: slog.LevelDebug,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
			fanout := NewFanoutHandler(h)

			got := fanout.Enabled(context.Background(), tt.level)
			if got != tt.want {
				t.Errorf("Enabled(%v) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestFanoutHandler_WithAttrs(t *testing.T) {
	buf1 := &bytes.Buffer{}
	buf2 := &bytes.Buffer{}

	h1 := slog.NewJSONHandler(buf1, &slog.HandlerOptions{})
	h2 := slog.NewJSONHandler(buf2, &slog.HandlerOptions{})

	fanout := NewFanoutHandler(h1, h2)
	withAttrs := fanout.WithAttrs([]slog.Attr{slog.String("service", "test-svc")})

	record := slog.NewRecord(fanoutTestTime, slog.LevelInfo, "attr test", 0)
	err := withAttrs.Handle(context.Background(), record)
	if err != nil {
		t.Fatalf("Handle() unexpected error: %v", err)
	}

	if !strings.Contains(buf1.String(), `"service":"test-svc"`) {
		t.Errorf("handler 1 output missing attr, got: %v", buf1.String())
	}
	if !strings.Contains(buf2.String(), `"service":"test-svc"`) {
		t.Errorf("handler 2 output missing attr, got: %v", buf2.String())
	}
}

func TestFanoutHandler_WithGroup(t *testing.T) {
	buf1 := &bytes.Buffer{}
	buf2 := &bytes.Buffer{}

	h1 := slog.NewJSONHandler(buf1, &slog.HandlerOptions{})
	h2 := slog.NewJSONHandler(buf2, &slog.HandlerOptions{})

	fanout := NewFanoutHandler(h1, h2)
	withGroup := fanout.WithGroup("my_group")

	// Add an attr so group nesting is visible
	withGroupAndAttr := withGroup.WithAttrs([]slog.Attr{slog.String("key", "val")})

	record := slog.NewRecord(fanoutTestTime, slog.LevelInfo, "group test", 0)
	err := withGroupAndAttr.Handle(context.Background(), record)
	if err != nil {
		t.Fatalf("Handle() unexpected error: %v", err)
	}

	if !strings.Contains(buf1.String(), `"my_group"`) {
		t.Errorf("handler 1 output missing group, got: %v", buf1.String())
	}
	if !strings.Contains(buf2.String(), `"my_group"`) {
		t.Errorf("handler 2 output missing group, got: %v", buf2.String())
	}
}

func TestFanoutHandler_Handle_SkipsDisabledHandler(t *testing.T) {
	buf1 := &bytes.Buffer{}
	buf2 := &bytes.Buffer{}

	// h1 accepts Debug+, h2 only accepts Warn+
	h1 := slog.NewJSONHandler(buf1, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := slog.NewJSONHandler(buf2, &slog.HandlerOptions{Level: slog.LevelWarn})

	fanout := NewFanoutHandler(h1, h2)

	record := slog.NewRecord(fanoutTestTime, slog.LevelInfo, "info only", 0)
	err := fanout.Handle(context.Background(), record)
	if err != nil {
		t.Fatalf("Handle() unexpected error: %v", err)
	}

	if !strings.Contains(buf1.String(), "info only") {
		t.Errorf("handler 1 should have received the record, got: %v", buf1.String())
	}
	if buf2.Len() != 0 {
		t.Errorf("handler 2 should not have received the record, got: %v", buf2.String())
	}
}

type mockErrHandler struct {
	err error
}

func (m *mockErrHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (m *mockErrHandler) Handle(context.Context, slog.Record) error { return m.err }
func (m *mockErrHandler) WithAttrs([]slog.Attr) slog.Handler        { return m }
func (m *mockErrHandler) WithGroup(string) slog.Handler             { return m }

func TestFanoutHandler_Handle_ReturnsError(t *testing.T) {
	expectedErr := context.Canceled
	h1 := &mockErrHandler{err: expectedErr}

	buf2 := new(bytes.Buffer)
	h2 := slog.NewJSONHandler(buf2, nil)

	fanout := NewFanoutHandler(h1, h2)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "test error log", 0)

	err := fanout.Handle(context.Background(), record)
	if !errors.Is(err, expectedErr) {
		t.Errorf("Handle() error = %v, want %v", err, expectedErr)
	}

	if buf2.Len() == 0 {
		t.Errorf("h2 should have received the log record despite h1 failing")
	}
}
