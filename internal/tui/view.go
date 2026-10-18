package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// mathSin keeps the meter formula short.
func mathSin(x float64) float64 { return math.Sin(x) }

// Palette — a fixed dark synthwave set (the CLI runs outside Omarchy too,
// so the panel colors come with the app instead of the desktop theme).
var (
	colorBg    = lipgloss.Color("#141021")
	colorFg    = lipgloss.Color("#e8e4f0")
	colorDim   = lipgloss.Color("#8b85a0")
	colorFaint = lipgloss.Color("#5a5470")
	colorHot   = lipgloss.Color("#ff2e97") // LIVE dot + urgent errors
	colorCool  = lipgloss.Color("#4fc3f7") // BUFFERING
	colorWarm  = lipgloss.Color("#ffc542") // PAUSED
	colorBar   = lipgloss.Color("#7c5cff") // progress + meter
)

var (
	baseStyle  = lipgloss.NewStyle().Foreground(colorFg).Background(colorBg)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorFg).Background(colorBg)
	dimStyle   = lipgloss.NewStyle().Foreground(colorDim).Background(colorBg)
	hotStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorHot).Background(colorBg)
	warmStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorWarm).Background(colorBg)
	coolStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorCool).Background(colorBg)
	idleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorDim).Background(colorBg)
	errStyle   = lipgloss.NewStyle().Foreground(colorHot).Background(colorBg)
	footKey    = lipgloss.NewStyle().Bold(true).Foreground(colorFg).Background(colorBg)
	footHint   = lipgloss.NewStyle().Foreground(colorFaint).Background(colorBg)
	barStyle   = lipgloss.NewStyle().Foreground(colorBar).Background(colorBg)
	waveStyle  = lipgloss.NewStyle().Foreground(colorBar).Background(colorBg)
	calmStyle  = lipgloss.NewStyle().Foreground(colorFaint).Background(colorBg)
	artBox     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorFaint)
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

// outputLabel mirrors the panel: the current device's label, or
// "System default" for "".
func (m Model) outputLabel() string {
	if m.st.Output == "" {
		return "System default"
	}
	for _, d := range m.devices {
		if d.Name == m.st.Output {
			if label := strings.TrimSpace(d.Description); label != "" {
				return label
			}
			return d.Name
		}
	}
	return cutRunes(m.st.Output, 40)
}

func seconds(total int) string {
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

func cutRunes(s string, limit int) string {
	runes := []rune(s)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit])
	}
	return s
}

// renderWidth returns the content width (sane default before measurement).
func (m Model) renderWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 60
}

// progressBar is the thin textual progress line with a fraction dot
// (the fuller wave meter sits below).
func (m Model) progressBar() string {
	if m.posLen <= 0 {
		return ""
	}
	width := max(24, m.renderWidth()/3)
	filled := max(0, min(width-1, m.posNow*width/m.posLen))
	line := strings.Repeat("━", filled) + "●" + strings.Repeat("━", max(0, width-1-filled))
	return barStyle.Render(line) + dimStyle.Render(" "+seconds(m.posNow)+" / "+seconds(m.posLen))
}

// meterBand renders the meter: the backend's real band levels when its
// sample tap answers, else the decorative wave: sine-ish tilts around the
// midline, phase-advanced only while playing (gated on real playback). The
// band freezes into the faint midline when idle/paused, like the panel
// canvas.
func meterBand(cols int, phase float64, live bool, bands []float32) string {
	if cols <= 0 {
		return ""
	}
	if !live {
		return calmStyle.Render(strings.Repeat("▁", cols))
	}
	if len(bands) > 0 {
		return spectrumBand(cols, bands)
	}
	runes := make([]rune, 0, cols)
	for x := 0; x < cols; x++ {
		// Blend a couple of spatial frequencies; the engine backend feeds
		// real levels instead (bands), this faux amp is the fallback.
		v := 0.55*((mathSin(0.32*float64(x)+phase)+1)/2) + 0.45*((mathSin(0.09*float64(x)-phase*0.6)+1)/2)
		runes = append(runes, waveGlyph(v))
	}
	return waveStyle.Render(string(runes))
}

// spectrumBand draws the backend's log-spaced band levels across cols
// glyphs, resampling by nearest band so any panel width works.
func spectrumBand(cols int, bands []float32) string {
	runes := make([]rune, 0, cols)
	for x := 0; x < cols; x++ {
		runes = append(runes, waveGlyph(max(0, min(1, float64(bands[x*len(bands)/cols])))))
	}
	return waveStyle.Render(string(runes))
}

