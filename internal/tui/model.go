// Package tui is the Nightwave Plaza panel. Interfaces are declared here
// (consumer side) so tests drive fakes — TUI tests need no mpv and no HTTP.
package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// Poll cycles, parity with the panel timers: status 3 s, Plaza metadata
// 30 s, 1 s extrapolated progress.
const (
	statusInterval = 3 * time.Second
	metaInterval   = 30 * time.Second
	progressTick   = 1 * time.Second
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

// MetaSource is the Plaza data surface the panel consumes so far.
type MetaSource interface {
	Metadata(ctx context.Context) (*plaza.Metadata, error)
}

// Model is the panel: honest badge, now-playing, transport, volume.
type Model struct {
	play Playback
	meta MetaSource

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

	width, height int
}

// New builds the model around real seams.
func New(play Playback, meta MetaSource) Model {
	return Model{
		play: play,
		meta: meta,
		st:   player.State{Volume: player.DefaultVolume},
	}
}

// --- messages -----------------------------------------------------------

type statusMsg struct {
	st  player.State
	err error
}

type metaMsg struct {
	data *plaza.Metadata
	err  error
}

// actionMsg reports the state returned by a transport action
// (toggle/stop/volume) so the panel reflects the outcome immediately.
type actionMsg struct {
	st  player.State
	err error
}

// statusTickMsg fires every 3 s → status poll.
type statusTickMsg time.Time

// metaTickMsg fires every 30 s → Plaza metadata poll.
type metaTickMsg time.Time

// progressTickMsg fires every 1 s → extrapolated progress.
type progressTickMsg time.Time

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

// --- tick makers --------------------------------------------------------

func everyStatus(t time.Time) tea.Msg { return statusTickMsg(t) }

func everyMeta(t time.Time) tea.Msg { return metaTickMsg(t) }

func everyProgress(t time.Time) tea.Msg { return progressTickMsg(t) }

// Init starts the immediate polls plus the recurring timers (parity with
// the panel's triggeredOnStart timers).
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		statusPoll(m.play),
		metaPoll(m.meta),
		tea.Every(statusInterval, everyStatus),
		tea.Every(metaInterval, everyMeta),
		tea.Every(progressTick, everyProgress),
	)
}
