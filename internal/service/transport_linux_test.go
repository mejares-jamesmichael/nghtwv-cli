//go:build linux

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// TestTransportUnixSocket covers the production Linux transport end to end:
// Listen binds the real Unix socket, Probe (with the platform dialer) finds
// it, and verbs round-trip over it.
func TestTransportUnixSocket(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "service.sock")

	// A stale socket file from a crashed service must not block a bind.
	if err := os.WriteFile(endpoint, nil, 0o600); err != nil {
		t.Fatalf("seed stale artifact: %v", err)
	}
	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen over a stale artifact: %v", err)
	}
	backend := &fakeBackend{state: player.State{Volume: 12}}
	done := make(chan error, 1)
	go func() { done <- NewServer(backend, ln).Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})

	if !Probe(context.Background(), endpoint) {
		t.Fatal("Probe must find the service on the real Unix socket")
	}

	client := NewClient(Config{
		Endpoint: endpoint,
		LockPath: filepath.Join(t.TempDir(), "service.lock"),
		Store:    fakeStore{},
		Clean:    func() { _ = os.Remove(endpoint) },
	})
	st, err := client.Volume(context.Background(), 12)
	if err != nil || st.Volume != 12 {
		t.Fatalf("Volume over the real socket = %+v, %v", st, err)
	}

	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop over the real socket: %v", err)
	}
	if Probe(context.Background(), endpoint) {
		t.Fatal("nothing should answer the endpoint after a stop")
	}
}
