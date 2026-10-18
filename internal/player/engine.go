package player

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// TitleSource supplies now-playing metadata for the engine backend, which —
// unlike mpv — has no media-title property (satisfied by *plaza.Client).
type TitleSource interface {
	Metadata(ctx context.Context) (*plaza.Metadata, error)
}

// titleTTL caches synthesized titles: Plaza is polled on a 30 s cadence
// anyway, and a slightly stale title beats a status that errors.
var titleTTL = 30 * time.Second

// EnginePlayer is the semantic player over the built-in audio engine. It
// mirrors *Player's surface so the service can serve either backend behind
// one protocol: same state shape, same clamping and persistence rules, same
// parity error text for unknown outputs.
//
// Outputs are owned by the player's router: the engine writes into a stable
// seam while a device switch swaps the backend underneath it, so switching
// devices neither restarts the engine nor reconnects the stream (the engine
// contract keeps one output open across stop/start cycles).
type EnginePlayer struct {
	router *outputRouter
	eng    *audio.Engine
	store  Store
	titles TitleSource

	mu     sync.Mutex // guards the transport bookkeeping below
	opened bool
	device string // resolved device backing the open output ("" = default)
	paused bool   // pause requested while nothing is running (report-only)

	tmu     sync.Mutex // guards the title cache
	title   string
	fetched time.Time
}

// NewEnginePlayer wires the engine behind the player surface. newOut opens
// one connection to the audio backend (production: audio.NewOutput) and is
// called lazily — device enumeration, then each device switch — so the
// service starts even when no sound server is running, and a switch can
// replace the backend without touching the engine. The remembered volume
// seeds the gain, and Plaza supplies the title the engine cannot report
// itself.
func NewEnginePlayer(newOut func() (audio.Output, error), rate int, opener audio.Opener, store Store, titles TitleSource) *EnginePlayer {
	volume := DefaultVolume
	if s, err := store.Load(); err == nil && s.Volume != nil {
		volume = clamp(*s.Volume)
	}
	router := &outputRouter{newOut: newOut}
	return &EnginePlayer{
		router: router,
		eng:    audio.NewEngine(router, rate, opener, audio.WithVolume(volume)),
		store:  store,
		titles: titles,
	}
}

// Status reports live state in mpv's shape: playing covers buffering and
// paused (both hold a stream), and the title is synthesized from Plaza
// because the engine has no media-title. Never errors (parity).
func (ep *EnginePlayer) Status(ctx context.Context) State { return ep.state(ctx) }

// Play opens the device on first use and starts or resumes the stream. The
// stream connects in the background: the reply is buffering until audio
// arrives, exactly like mpv loading a live stream.
func (ep *EnginePlayer) Play(ctx context.Context) (State, error) {
	if err := ep.ensureOpen(); err != nil {
		return ep.state(ctx), err
	}
	ep.eng.Play()
	ep.mu.Lock()
	ep.paused = false
	ep.mu.Unlock()
	return ep.state(ctx), nil
}

// Pause pauses a running stream. With nothing running it only records the
// request and reports {playing:false,paused:true}: a pause never opens the
// device or connects the stream (mpv parity: pause does not load a stream).
// The next play starts playback, like startOrResume's unpause.
func (ep *EnginePlayer) Pause(ctx context.Context) (State, error) {
	ep.mu.Lock()
	ep.paused = true
	ep.mu.Unlock()
	ep.eng.Pause() // no-op unless a stream is running
	return ep.state(ctx), nil
}

// Toggle starts playback when stopped, otherwise flips pause (mpv parity:
// toggle on an idle player starts the stream).
func (ep *EnginePlayer) Toggle(ctx context.Context) (State, error) {
	switch ep.eng.Status().State {
	case audio.StateStopped:
		return ep.Play(ctx)
	case audio.StatePaused:
		ep.eng.Play()
		ep.mu.Lock()
		ep.paused = false
		ep.mu.Unlock()
	default:
		ep.mu.Lock()
		ep.paused = true
		ep.mu.Unlock()
		ep.eng.Pause()
	}
	return ep.state(ctx), nil
}

// Stop halts decode and drops queued audio; the device stays open for the
// next start (the engine's contract). A frozen stop shuts the service down
// right after, which releases it.
func (ep *EnginePlayer) Stop(ctx context.Context) error {
	ep.mu.Lock()
	ep.paused = false
	ep.mu.Unlock()
	ep.eng.Stop()
	return nil
}

// Volume sets the gain, clamped to 0-100, and persists it. Unlike mpv a
// volume command does not acquire the device: the next play opens it with
// the new gain.
func (ep *EnginePlayer) Volume(ctx context.Context, volume int) (State, error) {
	v := clamp(volume)
	ep.eng.SetVolume(v)
	if err := rememberVolume(ep.store, v); err != nil {
		return ep.state(ctx), fmt.Errorf("save volume: %w", err)
	}
	return ep.state(ctx), nil
}

