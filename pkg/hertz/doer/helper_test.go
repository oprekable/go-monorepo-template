package doer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.9.0"
	"go.opentelemetry.io/otel/trace"
)

var testTime = time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)

type MockTimeHandler struct {
	slog.Handler
	t *testing.T
}

func NewMockTimeHandler(next slog.Handler, tT *testing.T) *MockTimeHandler {
	return &MockTimeHandler{
		t:       tT,
		Handler: next,
	}
}

func (h *MockTimeHandler) Handle(ctx context.Context, r slog.Record) error {
	h.t.Helper()
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().IsValid() {
		r.AddAttrs(slog.String("trace_id", span.SpanContext().TraceID().String()))
		r.AddAttrs(slog.String("span_id", span.SpanContext().SpanID().String()))
	}

	return h.Handler.Handle(ctx, r)
}

func (h *MockTimeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.t.Helper()
	return &MockTimeHandler{Handler: h.Handler.WithAttrs(attrs), t: h.t}
}

func (h *MockTimeHandler) WithGroup(name string) slog.Handler {
	h.t.Helper()
	return &MockTimeHandler{Handler: h.Handler.WithGroup(name), t: h.t}
}

// Mock Tracer
func getTestTracer(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
	t.Helper()
	res, _ := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String("test-service"),
		),
	)

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	tr := tp.Tracer("test-tracer")
	ctx, span := tr.Start(t.Context(), "test-span")

	// Shutdown TracerProvider when test finishes
	// Note: span.End() is now called manually in the test before reading spans
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})

	return ctx, span, exporter
}

func getLogger(t *testing.T, w io.Writer, mockTime time.Time) *slog.Logger {
	t.Helper()
	programLevel := new(slog.LevelVar)
	programLevel.Set(slog.LevelDebug)

	opts := &slog.HandlerOptions{
		Level: programLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, mockTime)
			}
			return a
		},
	}

	handler := NewMockTimeHandler(slog.NewJSONHandler(w, opts), t)
	slogLogger := slog.New(handler)
	slog.SetDefault(slogLogger)
	return slogLogger
}

