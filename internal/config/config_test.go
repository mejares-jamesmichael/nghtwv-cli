package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// isolate points the config indirection at a fresh temp dir for the test.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := osUserConfigDir
	osUserConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { osUserConfigDir = previous })
	return dir
}

func volumeOf(n int) *int { return &n }

func TestLoadMissingFileDefaults(t *testing.T) {
	isolate(t)
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Output != "" || s.Volume != nil {
		t.Fatalf("default settings = %+v, want zero", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	isolate(t)
	in := Settings{Output: "pipewire/alsa_output.pci-0000:00:1f.3", Volume: volumeOf(42)}
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Output != in.Output || out.Volume == nil || *out.Volume != 42 {
		t.Fatalf("round trip got %+v, want %+v", out, in)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := isolate(t)
	if err := Save(Settings{Output: "auto-replaced"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("temp file persisted: %v", files)
	}
}

func TestLoadMalformedIsDefaults(t *testing.T) {
	dir := isolate(t)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Output != "" || s.Volume != nil {
		t.Fatalf("malformed file should yield defaults, got %+v", s)
	}
}

func TestLoadSanitizesInvalidOutput(t *testing.T) {
	dir := isolate(t)
	path := filepath.Join(dir, "settings.json")
	blob, _ := json.Marshal(Settings{Output: "bad name with space\n"})
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Output != "" {
		t.Fatalf("invalid remembered output should sanitize to \"\", got %q", s.Output)
	}
}

func TestSaveRejectsInvalidOutput(t *testing.T) {
	isolate(t)
	err := Save(Settings{Output: "bad name"})
	if err == nil {
		t.Fatal("Save should reject device names containing whitespace")
	}
}

func TestValidDeviceName(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"", false},
		{"auto", true},
		{"pipewire/alsa_output.pci-0000:00:1f.3.analog-stereo", true},
		{"wasapi/{0.0.0.00000000}.{d3e8b3e9-4c62-4bd8-a4f3-e5cb4a5c9b34}", true},
		{"name with space", false},
		{"tab\there", false},
		{"line\nbreak", false},
		{string(make([]byte, 201)), false},
	}
	for _, c := range cases {
		if got := ValidDeviceName(c.name); got != c.valid {
			t.Errorf("ValidDeviceName(%q) = %v, want %v", c.name, got, c.valid)
		}
	}
}
