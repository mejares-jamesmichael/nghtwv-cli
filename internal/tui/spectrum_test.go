package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// fakeSpectrumPlayback is a playback seam with a sample tap (the engine
// backend), so the panel can render real band levels.
type fakeSpectrumPlayback struct {
	*fakePlayback
	snap  audio.Snapshot
	err   error
	calls int
}

func (f *fakeSpectrumPlayback) Spectrum(context.Context) (audio.Snapshot, error) {
	f.calls++
	return f.snap, f.err
}

// liveModel folds a playing state into a fresh model.
func liveModel(play Playback) Model {
	return fold(New(play, &fakeMeta{}), statusMsg{st: player.State{Playing: true, Volume: 70}})
}

// tick folds one meter tick into the model.
func tick(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(meterTickMsg{})
	return next.(Model), cmd
}

// fullBands is a snapshot with every band at full scale.
func fullBands() audio.Snapshot {
	snap := audio.Snapshot{Bands: make([]float32, audio.Bands()), Level: 1}
	for i := range snap.Bands {
		snap.Bands[i] = 1
	}
	return snap
}

func TestMeterPollsRealBandsWhenAvailable(t *testing.T) {
	play := &fakeSpectrumPlayback{fakePlayback: &fakePlayback{}, snap: fullBands()}
	m := liveModel(play)

	m, cmd := tick(t, m)
	if cmd == nil {
		t.Fatal("the meter tick must keep the timer chain alive")
	}
	if !m.spectrumBusy {
		t.Fatal("a backend with a tap must be polled on the meter tick")
	}

	// The tick's batch carries the poll; drive it and fold the reply.
	m = fold(m, spectrumPoll(play)())
	if m.spectrumBusy {
		t.Fatal("the reply must clear the in-flight marker")
	}
	if len(m.bands) != audio.Bands() {
		t.Fatalf("bands = %d levels, want %d", len(m.bands), audio.Bands())
	}
	if view := m.View().Content; !strings.Contains(view, strings.Repeat("█", 8)) {
		t.Fatalf("the meter must render the real band levels:\n%s", stripANSI(view))
	}
}

func TestMeterKeepsDecorativeWaveWithoutATap(t *testing.T) {
	m := liveModel(&fakePlayback{})

	m, _ = tick(t, m)
	if m.spectrumBusy {
		t.Fatal("a backend without a tap must not be polled")
	}
	if m.wavePhase == 0 {
		t.Fatal("the decorative wave must keep advancing while playing")
	}

	// An unsupported reply latches the fallback: later ticks never ask again.
	m = fold(m, spectrumMsg{err: audio.ErrSpectrumUnavailable})
	if !m.spectrumOff {
		t.Fatal("an unsupported backend must be remembered")
	}
	m, _ = tick(t, m)
	if m.spectrumBusy {
		t.Fatal("a latched fallback must stop polling")
	}
}

func TestMeterStopsPollingWhenPaused(t *testing.T) {
	play := &fakeSpectrumPlayback{fakePlayback: &fakePlayback{}, snap: fullBands()}
	m := liveModel(play)
	m = fold(m, statusMsg{st: player.State{Playing: true, Paused: true, Volume: 70}})

	m, _ = tick(t, m)
	if m.spectrumBusy {
		t.Fatal("a paused panel must not poll bands")
	}
	if m.wavePhase != 0 {
		t.Fatalf("a paused panel must freeze the wave, phase = %v", m.wavePhase)
	}
}

func TestSpectrumBandResamplesAnyWidth(t *testing.T) {
	bands := []float32{0, 0.5, 1}
	if got := stripANSI(spectrumBand(6, bands)); len([]rune(got)) != 6 {
		t.Fatalf("spectrumBand width = %q, want 6 glyphs", got)
	}
	if got := stripANSI(spectrumBand(0, bands)); got != "" {
		t.Fatalf("spectrumBand(0) = %q, want empty", got)
	}
}

// TestWaveGlyphUsesTheRunes pins the shade ladder: the glyphs are multi-byte
// UTF-8, and indexing the ladder as bytes used to render control characters.
func TestWaveGlyphUsesTheRunes(t *testing.T) {
	if got := waveGlyph(0); got != '▁' {
		t.Fatalf("waveGlyph(0) = %q, want the lowest shade", got)
	}
	if got := waveGlyph(1); got != '█' {
		t.Fatalf("waveGlyph(1) = %q, want the full block", got)
	}
	if got := waveGlyph(2); got != '█' {
		t.Fatalf("waveGlyph(2) = %q, want the clamp at the full block", got)
	}
	for _, v := range []float64{0, 0.25, 0.5, 0.75, 1} {
		if got := waveGlyph(v); got < '▁' || got > '█' {
			t.Fatalf("waveGlyph(%v) = %q, want a ladder glyph", v, got)
		}
	}
}
