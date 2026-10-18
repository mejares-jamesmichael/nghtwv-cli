// Package player coordinates transport semantics on top of the mpv IPC
// endpoint: play/pause/stop toggling, volume, output routing, and honest
// status reporting (parity with plaza-player's controller behavior).
//
// Seams: this package declares the interfaces it needs — the IPC client
// and the attach-or-spawn supervisor — so tests drive fakes instead of a
// real mpv. mpv.Client and mpv.Supervisor satisfy them.
package player

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
)

// StreamURL is the Nightwave Plaza stream (parity source constant).
const StreamURL = "https://radio.plaza.one/mp3"

// DefaultVolume is the spawn default (parity: --volume=70).
const DefaultVolume = 70

// State mirrors plaza-player's state() output shape exactly.
type State struct {
	Playing   bool   `json:"playing"`
	Paused    bool   `json:"paused"`
	Buffering bool   `json:"buffering"`
	Volume    int    `json:"volume"`
	Title     string `json:"title"`
	Output    string `json:"output"`
}

// Client is the IPC surface the player needs (satisfied by mpv.Client).
type Client interface {
	Call(ctx context.Context, command []any) (json.RawMessage, error)
}

// Supervisor guarantees a responding endpoint: attach-or-spawn plus the
// startup handshake (satisfied by mpv.Supervisor).
type Supervisor interface {
	Alive(ctx context.Context) bool
	Ensure(ctx context.Context, spawnWithStream bool) (spawned bool, err error)
}

// Store persists user settings (satisfied by config.Load/Save wrapper).
type Store interface {
	Load() (config.Settings, error)
	Save(config.Settings) error
}

// Player is the semantic controller over one mpv endpoint.
type Player struct {
	client Client
	sup    Supervisor
	store  Store
}

// New builds a player bound to one endpoint's client, supervisor, and
// settings store.
func New(client Client, sup Supervisor, store Store) *Player {
	return &Player{client: client, sup: sup, store: store}
}

// Status reads live playback state. Any IPC failure collapses to the
// neutral state (honest idle with remembered defaults) — never an error,
// matching the plugin's state() behavior. Parity exception: the neutral
// volume honours a remembered setting instead of the fixed 70.
func (p *Player) Status(ctx context.Context) State {
	s, ok := p.collectState(ctx)
	if !ok {
		return p.neutral()
	}
	return s
}

// Play starts the stream or resumes it, spawning mpv when absent (parity:
// launch(play_stream=True) + start_stream()).
func (p *Player) Play(ctx context.Context) (State, error) {
	if _, ok := p.probeBool(ctx, "idle-active"); !ok {
		if _, err := p.sup.Ensure(ctx, true); err != nil {
			return p.neutral(), err
		}
		return p.Status(ctx), nil
	}
	return p.startOrResume(ctx)
}

// Pause is the inverse of Play, for symmetry: paused spawn when absent.
func (p *Player) Pause(ctx context.Context) (State, error) {
	if _, ok := p.probeBool(ctx, "idle-active"); !ok {
		if _, err := p.sup.Ensure(ctx, false); err != nil {
			return p.neutral(), err
		}
	}
	if err := p.setProperty(ctx, "pause", true); err != nil {
		return p.Status(ctx), fmt.Errorf("pause: %w", err)
	}
	return p.Status(ctx), nil
}

// Toggle starts playback when absent/idle, otherwise flips pause (parity).
func (p *Player) Toggle(ctx context.Context) (State, error) {
	idle, ok := p.probeBool(ctx, "idle-active")
	if !ok {
		if _, err := p.sup.Ensure(ctx, true); err != nil {
			return p.neutral(), err
		}
		return p.Status(ctx), nil
	}
	if idle {
		return p.startOrResume(ctx)
	}
	paused, pausedOK := p.probeBool(ctx, "pause")
	if !pausedOK {
		return p.neutral(), fmt.Errorf("read pause")
	}
	if err := p.setProperty(ctx, "pause", !paused); err != nil {
		return p.Status(ctx), fmt.Errorf("flip pause: %w", err)
	}
	return p.Status(ctx), nil
}

// Stop asks mpv to quit over IPC and succeeds silently when already
// stopped (parity). Endpoint residue is cleaned at the next launch.
func (p *Player) Stop(ctx context.Context) error {
	_, _ = p.client.Call(ctx, []any{"quit"}) // expected to fail once mpv exits mid-call
	return nil
}

