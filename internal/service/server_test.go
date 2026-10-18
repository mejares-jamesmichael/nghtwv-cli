package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// fakeBackend records dispatched verbs and returns canned results.
type fakeBackend struct {
	mu    sync.Mutex
	calls []string
	state player.State
	err   error
}

func (f *fakeBackend) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeBackend) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBackend) Status(context.Context) player.State {
	f.record("status")
	return f.state
}

func (f *fakeBackend) Play(context.Context) (player.State, error) {
	f.record("play")
	return f.state, f.err
}

func (f *fakeBackend) Pause(context.Context) (player.State, error) {
	f.record("pause")
	return f.state, f.err
}

func (f *fakeBackend) Toggle(context.Context) (player.State, error) {
	f.record("toggle")
	return f.state, f.err
}

func (f *fakeBackend) Stop(context.Context) error {
	f.record("stop")
	return f.err
}

func (f *fakeBackend) Volume(_ context.Context, volume int) (player.State, error) {
	f.record(fmt.Sprintf("volume:%d", volume))
	return f.state, f.err
}

func (f *fakeBackend) Outputs(context.Context) ([]mpv.Device, error) {
	f.record("outputs")
	if f.err != nil {
		return nil, f.err
	}
	return []mpv.Device{{Name: "auto", Description: "Autoselect"}, {Name: "sink-1", Description: "Desk"}}, nil
}

func (f *fakeBackend) SetOutput(_ context.Context, device string) (player.State, error) {
	f.record(fmt.Sprintf("output:%q", device))
	return f.state, f.err
}

// testServer runs a server on a loopback listener and returns its address
// plus a channel carrying Serve's result.
func testServer(t *testing.T, backend Backend, proto int) (string, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(backend, ln)
	if proto != 0 {
		srv.SetProtocol(proto)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		default:
		}
	})
	return ln.Addr().String(), done
}

// tcpDialer dials loopback test endpoints.
func tcpDialer(addr string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// testClient wires a client against a loopback test server with an isolated
// launch lock, so ensuring verbs attach instead of starting anything.
func testClient(t *testing.T, addr string) *Client {
	t.Helper()
	return NewClient(Config{
		Endpoint: addr,
		LockPath: filepath.Join(t.TempDir(), "service.lock"),
		Dial:     tcpDialer(addr),
		Store:    fakeStore{},
	})
}

// rawCall sends one raw request line and decodes the reply.
func rawCall(t *testing.T, addr, line string) Reply {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer nc.Close()
	if _, err := nc.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw, err := bufio.NewReader(nc).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var reply Reply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("reply %q is not JSON: %v", raw, err)
	}
	return reply
}

func TestServerPingIsCheap(t *testing.T) {
	backend := &fakeBackend{}
	addr, _ := testServer(t, backend, 0)

	reply := rawCall(t, addr, `{"id":1,"proto":1,"cmd":"ping"}`)
	if !reply.OK || reply.Proto != Protocol {
		t.Fatalf("ping reply = %+v", reply)
	}
	if got := backend.called(); len(got) != 0 {
		t.Fatalf("ping must not disturb the backend, calls = %v", got)
	}
}

func TestServerBadRequests(t *testing.T) {
	backend := &fakeBackend{}
	addr, _ := testServer(t, backend, 0)

	cases := []struct {
		name string
		line string
		code string
	}{
		{"garbage", `not json`, CodeBadRequest},
		{"unknown verb", `{"id":5,"proto":1,"cmd":"fly"}`, CodeUnknownCommand},
		{"volume args", `{"id":6,"proto":1,"cmd":"volume","args":{"value":"loud"}}`, CodeBadRequest},
		{"output args", `{"id":7,"proto":1,"cmd":"output","args":{"device":7}}`, CodeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply := rawCall(t, addr, c.line)
			if reply.OK {
				t.Fatalf("reply reported ok for a bad request: %+v", reply)
			}
			if reply.Code != c.code {
				t.Fatalf("code = %q, want %q (reply %+v)", reply.Code, c.code, reply)
			}
			if reply.Proto != Protocol {
				t.Fatalf("reply proto = %d, want %d", reply.Proto, Protocol)
			}
		})
	}

	// Rule 1: unknown fields are ignored, not rejected.
	reply := rawCall(t, addr, `{"id":9,"proto":1,"cmd":"play","future":{"x":1}}`)
	if !reply.OK {
		t.Fatalf("unknown fields must be ignored, got %+v", reply)
	}
	if got := backend.called(); len(got) != 1 || got[0] != "play" {
		t.Fatalf("backend calls = %v, want [play]", got)
	}
}

func TestServerRejectsProtocolMismatch(t *testing.T) {
	backend := &fakeBackend{state: player.State{Playing: true, Volume: 70}}
	addr, done := testServer(t, backend, 99)

	reply := rawCall(t, addr, `{"id":1,"proto":1,"cmd":"play"}`)
	if reply.OK || reply.Code != CodeProtocolMismatch {
		t.Fatalf("mismatched play must be refused, got %+v", reply)
	}
	if !strings.Contains(reply.Error, "service speaks protocol 99, client speaks 1") {
		t.Fatalf("mismatch error should name both versions: %q", reply.Error)
	}
	if reply.Proto != 99 {
		t.Fatalf("reply proto = %d, want the server's 99", reply.Proto)
	}
	if got := backend.called(); len(got) != 0 {
		t.Fatalf("refused requests must not reach the backend, calls = %v", got)
	}

	// The frozen stop still works against a service from another revision.
	reply = rawCall(t, addr, `{"id":2,"cmd":"stop"}`)
	if !reply.OK {
		t.Fatalf("frozen stop must be accepted, got %+v", reply)
	}
	if got := backend.called(); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("backend calls = %v, want [stop]", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after a stop, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop must shut the server down")
	}
}

func TestServerStopShutsDownAndSkipsVersion(t *testing.T) {
	backend := &fakeBackend{}
	addr, done := testServer(t, backend, 0)

	// Stop without a proto field (the frozen envelope) is accepted and the
	// connection still gets its acknowledgement before the server exits.
	reply := rawCall(t, addr, `{"id":3,"cmd":"stop"}`)
	if !reply.OK {
		t.Fatalf("stop reply = %+v, want ok", reply)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after a stop request")
	}
}

func TestServerContextCancelShutsDown(t *testing.T) {
	backend := &fakeBackend{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(backend, ln)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v after cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled context must shut the server down")
	}
}
