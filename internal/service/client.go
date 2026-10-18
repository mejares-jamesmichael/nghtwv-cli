package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/platform"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// Timing mirrors the mpv supervisor's parity values, plus the bounded wait
// that confirms a service actually exited after a frozen stop.
const (
	DefaultRequestTimeout    = 20 * time.Second
	DefaultHandshakeDeadline = 5 * time.Second
	handshakeInterval        = 50 * time.Millisecond
	probeTimeout             = 300 * time.Millisecond
	lockWait                 = 10 * time.Second
	stopConfirmTimeout       = 2 * time.Second
	stopConfirmInterval      = 100 * time.Millisecond
	// spectrumTimeout bounds one band poll: the panel drops a missed tick
	// rather than queueing stale visuals.
	spectrumTimeout = 500 * time.Millisecond
)

// replyLimit caps one reply line, mirroring the mpv client's 64 KiB
// accumulation cap.
const replyLimit = 64 * 1024

// ErrReplyTooLarge exceeds the reply limit.
var ErrReplyTooLarge = errors.New("service reply too large")

// errServiceNotStarted is the spawn handshake failure.
var errServiceNotStarted = errors.New("nghtwv service did not start")

// DialFunc opens one connection to the service endpoint.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Process is the handle to a spawned service process: only the stillborn
// cleanup paths matter (release / kill), so tests inject fakes.
type Process interface {
	Release() error
	Kill() error
}

// spawnFunc launches the detached service process; tests inject fakes.
type spawnFunc func(ctx context.Context) (Process, error)

// Config describes how a client reaches and starts the service.
type Config struct {
	// Endpoint is the service socket path / pipe name.
	Endpoint string
	// LockPath serialises the probe+spawn sequence across invocations.
	LockPath string
	// Timeout bounds one request; zero means DefaultRequestTimeout.
	Timeout time.Duration
	// Store supplies the remembered volume/output for the neutral state
	// reported when no service is running; zero means the real settings.
	Store player.Store

	// Seams, defaulted in NewClient and overridden in tests: Dial opens one
	// connection (OS transport), Spawn starts the detached service (this
	// binary's hidden verb), Clean removes a stale endpoint artifact, and
	// OrphanStop stops playback a dead service left behind.
	Dial       DialFunc
	Spawn      spawnFunc
	Clean      func()
	OrphanStop func(ctx context.Context) error
}

// Client is the CLI/TUI side of the protocol. It implements the same
// semantic surface as the mpv-backed *player.Player — status, transport,
// volume, outputs — so the one-shots, the panel, and (next stage) the
// audio engine share one contract.
type Client struct {
	cfg    Config
	nextID atomic.Int64
}

// NewClient wires a client, filling the OS seams and timing defaults.
func NewClient(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultRequestTimeout
	}
	if cfg.Store == nil {
		cfg.Store = config.Store{}
	}
	if cfg.Dial == nil {
		cfg.Dial = newDialer(cfg.Endpoint)
	}
	if cfg.Spawn == nil {
		cfg.Spawn = startService
	}
	if cfg.Clean == nil {
		cfg.Clean = func() {}
	}
	if cfg.OrphanStop == nil {
		cfg.OrphanStop = stopOrphanMPV
	}
	return &Client{cfg: cfg}
}

// ---- semantic surface (the one-shots' and TUI's playback seam) ----

// Status reads live playback state. Every failure — no service, no reply,
// protocol mismatch — collapses to the neutral state, matching the
// mpv-era player's never-errors contract (the panel must not blank).
// StatusChecked is the CLI's variant with the mismatch surfaced.
func (c *Client) Status(ctx context.Context) player.State {
	st, _ := c.StatusChecked(ctx)
	return st
}

// StatusChecked is Status plus the protocol-mismatch error, so one-shot
// callers report a service they cannot talk to instead of a misleading
// idle state. Other failures stay neutral, exactly like mpv being absent.
func (c *Client) StatusChecked(ctx context.Context) (player.State, error) {
	reply, err := c.call(ctx, Request{Cmd: CmdStatus, Proto: Protocol}, 0)
	if err != nil {
		return c.neutral(), nil
	}
	if !reply.OK {
		if reply.Code == CodeProtocolMismatch {
			return c.neutral(), mismatchError(reply)
		}
		return c.neutral(), nil
	}
	var st player.State
	if err := json.Unmarshal(reply.Result, &st); err != nil {
		return c.neutral(), nil
	}
	return st, nil
}

