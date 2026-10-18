package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// fakeProc records release/kill without a real OS process.
type fakeProc struct {
	mu     sync.Mutex
	killed bool
}

func (p *fakeProc) Release() error { return nil }

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// fakeStore supplies remembered settings for neutral states.
type fakeStore struct {
	settings config.Settings
}

func (f fakeStore) Load() (config.Settings, error) { return f.settings, nil }
func (f fakeStore) Save(config.Settings) error     { return nil }

func TestClientTransportVerbs(t *testing.T) {
	backend := &fakeBackend{state: player.State{Playing: true, Volume: 42, Title: "Neon Rain"}}
	addr, _ := testServer(t, backend, 0)
	client := testClient(t, addr)
	ctx := context.Background()

	st, err := client.Play(ctx)
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if !st.Playing || st.Volume != 42 || st.Title != "Neon Rain" {
		t.Fatalf("Play state = %+v", st)
	}
	if _, err := client.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := client.Toggle(ctx); err != nil {
		t.Fatalf("Toggle: %v", err)
	}
	if _, err := client.Volume(ctx, 42); err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if _, err := client.SetOutput(ctx, "sink-1"); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	devices, err := client.Outputs(ctx)
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}
	if len(devices) != 2 || devices[0].Name != "auto" || devices[1].Description != "Desk" {
		t.Fatalf("devices = %+v", devices)
	}

	want := []string{"play", "pause", "toggle", "volume:42", `output:"sink-1"`, "outputs"}
	got := backend.called()
	if len(got) != len(want) {
		t.Fatalf("backend calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backend calls = %v, want %v", got, want)
		}
	}
}

func TestClientNeutralWithoutService(t *testing.T) {
	vol := 55
	store := fakeStore{settings: config.Settings{Volume: &vol, Output: "sink-x"}}
	client := NewClient(Config{
		Endpoint: "nowhere",
		Dial:     func(context.Context) (net.Conn, error) { return nil, errors.New("no service") },
		Store:    store,
	})

	st := client.Status(context.Background())
	want := player.State{Volume: 55, Output: "sink-x"}
	if st != want {
		t.Fatalf("Status = %+v, want the remembered neutral state %+v", st, want)
	}
	if _, err := client.StatusChecked(context.Background()); err != nil {
		t.Fatalf("an absent service is not a status error (mpv parity): %v", err)
	}
}

func TestClientMismatchRejectsAndStopWorks(t *testing.T) {
	backend := &fakeBackend{state: player.State{Playing: true}}
	addr, done := testServer(t, backend, Protocol+1)
	client := testClient(t, addr)
	ctx := context.Background()

	_, err := client.Play(ctx)
	if err == nil || !strings.Contains(err.Error(), "protocol") || !strings.Contains(err.Error(), `"nghtwv stop"`) {
		t.Fatalf("Play error = %v; want the mismatch plus the stop remedy", err)
	}
	if st := client.Status(ctx); st.Playing {
		t.Fatalf("Status must collapse to neutral on a mismatch, got %+v", st)
	}
	if _, err := client.StatusChecked(ctx); err == nil {
		t.Fatal("StatusChecked must surface the mismatch")
	}
	if _, err := client.Outputs(ctx); err == nil || !strings.Contains(err.Error(), "audio outputs are unavailable") {
		t.Fatalf("Outputs error = %v; want the parity prefix intact", err)
	}
	if got := backend.called(); len(got) != 0 {
		t.Fatalf("refused requests must not reach the backend, calls = %v", got)
	}

	// The frozen stop envelope still reaches the mismatched service.
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if got := backend.called(); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("backend calls = %v, want [stop]", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil after the frozen stop", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the mismatched service must shut down")
	}
}

func TestClientStopWithoutServiceCleansOrphan(t *testing.T) {
	orphans := 0
	client := NewClient(Config{
		Endpoint:   "nowhere",
		Dial:       func(context.Context) (net.Conn, error) { return nil, errors.New("no service") },
		Store:      fakeStore{},
		OrphanStop: func(context.Context) error { orphans++; return nil },
	})

	if err := client.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if orphans != 1 {
		t.Fatalf("orphan cleanup calls = %d, want 1", orphans)
	}
}

func TestClientStopReportsStuckService(t *testing.T) {
	addr := inertEndpoint(t)
	client := NewClient(Config{Endpoint: addr, Dial: tcpDialer(addr), Store: fakeStore{}})

	err := client.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not shut down") {
		t.Fatalf("Stop = %v; want a shutdown-confirmation failure", err)
	}
}

