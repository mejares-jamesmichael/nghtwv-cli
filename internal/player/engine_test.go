package player

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopxl/beep/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

const engineTestRate = 44100

var engineFormat = beep.Format{SampleRate: engineTestRate, NumChannels: 2, Precision: 4}

// fakeEngineOutput is one device connection: it records which device was
// opened, accepts writes immediately (so playback reaches Playing quickly),
// and can be told to fail specific opens.
type fakeEngineOutput struct {
	mu      sync.Mutex
	devices []audio.Device
	openErr map[string]error

	opened bool
	device string
	opens  int
	closes int
}

func (o *fakeEngineOutput) Devices() ([]audio.Device, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]audio.Device(nil), o.devices...), nil
}

func (o *fakeEngineOutput) Default() (audio.Device, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.devices) == 0 {
		return audio.Device{}, audio.ErrNoDevice
	}
	return o.devices[0], nil
}

func (o *fakeEngineOutput) Open(device string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.openErr[device]; err != nil {
		return err
	}
	if o.opened {
		return errors.New("already open")
	}
	o.opened, o.device, o.opens = true, device, o.opens+1
	return nil
}

func (o *fakeEngineOutput) Write([]float32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.opened {
		return audio.ErrClosed
	}
	return nil
}

func (o *fakeEngineOutput) Flush() error    { return nil }
func (o *fakeEngineOutput) Underflow() bool { return false }

func (o *fakeEngineOutput) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.opened {
		return audio.ErrClosed
	}
	o.opened, o.closes = false, o.closes+1
	return nil
}

// openState reports the open device plus how often it changed.
func (o *fakeEngineOutput) openState() (opened bool, device string, opens, closes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened, o.device, o.opens, o.closes
}

// engineDevices is the fake backend's device list.
func engineDevices() []audio.Device {
	return []audio.Device{
		{Name: "sink-a", Description: "Desk speakers"},
		{Name: "sink-b", Description: "Headphones"},
	}
}

// engineFakes vends output connections the way the backend does: the first
// use creates one, and a device switch on a backend without reroute support
// creates another.
type engineFakes struct {
	mu      sync.Mutex
	outputs []*fakeEngineOutput
}

func (f *engineFakes) newOut() (audio.Output, error) {
	out := &fakeEngineOutput{devices: engineDevices(), openErr: map[string]error{}}
	f.mu.Lock()
	f.outputs = append(f.outputs, out)
	f.mu.Unlock()
	return out, nil
}

// all lists the connections created so far.
func (f *engineFakes) all() []*fakeEngineOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeEngineOutput(nil), f.outputs...)
}

// last is the most recently created connection (nil before the first use).
func (f *engineFakes) last() *fakeEngineOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.outputs) == 0 {
		return nil
	}
	return f.outputs[len(f.outputs)-1]
}

func (f *engineFakes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.outputs)
}

// movingOutput is a fake output with the optional audio.Mover capability: a
// device switch reroutes it instead of replacing the connection.
type movingOutput struct {
	*fakeEngineOutput

	mu   sync.Mutex
	to   string
	fail error
}

func (o *movingOutput) Move(device string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.to = device
	return nil
}

func (o *movingOutput) movedTo() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.to
}

// silenceStream is an endless PCM source, so the engine stays Playing
// instead of reconnecting.
type silenceStream struct{}

func (s *silenceStream) Stream(frames [][2]float64) (int, bool) {
	for i := range frames {
		frames[i] = [2]float64{}
	}
	return len(frames), true
}

func (s *silenceStream) Err() error   { return nil }
func (s *silenceStream) Close() error { return nil }

// openerCounter counts stream opens and can stall them (the buffering state).
type openerCounter struct {
	mu    sync.Mutex
	calls int
	stall bool
}

func (o *openerCounter) open(ctx context.Context) (audio.Stream, beep.Format, error) {
	o.mu.Lock()
	o.calls++
	stall := o.stall
	o.mu.Unlock()
	if stall {
		<-ctx.Done()
		return nil, beep.Format{}, ctx.Err()
	}
	return &silenceStream{}, engineFormat, nil
}

