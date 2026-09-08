package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/autorecord"
)

// withTestAutoRecordSettingsPath redirects autoRecordSettingsPath at a temp
// file for the duration of one test - mirrors withTestAlertSettingsPath.
func withTestAutoRecordSettingsPath(t *testing.T) {
	t.Helper()
	orig := autoRecordSettingsPath
	autoRecordSettingsPath = filepath.Join(t.TempDir(), "autorecord-settings.json")
	t.Cleanup(func() { autoRecordSettingsPath = orig })
}

func TestAutoRecordSettings_LoadMissingFileDegradesToDisabledDefaults(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	s := loadAutoRecordSettings()
	if s != autorecord.DefaultSettings() {
		t.Errorf("expected defaults for a missing settings file, got %+v", s)
	}
	if s.Enabled {
		t.Error("a missing settings file must never degrade to enabled")
	}
}

func TestAutoRecordSettings_LoadCorruptFileDegradesToDefaults(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	if err := os.WriteFile(autoRecordSettingsPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := loadAutoRecordSettings()
	if s != autorecord.DefaultSettings() {
		t.Errorf("expected defaults for a corrupt settings file, got %+v", s)
	}
}

func TestAutoRecordSettings_LoadSchemaMismatchDegradesToDefaults(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	s := autorecord.DefaultSettings()
	s.SchemaVersion = autorecord.SettingsSchemaVersion + 1
	s.Enabled = true // must not survive the schema-mismatch fallback
	if err := saveAutoRecordSettingsBypassingValidation(t, s); err != nil {
		t.Fatal(err)
	}
	got := loadAutoRecordSettings()
	if got != autorecord.DefaultSettings() {
		t.Errorf("expected defaults for a schema-mismatched file, got %+v", got)
	}
}

func TestAutoRecordSettings_LoadInvalidSettingsDegradesToDefaults(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	s := autorecord.DefaultSettings()
	s.StopGroundspeedKnots = s.StartGroundspeedKnots // invalid: violates strict hysteresis
	s.Enabled = true
	if err := saveAutoRecordSettingsBypassingValidation(t, s); err != nil {
		t.Fatal(err)
	}
	got := loadAutoRecordSettings()
	if got != autorecord.DefaultSettings() {
		t.Errorf("expected defaults for an invalid persisted settings file, got %+v", got)
	}
}

func TestAutoRecordSettings_SaveRejectsInvalidSettings(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	bad := autorecord.DefaultSettings()
	bad.StartDwellSeconds = -1
	if err := saveAutoRecordSettings(bad); err == nil {
		t.Fatal("saveAutoRecordSettings accepted an invalid settings value")
	}
	if _, err := os.Stat(autoRecordSettingsPath); !os.IsNotExist(err) {
		t.Fatal("an invalid settings value must never be partially written")
	}
}

func TestAutoRecordSettings_SaveThenLoadRoundTrips(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	s := autorecord.DefaultSettings()
	s.Enabled = true
	s.StartGroundspeedKnots = 12
	s.StopGroundspeedKnots = 5
	if err := saveAutoRecordSettings(s); err != nil {
		t.Fatalf("saveAutoRecordSettings: %v", err)
	}
	got := loadAutoRecordSettings()
	if got != s {
		t.Errorf("round-trip mismatch: saved %+v, loaded %+v", s, got)
	}
}

func TestAutoRecordSettings_SaveIsAtomic(t *testing.T) {
	withTestAutoRecordSettingsPath(t)
	s := autorecord.DefaultSettings()
	if err := saveAutoRecordSettings(s); err != nil {
		t.Fatalf("saveAutoRecordSettings: %v", err)
	}
	if _, err := os.Stat(autoRecordSettingsPath + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file was not cleaned up after a successful save")
	}
}

// saveAutoRecordSettingsBypassingValidation writes s directly to
// autoRecordSettingsPath without going through saveAutoRecordSettings'
// own Validate() gate - used only to construct on-disk fixtures
// (schema-mismatched or otherwise invalid) that a real caller could never
// produce through the normal save path, so loadAutoRecordSettings' own
// defense-in-depth can be tested directly.
func saveAutoRecordSettingsBypassingValidation(t *testing.T, s autorecord.Settings) error {
	t.Helper()
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(autoRecordSettingsPath, data, 0o644)
}
