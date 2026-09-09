package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withTestFISBCacheSettingsPath redirects fisbCacheSettingsPath at a temp
// file for the duration of one test - mirrors withTestAutoRecordSettingsPath.
func withTestFISBCacheSettingsPath(t *testing.T) {
	t.Helper()
	orig := fisbCacheSettingsPath
	fisbCacheSettingsPath = filepath.Join(t.TempDir(), "fisbcache-settings.json")
	t.Cleanup(func() { fisbCacheSettingsPath = orig })
}

func TestFISBCacheSettings_DefaultIsDisabled(t *testing.T) {
	s := DefaultFISBCacheSettings()
	if s.Enabled {
		t.Fatal("default settings must be disabled")
	}
	if s.PersistenceEnabled {
		t.Fatal("default settings must not enable persistence")
	}
	if s.ReplayEnabled {
		t.Fatal("default settings must not enable replay")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("default settings must validate cleanly: %v", err)
	}
}

func TestFISBCacheSettings_LoadMissingFileDegradesToDisabledDefaults(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	s := loadFISBCacheSettings()
	if s != DefaultFISBCacheSettings() {
		t.Errorf("expected defaults for a missing settings file, got %+v", s)
	}
}

func TestFISBCacheSettings_LoadCorruptFileDegradesToDefaults(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	if err := os.WriteFile(fisbCacheSettingsPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := loadFISBCacheSettings()
	if s != DefaultFISBCacheSettings() {
		t.Errorf("expected defaults for a corrupt settings file, got %+v", s)
	}
}

func TestFISBCacheSettings_ValidateRejectsReplayEnabled(t *testing.T) {
	s := DefaultFISBCacheSettings()
	s.ReplayEnabled = true
	if err := s.Validate(); err == nil {
		t.Fatal("expected replayEnabled=true to be rejected - no product's replay path is proven safe in this build")
	}
}

func TestFISBCacheSettings_ValidateRejectsOutOfBoundValues(t *testing.T) {
	cases := []FISBCacheSettings{
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxCacheBytes = 0; return s }(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxCacheBytes = -1; return s }(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxCacheBytes = 1 << 40; return s }(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxEntries = 0; return s }(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxEntries = -1; return s }(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxEntries = 1000000; return s }(),
	}
	for i, s := range cases {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: expected rejection for %+v", i, s)
		}
	}
}

func TestFISBCacheSettings_SaveRejectsInvalidSettings(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	bad := DefaultFISBCacheSettings()
	bad.MaxEntries = -1
	if err := saveFISBCacheSettings(bad); err == nil {
		t.Fatal("saveFISBCacheSettings accepted invalid settings")
	}
	if _, err := os.Stat(fisbCacheSettingsPath); !os.IsNotExist(err) {
		t.Fatal("an invalid settings value must never be partially written")
	}
}

func TestFISBCacheSettings_SaveThenLoadRoundTrips(t *testing.T) {
	withTestFISBCacheSettingsPath(t)
	s := DefaultFISBCacheSettings()
	s.Enabled = true
	s.PersistenceEnabled = true
	s.MaxEntries = 500
	if err := saveFISBCacheSettings(s); err != nil {
		t.Fatalf("saveFISBCacheSettings: %v", err)
	}
	got := loadFISBCacheSettings()
	if got != s {
		t.Errorf("round-trip mismatch: saved %+v, loaded %+v", s, got)
	}
}
