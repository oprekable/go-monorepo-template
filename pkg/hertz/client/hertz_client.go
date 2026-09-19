package client

import (
	"crypto/tls"
	"net/url"
	"time"

	hclient "github.com/cloudwego/hertz/pkg/app/client"
	hconfig "github.com/cloudwego/hertz/pkg/common/config"
)

type DurationReadTimeout time.Duration

func NewOptimizedHertzClient(clientName string, baseURL string, readTimeout *DurationReadTimeout) (*hclient.Client, error) {
	var clientReadTimeout time.Duration
	if readTimeout == nil {
		clientReadTimeout = 70 * time.Second
	} else {
		clientReadTimeout = time.Duration(*readTimeout)
	}

	opts := []hconfig.ClientOption{
		hclient.WithName(clientName),
		hclient.WithMaxConnsPerHost(20000),
		hclient.WithMaxIdleConnDuration(120 * time.Second),
		hclient.WithDialTimeout(5 * time.Second),
		hclient.WithClientReadTimeout(clientReadTimeout),
		hclient.WithWriteTimeout(10 * time.Second),
		hclient.WithKeepAlive(true),
		hclient.WithDisablePathNormalizing(true),
		hclient.WithDisableHeaderNamesNormalizing(true),
		hclient.WithNoDefaultUserAgentHeader(true),
	}

	// Only add TLS config for https URLs
	if u, err := url.Parse(baseURL); err == nil && u.Scheme == "https" {
		opts = append(opts, hclient.WithTLSConfig(&tls.Config{
			InsecureSkipVerify: false,
			MinVersion:         tls.VersionTLS12,
		}))
	}

	return hclient.NewClient(opts...)
}
