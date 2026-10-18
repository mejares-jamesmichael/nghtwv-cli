package tui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// fold updates the model with a message, restoring the concrete type.
func fold(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

// foldCmd updates and also keeps the returned command.
func foldCmd(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// fakeImages records fetches (artwork tests: no HTTP).
type fakeImages struct {
	urls []string
	img  image.Image
	err  error
}

func (f *fakeImages) Fetch(_ context.Context, url string) (image.Image, error) {
	f.urls = append(f.urls, url)
	return f.img, f.err
}

// flatImage paints a 4×4 grid (red top half, blue bottom) so half-block
// rendering is deterministic.
func flatImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if y < 2 {
				img.Set(x, y, color.RGBA{R: 0xff, A: 0xff})
			} else {
				img.Set(x, y, color.RGBA{B: 0xff, A: 0xff})
			}
		}
	}
	return img
}

func TestOutputPickerLifecycle(t *testing.T) {
	play := &fakePlayback{
		outputs: []mpv.Device{
			{Name: "pipewire/speakers", Description: "Speakers"},
			{Name: "alsa/headphones"}, // no description → label falls back
		},
	}
	m := New(play, &fakeMeta{})
	// A status poll delivers the honest current output ("" = System default).
	m = fold(m, statusMsg{st: player.State{Playing: true, Volume: 70}})

	// o opens the picker and fetches outputs on demand.
	m, cmd := press(m, "o", "")
	if !m.pickerOpen {
		t.Fatal("o must open the picker")
	}
	msg := invoke(t, cmd)
	om, ok := msg.(outputsMsg)
	if !ok {
		t.Fatalf("o must trigger an outputs fetch, got %T", msg)
	}
	m = fold(m, om)
	if len(m.devices) != 2 || m.outputsErr != "" {
		t.Fatalf("outputs fold = %+v err=%q", m.devices, m.outputsErr)
	}

	// Rows: System default first; ● marks the current output; ▸ the cursor;
	// description-less devices fall back to their name.
	view := m.View().Content
	for _, want := range []string{"● System default", "○ Speakers", "○ alsa/headphones", "▸"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker view missing %q:\n%s", want, stripANSI(view))
		}
	}

	// enter selects row 0 → SetOutput("") (keep following the default).
	m, cmd = press(m, "enter", "")
	if m.pickerOpen {
		t.Fatal("enter closes the picker")
	}
	_ = invoke(t, cmd)
	if len(play.outputNames) != 1 || play.outputNames[0] != "" {
		t.Fatalf("row 0 must clear the output, got %v", play.outputNames)
	}

	// Reopen, move down twice, enter → selects devices[1].
	m, cmd = press(m, "o", "")
	_ = invoke(t, cmd)
	m = fold(m, outputsMsg{devices: play.outputs})
	m, _ = press(m, "down", "j")
	m, _ = press(m, "down", "j")
	if m.pickerCursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.pickerCursor)
	}
	m, cmd = press(m, "enter", "")
	_ = invoke(t, cmd)
	if play.outputNames[len(play.outputNames)-1] != "alsa/headphones" {
		t.Fatalf("row 2 must select devices[1], got %v", play.outputNames)
	}

	// SetOutput("") folds back into the panel (actionMsg winner).
	m = fold(m, actionMsg{st: player.State{Playing: true, Volume: 70, Output: "alsa/headphones"}})
	if got := m.outputLabel(); got != "alsa/headphones" {
		t.Fatalf("output label = %q, want alsa/headphones", got)
	}

	// esc closes the picker instead of quitting (locked precedence).
	m, cmd = press(m, "o", "")
	_ = invoke(t, cmd)
	m = fold(m, outputsMsg{devices: play.outputs})
	var next tea.Model
	next, cmd = m.Update(keyByName("esc"))
	m = next.(Model)
	if m.pickerOpen {
		t.Fatal("esc closes the picker")
	}
	if cmd != nil {
		t.Fatal("esc with the picker open must not schedule anything (let alone quit)")
	}
}

// keyByName is press without the fold (for cases needing the raw cmd).
func keyByName(name string) tea.KeyPressMsg {
	switch name {
	case "space":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeySpace})
	case "esc":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})
	case "enter":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	default:
		return tea.KeyPressMsg(tea.Key{Code: rune(name[0]), Text: name})
	}
}

