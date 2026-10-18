//go:build linux

package audio

import (
	"errors"
	"fmt"
	"sync"

	"github.com/jfreymuth/pulse"
)

// pulseOutput implements Output on top of the PulseAudio protocol. On modern
// Linux systems this reaches PipeWire through pipewire-pulse; systems without
// a PulseAudio-compatible server are unsupported by this backend.
type pulseOutput struct {
	client *pulse.Client
	rate   int

	mu      sync.Mutex
	ring    *pcmRing
	stream  *pulse.PlaybackStream
	closed  bool
	primed  bool
	starved bool
}

// NewOutput connects to the PulseAudio-compatible server and returns an
// output that renders at the given sample rate.
func NewOutput(rate int) (Output, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName("nghtwv"))
	if err != nil {
		return nil, fmt.Errorf("audio: connect: %w", err)
	}
	return &pulseOutput{client: client, rate: rate}, nil
}

func (o *pulseOutput) Devices() ([]Device, error) {
	sinks, err := o.client.ListSinks()
	if err != nil {
		return nil, fmt.Errorf("audio: list devices: %w", err)
	}
	devices := make([]Device, 0, len(sinks))
	for _, sink := range sinks {
		devices = append(devices, Device{Name: sink.ID(), Description: sink.Name()})
	}
	return devices, nil
}

func (o *pulseOutput) Default() (Device, error) {
	sink, err := o.client.DefaultSink()
	if err != nil {
		return Device{}, fmt.Errorf("audio: default device: %w", err)
	}
	return Device{Name: sink.ID(), Description: sink.Name()}, nil
}

func (o *pulseOutput) Open(device string) error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrClosed
	}
	if o.stream != nil {
		o.mu.Unlock()
		return errors.New("audio: already open")
	}
	o.mu.Unlock()

	sink, err := o.resolve(device)
	if err != nil {
		return err
	}
	// Half a second of stereo frames of decode-ahead.
	ring := newPCMRing(o.rate)
	stream, err := o.client.NewPlayback(
		pulse.Float32Reader(o.pull),
		pulse.PlaybackStereo,
		pulse.PlaybackSampleRate(o.rate),
		// The library requires latency to be set after the sample rate and
		// channel options.
		pulse.PlaybackLatency(0.3),
		pulse.PlaybackSink(sink),
		pulse.PlaybackMediaName("nghtwv"),
	)
	if err != nil {
		return fmt.Errorf("audio: open %q: %w", device, err)
	}

	o.mu.Lock()
	o.ring = ring
	o.stream = stream
	o.mu.Unlock()

	// Start must not run under o.mu: it wakes the pull goroutine, which
	// locks o.mu before it can produce any data.
	stream.Start()
	return nil
}

// resolve turns an empty device name into the server default and a stable
// sink name into the sink itself.
func (o *pulseOutput) resolve(device string) (*pulse.Sink, error) {
	if device == "" {
		sink, err := o.client.DefaultSink()
		if err != nil {
			return nil, fmt.Errorf("audio: default device: %w", err)
		}
		return sink, nil
	}
	sink, err := o.client.SinkByID(device)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNoDevice, device)
	}
	return sink, nil
}

// pull is invoked by the pulse client goroutine whenever the server's buffer
// wants data. It must never return (0, nil): the client spins on a reader
// that makes no progress. Falling behind is answered with silence and
// recorded as an underflow.
func (o *pulseOutput) pull(buf []float32) (int, error) {
	o.mu.Lock()
	ring, closed, primed := o.ring, o.closed, o.primed
	o.mu.Unlock()
	if ring == nil || closed {
		return 0, pulse.EndOfData
	}
	n := ring.Read(buf)
	if n < len(buf) {
		clear(buf[n:])
		// Silence before the first sample is the caller warming up (fetching
		// and decoding the stream), not starvation.
		if primed {
			o.markStarved()
		}
	}
	if n > 0 {
		o.mu.Lock()
		o.primed = true
		o.mu.Unlock()
	}
	return len(buf), nil
}

func (o *pulseOutput) markStarved() {
	o.mu.Lock()
	o.starved = true
	o.mu.Unlock()
}

func (o *pulseOutput) Write(frames []float32) error {
	o.mu.Lock()
	ring, closed := o.ring, o.closed
	o.mu.Unlock()
	if ring == nil || closed {
		return ErrClosed
	}
	if _, err := ring.Write(frames); err != nil {
		if errors.Is(err, errRingClosed) {
			return ErrClosed
		}
		return err
	}
	return nil
}

// Flush drops everything the ring has buffered. The Pulse server also keeps
// PlaybackLatency (~300ms) of audio that the library exposes no flush for, so
// that much may still play out after a pause.
func (o *pulseOutput) Flush() error {
	o.mu.Lock()
	ring, closed := o.ring, o.closed
	o.mu.Unlock()
	if ring == nil || closed {
		return ErrClosed
	}
	ring.Reset()
	return nil
}

func (o *pulseOutput) Underflow() bool {
	o.mu.Lock()
	starved, stream := o.starved, o.stream
	o.mu.Unlock()
	return starved || (stream != nil && stream.Underflow())
}

func (o *pulseOutput) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	ring, stream := o.ring, o.stream
	o.ring, o.stream = nil, nil
	o.mu.Unlock()
	if ring != nil {
		ring.Close()
	}
	if stream != nil {
		stream.Stop()
		stream.Close()
	}
	o.client.Close()
	return nil
}
