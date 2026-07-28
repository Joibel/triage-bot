package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Joibel/triage-bot/internal/lockfile"
)

// Load reads and parses the status file. It takes no lock: Save writes
// atomically via os.Rename, so a concurrent reader always sees either the old
// or the new complete file, never a torn one.
//
// Zero-valued settings are filled from DefaultSettings, so a hand-written file
// need only name the knobs it wants to change.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is from a command-line flag
	if err != nil {
		return nil, fmt.Errorf("failed to read status file: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse status file: %w", err)
	}
	if c.Version > SchemaVersion {
		return nil, fmt.Errorf(
			"status file is schema version %d but this build understands only %d - upgrade triage-bot",
			c.Version, SchemaVersion)
	}
	c.applyDefaults()
	return &c, nil
}

// applyDefaults fills unset settings. Done on load rather than at use sites so
// there is exactly one place defaults live.
func (c *Config) applyDefaults() {
	d := DefaultSettings()
	if c.Version == 0 {
		c.Version = SchemaVersion
	}
	if c.Settings.MaxOpenBeads == 0 {
		c.Settings.MaxOpenBeads = d.MaxOpenBeads
	}
	if c.Settings.RetriageAfter == 0 {
		c.Settings.RetriageAfter = d.RetriageAfter
	}
	if c.Settings.MaxTemplateAttempts == 0 {
		c.Settings.MaxTemplateAttempts = d.MaxTemplateAttempts
	}
	if c.Settings.BeadLabel == "" {
		c.Settings.BeadLabel = d.BeadLabel
	}
}

// Save writes the config atomically: marshal, write a temp file in the same
// directory, fsync it, then os.Rename over the destination. The rename is
// atomic on POSIX filesystems, so readers never observe a partial write.
func Save(path string, c *Config) error {
	c.sortItems()
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal status file: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".triage-bot-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we fail before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename temp file into place: %w", err)
	}

	// Best-effort fsync of the directory so the rename is durable.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // dir is the status file's own directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Update is the transactional primitive every writer uses. It acquires the
// exclusive writer lock, reloads the current on-disk state (so it picks up any
// change made since this process last read the file), applies mutate, and saves
// atomically. Reloading inside the lock is what prevents read-modify-write
// clobbering between the daemon and an interactive command.
func Update(path string, mutate func(*Config) error) error {
	lk, err := lockfile.Acquire(path)
	if err != nil {
		return fmt.Errorf("failed to lock status file: %w", err)
	}
	defer func() { _ = lk.Release() }()

	c, err := Load(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		c = &Config{}
		c.applyDefaults()
	}

	if err := mutate(c); err != nil {
		return err
	}

	return Save(path, c)
}
