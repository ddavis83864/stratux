package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// withTestAlertSettingsPath redirects alertSettingsPath at a temp file for
// the duration of one test - mirrors main/recordingapi.go's
// withTestRecordingsDir pattern.
func withTestAlertSettingsPath(t *testing.T) {
	t.Helper()
	orig := alertSettingsPath
	alertSettingsPath = filepath.Join(t.TempDir(), "alert-settings.json")
	t.Cleanup(func() { alertSettingsPath = orig })
}

func TestDefaultAlertSettings_IsValid(t *testing.T) {
	if err := DefaultAlertSettings().Validate(); err != nil {
		t.Errorf("DefaultAlertSettings() failed validation: %v", err)
	}
}

func TestDefaultAlertSettings_ConservativeDefaults(t *testing.T) {
	d := DefaultAlertSettings()
	if d.BrowserAudioEnabled {
		t.Error("BrowserAudioEnabled should default to false - requires an explicit user gesture")
	}
	if d.SystemAudioEnabled {
		t.Error("SystemAudioEnabled should default to false")
	}
	if d.Muted || d.MutedIndefinitely {
		t.Error("no active mute should exist by default")
	}
	if !d.MasterEnabled || !d.VisualTrafficNoticesEnabled || !d.SystemVisualEnabled {
		t.Error("visual notices and master alerting should default to enabled")
	}
}

func TestAlertSettings_LoadMissingFileDegradesToDefaults(t *testing.T) {
	withTestAlertSettingsPath(t)
	s := loadAlertSettings()
	if s.SchemaVersion != AlertSettingsSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", s.SchemaVersion, AlertSettingsSchemaVersion)
	}
	if s != DefaultAlertSettings() {
		t.Errorf("expected defaults for a missing settings file, got %+v", s)
	}
}

