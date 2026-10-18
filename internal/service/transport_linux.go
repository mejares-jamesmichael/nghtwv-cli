//go:build linux

package service

import (
	"context"
	"net"
	"os"
)

// newDialer dials the service's Unix socket endpoint.
func newDialer(endpoint string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", endpoint)
	}
}

// Listen binds the service endpoint. A stale socket file (crashed service)
// is removed first: the launch lock serialises starts, so this can never
// steal an endpoint a live service is answering on.
func Listen(endpoint string) (net.Listener, error) {
	_ = os.Remove(endpoint)
	return net.Listen("unix", endpoint)
}
