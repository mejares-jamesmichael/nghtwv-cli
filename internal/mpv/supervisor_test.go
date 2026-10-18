package mpv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// spawnRecorder replaces the per-OS exec launcher in tests.
type spawnRecorder struct {
	calls    [][]string
	proc     *os.Process // nil is safe: Ensure treats it as "no stillborn to kill"
	failWith error
}

func (r *spawnRecorder) spawn(ctx context.Context, args []string) (*os.Process, error) {
	r.calls = append(r.calls, args)
	if r.failWith != nil {
		return nil, r.failWith
	}
	return r.proc, nil
}

func TestBaseArgs(t *testing.T) {
	cfg := ProcessConfig{Endpoint: "/run/user/1000/nghtwv/mpv.sock"}
	want := []string{
		"--no-video",
		"--force-window=no",
		"--idle=yes",
		"--input-ipc-server=/run/user/1000/nghtwv/mpv.sock",
		"--title=nghtwv",
		"--volume=70",
		"--network-timeout=30",
		"--cache-secs=20",
		"--demuxer-max-bytes=8MiB",
		"--demuxer-max-back-bytes=2MiB",
	}
	if got := baseArgs(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseArgs = %v\nwant        %v", got, want)
	}

	cfg.AudioDevice = "pipewire/alsa_output.pci-0000:00:1f.3"
	want = append(want, "--audio-device=pipewire/alsa_output.pci-0000:00:1f.3")
	if got := baseArgs(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseArgs with device = %v\nwant                      %v", got, want)
	}
}

func TestEnsureAttachedShortCircuits(t *testing.T) {
	dialer := &fakeDialer{handler: replySuccess(`true`)} // idle-active probe answers
	sup := NewSupervisor(testCfg(t), dialer)
	rec := &spawnRecorder{}
	sup.spawn = rec.spawn
	cleans := 0
	sup.clean = func() { cleans++ }

	spawned, err := sup.Ensure(context.Background(), false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if spawned {
		t.Fatal("attached mpv must not spawn a second one")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("spawn invoked %d times, want 0", len(rec.calls))
	}
	if cleans != 0 {
		t.Fatalf("clean invoked %d times, want 0 (endpoint is alive)", cleans)
	}
}

func TestEnsureSpawnsWithStreamAndCleansStale(t *testing.T) {
	dialer := &fakeDialer{
		handler: replySuccess(`true`),
		script:  []error{errors.New("dial: connection refused")}, // first probe: dead endpoint
	}
	cfg := testCfg(t)
	cfg.StreamURL = "https://radio.plaza.one/mp3"
	sup := NewSupervisor(cfg, dialer)
	rec := &spawnRecorder{}
	sup.spawn = rec.spawn
	cleans := 0
	sup.clean = func() { cleans++ }

	spawned, err := sup.Ensure(context.Background(), true)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !spawned {
		t.Fatal("Ensure should report a spawn")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("spawn invoked %d times, want 1", len(rec.calls))
	}
	args := rec.calls[0]
	if args[len(args)-1] != "https://radio.plaza.one/mp3" {
		t.Fatalf("stream URL must be the last spawn argument, got %v", args)
	}
	if cleans != 1 {
		t.Fatalf("clean invoked %d times, want 1 (stale socket removal)", cleans)
	}
}

func TestEnsureSpawnFailurePropagatesWithoutCleanupKill(t *testing.T) {
	boom := errors.New("exec: mpv: not found")
	dialer := &fakeDialer{handler: replySuccess(`true`), script: []error{errDead}} // probe fails
	sup := NewSupervisor(testCfg(t), dialer)
	rec := &spawnRecorder{failWith: boom}
	sup.spawn = rec.spawn
	cleans := 0
	sup.clean = func() { cleans++ }

	_, err := sup.Ensure(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "mpv: not found") {
		t.Fatalf("err = %v, want the exec failure", err)
	}
	if cleans != 1 {
		// one clean: stale endpoint removed after the failed probe,
		// before the spawn attempt (parity launch()).
		t.Fatalf("clean invoked %d times, want 1", cleans)
	}
}

func TestEnsureHandshakeTimeoutReportsAndCleans(t *testing.T) {
	dead := &fakeDialer{script: []error{errDead, errDead, errDead, errDead}}
	sup := NewSupervisor(tinyHandshakeCfg(t), dead)
	rec := &spawnRecorder{}
	sup.spawn = rec.spawn
	cleans := 0
	sup.clean = func() { cleans++ }

	_, err := sup.Ensure(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "mpv did not start") {
		t.Fatalf("err = %v, want the did-not-start failure", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("spawn invoked %d times, want 1", len(rec.calls))
	}
	if cleans != 2 { // once after failed probe, once on handshake failure
		t.Fatalf("clean invoked %d times, want 2", cleans)
	}
}

var errDead = errors.New("dial: connection refused")

func TestEnsureDeadEndpointThenHealthyHandshake(t *testing.T) {
	// probe fails → spawn → handshake succeeds on the second dial
	dialer := &fakeDialer{handler: replySuccess(`false`), script: []error{errDead}}
	sup := NewSupervisor(testCfg(t), dialer)
	rec := &spawnRecorder{}
	sup.spawn = rec.spawn

	spawned, err := sup.Ensure(context.Background(), false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !spawned {
		t.Fatal("want a spawn report")
	}
}

func TestClientBinding(t *testing.T) {
	sup := NewSupervisor(testCfg(t), &fakeDialer{handler: replySuccess(`7`)})
	client := sup.Client()
	if client != sup.Client() {
		t.Fatal("Client must be stable per supervisor")
	}
}

// helpers

func testCfg(t *testing.T) ProcessConfig {
	t.Helper()
	assertTemp := t.TempDir()
	return ProcessConfig{
		Endpoint: filepath.Join(assertTemp, "mpv.sock"),
		LockPath: filepath.Join(assertTemp, "player.lock"),
	}
}

func tinyHandshakeCfg(t *testing.T) ProcessConfig {
	cfg := testCfg(t)
	cfg.HandshakeDeadline = 80 * time.Millisecond
	cfg.HandshakeProbe = 10 * time.Millisecond
	cfg.HandshakeInterval = 10 * time.Millisecond
	return cfg
}
