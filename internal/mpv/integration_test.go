//go:build linux

package mpv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestIntegrationRealMpv exercises the full lifecycle against a real,
// detached mpv: spawn idle → probe → set/get volume → quit → gone.
// Gated behind NGHTWV_IT=1 so CI never touches audio; run locally with:
//
//	NGHTWV_IT=1 go test -run TestIntegration ./internal/mpv/
func TestIntegrationRealMpv(t *testing.T) {
	if os.Getenv("NGHTWV_IT") != "1" {
		t.Skip("set NGHTWV_IT=1 to run real-mpv integration")
	}
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skipf("mpv is not installed: %v", err)
	}

	dir := t.TempDir()
	cfg := ProcessConfig{
		Endpoint: filepath.Join(dir, "mpv.sock"),
		LockPath: filepath.Join(dir, "player.lock"),
	}
	sup := NewSupervisor(cfg, SocketDialer{Path: cfg.Endpoint})

	spawned, err := sup.Ensure(context.Background(), false)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !spawned {
		t.Fatal("expected a spawn (no pre-existing endpoint)")
	}

	// Safety net: if the test exits abnormally at any point below, quit mpv.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = sup.Client().Call(ctx, []any{"quit"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !sup.Alive(ctx) {
		t.Fatal("endpoint must answer right after the handshake")
	}

	client := sup.Client()
	if err := client.SetProperty(ctx, "volume", 33.0); err != nil {
		t.Fatalf("set volume: %v", err)
	}
	raw, err := client.GetProperty(ctx, "volume")
	if err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if n, _ := PropInt(raw); n != 33 {
		t.Fatalf("volume round trip = %d, want 33", n)
	}
	if rawIdle, err := client.GetProperty(ctx, "idle-active"); err != nil {
		t.Fatalf("idle probe: %v", err)
	} else if b, _ := PropBool(rawIdle); !b {
		t.Fatal("idle spawn must report idle-active=true")
	}

	if _, err := client.Call(ctx, []any{"quit"}); err != nil {
		t.Logf("quit reply: %v (mpv may exit before answering)", err)
	}

	// mpv unlinks its own socket on clean exit; poll until the endpoint dies.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !sup.Alive(context.Background()) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("mpv still answering 3s after quit")
}
