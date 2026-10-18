//go:build windows

package platform

import (
	"os"
	"path/filepath"
)

// PipeName is nghtwv's IPC named pipe. mpv accepts any pipe path here; a
// fixed point in the global namespace keeps later invocations attachable.
const PipeName = `\\.\pipe\nghtwv-mpv`

// ConfigDir returns os.UserConfigDir()/nghtwv, creating it when missing.
// Windows has no XDG runtime dir; the config dir doubles as the lock home.
func ConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "nghtwv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// EndpointPath returns the named pipe address.
func EndpointPath() (string, error) {
	return PipeName, nil
}

// LockPath returns the launch-lock file path under the config dir.
func LockPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "player.lock"), nil
}

// CleanStaleEndpoint is a no-op on Windows: dead named pipes leave no
// filesystem artifact.
func CleanStaleEndpoint(string) error {
	return nil
}
