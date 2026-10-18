package mpv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// Framing limits and timing mirror plaza-player's request() exactly:
// a 1.5 s default command timeout and a 64 KiB response accumulation cap.
const (
	DefaultCommandTimeout = 1500 * time.Millisecond
	ResponseLimit         = 64 * 1024
)

// Handshake timing (supervisor startup probe, parity values; overridable
// per instance for tests).
const (
	DefaultHandshakeDeadline = 5 * time.Second
	DefaultHandshakeProbe    = 300 * time.Millisecond
	DefaultHandshakeInterval = 50 * time.Millisecond
)

// ErrNoResponse matches plaza-player's terminal failure: nothing answered
// (dead endpoint, EOF, or deadline) before a matching reply arrived.
var ErrNoResponse = errors.New("no response from mpv")

// ErrResponseTooLarge exceeds the 64 KiB accumulation cap.
var ErrResponseTooLarge = errors.New("mpv response too large")

// MPVError is a command mpv itself reported failing (error != success);
// the string is mpv's own error code, e.g. "property unavailable".
type MPVError string

func (e MPVError) Error() string { return string(e) }

// Response is one decoded mpv JSON IPC reply line.
// Other lines (events, foreign replies) are skipped during matching.
type Response struct {
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
	RequestID int64           `json:"request_id"`
}

// Conn carries out one IPC command exchange and is then discarded.
// Implementations are not safe for concurrent use.
type Conn interface {
	// Call writes the command and returns the data payload of the reply
	// whose request_id matches. Events and unmatched replies are skipped.
	Call(ctx context.Context, command []any, timeout time.Duration) (json.RawMessage, error)
	// Close releases the underlying connection.
	Close() error
}

// ipcConn implements Conn over any established net.Conn speaking mpv's
// newline-delimited JSON IPC.
type ipcConn struct {
	nc     net.Conn
	nextID int64
}

// NewConn adapts an established connection to the mpv IPC framing. Dialers
// (SocketDialer/PipeDialer) hand their result here.
func NewConn(nc net.Conn) Conn {
	return &ipcConn{nc: nc}
}

func (c *ipcConn) Call(ctx context.Context, command []any, timeout time.Duration) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)

	payload, err := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{Command: command, RequestID: id})
	if err != nil {
		return nil, fmt.Errorf("encode command: %w", err)
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d // the caller's context keeps veto power
	}
	if err := c.nc.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	if _, err := c.nc.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	reader := bufio.NewReader(c.nc)
	total := 0
	for {
		line, rerr := reader.ReadBytes('\n')
		total += len(line)
		if total > ResponseLimit {
			return nil, ErrResponseTooLarge
		}

		trimmed := bytes.TrimSpace(line)
		if rerr == nil || len(trimmed) > 0 {
			var resp Response
			if uerr := json.Unmarshal(trimmed, &resp); uerr != nil {
				// Malformed line: skip it, mirroring the plugin's tolerance.
				trimmed = nil
			}
			if len(trimmed) > 0 {
				if resp.RequestID != id {
					// mpv event or a foreign reply: keep scanning.
				} else if resp.Error != "success" {
					return nil, MPVError(resp.Error)
				} else {
					return resp.Data, nil
				}
			}
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, os.ErrDeadlineExceeded) {
				return nil, ErrNoResponse
			}
			return nil, fmt.Errorf("read: %w", rerr)
		}
	}
}

func (c *ipcConn) Close() error {
	return c.nc.Close()
}