func Test_defaultURLSanitizer(t *testing.T) {
	type args struct {
		rawURL string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "valid_url_with_query_and_fragment",
			args: args{
				rawURL: "https://api.example.com/users/123?token=secret&key=value#section",
			},
			want: "https://api.example.com/users/123",
		},
		{
			name: "valid_url_with_only_query",
			args: args{
				rawURL: "https://api.example.com/orders?page=1&limit=10",
			},
			want: "https://api.example.com/orders",
		},
		{
			name: "valid_url_with_only_fragment",
			args: args{
				rawURL: "https://api.example.com/docs#introduction",
			},
			want: "https://api.example.com/docs",
		},
		{
			name: "valid_url_without_query_or_fragment",
			args: args{
				rawURL: "https://api.example.com/health",
			},
			want: "https://api.example.com/health",
		},
		{
			name: "http_scheme",
			args: args{
				rawURL: "http://localhost:8080/api/v1/status?debug=true",
			},
			want: "http://localhost:8080/api/v1/status",
		},
		{
			name: "invalid_url",
			args: args{
				rawURL: "://invalid-url",
			},
			want: "/_invalid_url",
		},
		{
			name: "malformed_url",
			args: args{
				rawURL: "not a url at all",
			},
			want: "not%20a%20url%20at%20all",
		},
		{
			name: "empty_string",
			args: args{
				rawURL: "",
			},
			want: "",
		},
		{
			name: "url_with_port",
			args: args{
				rawURL: "https://api.example.com:443/resource?filter=active",
			},
			want: "https://api.example.com:443/resource",
		},
		{
			name: "url_with_username_password_is_stripped",
			args: args{
				rawURL: "https://user:pass@api.example.com/secure?token=xyz",
			},
			want: "https://api.example.com/secure",
		},
		{
			name: "url_with_username_password_but_without_scheme_is_stripped",
			args: args{
				rawURL: "user:pass@api.example.com/secure?token=xyz",
			},
			want: "user:api.example.com/secure",
		},
		{
			name: "url_with_opaque",
			args: args{
				rawURL: "scheme:example.com/path",
			},
			want: "scheme:example.com/path",
		},
		{
			name: "opaque_with_query_param_leak",
			args: args{
				rawURL: "scheme:example.com/path?token=secret123",
			},
			want: "scheme:example.com/path",
		},
		{
			name: "opaque_with_fragment_leak",
			args: args{
				rawURL: "scheme:example.com/path#section",
			},
			want: "scheme:example.com/path",
		},
		{
			name: "opaque_with_query_and_fragment_leak",
			args: args{
				rawURL: "scheme:example.com/path?token=secret#s1",
			},
			want: "scheme:example.com/path",
		},
		{
			name: "opaque_with_credentials_and_query",
			args: args{
				rawURL: "scheme:user:pass@example.com/path?token=secret",
			},
			want: "scheme:example.com/path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, _ := url.Parse(tt.args.rawURL)
			if got := defaultURLSanitizer(u); got != tt.want {
				t.Errorf("defaultURLSanitizer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_logSuccess(t *testing.T) {
	type args struct {
		buf        *bytes.Buffer
		mockTracer func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter)
		mockLogger func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger)
		message    string
		url        string
		method     string
		route      string
		duration   time.Duration
		slogLevel  slog.Level
		status     int
	}
	tests := []struct {
		name           string
		args           args
		wantSpanStatus codes.Code
	}{
		{
			name: "success_with_logger_and_span",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return getTestTracer(t)
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return getLogger(t, bf, tm)
				},
				duration:  50 * time.Millisecond,
				message:   "HTTP Outbound Success",
				slogLevel: slog.LevelDebug,
				url:       "https://api.example.com/users",
				method:    "GET",
				route:     "/users",
				status:    200,
			},
			wantSpanStatus: codes.Ok,
		},
		{
			name: "success_created_status",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return getTestTracer(t)
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return getLogger(t, bf, tm)
				},
				duration:  120 * time.Millisecond,
				message:   "Resource created",
				slogLevel: slog.LevelInfo,
				url:       "https://api.example.com/users",
				method:    "POST",
				route:     "/users",
				status:    201,
			},
			wantSpanStatus: codes.Ok,
		},
		{
			name: "success_without_logger",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return getTestTracer(t)
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return nil
				},
				duration:  30 * time.Millisecond,
				message:   "Success",
				slogLevel: slog.LevelDebug,
				url:       "https://api.example.com/health",
				method:    "GET",
				route:     "/health",
				status:    200,
			},
			wantSpanStatus: codes.Ok,
		},
		{
			name: "success_without_span",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return context.Background(), nil, nil
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return getLogger(t, bf, tm)
				},
				duration:  75 * time.Millisecond,
				message:   "Update successful",
				slogLevel: slog.LevelInfo,
				url:       "https://api.example.com/orders/123",
				method:    "PUT",
				route:     "/orders/{id}",
				status:    200,
			},
			wantSpanStatus: codes.Unset,
		},
		{
			name: "success_with_nil_logger_and_span",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return context.Background(), nil, nil
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return nil
				},
				duration:  10 * time.Millisecond,
				message:   "Deleted",
				slogLevel: slog.LevelInfo,
				url:       "https://api.example.com/cache",
				method:    "DELETE",
				route:     "/cache",
				status:    204,
			},
			wantSpanStatus: codes.Unset,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger := tt.args.mockLogger(t, tt.args.buf, testTime)
			ctx, span, exporter := tt.args.mockTracer(t)

			// Construct HertzDoer directly to test logSuccess without group wrapping
			d := &HertzDoer{
				logger:          logger,
				successLogLevel: tt.args.slogLevel,
			}

			d.logSuccess(
				ctx,
				span,
				tt.args.duration,
				tt.args.message,
				tt.args.url,
				tt.args.method,
				tt.args.route,
				tt.args.status,
			)

			// End the span to flush it to the exporter
			if span != nil {
				span.End()
			}

			defer tt.args.buf.Reset()

			dest := make(map[string]any)
			var spans tracetest.SpanStubs

			if exporter != nil {
				spans = exporter.GetSpans()
			}

			if logger != nil && len(spans) > 0 {
				_ = sonic.Unmarshal(tt.args.buf.Bytes(), &dest)
				assert.Equal(t, dest["time"], testTime.Format("2006-01-02T15:04:05Z07:00"))
				assert.Equal(t, dest["trace_id"], spans[0].SpanContext.TraceID().String())
				assert.Equal(t, dest["span_id"], spans[0].SpanContext.SpanID().String())
			}

			if len(spans) > 0 {
				assert.Equal(t, tt.wantSpanStatus, spans[0].Status.Code)

				for _, v := range spans[0].Attributes {
					key := string(v.Key)

					if val, ok := dest[key]; ok {
						if strVal, isString := val.(string); isString {
							assert.Equal(t, strVal, v.Value.AsString())
						}

						if boolVal, isBool := val.(bool); isBool {
							assert.Equal(t, boolVal, v.Value.AsBool())
						}

						if intVal, isInt := val.(int); isInt {
							assert.Equal(t, intVal, v.Value.AsInt64())
						}
					}
				}
			}
		})
	}
}