func (o *openerCounter) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// engineHarness is the engine player over the fakes, without a title source.
func engineHarness(store *memStore) (*EnginePlayer, *engineFakes, *openerCounter) {
	fakes := &engineFakes{}
	opener := &openerCounter{}
	return NewEnginePlayer(fakes.newOut, engineTestRate, opener.open, store, nil), fakes, opener
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// untilPlaying waits for the engine to be genuinely playing, past the
// buffering state Play reports while the stream connects.
func untilPlaying(t *testing.T, ep *EnginePlayer) {
	t.Helper()
	waitFor(t, "playing", func() bool {
		st := ep.Status(context.Background())
		return st.Playing && !st.Buffering
	})
}

func TestEnginePlayerTransportMapping(t *testing.T) {
	ep, fakes, _ := engineHarness(&memStore{})
	ctx := context.Background()

	st := ep.Status(ctx)
	if st.Playing || st.Paused || st.Buffering || st.Volume != DefaultVolume {
		t.Fatalf("fresh state = %+v, want the idle default", st)
	}
	if fakes.count() != 0 {
		t.Fatalf("status opened %d backend connections, want none", fakes.count())
	}

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	untilPlaying(t, ep)
	if st := ep.Status(ctx); st.Buffering || st.Paused {
		t.Fatalf("playing state = %+v, want plain playing", st)
	}

	if st, err := ep.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	} else if !st.Playing || !st.Paused {
		t.Fatalf("paused state = %+v, want playing+paused (mpv shape)", st)
	}

	if st, err := ep.Toggle(ctx); err != nil {
		t.Fatalf("Toggle: %v", err)
	} else if !st.Playing || st.Paused {
		t.Fatalf("toggled state = %+v, want playing", st)
	}

	if err := ep.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := ep.Status(ctx); st.Playing || st.Paused {
		t.Fatalf("stopped state = %+v, want idle", st)
	}
	// The device stays open across stop/start (the engine's contract).
	out := fakes.last()
	if opened, _, _, closes := out.openState(); !opened || closes != 0 {
		t.Fatalf("after stop: opened=%v closes=%d, want the output kept open", opened, closes)
	}
	if st, err := ep.Toggle(ctx); err != nil {
		t.Fatalf("Toggle after stop: %v", err)
	} else if !st.Playing {
		t.Fatalf("toggle after stop = %+v, want playing (startOrResume parity)", st)
	}
	_ = ep.Stop(ctx)
}

func TestEnginePlayerBufferingWhileConnecting(t *testing.T) {
	ep, _, opener := engineHarness(&memStore{})
	opener.stall = true
	ctx := context.Background()

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, "buffering", func() bool { return ep.Status(ctx).Buffering })
	if st := ep.Status(ctx); !st.Playing || st.Paused {
		t.Fatalf("buffering state = %+v, want playing+buffering", st)
	}
	_ = ep.Stop(ctx)
}

func TestEnginePlayerPauseWithNothingIsFlagOnly(t *testing.T) {
	ep, fakes, opener := engineHarness(&memStore{})
	ctx := context.Background()

	st, err := ep.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if st.Playing || !st.Paused {
		t.Fatalf("idle pause state = %+v, want {playing:false,paused:true} (mpv parity)", st)
	}
	if fakes.count() != 0 {
		t.Fatal("an idle pause must not open the device")
	}
	if got := opener.count(); got != 0 {
		t.Fatalf("stream opens = %d, want 0: an idle pause connects nothing", got)
	}

	// Play starts the stream and plays (startOrResume parity); toggle then
	// flips it back to paused.
	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	untilPlaying(t, ep)
	if st := ep.Status(ctx); st.Paused {
		t.Fatalf("play after an idle pause = %+v, want playing", st)
	}
	if st, err := ep.Toggle(ctx); err != nil {
		t.Fatalf("Toggle: %v", err)
	} else if !st.Paused {
		t.Fatalf("toggle = %+v, want paused", st)
	}
	_ = ep.Stop(ctx)
}