// Volume sets the volume, clamped to 0-100, spawning an idle mpv when
// absent (parity: no autoplay from a volume command). The chosen volume
// persists in settings.
func (p *Player) Volume(ctx context.Context, volume int) (State, error) {
	v := clamp(volume)
	if _, err := p.sup.Ensure(ctx, false); err != nil {
		return p.neutral(), err
	}
	if err := p.setProperty(ctx, "volume", v); err != nil {
		return p.Status(ctx), fmt.Errorf("set volume: %w", err)
	}
	if err := p.rememberVolume(v); err != nil {
		return p.Status(ctx), fmt.Errorf("save volume: %w", err)
	}
	return p.Status(ctx), nil
}

// Outputs enumerates audio output devices, spawning an idle mpv when
// absent (it is the only enumeration source, replacing the plugin's pactl).
// mpv's "auto" entry is included and means system default.
func (p *Player) Outputs(ctx context.Context) ([]mpv.Device, error) {
	if _, err := p.sup.Ensure(ctx, false); err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	raw, err := p.getProperty(ctx, "audio-device-list")
	if err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	devices, err := mpv.PropDevices(raw)
	if err != nil {
		return nil, fmt.Errorf("audio outputs are unavailable: %w", err)
	}
	return devices, nil
}

// SetOutput routes movement: empty device means system default ("auto").
// When mpv is running the device must exist in audio-device-list (unknown
// → "Unknown audio output", failed switch → "Audio output could not be
// changed", both parity messages). The choice persists either way; a
// remembered device is only ever applied at launch if still present.
func (p *Player) SetOutput(ctx context.Context, device string) (State, error) {
	if device != "" && !config.ValidDeviceName(device) {
		return p.Status(ctx), fmt.Errorf("Unknown audio output")
	}
	alive := p.sup.Alive(ctx)
	if alive {
		if err := p.verifyAndApply(ctx, device); err != nil {
			return p.Status(ctx), err
		}
	}
	if err := p.rememberOutput(device); err != nil {
		return p.Status(ctx), fmt.Errorf("save output: %w", err)
	}
	return p.Status(ctx), nil
}

