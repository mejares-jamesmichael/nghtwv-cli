// Command nghtwv plays Nightwave Plaza radio in a terminal.
//
// Run without arguments to start the interactive TUI (planned phase 4).
// One-shot subcommands print machine-readable JSON on stdout and report
// failures on stderr with a non-zero exit code: 1 for runtime errors,
// 2 for usage errors.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
  output [DEV]  change or clear the remembered output device
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
	case "status", "metadata", "history", "backgrounds", "outputs", "output",
		"play", "pause", "toggle", "stop", "volume":
		fmt.Fprintf(stderr, "%q is not implemented yet (see the phased build plan)\n", name)
		return 1
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
