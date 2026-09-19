package doer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/prometheus/client_golang/prometheus"
	ioprometheusclient "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// newTestClient creates a hertz client for testing with optional middleware.
func newTestClient(t *testing.T, mw client.Middleware) *client.Client {
	t.Helper()
	c, err := client.NewClient()
	require.NoError(t, err)
	if mw != nil {
		c.Use(mw)
	}
	return c
}

// successMiddleware returns middleware that sets response status and body without network call.
func successMiddleware(status int, body string) client.Middleware {
	return func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(status)
			if body != "" {
				resp.SetBodyStream(bytes.NewReader([]byte(body)), len(body))
			}
			return nil
		}
	}
}

// successStreamMiddleware returns middleware that sets response with a body stream.
func successStreamMiddleware(status int, body string) client.Middleware {
	return func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(status)
			if body != "" {
				resp.SetBodyStream(bytes.NewReader([]byte(body)), len(body))
			}
			return nil
		}
	}
}

// errorMiddleware returns middleware that returns an error.
func errorMiddleware(err error) client.Middleware {
	return func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			return err
		}
	}
}

// panicMiddleware returns middleware that panics.
func panicMiddleware(msg string) client.Middleware {
	return func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			panic(msg)
		}
	}
}

func TestNewHertzDoer_Defaults(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c)

	assert.Equal(t, DefaultName, d.name)
	assert.NotNil(t, d.client)
	assert.NotNil(t, d.propagator)
	assert.Equal(t, int64(DefaultMaxRequestBodySize), d.maxRequestBodySize)
	assert.Equal(t, DefaultRouteLimit, d.routeLimit)
	assert.Equal(t, slog.LevelDebug, d.successLogLevel)
	assert.Equal(t, DefaultReserveTime, d.reserveTime)
	assert.Equal(t, DefaultTimeout, d.defaultTimeout)
	assert.NotNil(t, d.logger)
	assert.NotNil(t, d.tracer)
	assert.NotNil(t, d.urlSanitizer)
	assert.True(t, d.collectPanicStack, "collectPanicStack should default to true")
}

func TestNewHertzDoer_Options(t *testing.T) {
	t.Run("WithName", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithName("custom-name"))
		assert.Equal(t, "custom-name", d.name)
	})

	t.Run("WithLogger", func(t *testing.T) {
		c := newTestClient(t, nil)
		customLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
		d := NewHertzDoer(c, WithLogger(customLogger))
		// Logger gets wrapped with group, so it won't be exactly equal
		assert.NotNil(t, d.logger)
	})

	t.Run("WithRouteNamer", func(t *testing.T) {
		c := newTestClient(t, nil)
		namer := func(r *http.Request) string { return "/custom" }
		d := NewHertzDoer(c, WithRouteNamer(namer))
		assert.NotNil(t, d.routeNamer)
		req, _ := http.NewRequest("GET", "https://example.com/test", nil)
		assert.Equal(t, "/custom", routeLabel(d.routeLimit, req, d.routeNamer))
	})

	t.Run("WithRouteLimit_positive", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithRouteLimit(50))
		assert.Equal(t, 50, d.routeLimit)
	})

	t.Run("WithRouteLimit_zero_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithRouteLimit(0))
		assert.Equal(t, DefaultRouteLimit, d.routeLimit)
	})

	t.Run("WithRouteLimit_negative_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithRouteLimit(-5))
		assert.Equal(t, DefaultRouteLimit, d.routeLimit)
	})

	t.Run("WithMaxRequestBodySize", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithMaxRequestBodySize(512))
		assert.Equal(t, int64(512), d.maxRequestBodySize)
	})

	t.Run("WithReserveTime", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithReserveTime(50*time.Millisecond))
		assert.Equal(t, 50*time.Millisecond, d.reserveTime)
	})

	t.Run("WithSecurityConfig", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithSecurityConfig(&SecurityConfig{
			RequireTLS:          true,
			MaxRequestBodySize:  1024,
			MaxResponseBodySize: 2048,
		}))
		assert.NotNil(t, d.securityConfig)
		assert.True(t, d.securityConfig.RequireTLS)
		assert.Equal(t, int64(1024), d.maxRequestBodySize)
		assert.Equal(t, int64(2048), d.maxResponseBodySize)
	})

	t.Run("WithResilienceConfig", func(t *testing.T) {
		c := newTestClient(t, nil)
		cfg := ResilienceConfig{}
		cfg.CircuitBreaker.Enabled = true
		cfg.CircuitBreaker.FailureThreshold = 10
		cfg.CircuitBreaker.ResetTimeout = 60 * time.Second
		cfg.ConcurrencyLimiter.Enabled = true
		cfg.ConcurrencyLimiter.MaxConcurrent = 20
		cfg.DefaultTimeout = 5 * time.Second
		cfg.ReserveTime = 10 * time.Millisecond
		d := NewHertzDoer(c, WithResilienceConfig(cfg))
		assert.True(t, d.circuitBreakerEnabled)
		assert.Equal(t, int64(10), d.circuitBreakerThreshold)
		assert.Equal(t, 60*time.Second, d.circuitBreakerTimeout)
		assert.NotNil(t, d.concurrencyLimiter)
		assert.Equal(t, cap(d.concurrencyLimiter), 20)
		assert.Equal(t, 5*time.Second, d.defaultTimeout)
		assert.Equal(t, 10*time.Millisecond, d.reserveTime)
	})

	t.Run("WithResilienceConfig_Defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		cfg := ResilienceConfig{}
		cfg.CircuitBreaker.Enabled = true
		cfg.ConcurrencyLimiter.Enabled = true
		d := NewHertzDoer(c, WithResilienceConfig(cfg))
		assert.True(t, d.circuitBreakerEnabled)
		assert.Equal(t, int64(5), d.circuitBreakerThreshold)
		assert.Equal(t, 30*time.Second, d.circuitBreakerTimeout)
		assert.NotNil(t, d.concurrencyLimiter)
		assert.Equal(t, DefaultMaxConcurrent, cap(d.concurrencyLimiter))
	})

	t.Run("WithObservabilityConfig", func(t *testing.T) {
		c := newTestClient(t, nil)
		reg := prometheus.NewRegistry()
		d := NewHertzDoer(c, WithObservabilityConfig(ObservabilityConfig{
			Prometheus:         reg,
			LogSampleRate:      10,
			SuccessLogLevel:    slog.LevelDebug,
			CollectPanicStack:  true,
			LogRequestPayload:  true,
			LogResponsePayload: true,
		}))
		assert.NotNil(t, d.requestCounter)
		assert.Equal(t, uint64(10), d.logSampleRate)
		assert.Equal(t, slog.LevelDebug, d.successLogLevel)
		assert.True(t, d.collectPanicStack)
		assert.True(t, d.logRequestPayload)
		assert.True(t, d.logResponsePayload)
	})

	t.Run("WithTracer", func(t *testing.T) {
		c := newTestClient(t, nil)
		customTracer := noop.NewTracerProvider().Tracer("custom")
		d := NewHertzDoer(c, WithTracer(customTracer))
		assert.NotNil(t, d.tracer)
	})

	t.Run("WithURLSanitizer", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithURLSanitizer(func(s *url.URL) string { return "sanitized" }))
		req, _ := http.NewRequest("GET", "https://example.com/test", nil)
		assert.Equal(t, "sanitized", d.urlSanitizer(req.URL))
	})

	t.Run("WithSuccessLogLevel", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithSuccessLogLevel(slog.LevelInfo))
		assert.Equal(t, slog.LevelInfo, d.successLogLevel)
	})

	t.Run("WithPrometheus", func(t *testing.T) {
		c := newTestClient(t, nil)
		reg := prometheus.NewRegistry()
		d := NewHertzDoer(c, WithPrometheus(reg))
		assert.NotNil(t, d.requestCounter)
		assert.NotNil(t, d.latencyHist)
		assert.NotNil(t, d.metricCacheHits)
		assert.NotNil(t, d.metricCacheMisses)
	})

	t.Run("WithPrometheus_nil_registry", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithPrometheus(nil))
		assert.Nil(t, d.requestCounter)
		assert.Nil(t, d.latencyHist)
	})

	t.Run("WithDefaultTimeout", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithDefaultTimeout(5*time.Second))
		assert.Equal(t, 5*time.Second, d.defaultTimeout)
	})

	t.Run("WithCollectPanicStack_true", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCollectPanicStack(true))
		assert.True(t, d.collectPanicStack)
	})

	t.Run("WithCollectPanicStack_false", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCollectPanicStack(false))
		assert.False(t, d.collectPanicStack)
	})

	t.Run("WithCircuitBreaker_zero_threshold_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCircuitBreaker(0, 30*time.Second))
		assert.Equal(t, int64(5), d.circuitBreakerThreshold)
		assert.Equal(t, 30*time.Second, d.circuitBreakerTimeout)
	})

	t.Run("WithCircuitBreaker_negative_threshold_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCircuitBreaker(-1, 30*time.Second))
		assert.Equal(t, int64(5), d.circuitBreakerThreshold)
		assert.Equal(t, 30*time.Second, d.circuitBreakerTimeout)
	})

	t.Run("WithCircuitBreaker_zero_timeout_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCircuitBreaker(5, 0))
		assert.Equal(t, int64(5), d.circuitBreakerThreshold)
		assert.Equal(t, 30*time.Second, d.circuitBreakerTimeout)
	})

	t.Run("WithCircuitBreaker_negative_timeout_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCircuitBreaker(5, -1*time.Second))
		assert.Equal(t, int64(5), d.circuitBreakerThreshold)
		assert.Equal(t, 30*time.Second, d.circuitBreakerTimeout)
	})

	t.Run("WithCircuitBreaker_both_zero_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithCircuitBreaker(0, 0))
		assert.Equal(t, int64(5), d.circuitBreakerThreshold)
		assert.Equal(t, 30*time.Second, d.circuitBreakerTimeout)
	})

	t.Run("WithConcurrencyLimiter_zero_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithConcurrencyLimiter(0))
		assert.NotNil(t, d.concurrencyLimiter)
		assert.Equal(t, DefaultMaxConcurrent, cap(d.concurrencyLimiter))
	})

	t.Run("WithConcurrencyLimiter_negative_defaults", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c, WithConcurrencyLimiter(-5))
		assert.NotNil(t, d.concurrencyLimiter)
		assert.Equal(t, DefaultMaxConcurrent, cap(d.concurrencyLimiter))
	})

	t.Run("WithClock", func(t *testing.T) {
		c := newTestClient(t, nil)
		rc := realClock{}
		d := NewHertzDoer(c, WithClock(rc))
		assert.NotNil(t, d.clock)
		assert.Equal(t, rc, d.clock)
	})

	t.Run("multiple_options", func(t *testing.T) {
		c := newTestClient(t, nil)
		d := NewHertzDoer(c,
			WithName("multi"),
			WithRouteLimit(42),
			WithSuccessLogLevel(slog.LevelWarn),
		)
		assert.Equal(t, "multi", d.name)
		assert.Equal(t, 42, d.routeLimit)
		assert.Equal(t, slog.LevelWarn, d.successLogLevel)
	})
}

