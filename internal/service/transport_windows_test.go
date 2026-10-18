//go:build windows

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// TestTransportPipe covers the production Windows transport end to end:
// Listen binds a real named pipe, Probe (with the platform dialer) finds
// it, and verbs round-trip over it.
func TestTransportPipe(t *testing.T) {
	endpoint := fmt.Sprintf(`\\.\pipe\nghtwv-test-%d`, os.Getpid())

	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	backend := &fakeBackend{state: player.State{Volume: 12}}
	done := make(chan error, 1)
	go func() { done <- NewServer(backend, ln).Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})

	if !Probe(context.Background(), endpoint) {
		t.Fatal("Probe must find the service on the real named pipe")
	}

	client := NewClient(Config{
		Endpoint: endpoint,
		LockPath: filepath.Join(t.TempDir(), "service.lock"),
		Store:    fakeStore{},
	})
	st, err := client.Volume(context.Background(), 12)
	if err != nil || st.Volume != 12 {
		t.Fatalf("Volume over the real pipe = %+v, %v", st, err)
	}

	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop over the real pipe: %v", err)
	}
	if Probe(context.Background(), endpoint) {
		t.Fatal("nothing should answer the endpoint after a stop")
	}
}
