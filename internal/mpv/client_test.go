package mpv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// --- fake mpv endpoint harness ---

// ipcHandler processes one raw IPC request line and returns the reply text
// (any number of \n-terminated lines). This exercises the real framing on
// the client side of a net.Pipe — no filesystem endpoints needed.
type ipcHandler func(requestID int64, raw []byte) string

// fakeDialer serves in-memory pipes; dialScripted failures are consumed
// first (one per Dial), then every connection is served.
type fakeDialer struct {
	handler ipcHandler
	script  []error // queued dial failures
}

func (d *fakeDialer) Dial(ctx context.Context) (Conn, error) {
	if len(d.script) > 0 {
		err := d.script[0]
		d.script = d.script[1:]
		if err != nil {
			return nil, err
		}
	}
	if d.handler == nil {
		// Scripted-dead endpoint: never serves a connection.
		return nil, errors.New("dial: no endpoint")
	}
	client, server := net.Pipe()
	go serveMpv(server, d.handler)
	return NewConn(client), nil
}

func serveMpv(c net.Conn, handler ipcHandler) {
	defer c.Close()
	reader := bufio.NewReader(c)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		raw := bytes.TrimSpace(line)
		var req struct {
			Command   []any `json:"command"`
			RequestID int64 `json:"request_id"`
		}
		if json.Unmarshal(raw, &req) != nil {
			continue
		}
		if _, err := c.Write([]byte(handler(req.RequestID, raw))); err != nil {
			return
		}
	}
}

// handler builders

func replySuccess(data string) ipcHandler {
	return func(id int64, _ []byte) string {
		return fmt.Sprintf(`{"error":"success","request_id":%d,"data":%s}`+"\n", id, data)
	}
}

func replyError(code string) ipcHandler {
	return func(id int64, _ []byte) string {
		return fmt.Sprintf(`{"error":%q,"request_id":%d}`+"\n", code, id)
	}
}

func silence(ipcHandler) ipcHandler { return func(int64, []byte) string { return "" } }

// capture records the raw wire line for framing assertions.
type capture struct{ line []byte }

func withCapture(c *capture, inner ipcHandler) ipcHandler {
	return func(id int64, raw []byte) string {
		c.line = append([]byte(nil), raw...)
		return inner(id, raw)
	}
}

// --- framing tests ---

func TestCallSuccessReturnsData(t *testing.T) {
	var wire capture
	dialer := &fakeDialer{handler: withCapture(&wire, replySuccess(`"hello"`))}
	client := NewClient(dialer)

	raw, err := client.Call(context.Background(), []any{"get_property", "media-title"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	s, err := PropString(raw)
	if err != nil || s != "hello" {
		t.Fatalf("PropString = %q, %v; want hello", s, err)
	}

	// The exact on-wire shape (parity with plaza-player's request()).
	var req map[string]any
	if err := json.Unmarshal(wire.line, &req); err != nil {
		t.Fatalf("wire line is not the command object: %v", err)
	}
	if got := req["request_id"].(float64); got != 1 {
		t.Errorf("request_id = %v, want 1 on a fresh connection", got)
	}
	if cmd, ok := req["command"].([]any); !ok || len(cmd) != 2 {
		t.Errorf("command = %v, want [get_property media-title]", req["command"])
	}
}

func TestCallSkipsEventsAndMalformedLines(t *testing.T) {
	noise := ipcHandler(func(id int64, _ []byte) string {
		return "cannot even json\n" +
			`{"event":"end-file"}` + "\n" +
			"\n" +
			fmt.Sprintf(`{"error":"success","request_id":%d,"data":42}`+"\n", id)
	})
	client := NewClient(&fakeDialer{handler: noise})

	raw, err := client.Call(context.Background(), []any{"get_property", "volume"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if n, err := PropInt(raw); err != nil || n != 42 {
		t.Fatalf("PropInt = %d, %v; want 42", n, err)
	}
}

func TestCallMPVErrorSurfaces(t *testing.T) {
	client := NewClient(&fakeDialer{handler: replyError("property unavailable")})
	_, err := client.Call(context.Background(), []any{"get_property", "nope"})
	if err == nil || err.Error() != "property unavailable" {
		t.Fatalf("err = %v, want mpv error text", err)
	}
}

func TestCallEOFWithoutReply(t *testing.T) {
	client := NewClient(&fakeDialer{handler: silence(nil)})
	_, err := client.Call(context.Background(), []any{"get_property", "volume"})
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("err = %v, want ErrNoResponse", err)
	}
}

func TestCallDeadlineIsNoResponse(t *testing.T) {
	slow := ipcHandler(func(int64, []byte) string {
		time.Sleep(200 * time.Millisecond)
		return ""
	})
	client := NewClient(&fakeDialer{handler: slow})
	_, err := client.CallTimeout(context.Background(), []any{"quit"}, 30*time.Millisecond)
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("err = %v, want ErrNoResponse", err)
	}
}

func TestResponseTooLargeCapped(t *testing.T) {
	big := ipcHandler(func(id int64, _ []byte) string {
		return strings.Repeat("a", 70*1024) + "\n"
	})
	client := NewClient(&fakeDialer{handler: big})
	_, err := client.Call(context.Background(), []any{"get_property", "volume"})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
}

// --- client property wrappers ---

func TestGetPropertyAndSetProperty(t *testing.T) {
	client := NewClient(&fakeDialer{handler: replySuccess(`55.7`)})
	raw, err := client.GetProperty(context.Background(), "volume")
	if err != nil {
		t.Fatalf("GetProperty: %v", err)
	}
	if n, _ := PropInt(raw); n != 56 { // mpv JSON number arrives as float; rounds
		t.Fatalf("volume = %d, want 56", n)
	}
}

func TestDialFailureWrapped(t *testing.T) {
	client := NewClient(&fakeDialer{script: []error{errors.New("connection refused")}})
	_, err := client.Call(context.Background(), []any{"quit"})
	if err == nil || !strings.Contains(err.Error(), "dial: connection refused") {
		t.Fatalf("err = %v, want wrapped dial error", err)
	}
}