func (p *Player) verifyAndApply(ctx context.Context, device string) error {
	target := device // "" → "auto"
	if device != "" {
		raw, err := p.getProperty(ctx, "audio-device-list")
		if err != nil {
			return fmt.Errorf("audio outputs are unavailable: %w", err)
		}
		devices, err := mpv.PropDevices(raw)
		if err != nil {
			return fmt.Errorf("audio outputs are unavailable: %w", err)
		}
		found := false
		for _, d := range devices {
			if d.Name == device {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("Unknown audio output")
		}
	}
	if target == "" {
		target = "auto"
	}
	if err := p.setProperty(ctx, "audio-device", target); err != nil {
		return fmt.Errorf("Audio output could not be changed")
	}
	return nil
}

// startOrResume loads the stream if idle, then unpauses (parity
// start_stream()).
func (p *Player) startOrResume(ctx context.Context) (State, error) {
	idle, ok := p.probeBool(ctx, "idle-active")
	if !ok {
		return p.neutral(), fmt.Errorf("read idle")
	}
	if idle {
		if _, err := p.client.Call(ctx, []any{"loadfile", StreamURL, "replace"}); err != nil {
			return p.Status(ctx), fmt.Errorf("load stream: %w", err)
		}
	}
	if err := p.setProperty(ctx, "pause", false); err != nil {
		return p.Status(ctx), fmt.Errorf("unpause: %w", err)
	}
	return p.Status(ctx), nil
}

// collectState gates on one STRICT probe (idle-active decides live-vs-neutral
// entirely, parity), then reads every remaining property tolerantly —
// per-property defaults on failure (parity: the plugin's get_property calls
// each had their own default). This matters: freshly loaded streams report
// "property unavailable" for paused-for-cache/media-title for several
// seconds while the demuxer spins up; a strict read would fake-idle the
// whole status, the plugin tolerated those individually, and so do we.
func (p *Player) collectState(ctx context.Context) (State, bool) {
	idleRaw, err := p.getProperty(ctx, "idle-active")
	if err != nil {
		return State{}, false
	}
	idle, err := mpv.PropBool(idleRaw)
	if err != nil {
		return State{}, false
	}
	paused := p.tolerantBool(ctx, "pause")
	buffering := p.tolerantBool(ctx, "paused-for-cache")
	title, _ := p.probeString(ctx, "media-title") // "" when unavailable
	volume := p.tolerantVolume(ctx)
	output := p.currentOutput(ctx)
	return State{
		Playing:   !idle,
		Paused:    paused,
		Buffering: buffering,
		Volume:    volume,
		Title:     title,
		Output:    normalizeOutput(output),
	}, true
}

// tolerantBool mirrors the plugin's get_property(name, false) default.
func (p *Player) tolerantBool(ctx context.Context, name string) bool {
	v, ok := p.probeBool(ctx, name)
	return ok && v
}

// tolerantVolume mirrors the plugin's get_property("volume", 70) default.
func (p *Player) tolerantVolume(ctx context.Context) int {
	raw, err := p.getProperty(ctx, "volume")
	if err != nil {
		return DefaultVolume
	}
	f, err := mpv.PropFloat(raw)
	if err != nil {
		return DefaultVolume
	}
	return clamp(int(math.Round(f)))
}

func (p *Player) probeBool(ctx context.Context, name string) (bool, bool) {
	raw, err := p.getProperty(ctx, name)
	if err != nil {
		debugProbe(name, err)
		return false, false
	}
	v, err := mpv.PropBool(raw)
	if err != nil {
		debugProbe(name, err)
		return false, false
	}
	return v, true
}

// debugProbe surfaces swallowed probe failures when NGHTWV_DEBUG=1.
func debugProbe(name string, err error) {
	if os.Getenv("NGHTWV_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "probe %s failed: %v\n", name, err)
	}
}

func (p *Player) probeString(ctx context.Context, name string) (string, bool) {
	raw, err := p.getProperty(ctx, name)
	if err != nil {
		return "", false
	}
	v, err := mpv.PropString(raw)
	if err != nil {
		return "", false
	}
	return v, true
}

// getProperty reads one property through the transport seam (the player
// interface is Call-only; property naming is internal convention).
func (p *Player) getProperty(ctx context.Context, name string) (json.RawMessage, error) {
	return p.client.Call(ctx, []any{"get_property", name})
}

// setProperty writes one property through the transport seam.
func (p *Player) setProperty(ctx context.Context, name string, value any) error {
	_, err := p.client.Call(ctx, []any{"set_property", name, value})
	return err
}

// currentOutput reads the live audio-device property; "" when auto or
// unreadable (mpv keeps the device on its own naming scheme).
func (p *Player) currentOutput(ctx context.Context) string {
	raw, err := p.getProperty(ctx, "audio-device")
	if err != nil {
		return ""
	}
	s, err := mpv.PropString(raw)
	if err != nil {
		return ""
	}
	return normalizeOutput(s)
}

// normalizeOutput maps mpv's auto/empty device to the report form "".
func normalizeOutput(device string) string {
	if device == "" || device == "auto" {
		return ""
	}
	return device
}

// neutral is the honest idle state when mpv is absent or unreachable.
func (p *Player) neutral() State {
	return NeutralState(p.store)
}

// NeutralState is the honest idle state for an unreachable endpoint:
// remembered volume and output, nothing playing (parity: state() collapse
// to defaults on any IPC failure). Exported so the service client reports
// the same state when no service is running.
func NeutralState(store Store) State {
	s, err := store.Load()
	if err != nil {
		s = config.Settings{}
	}
	vol := DefaultVolume
	if s.Volume != nil {
		vol = clamp(*s.Volume)
	}
	return State{Volume: vol, Output: s.Output}
}

func (p *Player) rememberVolume(v int) error {
	s, err := p.store.Load()
	if err != nil {
		return err
	}
	s.Volume = &v
	return p.store.Save(s)
}

func (p *Player) rememberOutput(device string) error {
	s, err := p.store.Load()
	if err != nil {
		return err
	}
	s.Output = device
	return p.store.Save(s)
}

func clamp(volume int) int {
	if volume < 0 {
		return 0
	}
	if volume > 100 {
		return 100
	}
	return volume
}
