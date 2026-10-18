// Package service hosts the persistent nghtwv player: one long-lived
// process owns the audio backend and serves the CLI one-shots and the TUI
// over the platform IPC endpoint. Clients attach or start it under a
// launch lock — the mpv supervisor's process model with a semantic
// protocol in front, so the backend can move from mpv to the built-in
// audio engine without the CLI or the panel noticing.
package service

import (
	"encoding/json"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/mpv"
)

// Protocol is the wire version this build speaks. Bump it whenever a
// verb's request or reply shape changes incompatibly. Every revision must
// honour these compatibility rules:
//
//  1. Unknown request and reply fields are ignored, never rejected, so
//     fields can be added without a version bump.
//  2. Requests carry "proto"; a service refuses verbs from other versions
//     with CodeProtocolMismatch instead of guessing at their meaning.
//  3. Stop is frozen: {"id":N,"cmd":"stop"} is dispatched before the
//     version check, requires no other fields, and always means "stop
//     playback, release the backend, exit". It is the escape hatch from
//     rule 2: a client that cannot speak a running service's dialect can
//     still shut it down intentionally.
const Protocol = 1

// Command names (rule 3 freezes CmdStop's envelope, not its spelling).
const (
	CmdPing    = "ping"
	CmdStatus  = "status"
	CmdPlay    = "play"
	CmdPause   = "pause"
	CmdToggle  = "toggle"
	CmdStop    = "stop"
	CmdVolume  = "volume"
	CmdOutputs = "outputs"
	CmdOutput  = "output"
)

// Reply codes. An empty code means the error text came from the backend
// verb itself (e.g. "Unknown audio output").
const (
	CodeProtocolMismatch = "protocol_mismatch"
	CodeBadRequest       = "bad_request"
	CodeUnknownCommand   = "unknown_command"
	CodeBackend          = "backend"
)

// Request is one request line: newline-delimited JSON over the endpoint.
type Request struct {
	ID    int64           `json:"id"`
	Proto int             `json:"proto,omitempty"`
	Cmd   string          `json:"cmd"`
	Args  json.RawMessage `json:"args,omitempty"`
}

// Reply is one reply line. Result carries the verb's payload: a
// player.State for the transport verbs, an OutputsResult for outputs,
// nothing for stop.
type Reply struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Proto  int             `json:"proto"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// VolumeArgs is the "volume" payload.
type VolumeArgs struct {
	Value int `json:"value"`
}

// OutputArgs is the "output" payload ("" means system default).
type OutputArgs struct {
	Device string `json:"device"`
}

// OutputsResult is the "outputs" payload.
type OutputsResult struct {
	Outputs []mpv.Device `json:"outputs"`
}