// Play starts the stream or resumes it, starting the service when absent.
func (c *Client) Play(ctx context.Context) (player.State, error) {
	return c.action(ctx, CmdPlay, nil)
}

// Pause pauses playback (spawning an idle backend when absent, parity).
func (c *Client) Pause(ctx context.Context) (player.State, error) {
	return c.action(ctx, CmdPause, nil)
}

// Toggle starts playback when idle/absent, otherwise flips pause.
func (c *Client) Toggle(ctx context.Context) (player.State, error) {
	return c.action(ctx, CmdToggle, nil)
}

// Volume sets the volume, clamped by the backend; the chosen volume
// persists in settings (the service owns that write).
func (c *Client) Volume(ctx context.Context, volume int) (player.State, error) {
	args, err := json.Marshal(VolumeArgs{Value: volume})
	if err != nil {
		return c.neutral(), err
	}
	return c.action(ctx, CmdVolume, args)
}

// Outputs enumerates audio output devices; errors carry the parity
// "audio outputs are unavailable" prefix.
func (c *Client) Outputs(ctx context.Context) ([]mpv.Device, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	reply, err := c.call(ctx, Request{Cmd: CmdOutputs, Proto: Protocol}, 0)
	if err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	if !reply.OK {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", replyError(reply))
	}
	var res OutputsResult
	if err := json.Unmarshal(reply.Result, &res); err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	return res.Outputs, nil
}

// SetOutput changes or clears the output device ("" means system default).
func (c *Client) SetOutput(ctx context.Context, device string) (player.State, error) {
	args, err := json.Marshal(OutputArgs{Device: device})
	if err != nil {
		return c.neutral(), err
	}
	return c.action(ctx, CmdOutput, args)
}

// Spectrum polls the backend's latest band levels. It never attaches or
// starts a service (the panel polls it only while playing, which implies one
// is up); a backend without a sample tap reports audio.ErrSpectrumUnavailable
// so callers fall back to decorative visuals instead of retrying.
func (c *Client) Spectrum(ctx context.Context) (audio.Snapshot, error) {
	reply, err := c.call(ctx, Request{Cmd: CmdSpectrum, Proto: Protocol}, spectrumTimeout)
	if err != nil {
		return audio.Snapshot{}, err
	}
	if !reply.OK {
		if reply.Code == CodeUnsupported {
			return audio.Snapshot{}, audio.ErrSpectrumUnavailable
		}
		return audio.Snapshot{}, replyError(reply)
	}
	var snap audio.Snapshot
	if err := json.Unmarshal(reply.Result, &snap); err != nil {
		return audio.Snapshot{}, err
	}
	return snap, nil
}

// Stop stops playback and shuts the service down. The request is the
// frozen stop envelope — no protocol field, no arguments — so it reaches
// services this client cannot otherwise speak to, and any reply (or an
// abrupt close) is tolerated: the shutdown is confirmed by the endpoint
// going away. When no service answers, an mpv left behind by a dead
// service is quit directly (migration-era cleanup).
func (c *Client) Stop(ctx context.Context) error {
	if !c.alive(ctx) {
		return c.cfg.OrphanStop(ctx)
	}
	_, _ = c.call(ctx, Request{Cmd: CmdStop}, stopConfirmTimeout)
	if err := c.waitGone(ctx); err != nil {
		return fmt.Errorf("nghtwv service did not shut down: %w", err)
	}
	return nil
}

// ---- request plumbing ----

// action ensures the service and runs one transport verb.
func (c *Client) action(ctx context.Context, cmd string, args json.RawMessage) (player.State, error) {
	if err := c.ensure(ctx); err != nil {
		return c.neutral(), err
	}
	reply, err := c.call(ctx, Request{Cmd: cmd, Proto: Protocol, Args: args}, 0)
	if err != nil {
		return c.neutral(), err
	}
	if !reply.OK {
		return c.neutral(), replyError(reply)
	}
	var st player.State
	if err := json.Unmarshal(reply.Result, &st); err != nil {
		return c.neutral(), err
	}
	return st, nil
}

