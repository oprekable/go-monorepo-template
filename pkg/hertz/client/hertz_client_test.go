package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/errors"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/stretchr/testify/assert"
)

func TestNewOptimizedHertzClient(t *testing.T) {
	type args struct {
		readTimeout *DurationReadTimeout
		clientName  string
		baseURL     string
	}

	tests := []struct {
		args    args
		wantErr error
		want    *hconfig.ClientOptions
		name    string
	}{
		{
			name: "Ok - readTimeout not nil",
			args: args{
				clientName: "test",
				baseURL:    "https://example.com",
				readTimeout: func() *DurationReadTimeout {
					d := 1 * time.Second
					return (*DurationReadTimeout)(&d)
				}(),
			},
			want: &hconfig.ClientOptions{
				Name:                          "test",
				MaxConnsPerHost:               20000,
				MaxIdleConnDuration:           120 * time.Second,
				DialTimeout:                   5 * time.Second,
				ReadTimeout:                   1 * time.Second,
				WriteTimeout:                  10 * time.Second,
				KeepAlive:                     true,
				DisablePathNormalizing:        true,
				DisableHeaderNamesNormalizing: true,
				NoDefaultUserAgentHeader:      true,
			},
			wantErr: nil,
		},
		{
			name: "Ok - readTimeout nil",
			args: args{
				clientName:  "test",
				baseURL:     "https://example.com",
				readTimeout: nil,
			},
			want: &hconfig.ClientOptions{
				Name:                          "test",
				MaxConnsPerHost:               20000,
				MaxIdleConnDuration:           120 * time.Second,
				DialTimeout:                   5 * time.Second,
				ReadTimeout:                   70 * time.Second,
				WriteTimeout:                  10 * time.Second,
				KeepAlive:                     true,
				DisablePathNormalizing:        true,
				DisableHeaderNamesNormalizing: true,
				NoDefaultUserAgentHeader:      true,
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClient, err := NewOptimizedHertzClient(tt.args.clientName, tt.args.baseURL, tt.args.readTimeout)

			assert.Equalf(t, tt.wantErr, err, "NewOptimizedHertzClient(%v).error", tt.args.clientName)

			assert.Equalf(t, tt.want.Name, gotClient.GetOptions().Name, "NewOptimizedHertzClient(%v).GetOptions().Name", tt.args.clientName)
			assert.Equalf(t, tt.want.MaxConnsPerHost, gotClient.GetOptions().MaxConnsPerHost, "NewOptimizedHertzClient(%v).GetOptions().MaxConnsPerHost", tt.args.clientName)
			assert.Equalf(t, tt.want.DialTimeout, gotClient.GetOptions().DialTimeout, "NewOptimizedHertzClient(%v).GetOptions().DialTimeout", tt.args.clientName)
			assert.Equalf(t, tt.want.ReadTimeout, gotClient.GetOptions().ReadTimeout, "NewOptimizedHertzClient(%v).GetOptions().ReadTimeout", tt.args.clientName)
			assert.Equalf(t, tt.want.WriteTimeout, gotClient.GetOptions().WriteTimeout, "NewOptimizedHertzClient(%v).GetOptions().WriteTimeout", tt.args.clientName)
			assert.Equalf(t, tt.want.KeepAlive, gotClient.GetOptions().KeepAlive, "NewOptimizedHertzClient(%v).GetOptions().KeepAlive", tt.args.clientName)
			assert.Equalf(t, tt.want.DisablePathNormalizing, gotClient.GetOptions().DisablePathNormalizing, "NewOptimizedHertzClient(%v).GetOptions().DisablePathNormalizing", tt.args.clientName)
			assert.Equalf(t, tt.want.DisableHeaderNamesNormalizing, gotClient.GetOptions().DisableHeaderNamesNormalizing, "NewOptimizedHertzClient(%v).GetOptions().DisableHeaderNamesNormalizing", tt.args.clientName)
			assert.Equalf(t, tt.want.NoDefaultUserAgentHeader, gotClient.GetOptions().NoDefaultUserAgentHeader, "NewOptimizedHertzClient(%v).GetOptions().NoDefaultUserAgentHeader", tt.args.clientName)
			assert.Equalf(t, tt.want.MaxIdleConnDuration, gotClient.GetOptions().MaxIdleConnDuration, "NewOptimizedHertzClient(%v).GetOptions().MaxIdleConnDuration", tt.args.clientName)
		})
	}
}

func TestNewOptimizedHertzClient_DoRequest_Timeout(t *testing.T) {
	type args struct {
		readTimeout    *DurationReadTimeout
		httpTestServer func() *httptest.Server
		clientName     string
		baseURL        string
	}

	tests := []struct {
		args    args
		wantErr error
		want    *hconfig.ClientOptions
		name    string
	}{
		{
			name: "Ok",
			args: args{
				clientName: "test",
				baseURL:    "http://localhost",
				readTimeout: func() *DurationReadTimeout {
					d := 1 * time.Second
					return (*DurationReadTimeout)(&d)
				}(),
				httpTestServer: func() *httptest.Server {
					return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						time.Sleep(1*time.Second - 100*time.Millisecond)
						w.WriteHeader(200)
					}))
				},
			},

			wantErr: nil,
		},
		{
			name: "Timeout",
			args: args{
				clientName: "test",
				baseURL:    "http://localhost",
				readTimeout: func() *DurationReadTimeout {
					d := 1 * time.Second
					return (*DurationReadTimeout)(&d)
				}(),
				httpTestServer: func() *httptest.Server {
					return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						time.Sleep(1*time.Second + 100*time.Millisecond)
						w.WriteHeader(200)
					}))
				},
			},

			wantErr: errors.ErrTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := tt.args.httpTestServer()
			defer server.Close()

			hClient, _ := NewOptimizedHertzClient(tt.args.clientName, tt.args.baseURL, tt.args.readTimeout)

			req := protocol.AcquireRequest()
			resp := protocol.AcquireResponse()

			req.SetRequestURI(server.URL)
			req.Header.SetMethod("GET")

			err := hClient.Do(context.Background(), req, resp)
			assert.Equalf(t, tt.wantErr, err, "NewOptimizedHertzClient(%v).error", tt.args.clientName)
		})
	}
}
