// Package tui is the Nightwave Plaza panel. Interfaces are declared here
// (consumer side) so tests drive fakes — TUI tests need no mpv and no HTTP.
package tui

import (
	"context"
	"image"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// PickerOpen reports whether the output picker overlay is open.
func (m Model) PickerOpen() bool { return m.pickerOpen }

// Devices lists the cached audio outputs for the picker.
func (m Model) Devices() []mpv.Device { return m.devices }

// OutputsLoaded reports whether the last open finished fetching the list.
func (m Model) OutputsLoaded() bool { return m.outputsLoaded }

// OutputsErr is the last outputs-fetch failure (empty when none).
func (m Model) OutputsErr() string { return m.outputsErr }

// HelpOpen reports whether the help overlay is open.
func (m Model) HelpOpen() bool { return m.helpOpen }

// Poll cycles, parity with the panel timers: status 3 s, Plaza metadata
// 30 s, history 60 s, 1 s extrapolated progress, 100 ms decorative meter.
const (
	statusInterval  = 3 * time.Second
	metaInterval    = 30 * time.Second
	historyInterval = 60 * time.Second
	progressTick    = 1 * time.Second
	meterTick       = 100 * time.Millisecond
)

// Playback is the minimal player surface the panel drives. It is exactly
// the *player.Player method set (declared here on the consumer side so
// tests drive a fake; no mpv, no HTTP).
type Playback interface {
	Status(ctx context.Context) player.State
	Toggle(ctx context.Context) (player.State, error)
	Stop(ctx context.Context) error
	Volume(ctx context.Context, volume int) (player.State, error)
	SetOutput(ctx context.Context, device string) (player.State, error)
	Outputs(ctx context.Context) ([]mpv.Device, error)
}

// MetaSource is the Plaza data surface the panel consumes (history shares
// the one-shot's already-cleaned entries).
type MetaSource interface {
	Metadata(ctx context.Context) (*plaza.Metadata, error)
	History(ctx context.Context) (*plaza.History, error)
}

// ImageSource decodes artwork URLs so the panel can paint them as
// half-block cells; tests inject decoders without HTTP.
type ImageSource interface {
	Fetch(ctx context.Context, url string) (image.Image, error)
}

// Model is the panel: honest badge, now-playing, transport, volume,
// history, output picker, artwork, decorative meter, help.
type Model struct {
	play   Playback
	meta   MetaSource
	images ImageSource

	// Last known playback state. Kept across failed polls (panel parity:
	// the widget never blanks on a failed refresh).
	st      player.State
	liveErr string

	// Last-known-good Plaza metadata. Plaza failures keep showing the
	// previous track ("Keep the last known track when a fetch fails").
	plazaData *plaza.Metadata
	metaErr   string

	// Progress extrapolation (panel tickTimer semantics): posNow =
	// min(posSync + (now - posStamp), posLen) while playing and unpaused,
	// parked at posSync otherwise.
	posStamp time.Time
	posSync  int
	posNow   int
	posLen   int

	// Recent transmissions (panel: sliced to 10, HH:mm local stamp).
	history []plaza.HistoryEntry
	histErr string

	// Output picker: opened with o, rows marked ● for the current output,
	// ● current / ○ others with the "System default" first row.
	pickerOpen    bool
	devices       []mpv.Device
	pickerCursor  int
	outputsErr    string
	outputsLoaded bool

	// Artwork box: art rendered per URL once (cache), note placeholder
	// without artwork or on fetch/render failure or non-truecolor
	// profiles.
	artwork       string
	artworkURL    string
	artworkFailed bool
	truecolor     bool

	// Decorative meter: a free-running phase, advanced only while playing
	// (gated on real playback); levels[] is the future data source seam.
	wavePhase float64

	// Help overlay toggles with ?.
	helpOpen bool

	width, height int
}

// New builds the model around real seams with the default artwork pipeline.
func New(play Playback, meta MetaSource) Model {
	return Model{
		play:   play,
		meta:   meta,
		images: newImageSource(),
		st:     player.State{Volume: player.DefaultVolume},
	}
}

// --- messages -----------------------------------------------------------

type statusMsg struct {
	st player.State
}

type metaMsg struct {
	data *plaza.Metadata
	err  error
}

type historyMsg struct {
	data *plaza.History
	err  error
}

type outputsMsg struct {
	devices []mpv.Device
	err     error
}

// actionMsg reports the state returned by a transport action
// (toggle/stop/volume/output) so the panel reflects the outcome
// immediately.
type actionMsg struct {
	st  player.State
	err error
}

// artworkMsg carries a decoded artwork render (empty on failure — the
// panel falls back to the music-note placeholder).
type artworkMsg struct {
	url string
	art string
}

// statusTickMsg fires every 3 s → status poll.
type statusTickMsg time.Time

// metaTickMsg fires every 30 s → Plaza metadata poll.
type metaTickMsg time.Time

// historyTickMsg fires every 60 s → Plaza history poll.
type historyTickMsg time.Time

// progressTickMsg fires every 1 s → extrapolated progress.
type progressTickMsg time.Time

// meterTickMsg fires every 100 ms → decorative meter advance.
type meterTickMsg struct{}

func statusPoll(p Playback) tea.Cmd {
	// Status never errors (player parity: failures collapse to the
	// neutral state), so the message carries no error.
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		return statusMsg{st: p.Status(ctx)}
	}
}

func metaPoll(s MetaSource) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		data, err := s.Metadata(ctx)
		return metaMsg{data: data, err: err}
	}
}

func historyPoll(s MetaSource) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		data, err := s.History(ctx)
		return historyMsg{data: data, err: err}
	}
}

// outputsPoll lists devices for the picker (through the player so an idle
// mpv is spawned on demand).
func outputsPoll(p Playback) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		devices, err := p.Outputs(ctx)
		return outputsMsg{devices: devices, err: err}
	}
}

// doAction runs a transport action and wraps its returned state for an
// immediate panel refresh.
func doAction(fn func(ctx context.Context) (player.State, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		st, err := fn(ctx)
		return actionMsg{st: st, err: err}
	}
}

// stopAction runs Stop (error-only signature) and then reads fresh state.
func stopAction(p Playback) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := p.Stop(ctx); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{st: p.Status(ctx)}
	}
}

// fetchArtwork decodes an image URL and renders the half-block art once.
func fetchArtwork(src ImageSource, url string) tea.Cmd {
	if url == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		img, err := src.Fetch(ctx, url)
		if err != nil {
			return artworkMsg{url: url}
		}
		return artworkMsg{url: url, art: renderHalfBlocks(img, artworkCols, artworkRows)}
	}
}

// --- tick makers --------------------------------------------------------

func everyStatus(t time.Time) tea.Msg { return statusTickMsg(t) }

func everyMeta(t time.Time) tea.Msg { return metaTickMsg(t) }

func everyHistory(t time.Time) tea.Msg { return historyTickMsg(t) }

func everyProgress(t time.Time) tea.Msg { return progressTickMsg(t) }

func everyMeter(time.Time) tea.Msg { return meterTickMsg{} }

// Init starts the immediate polls plus the recurring timers (parity with
// the panel's triggeredOnStart timers).
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		statusPoll(m.play),
		metaPoll(m.meta),
		historyPoll(m.meta),
		tea.Every(statusInterval, everyStatus),
		tea.Every(metaInterval, everyMeta),
		tea.Every(historyInterval, everyHistory),
		tea.Every(progressTick, everyProgress),
		tea.Every(meterTick, everyMeter),
	)
}