// waveGlyph maps a normalized 0..1 amplitude into the shade ladder. The
// ladder is indexed as runes: the glyphs are multi-byte, so a byte index
// would slice them into control characters.
func waveGlyph(v float64) rune {
	ladder := []rune("▁▂▃▄▅▆▇█")
	i := max(0, min(len(ladder)-1, int(v*float64(len(ladder)))))
	return ladder[i]
}

// pickerStatus is the one-line state rendered under the device list (or ""
// when devices are listed): scanning, empty, or the fetch error.
func (m Model) pickerStatus() string {
	switch {
	case m.outputsErr != "":
		return errStyle.Render(cutRunes("outputs: "+m.outputsErr, 60))
	case !m.outputsLoaded:
		return dimStyle.Render("scanning outputs…")
	case len(m.devices) == 0:
		return dimStyle.Render("no extra outputs found")
	}
	return ""
}

// pickerFrame renders the output picker as a floating menu that fits the row
// budget: the two border rows and the status line are reserved, and the rest
// is a window over the list that follows the cursor. A PipeWire host can
// enumerate dozens of devices against a few rows on a 24-row terminal, and
// the cursor row must never scroll out of view: enter applies the unseen row.
func (m Model) pickerFrame(rows int) string {
	items := []string{"System default"}
	for _, d := range m.devices {
		label := strings.TrimSpace(d.Description)
		if label == "" {
			label = d.Name
		}
		items = append(items, label)
	}
	status := m.pickerStatus()

	listRows := rows - 2 // rounded border
	if status != "" {
		listRows--
	}
	listRows = max(1, listRows)

	cursor := max(0, min(m.pickerCursor, len(items)-1))
	start := 0
	if len(items) > listRows {
		start = max(0, min(cursor-listRows/2, len(items)-listRows))
	}
	end := min(len(items), start+listRows)

	var b strings.Builder
	for i := start; i < end; i++ {
		// Two marks: the cursor (▸, styled hot) and the current output
		// (● vs ○) — the cursor must not swallow the current marker.
		cursorMark, style := " ", dimStyle
		if i == cursor {
			cursorMark, style = "▸", hotStyle
		}
		current := (i == 0 && m.st.Output == "") ||
			(i > 0 && i-1 < len(m.devices) && m.devices[i-1].Name == m.st.Output)
		currentMark := "○"
		if current {
			currentMark = "●"
		}
		b.WriteString(style.Render(cursorMark+" "+currentMark+" "+cutRunes(items[i], 60)) + "\n")
	}
	if status != "" {
		b.WriteString(status + "\n")
	}
	return artBox.Render(strings.TrimSuffix(b.String(), "\n"))
}

// helpRows is the ? overlay.
func helpRows() string {
	rows := []string{
		"space   pause/play",
		"s       stop",
		"+ / -   volume (5 steps)",
		"o       output picker",
		"?       help",
		"esc     close picker, then quit",
		"q       quit",
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("keys") + "\n")
	for _, r := range rows {
		b.WriteString(dimStyle.Render(r) + "\n")
	}
	return b.String()
}

// historyRows renders "RECENT TRANSMISSIONS": each entry "artist — track"
// (or the concatenation when one side is missing, or "—"), with the
// played_at stamp on the right in HH:mm local time.
func (m Model) historyRows(width int) string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("RECENT TRANSMISSIONS") + "\n")
	if len(m.history) == 0 {
		b.WriteString(calmStyle.Render("—") + "\n")
		return b.String()
	}
	for _, entry := range m.history {
		artist := cutRunes(entry.Artist, 80)
		track := cutRunes(entry.Track, 80)
		var line string
		switch {
		case artist != "" && track != "":
			line = artist + " — " + track
		case artist != "" || track != "":
			line = artist + track
		default:
			line = "—"
		}
		left := cutRunes(line, max(0, width-8))
		stamp := ""
		if entry.PlayedAt != nil && *entry.PlayedAt > 0 {
			stamp = playedAtClock(*entry.PlayedAt)
		}
		pad := max(0, width-len([]rune(left))-len([]rune(stamp))-2)
		b.WriteString(baseStyle.Render(left) + strings.Repeat(" ", pad) +
			dimStyle.Render(stamp) + "\n")
	}
	return b.String()
}

