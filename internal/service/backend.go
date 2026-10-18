package service

import (
	"context"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// Backend is the semantic surface the server dispatches onto. It is
// exactly *player.Player's method set — the mpv-backed player today, the
// built-in audio engine behind the same seam next — declared here (the
// consumer side) so server tests drive fakes instead of a live backend.
type Backend interface {
	Status(ctx context.Context) player.State
	Play(ctx context.Context) (player.State, error)
	Pause(ctx context.Context) (player.State, error)
	Toggle(ctx context.Context) (player.State, error)
	Stop(ctx context.Context) error
	Volume(ctx context.Context, volume int) (player.State, error)
	Outputs(ctx context.Context) ([]mpv.Device, error)
	SetOutput(ctx context.Context, device string) (player.State, error)
}

// SpectrumProvider is the optional backend extension behind the "spectrum"
// verb: backends with a real sample tap (the engine) implement it, the mpv
// backend does not and the server answers CodeUnsupported.
type SpectrumProvider interface {
	// Spectrum returns the latest band levels; it does no work beyond a
	// copy, so callers may poll it at frame cadence.
	Spectrum() audio.Snapshot
}