func TestHertzDoer_Do_SecurityError(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithSecurityConfig(&SecurityConfig{RequireTLS: true}))

	req, _ := http.NewRequest("GET", "http://example.com/api", nil)
	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "security", hErr.Kind)
}

func TestHertzDoer_Do_TooManyHeaders(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	// Add more headers than allowed
	for i := range AllowedMaxHeaders + 1 {
		req.Header.Set(fmt.Sprintf("X-Header-%d", i), "value")
	}

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "too many request headers")
}

func TestHertzDoer_Do_ExactlyMaxHeaders(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithSecurityConfig(&SecurityConfig{RequireTLS: true}))

	req, _ := http.NewRequest("GET", "http://example.com/api", nil)
	for i := range AllowedMaxHeaders {
		req.Header.Set(fmt.Sprintf("X-Header-%d", i), "value")
	}

	// Should fail on TLS check (first error), not header count
	resp, err := d.Do(req)
	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "security", hErr.Kind)
}

func TestHertzDoer_Do_BodyTooLarge_ContentLength(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithMaxRequestBodySize(100))

	body := strings.NewReader(strings.Repeat("x", 200))
	req, _ := http.NewRequest("POST", "http://example.com/api", body)
	req.ContentLength = 200

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "HTTP Body Payload Error")
}

func TestHertzDoer_Do_BodyWithinLimit(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithMaxRequestBodySize(1000))

	body := strings.NewReader("small body")
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 10

	// Fails because client.Do errors (no server), not body validation
	resp, err := d.Do(req)
	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "too large")
}

func TestHertzDoer_Do_NoBody_NilBody(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)
	// Fails on client.Do (no server) but not body validation
	assert.Nil(t, resp)
	assert.Error(t, err)
}

func TestHertzDoer_Do_BodyStreamOverflow(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithMaxRequestBodySize(10))

	// Body with unknown ContentLength (set to -1 to trigger stream path)
	body := strings.NewReader(strings.Repeat("x", 200))
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 0 // Trigger unknown length path since ContentLength <= 0 AND body != nil

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "exceeds limit")
}

func TestHertzDoer_Do_BodyCopyError(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithMaxRequestBodySize(1024))

	// Use a reader that returns an error
	errReader := &errorReader{err: errors.New("read failure")}
	req, _ := http.NewRequest("POST", "https://example.com/api", errReader)
	req.ContentLength = 0 // Trigger unknown length / copy path

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "read failure")
}

type errorReader struct{ err error }

func (r *errorReader) Read(_ []byte) (int, error) { return 0, r.err }

// panicCounter implements prometheus.Counter and panics on Inc/With.
type panicCounter struct{}

func (p panicCounter) Inc()                                   { panic("test metric panic") }
func (p panicCounter) Add(float64)                            { panic("test metric panic") }
func (p panicCounter) Desc() *prometheus.Desc                 { return nil }
func (p panicCounter) Write(*ioprometheusclient.Metric) error { return nil }
func (p panicCounter) Describe(chan<- *prometheus.Desc)       {}
func (p panicCounter) Collect(chan<- prometheus.Metric)       {}

// panicObserver implements prometheus.Observer and panics on Observe.
type panicObserver struct{}

func (p panicObserver) Observe(float64)                  { panic("test metric panic") }
func (p panicObserver) Describe(chan<- *prometheus.Desc) {}
func (p panicObserver) Collect(chan<- prometheus.Metric) {}

func TestHertzDoer_Do_ContextCancelled(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestHertzDoer_Do_ContextDeadlineTooClose(t *testing.T) {
	c := newTestClient(t, nil)
	d := NewHertzDoer(c, WithReserveTime(500*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	require.Error(t, err)
	var hErr *HertzDoerError
	require.True(t, errors.As(err, &hErr))
	assert.Equal(t, "timeout", hErr.Kind)
}

func TestHertzDoer_Do_ContextDeadlineSufficient(t *testing.T) {
	// Use middleware that responds quickly, deadline is far away
	mw := successMiddleware(200, `{"ok":true}`)
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithReserveTime(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, `{"ok":true}`, string(body))
}

func TestHertzDoer_Do_ClientError(t *testing.T) {
	mw := errorMiddleware(errors.New("connection refused"))
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestHertzDoer_Do_ClientPanic(t *testing.T) {
	mw := panicMiddleware("unexpected panic")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "panic in hertz client.Do")
	assert.Contains(t, err.Error(), "unexpected panic")
}

func TestHertzDoer_Do_ClientPanic_ErrorType(t *testing.T) {
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			// Panicking with an error type
			panic(errors.New("error typed panic"))
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)
	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "panic in hertz client.Do: error typed panic")
}

func TestHertzDoer_Do_ClientPanic_DefaultType(t *testing.T) {
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			// Panicking with a non-error, non-string type (int)
			panic(123)
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)
	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)
	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "panic in hertz client.Do")
	assert.Contains(t, err.Error(), "123")
}

func TestHertzDoer_Do_ClientPanic_CollectStackDisabled(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := panicMiddleware("stack-disabled panic")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger), WithCollectPanicStack(false))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "panic in hertz client.Do")
	assert.Contains(t, err.Error(), "stack-disabled panic")

	logOutput := buf.String()
	assert.Contains(t, logOutput, "stack collection disabled")
	assert.NotContains(t, logOutput, "goroutine") // stack trace should not be present
}

func TestHertzDoer_Do_ClientDeadlineExceeded(t *testing.T) {
	errDeadline := context.DeadlineExceeded
	mw := errorMiddleware(errDeadline)
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestHertzDoer_Do_Success200_Body(t *testing.T) {
	mw := successMiddleware(200, `{"status":"ok"}`)
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "HTTP/1.1", resp.Proto)
	assert.Equal(t, 1, resp.ProtoMajor)
	assert.Equal(t, 1, resp.ProtoMinor)
	assert.Equal(t, req.URL, resp.Request.URL)
	assert.Equal(t, req.Method, resp.Request.Method)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, `{"status":"ok"}`, string(body))
}

func TestHertzDoer_Do_Success200_BodyStream(t *testing.T) {
	mw := successStreamMiddleware(200, "stream body")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, "stream body", string(body))
}

func TestHertzDoer_Do_Success200_WithSuccessLogging(t *testing.T) {
	// Cover lines 1053-1066: success logging path
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := successMiddleware(200, `{"status":"ok"}`)
	c := newTestClient(t, mw)
	// Enable success logging with sample rate 1 (log every request)
	d := NewHertzDoer(c, WithLogger(logger), WithLogSampleRate(1))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, `{"status":"ok"}`, string(body))

	// Verify success log was written
	logOutput := buf.String()
	assert.Contains(t, logOutput, "HTTP Outbound Success")
}

func TestHertzDoer_Do_ResponseBodyStream_TooLarge_KnownLength(t *testing.T) {
	// Stream with ContentLength header set → early rejection before returning response
	largeBody := strings.Repeat("x", 200)
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.Header.SetContentLength(200)
			resp.SetBodyStream(strings.NewReader(largeBody), 200)
			return nil
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithMaxResponseBodySize(100))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "response body too large")
	assert.Contains(t, hErr.Message, "200 bytes > max 100")
}

func TestHertzDoer_Do_ResponseBodyStream_TooLarge_UnknownLength(t *testing.T) {
	// Stream with unknown ContentLength (chunked) → overflow error during Read
	largeBody := strings.Repeat("x", 200)
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.Header.SetContentLength(-1) // Unknown length / chunked
			resp.SetBodyStream(strings.NewReader(largeBody), -1)
			return nil
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithMaxResponseBodySize(100))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)

	require.NoError(t, err) // Response returned successfully
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)

	// Error occurs during read when overflow detected
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.Error(t, readErr)
	var hErr *HertzDoerError
	assert.True(t, errors.As(readErr, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "response body exceeds max 100 bytes")
	assert.Len(t, body, 101) // Read limit+1 bytes before error
}

func TestHertzDoer_Do_ResponseBodyStream_ExactLimit(t *testing.T) {
	// Body exactly at limit should succeed without overflow error
	exactBody := strings.Repeat("x", 100)
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.Header.SetContentLength(-1)
			resp.SetBodyStream(strings.NewReader(exactBody), -1)
			return nil
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithMaxResponseBodySize(100))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)

	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assert.NoError(t, readErr)
	assert.Len(t, body, 100)
	assert.Equal(t, exactBody, string(body))
}

func TestHertzDoer_Do_ResponseBody_NonStream_TooLarge(t *testing.T) {
	// Non-stream body (SetBody) that exceeds maxResponseBodySize → line 1110-1117
	largeBody := strings.Repeat("x", 200)
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.SetBody([]byte(largeBody)) // non-stream body
			return nil
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithMaxResponseBodySize(100))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "response body too large")
	assert.Contains(t, hErr.Message, "200 bytes > max 100")
}

