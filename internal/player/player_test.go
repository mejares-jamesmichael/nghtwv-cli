package player

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
)

// --- fakes ---

// fakeIPC speaks just enough protocol for the player: scripted properties,
// recorded commands, set_property/loadfile effects applied back into props.
type fakeIPC struct {
	props    map[string]json.RawMessage
	fail     bool
	commands [][]any
}

func (f *fakeIPC) Call(ctx context.Context, command []any) (json.RawMessage, error) {
	f.commands = append(f.commands, command)
	if f.fail {
		return nil, errors.New("dial: connection refused")
	}
	switch command[0] {
	case "get_property":
		name, _ := command[1].(string)
		raw, ok := f.props[name]
		if !ok {
			return nil, mpv.MPVError("property unavailable")
		}
		return raw, nil
	case "set_property":
		name, _ := command[1].(string)
		b, _ := json.Marshal(command[2])
		f.props[name] = b
		return json.RawMessage("null"), nil
	case "loadfile":
		f.props["idle-active"] = json.RawMessage("false")
		return json.RawMessage("null"), nil
	case "quit":
		f.props["quit-called"] = json.RawMessage("true")
		return json.RawMessage("null"), nil
	default:
		return nil, errors.New("unexpected command")
	}
}

func (f *fakeIPC) called(verb string) bool {
	for _, c := range f.commands {
		if v, _ := c[0].(string); v == verb {
			return true
		}
	}
	return false
}

// fakeSup records attach-or-spawn decisions.
type fakeSup struct {
	aliveResult bool
	spawned     bool
	ensureErr   error
	ensureCalls []bool // spawnWithStream flags, in order
	onEnsure    func() // hook: runs at Ensure time (e.g. endpoint comes alive)
}

func (f *fakeSup) Alive(ctx context.Context) bool { return f.aliveResult }
func (f *fakeSup) Ensure(ctx context.Context, on bool) (bool, error) {
	f.ensureCalls = append(f.ensureCalls, on)
	if f.onEnsure != nil {
		f.onEnsure()
	}
	return f.spawned, f.ensureErr
}

// fullProps is a complete live-mpv property set: collectState mirrors the
// parity all-or-nothing rule, so tests must seed every property that real
// mpv always serves.
func fullProps(idle, paused bool) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"idle-active":      rawBool(idle),
		"pause":            rawBool(paused),
		"paused-for-cache": rawBool(false),
		"media-title":      json.RawMessage(`"mp3"`),
		"volume":           json.RawMessage(`70`),
		"audio-device":     json.RawMessage(`"auto"`),
	}
}

