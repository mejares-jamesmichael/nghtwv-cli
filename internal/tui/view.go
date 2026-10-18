package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// Palette — a fixed dark synthwave set (the CLI runs outside Omarchy too,
// so the panel colors come with the app instead of the desktop theme).
var (
	colorBg    = lipgloss.Color("#141021")
	colorFg    = lipgloss.Color("#e8e4f0")
	colorDim   = lipgloss.Color("#8b85a0")
	colorFaint = lipgloss.Color("#5a5470")
	colorHot   = lipgloss.Color("#ff2e97") // LIVE dot
	colorCool  = lipgloss.Color("#4fc3f7") // BUFFERING
	colorWarm  = lipgloss.Color("#ffc542") // PAUSED + errors
	colorBar   = lipgloss.Color("#7c5cff") // progress line
)

var (
	baseStyle  = lipgloss.NewStyle().Foreground(colorFg).Background(colorBg)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorFg).Background(colorBg)
	dimStyle   = lipgloss.NewStyle().Foreground(colorDim).Background(colorBg)
	hotStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorHot).Background(colorBg)
	warmStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorWarm).Background(colorBg)
	coolStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorCool).Background(colorBg)
	idleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorDim).Background(colorBg)
	errStyle   = lipgloss.NewStyle().Foreground(colorWarm).Background(colorBg)
	footKey    = lipgloss.NewStyle().Bold(true).Foreground(colorFg).Background(colorBg)
	footHint   = lipgloss.NewStyle().Foreground(colorFaint).Background(colorBg)
	barStyle   = lipgloss.NewStyle().Foreground(colorBar).Background(colorBg)
)

// badge is the honest state word, parity with the panel:
// playing ? (buffering ? BUFFERING : paused ? PAUSED : LIVE) : IDLE.
func badge(st player.State) (string, lipgloss.Style) {
	switch {
	case !st.Playing:
		return "IDLE", idleStyle
	case st.Buffering:
		return "BUFFERING", coolStyle
	case st.Paused:
		return "PAUSED", warmStyle
	default:
		return "LIVE", hotStyle
	}
}

// displayTrack is the now-playing line. Plaza track wins; mpv's generic
// stream titles (e.g. "mp3") lose to Plaza metadata when available
// (locked behavior); "—" fills when nothing is known.
func (m Model) displayTrack() string {
	if m.plazaData != nil && m.plazaData.Track != "" {
		return m.plazaData.Track
	}
	if m.st.Title != "" {
		return m.st.Title
	}
	return "—"
}

// displayArtist mirrors the panel: artist, album appended when present
// and different from the track.
func (m Model) displayArtist() string {
	if m.plazaData == nil {
		return ""
	}
	artist := m.plazaData.Artist
	if artist == "" {
		return ""
	}
	if album := m.plazaData.Album; album != "" && album != m.plazaData.Track {
		return artist + " · " + album
	}
	return artist
}

func seconds(total int) string {
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// trackWidth returns the render width (0 = not yet measured).
func (m Model) renderWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 60
}

// progressBar is a thin textual progress line with a fraction dot,
// mirroring the panel's progress ratio (the decorative wave meter is the
// phase 5 rework).
func (m Model) progressBar() string {
	if m.posLen <= 0 {
		return ""
	}
	width := max(24, m.renderWidth()/3)
	filled := max(0, min(width-1, m.posNow*width/m.posLen))
	line := strings.Repeat("━", filled) + "●" + strings.Repeat("━", max(0, width-1-filled))
	return barStyle.Render(line) + dimStyle.Render(" "+seconds(m.posNow)+" / "+seconds(m.posLen))
}

// View renders the altscreen panel.
func (m Model) View() tea.View {
	var b strings.Builder

	// badge row: dot + state + listener count (panel text parity).
	state, style := badge(m.st)
	dot := dimStyle.Render("●")
	if m.st.Playing && !m.st.Paused {
		dot = hotStyle.Render("●")
	}
	head := dot + " " + style.Render(state)
	if m.plazaData != nil && m.plazaData.Listeners != nil {
		head += dimStyle.Render(fmt.Sprintf(" · %d listening", *m.plazaData.Listeners))
	}
	b.WriteString(head + "\n\n")

	// now playing
	track := wrapToWidth(m.displayTrack(), max(20, m.renderWidth()-2))
	b.WriteString(titleStyle.Render(track) + "\n")
	if artist := wrapToWidth(m.displayArtist(), max(20, m.renderWidth()-2)); artist != "" {
		b.WriteString(dimStyle.Render(artist) + "\n")
	}

	b.WriteString("\n")

	// progress (only when duration is known)
	if bar := m.progressBar(); bar != "" {
		b.WriteString(bar + "\n\n")
	}

	// honest error line, never blanking the panel
	var errs []string
	if m.liveErr != "" {
		errs = append(errs, "mpv: "+m.liveErr)
	}
	if m.metaErr != "" {
		errs = append(errs, "plaza: "+m.metaErr)
	}
	for _, e := range errs {
		b.WriteString(errStyle.Render(wrapToWidth(e, max(20, m.renderWidth()-2))) + "\n")
	}
	b.WriteString("\n")

	// transport footer (picker + help grow here in phase 5)
	hints := []string{
		footKey.Render("space") + footHint.Render(" pause/play"),
		footKey.Render("s") + footHint.Render(" stop"),
		footKey.Render("+/-") + footHint.Render(" volume"),
		footKey.Render("q") + footHint.Render(" quit"),
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("volume %d%%", m.st.Volume)) +
		"  " + strings.Join(hints, footHint.Render(" · ")))

	v := tea.NewView(baseStyle.Render(b.String()))
	v.AltScreen = true
	v.BackgroundColor = colorBg
	return v
}

// wrapToWidth hard-wraps a single line to w columns (the panel wraps its
// title to two lines; plain wrap until the phase 5 layout pass).
func wrapToWidth(s string, w int) string {
	if w <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= w {
		return s
	}
	var lines []string
	for len(runes) > w {
		lines = append(lines, string(runes[:w]))
		runes = runes[w:]
	}
	lines = append(lines, string(runes))
	return strings.Join(lines, "\n")
}
