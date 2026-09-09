/*
fisbcachesettings.go: versioned, atomically-persisted settings for the
Rolling FIS-B Weather Cache - mirrors autorecordsettings.go's/
alertsettings.go's established temp-file/fsync/rename pattern exactly.
See docs/fisb-weather-cache.md's "Settings" section.
*/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// FISBCacheSettingsSchemaVersion is bumped whenever FISBCacheSettings
// gains, removes, or changes the meaning of a field a consumer should
// notice - mirrors autorecord.SettingsSchemaVersion's own convention.
const FISBCacheSettingsSchemaVersion = 1

// FISBCacheSettings is this feature's persisted, user-configurable
// settings - disabled by default (see DefaultFISBCacheSettings), exactly
// mirroring autorecord.Settings' own "disabled by default" posture.
type FISBCacheSettings struct {
	SchemaVersion int `json:"schemaVersion"`

	// Enabled is false by default - this feature never caches, persists,
	// or serves any FIS-B product unless explicitly turned on.
	Enabled bool `json:"enabled"`
	// PersistenceEnabled: when false (even if Enabled is true), the
	// in-memory Store still admits/tracks live products for dashboard
	// visibility, but nothing is ever written to persistent storage or
	// survives a restart. Separated from Enabled so an owner can trial
	// the feature without committing to disk usage.
	PersistenceEnabled bool `json:"persistenceEnabled"`
	// ReplayEnabled: always false in this release - see
	// docs/fisb-weather-cache.md's "Replay behavior" section for why
	// downstream GDL90 replay could not yet be proven safe. Present as
	// an explicit field (not simply absent) so a future release that
	// DOES prove a specific product safe to replay has a place to turn
	// it on without a settings-schema break, and so this document/schema
	// itself is honest that this decision was considered, not omitted.
	ReplayEnabled bool `json:"replayEnabled"`

	MaxCacheBytes int64 `json:"maxCacheBytes"`
	MaxEntries    int   `json:"maxEntries"`
}

// DefaultFISBCacheSettings returns the safe, disabled-by-default
// settings - the only values ever used before an owner explicitly
// configures this feature, and the fallback for a missing, corrupt, or
// invalid settings file (see loadFISBCacheSettings).
func DefaultFISBCacheSettings() FISBCacheSettings {
	return FISBCacheSettings{
		SchemaVersion:      FISBCacheSettingsSchemaVersion,
		Enabled:            false,
		PersistenceEnabled: false,
		ReplayEnabled:      false,
		MaxCacheBytes:      16 * 1024 * 1024, // 16 MiB - generous for many text reports and a modest number of NEXRAD tiles, small next to this device's typical persistent-storage headroom.
		MaxEntries:         2000,
	}
}

// Validate reports whether s is safe to persist/apply - mirrors
// autorecord.Settings.Validate's own bounded-values philosophy.
// ReplayEnabled=true is always rejected in this release: no product's
// replay path has been proven safe (see docs/fisb-weather-cache.md), so
// accepting this value would silently promise a capability this build
// does not actually provide.
func (s FISBCacheSettings) Validate() error {
	if s.ReplayEnabled {
		return fmt.Errorf("fisbcache: replayEnabled is not yet supported by this build - see docs/fisb-weather-cache.md")
	}
	if s.MaxCacheBytes <= 0 {
		return fmt.Errorf("fisbcache: maxCacheBytes must be positive")
	}
	if s.MaxCacheBytes > 256*1024*1024 {
		return fmt.Errorf("fisbcache: maxCacheBytes must not exceed 256 MiB")
	}
	if s.MaxEntries <= 0 {
		return fmt.Errorf("fisbcache: maxEntries must be positive")
	}
	if s.MaxEntries > 100000 {
		return fmt.Errorf("fisbcache: maxEntries must not exceed 100000")
	}
	return nil
}

// fisbCacheSettingsPath is a var, not a const, solely so tests can
// redirect it at a temp file for the duration of one test - mirrors
// autoRecordSettingsPath's identical pattern. Never reassigned in
// production.
var fisbCacheSettingsPath = PersistentDataPath + "/fisbcache-settings.json"

var fisbCacheSettingsMu sync.Mutex

// loadFISBCacheSettings reads fisbCacheSettingsPath. A missing or
// corrupt file, or one that fails validation, degrades safely to
// DefaultFISBCacheSettings() (logged, never fatal) - matches
// loadAutoRecordSettings's own documented fail-safe design exactly: a
// corrupt settings file must never prevent the daemon from starting, and
// must never leave this feature silently enabled by falling back to
// anything other than the documented (disabled) defaults.
func loadFISBCacheSettings() FISBCacheSettings {
	fisbCacheSettingsMu.Lock()
	defer fisbCacheSettingsMu.Unlock()
	data, err := os.ReadFile(fisbCacheSettingsPath)
	if err != nil {
		return DefaultFISBCacheSettings()
	}
	var s FISBCacheSettings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("fisbcache: settings file corrupt, using defaults: %s\n", err)
		return DefaultFISBCacheSettings()
	}
	if s.SchemaVersion != FISBCacheSettingsSchemaVersion {
		log.Printf("fisbcache: settings file schema mismatch (got %d, want %d), using defaults\n", s.SchemaVersion, FISBCacheSettingsSchemaVersion)
		return DefaultFISBCacheSettings()
	}
	if err := s.Validate(); err != nil {
		log.Printf("fisbcache: settings file failed validation, using defaults: %s\n", err)
		return DefaultFISBCacheSettings()
	}
	return s
}

// saveFISBCacheSettings validates and atomically persists s - temp file
// in the same directory, fsync, rename, matching saveAutoRecordSettings'
// established pattern exactly. An invalid update is rejected outright
// and never partially applied.
func saveFISBCacheSettings(s FISBCacheSettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	fisbCacheSettingsMu.Lock()
	defer fisbCacheSettingsMu.Unlock()

	dir := filepath.Dir(fisbCacheSettingsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal fisbcache settings: %w", err)
	}
	tmp := fisbCacheSettingsPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("could not create temp settings file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not write temp settings file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not sync temp settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not close temp settings file: %w", err)
	}
	if err := os.Rename(tmp, fisbCacheSettingsPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not finalize settings file: %w", err)
	}
	return nil
}
