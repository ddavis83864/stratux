/*
autorecordsettings.go: versioned, atomically-persisted settings for the
Automatic Flight Recording feature - mirrors alertsettings.go's established
temp-file/fsync/rename pattern exactly (see docs/automatic-flight-recording.md's
"Configuration and persistence" section).
*/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/stratux/stratux/autorecord"
)

// autoRecordSettingsPath is a var, not a const, solely so tests can
// redirect it at a temp file for the duration of one test - mirrors
// alertSettingsPath/recordingsDir's pattern. Never reassigned in
// production.
var autoRecordSettingsPath = PersistentDataPath + "/autorecord-settings.json"

var autoRecordSettingsMu sync.Mutex

// loadAutoRecordSettings reads autoRecordSettingsPath. A missing or
// corrupt file, or one that fails validation, degrades safely to
// autorecord.DefaultSettings() (logged, never fatal) - a corrupt settings
// file must never prevent the daemon from starting, and must never leave
// Automatic Flight Recording silently enabled by falling back to
// anything other than the documented (disabled) defaults.
func loadAutoRecordSettings() autorecord.Settings {
	autoRecordSettingsMu.Lock()
	defer autoRecordSettingsMu.Unlock()
	data, err := os.ReadFile(autoRecordSettingsPath)
	if err != nil {
		return autorecord.DefaultSettings()
	}
	var s autorecord.Settings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("autorecord: settings file corrupt, using defaults: %s\n", err)
		return autorecord.DefaultSettings()
	}
	if s.SchemaVersion != autorecord.SettingsSchemaVersion {
		log.Printf("autorecord: settings file schema mismatch (got %d, want %d), using defaults\n", s.SchemaVersion, autorecord.SettingsSchemaVersion)
		return autorecord.DefaultSettings()
	}
	if err := s.Validate(); err != nil {
		log.Printf("autorecord: settings file failed validation, using defaults: %s\n", err)
		return autorecord.DefaultSettings()
	}
	return s
}

// saveAutoRecordSettings validates and atomically persists s - temp file
// in the same directory, fsync, rename, matching saveAlertSettings'
// established pattern. An invalid update is rejected outright and never
// partially applied.
func saveAutoRecordSettings(s autorecord.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	autoRecordSettingsMu.Lock()
	defer autoRecordSettingsMu.Unlock()

	dir := filepath.Dir(autoRecordSettingsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal autorecord settings: %w", err)
	}
	tmp := autoRecordSettingsPath + ".tmp"
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
	if err := os.Rename(tmp, autoRecordSettingsPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not finalize settings file: %w", err)
	}
	return nil
}
