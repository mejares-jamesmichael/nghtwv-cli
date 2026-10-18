package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// describe shapes an error for the status line without trailing noise.
func describe(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func clampVolume(v int) int {
	return max(0, min(v, 100))
}

// Update handles polls, transport actions, progress, and the locked key
// set: space toggle · s stop · +/− volume · esc/q quit (esc grows
// picker-close behavior when the picker lands).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case statusTickMsg:
		return m, statusPoll(m.play)

	case metaTickMsg:
		return m, metaPoll(m.meta)

	case progressTickMsg:
		m.trackProgress(time.Time(msg))
		return m, nil

	case statusMsg:
		m.liveErr = ""
		m.st = msg.st
		return m, nil

	case metaMsg:
		if msg.err != nil {
			// Keep the last-known-good metadata; the footer notes the
			// fetch failure.
			m.metaErr = describe(msg.err)
			return m, nil
		}
		if msg.data != nil {
			m.metaErr = ""
			m.applyMeta(msg.data)
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.liveErr = describe(msg.err)
			return m, nil
		}
		m.liveErr = ""
		m.st = msg.st
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "space":
			return m, doAction(m.play.Toggle)
		case "s":
			return m, stopAction(m.play)
		case "+", "=":
			// Panel parity: plus and equal both step up 5.
			return m, doAction(func(ctx context.Context) (player.State, error) {
				return m.play.Volume(ctx, clampVolume(m.st.Volume+5))
			})
		case "-":
			return m, doAction(func(ctx context.Context) (player.State, error) {
				return m.play.Volume(ctx, clampVolume(m.st.Volume-5))
			})
		case "esc", "q", "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

// applyMeta folds a fresh Plaza payload in and re-stamps the progress
// extrapolation (parity with the panel's metadata handler).
func (m *Model) applyMeta(data *plaza.Metadata) {
	m.plazaData = data
	m.posStamp = time.Now()
	m.posLen = 0
	if data.Length != nil {
		m.posLen = *data.Length
	}
	m.posSync = 0
	if data.Position != nil {
		m.posSync = *data.Position
	}
	m.posNow = m.posSync
}

// trackProgress advances rendered progress (panel tickTimer semantics).
func (m *Model) trackProgress(now time.Time) {
	if !m.st.Playing || m.st.Paused || m.posLen <= 0 || m.posStamp.IsZero() {
		return
	}
	elapsed := int(now.Sub(m.posStamp).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	m.posNow = min(m.posSync+elapsed, m.posLen)
}