func TestPickerStatesParity(t *testing.T) {
	// Error from the outputs one-shot surfaces under the rows (urgent).
	play := &fakePlayback{err: errors.New("audio outputs are unavailable: no response from mpv")}
	m := New(play, &fakeMeta{})
	m, cmd := press(m, "o", "")
	m = fold(m, invoke(t, cmd))
	if m.outputsErr != "audio outputs are unavailable: no response from mpv" {
		t.Fatalf("outputs error = %q", m.outputsErr)
	}
	if got := m.View().Content; !strings.Contains(got, "outputs: audio outputs are unavailable") {
		t.Errorf("picker must surface the error: %s", stripANSI(got))
	}

	// Successful but empty → "no extra outputs found" (panel parity).
	empty := New(&fakePlayback{}, &fakeMeta{})
	empty, cmd = press(empty, "o", "")
	empty = fold(empty, invoke(t, cmd))
	if got := empty.View().Content; !strings.Contains(got, "no extra outputs found") {
		t.Errorf("empty list must say so: %s", stripANSI(got))
	}

	// Reopening (after closing) shows the async scanning state until the
	// fetch returns.
	empty, _ = press(empty, "esc", "")
	empty, _ = press(empty, "o", "")
	if got := empty.View().Content; !strings.Contains(got, "scanning outputs…") {
		t.Errorf("unloaded picker must show scanning state: %s", stripANSI(got))
	}
}

func TestPickerWindowFollowsCursor(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	m.height = 24 // 5 rows for the lower block: border, status, two list rows

	devices := make([]mpv.Device, 30)
	for i := range devices {
		devices[i] = mpv.Device{Name: fmt.Sprintf("sink-%02d", i), Description: fmt.Sprintf("Device %02d", i)}
	}
	m = fold(m, outputsMsg{devices: devices})
	m, _ = press(m, "o", "")
	for range devices {
		m, _ = press(m, "down", "")
	}
	if m.pickerCursor != len(devices) {
		t.Fatalf("cursor = %d, want %d", m.pickerCursor, len(devices))
	}

	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "Device 29") {
		t.Fatalf("cursor row is off-window:\n%s", view)
	}
	if strings.Contains(view, "Device 00") {
		t.Fatalf("window must follow the cursor, not start at the top:\n%s", view)
	}
	if rows := strings.Count(view, "\n") + 1; rows > 24 {
		t.Fatalf("frame is %d rows on a 24-row terminal:\n%s", rows, view)
	}

	// Walk back to the top: the first row must be visible again.
	for range devices {
		m, _ = press(m, "up", "")
	}
	view = stripANSI(m.View().Content)
	if !strings.Contains(view, "▸ ● System default") {
		t.Fatalf("first row not visible after scrolling back:\n%s", view)
	}
}

func TestRightColumnCappedAtPanelHeight(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	m.height = 24
	m.width = 80
	m.st = player.State{Playing: true, Volume: 70}
	m.plazaData = &plaza.Metadata{
		Track:  strings.Repeat("very long track title ", 12),
		Artist: strings.Repeat("very long artist name ", 12),
	}
	m.liveErr = strings.Repeat("mpv exploded ", 20)
	m.metaErr = strings.Repeat("plaza exploded ", 20)

	view := stripANSI(m.View().Content)
	if rows := strings.Count(view, "\n") + 1; rows > 24 {
		t.Fatalf("frame is %d rows on a 24-row terminal:\n%s", rows, view)
	}
	if !strings.Contains(view, "volume 70%") {
		t.Fatalf("footer must survive the wrapped deck:\n%s", view)
	}
}

func TestHelpToggle(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	m.height = 30 // realistic terminal; lowerBlockBudget needs this to show all rows
	m, _ = press(m, "?", "?")
	if !m.helpOpen {
		t.Fatal("? must open help")
	}
	if got := m.View().Content; !strings.Contains(got, "close picker, then quit") {
		t.Errorf("help must document the esc precedence: %s", stripANSI(got))
	}
	m, _ = press(m, "esc", "")
	if m.helpOpen {
		t.Fatal("esc closes help")
	}
	// esc with nothing open quits.
	_, cmd := press(m, "esc", "")
	msg := invoke(t, cmd)
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatal("esc with no overlay must quit")
	}
}

func TestHistoryFoldAndRows(t *testing.T) {
	m := New(&fakePlayback{}, &fakeMeta{})
	stamp := time.Date(2026, 10, 18, 3, 42, 0, 0, time.Local)
	epoch := int(stamp.Unix())

	m = fold(m, historyMsg{data: &plaza.History{Songs: []plaza.HistoryEntry{
		{Artist: "Finlii", Track: "Wonder", PlayedAt: &epoch},
		{Artist: "solo only"},
		{Track: "track only"},
		{}, // neither side: still listed on its own line (panel: em dash)
	}}})
	if len(m.history) != 4 {
		t.Fatalf("history = %d entries, want 4", len(m.history))
	}
	view := m.View().Content
	for _, want := range []string{
		"RECENT TRANSMISSIONS",
		"Finlii — Wonder",
		"solo only",
		"track only",
		"3:42", // HH:mm local stamp
	} {
		if !strings.Contains(view, want) {
			t.Errorf("history view missing %q:\n%s", want, stripANSI(view))
		}
	}

	// Failures keep the last list (panel parity).
	m = fold(m, historyMsg{err: errors.New("HTTP Error 503: Service Unavailable")})
	if m.histErr == "" || len(m.history) != 4 {
		t.Fatalf("history failure must retain entries: err=%q n=%d", m.histErr, len(m.history))
	}
}

