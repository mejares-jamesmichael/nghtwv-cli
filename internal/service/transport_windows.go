//go:build windows

package service

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// newDialer dials the service's named-pipe endpoint.
func newDialer(endpoint string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		return winio.DialPipeContext(ctx, endpoint)
	}
}

// Listen binds the service's named pipe. Dead pipes leave no filesystem
// artifact, so there is nothing to clean first.
func Listen(endpoint string) (net.Listener, error) {
	return winio.ListenPipe(endpoint, nil)
}