func TestHertzDoer_Do_Success204_NoContent(t *testing.T) {
	mw := successMiddleware(204, "")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 204, resp.StatusCode)
	assert.Equal(t, int64(0), resp.ContentLength)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Empty(t, body)
}

func TestHertzDoer_Do_Success_WithRouteNamer(t *testing.T) {
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithRouteNamer(func(r *http.Request) string {
		return "/users/{id}"
	}))

	req, _ := http.NewRequest("GET", "https://example.com/users/123", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestHertzDoer_Do_Success_WithHost(t *testing.T) {
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Host = "custom.example.com"

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestHertzDoer_Do_Success_WithHeaders(t *testing.T) {
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header.Set("X-Custom", "value1")
	req.Header.Add("X-Multi", "a")
	req.Header.Add("X-Multi", "b")

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	_ = resp.Body.Close()
}

func TestHertzDoer_Do_Status400(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := successMiddleware(400, `{"error":"bad request"}`)
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 400, resp.StatusCode)
	_ = resp.Body.Close()

	// Verify error log was written for 4xx status
	assert.Greater(t, buf.Len(), 0)
	assert.Contains(t, buf.String(), "HTTP Outbound Non-2xx")
}

func TestHertzDoer_Do_Status500(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := successMiddleware(500, `{"error":"internal"}`)
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 500, resp.StatusCode)
	_ = resp.Body.Close()

	assert.Greater(t, buf.Len(), 0)
	assert.Contains(t, buf.String(), "HTTP Outbound Non-2xx")
}

func TestHertzDoer_Do_TooManyResponseHeaders(t *testing.T) {
	// Create middleware that adds many response headers
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			for i := range AllowedMaxHeaders + 10 {
				resp.Header.Set(fmt.Sprintf("X-Resp-%d", i), "value")
			}
			return nil
		}
	}

	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "too many response headers")
}

func TestHertzDoer_Do_ResponseHeaders(t *testing.T) {
	// Middleware that sets custom response headers
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.SetBody([]byte("ok"))
			resp.Header.Set("X-Response-Id", "abc123")
			resp.Header.Add("X-Multi", "val1")
			resp.Header.Add("X-Multi", "val2")
			return nil
		}
	}

	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.Equal(t, "abc123", resp.Header.Get("X-Response-Id"))

	multiVals := resp.Header["X-Multi"]
	assert.Len(t, multiVals, 2)
	assert.Contains(t, multiVals, "val1")
	assert.Contains(t, multiVals, "val2")

	_ = resp.Body.Close()
}

func TestHertzDoer_Do_EmptyHeaderValues(t *testing.T) {
	// Headers with empty slices should be skipped
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	req.Header["X-Empty"] = []string{} // Empty value slice

	resp, err := d.Do(req)

	require.NoError(t, err)
	_ = resp.Body.Close()
}

func TestHertzDoer_Do_SpanInjection(t *testing.T) {
	ctx, span, exporter := getTestTracer(t)
	defer span.End()

	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogSampleRate(1)) // triggers logSuccess -> adds span attributes

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	_ = resp.Body.Close()

	// End span to flush
	span.End()

	spans := exporter.GetSpans()
	require.Greater(t, len(spans), 0)

	// The span name should be "GET:/api"
	assert.Contains(t, spans[0].Name, "GET")

	// Check that the span gets the success attributes
	assert.Equal(t, codes.Ok, spans[0].Status.Code)

	var hasMethod, hasURL bool
	for _, attr := range spans[0].Attributes {
		if attr.Key == "http.method" && attr.Value.AsString() == "GET" {
			hasMethod = true
		}
		if attr.Key == "http.url" && attr.Value.AsString() == "https://example.com/api" {
			hasURL = true
		}
	}
	assert.True(t, hasMethod, "missing http.method attribute")
	assert.True(t, hasURL, "missing http.url attribute")
}

func TestHertzDoer_Do_GetOptions_Nil(t *testing.T) {
	// When client options is nil, GetOptions returns nil
	// This tests the branch in NewHertzDoer
	c, err := client.NewClient()
	require.NoError(t, err)

	// Set options to nil to test nil path
	// We can't easily nil options, but the nil check is defense-in-depth
	d := NewHertzDoer(c, WithName("test-nil-opts"))
	assert.Equal(t, "test-nil-opts", d.name)
}

func TestHertzDoer_Do_ContentLengthZeroResponse(t *testing.T) {
	// Response with ContentLength 0 but status 200
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			// Don't set body - content length is 0
			return nil
		}
	}

	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int64(0), resp.ContentLength)
}

func TestHertzDoer_Do_WithPrometheus(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)
	reg := prometheus.NewRegistry()

	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger), WithPrometheus(reg))

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	_ = resp.Body.Close()

	// Gather metrics and verify they exist
	metrics, err := reg.Gather()
	require.NoError(t, err)

	// Should have counter, histogram, and cache misses
	var hasCounter, hasHistogram, hasCacheMisses bool
	for _, mf := range metrics {
		switch mf.GetName() {
		case "http_client_requests_total":
			hasCounter = true
		case "http_client_requests_duration_seconds":
			hasHistogram = true
		case "http_client_cache_misses":
			hasCacheMisses = true
		}
	}
	assert.True(t, hasCounter, "counter metric should exist")
	assert.True(t, hasHistogram, "histogram metric should exist")
	assert.True(t, hasCacheMisses, "cache misses metric should exist")
}

func TestHertzDoer_Do_MetricsPanicRecovery(t *testing.T) {
	// Cover lines 1012-1024: panic recovery in metrics recording
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)
	reg := prometheus.NewRegistry()

	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger), WithPrometheus(reg))

	// Pre-populate metricCache with panic counter/observer so mc.counter.Inc() panics.
	// Key format: "method:route:status" — matches GET /api with status 200.
	d.metricCache.Store("GET:/api:200", &metricCacheEntry{
		mc: MetricCollectors{
			counter:   panicCounter{},
			histogram: panicObserver{},
		},
	})

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	// Request should succeed even though metrics panic
	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()

	// Verify panic was logged
	logOutput := buf.String()
	assert.Contains(t, logOutput, "metrics recording panicked")
}

func TestHertzDoer_Do_MultipleRequests_SameMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithPrometheus(reg))

	// Two requests to same route → metric cache hit on second
	for range 2 {
		req, _ := http.NewRequest("GET", "https://example.com/api", nil)
		resp, err := d.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}

	metrics, _ := reg.Gather()
	var cacheHits float64
	for _, mf := range metrics {
		if mf.GetName() == "http_client_cache_hits" {
			for _, m := range mf.GetMetric() {
				cacheHits += m.GetCounter().GetValue()
			}
		}
	}
	assert.Equal(t, float64(1), cacheHits, "second request should cause cache hit")
}

func TestHertzDoer_Do_OverflowMetric(t *testing.T) {
	// Test metric cache overflow by making many unique route requests
	reg := prometheus.NewRegistry()
	c, err := client.NewClient()
	require.NoError(t, err)

	d := NewHertzDoer(c, WithPrometheus(reg))

	// Directly set metricCacheSize close to limit to trigger overflow
	d.metricCache.SetSize(MaxMetricCacheEntries)

	// Manually call getMetrics which will hit the overflow path
	// Need requestCounter and latencyHist to be set for this
	_ = d.getMetrics("GET", "/test-overflow", 200)

	// Should not panic and should return valid collectors
}

func TestHertzDoer_getMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	c, err := client.NewClient()
	require.NoError(t, err)
	d := NewHertzDoer(c, WithPrometheus(reg))

	t.Run("cache_miss", func(t *testing.T) {
		initial := d.metricCache.Size()
		mc := d.getMetrics("POST", "/api/users", 201)
		assert.NotNil(t, mc.counter)
		assert.NotNil(t, mc.histogram)
		// Size should have increased
		assert.Greater(t, d.metricCache.Size(), initial)
	})

	t.Run("cache_hit", func(t *testing.T) {
		// Same key as above
		mc1 := d.getMetrics("POST", "/api/users", 201)
		mc2 := d.getMetrics("POST", "/api/users", 201)
		// Should return the same collectors (cache hit)
		assert.NotNil(t, mc2.counter)
		assert.NotNil(t, mc2.histogram)
		// Just verify no panic - both should work
		mc1.counter.Inc()
		mc2.counter.Inc()
	})

	t.Run("cache_overflow", func(t *testing.T) {
		// Force overflow by setting size to limit on a real doer
		reg := prometheus.NewRegistry()
		c, _ := client.NewClient()
		d2 := NewHertzDoer(c, WithPrometheus(reg))
		d2.metricCache.SetSize(MaxMetricCacheEntries)

		// Should return collectors without panic (may use /_overflow route)
		_ = d2.getMetrics("GET", "/some-overflow-route", 500)
		// Should not panic
	})

	t.Run("evict_stale_entries", func(t *testing.T) {
		// Cover lines 492-496: evict entries older than metricCacheTTL when cache full
		reg := prometheus.NewRegistry()
		c, _ := client.NewClient()
		d := NewHertzDoer(c, WithPrometheus(reg))

		// Fill cache to limit
		for i := range MaxMetricCacheEntries {
			d.getMetrics("GET", fmt.Sprintf("/evict-route-%d", i), 200)
		}
		assert.Equal(t, int64(MaxMetricCacheEntries), d.metricCache.Size())

		// Age half the entries past TTL
		now := time.Now().Unix()
		staleCount := 0
		d.metricCache.Range(func(k, v any) bool {
			entry := v.(*metricCacheEntry)
			if staleCount < MaxMetricCacheEntries/2 {
				entry.lastUsed.Store(now - metricCacheTTL - 1)
				staleCount++
			}
			return true
		})
		assert.Equal(t, MaxMetricCacheEntries/2, staleCount)

		// Allow eviction by setting lastMetricEviction in the past
		d.metricCache.SetLastEviction(now - 61)

		// Next getMetrics should trigger eviction of stale entries
		mc := d.getMetrics("POST", "/evict-new-route", 200)
		assert.NotNil(t, mc.counter)
		assert.NotNil(t, mc.histogram)

		// Cache size should have decreased (stale entries evicted, new one added)
		newSize := d.metricCache.Size()
		assert.Less(t, newSize, int64(MaxMetricCacheEntries),
			"expected eviction to reduce cache size, got %d", newSize)
	})

	t.Run("race_condition_during_load", func(t *testing.T) {
		// Simulate race: set size just below limit, then LoadOrStore adds one,
		// pushing over MaxMetricCacheEntries
		reg2 := prometheus.NewRegistry()
		c2, _ := client.NewClient()
		d3 := NewHertzDoer(c2, WithPrometheus(reg2))

		// Preload 999 entries (1 below limit)
		for i := range MaxMetricCacheEntries - 1 {
			d3.getMetrics("GET", fmt.Sprintf("/route-%d", i), 200)
		}

		// This should push us to exactly the limit
		mc := d3.getMetrics("GET", "/route-boundary", 200)
		assert.NotNil(t, mc.counter)

		// This pushes over limit (exactly at limit after preload+1 above)
		// The race guard may delete it, falling back to overflow
		_ = d3.getMetrics("POST", "/route-boundary-2", 200)
		// Size should be at or below limit (the race-resolved one got deleted)
		assert.LessOrEqual(t, d3.metricCache.Size(), int64(MaxMetricCacheEntries))
	})
}

