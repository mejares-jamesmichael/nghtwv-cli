// Package config persists remembered settings (output device, volume)
// atomically under the platform config dir.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unicode"
)

// ValidDeviceName reports whether name is a safe device identifier. mpv
// device names vary by platform (pipewire/…, wasapi/{GUID}…), so this is a
// safety check against escaping the settings JSON or command line — not the
// plugin's pactl sink charset — and any name must additionally exist in
// audio-device-list to be applied live.
func ValidDeviceName(name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Settings are the persisted user preferences.
type Settings struct {
	// Output is the raw mpv device name ("auto" is never stored; "" means
	// system default).
	Output string `json:"output,omitempty"`
	// Volume is persisted only once the user explicitly sets it; nil keeps
	// the spawn default (70).
	Volume *int `json:"volume,omitempty"`
}

// osUserConfigDir is the indirection tests use to swap the config root.
var osUserConfigDir = os.UserConfigDir

// Dir returns and creates the settings directory.
func Dir() (string, error) {
	base, err := osUserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "nghtwv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func settingsPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// Load reads settings. Parity: a missing or malformed file behaves as
// defaults, not an error.
func Load() (Settings, error) {
	path, err := settingsPath()
	if err != nil {
		return Settings{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, nil
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, nil
	}
	if s.Output != "" && !ValidDeviceName(s.Output) {
		s.Output = ""
	}
	return s, nil
}

// Save writes settings atomically (temp file + rename, parity requirement).
func Save(s Settings) error {
	if s.Output != "" && !ValidDeviceName(s.Output) {
		return fmt.Errorf("invalid output device name: %q", s.Output)
	}
	path, err := settingsPath()
	if err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
