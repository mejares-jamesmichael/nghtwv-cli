package service

import (
	"context"

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