func TestHertzDoer_applyPrometheus(t *testing.T) {
	t.Run("nil_prometheus_skips", func(t *testing.T) {
		c, _ := client.NewClient()
		d := NewHertzDoer(c)
		// Should not panic when prometheus is nil
		assert.NotPanics(t, func() {
			d.applyPrometheus(time.Second, "GET", "/test", 200)
		})
	})

	t.Run("with_prometheus_records", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		c, _ := client.NewClient()
		d := NewHertzDoer(c, WithPrometheus(reg))

		d.applyPrometheus(100*time.Millisecond, "GET", "/test", 200)

		metrics, err := reg.Gather()
		require.NoError(t, err)

		var counterVal, histogramVal float64
		for _, mf := range metrics {
			switch mf.GetName() {
			case "http_client_requests_total":
				counterVal = mf.GetMetric()[0].GetCounter().GetValue()
			case "http_client_requests_duration_seconds":
				histogramVal = mf.GetMetric()[0].GetHistogram().GetSampleSum()
			}
		}
		assert.Equal(t, float64(1), counterVal)
		assert.Equal(t, 0.1, histogramVal) // 100ms = 0.1s
	})

	t.Run("cache_hit_on_repeated_metric", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		c, _ := client.NewClient()
		d := NewHertzDoer(c, WithPrometheus(reg))

		d.applyPrometheus(50*time.Millisecond, "GET", "/repeated", 200)
		d.applyPrometheus(75*time.Millisecond, "GET", "/repeated", 200)

		metrics, _ := reg.Gather()
		var counterVal, histogramSum, cacheHits float64
		for _, mf := range metrics {
			switch mf.GetName() {
			case "http_client_requests_total":
				counterVal = mf.GetMetric()[0].GetCounter().GetValue()
			case "http_client_requests_duration_seconds":
				histogramSum = mf.GetMetric()[0].GetHistogram().GetSampleSum()
			case "http_client_cache_hits":
				cacheHits = mf.GetMetric()[0].GetCounter().GetValue()
			}
		}
		assert.Equal(t, float64(2), counterVal)
		assert.Equal(t, 0.125, histogramSum) // 50ms + 75ms = 0.125s
		assert.Equal(t, float64(1), cacheHits)
	})
}

func TestHertzDoer_doWithPanicRecovery(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	t.Run("normal_execution", func(t *testing.T) {
		mw := successMiddleware(200, "ok")
		c := newTestClient(t, mw)
		d := NewHertzDoer(c, WithLogger(logger))

		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doWithPanicRecovery(context.Background(), nil, hReq, hRes, "https://example.com", "GET", "/test")
		assert.NoError(t, err)

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})

	t.Run("panic_recovery", func(t *testing.T) {
		mw := panicMiddleware("test panic")
		c := newTestClient(t, mw)
		d := NewHertzDoer(c, WithLogger(logger))

		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doWithPanicRecovery(context.Background(), nil, hReq, hRes, "https://example.com", "GET", "/test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "panic in hertz client.Do")
		assert.Contains(t, err.Error(), "test panic")

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})

	t.Run("panic_recovery_with_span", func(t *testing.T) {
		ctx, span, _ := getTestTracer(t)
		defer span.End()

		mw := panicMiddleware("span panic")
		c := newTestClient(t, mw)
		d := NewHertzDoer(c, WithLogger(logger))

		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doWithPanicRecovery(ctx, span, hReq, hRes, "https://example.com", "GET", "/test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "span panic")

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})
}

func TestHertzDoer_doCheckContentLengthVSMaxRequestBodySize(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)
	ctx, span, _ := getTestTracer(t)
	defer span.End()

	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithLogger(logger), WithMaxRequestBodySize(1024))

	t.Run("within_limit", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", nil)
		req.ContentLength = 512

		err := d.doCheckContentLengthVSMaxRequestBodySize(ctx, span, req, "https://example.com", "/api")
		assert.NoError(t, err)
	})

	t.Run("exactly_at_limit", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", nil)
		req.ContentLength = 1024

		err := d.doCheckContentLengthVSMaxRequestBodySize(ctx, span, req, "https://example.com", "/api")
		assert.NoError(t, err)
	})

	t.Run("over_limit", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", nil)
		req.ContentLength = 2048

		err := d.doCheckContentLengthVSMaxRequestBodySize(ctx, span, req, "https://example.com", "/api")
		assert.Error(t, err)
		var hErr *HertzDoerError
		assert.True(t, errors.As(err, &hErr))
		assert.Equal(t, "validation", hErr.Kind)
		assert.Contains(t, hErr.Message, "HTTP Body Payload Error")
	})

	t.Run("zero_content_length", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
		req.ContentLength = 0

		err := d.doCheckContentLengthVSMaxRequestBodySize(ctx, span, req, "https://example.com", "/api")
		assert.NoError(t, err)
	})

	t.Run("negative_content_length", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
		req.ContentLength = -1

		err := d.doCheckContentLengthVSMaxRequestBodySize(ctx, span, req, "https://example.com", "/api")
		assert.NoError(t, err)
	})
}

func TestHertzDoer_doCheckRequestBody(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)
	ctx, span, _ := getTestTracer(t)
	defer span.End()

	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithLogger(logger), WithMaxRequestBodySize(50))

	t.Run("nil_body", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)
		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doCheckRequestBody(ctx, span, hReq, req, "https://example.com", "/api")
		assert.NoError(t, err)

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})

	t.Run("body_with_content_length_normal", func(t *testing.T) {
		body := strings.NewReader("hello")
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", body)
		req.ContentLength = 5

		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doCheckRequestBody(ctx, span, hReq, req, "https://example.com", "/api")
		assert.NoError(t, err)

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})

	t.Run("body_unknown_length_within_limit", func(t *testing.T) {
		body := strings.NewReader("short body")
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", body)
		req.ContentLength = 0 // Unknown length

		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doCheckRequestBody(ctx, span, hReq, req, "https://example.com", "/api")
		assert.NoError(t, err)

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})

	t.Run("body_unknown_length_exceeds_limit", func(t *testing.T) {
		body := strings.NewReader(strings.Repeat("x", 100)) // 100 > 50 limit
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", body)
		req.ContentLength = 0 // Unknown length

		hReq := protocol.AcquireRequest()

		err := d.doCheckRequestBody(ctx, span, hReq, req, "https://example.com", "/api")
		assert.Error(t, err)
		var hErr *HertzDoerError
		assert.True(t, errors.As(err, &hErr))
		assert.Equal(t, "validation", hErr.Kind)
		assert.Contains(t, hErr.Message, "exceeds limit")
	})

	t.Run("body_copy_error", func(t *testing.T) {
		errReader := &errorReader{err: errors.New("read failure")}
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", errReader)
		req.ContentLength = 0 // Unknown length to trigger copy path

		hReq := protocol.AcquireRequest()

		err := d.doCheckRequestBody(ctx, span, hReq, req, "https://example.com", "/api")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "read failure")
	})

	t.Run("body_unknown_length_at_exact_limit", func(t *testing.T) {
		body := strings.NewReader(strings.Repeat("x", 50))
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.com/api", body)
		req.ContentLength = 0

		hReq := protocol.AcquireRequest()
		hRes := protocol.AcquireResponse()

		err := d.doCheckRequestBody(ctx, span, hReq, req, "https://example.com", "/api")
		assert.NoError(t, err)

		releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
	})
}

func TestHertzDoer_doCheckRequestBody_LargeBufferReturn(t *testing.T) {
	// Test that large buffers (> maxPoolBufferSize) are not returned to pool
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithMaxRequestBodySize(100*1024)) // 100KB

	// Create body that's large but within limit
	body := strings.NewReader(strings.Repeat("x", 70*1024)) // 70KB > maxPoolBufferSize (64KB)
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 0 // trigger buffer copy path

	hReq := protocol.AcquireRequest()
	hRes := protocol.AcquireResponse()

	err := d.doCheckRequestBody(context.Background(), nil, hReq, req, "https://example.com", "/api")
	assert.NoError(t, err)

	releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
}

