package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

// TestFISBCacheSettings_ValidateAcceptsExactBoundaryValues proves the
// bounds in Validate() are correctly inclusive/exclusive at their exact
// edges (1 and 256 MiB / 100000 pass; 0 and 256 MiB+1 / 100001 do not,
// covered separately above) - a boundary condition is exactly where an
// off-by-one is most likely to hide.
func TestFISBCacheSettings_ValidateAcceptsExactBoundaryValues(t *testing.T) {
	cases := []FISBCacheSettings{
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxCacheBytes = 1; return s }(),
		func() FISBCacheSettings {
			s := DefaultFISBCacheSettings()
			s.MaxCacheBytes = 256 * 1024 * 1024
			return s
		}(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxEntries = 1; return s }(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxEntries = 100000; return s }(),
	}
	for i, s := range cases {
		if err := s.Validate(); err != nil {
			t.Errorf("case %d: expected the exact boundary value to be accepted, got error: %v (%+v)", i, err, s)
		}
	}
}

// TestFISBCacheSettings_ValidateRejectsJustOverBoundary proves the
// bound is strict, not off-by-one in the permissive direction.
func TestFISBCacheSettings_ValidateRejectsJustOverBoundary(t *testing.T) {
	cases := []FISBCacheSettings{
		func() FISBCacheSettings {
			s := DefaultFISBCacheSettings()
			s.MaxCacheBytes = 256*1024*1024 + 1
			return s
		}(),
		func() FISBCacheSettings { s := DefaultFISBCacheSettings(); s.MaxEntries = 100001; return s }(),
	}
	for i, s := range cases {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: expected rejection one past the boundary, got acceptance for %+v", i, s)
		}
	}
}

// TestHandleSetFISBCacheSettings_IntegerOverflowInRequestBodyRejected
// proves a JSON number that does not fit in the field's Go type is
// rejected as a decode error by encoding/json's own overflow detection
// (it never silently truncates or wraps) - end to end, through the real
// HTTP handler, not just asserted about the standard library in the
// abstract.
func TestHandleSetFISBCacheSettings_IntegerOverflowInRequestBodyRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	body := []byte(`{"enabled":true,"maxCacheBytes":99999999999999999999999999999,"maxEntries":10}`)
	req := httptest.NewRequest(http.MethodPost, "/setFISBCacheSettings", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleSetFISBCacheSettingsRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a maxCacheBytes value that overflows int64, got %d: %s", rec.Code, rec.Body.String())
	}
	if loadFISBCacheSettings().Enabled {
		t.Error("an overflow-rejected update must never have been persisted")
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
