package gateway

import (
	"context"
	"net"
	"net/http"
	"time"
)

// NewUpstreamClient builds the shared outbound client.
//
// Deliberately NO global http.Client.Timeout: it would kill long streams.
// Deadlines come from the per-request context (non-stream cap, stream idle
// watchdog handled by the relay).
func NewUpstreamClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Generous header timeout: reasoning models can take minutes before
		// response headers when upstreams buffer until first token.
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &http.Client{Transport: transport}
}

// contextWithCap adds a total deadline for non-streaming requests only.
func contextWithCap(ctx context.Context, stream bool, nonStreamCap time.Duration) (context.Context, context.CancelFunc) {
	if stream {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, nonStreamCap)
}
