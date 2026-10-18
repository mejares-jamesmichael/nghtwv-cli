//go:build linux

// Package platform resolves OS-dependent filesystem paths: the runtime
// directory holding the IPC socket and launch lock, and the endpoint path.
package platform

import (
	"os"
	"path/filepath"
)

// RuntimeDir returns the nghtwv runtime directory ($XDG_RUNTIME_DIR/nghtwv,
// falling back to the system temp dir), creating it with 0700 when missing.
// Parity: plaza-player's PLUGIN_DIR scheme.
func RuntimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "nghtwv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// EndpointPath returns the IPC socket path, creating the runtime dir.
func EndpointPath() (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mpv.sock"), nil
}

// LockPath returns the launch-lock file path, creating the runtime dir.
func LockPath() (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "player.lock"), nil
}

// CleanStaleEndpoint removes a dead socket file left behind by a crashed
// mpv. Harmless when the file is already gone or the endpoint is alive —
// the supervisor unlinks only after a failed probe.
func CleanStaleEndpoint(path string) error {
	return os.Remove(path)
}
