package mpv

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gofrs/flock"
)

// ProcessConfig describes how the supervisor reaches and launches mpv.
type ProcessConfig struct {
	// Endpoint is the Unix socket path or named pipe address passed to
	// --input-ipc-server.
	Endpoint string
	// LockPath guards the stale-socket probe + spawn sequence (parity:
	// rapid invocations must not stack mpvs).
	LockPath string
	// StreamURL is appended to the command line only when the caller wants
	// a streaming spawn (play/toggle); idle spawns never auto-play.
	StreamURL string
	// AudioDevice is the remembered output device; "" omits --audio-device.
	AudioDevice string

	// Handshake knobs default to parity values (5 s deadline, 0.3 s probe,
	// 50 ms cadence) and are overridable in tests.
	HandshakeDeadline time.Duration
	HandshakeProbe    time.Duration
	HandshakeInterval time.Duration
}

// spawnFunc launches a detached mpv with resolved arguments.
// Implemented per OS by startCommand; tests inject fakes.
type spawnFunc func(ctx context.Context, args []string) (*os.Process, error)

// cleanFunc removes a stale endpoint artifact (dead Unix socket file).
// No-op on Windows, where dead pipes leave nothing on disk.
type cleanFunc func()

// Supervisor owns attach-or-spawn bookkeeping for one endpoint.
type Supervisor struct {
	cfg    ProcessConfig
	dialer Dialer
	client *Client
	spawn  spawnFunc
	clean  cleanFunc
}

// NewSupervisor wires a supervisor for one endpoint, filling timing
// defaults. spawn defaults to the per-OS detached exec (startCommand) and
// clean to a no-op; tests may override via fields.
func NewSupervisor(cfg ProcessConfig, dialer Dialer) *Supervisor {
	cfg.HandshakeDeadline = orDefault(cfg.HandshakeDeadline, DefaultHandshakeDeadline)
	cfg.HandshakeProbe = orDefault(cfg.HandshakeProbe, DefaultHandshakeProbe)
	cfg.HandshakeInterval = orDefault(cfg.HandshakeInterval, DefaultHandshakeInterval)
	return &Supervisor{
		cfg:    cfg,
		dialer: dialer,
		client: NewClient(dialer),
		spawn:  startCommand,
		clean:  func() {},
	}
}

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Client exposes the one-shot IPC client bound to this endpoint.
func (s *Supervisor) Client() *Client {
	if s.client == nil {
		s.client = NewClient(s.dialer)
	}
	return s.client
}

// Alive reports whether an mpv answers at the endpoint right now.
func (s *Supervisor) Alive(ctx context.Context) bool {
	return s.responds(ctx, DefaultCommandTimeout)
}

// Ensure guarantees a responding mpv at the endpoint, spawning one when
// nothing answers. It reports whether this call spawned a new mpv process.
//
// The flock held for the whole probe+clean+spawn+handshake sequence is the
// parity hardening: concurrent invocations serialise, so rapid clicks
// produce one mpv, never a pile of zombies.
func (s *Supervisor) Ensure(ctx context.Context, spawnWithStream bool) (bool, error) {
	lock := flock.New(s.cfg.LockPath)
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// TryLockContext retries until the lock is acquired or the deadline
	// passes — parity blocking-flock semantics with a bounded wait.
	acquired, err := lock.TryLockContext(lockCtx, DefaultHandshakeInterval)
	if err != nil {
		return false, fmt.Errorf("player lock: %w", err)
	}
	if !acquired {
		return false, fmt.Errorf("player lock: timed out waiting for another nghtwv invocation")
	}
	defer lock.Unlock()

	if s.responds(ctx, s.cfg.HandshakeProbe) {
		return false, nil
	}
	s.clean()

	proc, err := s.spawn(ctx, s.launchArgs(spawnWithStream))
	if err != nil {
		return false, err
	}
	// The detached child outlives this process by contract (Setsid /
	// DETACHED_PROCESS); the handle is only for stillborn cleanup below.
	defer func() {
		if proc != nil {
			_ = proc.Release()
		}
	}()

	if err := s.handshake(ctx); err != nil {
		// Improvement over the plugin (which reparented and could only
		// warn): the stillborn child is ours, so reclaim it.
		if proc != nil {
			_ = proc.Kill()
		}
		s.clean()
		return false, err
	}
	return true, nil
}

// responds runs a cheap probe: does anything answer the endpoint?
func (s *Supervisor) responds(ctx context.Context, timeout time.Duration) bool {
	_, err := s.Client().CallTimeout(ctx, []any{"get_property", "idle-active"}, timeout)
	return err == nil
}

// launchArgs assembles the mpv command line for a spawn.
func (s *Supervisor) launchArgs(spawnWithStream bool) []string {
	args := baseArgs(s.cfg)
	if spawnWithStream && s.cfg.StreamURL != "" {
		args = append(args, s.cfg.StreamURL)
	}
	return args
}

// handshake polls until the fresh mpv responds, or gives up after the
// deadline (parity: 5 s, 0.3 s probe timeouts, 50 ms steps).
func (s *Supervisor) handshake(ctx context.Context) error {
	deadline := time.Now().Add(s.cfg.HandshakeDeadline)
	for {
		if s.responds(ctx, s.cfg.HandshakeProbe) {
			return nil
		}
		if time.Now().After(deadline) {
			return errMpvNotStarted
		}
		time.Sleep(s.cfg.HandshakeInterval)
	}
}

var errMpvNotStarted = mpvNotStartedError{}

type mpvNotStartedError struct{}

func (mpvNotStartedError) Error() string {
	return "mpv did not start; install mpv and check its runtime directory"
}

// baseArgs mirrors plaza-player's spawn options line for line.
func baseArgs(cfg ProcessConfig) []string {
	args := []string{
		"--no-video",
		"--force-window=no",
		"--idle=yes",
		"--input-ipc-server=" + cfg.Endpoint,
		"--title=nghtwv",
		"--volume=70",
		"--network-timeout=30",
		"--cache-secs=20",
		"--demuxer-max-bytes=8MiB",
		"--demuxer-max-back-bytes=2MiB",
	}
	if cfg.AudioDevice != "" {
		args = append(args, "--audio-device="+cfg.AudioDevice)
	}
	return args
}