func TestEnginePlayerVolumeClampsAndPersists(t *testing.T) {
	store := &memStore{}
	ep, _, _ := engineHarness(store)
	ctx := context.Background()

	if st, err := ep.Volume(ctx, 150); err != nil {
		t.Fatalf("Volume: %v", err)
	} else if st.Volume != 100 || ep.eng.Volume() != 100 {
		t.Fatalf("volume state = %+v engine = %d, want clamped 100", st, ep.eng.Volume())
	}
	if store.s.Volume == nil || *store.s.Volume != 100 {
		t.Fatalf("persisted volume = %v, want 100", store.s.Volume)
	}
	if _, err := ep.Volume(ctx, -3); err != nil {
		t.Fatalf("Volume: %v", err)
	}
	if ep.eng.Volume() != 0 {
		t.Fatalf("engine volume = %d, want 0", ep.eng.Volume())
	}
}

func TestEnginePlayerStartsOnRememberedVolume(t *testing.T) {
	v := 33
	ep, _, _ := engineHarness(&memStore{s: config.Settings{Volume: &v}})
	if got := ep.Status(context.Background()).Volume; got != 33 {
		t.Fatalf("status volume = %d, want the remembered 33", got)
	}
}

func TestEnginePlayerSetOutputSwitchesAndPersists(t *testing.T) {
	store := &memStore{}
	ep, fakes, _ := engineHarness(store)
	ctx := context.Background()

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	untilPlaying(t, ep)
	if _, dev, _, _ := fakes.last().openState(); dev != "" {
		t.Fatalf("first open device = %q, want the backend default", dev)
	}

	st, err := ep.SetOutput(ctx, "sink-b")
	if err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if st.Output != "sink-b" {
		t.Fatalf("state output = %q, want sink-b", st.Output)
	}
	// A backend without reroute support gets a fresh connection on the new
	// device, and the old one is closed only after the swap.
	if got := fakes.count(); got != 2 {
		t.Fatalf("backend connections = %d, want the switch to open a fresh one", got)
	}
	if _, dev, _, _ := fakes.last().openState(); dev != "sink-b" {
		t.Fatalf("new connection device = %q, want sink-b", dev)
	}
	if opened, _, _, closes := fakes.all()[0].openState(); opened || closes != 1 {
		t.Fatalf("old connection: opened=%v closes=%d, want it released", opened, closes)
	}
	if store.s.Output != "sink-b" {
		t.Fatalf("persisted output = %q, want sink-b", store.s.Output)
	}
	// The engine itself never restarts: playback continues on the new device.
	untilPlaying(t, ep)

	// An unknown device is refused with the parity text and changes nothing.
	if _, err := ep.SetOutput(ctx, "sink-gone"); err == nil || err.Error() != "Unknown audio output" {
		t.Fatalf("unknown output error = %v, want the parity message", err)
	}
	if got := fakes.count(); got != 2 {
		t.Fatalf("backend connections = %d after a refused switch, want no new one", got)
	}
	if store.s.Output != "sink-b" {
		t.Fatalf("persisted output = %q after a refused switch, want sink-b", store.s.Output)
	}

	// Clearing the choice returns to the backend default.
	st, err = ep.SetOutput(ctx, "")
	if err != nil {
		t.Fatalf("SetOutput(default): %v", err)
	}
	if st.Output != "" || store.s.Output != "" {
		t.Fatalf("cleared output = %q / %q, want the default", st.Output, store.s.Output)
	}
	if _, dev, _, _ := fakes.last().openState(); dev != "" {
		t.Fatalf("device = %q, want the default", dev)
	}
	_ = ep.Stop(ctx)
}

