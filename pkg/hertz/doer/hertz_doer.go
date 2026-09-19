package doer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// commonStatusErrors caches error objects for common HTTP error statuses
// to avoid allocating a new error on every non-2xx response.
var commonStatusErrors = func() map[int]error {
	m := make(map[int]error)
	for _, s := range []int{400, 401, 403, 404, 405, 408, 429, 500, 502, 503, 504} {
		m[s] = errors.New("HTTP Status " + strconv.Itoa(s))
	}
	return m
}()

// statusError returns a cached error for common HTTP error statuses,
// falling back to errors.New for uncommon statuses.
func statusError(status int) error {
	if e, ok := commonStatusErrors[status]; ok {
		return e
	}
	return errors.New("HTTP Status " + statusToString(status))
}

const (
	DefaultName                = "HertzDoer"
	DefaultMaxRequestBodySize  = 10 * 1024 * 1024 // 10MB
	DefaultMaxResponseBodySize = 50 * 1024 * 1024 // 50MB
	DefaultRouteLimit          = 100
	AllowedMaxHeaders          = 64
	MaxMetricCacheEntries      = 1000
	maxPoolBufferSize          = 64 * 1024 // 64KB
	maxPooledBuilderCap        = 256       // strings.Builder max cap returned to builderPool
	metricCacheTTL             = 3600      // 1 hour in seconds
	DefaultReserveTime         = 100 * time.Millisecond
	DefaultTimeout             = 10 * time.Second // More aggressive timeout for faster failure detection
	MetricKeyOverhead          = 20               // ":method:route:status"
	DefaultMaxConcurrent       = 1000             // Default max concurrent requests
)

// overflowReader wraps a reader that yields up to limit+1 bytes.
// If more than limit bytes are read, it returns an error instead of EOF,
// so callers can distinguish "body is exactly limit bytes" from "body was truncated".
type overflowReader struct {
	r        io.Reader
	overflow error
	limit    int64
	n        int64
}

func (o *overflowReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	o.n += int64(n)
	if o.n > o.limit {
		return n, o.overflow
	}
	return n, err
}

// overflowErrors caches pre-built overflow errors per body size limit.
// Avoids allocating a new HertzDoerError on every streamed response.
var overflowErrors sync.Map // map[int64]*HertzDoerError

// casBetweenLoadAndCompareHook is a test hook to simulate race conditions in the CAS loop.
// When set, it's called between Load() and CompareAndSwap() in getMetrics.
var casBetweenLoadAndCompareHook func()

// casBetweenReserveAndStoreHook is a test hook to simulate LoadOrStore races.
// When set, it's called between slot reservation and LoadOrStore() in getMetrics.
var casBetweenReserveAndStoreHook func(key string)

// Clock abstracts time operations for deterministic testing.
type Clock interface {
	Now() time.Time
}

// realClock delegates to time.Now.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func getOverflowError(limit int64) *HertzDoerError {
	if e, ok := overflowErrors.Load(limit); ok {
		return e.(*HertzDoerError)
	}
	e := &HertzDoerError{
		Kind:    "validation",
		Message: "response body exceeds max " + strconv.FormatInt(limit, 10) + " bytes",
	}
	actual, _ := overflowErrors.LoadOrStore(limit, e)
	return actual.(*HertzDoerError)
}

type SecurityConfig struct {
	RequireTLS          bool
	MaxRequestBodySize  int64
	MaxResponseBodySize int64
}

// ResilienceConfig groups resilience-related options.
type ResilienceConfig struct {
	// CircuitBreaker enables per-host circuit breaker.
	CircuitBreaker struct {
		Enabled          bool
		FailureThreshold int64
		ResetTimeout     time.Duration
	}

	// ConcurrencyLimiter limits concurrent requests.
	ConcurrencyLimiter struct {
		Enabled       bool
		MaxConcurrent int
	}

	// DefaultTimeout for requests without deadline.
	DefaultTimeout time.Duration

	// ReserveTime is the time reserved for span flushing.
	ReserveTime time.Duration
}

// ObservabilityConfig groups observability-related options.
type ObservabilityConfig struct {
	// Prometheus registry for metrics.
	Prometheus *prometheus.Registry

	// LogSampleRate logs every Nth successful request.
	LogSampleRate uint64

	// SuccessLogLevel for successful requests.
	SuccessLogLevel slog.Level
	// LogRequestPayload logs request body and headers.
	LogRequestPayload bool
	// LogResponsePayload logs response body and headers.
	LogResponsePayload bool

	// CollectPanicStack controls stack trace collection on panic.
	CollectPanicStack bool
}

// RouteNamer example: "/users/123" -> "/users/{id}"
type RouteNamer func(*http.Request) string

// HertzDoer Recommended production config for 10K RPS
/*
  hClient, _ := client.NewClient(
      // Connection pool
      client.WithMaxConnsPerHost(2000),        // 2x expected concurrent requests
      client.WithMaxIdleConnDuration(90*time.Second),
      client.WithMaxConnDuration(10*time.Minute), // force reconnect for DNS updates

      // Timeouts
      client.WithDialTimeout(5*time.Second),
      client.WithReadTimeout(30*time.Second),
      client.WithWriteTimeout(30*time.Second),

      // Keep-alive
      client.WithKeepAlive(true),
      client.WithMaxConnWaitTimeout(5*time.Second),

      // Retry (carefully with idempotency)
      client.WithRetryConfig(
          retry.WithMaxAttemptTimes(2),
          retry.WithInitDelay(100*time.Millisecond),
      ),
  )
*/

