package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/mp3"
)

const (
	defaultChunkFrames = 1024
	defaultBackoffBase = 500 * time.Millisecond
	defaultBackoffMax  = 8 * time.Second
)

// Stream is a decoded PCM source that can release its resources (typically
// the network connection) when the engine is done with it.
type Stream interface {
	beep.Streamer
	io.Closer
}

// Opener opens a decoded stream. The engine calls it again whenever a stream
// ends or fails, so a dropped live stream reconnects with backoff.
type Opener func(ctx context.Context) (Stream, beep.Format, error)

// PlazaOpener decodes the Nightwave Plaza MP3 stream. The stream is live, so
// the HTTP client sets no request timeout; ctx bounds the connection instead.
func PlazaOpener(url string) Opener {
	client := &http.Client{}
	return func(ctx context.Context) (Stream, beep.Format, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, beep.Format{}, err
		}
		req.Header.Set("User-Agent", "nghtwv")
		resp, err := client.Do(req)
		if err != nil {
			return nil, beep.Format{}, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, beep.Format{}, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, resp.Status)
		}
		decoded, format, err := mp3.Decode(resp.Body)
		if err != nil {
			resp.Body.Close()
			return nil, beep.Format{}, err
		}
		return decoded, format, nil
	}
}

// State is the engine's transport state.
type State int

const (
	StateStopped State = iota
	StateBuffering
	StatePlaying
	StatePaused
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateBuffering:
		return "buffering"
	case StatePlaying:
		return "playing"
	case StatePaused:
		return "paused"
	}
	return "unknown"
}

// Status is the engine's observable state.
type Status struct {
	State  State
	Volume int // 0..100
}

// Engine decodes a live stream and renders it through an Output, with a
// software volume stage and a sample tap feeding the spectrum analyzer.
//
// The Output is owned by the caller: it must be Open before Play and stays
// open across stop/start cycles (that matches the service model, where the
// device is selected independently of playback).
type Engine struct {
	out    Output
	rate   int
	opener Opener

	chunk       int
	backoffBase time.Duration
	backoffMax  time.Duration

	tap      *Tap
	spectrum *Spectrum

	mu      sync.Mutex
	running bool
	paused  bool
	volume  int
	state   State
	err     error
	runCtx  context.Context
	cancel  context.CancelFunc
}

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithVolume sets the initial volume (0..100, clamped).
func WithVolume(volume int) EngineOption {
	return func(e *Engine) { e.volume = clampVolume(volume) }
}

// WithChunkFrames sets how many frames the decode loop pulls per iteration.
func WithChunkFrames(frames int) EngineOption {
	return func(e *Engine) {
		if frames > 0 {
			e.chunk = frames
		}
	}
}

// WithBackoff sets the reconnect backoff bounds.
func WithBackoff(base, max time.Duration) EngineOption {
	return func(e *Engine) {
		if base > 0 {
			e.backoffBase = base
		}
		if max >= base {
			e.backoffMax = max
		}
	}
}

// NewEngine creates an engine rendering at the output's sample rate.
func NewEngine(out Output, rate int, opener Opener, opts ...EngineOption) *Engine {
	e := &Engine{
		out:         out,
		rate:        rate,
		opener:      opener,
		chunk:       defaultChunkFrames,
		backoffBase: defaultBackoffBase,
		backoffMax:  defaultBackoffMax,
		volume:      100,
		state:       StateStopped,
	}
	for _, opt := range opts {
		opt(e)
	}
	// ~150ms of interleaved stereo samples: enough for the analyzer's 50ms
	// tick with slack, small enough that overflow only skips visuals ahead.
	e.tap = newTap(e.rate * 3 / 10)
	e.spectrum = newSpectrum(e.tap, e.rate, spectrumBands)
	return e
}

// Play starts or resumes playback. It returns immediately; Status reports
// progress. Play may be called again after Stop.
func (e *Engine) Play() {
	e.mu.Lock()
	if e.running {
		e.paused = false
		e.state = StatePlaying
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running = true
	e.paused = false
	e.runCtx = ctx
	e.cancel = cancel
	e.state = StateBuffering
	e.mu.Unlock()

	e.spectrum.Start(ctx)
	go e.run(ctx)
}

// Pause keeps the stream connected and the decoder running (so playback
// stays at the live edge) but renders silence. Queued audio is flushed as
// soon as the pause is observed, so at most the backend's own buffer (the
// Pulse server latency, ~300ms) still plays out. Resume flushes again so the
// silence buffered while paused is not replayed.
func (e *Engine) Pause() {
	e.mu.Lock()
	if e.running {
		e.paused = true
		e.state = StatePaused
	}
	e.mu.Unlock()
}

// Stop cancels playback and the spectrum analyzer. The output stays open.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	cancel := e.cancel
	e.running = false
	e.paused = false
	e.runCtx = nil
	e.cancel = nil
	e.state = StateStopped
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// SetVolume sets the software volume (0..100, clamped).
func (e *Engine) SetVolume(volume int) {
	e.mu.Lock()
	e.volume = clampVolume(volume)
	e.mu.Unlock()
}

// Volume returns the current volume.
func (e *Engine) Volume() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.volume
}

