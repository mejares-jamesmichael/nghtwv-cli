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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/bubbletea/v2"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/config"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/platform"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/plaza"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/service"
	"github.com/mejares-jamesmichael/nghtwv-cli/internal/tui"
)

// version is injected at build time:
//
//	go build -trimpath -ldflags "-s -w -X main.version=v0.1.0" .
var version = "dev"

const usageText = `Usage: nghtwv [command]

Without a command, nghtwv runs the interactive panel (needs a terminal).

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
  stop          stop playback and shut down the player service
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
		return runTUI(stdout, stderr)
	}

	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "version":
		return cmdVersion(rest, stdout, stderr)
	case "service":
		return cmdService(rest, stdout, stderr)
	case "status":
		return clientCmd(rest, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return cl.StatusChecked(ctx)
		})
	case "play":
		return clientCmd(rest, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return cl.Play(ctx)
		})
	case "pause":
		return clientCmd(rest, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return cl.Pause(ctx)
		})
	case "toggle":
		return clientCmd(rest, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return cl.Toggle(ctx)
		})
	case "stop":
		return clientCmd(rest, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return nil, cl.Stop(ctx)
		})
	case "volume":
		n, uerr := parseVolume(rest)
		if uerr != "" {
			fmt.Fprintln(stderr, uerr)
			fmt.Fprint(stderr, usageText)
			return 2
		}
		return clientCmd(nil, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return cl.Volume(ctx, n)
		})
	case "outputs":
		return clientCmd(rest, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			devices, err := cl.Outputs(ctx)
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
		return clientCmd(nil, stdout, stderr, func(ctx context.Context, cl *service.Client) (any, error) {
			return cl.SetOutput(ctx, device)
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

// clientCmd wires a fresh service client and executes fn, writing fn's
// result as JSON (or an empty success when nil result and nil error).
func clientCmd(args []string, stdout, stderr io.Writer, fn func(ctx context.Context, cl *service.Client) (any, error)) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "this command takes no arguments")
		fmt.Fprint(stderr, usageText)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cl, err := newPlayback()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	result, err := fn(ctx, cl)
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

// cmdService runs the hidden service verb in the foreground: one process
// owns the audio backend and serves clients until a frozen stop request or
// a signal shuts it down. Detached spawns (service.startService) point
// here; stdout/stderr are the void in that case, so nothing is printed on
// the happy path.
func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "the service command takes no arguments")
		return 2
	}
	endpoint, err := platform.ServiceEndpointPath()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// Double start (or a lost spawn race): if something already answers the
	// endpoint, this instance exits quietly instead of stealing it.
	if service.Probe(context.Background(), endpoint) {
		return 0
	}
	backend, err := newBackend()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	ln, err := service.Listen(endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "listen %s: %v\n", endpoint, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.NewServer(backend, ln).Serve(ctx); err != nil {
		fmt.Fprintf(stderr, "service: %v\n", err)
		return 1
	}
	// Leave no socket file behind; a signal shutdown leaves playback alone
	// (a later start re-attaches), while a stop request already stopped it.
	_ = platform.CleanStaleEndpoint(endpoint)
	return 0
}

// tuiDebugProgram wraps the panel to trace every message (NGHTWV_DEBUG=1
// probe tracing, like the IPC layer). It keeps the live model value so
// folds accumulate exactly like the plain program.
type tuiDebugProgram struct {
	inner tea.Model
	f     *os.File
}

func (d *tuiDebugProgram) Init() tea.Cmd { return d.inner.Init() }

func (d *tuiDebugProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if d.f != nil {
		if k, ok := msg.(tea.KeyPressMsg); ok {
			fmt.Fprintf(d.f, "KEY %q code=%d text=%q\n", k.String(), k.Code, k.Text)
		} else {
			fmt.Fprintf(d.f, "%T\n", msg)
		}
	}
	next, cmd := d.inner.Update(msg)
	d.inner = next
	return d, cmd
}

func (d *tuiDebugProgram) View() tea.View {
	v := d.inner.View()
	if d.f != nil {
		if m, ok := d.inner.(tui.Model); ok {
			fmt.Fprintf(d.f, "VIEW pickerOpen=%v devices=%d loaded=%v err=%q help=%v\n",
				m.PickerOpen(), len(m.Devices()), m.OutputsLoaded(), m.OutputsErr(), m.HelpOpen())
		}
	}
	return v
}

// runTUI boots the panel model over the real seams. A TTY is required
// (the panel needs raw input); non-interactive invocations get a friendly
// one-shot pointer instead of a raw error.
func runTUI(stdout, stderr io.Writer) int {
	cl, err := newPlayback()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	model := tui.New(cl, newPlaza())
	var program *tea.Program
	if os.Getenv("NGHTWV_DEBUG") != "" {
		f, ferr := os.CreateTemp("", "nghtwv-tui-debug-*")
		if ferr == nil {
			defer f.Close()
			fmt.Fprintln(os.Stderr, "tui probe log:", f.Name())
			program = tea.NewProgram(&tuiDebugProgram{inner: model, f: f})
		}
	}
	if program == nil {
		program = tea.NewProgram(model)
	}
	_, runErr := program.Run()
	if runErr == nil {
		return 0
	}
	if strings.Contains(runErr.Error(), "error opening TTY") {
		fmt.Fprintln(stderr, "nghtwv without a command wants a terminal to show the panel;")
		fmt.Fprintln(stderr, "for one-shot JSON use: nghtwv {status|metadata|history|backgrounds|outputs|output [DEV]|toggle|play|stop|volume N}")
		return 1
	}
	fmt.Fprintf(stderr, "tui: %v\n", runErr)
	return 1
}

// Backend selection and the test-only stream override. mpv is the default on
// every platform until the engine's per-OS runtime gate passes; the engine
// is opt-in through the environment, and because the backend belongs to the
// service process, changing it takes effect after `nghtwv stop`.
const (
	backendEnv = "NGHTWV_BACKEND"
	streamEnv  = "NGHTWV_STREAM_URL"

	// engineRate is the engine's render rate: the output negotiates this
	// rate and PlazaOpener resamples the stream when it differs.
	engineRate = 44100
)

// newPlayback resolves the service endpoint and lock and wires the client
// the one-shots and the panel drive. The service is attached — or started
// detached — on the first verb that needs it.
func newPlayback() (*service.Client, error) {
	if _, err := backendName(); err != nil {
		return nil, err
	}
	endpoint, err := platform.ServiceEndpointPath()
	if err != nil {
		return nil, fmt.Errorf("resolve service endpoint: %w", err)
	}
	lock, err := platform.ServiceLockPath()
	if err != nil {
		return nil, fmt.Errorf("resolve service lock: %w", err)
	}
	return service.NewClient(service.Config{
		Endpoint: endpoint,
		LockPath: lock,
		Clean:    func() { _ = platform.CleanStaleEndpoint(endpoint) },
	}), nil
}

// backendName validates the NGHTWV_BACKEND selection (empty means mpv).
// Both the service and the clients validate it, so a typo reports the same
// error instead of surfacing as an unexplained failed handshake.
func backendName() (string, error) {
	switch name := os.Getenv(backendEnv); name {
	case "":
		return "mpv", nil
	case "mpv", "engine":
		return name, nil
	default:
		return "", fmt.Errorf("unknown %s %q: use \"engine\" or \"mpv\"", backendEnv, name)
	}
}

// newBackend wires the backend the service dispatches onto.
func newBackend() (service.Backend, error) {
	name, err := backendName()
	if err != nil {
		return nil, err
	}
	if name == "engine" {
		return newEngineBackend(), nil
	}
	return newMPVBackend()
}

// newMPVBackend wires the parity backend: the mpv-backed player. The
// remembered device is passed to the mpv spawn config so it starts on it
// (parity: --audio-device at launch); if the device vanished the player
// falls back to auto and a later SetOutput fixes the record.
func newMPVBackend() (*player.Player, error) {
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
		StreamURL:   streamURL(),
		AudioDevice: settings.Output,
	}, mpv.NewDialer(endpoint))
	return player.New(sup.Client(), sup, config.Store{}), nil
}

// newEngineBackend wires the built-in engine: the Plaza stream decoded
// in-process, Plaza metadata as the reported title (the engine has no
// media-title property), and backend connections created lazily — the
// service starts without a sound server, and a device switch opens its own
// connection rather than reusing one the engine still writes into.
func newEngineBackend() service.Backend {
	newOut := func() (audio.Output, error) { return audio.NewOutput(engineRate) }
	return player.NewEnginePlayer(newOut, engineRate, audio.PlazaOpener(streamURL()), config.Store{}, newPlaza())
}

// streamURL is the Plaza stream, overridable for tests (NGHTWV_STREAM_URL):
// reconnect and buffering are hard to exercise against a healthy stream, but
// trivial against a bogus host.
func streamURL() string {
	if v := strings.TrimSpace(os.Getenv(streamEnv)); v != "" {
		return v
	}
	return player.StreamURL
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