type HertzDoer struct {
	bufPool                 sync.Pool
	carrierPool             sync.Pool
	propagator              propagation.TextMapPropagator
	tracer                  trace.Tracer
	metricCache             *MetricCache
	metricCacheHits         prometheus.Counter
	metricCacheMisses       prometheus.Counter
	circuitBreakerOpens     prometheus.Counter
	circuitBreakerRejects   prometheus.Counter
	latencyHist             *prometheus.HistogramVec
	requestCounter          *prometheus.CounterVec
	securityConfig          *SecurityConfig
	client                  *client.Client
	logger                  *slog.Logger
	concurrencyLimiter      chan struct{}
	urlSanitizer            func(*url.URL) string
	routeNamer              func(*http.Request) string
	clock                   Clock
	circuitBreakers         sync.Map
	name                    string
	routeLimit              int
	defaultTimeout          time.Duration
	logSampleRate           uint64
	logSampleCounter        atomic.Uint64
	panicCounter            atomic.Uint64
	reserveTime             time.Duration
	maxRequestBodySize      int64
	circuitBreakerThreshold int64
	circuitBreakerTimeout   time.Duration
	successLogLevel         slog.Level
	maxResponseBodySize     int64
	circuitBreakerEnabled   bool
	collectPanicStack       bool
	logRequestPayload       bool
	logResponsePayload      bool
}

type MetricCollectors struct {
	counter   prometheus.Counter
	histogram prometheus.Observer
}

// metricCacheEntry wraps MetricCollectors with a timestamp for TTL-based eviction.
// Entries not accessed within metricCacheTTL (default 1 hour) are evicted to prevent
// unbounded cache growth from route cardinality explosion.
// Defined in hertz_metric_cache.go.

type Option func(*HertzDoer)

func WithName(fn string) Option {
	return func(d *HertzDoer) {
		d.name = fn
	}
}

func WithTracer(fn trace.Tracer) Option {
	return func(d *HertzDoer) {
		d.tracer = fn
	}
}

func WithLogger(fn *slog.Logger) Option {
	return func(d *HertzDoer) {
		d.logger = fn
	}
}

func WithRouteNamer(fn func(*http.Request) string) Option {
	return func(d *HertzDoer) {
		d.routeNamer = fn
	}
}

func WithRouteLimit(fn int) Option {
	return func(d *HertzDoer) {
		if fn <= 0 {
			fn = DefaultRouteLimit
		}

		d.routeLimit = fn
	}
}

func WithMaxRequestBodySize(fn int64) Option {
	return func(d *HertzDoer) {
		d.maxRequestBodySize = fn
	}
}

func WithMaxResponseBodySize(fn int64) Option {
	return func(d *HertzDoer) {
		d.maxResponseBodySize = fn
	}
}

func WithReserveTime(fn time.Duration) Option {
	return func(d *HertzDoer) {
		d.reserveTime = fn
	}
}

// initMetrics registers all Prometheus metrics on the given registry.
// Called by WithPrometheus and WithObservabilityConfig to avoid duplication.
func (d *HertzDoer) initMetrics(reg *prometheus.Registry) {
	d.requestCounter = promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_client_requests_total",
			Help: "Tracks the number of HTTP requests.",
		},
		[]string{"method", "route", "status"},
	)

	d.latencyHist = promauto.With(reg).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_client_requests_duration_seconds",
			Help:    "Tracks the duration seconds of HTTP requests.",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}, // Tuned for API calls
		},
		[]string{"method", "route"},
	)

	d.metricCacheHits = promauto.With(reg).NewCounter(
		prometheus.CounterOpts{
			Name: "http_client_cache_hits",
			Help: "Tracks the cache hits of HTTP requests.",
		},
	)

	d.metricCacheMisses = promauto.With(reg).NewCounter(
		prometheus.CounterOpts{
			Name: "http_client_cache_misses",
			Help: "Tracks the cache misses of HTTP requests.",
		},
	)

	d.circuitBreakerOpens = promauto.With(reg).NewCounter(
		prometheus.CounterOpts{
			Name: "http_client_circuit_breaker_opens_total",
			Help: "Tracks the number of times circuit breakers have opened.",
		},
	)

	d.circuitBreakerRejects = promauto.With(reg).NewCounter(
		prometheus.CounterOpts{
			Name: "http_client_circuit_breaker_rejects_total",
			Help: "Tracks the number of requests rejected by open circuit breakers.",
		},
	)
}

func WithPrometheus(fn *prometheus.Registry) Option {
	return func(d *HertzDoer) {
		if fn != nil {
			d.initMetrics(fn)
		}
	}
}

// Note: SetMaxKeepBodySize is called on every request because Hertz doesn't expose
// a way to configure this at the client level. This is a per-request setting in Hertz.
// The overhead is minimal (just setting a field on the pooled request/response objects).

