package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbletea/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// fakePlayback records every panel action (fake-player tests: no mpv,
// no sockets).
type fakePlayback struct {
	st      player.State
	toggleN int
	stopN   int
	volumes []int
	err     error
}

func (f *fakePlayback) Status(context.Context) player.State { return f.st }

func (f *fakePlayback) Toggle(context.Context) (player.State, error) {
	f.toggleN++
	return f.st, f.err
}

func (f *fakePlayback) Stop(context.Context) error { f.stopN++; return f.err }

func (f *fakePlayback) Volume(_ context.Context, volume int) (player.State, error) {
	f.volumes = append(f.volumes, volume)
	f.st.Volume = volume
	return f.st, f.err
}

func (f *fakePlayback) SetOutput(context.Context, string) (player.State, error) {
	return f.st, f.err
}

func (f *fakePlayback) Outputs(context.Context) ([]mpv.Device, error) { return nil, f.err }

// fakeMeta counts metadata fetches (fake-plaza tests: no HTTP).
type fakeMeta struct {
	data *plaza.Metadata
	err  error
	n    int
}

func (f *fakeMeta) Metadata(context.Context) (*plaza.Metadata, error) {
	f.n++
	return f.data, f.err
}

// press feeds a key to the model and returns the folded model + command.
func press(m Model, name, text string) (Model, tea.Cmd) {
	var km tea.Key
	switch {
	case name == "space" && text == "":
		km = tea.Key{Code: tea.KeySpace}
	case name == "esc" && text == "":
		km = tea.Key{Code: tea.KeyEscape}
	case text != "":
		km = tea.Key{Code: rune(text[0]), Text: text}
	default:
		km = tea.Key{Code: rune(name[0]), Text: name}
	}
	next, cmd := m.Update(tea.KeyPressMsg(km))
	return next.(Model), cmd
}

// invoke runs a command inline (fakes are synchronous) and returns the msg.
func invoke(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	return cmd()
}

func intPtr(v int) *int { return &v }

func TestBadgeParity(t *testing.T) {
	cases := []struct {
		st   player.State
		want string
	}{
		{player.State{}, "IDLE"},
		{player.State{Playing: true}, "LIVE"},
		{player.State{Playing: true, Paused: true}, "PAUSED"},
		{player.State{Playing: true, Buffering: true}, "BUFFERING"},
	}
	for _, c := range cases {
		if got, _ := badge(c.st); got != c.want {
			t.Errorf("badge(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

func TestKeysDriveActions(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Volume: 70}}
	m := New(play, &fakeMeta{})

	// space → toggle
	_, cmd := press(m, "space", "")
	if cmd == nil {
		t.Fatal("space must produce a toggle action")
	}
	_ = invoke(t, cmd) // runs the fake synchronously
	if play.toggleN != 1 {
		t.Fatalf("toggle calls = %d, want 1", play.toggleN)
	}

	// s → stop
	_, cmd = press(m, "s", "s")
	if cmd == nil {
		t.Fatal("s must produce a stop action")
	}
	_ = invoke(t, cmd)
	if play.stopN != 1 {
		t.Fatalf("stop calls = %d, want 1", play.stopN)
	}

	// + / = / - → volume steps of 5 (panel parity)
	for _, key := range []string{"+", "="} {
		play.volumes = nil
		_, cmd := press(m, key, key)
		if cmd == nil {
			t.Fatalf("%s must adjust volume", key)
		}
		_ = invoke(t, cmd)
		if len(play.volumes) != 1 || play.volumes[0] != 75 {
			t.Fatalf("%s volumes = %v, want [75]", key, play.volumes)
		}
	}
	play.st.Volume = 70
	play.volumes = nil
	_, cmd = press(m, "-", "-")
	if cmd == nil {
		t.Fatal("- must adjust volume")
	}
	_ = invoke(t, cmd)
	if len(play.volumes) != 1 || play.volumes[0] != 65 {
		t.Fatalf("- volumes = %v, want [65]", play.volumes)
	}

	// clamp never crosses 0..100 (the panel steps from its own state)
	next, _ := m.Update(statusMsg{st: player.State{Playing: true, Volume: 98}})
	m = next.(Model)
	play.volumes = nil
	_, cmd = press(m, "+", "+")
	if cmd == nil {
		t.Fatal("volume step must produce a command")
	}
	_ = invoke(t, cmd)
	if play.volumes[len(play.volumes)-1] != 100 {
		t.Fatalf("clamp failed: %v", play.volumes)
	}
}

func TestQuitKeys(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	for _, key := range []struct{ name, text string }{
		{"q", "q"},
		{"esc", ""},
	} {
		if _, cmd := press(m, key.name, key.text); cmd == nil {
			t.Errorf("%s must quit", key.name)
		}
	}
	_, cmd := press(m, "q", "q")
	msg := invoke(t, cmd)
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("quit command must yield QuitMsg, got %T", msg)
	}
}

func TestPlazaTitleBeatsGenericMpvTitle(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Title: "mp3"}}
	m := New(play, &fakeMeta{})
	next, _ := m.Update(statusMsg{st: play.st})
	m = next.(Model) // parity: the status poll delivers the mpv title

	if got := m.displayTrack(); got != "mp3" {
		t.Fatalf("before plaza data the mpv title shows, got %q", got)
	}

	// Panel behavior: the now-playing line comes from the metadata one-shot
	// ("track"/"artist"), so generic mp3 titles lose.
	m.applyMeta(&plaza.Metadata{Track: "Night Out In Russia", Artist: "NATIONAL"})
	if got := m.displayTrack(); got != "Night Out In Russia" {
		t.Fatalf("plaza track must win, got %q", got)
	}
	if got := m.displayArtist(); got != "NATIONAL" {
		t.Fatalf("artist line = %q", got)
	}

	// album appended only when different from the track (panel parity)
	m.applyMeta(&plaza.Metadata{Track: "same", Artist: "A", Album: "same"})
	if got := m.displayArtist(); got != "A" {
		t.Fatalf("album equal to track must not append, got %q", got)
	}
	m.applyMeta(&plaza.Metadata{Track: "t", Artist: "A", Album: "al"})
	if got := m.displayArtist(); got != "A · al" {
		t.Fatalf("album append = %q, want %q", got, "A · al")
	}
}

