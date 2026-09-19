package doer

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"

	"github.com/cloudwego/hertz/pkg/protocol"
)

// cleanupArgs bundles arguments for the releaseHertzResources callback
// registered with runtime.AddCleanup. Must remain a standalone struct
// because AddCleanup requires a single-arg function.
type cleanupArgs struct {
	req            *protocol.Request
	res            *protocol.Response
	logger         *slog.Logger
	ctx            context.Context
	err            error
	isForceRelease bool
}

// releaseHertzResources is the cleanup callback for runtime.AddCleanup.
// Standalone function required by AddCleanup's func(T) signature.
func releaseHertzResources(args cleanupArgs) {
	if args.ctx == nil {
		args.ctx = context.Background()
	}

	if args.req != nil {
		protocol.ReleaseRequest(args.req)
	}

	if args.res != nil {
		protocol.ReleaseResponse(args.res)
	}

	if args.logger != nil {
		if args.err != nil || args.isForceRelease {
			args.logger.LogAttrs(
				args.ctx,
				slog.LevelWarn,
				"hertz resource released",
				slog.Any("hertz.resources.release.error", args.err),
			)
		}
	}
}

type bodyWithRelease struct {
	bodyReader io.Reader
	closeErr   error
	req        *protocol.Request
	res        *protocol.Response
	logger     *slog.Logger
	cleanup    runtime.Cleanup
	once       sync.Once
}

func newBodyWithRelease(ctx context.Context, body io.Reader, req *protocol.Request, res *protocol.Response, logger *slog.Logger) *bodyWithRelease {
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
			req:            req,
			res:            res,
			logger:         logger,
			ctx:            ctx,
			isForceRelease: true,
		},
	)

	return b
}

func (b *bodyWithRelease) Read(p []byte) (n int, err error) {
	if b.bodyReader == nil {
		return 0, io.EOF
	}
	return b.bodyReader.Read(p)
}

func (b *bodyWithRelease) Close() error {
	b.once.Do(func() {
		b.cleanup.Stop() // abort auto cleanup because manual released
		if closer, ok := b.bodyReader.(io.Closer); ok {
			b.closeErr = closer.Close()
		}

		if b.req != nil {
			protocol.ReleaseRequest(b.req)
		}

		if b.res != nil {
			protocol.ReleaseResponse(b.res)
		}
	})
	return b.closeErr
}