func TestMeterGatedOnPlayback(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Volume: 70}}
	m := New(play, &fakeMeta{})
	m = fold(m, statusMsg{st: play.st})

	m = fold(m, meterTickMsg{})
	if m.wavePhase == 0 {
		t.Fatal("live playback must advance the meter phase")
	}
	phase := m.wavePhase

	// Paused freezes the band (parked on the midline, panel parity).
	m = fold(m, statusMsg{st: player.State{Playing: true, Paused: true, Volume: 70}})
	m = fold(m, meterTickMsg{})
	if m.wavePhase != phase {
		t.Fatal("paused playback must freeze the meter")
	}

	// Idle freezes it too.
	m = fold(m, statusMsg{st: player.State{}})
	m = fold(m, meterTickMsg{})
	if m.wavePhase != phase {
		t.Fatal("idle playback must freeze the meter")
	}

	// The off-air band renders the settled midline.
	m = fold(m, statusMsg{st: player.State{}})
	if got := m.View().Content; !strings.Contains(got, "▁") {
		t.Errorf("settled meter must render the faint midline: %s", stripANSI(got))
	}
}

func TestTruecolorGatesArtwork(t *testing.T) {
	play := &fakePlayback{st: player.State{Playing: true, Volume: 70}}
	images := &fakeImages{img: flatImage()}
	m := New(play, &fakeMeta{})
	m.images = images

	// Profile messages gate the half-block art (locked behavior).
	m = fold(m, tea.ColorProfileMsg{Profile: colorprofile.ANSI})
	if m.truecolor {
		t.Fatal("ANSI profile must disable truecolor")
	}
	m = fold(m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	if !m.truecolor {
		t.Fatal("truecolor profile must allow half-block art")
	}

	// New artwork URL → one fetch; repeat URL → none.
	m, cmd := foldCmd(m, metaMsg{data: &plaza.Metadata{
		Track: "x", Artwork: "https://i.plaza.one/art.jpg",
		Length: intPtr(200), Position: intPtr(5),
	}})
	if cmd == nil {
		t.Fatal("new artwork URL must enqueue a fetch")
	}
	msg := invoke(t, cmd)
	if am := msg.(artworkMsg); am.art == "" {
		t.Fatal("decoding flatImage must render non-empty cells")
	}
	m = fold(m, msg)
	if len(images.urls) != 1 || m.artwork == "" {
		t.Fatalf("fetches = %v, cached art = %q", images.urls, m.artwork)
	}
	if _, cmd = foldCmd(m, metaMsg{data: m.plazaData}); cmd != nil {
		t.Fatal("same artwork URL must not refetch")
	}

	// View paints the art on truecolor, the note placeholder otherwise.
	if got := m.View().Content; !strings.Contains(got, "▀") {
		t.Errorf("truecolor view must show half-block art: %s", stripANSI(got))
	}
	m.truecolor = false
	if got := m.View().Content; strings.Contains(got, "▀") || !strings.Contains(got, "♪") {
		t.Errorf("non-truecolor view must show the note placeholder: %s", stripANSI(got))
	}
	m.truecolor = true

	// Fetch failure keeps the placeholder (panel artworkFailed parity).
	fail := New(play, &fakeMeta{})
	fail.images = &fakeImages{err: errors.New("boom")}
	fail.truecolor = true
	fail, cmd = foldCmd(fail, metaMsg{data: &plaza.Metadata{Track: "x", Artwork: "https://x/y.jpg"}})
	_ = invoke(t, cmd) // artworkMsg with empty art
	fail = fold(fail, artworkMsg{url: "https://x/y.jpg"})
	if !fail.artworkFailed || fail.artwork != "" {
		t.Fatalf("failure must flag placeholder, got failed=%v art=%q", fail.artworkFailed, fail.artwork)
	}

	// Responses for URLs the panel has moved past are dropped.
	stale := New(play, &fakeMeta{})
	stale.truecolor = true
	stale.artworkURL = "old"
	stale = fold(stale, artworkMsg{url: "new", art: "fakeart"})
	if stale.artwork != "" {
		t.Fatal("artwork responses for stale URLs must be dropped")
	}
}

// stripANSI removes escape sequences so assertion failures stay readable.
func stripANSI(s string) string {
	b := &strings.Builder{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b {
			switch {
			case i+1 < len(s) && s[i+1] == '[':
				// CSI: runs until a final byte in 0x40..0x7e (ECMA-48).
				i += 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			case i+1 < len(s) && s[i+1] == ']':
				// OSC: runs until BEL or ST (ESC \).
				i += 2
				for i < len(s) {
					if s[i] == 0x07 || s[i] == 0x9c {
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i++
						break
					}
					i++
				}
			}
			continue
		}
		if c != '\r' {
			b.WriteByte(c)
		}
	}
	return b.String()
}