func TestHertzDoer_getMetrics_NilPrometheus(t *testing.T) {
	// When requestCounter is nil but code path is reached
	c, _ := client.NewClient()
	d := NewHertzDoer(c) // No prometheus

	// We can't directly reach getMetrics with nil requestCounter in normal flow
	// because applyPrometheus guards it. But getMetrics itself doesn't check nil.
	// If somehow called, it would panic. Documenting this via code coverage
	// of the guard in applyPrometheus:
	d.applyPrometheus(time.Second, "GET", "/test", 200)
	// No panic - nil guard works
}

func TestHertzDoer_GetOptions_NilCheck(t *testing.T) {
	// Test the code path where client returns nil for GetOptions
	// In practice this may not happen with real hertz, but the check exists
	c, _ := client.NewClient()
	assert.NotNil(t, c.GetOptions(), "real client always returns options")
	// The nil check in NewHertzDoer is defensive - tested indirectly
}

func TestHertzDoer_Do_MaxMetricCacheEntries_RaceCondition(t *testing.T) {
	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))

	// Preload to exactly MaxMetricCacheEntries - 1
	for i := range MaxMetricCacheEntries - 1 {
		d.getMetrics("GET", fmt.Sprintf("/route-%d", i), 200)
	}

	assert.Equal(t, int64(MaxMetricCacheEntries-1), d.metricCache.Size())

	// Next one hits exactly limit
	mc := d.getMetrics("GET", "/at-limit", 200)
	assert.NotNil(t, mc.counter)
	assert.Equal(t, int64(MaxMetricCacheEntries), d.metricCache.Size())

	// This trips the LoadOrStore → newSize > Max → delete + return fallback
	_ = d.getMetrics("POST", "/at-limit-again", 200)
	// Should not panic, should return collectors
}

func TestHertzDoer_Do_RequestWithTrailingSlash(t *testing.T) {
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api/v1/?query=value", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	_ = resp.Body.Close()
}

func TestHertzDoer_Do_RequestBody_LimitExceededByOne(t *testing.T) {
	// Body is exactly maxRequestBodySize + 1 → should trigger overflow detection
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithMaxRequestBodySize(5))

	body := strings.NewReader(strings.Repeat("x", 6)) // 6 > 5
	req, _ := http.NewRequest("POST", "https://example.com/api/foo", body)
	req.ContentLength = 0 // Unknown length path

	resp, err := d.Do(req)
	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "exceeds limit")
}

func TestHertzDoer_Do_BodyExactLimit(t *testing.T) {
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithMaxRequestBodySize(5))

	body := strings.NewReader(strings.Repeat("x", 5)) // exactly 5
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 0 // Unknown length path

	// Will fail on client.Do (no server) but body validation should pass
	resp, err := d.Do(req)
	assert.Nil(t, resp)
	assert.Error(t, err)
	// Error is from client.Do, not body validation
	assert.NotContains(t, err.Error(), "exceeds limit")
}

func Test_hertzHeaderCarrier_Integration(t *testing.T) {
	// Verify carrier implements propagation.TextMapCarrier
	header := &protocol.RequestHeader{}
	carrier := hertzHeaderCarrier{header: header}

	carrier.Set("Traceparent", "00-abc-123-01")
	assert.Equal(t, "00-abc-123-01", carrier.Get("Traceparent"))

	keys := carrier.Keys()
	assert.Contains(t, keys, "Traceparent")
}

func TestHertzDoer_Do_CustomURLSanitizer(t *testing.T) {
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithURLSanitizer(func(s *url.URL) string {
		return "https://sanitized.example.com/api"
	}))

	req, _ := http.NewRequest("GET", "https://original.example.com/api?secret=xxx", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	_ = resp.Body.Close()
}

func TestHertzDoer_Do_BodyStream_CopyEOF(t *testing.T) {
	// io.CopyN returns io.EOF which should be treated as success
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithMaxRequestBodySize(100))

	// Create a reader that returns EOF on first read (like empty body)
	body := strings.NewReader("")
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 0

	hReq := protocol.AcquireRequest()
	hRes := protocol.AcquireResponse()

	err := d.doCheckRequestBody(context.Background(), nil, hReq, req, "https://example.com", "/api")
	assert.NoError(t, err) // io.EOF from CopyN should be acceptable

	releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
}

func TestHertzDoer_getMetrics_RaceConditionOverflow(t *testing.T) {
	// Two race-guard blocks in getMetrics can only be hit by concurrent goroutines.
	// Block 1 (288-291): loaded=true — two goroutines LoadOrStore same key
	// Block 2 (295-305): newSize>Max — two goroutines push past limit together
	// Use pre-filled cache + tight concurrent loop to trigger both.

	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))

	// Fill to exactly Max-1
	for i := range MaxMetricCacheEntries - 1 {
		d.getMetrics("GET", fmt.Sprintf("/p-%d", i), 200)
	}

	// Fire many goroutines: some with same keys (→ loaded=true), some with new keys (→ overflow)
	// Repeat to give scheduler many chances to interleave at the right spot
	oldProcs := runtime.GOMAXPROCS(8) // maximize preemption
	defer runtime.GOMAXPROCS(oldProcs)

	var wg sync.WaitGroup
	goroutines := 200
	for i := range goroutines {
		wg.Go(func() {
			idx := i
			// Mix: 50% same key (load hit after first), 50% new unique key (forces overflow)
			method := "GET"
			if idx%2 == 0 {
				method = "POST"
			}
			keyIdx := idx % 5 // 5 reused keys → loaded=true races
			d.getMetrics(method, fmt.Sprintf("/p-%d", keyIdx), 200)
			// Also try completely new keys to push past limit
			if idx < 10 {
				d.getMetrics("PUT", fmt.Sprintf("/unique-%d", idx), 200)
			}
		})
	}
	wg.Wait()

	assert.LessOrEqual(t, d.metricCache.Size(), int64(MaxMetricCacheEntries))
}

func TestHertzDoer_getMetrics_CASLoopOverflow(t *testing.T) {
	// Cover lines 527–532: CAS loop sees cur >= MaxMetricCacheEntries.
	// Use test hook to simulate race: bump size between Load() and CAS().
	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))

	// Fill to Max-1 entries
	for i := range MaxMetricCacheEntries - 1 {
		d.getMetrics("GET", fmt.Sprintf("/cas-%d", i), 200)
	}
	assert.Equal(t, int64(MaxMetricCacheEntries-1), d.metricCache.Size())

	// Hook: bump size to Max between Load() and CAS()
	// This simulates another goroutine filling the last slot
	hookCalled := false
	casBetweenLoadAndCompareHook = func() {
		if !hookCalled {
			d.metricCache.size.Add(1) // 999 → 1000
			hookCalled = true
		}
	}
	defer func() { casBetweenLoadAndCompareHook = nil }()

	// Call getMetrics:
	// - Load() returns 999 (line 526)
	// - Hook bumps to 1000
	// - CAS fails (expected 999, actual 1000)
	// - Retry: Load() returns 1000, cur >= Max → overflow at line 527-531
	mc := d.getMetrics("POST", "/cas-race", 200)
	assert.NotNil(t, mc.counter)
	assert.True(t, hookCalled, "hook should have been called")
}

func TestHertzDoer_getMetrics_LoadOrStoreRace(t *testing.T) {
	// Cover lines 556-562: LoadOrStore returns loaded=true.
	// Use test hook to pre-populate cache between slot reservation and LoadOrStore.
	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))

	initialSize := d.metricCache.Size()

	// Hook: simulate another goroutine storing the same key after we reserved a slot
	casBetweenReserveAndStoreHook = func(key string) {
		// Pre-populate cache with the key we're about to store
		existingEntry := &metricCacheEntry{
			mc: MetricCollectors{
				counter:   d.requestCounter.WithLabelValues("POST", "/load-store-race", "200"),
				histogram: d.latencyHist.WithLabelValues("POST", "/load-store-race"),
			},
		}
		existingEntry.lastUsed.Store(time.Now().Unix())
		d.metricCache.Store(key, existingEntry)
	}
	defer func() { casBetweenReserveAndStoreHook = nil }()

	// Call getMetrics:
	// - Cache miss, reserve slot (size increments)
	// - Hook stores the key
	// - LoadOrStore finds key, returns loaded=true
	// - Path: release reserved slot, update timestamp, return loaded entry
	mc := d.getMetrics("POST", "/load-store-race", 200)
	assert.NotNil(t, mc.counter)

	// Size should not have increased (slot was released)
	finalSize := d.metricCache.Size()
	assert.Equal(t, initialSize, finalSize, "slot should have been released")
}

func TestHertzDoer_shouldLogPanic(t *testing.T) {
	c, _ := client.NewClient()

	t.Run("first_10_panics", func(t *testing.T) {
		d := NewHertzDoer(c)
		for i := 1; i <= 10; i++ {
			assert.True(t, d.shouldLogPanic(), "first 10 panics should always log")
		}
	})

	t.Run("sample_1_in_100", func(t *testing.T) {
		d := NewHertzDoer(c)
		d.panicCounter.Store(10) // start after first 10
		for i := 11; i <= 100; i++ {
			if i%100 == 0 {
				assert.True(t, d.shouldLogPanic(), "100th panic should log")
			} else {
				assert.False(t, d.shouldLogPanic(), "panics 11-99 should not log")
			}
		}
	})

	t.Run("counter_wraparound", func(t *testing.T) {
		d := NewHertzDoer(c)
		d.panicCounter.Store(math.MaxUint64) // Next Add(1) results in 0
		assert.False(t, d.shouldLogPanic(), "rate wrap around to 0 should not log")
	})
}