// Outputs enumerates the engine's devices. mpv's "auto" entry is kept first
// — the shape the one-shot and the picker already render — and means system
// default.
func (ep *EnginePlayer) Outputs(ctx context.Context) ([]mpv.Device, error) {
	devices, err := ep.router.Devices()
	if err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	out := make([]mpv.Device, 0, len(devices)+1)
	out = append(out, mpv.Device{Name: "auto", Description: "Autoselect device"})
	for _, d := range devices {
		out = append(out, mpv.Device{Name: d.Name, Description: d.Description})
	}
	return out, nil
}

// SetOutput routes output: "" means system default. The device must exist in
// the engine's list ("Unknown audio output" parity) and, when a device is
// already open, the switch closes and reopens it so playback continues on
// the new one (a failed swap restores the old device: "Audio output could
// not be changed"). The choice persists either way; a remembered device is
// only applied at start when it still exists.
func (ep *EnginePlayer) SetOutput(ctx context.Context, device string) (State, error) {
	if device != "" && !config.ValidDeviceName(device) {
		return ep.state(ctx), fmt.Errorf("Unknown audio output")
	}
	target, err := ep.resolveTarget(device)
	if err != nil {
		return ep.state(ctx), err
	}
	if err := ep.swap(target); err != nil {
		return ep.state(ctx), err
	}
	stored := device
	if target == "" {
		stored = "" // "auto" and other default aliases are never persisted
	}
	if err := rememberOutput(ep.store, stored); err != nil {
		return ep.state(ctx), fmt.Errorf("save output: %w", err)
	}
	return ep.state(ctx), nil
}

// Spectrum returns the latest band levels from the engine's sample tap. The
// analyzer runs off the playback path, so polling it at the panel's 100 ms
// cadence is cheap; it is the data behind the "spectrum" verb.
func (ep *EnginePlayer) Spectrum() audio.Snapshot { return ep.eng.Spectrum() }

// state assembles the reported state. The synthesized title is skipped when
// nothing is playing, so an idle poll never reaches out to Plaza (mpv parity:
// an idle player has no media-title).
func (ep *EnginePlayer) state(ctx context.Context) State {
	live := ep.eng.Status()
	out := State{Volume: live.Volume, Output: ep.reportOutput()}
	switch live.State {
	case audio.StatePlaying:
		out.Playing = true
	case audio.StateBuffering:
		out.Playing = true
		out.Buffering = true
	case audio.StatePaused:
		out.Playing = true
		out.Paused = true
	default:
		ep.mu.Lock()
		out.Paused = ep.paused
		ep.mu.Unlock()
		return out
	}
	out.Title = ep.currentTitle(ctx)
	return out
}

// ensureOpen opens the output on the remembered device once. A remembered
// device that no longer exists falls back to the system default without
// clearing the setting (mpv parity: the memory is only applied when valid).
func (ep *EnginePlayer) ensureOpen() error {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.opened {
		return nil
	}
	name := ""
	if s, err := ep.store.Load(); err == nil {
		name = migrateDeviceName(s.Output)
	}
	if name != "" {
		if devices, err := ep.router.Devices(); err != nil || !hasDevice(devices, name) {
			name = ""
		}
	}
	if err := ep.router.Open(name); err != nil {
		return fmt.Errorf("audio output is unavailable: %w", err)
	}
	ep.opened = true
	ep.device = name
	return nil
}

// resolveTarget maps the requested device onto an engine device name and
// verifies it exists. Enumeration failures pass the request through
// unverified: settings stay writable without a running sound server.
func (ep *EnginePlayer) resolveTarget(device string) (string, error) {
	target := migrateDeviceName(device)
	if target == "" {
		return "", nil
	}
	devices, err := ep.router.Devices()
	if err != nil {
		return target, nil
	}
	if !hasDevice(devices, target) {
		return "", fmt.Errorf("Unknown audio output")
	}
	return target, nil
}

// swap moves the open output onto target without disturbing playback: the
// backend's own reroute (PulseAudio's move-sink-input) is used when it has
// one, and a freshly opened output replaces the old one otherwise. A failed
// switch leaves the current device playing.
func (ep *EnginePlayer) swap(target string) error {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if !ep.opened || ep.device == target {
		return nil
	}
	if handled, err := ep.router.Move(target); handled && err == nil {
		ep.device = target
		return nil
	}
	if err := ep.router.Switch(target); err != nil {
		return fmt.Errorf("Audio output could not be changed")
	}
	ep.device = target
	return nil
}

// reportOutput is the honest current device: the open one, else the
// remembered choice, mirroring mpv's "audio-device" report.
func (ep *EnginePlayer) reportOutput() string {
	ep.mu.Lock()
	opened, device := ep.opened, ep.device
	ep.mu.Unlock()
	if opened {
		return normalizeOutput(device)
	}
	s, err := ep.store.Load()
	if err != nil {
		return ""
	}
	return normalizeOutput(migrateDeviceName(s.Output))
}

