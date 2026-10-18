//go:build linux

package mpv

import (
	"context"
	"net"
)

// SocketDialer dials nghtwv's Unix-socket IPC endpoint. The path comes from
// internal/platform ($XDG_RUNTIME_DIR/nghtwv/mpv.sock).
type SocketDialer struct {
	Path string
}

// Dial opens a fresh Unix socket for one command exchange.
func (d SocketDialer) Dial(ctx context.Context) (Conn, error) {
	var netDialer net.Dialer
	nc, err := netDialer.DialContext(ctx, "unix", d.Path)
	if err != nil {
		return nil, err
	}
	return NewConn(nc), nil
}

// NewDialer picks the OS-appropriate dialer for an endpoint address
// (build-tag selected: socket path on Linux, pipe name on Windows).
func NewDialer(endpoint string) Dialer {
	return SocketDialer{Path: endpoint}
}