func WithSecurityConfig(cfg *SecurityConfig) Option {
	return func(d *HertzDoer) {
		d.securityConfig = cfg
		if cfg.MaxRequestBodySize > 0 {
			d.maxRequestBodySize = cfg.MaxRequestBodySize
		}
		if cfg.MaxResponseBodySize > 0 {
			d.maxResponseBodySize = cfg.MaxResponseBodySize
		}
	}
}

// WithResilienceConfig applies resilience configuration in bulk.
func WithResilienceConfig(cfg ResilienceConfig) Option {
	return func(d *HertzDoer) {
		if cfg.CircuitBreaker.Enabled {
			threshold := cfg.CircuitBreaker.FailureThreshold
			if threshold <= 0 {
				threshold = 5
			}
			timeout := cfg.CircuitBreaker.ResetTimeout
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			d.circuitBreakerEnabled = true
			d.circuitBreakerThreshold = threshold
			d.circuitBreakerTimeout = timeout
		}

		if cfg.ConcurrencyLimiter.Enabled {
			maxConcurrent := cfg.ConcurrencyLimiter.MaxConcurrent
			if maxConcurrent <= 0 {
				maxConcurrent = DefaultMaxConcurrent
			}
			d.concurrencyLimiter = make(chan struct{}, maxConcurrent)
		}

		if cfg.DefaultTimeout > 0 {
			d.defaultTimeout = cfg.DefaultTimeout
		}

		if cfg.ReserveTime > 0 {
			d.reserveTime = cfg.ReserveTime
		}
	}
}

// WithObservabilityConfig applies observability configuration in bulk.
func WithObservabilityConfig(cfg ObservabilityConfig) Option {
	return func(d *HertzDoer) {
		if cfg.Prometheus != nil {
			d.initMetrics(cfg.Prometheus)
		}

		d.logSampleRate = cfg.LogSampleRate
		d.successLogLevel = cfg.SuccessLogLevel
		d.collectPanicStack = cfg.CollectPanicStack
		d.logRequestPayload = cfg.LogRequestPayload
		d.logResponsePayload = cfg.LogResponsePayload
	}
}

func WithURLSanitizer(fn func(*url.URL) string) Option {
	return func(d *HertzDoer) {
		d.urlSanitizer = fn
	}
}

func WithSuccessLogLevel(level slog.Level) Option {
	return func(d *HertzDoer) {
		d.successLogLevel = level
	}
}

// WithLogSampleRate logs every Nth successful request. 1=log every, 100=log 1%, 0=log none.
// Errors are always logged regardless of sample rate.
func WithLogSampleRate(n uint64) Option {
	return func(d *HertzDoer) {
		d.logSampleRate = n
	}
}

func WithDefaultTimeout(timeout time.Duration) Option {
	return func(d *HertzDoer) {
		d.defaultTimeout = timeout
	}
}

// WithCollectPanicStack controls whether stack traces are collected on panic recovery.
// Default: true. Disable in high-throughput production to reduce allocation spikes during panic storms.
func WithCollectPanicStack(collect bool) Option {
	return func(d *HertzDoer) {
		d.collectPanicStack = collect
	}
}

// WithCircuitBreaker enables per-host circuit breaker to prevent cascading failures.
// failureThreshold is the number of failures before the circuit opens.
// resetTimeout is how long the circuit stays open before transitioning to half-open.
// In half-open state, one probe request is allowed; success closes the circuit,
// failure reopens it.
func WithCircuitBreaker(failureThreshold int64, resetTimeout time.Duration) Option {
	return func(d *HertzDoer) {
		if failureThreshold <= 0 {
			failureThreshold = 5
		}
		if resetTimeout <= 0 {
			resetTimeout = 30 * time.Second
		}
		d.circuitBreakerEnabled = true
		d.circuitBreakerThreshold = failureThreshold
		d.circuitBreakerTimeout = resetTimeout
	}
}

// WithConcurrencyLimiter limits the number of concurrent requests to prevent queue buildup.
// maxConcurrent is the maximum number of requests that can be in flight simultaneously.
// Returns ErrConcurrencyLimit when the limit is exceeded.
func WithConcurrencyLimiter(maxConcurrent int) Option {
	return func(d *HertzDoer) {
		if maxConcurrent <= 0 {
			maxConcurrent = DefaultMaxConcurrent
		}
		d.concurrencyLimiter = make(chan struct{}, maxConcurrent)
	}
}

// WithClock injects a custom clock for deterministic time control in tests.
// Defaults to realClock (delegates to time.Now) when not provided.
func WithClock(c Clock) Option {
	return func(d *HertzDoer) {
		d.clock = c
	}
}