func TestClientEnsureStartsOnce(t *testing.T) {
	var mu sync.Mutex
	var addr string
	var spawns atomic.Int32
	backend := &fakeBackend{state: player.State{Playing: true}}

	spawn := func(context.Context) (Process, error) {
		spawns.Add(1)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		go NewServer(backend, ln).Serve(context.Background()) //nolint:errcheck // test server
		mu.Lock()
		addr = ln.Addr().String()
		mu.Unlock()
		return &fakeProc{}, nil
	}
	client := NewClient(Config{
		Endpoint: "service",
		LockPath: filepath.Join(t.TempDir(), "service.lock"),
		Store:    fakeStore{},
		Dial: func(ctx context.Context) (net.Conn, error) {
			mu.Lock()
			target := addr
			mu.Unlock()
			if target == "" {
				return nil, errors.New("not started")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target)
		},
		Spawn: spawn,
	})

	// Concurrent first commands must start exactly one service: the launch
	// lock serialises probe + spawn, so late callers attach.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Play(context.Background()); err != nil {
				t.Errorf("Play: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d, want 1", got)
	}

	// An attached client never spawns again.
	if _, err := client.Toggle(context.Background()); err != nil {
		t.Fatalf("Toggle: %v", err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("spawns after attach = %d, want 1", got)
	}
}

func TestClientEnsureReclaimsStillbornService(t *testing.T) {
	proc := &fakeProc{}
	cleaned := 0
	client := NewClient(Config{
		Endpoint: "service",
		LockPath: filepath.Join(t.TempDir(), "service.lock"),
		Store:    fakeStore{},
		Dial:     func(context.Context) (net.Conn, error) { return nil, errors.New("nothing there") },
		Spawn:    func(context.Context) (Process, error) { return proc, nil },
		Clean:    func() { cleaned++ },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Play(ctx)
	if err == nil {
		t.Fatal("Play must fail when the service never answers")
	}
	if !proc.wasKilled() {
		t.Fatal("a stillborn service must be reclaimed")
	}
	if cleaned == 0 {
		t.Fatal("a stale endpoint artifact must be cleaned before the retry")
	}
}

func TestClientSurfacesBackendErrors(t *testing.T) {
	backend := &fakeBackend{err: errors.New("mpv exploded")}
	addr, _ := testServer(t, backend, 0)
	client := testClient(t, addr)
	ctx := context.Background()

	st, err := client.Play(ctx)
	if err == nil || !strings.Contains(err.Error(), "mpv exploded") {
		t.Fatalf("Play error = %v; want the backend text", err)
	}
	if st.Volume != player.DefaultVolume {
		t.Fatalf("failed actions report the neutral state, got %+v", st)
	}
	if _, err := client.Outputs(ctx); err == nil || !strings.Contains(err.Error(), "audio outputs are unavailable: mpv exploded") {
		t.Fatalf("Outputs error = %v; want the parity prefix and the backend text", err)
	}
}

// inertEndpoint accepts clients, answers every request OK, and never
// exits: the service that ignores the frozen stop.
func inertEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer nc.Close()
				scanner := bufio.NewScanner(nc)
				for scanner.Scan() {
					b, _ := json.Marshal(Reply{ID: 1, OK: true, Proto: Protocol})
					if _, err := nc.Write(append(b, '\n')); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestClientSpectrum(t *testing.T) {
	snap := audio.Snapshot{Bands: []float32{0.1, 0.9}, Level: 0.4}
	addr, _ := testServer(t, &spectrumBackend{fakeBackend: &fakeBackend{}, snap: snap}, 0)

	got, err := testClient(t, addr).Spectrum(context.Background())
	if err != nil {
		t.Fatalf("Spectrum: %v", err)
	}
	if len(got.Bands) != 2 || got.Bands[1] != 0.9 || got.Level != 0.4 {
		t.Fatalf("snapshot = %+v, want %+v", got, snap)
	}
}

func TestClientSpectrumUnavailableOnMPVBackend(t *testing.T) {
	addr, _ := testServer(t, &fakeBackend{}, 0)
	if _, err := testClient(t, addr).Spectrum(context.Background()); !errors.Is(err, audio.ErrSpectrumUnavailable) {
		t.Fatalf("err = %v, want audio.ErrSpectrumUnavailable", err)
	}
}

func TestClientSpectrumNeverStartsAService(t *testing.T) {
	spawns := 0
	client := NewClient(Config{
		Endpoint: "nowhere",
		LockPath: filepath.Join(t.TempDir(), "service.lock"),
		Dial:     func(context.Context) (net.Conn, error) { return nil, errors.New("no service") },
		Spawn:    func(context.Context) (Process, error) { spawns++; return &fakeProc{}, nil },
		Store:    fakeStore{},
	})
	if _, err := client.Spectrum(context.Background()); err == nil {
		t.Fatal("Spectrum without a service must fail")
	}
	if spawns != 0 {
		t.Fatalf("spawns = %d, want 0: a band poll never starts a service", spawns)
	}
}
