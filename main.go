// Command nghtwv plays Nightwave Plaza radio in a terminal.
//
// Run without arguments to start the interactive TUI (planned phase 4).
// One-shot subcommands print machine-readable JSON on stdout and report
// failures on stderr with a non-zero exit code: 1 for runtime errors,
// 2 for usage errors.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/platform"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
)

// version is injected at build time:
//
//	go build -trimpath -ldflags "-s -w -X main.version=v0.1.0" .
var version = "dev"

const usageText = `Usage: nghtwv [command]

Without a command, nghtwv runs the interactive TUI (not implemented yet; planned phase 4).

Commands:
  status        print playback status as JSON
  metadata      print now-playing metadata from the Plaza API as JSON
  history       print up to 10 recent tracks as JSON
  backgrounds   print a random background pick as JSON
  outputs       list audio output devices as JSON
  output [DEV]  change or clear the remembered output device ("" = auto)
  play          start the stream or resume it
  pause         pause playback
  toggle        start/pause playback
  stop          stop playback and shut down mpv
  volume N      set the volume, clamped to 0-100
  version       print the binary version as JSON
  help          show this help

Exit codes: 0 success, 1 runtime error, 2 usage error.
`

type versionInfo struct {
	Version string `json:"version"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a command and returns the process exit code so tests
// can exercise every path without spawning the binary.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "the TUI is not implemented yet (planned phase 4)")
		fmt.Fprint(stderr, usageText)
		return 1
	}

	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "version":
		return cmdVersion(rest, stdout, stderr)
	case "status":
		return playerCmd(rest, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return pl.Status(ctx), nil
		})
	case "play":
		return playerCmd(rest, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return pl.Play(ctx)
		})
	case "pause":
		return playerCmd(rest, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return pl.Pause(ctx)
		})
	case "toggle":
		return playerCmd(rest, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return pl.Toggle(ctx)
		})
	case "stop":
		return playerCmd(rest, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return nil, pl.Stop(ctx)
		})
	case "volume":
		n, uerr := parseVolume(rest)
		if uerr != "" {
			fmt.Fprintln(stderr, uerr)
			fmt.Fprint(stderr, usageText)
			return 2
		}
		return playerCmd(nil, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return pl.Volume(ctx, n)
		})
	case "outputs":
		return playerCmd(rest, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			devices, err := pl.Outputs(ctx)
			if err != nil {
				return nil, err
			}
			return struct {
				Outputs []outputEntry `json:"outputs"`
			}{toOutputEntries(devices)}, nil
		})
	case "output":
		device, uerr := parseOutputArg(rest)
		if uerr != "" {
			fmt.Fprintln(stderr, uerr)
			fmt.Fprint(stderr, usageText)
			return 2
		}
		return playerCmd(nil, stdout, stderr, func(ctx context.Context, pl *player.Player) (any, error) {
			return pl.SetOutput(ctx, device)
		})
	case "metadata":
		return plazaCmd(rest, stdout, stderr, func(ctx context.Context, c *plaza.Client) (any, error) {
			return c.Metadata(ctx)
		})
	case "history":
		return plazaCmd(rest, stdout, stderr, func(ctx context.Context, c *plaza.Client) (any, error) {
			return c.History(ctx)
		})
	case "backgrounds":
		return plazaCmd(rest, stdout, stderr, func(ctx context.Context, c *plaza.Client) (any, error) {
			return c.Backgrounds(ctx)
		})
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", name)
		fmt.Fprint(stderr, usageText)
		return 2
	}
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "the version command takes no arguments")
		fmt.Fprintln(stderr)
		fmt.Fprint(stderr, usageText)
		return 2
	}
	b, err := json.Marshal(versionInfo{Version: version})
	if err != nil {
		fmt.Fprintf(stderr, "version encode: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

func parseVolume(args []string) (int, string) {
	if len(args) != 1 {
		return 0, "the volume command takes exactly one argument: N"
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Sprintf("volume must be a number, got %q", args[0])
	}
	return n, ""
}

func parseOutputArg(args []string) (string, string) {
	if len(args) > 1 {
		return "", "the output command takes at most one argument: [DEVICE]"
	}
	if len(args) == 1 {
		return args[0], ""
	}
	return "", ""
}

// outputEntry is the CLI shape of an audio output (parity with
// plaza-player outputs: {"outputs": [{"id", "label"}]}).
type outputEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func toOutputEntries(devices []mpv.Device) []outputEntry {
	entries := make([]outputEntry, 0, len(devices))
	for _, d := range devices {
		label := strings.TrimSpace(d.Description)
		if label == "" {
			label = d.Name
		}
		entries = append(entries, outputEntry{ID: d.Name, Label: label})
	}
	return entries
}

// newPlaza binds the API client to the versioned User-Agent.
func newPlaza() *plaza.Client {
	return plaza.New("nghtwv/" + version)
}

// plazaCmd wires the API client and executes fn, writing fn's result as
// JSON or the parity error message on stderr (exit 1).
func plazaCmd(args []string, stdout, stderr io.Writer, fn func(ctx context.Context, c *plaza.Client) (any, error)) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "this command takes no arguments")
		fmt.Fprint(stderr, usageText)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := fn(ctx, newPlaza())
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if result == nil {
		return 0
	}
	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintf(stderr, "encode: %v\n", err)
		return 1
	}
	return 0
}

// playerCmd wires a fresh player for the endpoint and executes fn, writing
// fn's result as JSON (or an empty success when nil result and nil error).
func playerCmd(args []string, stdout, stderr io.Writer, fn func(ctx context.Context, pl *player.Player) (any, error)) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "this command takes no arguments")
		fmt.Fprint(stderr, usageText)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pl, err := newPlayer()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	result, err := fn(ctx, pl)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if result == nil {
		return 0
	}
	if err := writeJSON(stdout, result); err != nil {
		fmt.Fprintf(stderr, "encode: %v\n", err)
		return 1
	}
	return 0
}

// newPlayer resolves runtime paths and wires supervisor + settings store +
// player. The remembered device is passed to the spawn config so mpv starts
// on it (parity: --audio-device at launch); if the device vanished the
// player falls back to auto and any later SetOutput fixes the record.
func newPlayer() (*player.Player, error) {
	endpoint, err := platform.EndpointPath()
	if err != nil {
		return nil, fmt.Errorf("resolve IPC endpoint: %w", err)
	}
	lock, err := platform.LockPath()
	if err != nil {
		return nil, fmt.Errorf("resolve launch lock: %w", err)
	}
	settings, err := config.Load()
	if err != nil {
		return nil, err
	}
	sup := mpv.NewSupervisor(mpv.ProcessConfig{
		Endpoint:    endpoint,
		LockPath:    lock,
		StreamURL:   player.StreamURL,
		AudioDevice: settings.Output,
	}, mpv.NewDialer(endpoint))
	return player.New(sup.Client(), sup, settingsStore{}), nil
}

// settingsStore adapts config's package functions to the player's store
// interface.
type settingsStore struct{}

func (settingsStore) Load() (config.Settings, error) { return config.Load() }
func (settingsStore) Save(s config.Settings) error   { return config.Save(s) }

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
