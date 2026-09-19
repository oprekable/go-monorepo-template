package doer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/stretchr/testify/assert"
)

// syncBuffer is a thread-safe wrapper around bytes.Buffer
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *syncBuffer) Bytes() []byte {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Bytes()
}

func (sb *syncBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

// MockCloser is a simple io.ReadCloser implementation for testing.
type MockCloser struct {
	closeErr error
	*bytes.Reader
	closeCount atomic.Int32
}

func (m *MockCloser) Close() error {
	m.closeCount.Add(1)
	return m.closeErr
}

func (m *MockCloser) CloseCount() int {
	return int(m.closeCount.Load())
}

func Test_newBodyWithRelease(t *testing.T) {
	type args struct {
		ctx    context.Context
		body   io.Reader
		req    *protocol.Request
		res    *protocol.Response
		logger *slog.Logger
	}
	tests := []struct {
		setup func() args
		check func(t *testing.T, b *bodyWithRelease)
		name  string
	}{
		{
			name: "create_with_all_args",
			setup: func() args {
				return args{
					ctx:    context.Background(),
					body:   bytes.NewReader(nil),
					req:    protocol.AcquireRequest(),
					res:    protocol.AcquireResponse(),
					logger: slog.Default(),
				}
			},
			check: func(t *testing.T, b *bodyWithRelease) {
				assert.NotNil(t, b)
				assert.NotNil(t, b.bodyReader)
				assert.NotNil(t, b.req)
				assert.NotNil(t, b.res)
				assert.NotNil(t, b.logger)
				assert.NotNil(t, b.cleanup)
			},
		},
		{
			name: "create_with_nil_logger",
			setup: func() args {
				return args{
					ctx:    context.Background(),
					body:   bytes.NewReader(nil),
					req:    protocol.AcquireRequest(),
					res:    protocol.AcquireResponse(),
					logger: nil,
				}
			},
			check: func(t *testing.T, b *bodyWithRelease) {
				assert.NotNil(t, b)
				assert.Equal(t, slog.Default(), b.logger)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := tt.setup()
			got := newBodyWithRelease(args.ctx, args.body, args.req, args.res, args.logger)
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func Test_bodyWithRelease_Read(t *testing.T) {
	testData := []byte("hello world")

	type fields struct {
		bodyReader io.Reader
	}
	type args struct {
		p []byte
	}
	tests := []struct {
		fields fields
		check  func(t *testing.T, byteToRead []byte, n int, err error)
		name   string
		args   args
	}{
		{
			name: "successful_read",
			fields: fields{
				bodyReader: bytes.NewReader(testData),
			},
			args: args{
				p: make([]byte, len(testData)),
			},
			check: func(t *testing.T, byteToRead []byte, n int, err error) {
				assert.Equal(t, byteToRead, testData)
				assert.Equal(t, len(byteToRead), n)
				assert.NoError(t, err)
			},
		},
		{
			name: "read_eof",
			fields: fields{
				bodyReader: bytes.NewReader([]byte{}),
			},
			args: args{
				p: make([]byte, 1),
			},
			check: func(t *testing.T, byteToRead []byte, n int, err error) {
				assert.Equal(t, 0, n)
				assert.Error(t, err)
			},
		},
		{
			name: "read_from_nil_body",
			fields: fields{
				bodyReader: nil,
			},
			args: args{
				p: make([]byte, 1),
			},
			check: func(t *testing.T, byteToRead []byte, n int, err error) {
				assert.Equal(t, 0, n)
				assert.Error(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &bodyWithRelease{
				bodyReader: tt.fields.bodyReader,
			}

			gotN, err := b.Read(tt.args.p)
			if tt.check != nil {
				tt.check(t, tt.args.p, gotN, err)
			}
		})
	}
}

func Test_bodyWithRelease_Close(t *testing.T) {
	tests := []struct {
		check func(t *testing.T)
		name  string
	}{
		{
			name: "close_with_no_error",
			check: func(t *testing.T) {
				mockReader := &MockCloser{Reader: bytes.NewReader(nil)}
				body := newBodyWithRelease(context.Background(), mockReader, protocol.AcquireRequest(), protocol.AcquireResponse(), nil)
				err := body.Close()
				assert.NoError(t, err)
				assert.Equal(t, 1, mockReader.CloseCount())
			},
		},
		{
			name: "close_with_error_from_reader",
			check: func(t *testing.T) {
				closeErr := errors.New("close error")
				mockReader := &MockCloser{Reader: bytes.NewReader(nil), closeErr: closeErr}
				body := newBodyWithRelease(context.Background(), mockReader, protocol.AcquireRequest(), protocol.AcquireResponse(), nil)
				err := body.Close()
				assert.Error(t, err)
				assert.Equal(t, closeErr, err)
				assert.Equal(t, 1, mockReader.CloseCount())
			},
		},
		{
			name: "multiple_closes",
			check: func(t *testing.T) {
				mockReader := &MockCloser{Reader: bytes.NewReader(nil)}
				body := newBodyWithRelease(context.Background(), mockReader, protocol.AcquireRequest(), protocol.AcquireResponse(), nil)
				_ = body.Close()
				_ = body.Close()
				_ = body.Close()
				assert.Equal(t, 1, mockReader.CloseCount())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.check(t)
		})
	}
}

func TestBodyWithRelease_RuntimeCleanup(t *testing.T) {
	buf := &syncBuffer{}
	logger := getLogger(t, buf, time.Now())
	cleanupErr := errors.New("cleanup triggered by GC")

	// This function scopes the lifetime of the bodyWithRelease object
	func() {
		req := protocol.AcquireRequest()
		res := protocol.AcquireResponse()
		// We create the body but never call Close() on it.
		// When this function returns, the object is no longer reachable.
		_ = newBodyWithReleaseWithCleanupError(context.Background(), nil, req, res, logger, cleanupErr)
	}() // The object is now eligible for GC

	// Force garbage collection to run, which should trigger the cleanup function.
	runtime.GC()

	// The cleanup function logs an error. We wait a moment for the log to be written.
	assert.Eventually(
		t, func() bool {
			return bytes.Contains(buf.Bytes(), []byte(cleanupErr.Error()))
		},
		2*time.Second,
		10*time.Millisecond,
		"Expected log message from runtime cleanup was not found",
	)

	logOutput := buf.String()
	assert.Contains(t, logOutput, `"level":"WARN"`)
	assert.Contains(t, logOutput, `"msg":"hertz resource released"`)
	assert.Contains(t, logOutput, `"hertz.resources.release.error":"cleanup triggered by GC"`)
}

// newBodyWithReleaseWithCleanupError is a helper to inject an error for the cleanup function
func newBodyWithReleaseWithCleanupError(ctx context.Context, body io.Reader, req *protocol.Request, res *protocol.Response, logger *slog.Logger, err error) *bodyWithRelease {
	if logger == nil {
		logger = slog.Default()
	}

	b := &bodyWithRelease{
		bodyReader: body,
		req:        req,
		res:        res,
		logger:     logger,
	}

	b.cleanup = runtime.AddCleanup(
		b,
		releaseHertzResources,
		cleanupArgs{
			req:    req,
			res:    res,
			logger: logger,
			ctx:    nil, // Use nil to ensure default context handling is also tested
			err:    err, // Inject the error here
		},
	)

	return b
}