func Test_releaseHertzResources(t *testing.T) {
	testErr := errors.New("test cleanup error")
	tests := []struct {
		getArgs     func(t *testing.T, buf *bytes.Buffer) cleanupArgs
		checkOutput func(t *testing.T, buf *bytes.Buffer, logger *slog.Logger)
		name        string
	}{
		{
			name: "release_with_valid_request_and_response_no_error",
			getArgs: func(t *testing.T, buf *bytes.Buffer) cleanupArgs {
				return cleanupArgs{
					req:    protocol.AcquireRequest(),
					res:    protocol.AcquireResponse(),
					logger: getLogger(t, buf, testTime),
					ctx:    context.Background(),
					err:    nil,
				}
			},
			checkOutput: func(t *testing.T, buf *bytes.Buffer, logger *slog.Logger) {
				assert.Zero(t, buf.Len(), "Logger should not have been called")
			},
		},
		{
			name: "release_with_error_and_logger",
			getArgs: func(t *testing.T, buf *bytes.Buffer) cleanupArgs {
				return cleanupArgs{
					req:    protocol.AcquireRequest(),
					res:    protocol.AcquireResponse(),
					logger: getLogger(t, buf, testTime),
					ctx:    context.Background(),
					err:    testErr,
				}
			},
			checkOutput: func(t *testing.T, buf *bytes.Buffer, logger *slog.Logger) {
				dest := make(map[string]any)
				if logger != nil {
					_ = sonic.Unmarshal(buf.Bytes(), &dest)

					assert.Equal(t, dest["time"], testTime.Format("2006-01-02T15:04:05Z07:00"))
					assert.Equal(t, dest["level"], "WARN")
					assert.Equal(t, dest["msg"], "hertz resource released")
					assert.Equal(t, dest["hertz.resources.release.error"], "test cleanup error")
				}
			},
		},
		{
			name: "release_with_nil_context",
			getArgs: func(t *testing.T, buf *bytes.Buffer) cleanupArgs {
				return cleanupArgs{
					req:    protocol.AcquireRequest(),
					res:    protocol.AcquireResponse(),
					logger: getLogger(t, buf, testTime),
					ctx:    nil,
					err:    testErr,
				}
			},
			checkOutput: func(t *testing.T, buf *bytes.Buffer, logger *slog.Logger) {
				dest := make(map[string]any)
				if logger != nil {
					_ = sonic.Unmarshal(buf.Bytes(), &dest)

					assert.Equal(t, dest["time"], testTime.Format("2006-01-02T15:04:05Z07:00"))
					assert.Equal(t, dest["level"], "WARN")
					assert.Equal(t, dest["msg"], "hertz resource released")
					assert.Equal(t, dest["hertz.resources.release.error"], "test cleanup error")
				}
			},
		},
		{
			name: "release_with_nil_logger",
			getArgs: func(t *testing.T, buf *bytes.Buffer) cleanupArgs {
				return cleanupArgs{
					req:    protocol.AcquireRequest(),
					res:    protocol.AcquireResponse(),
					logger: nil,
					ctx:    context.Background(),
					err:    testErr,
				}
			},
			checkOutput: func(t *testing.T, buf *bytes.Buffer, logger *slog.Logger) {
				assert.Zero(t, buf.Len(), "Logger should not have been called, and no panic should occur")
			},
		},
		{
			name: "release_with_all_nil",
			getArgs: func(t *testing.T, buf *bytes.Buffer) cleanupArgs {
				return cleanupArgs{
					req:    nil,
					res:    nil,
					logger: nil,
					ctx:    nil,
					err:    nil,
				}
			},
			checkOutput: func(t *testing.T, buf *bytes.Buffer, logger *slog.Logger) {
				assert.Zero(t, buf.Len(), "Should not panic or log anything")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			buf := &bytes.Buffer{}
			cleanup := tt.getArgs(t, buf)
			defer buf.Reset()

			assert.NotPanics(t, func() {
				releaseHertzResources(cleanup)
			}, "releaseHertzResources should not panic")

			if tt.checkOutput != nil {
				tt.checkOutput(t, buf, cleanup.logger)
			}
		})
	}
}