func TestAlertSettings_LoadCorruptFileDegradesToDefaults(t *testing.T) {
	withTestAlertSettingsPath(t)
	if err := os.WriteFile(alertSettingsPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := loadAlertSettings()
	if s != DefaultAlertSettings() {
		t.Errorf("expected defaults for a corrupt settings file, got %+v", s)
	}
}

func TestAlertSettings_SaveAndLoadRoundTrip(t *testing.T) {
	withTestAlertSettingsPath(t)
	s := DefaultAlertSettings()
	s.MasterEnabled = false
	s.AudioVolume = 0.75
	s.NoticeHorizontalNM = 2.5
	if err := saveAlertSettings(s); err != nil {
		t.Fatalf("saveAlertSettings: %v", err)
	}
	loaded := loadAlertSettings()
	if loaded.MasterEnabled != false || loaded.AudioVolume != 0.75 || loaded.NoticeHorizontalNM != 2.5 {
		t.Errorf("round-trip mismatch: got %+v", loaded)
	}
}

func TestAlertSettings_SaveIsAtomicNoTempFileLeftBehind(t *testing.T) {
	withTestAlertSettingsPath(t)
	if err := saveAlertSettings(DefaultAlertSettings()); err != nil {
		t.Fatalf("saveAlertSettings: %v", err)
	}
	if _, err := os.Stat(alertSettingsPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after a successful save: err=%v", err)
	}
}

func TestAlertSettings_SafeFilePermissions(t *testing.T) {
	withTestAlertSettingsPath(t)
	if err := saveAlertSettings(DefaultAlertSettings()); err != nil {
		t.Fatalf("saveAlertSettings: %v", err)
	}
	info, err := os.Stat(alertSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o002 != 0 {
		t.Errorf("settings file is world-writable: %v", info.Mode())
	}
}

func TestAlertSettings_ValidationRejectsNaNInfNegative(t *testing.T) {
	cases := []func(*AlertSettings){
		func(s *AlertSettings) { s.AudioVolume = math.NaN() },
		func(s *AlertSettings) { s.NoticeHorizontalNM = math.Inf(1) },
		func(s *AlertSettings) { s.CautionVerticalFeet = -1 },
		func(s *AlertSettings) { s.AudioVolume = 1.5 },
		func(s *AlertSettings) { s.MuteUntilUnixSeconds = -5 },
	}
	for i, mutate := range cases {
		s := DefaultAlertSettings()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: expected validation error, got nil (settings: %+v)", i, s)
		}
	}
}

func TestAlertSettings_ExcessiveMuteDurationRejected(t *testing.T) {
	s := DefaultAlertSettings()
	s.Muted = true
	s.MuteUntilUnixSeconds = 1 << 62 // absurdly far in the future
	if err := s.Validate(); err == nil {
		t.Error("expected an excessive mute duration to be rejected")
	}
}

// TestAlertSettings_LoadClearsOnlyStaleMuteKeepsOtherSettings reproduces a
// defect found during live hardware validation: on an RTC-less board, the
// wall clock reads a stale, far-earlier value until GPS trusted-time
// correction completes (see main/health.go). If loadAlertSettings runs in
// that window, a legitimately-short persisted mute can look "excessive"
// against the not-yet-corrected time.Now(), and previously caused the
// *entire* settings file - including the user's real thresholds and
// audio/visual toggles - to be discarded back to full defaults. The fix:
// a mute-only validation failure clears just the mute fields and keeps
// everything else the user actually configured.
func TestAlertSettings_LoadClearsOnlyStaleMuteKeepsOtherSettings(t *testing.T) {
	withTestAlertSettingsPath(t)
	s := DefaultAlertSettings()
	s.AudioVolume = 0.77
	s.SuppressGroundTraffic = true
	s.NoticeHorizontalNM = 2.5
	s.Muted = true
	s.MutedIndefinitely = false
	s.MuteUntilUnixSeconds = 1 << 62 // looks excessive against a stale "now"

	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alertSettingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAlertSettings()
	if got.Muted || got.MutedIndefinitely || got.MuteUntilUnixSeconds != 0 {
		t.Errorf("expected mute state to be cleared, got Muted=%v MutedIndefinitely=%v MuteUntilUnixSeconds=%d",
			got.Muted, got.MutedIndefinitely, got.MuteUntilUnixSeconds)
	}
	if got.AudioVolume != 0.77 || !got.SuppressGroundTraffic || got.NoticeHorizontalNM != 2.5 {
		t.Errorf("expected the user's other settings to survive a mute-only validation failure, got %+v", got)
	}
}

// TestAlertSettings_LoadStillDefaultsWhenNonMuteFieldAlsoInvalid confirms
// the mute-clearing recovery in loadAlertSettings does not mask a genuinely
// invalid non-mute field - the whole file still degrades to defaults in
// that case, exactly as before this fix.
func TestAlertSettings_LoadStillDefaultsWhenNonMuteFieldAlsoInvalid(t *testing.T) {
	withTestAlertSettingsPath(t)
	s := DefaultAlertSettings()
	s.CautionHorizontalNM = -1 // independently invalid
	s.Muted = true
	s.MuteUntilUnixSeconds = 1 << 62

	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alertSettingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAlertSettings()
	if got != DefaultAlertSettings() {
		t.Errorf("expected full defaults when a non-mute field is also invalid, got %+v", got)
	}
}

func TestAlertSettings_SaveRejectsInvalidSettings(t *testing.T) {
	withTestAlertSettingsPath(t)
	s := DefaultAlertSettings()
	s.CautionHorizontalNM = math.NaN()
	if err := saveAlertSettings(s); err == nil {
		t.Error("expected saveAlertSettings to reject an invalid config")
	}
	if _, err := os.Stat(alertSettingsPath); !os.IsNotExist(err) {
		t.Error("an invalid save must not create a settings file at all")
	}
}

func TestAlertSettings_ConcurrentReadersWriters(t *testing.T) {
	withTestAlertSettingsPath(t)
	if err := saveAlertSettings(DefaultAlertSettings()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = loadAlertSettings()
				s := DefaultAlertSettings()
				s.AudioVolume = float64(n) / 10.0
				saveAlertSettings(s)
			}
		}(i)
	}
	wg.Wait()
	// No panic/race and the file must still be validly loadable afterward.
	final := loadAlertSettings()
	if err := final.Validate(); err != nil {
		t.Errorf("settings file left in an invalid state after concurrent access: %v", err)
	}
}

func TestToAlertingConfig_UnitConversion(t *testing.T) {
	s := DefaultAlertSettings()
	s.NoticeHorizontalNM = 2.0
	cfg := toAlertingConfig(s, true, true)
	want := 2.0 * 1852.0
	if cfg.NoticeHorizontalMeters != want {
		t.Errorf("NoticeHorizontalMeters = %v, want %v", cfg.NoticeHorizontalMeters, want)
	}
}