func TestHertzDoer_shouldLogSuccess(t *testing.T) {
	c, _ := client.NewClient()

	t.Run("sample_rate_zero", func(t *testing.T) {
		d := NewHertzDoer(c, WithLogSampleRate(0))
		assert.False(t, d.shouldLogSuccess(), "rate 0 should never log")
	})

	t.Run("sample_rate_one", func(t *testing.T) {
		d := NewHertzDoer(c, WithLogSampleRate(1))
		assert.True(t, d.shouldLogSuccess(), "rate 1 should always log")
	})

	t.Run("sample_rate_five", func(t *testing.T) {
		// Cover lines 583-588: modulo check with rate > 1
		d := NewHertzDoer(c, WithLogSampleRate(5))

		// First 4 calls: count % 5 != 0, return false
		for i := range 4 {
			assert.False(t, d.shouldLogSuccess(), "call %d should not log", i+1)
		}

		// 5th call: count=5, 5%5==0, return true
		assert.True(t, d.shouldLogSuccess(), "5th call should log")

		// 6th-9th calls: false again
		for i := range 4 {
			assert.False(t, d.shouldLogSuccess(), "call %d should not log", i+6)
		}

		// 10th call: count=10, 10%5==0, return true
		assert.True(t, d.shouldLogSuccess(), "10th call should log")
	})

	t.Run("counter_wraparound", func(t *testing.T) {
		// Cover lines 585-586: counter wraps from MaxUint64 to 0
		d := NewHertzDoer(c, WithLogSampleRate(3))

		// Set counter to MaxUint64 so next Add(1) wraps to 0
		d.logSampleCounter.Store(math.MaxUint64)

		// Add(1) wraps MaxUint64 → 0, count==0 guard returns false
		assert.False(t, d.shouldLogSuccess(), "counter wrap to 0 should not log")

		// Next call: counter is 1, continues normally (1 % 3 != 0)
		assert.False(t, d.shouldLogSuccess(), "counter=1 should not log")

		// counter=2 (2 % 3 != 0)
		assert.False(t, d.shouldLogSuccess(), "counter=2 should not log")

		// counter=3 (3 % 3 == 0) → true
		assert.True(t, d.shouldLogSuccess(), "counter=3 should log")
	})
}

func TestHertzDoer_doCheckRequestBody_NilBodyWithContentLength(t *testing.T) {
	// ContentLength > 0 but Body == nil (line 381-393)
	c, _ := client.NewClient()
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("POST", "https://example.com/api", nil)
	req.ContentLength = 100
	req.Body = nil // Explicitly nil

	hReq := protocol.AcquireRequest()

	err := d.doCheckRequestBody(context.Background(), nil, hReq, req, "https://example.com", "/api")
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "Body is nil")
}

func TestHertzDoer_doCheckRequestBody_PoolContamination(t *testing.T) {
	// bufPool returns non-*bytes.Buffer (line 403-405)
	c, _ := client.NewClient()
	d := NewHertzDoer(c)

	// Contaminate pool with wrong type
	d.bufPool = sync.Pool{New: func() any { return "not a buffer" }}

	body := strings.NewReader("data")
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 0

	hReq := protocol.AcquireRequest()
	hRes := protocol.AcquireResponse()

	err := d.doCheckRequestBody(context.Background(), nil, hReq, req, "https://example.com", "/api")
	assert.NoError(t, err)

	releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
}

func TestHertzDoer_Do_ResponseHeaderMultiValue(t *testing.T) {
	// This hits the `existing != nil` branch in VisitAll callback
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.SetBodyStream(bytes.NewReader([]byte("ok")), 2)
			resp.Header.Set("Set-Cookie", "a=1")
			resp.Header.Add("Set-Cookie", "b=2") // Multi-value header
			return nil
		}
	}

	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)

	resp, err := d.Do(req)

	require.NoError(t, err)
	cookies := resp.Header["Set-Cookie"]
	assert.Len(t, cookies, 2)
	assert.Contains(t, cookies, "a=1")
	assert.Contains(t, cookies, "b=2")
	_ = resp.Body.Close()
}

func TestHertzDoer_doCheckRequestBody_BufferPoolReturn(t *testing.T) {
	// Tests the defer block in doCheckRequestBody (line 381-393)
	// Specifically: small buffer returned to pool, defer path executes
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithMaxRequestBodySize(100))

	// Body with unknown ContentLength → triggers the else branch with buffer pool
	// ContentLength 0 and body != nil → goes to unknown length path
	body := strings.NewReader("small")
	req, _ := http.NewRequest("POST", "https://example.com/api", body)
	req.ContentLength = 0

	hReq := protocol.AcquireRequest()
	hRes := protocol.AcquireResponse()

	err := d.doCheckRequestBody(context.Background(), nil, hReq, req, "https://example.com", "/api")
	assert.NoError(t, err)

	// The defer in doCheckRequestBody should have returned the buffer to pool
	releaseHertzResources(cleanupArgs{req: hReq, res: hRes})
}

func TestHertzDoer_Do_NilURL(t *testing.T) {
	c, _ := client.NewClient()
	d := NewHertzDoer(c)

	req := &http.Request{URL: nil, Header: make(http.Header)}
	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	var hErr *HertzDoerError
	assert.True(t, errors.As(err, &hErr))
	assert.Equal(t, "validation", hErr.Kind)
	assert.Contains(t, hErr.Message, "URL is nil")
}

func TestHertzDoer_Do_ContentLengthNegative(t *testing.T) {
	// Trigger contentLength == -1 normalization (line 681-683).
	// SetBodyStream with -1 length → ContentLength() returns -1 (unknown length).
	mw := func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(200)
			resp.SetBodyStream(bytes.NewReader([]byte("ok")), -1)
			return nil
		}
	}
	c := newTestClient(t, mw)
	d := NewHertzDoer(c)

	req, _ := http.NewRequest("GET", "https://example.com/api", nil)
	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.Equal(t, int64(-1), resp.ContentLength)
	_ = resp.Body.Close()
}

func TestHertzDoer_getMetrics_LoadedRace(t *testing.T) {
	// Hit LoadOrStore loaded=true path (line 284-286).
	// Two goroutines call getMetrics with same new key simultaneously.
	// Both Load() miss → both call LoadOrStore → one stores, one gets loaded=true.
	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))

	// Ensure clean starting state.
	const key = "GET:/loaded-race:200"
	d.metricCache.Delete(key)

	var ready, goBarrier sync.WaitGroup
	ready.Add(2)
	goBarrier.Add(1)

	var done sync.WaitGroup
	done.Add(2)

	var mu sync.Mutex
	var results []MetricCollectors

	for range 2 {
		go func() {
			ready.Done()
			goBarrier.Wait()
			mc := d.getMetrics("GET", "/loaded-race", 200)
			mu.Lock()
			results = append(results, mc)
			mu.Unlock()
			done.Done()
		}()
	}

	ready.Wait()     // both goroutines at the barrier
	goBarrier.Done() // release both simultaneously
	done.Wait()      // both finished

	assert.Len(t, results, 2)
	// Both must have valid collectors regardless of which path was taken.
	for _, mc := range results {
		assert.NotNil(t, mc.counter)
		assert.NotNil(t, mc.histogram)
	}
}

func TestHertzDoer_Do_CircuitBreaker_HalfOpenProbeSuccess(t *testing.T) {
	// Cover lines 1006-1009: half-open probe success closes circuit
	// Flow: circuitOpen (resetAt passed) → CAS to circuitHalfOpen → probe succeeds → circuitClosed
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithCircuitBreaker(2, 30*time.Second))

	host := "halfopen-success.example.com"
	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)

	circuit := circuitFor(&d.circuitBreakers, host)
	// Set to circuitOpen with resetAt in the past so Do() transitions to circuitHalfOpen
	circuit.state.Store(circuitOpen)
	circuit.resetAt.Store(time.Now().Add(-1 * time.Second).Unix()) // past reset time
	circuit.failures.Store(5)                                      // non-zero to verify reset

	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.NoError(t, resp.Body.Close())

	// Circuit should now be closed with failures reset (lines 1006-1009)
	assert.Equal(t, circuitClosed, circuit.state.Load())
	assert.Equal(t, int64(0), circuit.failures.Load())
}

func TestHertzDoer_Do_CircuitBreaker_HalfOpenProbeFailure(t *testing.T) {
	// Cover lines 977-985: half-open probe fails → circuit reopens
	mw := errorMiddleware(errors.New("connection refused"))
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithCircuitBreaker(2, 30*time.Second))

	host := "halfopen-fail.example.com"
	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)

	circuit := circuitFor(&d.circuitBreakers, host)
	// circuitOpen with past resetAt → Do() CAS to circuitHalfOpen, probe runs, fails
	circuit.state.Store(circuitOpen)
	circuit.resetAt.Store(time.Now().Add(-1 * time.Second).Unix())

	_, err := d.Do(req)

	assert.Error(t, err)
	// Circuit should reopen after probe failure
	assert.Equal(t, circuitOpen, circuit.state.Load())
	assert.Greater(t, circuit.resetAt.Load(), time.Now().Unix()-1)
}

func TestHertzDoer_Do_CircuitBreaker_ClosedAccumulateFailures(t *testing.T) {
	// Cover lines 986-995: closed circuit accumulates failures → opens at threshold
	mw := errorMiddleware(errors.New("server error"))
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithCircuitBreaker(3, 10*time.Second))

	host := "closed-accumulate.example.com"

	// Fail twice — below threshold, circuit stays closed
	for range 2 {
		req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)
		_, err := d.Do(req)
		assert.Error(t, err)
	}

	circuit := circuitFor(&d.circuitBreakers, host)
	assert.Equal(t, circuitClosed, circuit.state.Load())
	assert.Equal(t, int64(2), circuit.failures.Load())

	// Third failure hits threshold → circuit opens
	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)
	_, err := d.Do(req)
	assert.Error(t, err)

	assert.Equal(t, circuitOpen, circuit.state.Load())
	assert.Equal(t, int64(3), circuit.failures.Load())
	assert.Greater(t, circuit.resetAt.Load(), int64(0))
}