func playedAtClock(epoch int) string {
	return time.Unix(int64(epoch), 0).Local().Format("15:04")
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

	// left: the art box; right: now-playing + progress + meter. The art
	// anchors a fixed block so the lower sections stay within a standard
	// 24-row terminal; the picker/history share the leftover row budget.
	left := m.artworkPanel()
	w := max(18, m.renderWidth()-artBoxWidth-4)

	// The right column must not outgrow the art box: wrap-heavy titles used
	// to push the frame past the viewport. Title and artist get at most
	// three lines each, errors two, and the column is capped at the panel
	// height (only a fully loaded column with both errors can touch it).
	right := []string{titleStyle.Render(limitLines(wrapToWidth(m.displayTrack(), w), 3))}
	if artist := m.displayArtist(); artist != "" {
		right = append(right, "")
		right = append(right, dimStyle.Render(limitLines(wrapToWidth(artist, w), 3)))
	}
	if bar := m.progressBar(); bar != "" {
		right = append(right, "")
		right = append(right, bar)
	}
	right = append(right, "")
	right = append(right, meterBand(max(16, w-2), m.wavePhase, m.st.Playing && !m.st.Paused, m.bands))
	if m.liveErr != "" {
		right = append(right, errStyle.Render(limitLines(wrapToWidth("mpv: "+m.liveErr, w), 2)))
	}
	if m.metaErr != "" {
		right = append(right, errStyle.Render(limitLines(wrapToWidth("plaza: "+m.metaErr, w), 2)))
	}
	if len(right) > artPanelHeight {
		right = right[:artPanelHeight]
	}

	deck := lipgloss.JoinHorizontal(lipgloss.Top, left, baseStyle.Render(strings.Join(right, "\n")))
	b.WriteString(deck + "\n\n")

	// lower block: the picker or help replaces recent transmissions while
	// open — one shared row budget keeps the whole frame inside the
	// viewport (a full-bleed panel would push overlays off-screen). The
	// floating frames reserve their two border rows from the same budget.
	rows := m.lowerBlockBudget()
	if m.pickerOpen {
		b.WriteString(m.pickerFrame(rows))
	} else if m.helpOpen {
		b.WriteString(overlayFrame(helpRows(), rows))
	} else {
		b.WriteString(trimRows(m.historyRows(max(24, m.renderWidth()-4)), rows))
	}
	b.WriteString("\n\n")

	// transport footer
	hints := []string{
		footKey.Render("space") + footHint.Render(" play"),
		footKey.Render("s") + footHint.Render(" stop"),
		footKey.Render("+/-") + footHint.Render(" vol"),
		footKey.Render("o") + footHint.Render(" output"),
		footKey.Render("?") + footHint.Render(" help"),
		footKey.Render("q") + footHint.Render(" quit"),
	}
	footer := dimStyle.Render(fmt.Sprintf("%s · %s", m.outputLabel(), fmt.Sprintf("volume %d%%", m.st.Volume))) +
		"   " + strings.Join(hints, footHint.Render(" · "))

	b.WriteString(footer)

	v := tea.NewView(baseStyle.Render(b.String()))
	v.AltScreen = true
	v.BackgroundColor = colorBg
	return v
}

// lowerBlockBudget reserves: badge (1) + blank (1) + deck (art box
// height) + blank (1) + blank (1, the separator after the lower block) +
// footer (1). Everything else goes to the lower block so picks/history
// stay on a 24-row terminal instead of scrolling out of the viewport.
func (m Model) lowerBlockBudget() int {
	used := 5 + artPanelHeight
	if m.height > 0 {
		return max(1, m.height-used)
	}
	return 5 // before the first WindowSizeMsg
}

// trimRows limits a rendered block to n lines (viewport budget).
func trimRows(s string, n int) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// overlayFrame outlines a block as a floating menu, fitting its content to
// the row budget: the rounded border costs one row on top and one below.
func overlayFrame(content string, rows int) string {
	content = strings.TrimSuffix(trimRows(content, max(1, rows-2)), "\n")
	return artBox.Render(content)
}

// limitLines caps s to n lines (used so wrapped text cannot push the frame
// past the viewport).
func limitLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// artworkPanel renders the 24×12 art (or its placeholder) in a bordered
// box.
func (m Model) artworkPanel() string {
	inner := dimStyle.Render("♪")
	if m.artwork != "" && m.truecolor {
		inner = m.artwork
	}
	return artBox.Render(lipgloss.NewStyle().
		Width(artworkCols).Height(artworkRows).Align(lipgloss.Center, lipgloss.Center).
		Render(inner))
}

// wrapToWidth hard-wraps a single line to w columns (the panel wraps its
// title to two lines; plain wrap keeps it readable).
func wrapToWidth(s string, w int) string {
	if w <= 0 || s == "" {
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