func mergeProps(base, extra map[string]json.RawMessage) map[string]json.RawMessage {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// memStore is an in-memory settings store.
type memStore struct {
	s     config.Settings
	saves int
}

func (m *memStore) Load() (config.Settings, error) { return m.s, nil }
func (m *memStore) Save(s config.Settings) error   { m.s = s; m.saves++; return nil }

func rawBool(b bool) json.RawMessage { b2, _ := json.Marshal(b); return b2 }

func liveHarness(props map[string]json.RawMessage) (*Player, *fakeIPC, *fakeSup, *memStore) {
	ipc := &fakeIPC{props: props}
	sup := &fakeSup{}
	store := &memStore{}
	return New(ipc, sup, store), ipc, sup, store
}

// --- status ---

func TestStatusLiveAllProperties(t *testing.T) {
	pl, _, _, _ := liveHarness(map[string]json.RawMessage{
		"idle-active":      rawBool(false),
		"pause":            rawBool(true),
		"paused-for-cache": rawBool(true),
		"media-title":      json.RawMessage(`"mp3"`),
		"volume":           json.RawMessage(`55.4`),
		"audio-device":     json.RawMessage(`"pipewire/alsa_output.x"`),
	})
	got := pl.Status(context.Background())
	want := State{
		Playing: true, Paused: true, Buffering: true,
		Volume: 55, Title: "mp3", Output: "pipewire/alsa_output.x",
	}
	if got != want {
		t.Fatalf("State = %+v, want %+v", got, want)
	}
}

func TestStatusDeadCollapsesToNeutral(t *testing.T) {
	pl, ipc, _, store := liveHarness(nil)
	ipc.fail = true
	store.s = config.Settings{Output: "wasapi/speakers"} // remembered, unapplied

	got := pl.Status(context.Background())
	want := State{Volume: DefaultVolume, Output: "wasapi/speakers"}
	if got != want {
		t.Fatalf("State = %+v, want %+v", got, want)
	}
}

func TestStatusPropertyFailureDegradesGracefully(t *testing.T) {
	// A freshly spawned stream answers idle-active but reports "property
	// unavailable" for paused-for-cache (and sometimes media-title) while
	// the demuxer spins up. Parity: per-property defaults keep the status
	// honest and live instead of faking a full idle collapse.
	pl, _, _, _ := liveHarness(map[string]json.RawMessage{
		"idle-active": rawBool(false),
		"volume":      json.RawMessage(`55.4`),
	})
	got := pl.Status(context.Background())
	want := State{Playing: true, Volume: 55} // pause/buffering/title/output default
	if got != want {
		t.Fatalf("State = %+v, want %+v", got, want)
	}
}

func TestStatusIdleGateStillCollapsesToNeutral(t *testing.T) {
	// Only the idle-active gate is strict (parity): when the endpoint dies
	// entirely, everything collapses to the remembered neutral state.
	pl, ipc, _, store := liveHarness(map[string]json.RawMessage{
		"idle-active": rawBool(false),
	})
	ipc.fail = true
	store.s = config.Settings{Output: "wasapi/speakers"}

	got := pl.Status(context.Background())
	want := State{Volume: DefaultVolume, Output: "wasapi/speakers"}
	if got != want {
		t.Fatalf("State = %+v, want %+v", got, want)
	}
}

func TestStatusNormalizesAutoOutput(t *testing.T) {
	pl, _, _, _ := liveHarness(fullProps(true, false)) // idle mpv on auto output
	got := pl.Status(context.Background())
	if got.Output != "" {
		t.Fatalf("auto device must report \"\", got %q", got.Output)
	}
	if got.Playing {
		t.Fatal("idle mpv must not report Playing")
	}
}

// --- play / pause / toggle ---

func TestPlayDeadSpawnsWithStream(t *testing.T) {
	pl, ipc, sup, store := liveHarness(nil)
	ipc.fail = true
	store.s = config.Settings{}

	if _, err := pl.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if len(sup.ensureCalls) != 1 || !sup.ensureCalls[0] {
		t.Fatalf("Play must Ensure(spawnWithStream=true), got %v", sup.ensureCalls)
	}
}

func TestPlayIdleLoadsAndUnpauses(t *testing.T) {
	pl, ipc, sup, _ := liveHarness(fullProps(true, false))

	got, err := pl.Play(context.Background())
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	if !ipc.called("loadfile") {
		t.Fatal("idle mpv must load the stream")
	}
	if !got.Playing || got.Paused {
		t.Fatalf("after play the state must be live: %+v", got)
	}
	if len(sup.ensureCalls) != 0 {
		t.Fatal("live mpv must not be respawned")
	}
}

func TestPlayPlayingOnlyUnpauses(t *testing.T) {
	pl, ipc, _, _ := liveHarness(fullProps(false, false))
	if _, err := pl.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if ipc.called("loadfile") {
		t.Fatal("playing mpv must not reload the stream")
	}
}

func TestToggleIdleLoads(t *testing.T) {
	pl, ipc, _, _ := liveHarness(fullProps(true, false))
	got, err := pl.Toggle(context.Background())
	if err != nil {
		t.Fatalf("Toggle: %v", err)
	}
	if !ipc.called("loadfile") {
		t.Fatal("toggle on idle must load the stream")
	}
	if !got.Playing || got.Paused {
		t.Fatalf("state after toggle-on-idle: %+v", got)
	}
}

func TestTogglePlayingFlipsPause(t *testing.T) {
	pl, ipc, _, _ := liveHarness(fullProps(false, false))
	got, err := pl.Toggle(context.Background())
	if err != nil {
		t.Fatalf("Toggle: %v", err)
	}
	if !ipc.called("set_property") {
		t.Fatal("toggle on playing must flip pause")
	}
	if !got.Paused || !got.Playing {
		t.Fatalf("state after toggle should be paused-but-live: %+v", got)
	}
}

func TestPauseDeadSpawnsIdleThenPauses(t *testing.T) {
	ipc := &fakeIPC{fail: true, props: fullProps(true, false)}
	sup := &fakeSup{spawned: true}
	sup.onEnsure = func() { ipc.fail = false } // endpoint comes alive after spawn
	store := &memStore{}
	pl := New(ipc, sup, store)

	got, err := pl.Pause(context.Background())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if len(sup.ensureCalls) != 1 || sup.ensureCalls[0] {
		t.Fatalf("Pause must Ensure(spawnWithStream=false), got %v", sup.ensureCalls)
	}
	if !got.Paused || got.Playing {
		t.Fatalf("state after pause: %+v", got)
	}
}

// --- stop ---

func TestStopAsksQuitAndSucceedsSilently(t *testing.T) {
	pl, ipc, _, _ := liveHarness(map[string]json.RawMessage{"idle-active": rawBool(false)})
	if err := pl.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !ipc.called("quit") {
		t.Fatal("Stop must issue IPC quit")
	}

	// Dead endpoint quits are swallowed (parity: succeeds when already stopped).
	pl2, ipc2, _, _ := liveHarness(nil)
	ipc2.fail = true
	if err := pl2.Stop(context.Background()); err != nil {
		t.Fatalf("Stop on dead endpoint must not error: %v", err)
	}
}

// --- volume ---

func TestVolumeClampsAndPersists(t *testing.T) {
	cases := []struct {
		in, out int
	}{
		{-5, 0}, {0, 0}, {42, 42}, {100, 100}, {140, 100},
	}
	for _, c := range cases {
		pl, ipc, sup, store := liveHarness(fullProps(true, false))
		got, err := pl.Volume(context.Background(), c.in)
		if err != nil {
			t.Fatalf("Volume(%d): %v", c.in, err)
		}
		if got.Volume != c.out {
			t.Fatalf("Volume(%d) reported %d, want %d", c.in, got.Volume, c.out)
		}
		if store.s.Volume == nil || *store.s.Volume != c.out {
			t.Fatalf("Volume(%d) persisted %+v, want %d", c.in, store.s.Volume, c.out)
		}
		if len(sup.ensureCalls) != 1 || sup.ensureCalls[0] {
			t.Fatalf("Volume must Ensure(spawnWithStream=false), got %v", sup.ensureCalls)
		}
		_ = ipc
	}
}

// --- outputs ---

func deviceListJSON() json.RawMessage {
	return json.RawMessage(`[{"name":"auto","description":"Autoselect device"},
		{"name":"pipewire/alsa_output.x","description":"Studio monitors"}]`)
}

func TestOutputsEnumerates(t *testing.T) {
	pl, _, sup, _ := liveHarness(map[string]json.RawMessage{"audio-device-list": deviceListJSON()})
	devices, err := pl.Outputs(context.Background())
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}
	if len(devices) != 2 || devices[0].Name != "auto" {
		t.Fatalf("devices = %+v", devices)
	}
	// enumeration is allowed to spawn an idle mpv (never with a stream)
	if len(sup.ensureCalls) != 1 || sup.ensureCalls[0] {
		t.Fatalf("Outputs must Ensure(spawnWithStream=false), got %v", sup.ensureCalls)
	}
}