func Test_routeLabel(t *testing.T) {
	type args struct {
		req        *http.Request
		routeNamer func(*http.Request) string
		routeLimit int
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "normal_route",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: "/api/users"},
				},
				routeNamer: nil,
			},
			want: "/api/users",
		},
		{
			name: "route_with_custom_namer",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: "/api/users/123"},
				},
				routeNamer: func(req *http.Request) string {
					return "/api/users/{id}"
				},
			},
			want: "/api/users/{id}",
		},
		{
			name: "route_namer_returns_empty",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: "/api/orders"},
				},
				routeNamer: func(req *http.Request) string {
					return ""
				},
			},
			want: "/api/orders",
		},
		{
			name: "route_exceeds_limit",
			args: args{
				routeLimit: 10,
				req: &http.Request{
					URL: &url.URL{Path: "/api/very/long/route/that/exceeds/limit"},
				},
				routeNamer: nil,
			},
			want: "/_unknown",
		},
		{
			name: "route_with_newline_injection",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: "/api/inject\nmalicious"},
				},
				routeNamer: nil,
			},
			want: "/_invalid",
		},
		{
			name: "route_with_carriage_return_injection",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: "/api/inject\rmalicious"},
				},
				routeNamer: nil,
			},
			want: "/_invalid",
		},
		{
			name: "route_limit_zero_defaults_to_100",
			args: args{
				routeLimit: 0,
				req: &http.Request{
					URL: &url.URL{Path: "/api/test"},
				},
				routeNamer: nil,
			},
			want: "/api/test",
		},
		{
			name: "route_limit_negative_defaults_to_100",
			args: args{
				routeLimit: -1,
				req: &http.Request{
					URL: &url.URL{Path: "/health"},
				},
				routeNamer: nil,
			},
			want: "/health",
		},
		{
			name: "empty_route",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: ""},
				},
				routeNamer: nil,
			},
			want: "",
		},
		{
			name: "root_route",
			args: args{
				routeLimit: 100,
				req: &http.Request{
					URL: &url.URL{Path: "/"},
				},
				routeNamer: nil,
			},
			want: "/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeLabel(tt.args.routeLimit, tt.args.req, tt.args.routeNamer); got != tt.want {
				t.Errorf("routeLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_statusToString(t *testing.T) {
	type args struct {
		code int
	}
	tests := []struct {
		name string
		want string
		args args
	}{
		{
			name: "status_200",
			args: args{code: 200},
			want: "200",
		},
		{
			name: "status_201",
			args: args{code: 201},
			want: "201",
		},
		{
			name: "status_204",
			args: args{code: 204},
			want: "204",
		},
		{
			name: "status_400",
			args: args{code: 400},
			want: "400",
		},
		{
			name: "status_401",
			args: args{code: 401},
			want: "401",
		},
		{
			name: "status_404",
			args: args{code: 404},
			want: "404",
		},
		{
			name: "status_500",
			args: args{code: 500},
			want: "500",
		},
		{
			name: "status_503",
			args: args{code: 503},
			want: "503",
		},
		{
			name: "status_0",
			args: args{code: 0},
			want: "0",
		},
		{
			name: "status_599_boundary",
			args: args{code: 599},
			want: "599",
		},
		{
			name: "status_600_out_of_cache",
			args: args{code: 600},
			want: "600",
		},
		{
			name: "status_negative",
			args: args{code: -1},
			want: "-1",
		},
		{
			name: "status_large_number",
			args: args{code: 9999},
			want: "9999",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := statusToString(tt.args.code); got != tt.want {
				t.Errorf("statusToString() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test_statusToString_uncommonCacheHit covers the uncommonStatusCodeCache.Load
// path (helper.go lines 56-58): first call populates the cache, second call
// must return the cached value. Runs sequentially to guarantee deterministic
// cache state.
func Test_statusToString_uncommonCacheHit(t *testing.T) {
	code := 418 // uncommon, not in statusCodeStrings
	want := "418"

	// First call: miss both caches, converts via strconv and stores in uncommon cache.
	if got := statusToString(code); got != want {
		t.Fatalf("first call: statusToString(%d) = %v, want %v", code, got, want)
	}

	// Second call: must hit uncommonStatusCodeCache (lines 56-58).
	if got := statusToString(code); got != want {
		t.Errorf("second call (cache hit): statusToString(%d) = %v, want %v", code, got, want)
	}

	// Verify the value actually lives in the uncommon cache.
	v, ok := uncommonStatusCodeCache.Load(code)
	if !ok {
		t.Fatal("expected code 418 to be present in uncommonStatusCodeCache")
	}
	if v.(string) != want {
		t.Errorf("uncommonStatusCodeCache[%d] = %v, want %v", code, v, want)
	}
}

func Test_cleanOpaque(t *testing.T) {
	tests := []struct {
		name   string
		opaque string
		want   string
	}{
		{"empty", "", ""},
		{"no_credentials", "example.com/path", "example.com/path"},
		{"with_credentials", "user:pass@example.com/path", "example.com/path"},
		{"credentials_only", "user:pass@", ""},
		{"with_query", "example.com/path?key=val", "example.com/path"},
		{"with_fragment", "example.com/path#section", "example.com/path"},
		{"with_query_and_fragment", "example.com/path?key=val#sec", "example.com/path"},
		{"credentials_with_query", "user:pass@host/path?token=x", "host/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanOpaque(tt.opaque); got != tt.want {
				t.Errorf("cleanOpaque(%q) = %q, want %q", tt.opaque, got, tt.want)
			}
		})
	}
}