func TestMetaFailureRetainsLastKnownGood(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	m.applyMeta(&plaza.Metadata{Track: "kept", Artist: "kept artist"})

	next, _ := m.Update(metaMsg{err: errors.New("HTTP Error 503: Service Unavailable")})
	m = next.(Model)
	if m.metaErr == "" {
		t.Fatal("meta failure must surface in the status area")
	}
	if m.plazaData == nil || m.plazaData.Track != "kept" {
		t.Fatalf("plaza failure must keep last-known metadata, got %+v", m.plazaData)
	}

	// a successful later fetch replaces it
	fresh := &plaza.Metadata{Track: "fresh"}
	next, _ = m.Update(metaMsg{data: fresh})
	if next.(Model).plazaData != fresh {
		t.Fatal("successful fetch must replace retained metadata")
	}
}

func TestActionFailureSurfacesAndStatusPollClears(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	next, _ := m.Update(actionMsg{err: errors.New("no response from mpv")})
	if next.(Model).liveErr != "no response from mpv" {
		t.Fatalf("liveErr = %q", next.(Model).liveErr)
	}
	next, _ = next.Update(statusMsg{st: player.State{Playing: true, Volume: 70, Title: "mp3"}})
	m = next.(Model)
	if m.liveErr != "" {
		t.Fatalf("a good status poll must clear the error, got %q", m.liveErr)
	}
	if m.st.Title != "mp3" {
		t.Fatalf("status must fold in, got %+v", m.st)
	}
}

func TestStopActionComposesStopThenStatus(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Volume: 70}}
	m := New(play, &fakeMeta{})
	_, cmd := press(m, "s", "s")
	msg := invoke(t, cmd)
	am, ok := msg.(actionMsg)
	if !ok {
		t.Fatalf("stop must return an actionMsg, got %T", msg)
	}
	if am.err != nil {
		t.Fatalf("unexpected stop error: %v", am.err)
	}
	if play.stopN != 1 {
		t.Fatalf("stop calls = %d", play.stopN)
	}
}

func TestProgressExtrapolationParity(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Volume: 70}}
	m := New(play, &fakeMeta{})
	next, _ := m.Update(statusMsg{st: play.st})
	m = next.(Model) // parity: playing/unpaused arrives from the status poll

	// sync at 10/120 → two seconds later renders 12
	m.applyMeta(&plaza.Metadata{Track: "x", Length: intPtr(120), Position: intPtr(10)})
	if m.posNow != 10 || m.posLen != 120 {
		t.Fatalf("applyMeta = %d/%d, want 10/120", m.posNow, m.posLen)
	}
	m.trackProgress(m.posStamp.Add(2 * time.Second))
	if m.posNow != 12 {
		t.Fatalf("extrapolated = %d, want 12", m.posNow)
	}

	// clamp at length
	m.trackProgress(m.posStamp.Add(1000 * time.Second))
	if m.posNow != 120 {
		t.Fatalf("clamp = %d, want 120", m.posNow)
	}

	// pauses park the progression (panel parity) — sync a fresh 10/120
	m.applyMeta(&plaza.Metadata{Track: "x", Length: intPtr(120), Position: intPtr(10)})
	paused := play
	paused.st.Paused = true
	next, _ = m.Update(statusMsg{st: paused.st})
	m = next.(Model)
	m.trackProgress(m.posStamp.Add(5 * time.Second))
	if m.posNow != 10 {
		t.Fatalf("paused extrapolation = %d, want parked 10", m.posNow)
	}

	// without a known length nothing renders
	m.applyMeta(&plaza.Metadata{Track: "x"})
	m.trackProgress(m.posStamp.Add(5 * time.Second))
	if m.posLen != 0 || m.posNow != 0 {
		t.Fatalf("no length must park progress, got %d/%d", m.posNow, m.posLen)
	}
}

func TestViewShape(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Volume: 62, Title: "mp3"}}
	m := New(play, &fakeMeta{})
	m.applyMeta(&plaza.Metadata{
		Track: "Night Out In Russia", Artist: "NATIONAL",
		Length: intPtr(137), Position: intPtr(34), Listeners: intPtr(152),
	})
	next, _ := m.Update(statusMsg{st: play.st})
	m = next.(Model)
	m.width = 80

	view := m.View()
	s := view.Content
	for _, want := range []string{
		"LIVE", "· 152 listening",
		"Night Out In Russia", "NATIONAL",
		"0:34 / 2:17", "space", "volume 62%",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("view missing %q:\n%s", want, s)
		}
	}
	if !view.AltScreen {
		t.Error("panel must render on the alternate screen")
	}

	// the generic mpv title must not leak once plaza metadata arrived
	if strings.Contains(s, "mp3") {
		t.Errorf("view leaked the generic mpv title:\n%s", s)
	}

	// idle model without data: placeholder dash, no listening suffix
	empty := New(&fakePlayback{}, &fakeMeta{}).View()
	if !strings.Contains(empty.Content, "—") {
		t.Errorf("empty view must show the placeholder dash:\n%s", empty.Content)
	}
	if strings.Contains(empty.Content, "listening") {
		t.Errorf("no listeners must omit the listening suffix:\n%s", empty.Content)
	}
}