func NewHertzDoer(hClient *client.Client, opts ...Option) *HertzDoer {
	d := &HertzDoer{
		name:                DefaultName,
		client:              hClient,
		propagator:          otel.GetTextMapPropagator(),
		maxRequestBodySize:  DefaultMaxRequestBodySize,
		maxResponseBodySize: DefaultMaxResponseBodySize,
		routeLimit:          DefaultRouteLimit,
		successLogLevel:     slog.LevelDebug,
		collectPanicStack:   true, // Default: collect stacks for debugging
	}

	d.bufPool = sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}

	d.carrierPool = sync.Pool{
		New: func() any {
			return new(hertzHeaderCarrier)
		},
	}

	d.urlSanitizer = defaultURLSanitizer
	d.reserveTime = DefaultReserveTime
	d.defaultTimeout = DefaultTimeout
	d.clock = realClock{}

	for _, opt := range opts {
		opt(d)
	}

	// Create MetricCache after all options are applied
	if d.requestCounter != nil && d.latencyHist != nil {
		d.metricCache = NewMetricCache(
			d.requestCounter,
			d.latencyHist,
			d.metricCacheHits,
			d.metricCacheMisses,
		)
	}

	if d.logger == nil {
		d.logger = slog.Default()
	}

	d.logger = d.logger.WithGroup(d.name)

	if d.tracer == nil {
		d.tracer = otel.Tracer(d.name)
	}

	return d
}

// getMetrics returns cached metric collectors for the given method, route, and status.
// Delegates to MetricCache.Get for all caching logic.
func (d *HertzDoer) getMetrics(method, route string, status int) MetricCollectors {
	if d.metricCache == nil {
		return MetricCollectors{
			counter:   d.requestCounter.WithLabelValues(method, route, statusToString(status)),
			histogram: d.latencyHist.WithLabelValues(method, route),
		}
	}
	return d.metricCache.Get(method, route, status, d.clock.Now())
}

// shouldLogSuccess returns true if this success response should be logged based on sample rate.
func (d *HertzDoer) shouldLogSuccess() bool {
	if d.logSampleRate == 0 {
		return false
	}
	if d.logSampleRate == 1 {
		return true
	}
	count := d.logSampleCounter.Add(1)
	// Prevent logging when counter wraps from MaxUint64 to 0
	if count == 0 {
		return false
	}
	return count%d.logSampleRate == 0
}

// shouldLogPanic returns true if this panic should be logged.
// Uses a separate counter to sample panics independently of success logs.
// Always logs the first 10 panics, then samples 1 in 100 to prevent log flooding.
func (d *HertzDoer) shouldLogPanic() bool {
	count := d.panicCounter.Add(1)
	// Prevent logging when counter wraps from MaxUint64 to 0
	if count == 0 {
		return false
	}
	// Always log first 10 panics for debugging
	if count <= 10 {
		return true
	}
	// Then sample 1% to prevent flooding during cascading failures
	return count%100 == 0
}

func (d *HertzDoer) applyPrometheus(duration time.Duration, method string, route string, status int) {
	if d.metricCache == nil {
		return
	}

	// Single cache lookup for both counter + histogram
	mc := d.metricCache.Get(method, route, status, d.clock.Now())
	mc.counter.Inc()
	mc.histogram.Observe(duration.Seconds())
}

// logError records errors to span and logger. Level is hardcoded to Error.
// Logger is taken from the receiver (d.logger).
func (d *HertzDoer) logError(ctx context.Context, span trace.Span, duration time.Duration, err error, url string, method string, route string, status int, isTimeout bool) {
	if span != nil && span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(
			attribute.String("http.method", method),
			attribute.String("http.route", route),
			attribute.String("http.url", url),
			attribute.Int("http.status_code", status),
			attribute.Bool("http.is_timeout", isTimeout),
		)
	}

	if d.logger != nil {
		d.logger.LogAttrs(
			ctx,
			slog.LevelError,
			err.Error(),
			slog.String("http.method", method),
			slog.String("http.route", route),
			slog.String("http.url", url),
			slog.Int("http.status_code", status),
			slog.Bool("http.is_timeout", isTimeout),
			slog.Any("http.err", err),
			slog.Duration("http.duration", duration),
		)
	}
}

// logSuccess records successful requests to span and logger based on sample rate.
// Replaces the free function okAction with a receiver method that uses d.logger.
func (d *HertzDoer) logSuccess(ctx context.Context, span trace.Span, duration time.Duration, message string, url string, method string, route string, status int) {
	if span != nil && span.IsRecording() {
		span.SetStatus(codes.Ok, "OK")
		span.SetAttributes(
			attribute.String("http.method", method),
			attribute.String("http.route", route),
			attribute.String("http.url", url),
			attribute.Int("http.status_code", status),
			attribute.Bool("http.is_timeout", false),
		)
	}

	if d.logger != nil {
		d.logger.LogAttrs(
			ctx,
			d.successLogLevel,
			message,
			slog.String("http.method", method),
			slog.String("http.route", route),
			slog.String("http.url", url),
			slog.Bool("http.is_timeout", false),
			slog.Duration("http.duration", duration),
		)
	}
}

// releaseResources releases Hertz request and response objects back to their pools.
// Logs a warning if an error occurred or if force release is enabled.
func (d *HertzDoer) releaseResources(ctx context.Context, req *protocol.Request, res *protocol.Response, err error, isForceRelease bool) {
	if ctx == nil {
		ctx = context.Background()
	}

	if req != nil {
		protocol.ReleaseRequest(req)
	}

	if res != nil {
		protocol.ReleaseResponse(res)
	}

	if d.logger != nil {
		if err != nil || isForceRelease {
			message := "hertz resource released"
			d.logger.LogAttrs(
				ctx,
				slog.LevelWarn,
				message,
				slog.Any("hertz.resources.release.error", err),
			)
		}
	}
}

