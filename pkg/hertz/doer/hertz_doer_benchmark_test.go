package doer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/prometheus/client_golang/prometheus"
)

func newBenchClient(b *testing.B, status int, body string) *client.Client {
	b.Helper()
	c, err := client.NewClient()
	if err != nil {
		b.Fatal(err)
	}
	c.Use(func(next client.Endpoint) client.Endpoint {
		return func(ctx context.Context, req *protocol.Request, resp *protocol.Response) error {
			resp.SetStatusCode(status)
			if body != "" {
				resp.SetBodyStream(strings.NewReader(body), len(body))
			}
			return nil
		}
	})
	return c
}

func newBenchDoer(b *testing.B, status int, body string, opts ...Option) *HertzDoer {
	b.Helper()
	return NewHertzDoer(newBenchClient(b, status, body), opts...)
}

func reqToCtxReq(b *testing.B, method string, url string, body io.Reader, ctx context.Context) (req *http.Request) {
	b.Helper()
	req, _ = http.NewRequest(method, url, body)
	req = req.WithContext(ctx)
	return
}

func BenchmarkDo_Success_NoBody(b *testing.B) {
	d := newBenchDoer(b, 200, "", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	req := reqToCtxReq(b, "GET", "https://example.com/api/product", nil, context.Background())

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Success_SmallBody(b *testing.B) {
	body := `{"status":"ok","count":42}`
	d := newBenchDoer(b, 200, body, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	req := reqToCtxReq(b, "GET", "https://example.com/api/data", nil, context.Background())

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Success_LargeBody(b *testing.B) {
	body := strings.Repeat("x", 65536)
	d := newBenchDoer(b, 200, body, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	req := reqToCtxReq(b, "GET", "https://example.com/api/large", nil, context.Background())

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Success_ManyHeaders(b *testing.B) {
	d := newBenchDoer(b, 200, "ok", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	req, _ := http.NewRequest("GET", "https://example.com/api/data", nil)
	ctx := context.Background()
	req = req.WithContext(ctx)
	for i := range 20 {
		req.Header.Set("X-Header-"+string('A'+rune(i%26)), "value")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Success_WithPrometheus(b *testing.B) {
	reg := prometheus.NewRegistry()
	d := newBenchDoer(b, 200, "ok",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithPrometheus(reg),
	)
	req := reqToCtxReq(b, "GET", "https://example.com/api/users", nil, context.Background())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Success_WithRouteNamer(b *testing.B) {
	d := newBenchDoer(b, 200, "ok",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithRouteNamer(func(r *http.Request) string { return "/api/users/{id}" }),
	)
	req, _ := http.NewRequest("GET", "https://example.com/api/users/12345", nil)
	ctx := context.Background()
	req = req.WithContext(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Success_Parallel(b *testing.B) {
	d := newBenchDoer(b, 200, `{"ok":true}`, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := reqToCtxReq(b, "GET", "https://example.com/api/devices", nil, context.Background())
			resp, err := d.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}

func BenchmarkDo_Success_WithPrometheus_Parallel(b *testing.B) {
	reg := prometheus.NewRegistry()
	d := newBenchDoer(b, 200, `{"ok":true}`,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithPrometheus(reg),
	)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("GET", "https://example.com/api/users", nil)
			ctx := context.Background()
			req = req.WithContext(ctx)
			resp, err := d.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
}

func BenchmarkGetMetrics_CacheHit(b *testing.B) {
	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))
	_ = d.getMetrics("GET", "/api/users", 200)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = d.getMetrics("GET", "/api/users", 200)
	}
}

func BenchmarkGetMetrics_CacheMiss(b *testing.B) {
	reg := prometheus.NewRegistry()
	c, _ := client.NewClient()
	d := NewHertzDoer(c, WithPrometheus(reg))
	_ = d.getMetrics("GET", "/api/unique", 200)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = d.getMetrics("POST", "/api/orders", 201)
	}
}

func BenchmarkURLSanitizer(b *testing.B) {
	u, _ := url.Parse("https://user:pass@api.example.com/users/123?token=secret&key=value#section")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = defaultURLSanitizer(u)
	}
}

func BenchmarkRouteLabel(b *testing.B) {
	req, _ := http.NewRequest("GET", "https://example.com/api/users/12345", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = routeLabel(100, req, nil)
	}
}

func BenchmarkReleaseHertzResources(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := protocol.AcquireRequest()
		res := protocol.AcquireResponse()
		releaseHertzResources(cleanupArgs{
			req: req,
			res: res,
			ctx: context.Background(),
		})
	}
}

func BenchmarkBodyWithRelease_ReadClose(b *testing.B) {
	body := strings.NewReader(strings.Repeat("x", 4096))
	req := protocol.AcquireRequest()
	res := protocol.AcquireResponse()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		bw := newBodyWithRelease(context.Background(), body, req, res, nil)
		_, _ = io.Copy(io.Discard, bw)
		_ = bw.Close()
	}
}
