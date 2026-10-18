package audio

import (
	"math"
	"testing"
)

func TestSpectrumFindsTone(t *testing.T) {
	sp := newSpectrum(newTap(8192), 44100, spectrumBands)
	const freq = 1000.0
	mono := make([]float32, spectrumWindow)
	for i := range mono {
		mono[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/44100))
	}
	sp.advance(mono)
	snap := sp.analyze()

	peakBand, peak := 0, float32(0)
	for b, v := range snap.Bands {
		if v > peak {
			peakBand, peak = b, v
		}
	}
	// 1kHz lands in band 8 of 16 (40Hz..16kHz, log-spaced).
	if peakBand < 7 || peakBand > 9 {
		t.Fatalf("peak band = %d; want band 7..9 for 1kHz", peakBand)
	}
	if peak < 0.4 {
		t.Fatalf("peak level = %.2f; want >= 0.4", peak)
	}
	if snap.Level < 0.1 {
		t.Fatalf("loudness = %.2f; want >= 0.1", snap.Level)
	}
}

func TestSpectrumDecaysToSilence(t *testing.T) {
	sp := newSpectrum(newTap(8192), 44100, spectrumBands)
	mono := make([]float32, spectrumWindow)
	for i := range mono {
		mono[i] = float32(0.5 * math.Sin(2*math.Pi*1000*float64(i)/44100))
	}
	sp.advance(mono)
	sp.analyze()

	silence := make([]float32, 512)
	for range 30 {
		sp.advance(silence)
		sp.analyze()
	}
	snap := sp.Latest()
	if snap.Level > 0.01 {
		t.Fatalf("loudness after silence = %.3f; want ~0", snap.Level)
	}
	for b, v := range snap.Bands {
		if v > 0.05 {
			t.Fatalf("band %d after silence = %.3f; want ~0", b, v)
		}
	}
}

func TestSpectrumLatestIsACopy(t *testing.T) {
	sp := newSpectrum(newTap(8192), 44100, spectrumBands)
	got := sp.Latest()
	for i := range got.Bands {
		got.Bands[i] = 1
	}
	if again := sp.Latest(); again.Bands[0] != 0 {
		t.Fatal("Latest returned a slice shared with the analyzer")
	}
}

func TestFFTPeaksAtInputBin(t *testing.T) {
	const (
		n   = 256
		bin = 10
	)
	re := make([]float64, n)
	im := make([]float64, n)
	for i := range re {
		re[i] = math.Cos(2 * math.Pi * bin * float64(i) / n)
	}
	fft(re, im)

	peak, mag := 0, 0.0
	for k := range n / 2 {
		if m := math.Hypot(re[k], im[k]); m > mag {
			peak, mag = k, m
		}
	}
	if peak != bin {
		t.Fatalf("fft peak bin = %d; want %d", peak, bin)
	}
}