func (d *HertzDoer) doWithPanicRecovery(ctx context.Context, span trace.Span, hReq *protocol.Request, hRes *protocol.Response, url, method, route string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Panic message formatting with type switch for common cases
			var panicMsg string
			switch v := r.(type) {
			case error:
				panicMsg = v.Error()
			case string:
				panicMsg = v
			default:
				panicMsg = fmt.Sprintf("%v", v)
			}
			err = errors.New("panic in hertz client.Do: " + panicMsg)

			// Sample panic logs to prevent flooding during cascading failures
			if d.shouldLogPanic() {
				// Conditionally collect stack trace to reduce allocation spikes during panic storms
				if d.collectPanicStack {
					stack := debug.Stack()
					d.logger.ErrorContext(ctx, "recovered from panic",
						slog.Any("panic", r),
						slog.String("stack", string(stack)),
					)
				} else {
					d.logger.ErrorContext(ctx, "recovered from panic (stack collection disabled)",
						slog.Any("panic", r),
					)
				}
			}

			d.logError(
				ctx,
				span,
				time.Duration(0),
				&HertzDoerError{
					Kind:    "recovery",
					Message: "Panic Error",
					Cause:   err,
				},
				url,
				method,
				route,
				0,
				false,
			)
		}
	}()

	return d.client.Do(ctx, hReq, hRes)
}

func (d *HertzDoer) doCheckContentLengthVSMaxRequestBodySize(ctx context.Context, span trace.Span, req *http.Request, url, route string) (err error) {
	if req.ContentLength > d.maxRequestBodySize {
		originalError := errors.New("request body too large: " +
			strconv.FormatInt(req.ContentLength, 10) + " bytes (max " +
			strconv.FormatInt(d.maxRequestBodySize, 10) + ")")

		err = &HertzDoerError{
			Kind:    "validation",
			Message: "HTTP Body Payload Error",
			Cause:   originalError,
		}

		d.logError(
			ctx,
			span,
			time.Duration(0),
			err,
			url,
			req.Method,
			route,
			0,
			false,
		)

		return err
	}

	return nil
}

func (d *HertzDoer) doCheckRequestBody(ctx context.Context, span trace.Span, hReq *protocol.Request, req *http.Request, url, route string) (err error) {
	// Handle case where ContentLength is set but Body is nil.
	if req.ContentLength > 0 && req.Body == nil {
		err = &HertzDoerError{
			Kind:    "validation",
			Message: "request has ContentLength > 0 but Body is nil",
			Cause:   errors.New("ContentLength: " + strconv.FormatInt(req.ContentLength, 10)),
		}
		d.logError(ctx, span, time.Duration(0),
			err, url, req.Method, route, 0, false)
		return err
	}
	if req.Body != nil {
		// Defense in depth: wrap with LimitReader if ContentLength valid
		limitedBody := io.LimitReader(req.Body, d.maxRequestBodySize+1) // +1 for detect overflow

		if req.ContentLength > 0 {
			hReq.SetBodyStream(limitedBody, int(req.ContentLength))
		} else {
			// Unknown length: use pooled buffer
			buf, ok := d.bufPool.Get().(*bytes.Buffer)
			if !ok {
				buf = new(bytes.Buffer) // Fallback if pool is contaminated
			}

			n, copyErr := io.CopyN(buf, limitedBody, d.maxRequestBodySize+1)
			// Return small buffers to pool right away (Hertz copies body data
			// in SetBodyRaw); large buffers are GC'd.
			returnBuf := buf.Cap() <= maxPoolBufferSize
			defer func() {
				if returnBuf {
					buf.Reset()
					d.bufPool.Put(buf)
				}
			}()

			if n > d.maxRequestBodySize {
				err = &HertzDoerError{
					Kind:    "validation",
					Message: "request body exceeds limit (no Content-Length)",
				}
				d.logError(ctx, span, time.Duration(0),
					err, url, req.Method, route, 0, false)
				return err
			}

			if copyErr != nil && copyErr != io.EOF {
				d.logError(ctx, span, time.Duration(0),
					copyErr, url, req.Method, route, 0, false)
				return copyErr
			}

			hReq.SetBodyRaw(buf.Bytes())
		}
	}

	return nil
}

