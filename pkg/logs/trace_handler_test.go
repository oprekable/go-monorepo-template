package logs

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

var testTime = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

func TestNewTraceHandler(t *testing.T) {
	type args struct {
		nextFunc func(b *bytes.Buffer) slog.Handler
		buf      *bytes.Buffer
		record   slog.Record
	}

	tests := []struct {
		name       string
		wantPrefix string
		args       args
	}{
		{
			name: "Ok",
			args: args{
				buf:    &bytes.Buffer{},
				record: slog.NewRecord(testTime, slog.LevelInfo, "a message", 0),
				nextFunc: func(b *bytes.Buffer) slog.Handler {
					return slog.NewTextHandler(b, &slog.HandlerOptions{})
				},
			},
			wantPrefix: `time=2000-01-02T03:04:05.000Z level=INFO msg="a message"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseHandler := NewTraceHandler(tt.args.nextFunc(tt.args.buf))
			_ = baseHandler.Handle(context.TODO(), tt.args.record)
			logString := tt.args.buf.String()
			defer tt.args.buf.Reset()
			if got := logString[:len(logString)-1]; !reflect.DeepEqual(got, tt.wantPrefix) {
				t.Errorf("NewTraceHandler() = %v, wantPrefix %v", got, tt.wantPrefix)
			}
		})
	}
}

func TestTraceHandler_Handle(t *testing.T) {
	type args struct {
		ctx      context.Context
		nextFunc func(b *bytes.Buffer) slog.Handler
		buf      *bytes.Buffer
		record   slog.Record
	}

	tests := []struct {
		name       string
		wantPrefix string
		args       args
	}{
		{
			name: "Ok - With Span",
			args: args{
				buf:    &bytes.Buffer{},
				record: slog.NewRecord(testTime, slog.LevelInfo, "a message", 0),
				ctx: func() context.Context {
					traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
					spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")

					sc := trace.NewSpanContext(trace.SpanContextConfig{
						TraceID:    traceID,
						SpanID:     spanID,
						TraceFlags: trace.FlagsSampled,
					})

					return trace.ContextWithSpanContext(context.Background(), sc)
				}(),
				nextFunc: func(b *bytes.Buffer) slog.Handler {
					return slog.NewTextHandler(b, &slog.HandlerOptions{})
				},
			},
			wantPrefix: `time=2000-01-02T03:04:05.000Z level=INFO msg="a message" trace_id=4bf92f3577b34da6a3ce929d0e0e4736 span_id=00f067aa0ba902b7`,
		},
		{
			name: "Ok - No Span",
			args: args{
				buf:    &bytes.Buffer{},
				record: slog.NewRecord(testTime, slog.LevelInfo, "a message", 0),
				ctx: func() context.Context {
					return context.Background()
				}(),
				nextFunc: func(b *bytes.Buffer) slog.Handler {
					return slog.NewTextHandler(b, &slog.HandlerOptions{})
				},
			},
			wantPrefix: `time=2000-01-02T03:04:05.000Z level=INFO msg="a message"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseHandler := NewTraceHandler(tt.args.nextFunc(tt.args.buf))
			_ = baseHandler.Handle(tt.args.ctx, tt.args.record)
			logString := tt.args.buf.String()
			defer tt.args.buf.Reset()
			if got := logString[:len(logString)-1]; !reflect.DeepEqual(got, tt.wantPrefix) {
				t.Errorf("NewTraceHandler() = %v, wantPrefix %v", got, tt.wantPrefix)
			}
		})
	}
}

func TestNewTraceHandler_WithAttrs(t *testing.T) {
	type args struct {
		nextFunc func(b *bytes.Buffer) slog.Handler
		buf      *bytes.Buffer
		msg      string
		attrs    []any
	}

	tests := []struct {
		name       string
		wantPrefix string
		args       args
	}{
		{
			name: "Ok",
			args: args{
				buf:   &bytes.Buffer{},
				msg:   "a message",
				attrs: []any{"common_key", "common_value"},
				nextFunc: func(b *bytes.Buffer) slog.Handler {
					return slog.NewTextHandler(b, &slog.HandlerOptions{
						ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
							if a.Key == slog.TimeKey {
								return slog.Attr{}
							}
							return a
						},
					})
				},
			},
			wantPrefix: `level=INFO msg="a message" common_key=common_value`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseHandler := NewTraceHandler(tt.args.nextFunc(tt.args.buf))
			logger := slog.New(baseHandler).With(tt.args.attrs...)
			logger.Info(tt.args.msg)
			logString := tt.args.buf.String()
			defer tt.args.buf.Reset()
			if got := logString[:len(logString)-1]; !reflect.DeepEqual(got, tt.wantPrefix) {
				t.Errorf("NewTraceHandler.WithAttrs() = %v, wantPrefix %v", got, tt.wantPrefix)
			}
		})
	}
}

func TestNewTraceHandler_WithGroup(t *testing.T) {
	type args struct {
		nextFunc func(b *bytes.Buffer) slog.Handler
		buf      *bytes.Buffer
		group    string
		msg      string
		attrs    []any
	}

	tests := []struct {
		name       string
		wantPrefix string
		args       args
	}{
		{
			name: "Ok",
			args: args{
				buf:   &bytes.Buffer{},
				msg:   "a message",
				attrs: []any{"common_key", "common_value"},
				group: "my_group",
				nextFunc: func(b *bytes.Buffer) slog.Handler {
					return slog.NewTextHandler(b, &slog.HandlerOptions{
						ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
							if a.Key == slog.TimeKey {
								return slog.Attr{}
							}
							return a
						},
					})
				},
			},
			wantPrefix: `level=INFO msg="a message" my_group.common_key=common_value`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseHandler := NewTraceHandler(tt.args.nextFunc(tt.args.buf))
			logger := slog.New(baseHandler).WithGroup(tt.args.group).With(tt.args.attrs...)
			logger.Info(tt.args.msg)
			logString := tt.args.buf.String()
			defer tt.args.buf.Reset()
			if got := logString[:len(logString)-1]; !reflect.DeepEqual(got, tt.wantPrefix) {
				t.Errorf("NewTraceHandler.WithGroup() = %v, wantPrefix %v", got, tt.wantPrefix)
			}
		})
	}
}