// Status returns the current transport state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Status{State: e.state, Volume: e.volume}
}

// Err returns the last transport error. It is cleared when a new stream
// connects.
func (e *Engine) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// Spectrum returns the latest spectrum snapshot.
func (e *Engine) Spectrum() Snapshot { return e.spectrum.Latest() }

func clampVolume(v int) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}

// run opens stream after stream until its context is cancelled, reconnecting
// with exponential backoff whenever a stream ends or fails.
func (e *Engine) run(ctx context.Context) {
	defer e.finish(ctx)
	backoff := e.backoffBase
	for {
		stream, format, err := e.opener(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.setError(err)
			e.setState(StateBuffering)
		} else {
			backoff = e.backoffBase
			e.setError(nil)
			err = e.pump(ctx, stream, format)
			stream.Close()
			if ctx.Err() != nil {
				return
			}
			e.setError(err)
			if errors.Is(err, ErrClosed) {
				// The device is gone; retrying cannot help. The owner
				// observes this through the output's ErrReporter.
				e.setState(StateStopped)
				return
			}
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > e.backoffMax {
			backoff = e.backoffMax
		}
	}
}

// finish clears the session only if it still owns it, so a fast
// Stop-then-Play cannot be clobbered by the old goroutine.
func (e *Engine) finish(ctx context.Context) {
	e.mu.Lock()
	if e.runCtx == ctx {
		e.running = false
		e.runCtx = nil
		e.cancel = nil
	}
	e.mu.Unlock()
}

// pump transfers one stream to the output until it drains, fails or the
// context is cancelled.
func (e *Engine) pump(ctx context.Context, stream Stream, format beep.Format) error {
	source := beep.Streamer(stream)
	if int(format.SampleRate) != e.rate {
		source = beep.Resample(4, format.SampleRate, beep.SampleRate(e.rate), source)
	}
	frames := make([][2]float64, e.chunk)
	samples := make([]float32, e.chunk*2)
	paused := e.isPaused()
	if paused {
		e.setState(StatePaused)
	} else {
		e.setState(StatePlaying)
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if now := e.isPaused(); now != paused {
			paused = now
			if paused {
				// Drop queued audio right away, otherwise up to the ring's
				// half second would keep playing behind the silence.
				if err := e.out.Flush(); err != nil {
					return err
				}
				e.setState(StatePaused)
			} else {
				// Drop the silence buffered while paused so playback resumes
				// at the live edge.
				if err := e.out.Flush(); err != nil {
					return err
				}
				e.setState(StatePlaying)
			}
		}
		n, ok := source.Stream(frames)
		if n > 0 {
			if paused {
				// Keep decoding so resume is instant, but render silence.
				clear(samples[:n*2])
				if err := e.out.Write(samples[:n*2]); err != nil {
					return err
				}
			} else {
				for i, sample := range frames[:n] {
					samples[2*i] = float32(sample[0])
					samples[2*i+1] = float32(sample[1])
				}
				e.tap.Write(samples[:n*2]) // pre-gain: visuals ignore volume
				gain := e.gain()
				for i := range samples[:n*2] {
					samples[i] *= gain
				}
				if err := e.out.Write(samples[:n*2]); err != nil {
					return err
				}
			}
		}
		if !ok {
			return source.Err()
		}
	}
}

func (e *Engine) isPaused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused
}

// gain is the software volume. mpv's mixer computes pow(volume/100, 3) (see
// audio_get_gain in mpv's player/audio.c), so the cubic curve keeps every
// persisted player volume at the same loudness after the engine swap.
func (e *Engine) gain() float32 {
	e.mu.Lock()
	v := e.volume
	e.mu.Unlock()
	vol := float64(v) / 100
	return float32(vol * vol * vol)
}

func (e *Engine) setState(state State) {
	e.mu.Lock()
	e.state = state
	e.mu.Unlock()
}

func (e *Engine) setError(err error) {
	e.mu.Lock()
	e.err = err
	e.mu.Unlock()
}

// sleepCtx sleeps for d, returning false if ctx is done first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