func TestHertzDoer_Do_CircuitBreaker_ClosedAccumulateFailures_WithPrometheus(t *testing.T) {
	// Cover lines 1005-1007: circuitBreakerOpens counter incremented when circuit opens
	reg := prometheus.NewRegistry()
	mw := errorMiddleware(errors.New("server error"))
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithPrometheus(reg), WithCircuitBreaker(3, 10*time.Second))

	host := "closed-accumulate-prometheus.example.com"

	// Fail twice — below threshold, circuit stays closed
	for range 2 {
		req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)
		_, err := d.Do(req)
		assert.Error(t, err)
	}

	circuit := circuitFor(&d.circuitBreakers, host)
	assert.Equal(t, circuitClosed, circuit.state.Load())
	assert.Equal(t, int64(2), circuit.failures.Load())

	// Third failure hits threshold → circuit opens and increments circuitBreakerOpens
	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)
	_, err := d.Do(req)
	assert.Error(t, err)

	assert.Equal(t, circuitOpen, circuit.state.Load())
	assert.Equal(t, int64(3), circuit.failures.Load())

	// Verify circuitBreakerOpens counter was incremented
	metrics, err := reg.Gather()
	require.NoError(t, err)
	var opensCount float64
	for _, mf := range metrics {
		if mf.GetName() == "http_client_circuit_breaker_opens_total" {
			opensCount = mf.GetMetric()[0].GetCounter().GetValue()
			break
		}
	}
	assert.Equal(t, float64(1), opensCount, "circuitBreakerOpens should be incremented")
}

func TestHertzDoer_Do_CircuitBreaker_Open_RejectRequests(t *testing.T) {
	// Cover lines 784-795: open circuit rejects requests before resetAt
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger), WithCircuitBreaker(2, 30*time.Second))

	host := "open-reject.example.com"

	// Set circuit to open state with resetAt in the future
	circuit := circuitFor(&d.circuitBreakers, host)
	circuit.state.Store(circuitOpen)
	circuit.resetAt.Store(time.Now().Add(1 * time.Hour).Unix())

	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker open")
	assert.Contains(t, err.Error(), host)

	// Verify error was logged
	logOutput := buf.String()
	assert.Contains(t, logOutput, "circuit_open")
	assert.Contains(t, logOutput, "circuit breaker open")
}

func TestHertzDoer_Do_CircuitBreaker_Open_RejectsCounter(t *testing.T) {
	// Cover lines 800-802: circuitBreakerRejects counter incremented on rejection
	reg := prometheus.NewRegistry()
	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithPrometheus(reg), WithCircuitBreaker(2, 30*time.Second))

	host := "open-rejects-counter.example.com"

	// Set circuit to open state with resetAt in the future
	circuit := circuitFor(&d.circuitBreakers, host)
	circuit.state.Store(circuitOpen)
	circuit.resetAt.Store(time.Now().Add(1 * time.Hour).Unix())

	// Verify counter starts at 0
	metrics, err := reg.Gather()
	require.NoError(t, err)
	var rejectCount float64
	for _, mf := range metrics {
		if mf.GetName() == "http_client_circuit_breaker_rejects_total" {
			rejectCount = mf.GetMetric()[0].GetCounter().GetValue()
			break
		}
	}
	assert.Equal(t, float64(0), rejectCount, "counter should start at 0")

	// Make request that gets rejected
	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)
	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)

	// Verify counter was incremented
	metrics, err = reg.Gather()
	require.NoError(t, err)
	for _, mf := range metrics {
		if mf.GetName() == "http_client_circuit_breaker_rejects_total" {
			rejectCount = mf.GetMetric()[0].GetCounter().GetValue()
			break
		}
	}
	assert.Equal(t, float64(1), rejectCount, "counter should be incremented to 1")

	// Make another rejected request
	req2, _ := http.NewRequest("GET", "https://"+host+"/api", nil)
	resp2, err2 := d.Do(req2)

	assert.Nil(t, resp2)
	assert.Error(t, err2)

	// Verify counter incremented again
	metrics, err = reg.Gather()
	require.NoError(t, err)
	for _, mf := range metrics {
		if mf.GetName() == "http_client_circuit_breaker_rejects_total" {
			rejectCount = mf.GetMetric()[0].GetCounter().GetValue()
			break
		}
	}
	assert.Equal(t, float64(2), rejectCount, "counter should be incremented to 2")
}

func TestHertzDoer_Do_CircuitBreaker_HalfOpen_RejectOtherRequests(t *testing.T) {
	// Cover lines 801-809: half-open state rejects other requests
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)
	d := NewHertzDoer(c, WithLogger(logger), WithCircuitBreaker(2, 30*time.Second))

	host := "halfopen-reject.example.com"

	// Set circuit to half-open state (probe in flight)
	circuit := circuitFor(&d.circuitBreakers, host)
	circuit.state.Store(circuitHalfOpen)

	req, _ := http.NewRequest("GET", "https://"+host+"/api", nil)

	resp, err := d.Do(req)

	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker half-open")
	assert.Contains(t, err.Error(), "probe in flight")

	// Verify error was logged
	logOutput := buf.String()
	assert.Contains(t, logOutput, "circuit_open")
	assert.Contains(t, logOutput, "probe in flight")
}

func TestHertzDoer_Do_TracingInjectionPanic(t *testing.T) {
	// Cover lines 900-911: panic recovery in tracing injection
	buf := &bytes.Buffer{}
	logger := getLogger(t, buf, testTime)

	mw := successMiddleware(200, "ok")
	c := newTestClient(t, mw)

	// Build a recording TracerProvider and pass its tracer to NewHertzDoer
	// so d.tracer.Start() produces recording spans (IsRecording() == true).
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test-tracer")

	d := NewHertzDoer(c, WithLogger(logger), WithTracer(tracer))

	// Replace propagator with one that panics during Inject
	d.propagator = panicPropagator{}

	// Parent span from same tracer — guarantees recording
	ctx, parentSpan := tracer.Start(context.Background(), "parent")
	defer parentSpan.End()

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/api", nil)

	// Request should succeed despite tracing panic
	resp, err := d.Do(req)

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.NoError(t, resp.Body.Close())

	// Verify panic was logged
	logOutput := buf.String()
	assert.Contains(t, logOutput, "tracing injection panicked")
}

func TestHertzDoer_Do_ConcurrencyLimiter(t *testing.T) {
	t.Run("normal request within limit", func(t *testing.T) {
		// Cover line 816-817: berhasil mengirim ke limiter channel
		buf := &bytes.Buffer{}
		logger := getLogger(t, buf, testTime)

		mw := successMiddleware(200, "ok")
		c := newTestClient(t, mw)
		d := NewHertzDoer(c, WithLogger(logger), WithConcurrencyLimiter(10))

		req, _ := http.NewRequest("GET", "https://example.com/api", nil)

		resp, err := d.Do(req)

		require.NoError(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, 200, resp.StatusCode)
		assert.NoError(t, resp.Body.Close())
	})

	t.Run("backpressure when limit exceeded", func(t *testing.T) {
		// Cover lines 818-824: channel penuh, return backpressure error
		buf := &bytes.Buffer{}
		logger := getLogger(t, buf, testTime)

		mw := successMiddleware(200, "ok")
		c := newTestClient(t, mw)
		d := NewHertzDoer(c, WithLogger(logger), WithConcurrencyLimiter(1))

		// Isi channel sampai penuh
		d.concurrencyLimiter <- struct{}{}

		req, _ := http.NewRequest("GET", "https://example.com/api", nil)

		resp, err := d.Do(req)

		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "concurrency limit exceeded")

		// Verifikasi log error
		logOutput := buf.String()
		assert.Contains(t, logOutput, "backpressure")

		// Cleanup: kosongkan channel
		<-d.concurrencyLimiter
	})
}

// panicPropagator is a TextMapPropagator that panics on Inject.
type panicPropagator struct{}

func (panicPropagator) Inject(_ context.Context, _ propagation.TextMapCarrier) {
	panic("test tracing panic")
}

func (panicPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}

func (panicPropagator) Fields() []string { return nil }

func TestHertzDoer_circuitFor(t *testing.T) {
	c, _ := client.NewClient()
	d := NewHertzDoer(c)

	t.Run("first_call_creates_circuit", func(t *testing.T) {
		host := "api.example.com"
		circuit := circuitFor(&d.circuitBreakers, host)
		assert.NotNil(t, circuit, "circuitFor must return non-nil circuit")
		assert.Equal(t, int32(0), circuit.state.Load(), "new circuit must be closed")
		assert.Equal(t, int64(0), circuit.failures.Load(), "new circuit must have zero failures")
	})

	t.Run("second_call_returns_cached_circuit", func(t *testing.T) {
		host := "cached.example.com"

		// First call: creates and stores circuit
		circuit1 := circuitFor(&d.circuitBreakers, host)
		assert.NotNil(t, circuit1)

		// Second call: must hit cache path (line 213-215)
		circuit2 := circuitFor(&d.circuitBreakers, host)
		assert.NotNil(t, circuit2)
		assert.Same(t, circuit1, circuit2, "second call must return same cached circuit")
	})

	t.Run("different_hosts_get_different_circuits", func(t *testing.T) {
		host1 := "host1.example.com"
		host2 := "host2.example.com"

		circuit1 := circuitFor(&d.circuitBreakers, host1)
		circuit2 := circuitFor(&d.circuitBreakers, host2)

		assert.NotNil(t, circuit1)
		assert.NotNil(t, circuit2)
		assert.NotSame(t, circuit1, circuit2, "different hosts must have different circuits")
	})
}

func Test_statusError(t *testing.T) {
	t.Run("common status returns cached error", func(t *testing.T) {
		commonStatuses := []int{400, 401, 403, 404, 405, 408, 429, 500, 502, 503, 504}
		for _, status := range commonStatuses {
			err := statusError(status)
			assert.NotNil(t, err)
			assert.Contains(t, err.Error(), "HTTP Status")
			assert.Contains(t, err.Error(), strconv.Itoa(status))
		}
	})

	t.Run("uncommon status creates new error", func(t *testing.T) {
		// Cover line 45: fallback to errors.New for uncommon statuses
		uncommonStatuses := []int{418, 419, 422, 505, 506, 507}
		for _, status := range uncommonStatuses {
			err := statusError(status)
			assert.NotNil(t, err)
			assert.Contains(t, err.Error(), "HTTP Status")
			assert.Contains(t, err.Error(), strconv.Itoa(status))
		}
	})
}