// currentTitle is the mpv media-title stand-in: Plaza metadata cached for
// titleTTL, last-known-good on failure, never an error (parity: an
// unavailable media-title is "", not a failure).
func (ep *EnginePlayer) currentTitle(ctx context.Context) string {
	if ep.titles == nil {
		return ""
	}
	ep.tmu.Lock()
	title, fetched := ep.title, ep.fetched
	ep.tmu.Unlock()
	if !fetched.IsZero() && time.Since(fetched) < titleTTL {
		return title
	}
	if data, err := ep.titles.Metadata(ctx); err == nil && data != nil {
		title = synthesizeTitle(data)
	}
	ep.tmu.Lock()
	ep.title = title // a failed fetch keeps the last known title
	ep.fetched = time.Now()
	ep.tmu.Unlock()
	return title
}

// synthesizeTitle builds the reported title the way the panel reads a track:
// "Artist — Title", or whichever half is present (mpv's media-title is the
// stream's own title string, which the engine has no access to).
func synthesizeTitle(data *plaza.Metadata) string {
	title := strings.TrimSpace(data.Title)
	artist := strings.TrimSpace(data.Artist)
	switch {
	case title == "":
		return artist
	case artist == "":
		return title
	}
	return artist + " — " + title
}

// migrateDeviceName maps device names remembered under the mpv backend onto
// engine names: mpv's "auto" and bare backend names mean the system default,
// and "pulse/<sink>" / "pipewire/<sink>" carry the sink the engine opens.
// Anything else passes through and is resolved against the live device list,
// where unknown names fall back to the default.
func migrateDeviceName(name string) string {
	name = strings.TrimSpace(name)
	switch name {
	case "", "auto", "pulse", "pipewire", "alsa", "jack":
		return ""
	}
	for _, prefix := range []string{"pulse/", "pipewire/"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return name
}

func hasDevice(devices []audio.Device, name string) bool {
	for _, d := range devices {
		if d.Name == name {
			return true
		}
	}
	return false
}

// outputRouter is the stable output the engine writes into. It owns one
// backend output at a time and swaps it in place, so a device switch neither
// restarts the engine nor reconnects the stream: outputs that can reroute
// (PulseAudio's move-sink-input) are moved, the rest are replaced by a fresh
// output opened on the new device. Connections are created lazily so device
// enumeration works before playback starts.
type outputRouter struct {
	newOut func() (audio.Output, error)

	mu  sync.Mutex
	out audio.Output // active output, nil until first use
}

// active returns the current output, creating the first connection lazily.
func (r *outputRouter) active() (audio.Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked()
}

func (r *outputRouter) activeLocked() (audio.Output, error) {
	if r.out == nil {
		out, err := r.newOut()
		if err != nil {
			return nil, err
		}
		r.out = out
	}
	return r.out, nil
}

func (r *outputRouter) Devices() ([]audio.Device, error) {
	out, err := r.active()
	if err != nil {
		return nil, err
	}
	return out.Devices()
}

func (r *outputRouter) Default() (audio.Device, error) {
	out, err := r.active()
	if err != nil {
		return audio.Device{}, err
	}
	return out.Default()
}

func (r *outputRouter) Open(device string) error {
	out, err := r.active()
	if err != nil {
		return err
	}
	return out.Open(device)
}

// Write, Flush and Underflow forward to the active output while holding the
// router lock, so a switch waits for an in-flight write instead of racing it.
func (r *outputRouter) Write(frames []float32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		return audio.ErrClosed
	}
	return r.out.Write(frames)
}

func (r *outputRouter) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		return audio.ErrClosed
	}
	return r.out.Flush()
}

func (r *outputRouter) Underflow() bool {
	r.mu.Lock()
	out := r.out
	r.mu.Unlock()
	return out != nil && out.Underflow()
}

func (r *outputRouter) Close() error {
	r.mu.Lock()
	out := r.out
	r.out = nil
	r.mu.Unlock()
	if out == nil {
		return nil
	}
	return out.Close()
}

// Move reroutes the active output when the backend supports it. It reports
// false when there is no move support, so callers fall back to Switch.
func (r *outputRouter) Move(device string) (bool, error) {
	r.mu.Lock()
	out := r.out
	r.mu.Unlock()
	if out == nil {
		return false, audio.ErrClosed
	}
	mover, ok := out.(audio.Mover)
	if !ok {
		return false, nil
	}
	if err := mover.Move(device); err != nil {
		return true, err
	}
	return true, nil
}

// Switch opens a fresh output on device and replaces the active one; the old
// output is closed only after the swap, so a failed open leaves the current
// device playing.
func (r *outputRouter) Switch(device string) error {
	fresh, err := r.newOut()
	if err != nil {
		return err
	}
	if err := fresh.Open(device); err != nil {
		_ = fresh.Close()
		return err
	}
	r.mu.Lock()
	old := r.out
	r.out = fresh
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}
