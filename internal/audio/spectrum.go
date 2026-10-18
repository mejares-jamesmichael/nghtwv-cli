package audio

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

const (
	// spectrumWindow is the FFT window in mono samples (~23ms at 44.1kHz).
	spectrumWindow = 1024
	// spectrumBands is the number of log-spaced output bands.
	spectrumBands = 16
	// spectrumTick is the analysis cadence.
	spectrumTick = 50 * time.Millisecond
	// spectrumLow and spectrumHigh bound the analyzed range in Hz.
	spectrumLow  = 40.0
	spectrumHigh = 16000.0
)

// Snapshot is the latest spectrum analysis: Bands holds 0..1 levels from low
// to high frequency, Level is the overall loudness (0..1). It reflects the
// audio before volume gain, so visuals do not vanish when the volume is low.
// This is the payload of the service's "spectrum" verb; Bands is empty until
// the first analysis window lands.
type Snapshot struct {
	Bands []float32 `json:"bands"`
	Level float32   `json:"level"`
}

// ErrSpectrumUnavailable reports a backend that has no sample tap: the mpv
// backend answers "unsupported" over the service protocol, and callers fall
// back to decorative visuals instead of retrying.
var ErrSpectrumUnavailable = errors.New("audio: spectrum unavailable")

// Bands reports how many log-spaced levels every Snapshot carries, so
// clients can lay out visuals without hardcoding the count.
func Bands() int { return spectrumBands }

// Spectrum turns tapped PCM into band levels off the playback path. All
// analysis state is guarded by one mutex, so a restarted analyzer can never
// race an old one.
type Spectrum struct {
	tap  *Tap
	rate int

	edgeStart []int // per band: first FFT bin
	edgeEnd   []int // per band: one past the last FFT bin
	hann      []float64

	roll   []float32 // rolling mono window
	re, im []float64
	levels []float32 // smoothed band levels

	mu     sync.Mutex
	latest Snapshot
}

// newSpectrum creates an analyzer reading from tap at the given rate.
func newSpectrum(tap *Tap, rate, bands int) *Spectrum {
	s := &Spectrum{
		tap:    tap,
		rate:   rate,
		roll:   make([]float32, spectrumWindow),
		re:     make([]float64, spectrumWindow),
		im:     make([]float64, spectrumWindow),
		hann:   make([]float64, spectrumWindow),
		levels: make([]float32, bands),
		latest: Snapshot{Bands: make([]float32, bands)},
	}
	for i := range s.hann {
		s.hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(spectrumWindow))
	}

	// Log-spaced band edges, clamped to ascending bins in [1, N/2).
	s.edgeStart = make([]int, bands)
	s.edgeEnd = make([]int, bands)
	bin := func(f float64) int {
		b := int(f/float64(rate)*spectrumWindow + 0.5)
		if b < 1 {
			b = 1
		}
		if b > spectrumWindow/2-1 {
			b = spectrumWindow/2 - 1
		}
		return b
	}
	start := 1
	for b := range bands {
		lo := spectrumLow * math.Pow(spectrumHigh/spectrumLow, float64(b)/float64(bands))
		hi := spectrumLow * math.Pow(spectrumHigh/spectrumLow, float64(b+1)/float64(bands))
		s0, e0 := bin(lo), bin(hi)
		if s0 < start {
			s0 = start
		}
		if e0 <= s0 {
			e0 = s0 + 1
		}
		s.edgeStart[b], s.edgeEnd[b] = s0, e0
		start = e0
	}
	return s
}

// Start runs the analyzer until ctx is done.
func (s *Spectrum) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(spectrumTick)
		defer ticker.Stop()
		buf := make([]float32, s.tap.cap())
		mono := make([]float32, len(buf)/2)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n := s.tap.Read(buf)
				m := 0
				for i := 0; i+1 < n; i += 2 {
					mono[m] = (buf[i] + buf[i+1]) / 2
					m++
				}
				s.advance(mono[:m])
				s.analyze()
			}
		}
	}()
}

// advance shifts the rolling window: mono samples are appended (newest kept
// when there are more than a window) and silence fills the rest, so a paused
// stream visibly decays instead of freezing.
func (s *Spectrum) advance(mono []float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(mono) >= len(s.roll) {
		copy(s.roll, mono[len(mono)-len(s.roll):])
		return
	}
	shift := len(mono)
	if shift == 0 {
		shift = len(s.roll) / 4
	}
	copy(s.roll, s.roll[shift:])
	tail := s.roll[len(s.roll)-shift:]
	if n := copy(tail, mono); n < len(tail) {
		clear(tail[n:])
	}
}

// analyze transforms the current window, updates the smoothed levels, stores
// the latest snapshot and returns it.
func (s *Spectrum) analyze() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.roll {
		s.re[i] = float64(s.roll[i]) * s.hann[i]
		s.im[i] = 0
	}
	fft(s.re, s.im)

	bands := make([]float32, len(s.levels))
	for b := range bands {
		amp := 0.0
		for k := s.edgeStart[b]; k < s.edgeEnd[b]; k++ {
			if m := math.Hypot(s.re[k], s.im[k]); m > amp {
				amp = m
			}
		}
		// A full-scale sine peaks at window/2 in its bin.
		amp /= float64(spectrumWindow) / 2
		level := (20*math.Log10(amp+1e-9) + 70) / 60
		switch {
		case level < 0:
			level = 0
		case level > 1:
			level = 1
		}
		// Fast attack, slow release.
		rate := 0.2
		if level > float64(s.levels[b]) {
			rate = 0.6
		}
		s.levels[b] += float32((level - float64(s.levels[b])) * rate)
		bands[b] = s.levels[b]
	}

	var sum float64
	for _, v := range s.roll {
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / float64(len(s.roll)))
	loudness := math.Min(1, rms*3)

	s.latest = Snapshot{Bands: bands, Level: float32(loudness)}
	return s.latest
}

// Latest returns a copy of the most recent snapshot.
func (s *Spectrum) Latest() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	bands := make([]float32, len(s.latest.Bands))
	copy(bands, s.latest.Bands)
	return Snapshot{Bands: bands, Level: s.latest.Level}
}

// fft replaces re/im with their discrete Fourier transform, in place
// (iterative radix-2; len(re) must be a power of two).
func fft(re, im []float64) {
	n := len(re)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j &^= bit
		}
		j |= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wr, wi := math.Cos(ang), math.Sin(ang)
		for i := 0; i < n; i += length {
			cr, ci := 1.0, 0.0
			for k := 0; k < length/2; k++ {
				ur, ui := re[i+k], im[i+k]
				vr := re[i+k+length/2]*cr - im[i+k+length/2]*ci
				vi := re[i+k+length/2]*ci + im[i+k+length/2]*cr
				re[i+k], im[i+k] = ur+vr, ui+vi
				re[i+k+length/2], im[i+k+length/2] = ur-vr, ui-vi
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}
