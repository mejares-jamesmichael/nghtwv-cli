package config

// Store adapts the package-level Load/Save pair to the consumer-side store
// interfaces (internal/player, internal/service) so callers can take a
// settings dependency they can swap in tests.
type Store struct{}

// Load reads the persisted settings (missing/malformed files are
// defaults, never an error — see Load).
func (Store) Load() (Settings, error) { return Load() }

// Save writes the settings atomically.
func (Store) Save(s Settings) error { return Save(s) }
