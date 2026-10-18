package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

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

// Update handles polls, transport actions, progress, the picker, help,
// artwork, the decorative meter, and the locked key set: space toggle ·
// s stop · +/− volume · o output picker (● current) · ? help · esc closes
// the picker first, then quits · q quit.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.ColorProfileMsg:
		// Artwork renders as half-block cells only on truecolor profiles
		// (locked behavior: music-note placeholder otherwise).
		m.truecolor = msg.Profile >= colorprofile.TrueColor
		return m, nil

	case statusTickMsg:
		return m, tea.Batch(statusPoll(m.play), tea.Every(statusInterval, everyStatus))

	case metaTickMsg:
		return m, tea.Batch(metaPoll(m.meta), tea.Every(metaInterval, everyMeta))

	case historyTickMsg:
		return m, tea.Batch(historyPoll(m.meta), tea.Every(historyInterval, everyHistory))

	case progressTickMsg:
		m.trackProgress(time.Time(msg))
		return m, tea.Every(progressTick, everyProgress)

	case meterTickMsg:
		// Gated on real playback: paused/idle keeps the band settled.
		if m.st.Playing && !m.st.Paused {
			m.wavePhase += 0.35
		}
		return m, tea.Every(meterTick, everyMeter)

	case statusMsg:
		m.liveErr = ""
		m.st = msg.st
		return m, nil

	case metaMsg:
		if msg.err != nil {
			// Keep the last-known-good metadata; the footer notes the
			// fetch failure.
			m.metaErr = describe(msg.err)
		} else if msg.data != nil {
			m.metaErr = ""
			m.applyMeta(msg.data)
		}
		return m, m.refreshArtwork()

	case historyMsg:
		if msg.err != nil {
			m.histErr = describe(msg.err)
			return m, nil
		}
		m.histErr = ""
		if msg.data != nil {
			// The History one-shot already applied the 10-entry cap and
			// cleaned the text; keep plain struct copies.
			m.history = msg.data.Songs
		}
		return m, nil

	case outputsMsg:
		m.outputsLoaded = true
		if msg.err != nil {
			m.outputsErr = describe(msg.err)
		} else {
			m.outputsErr = ""
			m.devices = msg.devices
			// Rows: System default + one per device.
			if m.pickerCursor > len(m.devices) {
				m.pickerCursor = len(m.devices)
			}
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

	case artworkMsg:
		if msg.url != m.artworkURL {
			return m, nil // stale response
		}
		if msg.art == "" {
			m.artworkFailed = true
			return m, nil
		}
		m.artworkFailed = false
		m.artwork = msg.art
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
		case "o":
			// Output picker: open (fetching on demand) or close.
			if m.pickerOpen {
				m.pickerOpen = false
				return m, nil
			}
			m.pickerOpen = true
			m.outputsLoaded = false
			return m, outputsPoll(m.play)
		case "?":
			m.helpOpen = !m.helpOpen
			return m, nil
		case "enter":
			if m.pickerOpen {
				// Row 0 is System default (""); row r maps to devices[r-1].
				device := ""
				if m.pickerCursor > 0 && m.pickerCursor <= len(m.devices) {
					device = m.devices[m.pickerCursor-1].Name
				}
				m.pickerOpen = false
				return m, doAction(func(ctx context.Context) (player.State, error) {
					return m.play.SetOutput(ctx, device)
				})
			}
			return m, nil
		case "up", "k":
			if m.pickerOpen && m.pickerCursor > 0 {
				m.pickerCursor--
			}
			return m, nil
		case "down", "j":
			if m.pickerOpen {
				m.pickerCursor = min(m.pickerCursor+1, len(m.devices)) // +1 rows: system default first
			}
			return m, nil
		case "esc":
			switch {
			case m.pickerOpen:
				m.pickerOpen = false
			case m.helpOpen:
				m.helpOpen = false
			default:
				return m, tea.Quit
			}
			return m, nil
		case "q", "ctrl+c":
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

// refreshArtwork queues a half-block render when the artwork URL changed.
func (m *Model) refreshArtwork() tea.Cmd {
	if m.plazaData == nil || m.plazaData.Artwork == "" {
		return nil
	}
	if m.plazaData.Artwork == m.artworkURL {
		return nil
	}
	m.artworkURL = m.plazaData.Artwork
	return fetchArtwork(m.images, m.artworkURL)
}