func (d *HertzDoer) Do(req *http.Request) (resp *http.Response, err error) {
	if req.URL == nil {
		return nil, &HertzDoerError{
			Kind:    "validation",
			Message: "request URL is nil",
		}
	}
	if d.securityConfig != nil && d.securityConfig.RequireTLS && req.URL.Scheme != "https" {
		return nil, &HertzDoerError{
			Kind:    "security",
			Message: "TLS required but HTTP scheme used",
		}
	}

	if len(req.Header) > AllowedMaxHeaders {
		return nil, &HertzDoerError{
			Kind:    "validation",
			Message: "too many request headers: " + strconv.Itoa(len(req.Header)) + " > " + strconv.Itoa(AllowedMaxHeaders),
		}
	}

	start := d.clock.Now()
	isTimeout := false
	sanitizedURL := d.urlSanitizer(req.URL)
	route := routeLabel(d.routeLimit, req, d.routeNamer)

	// Circuit breaker: per-host circuit prevents one failing host from blocking others.
	// Checked before span creation so fast-failed requests don't allocate spans.
	var circuit *hostCircuit
	if d.circuitBreakerEnabled {
		host := req.URL.Host
		circuit = circuitFor(&d.circuitBreakers, host)

		if allowed, reason := circuit.allow(d.clock.Now().Unix()); !allowed {
			if d.circuitBreakerRejects != nil {
				d.circuitBreakerRejects.Inc()
			}
			err = &HertzDoerError{
				Kind:    "circuit_open",
				Message: reason + " for host " + host,
			}
			d.logError(req.Context(), nil, 0, err, sanitizedURL, req.Method, route, 0, false)
			return nil, err
		}
	}

	// Backpressure: limit concurrent requests to prevent queue buildup.
	// Checked before span creation so blocked requests don't allocate spans.
	if d.concurrencyLimiter != nil {
		select {
		case d.concurrencyLimiter <- struct{}{}:
			defer func() { <-d.concurrencyLimiter }()
		default:
			err = &HertzDoerError{
				Kind:    "backpressure",
				Message: "concurrency limit exceeded",
			}
			d.logError(req.Context(), nil, 0, err, sanitizedURL, req.Method, route, 0, false)
			return nil, err
		}
	}

	spanName := req.Method + ":" + route
	// Use WithoutCancel for span context to ensure span.End() can flush tracing data
	// even if the parent context is canceled. The span context carries trace/span IDs
	// and attributes, but is decoupled from request cancellation.
	_, span := d.tracer.Start(context.WithoutCancel(req.Context()), spanName)
	defer span.End()

	ctx := trace.ContextWithSpan(req.Context(), span)
	err = d.doCheckContentLengthVSMaxRequestBodySize(
		ctx,
		span,
		req,
		sanitizedURL,
		route,
	)
	if err != nil {
		return nil, err
	}

	hReq := protocol.AcquireRequest()
	hReq.SetMaxKeepBodySize(int(d.maxRequestBodySize))

	hRes := protocol.AcquireResponse()
	hRes.SetMaxKeepBodySize(int(d.maxResponseBodySize))
	defer func() {
		if resp == nil {
			d.releaseResources(ctx, hReq, hRes, err, false)
		}
	}()

	hReq.SetMethod(req.Method)
	hReq.SetRequestURI(req.URL.String())

	if req.Host != "" {
		hReq.SetHost(req.Host)
	}

	err = d.doCheckRequestBody(
		ctx,
		span,
		hReq,
		req,
		sanitizedURL,
		route,
	)
	if err != nil {
		return nil, err
	}

	// Header transfer from http.Request to Hertz request.
	// Single-value headers use Set, multi-value headers use Add.
	for k, vv := range req.Header {
		if len(vv) == 0 {
			continue
		}

		if len(vv) == 1 {
			hReq.Header.Set(k, vv[0])
		} else {
			// Multi-value: must use Add
			for _, v := range vv {
				hReq.Header.Add(k, v)
			}
		}
	}

	// Graceful degradation: tracing injection should never fail the request
	if span.IsRecording() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					if d.logger != nil {
						d.logger.LogAttrs(ctx, slog.LevelError, "tracing injection panicked",
							slog.Any("panic", r),
							slog.String("http.method", req.Method),
							slog.String("http.route", route),
						)
					}
				}
			}()
			carrier := d.carrierPool.Get().(*hertzHeaderCarrier)
			carrier.header = &hReq.Header
			d.propagator.Inject(ctx, carrier)
			// Reset cached keys and header to avoid stale data when carrier is reused
			carrier.keys = nil
			carrier.header = nil
			d.carrierPool.Put(carrier)
		}()
	}

	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= d.reserveTime {
			err = &HertzDoerError{
				Kind:    "timeout",
				Message: "insufficient time remaining",
				Cause:   errors.New("need >" + d.reserveTime.String() + ", got " + remaining.String()),
			}
			d.logError(ctx, span, 0, err, sanitizedURL, req.Method, route, 0, false)
			return nil, err
		}
		ctx, cancel = context.WithTimeout(ctx, remaining-d.reserveTime)
	} else {
		// No deadline set — apply default timeout to prevent goroutine leaks.
		ctx, cancel = context.WithTimeout(ctx, d.defaultTimeout)
	}
	defer cancel()

	// Fast-path: skip hertz call if ctx already dead — avoids unnecessary pool allocations.
	select {
	case <-ctx.Done():
		err = ctx.Err()
		d.logError(ctx, span, 0, err, sanitizedURL, req.Method, route, 0, true)
		return nil, err
	default:
	}

	if d.logRequestPayload && d.logger != nil {
		var headerReqBuf bytes.Buffer
		hReq.Header.VisitAll(func(key, value []byte) {
			headerReqBuf.WriteString(string(key))
			headerReqBuf.WriteString(": ")
			headerReqBuf.WriteString(string(value))
			headerReqBuf.WriteString("\n")
		})

		bodyBytes := hReq.Body()
		if len(bodyBytes) > 0 {
			d.logger.LogAttrs(ctx, slog.LevelInfo, "HTTP Request Payload",
				slog.String("http.request.headers", headerReqBuf.String()),
				slog.String("http.request.body", string(bodyBytes)),
				slog.String("http.method", req.Method),
				slog.String("http.route", route),
				slog.String("http.url", sanitizedURL),
			)
		}
	}

	err = d.doWithPanicRecovery(ctx, span, hReq, hRes, sanitizedURL, req.Method, route)

	duration := time.Since(start)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			isTimeout = true
		}
		d.logError(
			ctx,
			span,
			duration,
			&HertzDoerError{
				Kind:    "http_client",
				Message: "HTTP Outbound Error",
				Cause:   err,
			},
			sanitizedURL,
			req.Method,
			route,
			0,
			isTimeout,
		)

		// Circuit breaker: track failures per-host (exclude client-initiated cancellations)
		if d.circuitBreakerEnabled && circuit != nil && !errors.Is(err, context.Canceled) {
			if circuit.recordFailure(d.clock.Now().Unix(), d.circuitBreakerTimeout, d.circuitBreakerThreshold) {
				if d.circuitBreakerOpens != nil {
					d.circuitBreakerOpens.Inc()
				}
			}
		}

		return nil, err
	}

	status := hRes.StatusCode()
	if d.logResponsePayload && d.logger != nil {
		var bodyStr string
		if s := hRes.BodyStream(); s != protocol.NoResponseBody && s != nil {
			bodyStr = "<streamed response body>"
		} else {
			bodyStr = string(hRes.Body())
		}
		var headerBuf bytes.Buffer
		hRes.Header.VisitAll(func(key, value []byte) {
			headerBuf.WriteString(string(key))
			headerBuf.WriteString(": ")
			headerBuf.WriteString(string(value))
			headerBuf.WriteString("\n")
		})
		d.logger.LogAttrs(ctx, slog.LevelInfo, "HTTP Response Payload",
			slog.String("http.response.headers", headerBuf.String()),
			slog.String("http.response.body", bodyStr),
			slog.Int("http.response.status_code", status),
			slog.String("http.route", route),
		)
	}

	// Circuit breaker: half-open probe succeeded — close the circuit.
	// Any HTTP response (even 4xx/5xx) proves connectivity; downstream
	// errors are tracked separately via the error path above.
	if d.circuitBreakerEnabled && circuit != nil {
		circuit.recordSuccess()
	}

	// Graceful degradation: metrics should never fail the request
	func() {
		defer func() {
			if r := recover(); r != nil {
				if d.logger != nil {
					d.logger.LogAttrs(ctx, slog.LevelError, "metrics recording panicked",
						slog.Any("panic", r),
						slog.String("http.method", req.Method),
						slog.String("http.route", route),
						slog.Int("http.status_code", status),
					)
				}
			}
		}()
		d.applyPrometheus(
			duration,
			req.Method,
			route,
			status,
		)
	}()

	switch {
	case status >= 400:
		d.logError(
			ctx,
			span,
			duration,
			&HertzDoerError{
				Kind:    "status",
				Message: "HTTP Outbound Non-2xx",
				Cause:   statusError(status),
			},
			sanitizedURL,
			req.Method,
			route,
			status,
			isTimeout,
		)
	default:
		if d.shouldLogSuccess() {
			d.logSuccess(
				ctx,
				span,
				duration,
				"HTTP Outbound Success",
				sanitizedURL,
				req.Method,
				route,
				status,
			)
		}
	}

	contentLength := int64(hRes.Header.ContentLength())
	// Preserve -1 (unknown length) for streamed responses.
	// Only normalize when ContentLength() reports a non-negative value.

	headerLen := hRes.Header.Len()
	if headerLen > AllowedMaxHeaders {
		err = &HertzDoerError{
			Kind:    "validation",
			Message: "too many response headers: " + strconv.Itoa(headerLen) + " > " + strconv.Itoa(AllowedMaxHeaders),
		}
		d.logError(ctx, span, duration, err, sanitizedURL, req.Method, route, status, false)
		return nil, err
	}

	// Response header maps are owned by the returned http.Response, so pooling them
	// would never allow return to the pool — use a plain allocation instead.
	responseHeader := make(http.Header, headerLen)
	hRes.Header.VisitAll(func(key, value []byte) {
		k := string(key)
		v := string(value)
		responseHeader[k] = append(responseHeader[k], v)
	})

	var finalBody io.Reader
	if s := hRes.BodyStream(); s != protocol.NoResponseBody && s != nil {
		// Early rejection when ContentLength is known and exceeds limit
		if cl := int64(hRes.Header.ContentLength()); cl > 0 && cl > d.maxResponseBodySize {
			err = &HertzDoerError{
				Kind:    "validation",
				Message: "response body too large: " + strconv.FormatInt(cl, 10) + " bytes > max " + strconv.FormatInt(d.maxResponseBodySize, 10),
			}
			d.logError(ctx, span, duration, err, sanitizedURL, req.Method, route, status, false)
			return nil, err
		}
		// For unknown-length streams, allow limit+1 bytes; overflowReader returns error if >limit bytes read
		finalBody = &overflowReader{
			r:        io.LimitReader(s, d.maxResponseBodySize+1),
			limit:    d.maxResponseBodySize,
			overflow: getOverflowError(d.maxResponseBodySize),
		}
	} else if body := hRes.Body(); len(body) > 0 {
		if int64(len(body)) > d.maxResponseBodySize {
			err = &HertzDoerError{
				Kind:    "validation",
				Message: "response body too large: " + strconv.Itoa(len(body)) + " bytes > max " + strconv.FormatInt(d.maxResponseBodySize, 10),
			}
			d.logError(ctx, span, duration, err, sanitizedURL, req.Method, route, status, false)
			return nil, err
		}
		// Copy body bytes and release hRes immediately to reduce memory pressure.
		// For non-stream bodies, we don't need to hold the response object until Close().
		bodyCopy := make([]byte, len(body))
		copy(bodyCopy, body)
		protocol.ReleaseResponse(hRes)
		hRes = nil
		finalBody = bytes.NewReader(bodyCopy)
	} else {
		// Nobody, release hRes immediately
		protocol.ReleaseResponse(hRes)
		hRes = nil
		finalBody = http.NoBody
	}

	if status == http.StatusNoContent {
		finalBody = http.NoBody
		contentLength = 0
	}

	// The original resp is now responsible for releasing hReq (and hRes for stream bodies)
	// via bodyWithRelease. For non-stream bodies, hRes was already released above.
	// We set resp here so the deferred cleanup knows not to release them.
	resp = &http.Response{
		StatusCode: status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     responseHeader,
		Body: newBodyWithRelease(
			ctx,
			finalBody,
			hReq,
			hRes,
			d.logger,
		),
		ContentLength: contentLength,
		Request:       req,
	}
	return resp, nil
}