func TestOutputsUnavailableOnError(t *testing.T) {
	pl, ipc, _, _ := liveHarness(nil)
	ipc.fail = true
	if _, err := pl.Outputs(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "audio outputs are unavailable") {
		t.Fatalf("err = %v, want availability failure", err)
	}
}

func TestSetOutputAppliesWhenAlive(t *testing.T) {
	ipc := &fakeIPC{props: mergeProps(fullProps(false, false), map[string]json.RawMessage{
		"audio-device-list": deviceListJSON(),
	})}
	store := &memStore{}
	pl := New(ipc, &fakeSup{aliveResult: true}, store)

	got, err := pl.SetOutput(context.Background(), "pipewire/alsa_output.x")
	if err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if store.s.Output != "pipewire/alsa_output.x" {
		t.Fatalf("remembered output = %q", store.s.Output)
	}
	if got.Output != "pipewire/alsa_output.x" {
		t.Fatalf("live output now = %q", got.Output)
	}
	applied := string(ipc.props["audio-device"])
	if applied != `"pipewire/alsa_output.x"` {
		t.Fatalf("set_property audio-device got %s", applied)
	}
}

func TestSetOutputAutoWhenEmpty(t *testing.T) {
	ipc := &fakeIPC{props: mergeProps(fullProps(false, false), map[string]json.RawMessage{
		"audio-device-list": deviceListJSON(),
	})}
	store := &memStore{}
	pl := New(ipc, &fakeSup{aliveResult: true}, store)

	got, err := pl.SetOutput(context.Background(), "")
	if err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if store.s.Output != "" {
		t.Fatalf("remembered output = %q, want \"\"", store.s.Output)
	}
	if got.Output != "" || !ipc.called("set_property") {
		t.Fatalf("auto must be applied live and clear the record: %+v", got)
	}
}

func TestSetOutputUnknownRejected(t *testing.T) {
	ipc := &fakeIPC{props: mergeProps(fullProps(false, false), map[string]json.RawMessage{
		"audio-device-list": deviceListJSON(),
	})}
	store := &memStore{}
	pl := New(ipc, &fakeSup{aliveResult: true}, store)

	if _, err := pl.SetOutput(context.Background(), "not/in/the/list"); err == nil ||
		err.Error() != "Unknown audio output" {
		t.Fatalf("err = %v, want Unknown audio output", err)
	}
	if store.s.Output != "" {
		t.Fatalf("failed switch must not persist, got %q", store.s.Output)
	}
}

func TestSetOutputDeadPersistsOnly(t *testing.T) {
	pl, ipc, sup, store := liveHarness(nil)
	ipc.fail = true

	if _, err := pl.SetOutput(context.Background(), "wasapi/speakers"); err != nil {
		t.Fatalf("SetOutput with mpv down persists only, err: %v", err)
	}
	if store.s.Output != "wasapi/speakers" {
		t.Fatalf("remembered output = %q", store.s.Output)
	}
	if sup.aliveResult || len(sup.ensureCalls) != 0 {
		t.Fatalf("dead endpoint must skip live application: sup=%+v", sup)
	}
}