func TestEnginePlayerSwitchMovesWhenTheBackendSupportsIt(t *testing.T) {
	out := &movingOutput{fakeEngineOutput: &fakeEngineOutput{devices: engineDevices(), openErr: map[string]error{}}}
	opener := &openerCounter{}
	connections := 0
	newOut := func() (audio.Output, error) { connections++; return out, nil }
	store := &memStore{}
	ep := NewEnginePlayer(newOut, engineTestRate, opener.open, store, nil)
	ctx := context.Background()

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	untilPlaying(t, ep)

	st, err := ep.SetOutput(ctx, "sink-b")
	if err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if st.Output != "sink-b" {
		t.Fatalf("state output = %q, want sink-b", st.Output)
	}
	if got := out.movedTo(); got != "sink-b" {
		t.Fatalf("stream moved to %q, want sink-b", got)
	}
	if _, dev, opens, _ := out.openState(); dev != "" || opens != 1 {
		t.Fatalf("device = %q opens = %d, want the stream left open", dev, opens)
	}
	if connections != 1 {
		t.Fatalf("backend connections = %d, want the single original one", connections)
	}
	if store.s.Output != "sink-b" {
		t.Fatalf("persisted output = %q, want sink-b", store.s.Output)
	}
	if st := ep.Status(ctx); !st.Playing || st.Paused {
		t.Fatalf("state = %+v, want it still playing", st)
	}
	_ = ep.Stop(ctx)
}

func TestEnginePlayerFailedSwitchKeepsPlaying(t *testing.T) {
	first := &fakeEngineOutput{devices: engineDevices(), openErr: map[string]error{}}
	second := &fakeEngineOutput{
		devices: engineDevices(),
		openErr: map[string]error{"sink-b": errors.New("device busy")},
	}
	vended := 0
	newOut := func() (audio.Output, error) {
		vended++
		if vended > 1 {
			return second, nil
		}
		return first, nil
	}
	store := &memStore{}
	ep := NewEnginePlayer(newOut, engineTestRate, (&openerCounter{}).open, store, nil)
	ctx := context.Background()

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	untilPlaying(t, ep)

	if _, err := ep.SetOutput(ctx, "sink-b"); err == nil || err.Error() != "Audio output could not be changed" {
		t.Fatalf("error = %v, want the parity message", err)
	}
	if _, dev, _, _ := first.openState(); dev != "" {
		t.Fatalf("device = %q, want the original default still playing", dev)
	}
	if store.s.Output != "" {
		t.Fatalf("persisted output = %q, want nothing saved after a failed switch", store.s.Output)
	}
	if st := ep.Status(ctx); !st.Playing || st.Paused {
		t.Fatalf("state = %+v, want playback untouched", st)
	}
	_ = ep.Stop(ctx)
}

func TestEnginePlayerAppliesRememberedDeviceOnStart(t *testing.T) {
	ctx := context.Background()

	// mpv-era names migrate: pipewire/<sink> carries the sink.
	store := &memStore{s: config.Settings{Output: "pipewire/sink-b"}}
	ep, fakes, _ := engineHarness(store)
	if st := ep.Status(ctx); st.Output != "sink-b" {
		t.Fatalf("status output = %q, want the migrated sink-b", st.Output)
	}
	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if _, dev, _, _ := fakes.last().openState(); dev != "sink-b" {
		t.Fatalf("opened device = %q, want the remembered sink-b", dev)
	}
	_ = ep.Stop(ctx)

	// A remembered device that vanished falls back to the default without
	// clearing the setting (mpv parity: the memory is only applied if valid).
	gone := &memStore{s: config.Settings{Output: "sink-gone"}}
	ep2, fakes2, _ := engineHarness(gone)
	if _, err := ep2.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if _, dev, _, _ := fakes2.last().openState(); dev != "" {
		t.Fatalf("opened device = %q, want the default", dev)
	}
	if gone.s.Output != "sink-gone" {
		t.Fatalf("remembered output = %q, want it left untouched", gone.s.Output)
	}
	_ = ep2.Stop(ctx)
}

func TestEnginePlayerOutputsKeepMPVShape(t *testing.T) {
	ep, _, _ := engineHarness(&memStore{})
	devices, err := ep.Outputs(context.Background())
	if err != nil {
		t.Fatalf("Outputs: %v", err)
	}
	want := []mpv.Device{
		{Name: "auto", Description: "Autoselect device"},
		{Name: "sink-a", Description: "Desk speakers"},
		{Name: "sink-b", Description: "Headphones"},
	}
	if len(devices) != len(want) {
		t.Fatalf("outputs = %+v, want %+v", devices, want)
	}
	for i := range want {
		if devices[i] != want[i] {
			t.Fatalf("outputs[%d] = %+v, want %+v", i, devices[i], want[i])
		}
	}
}