// defaultURLSanitizer returns a sanitized URL string for metrics.
// Strips userinfo, query, fragment for security and cardinality.
// Handles opaque URLs (e.g., "mailto:user@example.com") specially.
func defaultURLSanitizer(u *url.URL) string {
	if u == nil {
		return "/_invalid_url"
	}

	// Handle opaque URLs specially (e.g., "mailto:user@example.com")
	if u.Opaque != "" {
		return u.Scheme + ":" + cleanOpaque(u.Opaque)
	}

	// For standard URLs, build directly without intermediate struct copy.
	// This avoids the overhead of url.URL.String() which handles many edge cases
	// we don't need for metrics labels. We only need: scheme://host/path
	// Strips: userinfo, query, fragment (for security and cardinality)
	var sb strings.Builder
	sb.Grow(64) // Typical URL length

	// Write scheme
	if u.Scheme != "" {
		sb.WriteString(u.Scheme)
		sb.WriteString("://")
	}

	// Write host (userinfo is already separate in url.URL struct, so this is safe)
	if u.Host != "" {
		sb.WriteString(u.Host)
	}

	// Write path (use EscapedPath to preserve percent-encoding for malformed URLs)
	if u.Path != "" {
		sb.WriteString(u.EscapedPath())
	}

	return sb.String()
}