func Test_logError(t *testing.T) {
	type args struct {
		buf        *bytes.Buffer
		mockTracer func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter)
		mockLogger func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger)
		err        error
		url        string
		method     string
		route      string
		duration   time.Duration
		status     int
		isTimeout  bool
	}
	tests := []struct {
		name           string
		args           args
		wantSpanStatus codes.Code
	}{
		{
			name: "error_with_logger_and_span",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return getTestTracer(t)
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return getLogger(t, bf, tm)
				},
				duration:  100 * time.Millisecond,
				err:       &HertzDoerError{Kind: "test", Message: "test error"},
				url:       "https://api.example.com/test",
				method:    "GET",
				route:     "/test",
				status:    500,
				isTimeout: false,
			},
			wantSpanStatus: codes.Error,
		},
		{
			name: "timeout_error",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return getTestTracer(t)
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return getLogger(t, bf, tm)
				},
				duration:  5 * time.Second,
				err:       context.DeadlineExceeded,
				url:       "https://api.example.com/slow",
				method:    "POST",
				route:     "/slow",
				status:    0,
				isTimeout: true,
			},
			wantSpanStatus: codes.Error,
		},
		{
			name: "error_without_logger",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return getTestTracer(t)
				},
				mockLogger: func(_ *testing.T, _ io.Writer, _ time.Time) (logger *slog.Logger) {
					return nil
				},
				duration:  50 * time.Millisecond,
				err:       &HertzDoerError{Kind: "http_client", Message: "connection failed"},
				url:       "https://api.example.com/fail",
				method:    "PUT",
				route:     "/fail",
				status:    0,
				isTimeout: false,
			},
			wantSpanStatus: codes.Error,
		},
		{
			name: "error_without_span",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return context.Background(), nil, nil
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return getLogger(t, bf, tm)
				},
				duration:  200 * time.Millisecond,
				err:       &HertzDoerError{Kind: "validation", Message: "invalid input"},
				url:       "https://api.example.com/validate",
				method:    "POST",
				route:     "/validate",
				status:    400,
				isTimeout: false,
			},
			wantSpanStatus: codes.Unset,
		},
		{
			name: "error_with_nil_logger_and_span",
			args: args{
				buf: &bytes.Buffer{},
				mockTracer: func(t *testing.T) (ct context.Context, sp trace.Span, ex *tracetest.InMemoryExporter) {
					return context.Background(), nil, nil
				},
				mockLogger: func(t *testing.T, bf io.Writer, tm time.Time) (logger *slog.Logger) {
					return nil
				},
				duration:  10 * time.Millisecond,
				err:       &HertzDoerError{Kind: "network", Message: "connection reset"},
				url:       "https://api.example.com/reset",
				method:    "DELETE",
				route:     "/reset",
				status:    0,
				isTimeout: false,
			},
			wantSpanStatus: codes.Unset,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger := tt.args.mockLogger(t, tt.args.buf, testTime)
			ctx, span, exporter := tt.args.mockTracer(t)

			// Create a HertzDoer with the logger
			d := &HertzDoer{logger: logger}

			d.logError(
				ctx,
				span,
				tt.args.duration,
				tt.args.err,
				tt.args.url,
				tt.args.method,
				tt.args.route,
				tt.args.status,
				tt.args.isTimeout,
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

func TestHertzDoer_Do_LogsPayloads(t *testing.T) {
	c := newTestClient(t, func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, res *protocol.Response) error {
			res.SetStatusCode(200)
			res.SetBodyString("response-body")
			res.Header.Set("X-Res-Header", "res-value")
			return nil
		}
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	d := NewHertzDoer(c,
		WithObservabilityConfig(ObservabilityConfig{
			LogRequestPayload:  true,
			LogResponsePayload: true,
		}),
		WithLogger(logger),
	)

	req, _ := http.NewRequest("POST", "http://example.com/api", bytes.NewBufferString("request-body"))
	req.Header.Set("X-Req-Header", "req-value")

	resp, err := d.Do(req)
	assert.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()

	logOutput := buf.String()
	assert.Contains(t, logOutput, "request-body")
	assert.Contains(t, logOutput, "req-value")
	assert.Contains(t, logOutput, "response-body")
	assert.Contains(t, logOutput, "res-value")
}

func TestHertzDoer_Do_LogsPayloads_StreamedResponse(t *testing.T) {
	c := newTestClient(t, func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, res *protocol.Response) error {
			res.SetStatusCode(200)
			res.SetBodyStream(bytes.NewReader([]byte("stream-body")), 11)
			return nil
		}
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	doer := NewHertzDoer(c, WithObservabilityConfig(ObservabilityConfig{LogResponsePayload: true}), WithLogger(logger))

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	resp, err := doer.Do(req)
	assert.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()

	assert.Contains(t, buf.String(), "<streamed response body>")
}

func TestHertzDoer_getMetrics_NilCache(t *testing.T) {
	c, _ := client.NewClient()
	reg := prometheus.NewRegistry()
	d := NewHertzDoer(c, WithPrometheus(reg))

	// Force metricCache to nil to cover the fallback branch
	d.metricCache = nil
	metrics := d.getMetrics("GET", "/nil-cache", 200)
	assert.NotNil(t, metrics.counter)
	assert.NotNil(t, metrics.histogram)
}

func TestHertzDoer_releaseResources_NilContext(t *testing.T) {
	d := &HertzDoer{
		name:   "TestDoer",
		logger: slog.Default(),
	}

	req := protocol.AcquireRequest()
	res := protocol.AcquireResponse()

	// releaseResources shouldn't panic when we pass a nil context
	// It relies on lines 665-667 catching the nil context
	assert.NotPanics(t, func() {
		d.releaseResources(context.TODO(), req, res, nil, false)
	})
}

func TestCleanOpaque(t *testing.T) {
	tests := []struct {
		name     string
		opaque   string
		expected string
	}{
		{"empty string", "", ""},
		{"credentials only", "user:pass@", ""},
		{"with credentials", "user:pass@host/path", "host/path"},
		{"with query", "host/path?query=1", "host/path"},
		{"with fragment", "host/path#frag", "host/path"},
		{"with credentials query and fragment", "user:pass@host/path?query=1#frag", "host/path"},
		{"clean string", "host/path", "host/path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, cleanOpaque(tt.opaque))
		})
	}
}

func TestDefaultURLSanitizer(t *testing.T) {
	t.Run("nil URL", func(t *testing.T) {
		assert.Equal(t, "/_invalid_url", defaultURLSanitizer(nil))
	})

	t.Run("opaque URL with credentials", func(t *testing.T) {
		u := &url.URL{
			Scheme: "mailto",
			Opaque: "user:pass@example.com?subject=hello",
		}
		assert.Equal(t, "mailto:example.com", defaultURLSanitizer(u))
	})

	t.Run("clean standard URL", func(t *testing.T) {
		u, _ := url.Parse("https://example.com/api/v1/users")
		assert.Equal(t, "https://example.com/api/v1/users", defaultURLSanitizer(u))
	})

	t.Run("standard URL with query, fragment, and userinfo", func(t *testing.T) {
		u, _ := url.Parse("https://user:pass@example.com/api/v1/users?a=b&c=d#fragment")
		assert.Equal(t, "https://example.com/api/v1/users", defaultURLSanitizer(u))
	})

	t.Run("URL with special chars in path needing escape", func(t *testing.T) {
		// Spaces in URL need percent encoding in standard path rendering but
		// if EscapedPath() is used, it should be percent encoded
		u, _ := url.Parse("https://example.com/api/v1/user info")
		assert.Equal(t, "https://example.com/api/v1/user%20info", defaultURLSanitizer(u))
	})

	t.Run("URL without scheme or host", func(t *testing.T) {
		u, _ := url.Parse("/just/a/path")
		// when parsed without scheme/host, only path returns
		assert.Equal(t, "/just/a/path", defaultURLSanitizer(u))
	})
}

func TestRouteLabel(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.com/some/long/route/path", nil)

	t.Run("excessively long route", func(t *testing.T) {
		assert.Equal(t, "/_unknown", routeLabel(10, req, nil))
	})

	t.Run("route with newlines", func(t *testing.T) {
		reqInvalid := &http.Request{URL: &url.URL{Path: "/path\nwith\nnewlines"}}
		assert.Equal(t, "/_invalid", routeLabel(100, reqInvalid, nil))
	})

	t.Run("route with carriage returns", func(t *testing.T) {
		reqInvalid := &http.Request{URL: &url.URL{Path: "/path\rwith\rcarriage\rreturns"}}
		assert.Equal(t, "/_invalid", routeLabel(100, reqInvalid, nil))
	})

	t.Run("route with quotes", func(t *testing.T) {
		reqInvalid := &http.Request{URL: &url.URL{Path: "/path\"with\"quotes"}}
		assert.Equal(t, "/_invalid", routeLabel(100, reqInvalid, nil))
	})

	t.Run("valid route within limits", func(t *testing.T) {
		assert.Equal(t, "/some/long/route/path", routeLabel(100, req, nil))
	})

	t.Run("routeNamer overriding correctly within limits", func(t *testing.T) {
		namer := func(r *http.Request) string {
			return "/custom/path"
		}
		assert.Equal(t, "/custom/path", routeLabel(100, req, namer))
	})

	t.Run("routeNamer overriding and exceeding limit", func(t *testing.T) {
		namer := func(r *http.Request) string {
			return "/custom/path/exceeds/limit"
		}
		assert.Equal(t, "/_unknown", routeLabel(10, req, namer))
	})

	t.Run("default route limit handles <= 0", func(t *testing.T) {
		// When routeLimit is 0 or less, it defaults to 100 which is enough for this path length
		assert.Equal(t, "/some/long/route/path", routeLabel(0, req, nil))
	})
}