func TestEnginePlayerSpectrumComesFromTheTap(t *testing.T) {
	ep, _, _ := engineHarness(&memStore{})
	ctx := context.Background()

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	// The analyzer fills the log-spaced bands off the tapped PCM.
	waitFor(t, "band levels", func() bool { return len(ep.Spectrum().Bands) == audio.Bands() })
	_ = ep.Stop(ctx)
}

// fakeTitles counts metadata fetches and returns canned Plaza data.
type fakeTitles struct {
	mu    sync.Mutex
	calls int
	data  *plaza.Metadata
	err   error
}

func (f *fakeTitles) Metadata(context.Context) (*plaza.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.data, f.err
}

func (f *fakeTitles) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTitles) set(data *plaza.Metadata, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data, f.err = data, err
}

// shortenTitleTTL makes the title cache expire immediately for one test.
func shortenTitleTTL(t *testing.T) {
	t.Helper()
	old := titleTTL
	titleTTL = time.Millisecond
	t.Cleanup(func() { titleTTL = old })
}

func TestEnginePlayerTitleFromPlaza(t *testing.T) {
	titles := &fakeTitles{data: &plaza.Metadata{Title: "Neon Rain", Artist: "Aeon"}}
	fakes := &engineFakes{}
	opener := &openerCounter{}
	ep := NewEnginePlayer(fakes.newOut, engineTestRate, opener.open, &memStore{}, titles)
	ctx := context.Background()

	if _, err := ep.Play(ctx); err != nil {
		t.Fatalf("Play: %v", err)
	}
	untilPlaying(t, ep)

	if got := ep.Status(ctx).Title; got != "Aeon — Neon Rain" {
		t.Fatalf("title = %q, want the synthesized artist — title", got)
	}
	if got := titles.count(); got != 1 {
		t.Fatalf("title fetches = %d, want 1 (cached within the TTL)", got)
	}
	_ = ep.Status(ctx)
	if got := titles.count(); got != 1 {
		t.Fatalf("title fetches = %d, want the cache reused", got)
	}

	// A failed refresh keeps the last known title instead of blanking it.
	shortenTitleTTL(t)
	titles.set(nil, errors.New("plaza down"))
	time.Sleep(2 * time.Millisecond)
	if got := ep.Status(ctx).Title; got != "Aeon — Neon Rain" {
		t.Fatalf("title after a failed refresh = %q, want the last known", got)
	}
	if got := titles.count(); got < 2 {
		t.Fatalf("title fetches = %d, want a refresh attempt", got)
	}
	_ = ep.Stop(ctx)

	// Idle status never reaches Plaza (mpv parity: no media-title).
	before := titles.count()
	if got := ep.Status(ctx).Title; got != "" {
		t.Fatalf("idle title = %q, want empty", got)
	}
	if got := titles.count(); got != before {
		t.Fatalf("idle status fetched metadata (%d → %d), want none", before, got)
	}
}

func TestSynthesizeTitle(t *testing.T) {
	cases := []struct{ title, artist, want string }{
		{"Neon Rain", "Aeon", "Aeon — Neon Rain"},
		{"Neon Rain", "  ", "Neon Rain"},
		{"", "Aeon", "Aeon"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := synthesizeTitle(&plaza.Metadata{Title: c.title, Artist: c.artist}); got != c.want {
			t.Errorf("synthesizeTitle(%q, %q) = %q, want %q", c.title, c.artist, got, c.want)
		}
	}
}

func TestMigrateDeviceName(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"auto":                 "",
		"pulse":                "",
		"pipewire":             "",
		"alsa":                 "",
		"pulse/sink-a":         "sink-a",
		"pipewire/sink-a":      "sink-a",
		"sink-a":               "sink-a",
		"{0.0.0.00000000}.{x}": "{0.0.0.00000000}.{x}",
	}
	for in, want := range cases {
		if got := migrateDeviceName(in); got != want {
			t.Errorf("migrateDeviceName(%q) = %q, want %q", in, got, want)
		}
	}
}