func cleanOpaque(opaque string) string {
	if opaque == "" {
		return ""
	}
	// Strip credentials: "user:pass@host/path" -> "host/path"
	if atIdx := strings.Index(opaque, "@"); atIdx >= 0 {
		opaque = opaque[atIdx+1:]
		if opaque == "" {
			return ""
		}
	}
	// Strip query and fragment from opaque part.
	// url.URL.String() uses Opaque verbatim; c.RawQuery="" doesn't help here.
	if qIdx := strings.IndexByte(opaque, '?'); qIdx >= 0 {
		opaque = opaque[:qIdx]
	}
	if hIdx := strings.IndexByte(opaque, '#'); hIdx >= 0 {
		opaque = opaque[:hIdx]
	}
	return opaque
}

// routeLabel extracts and sanitizes the route label from an HTTP request.
//
// The routeNamer callback is invoked on every request (hot path). For optimal
// performance, it should be allocation-free or use caching for complex routing logic.
// Example: using a precompiled regex matcher or a lookup table.
//
// Returns:
//   - "/_unknown" if route exceeds routeLimit (DoS protection)
//   - "/_invalid" if route contains invalid Prometheus label characters
//   - the sanitized route label otherwise
func routeLabel(routeLimit int, req *http.Request, routeNamer func(*http.Request) string) string {
	if routeLimit <= 0 {
		routeLimit = 100
	}

	route := req.URL.Path
	if routeNamer != nil {
		if r := routeNamer(req); r != "" {
			route = r
		}
	}

	// DoS protection: reject excessively long routes
	if len(route) > routeLimit {
		return "/_unknown"
	}

	// Prometheus label validation: prevent metric injection
	if strings.ContainsAny(route, "\n\r\"") {
		return "/_invalid"
	}

	return route
}
