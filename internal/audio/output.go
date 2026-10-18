// Package audio contains the playback engine that will replace mpv: output
// backends for Linux (PulseAudio protocol, reached through pipewire-pulse on
// modern systems) and Windows (WASAPI through pure-Go COM bindings), plus the
// decode, gain and spectrum stages built on top of them.
//
// Nothing in the shipping CLI uses this package yet; mpv remains the
// production backend until the engine migration is proven.
package audio

import "errors"

// Device is a selectable audio output. Name is the stable identifier that
// gets persisted (a PulseAudio sink name on Linux, a WASAPI endpoint id on
// Windows); Description is the human-readable label. The JSON shape matches
// the legacy mpv device shape so the one-shot `outputs` contract is
// preserved.
type Device struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Errors returned by Output implementations.
var (
	// ErrNoDevice reports that the requested device does not exist, for
	// example because it was unplugged since it was persisted.
	ErrNoDevice = errors.New("audio: output device not found")
	// ErrClosed reports use of an output that has been closed.
	ErrClosed = errors.New("audio: output closed")
)

// ErrReporter is implemented by outputs that can fail asynchronously, for
// example when a device disappears mid-stream. Writes fail with ErrClosed
// once such a failure has been recorded.
type ErrReporter interface {
	Err() error
}

// Mover is implemented by outputs that can reroute an open stream to another
// device without recreating it (PulseAudio's move-sink-input), so switching
// devices keeps playing seamlessly. Outputs without it are switched by
// opening a fresh output on the new device instead.
type Mover interface {
	// Move reroutes the open stream to the named device. An empty name
	// selects the backend default. It fails with ErrNoDevice when the
	// device does not exist and ErrClosed when nothing is open.
	Move(device string) error
}

// Output renders interleaved stereo float32 frames at a fixed sample rate.
//
// Backpressure: implementations buffer roughly half a second of audio
// internally; Write blocks while that buffer is full. Decoding can therefore
// run ahead of playback by at most the buffer size, and a stalled consumer
// surfaces as blocked writes instead of unbounded memory growth.
type Output interface {
	// Devices lists the selectable outputs.
	Devices() ([]Device, error)
	// Default returns the backend's default output.
	Default() (Device, error)
	// Open starts playback on the device with the given stable Name. An
	// empty name selects the backend default. It fails with ErrNoDevice if
	// the device does not exist.
	Open(device string) error
	// Write sends interleaved stereo samples. len(frames) must be even.
	Write(frames []float32) error
	// Flush drops audio buffered in the backend so the next Write starts from
	// an empty buffer. It must never block and must wake writers blocked on
	// a full buffer. It is used on pause (drop queued audio so silence
	// arrives promptly), on resume (drop buffered silence so playback
	// resumes at the live edge) and on stop (leave nothing behind).
	// Backends may keep a small buffer of their own that cannot be recalled.
	Flush() error
	// Underflow reports whether the backend inserted silence because the
	// producer fell behind since Open.
	Underflow() bool
	// Close stops playback and releases the device.
	Close() error
}