// call performs one request/reply exchange on a fresh connection.
func (c *Client) call(ctx context.Context, req Request, timeout time.Duration) (Reply, error) {
	req.ID = c.nextID.Add(1)
	if timeout <= 0 {
		timeout = c.cfg.Timeout
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	nc, err := c.cfg.Dial(ctx)
	if err != nil {
		return Reply{}, err
	}
	defer nc.Close()
	_ = nc.SetDeadline(deadline)

	line, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	if _, err := nc.Write(append(line, '\n')); err != nil {
		return Reply{}, err
	}
	raw, err := readLine(bufio.NewReaderSize(nc, 4096))
	if err != nil {
		return Reply{}, err
	}
	var reply Reply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return Reply{}, fmt.Errorf("bad service reply: %w", err)
	}
	return reply, nil
}

// readLine reads one reply line within the accumulation cap.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > replyLimit {
			return nil, ErrReplyTooLarge
		}
		if err == nil {
			return bytes.TrimSuffix(buf, []byte{'\n'}), nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

// alive reports whether a service answers the endpoint, tolerating any
// protocol version: a reply — even a refusal — means a service is there.
// The ping verb does no backend work, so probing never disturbs playback.
func (c *Client) alive(ctx context.Context) bool {
	_, err := c.call(ctx, Request{Cmd: CmdPing, Proto: Protocol}, probeTimeout)
	return err == nil
}

// Probe reports whether a service answers the endpoint: any reply — even a
// protocol refusal — counts, so callers can tell "nothing there" from
// "something speaking another dialect". The hidden service verb uses it to
// bow out when another instance already owns the endpoint.
func Probe(ctx context.Context, endpoint string) bool {
	return NewClient(Config{Endpoint: endpoint}).alive(ctx)
}

// waitGone polls until the endpoint stops answering, bounded by the stop
// confirmation window.
func (c *Client) waitGone(ctx context.Context) error {
	deadline := time.Now().Add(stopConfirmTimeout)
	for {
		if !c.alive(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("endpoint still answering")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stopConfirmInterval):
		}
	}
}

// ensure attaches to the service, starting it under the launch lock when
// nothing answers: probe, clean a stale artifact, spawn detached, then
// handshake — the mpv supervisor's serialisation pattern, so concurrent
// invocations start one service, never a pile.
func (c *Client) ensure(ctx context.Context) error {
	lock := flock.New(c.cfg.LockPath)
	lockCtx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	acquired, err := lock.TryLockContext(lockCtx, handshakeInterval)
	if err != nil {
		return fmt.Errorf("service lock: %w", err)
	}
	if !acquired {
		return errors.New("service lock: timed out waiting for another nghtwv invocation")
	}
	defer lock.Unlock()

	if c.alive(ctx) {
		return nil
	}
	c.cfg.Clean()

	proc, err := c.cfg.Spawn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if proc != nil {
			_ = proc.Release()
		}
	}()

	if err := c.handshake(ctx); err != nil {
		if proc != nil {
			_ = proc.Kill()
		}
		c.cfg.Clean()
		return err
	}
	return nil
}

// handshake polls until the fresh service answers or the deadline passes.
func (c *Client) handshake(ctx context.Context) error {
	deadline := time.Now().Add(DefaultHandshakeDeadline)
	for {
		if c.alive(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return errServiceNotStarted
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(handshakeInterval):
		}
	}
}

// neutral is the state reported when no service can be reached.
func (c *Client) neutral() player.State {
	return player.NeutralState(c.cfg.Store)
}

// mismatchError adds the remedy to the service's own mismatch message:
// normal requests are refused, but the frozen stop always works.
func mismatchError(reply Reply) error {
	text := reply.Error
	if text == "" {
		text = "service protocol version mismatch"
	}
	return fmt.Errorf("nghtwv %s; run \"nghtwv stop\" to shut it down, then retry", text)
}

// replyError maps a failed reply onto an error, keeping the mismatch
// remedy intact.
func replyError(reply Reply) error {
	if reply.Code == CodeProtocolMismatch {
		return mismatchError(reply)
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	return errors.New("service error")
}

// stopOrphanMPV quits an mpv left behind by a service that died mid-play.
// It is the mpv-era migration shim: once the audio engine lives inside the
// service there is no detached player to clean up, and this disappears
// with internal/mpv. Stop never errors when nothing is playing.
func stopOrphanMPV(ctx context.Context) error {
	endpoint, err := platform.EndpointPath()
	if err != nil {
		return nil
	}
	client := mpv.NewClient(mpv.NewDialer(endpoint))
	_, _ = client.Call(ctx, []any{"quit"}) // expected to fail once mpv exits mid-call
	return nil
}
