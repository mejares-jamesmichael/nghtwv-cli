//go:build windows

package mpv

import (
	"context"

	"github.com/Microsoft/go-winio"
)

// PipeDialer dials nghtwv's named-pipe IPC endpoint (\\.\pipe\nghtwv-mpv).
type PipeDialer struct {
	Name string
}

// Dial opens a fresh pipe connection for one command exchange.
func (d PipeDialer) Dial(ctx context.Context) (Conn, error) {
	nc, err := winio.DialPipeContext(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return NewConn(nc), nil
}

// NewDialer picks the OS-appropriate dialer for an endpoint address
// (build-tag selected: socket path on Linux, pipe name on Windows).
func NewDialer(endpoint string) Dialer {
	return PipeDialer{Name: endpoint}
}
