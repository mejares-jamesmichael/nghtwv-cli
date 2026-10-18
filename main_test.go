package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", strings.TrimSpace(stderr.String()))
	}
	var got versionInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", stdout.String(), err)
	}
	if got.Version == "" {
		t.Error("version field is empty")
	}
}

func TestRunVersionRejectsArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage: nghtwv") {
		t.Error("help output should contain usage text")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Error("should report the unknown command")
	}
}

func TestRunNoArgsWithoutTTY(t *testing.T) {
	// In a test there is no terminal on stdin, so the TUI path must bail
	// out with a helpful pointer instead of hanging or crashing.
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1 (TUI needs a TTY)", code)
	}
	if !strings.Contains(stderr.String(), "terminal") {
		t.Errorf("non-TTY no-args run should point at the terminal requirement, stderr = %q", stderr.String())
	}
	for _, name := range []string{"status", "toggle", "volume"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("non-TTY message should list one-shots, missing %q: %q", name, stderr.String())
		}
	}
}

func TestRunPlazaCommandsRejectArgs(t *testing.T) {
	// The API commands take no arguments; extra words are usage errors
	// (exit 2) — no network touched on this path.
	for _, name := range []string{"metadata", "history", "backgrounds"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{name, "unexpected"}, &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit code = %d, want 2", name, code)
		}
		if !strings.Contains(stderr.String(), "this command takes no arguments") {
			t.Errorf("%s: should reject unexpected args, stderr = %q", name, stderr.String())
		}
	}
}

func TestRunVolumeArgParsing(t *testing.T) {
	// Bad volume grammar is a usage error (exit 2), before any player wiring.
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"volume"}, 2},
		{[]string{"volume", "nan"}, 2},
		{[]string{"volume", "1", "2"}, 2},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(c.args, &stdout, &stderr); code != c.code {
			t.Errorf("run(%v) = %d, want %d", c.args, code, c.code)
		}
	}
}

func TestRunOutputArgArity(t *testing.T) {
	// Too many arguments is a usage error before any wiring happens.
	// The zero-arg / one-arg cases need real IPC, so only the arity
	// violation is checked here.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"output", "a", "b"}, &stdout, &stderr); code != 2 {
		t.Fatalf("output with two args = %d, want 2", code)
	}
}
