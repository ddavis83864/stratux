package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/autorecord"
	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/configbackup"
	"github.com/stratux/stratux/ota"
)

// withConfigBackupTestEnv wires a temp calibration-profile store, a temp
// alert-settings file, a temp autorecord-settings file, a temp
// fisbcache-settings file, a monotonic clock, and a clean restore-
// operation state machine for the duration of one test - mirrors
// withTestProfilesStore (calprofilesapi_test.go), withTestAlertSettingsPath
// (alertsettings_test.go), withTestAutoRecordSettingsPath
// (autorecordsettings_test.go), and withTestFISBCacheSettingsPath
// (fisbcachesettings_test.go), plus this subsystem's own state. Each
// settings-path redirect matters here specifically:
// gatherConfigBackupCurrentState/applyConfigBackupTransaction read/write
// all of them, which otherwise default to the real PersistentDataPath -
// without this, a configbackup test run on a real device or a developer
// machine with /var/lib/stratux-data present could read or write those
// real files.
func withConfigBackupTestEnv(t *testing.T) *calprofile.Store {
	t.Helper()
	store := withTestProfilesStore(t)
	withTestAlertSettingsPath(t)
	withTestAutoRecordSettingsPath(t)
	withTestFISBCacheSettingsPath(t)
	withFakePersistentStorageForTest(t)
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}

	origState := configBackupState
	origPending := configBackupPending
	origLast := configBackupLast
	origLastErr := configBackupLastErr
	origRestored := configBackupRestoredThisBoot
	configBackupMu.Lock()
	configBackupState = restoreStateIdle
	configBackupPending = nil
	configBackupLast = nil
	configBackupLastErr = ""
	configBackupRestoredThisBoot = false
	configBackupMu.Unlock()
	t.Cleanup(func() {
		configBackupMu.Lock()
		configBackupState = origState
		configBackupPending = origPending
		configBackupLast = origLast
		configBackupLastErr = origLastErr
		configBackupRestoredThisBoot = origRestored
		configBackupMu.Unlock()
	})

	origOTADir := otaDir
	t.Cleanup(func() { otaDir = origOTADir })
	otaDir = t.TempDir() // no staged/state file here -> ota.LoadState reports StageIdle

	// saveSettings() (called by applyConfigBackupTransaction) writes to
	// configLocation, which defaults to /boot/firmware/stratux.conf - not
	// writable by a test process, and not something a test should touch
	// even if it were. Worse than just failing: saveSettings()'s error
	// path calls addSingleSystemErrorf, which locks systemErrsMutex - a
	// *sync.Mutex only ever initialized inside main() (never called by
	// `go test`), so it's nil here and locking it hangs the test
	// (observed: a real deadlock, reproduced with `go test -timeout`).
	// Redirecting configLocation to a writable temp file keeps
	// saveSettings() on its normal, already-tested success path instead.
	origConfigLocation := configLocation
	t.Cleanup(func() { configLocation = origConfigLocation })
	configLocation = filepath.Join(t.TempDir(), "stratux.conf")

	// withTestProfilesStore hands back an empty store - seed one profile
	// and activate it, matching every real device's post-migration state
	// (see calprofile.EnsureMigrated), so tests exercise a realistic
	// "already has a profile" starting point rather than an empty store.
	seed := calprofile.Profile{
		ID:               calprofile.NewID(),
		Name:             "Current Installation",
		IMUMapping:       [2]int{-1, 0},
		SensorQuaternion: [4]float64{0.1, 0.2, 0.3, 0.9},
		D:                [3]float64{1, 2, 3},
		Kind:             calprofile.KindMigrated,
		SchemaVersion:    calprofile.SchemaVersion,
		CreatedAt:        time.Now().UTC(),
		ModifiedAt:       time.Now().UTC(),
	}
	seed.RecomputeValidity()
	if err := store.Save(seed); err != nil {
		t.Fatalf("could not seed a test calibration profile: %v", err)
	}
	if err := store.SetActiveID(seed.ID, time.Now().UTC()); err != nil {
		t.Fatalf("could not activate the seeded test calibration profile: %v", err)
	}
	applyProfileToGlobalSettingsLocked(seed)

	return store
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("could not decode response body %q: %v", rec.Body.String(), err)
	}
	return m
}

// --- download -----------------------------------------------------

func TestHandleDownloadConfigurationBackup_OK(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup", nil)
	rec := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Error("expected a Content-Disposition header naming the sanitized filename")
	}
	var doc configbackup.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response body is not a valid Document: %v", err)
	}
	res := configbackup.Validate(doc, rec.Body.Len())
	if !res.OK() {
		t.Fatalf("downloaded backup failed its own validation: %v", res.Errors)
	}
	if len(doc.CalibrationProfiles) != 1 {
		t.Fatalf("expected exactly one calibration profile (the migrated default), got %d", len(doc.CalibrationProfiles))
	}
}

func TestHandleDownloadConfigurationBackup_WrongMethod(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/downloadConfigurationBackup", nil)
	rec := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// withPrivacySensitiveDeviceSettings sets every ownship/owner-identifying
// globalSettings field to a distinguishable test value for the duration
// of one test, restoring the original (zero) values afterward.
func withPrivacySensitiveDeviceSettings(t *testing.T) {
	t.Helper()
	origModeS, origAddr, origReg, origPilot := globalSettings.OwnshipModeS, globalSettings.OGNAddr, globalSettings.OGNReg, globalSettings.OGNPilot
	globalSettings.OwnshipModeS = "A1B2C3"
	globalSettings.OGNAddr = "DDEEFF"
	globalSettings.OGNReg = "N12345"
	globalSettings.OGNPilot = "Test Pilot"
	t.Cleanup(func() {
		globalSettings.OwnshipModeS, globalSettings.OGNAddr, globalSettings.OGNReg, globalSettings.OGNPilot = origModeS, origAddr, origReg, origPilot
	})
}

func TestHandleDownloadConfigurationBackup_DefaultExcludesPrivacy(t *testing.T) {
	withConfigBackupTestEnv(t)
	withPrivacySensitiveDeviceSettings(t)

	req := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup", nil)
	rec := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, needle := range []string{"A1B2C3", "DDEEFF", "N12345", "Test Pilot"} {
		if strings.Contains(body, needle) {
			t.Fatalf("default download must never include privacy-sensitive values, found %q in: %s", needle, body)
		}
	}
	var doc configbackup.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Configuration.PrivacySensitiveIncluded {
		t.Error("expected privacySensitiveIncluded to be false by default")
	}
	if !doc.Configuration.PrivacySensitive.Empty() {
		t.Errorf("expected an empty privacy section by default, got %+v", doc.Configuration.PrivacySensitive)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "stratux-configuration-backup.json") {
		t.Errorf("expected the sanitized filename, got %q", cd)
	}
}

func TestHandleDownloadConfigurationBackup_ExplicitOptInIncludesPrivacy(t *testing.T) {
	withConfigBackupTestEnv(t)
	withPrivacySensitiveDeviceSettings(t)

	req := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup?includePrivacySensitive=true", nil)
	rec := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc configbackup.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Configuration.PrivacySensitiveIncluded {
		t.Fatal("expected privacySensitiveIncluded to be true with explicit opt-in")
	}
	if doc.Configuration.PrivacySensitive.OwnshipModeS != "A1B2C3" {
		t.Errorf("expected the real ownshipModeS value, got %q", doc.Configuration.PrivacySensitive.OwnshipModeS)
	}
	res := configbackup.Validate(doc, rec.Body.Len())
	if !res.OK() {
		t.Fatalf("an honestly-flagged privacy-inclusive document must validate, got errors: %v", res.Errors)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "stratux-configuration-backup-with-identifiers.json") {
		t.Errorf("expected the identifiers-inclusive filename, got %q", cd)
	}
}

func TestHandleDownloadConfigurationBackup_InvalidIncludeFlagRejected(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup?includePrivacySensitive=yes", nil)
	rec := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid includePrivacySensitive value, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_OmittedPrivacyPreservesCurrentValues(t *testing.T) {
	withConfigBackupTestEnv(t)
	withPrivacySensitiveDeviceSettings(t)

	// A sanitized (default) backup - privacy section omitted.
	body := downloadTestBackup(t)
	var doc configbackup.Document
	json.Unmarshal(body, &doc)
	if doc.Configuration.PrivacySensitiveIncluded {
		t.Fatal("test precondition failed: expected the default backup to omit privacy fields")
	}

	token, backup := validateAndGetToken(t, body)

	configBackupMu.Lock()
	preview := configBackupPending.Preview
	configBackupMu.Unlock()
	if preview.PrivacySectionIncluded {
		t.Error("expected PrivacySectionIncluded to be false for a sanitized no-op backup")
	}
	if preview.PrivacyPreserved == "" {
		t.Error("expected a non-empty preservation note")
	}

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if globalSettings.OwnshipModeS != "A1B2C3" || globalSettings.OGNAddr != "DDEEFF" ||
		globalSettings.OGNReg != "N12345" || globalSettings.OGNPilot != "Test Pilot" {
		t.Errorf("expected privacy-sensitive device settings to be preserved untouched, got %+v",
			[]string{globalSettings.OwnshipModeS, globalSettings.OGNAddr, globalSettings.OGNReg, globalSettings.OGNPilot})
	}
}

func TestHandleApplyConfigurationBackup_IncludedPrivacyAppliesChange(t *testing.T) {
	withConfigBackupTestEnv(t)
	withPrivacySensitiveDeviceSettings(t)

	// A privacy-inclusive backup capturing the CURRENT (pre-change) values.
	req0 := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup?includePrivacySensitive=true", nil)
	rec0 := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec0, req0)
	var doc configbackup.Document
	json.Unmarshal(rec0.Body.Bytes(), &doc)

	// Now change the device's live value to something else.
	globalSettings.OGNPilot = "Someone Else"

	body, _ := json.Marshal(doc)
	token, backup := validateAndGetToken(t, body)

	configBackupMu.Lock()
	preview := configBackupPending.Preview
	configBackupMu.Unlock()
	if !preview.PrivacySectionIncluded {
		t.Fatal("expected PrivacySectionIncluded to be true")
	}
	found := false
	for _, n := range preview.PrivacySensitiveFieldNames {
		if n == "ognPilot" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ognPilot to be named in the preview, got %v", preview.PrivacySensitiveFieldNames)
	}

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if globalSettings.OGNPilot != "Test Pilot" {
		t.Errorf("expected ognPilot restored to the backed-up value, got %q", globalSettings.OGNPilot)
	}
}

// --- validate -------------------------------------------------------

func downloadTestBackup(t *testing.T) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/downloadConfigurationBackup", nil)
	rec := httptest.NewRecorder()
	handleDownloadConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("could not obtain a test backup: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func TestHandleValidateConfigurationBackup_ValidNoOp(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)

	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody(t, rec)
	if resp["success"] != true {
		t.Fatalf("expected success, got %+v", resp)
	}
	token, _ := resp["confirmationToken"].(string)
	if token == "" {
		t.Fatal("expected a non-empty confirmationToken")
	}
	preview, ok := resp["preview"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a preview object, got %+v", resp)
	}
	if hasChanges, _ := preview["hasChanges"].(bool); hasChanges {
		t.Errorf("expected hasChanges=false for a no-op backup, got %+v", preview)
	}

	status := configBackupStatusSnapshot()
	if status.State != restoreStatePreviewReady {
		t.Errorf("expected state preview-ready after a successful validate, got %q", status.State)
	}
	if !status.PreviewPending {
		t.Error("expected PreviewPending after a successful validate")
	}
}

func TestHandleValidateConfigurationBackup_MalformedJSON(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleValidateConfigurationBackup_TruncatedJSON(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	truncated := body[:len(body)/2]
	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(truncated))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for truncated JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleValidateConfigurationBackup_ChecksumMismatch(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	var doc configbackup.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Configuration.RegionSelected = doc.Configuration.RegionSelected + 1 // tamper after checksums stamped
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(tampered))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a checksum mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleValidateConfigurationBackup_UnsupportedSchema(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	var doc configbackup.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	doc.SchemaVersion = configbackup.SchemaVersion + 99
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(tampered))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unsupported schema version, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleValidateConfigurationBackup_Oversized(t *testing.T) {
	withConfigBackupTestEnv(t)
	oversized := bytes.Repeat([]byte("a"), configBackupMaxRequestBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(oversized))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleValidateConfigurationBackup_WrongMethod(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/validateConfigurationBackup", nil)
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleValidateConfigurationBackup_ConcurrentValidationsBothSucceed(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)

	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			handleValidateConfigurationBackupRequest(rec, req)
			done <- rec.Code
		}()
	}
	for i := 0; i < 2; i++ {
		if code := <-done; code != http.StatusOK {
			t.Errorf("expected both concurrent validations to succeed, got %d", code)
		}
	}
}

// --- apply ------------------------------------------------------------

// validateAndGetToken runs a real validate call and returns the token
// plus the exact bytes to re-submit as "backup" at apply time.
func validateAndGetToken(t *testing.T, body []byte) (token string, backup json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody(t, rec)
	tok, _ := resp["confirmationToken"].(string)
	if tok == "" {
		t.Fatal("expected a confirmation token")
	}
	return tok, json.RawMessage(body)
}

func applyRequestBody(t *testing.T, token string, backup json.RawMessage) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{"confirmationToken": token, "backup": json.RawMessage(backup)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandleApplyConfigurationBackup_NoOpSucceedsAndChangesNothing(t *testing.T) {
	store := withConfigBackupTestEnv(t)
	before := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, before)

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a no-op restore, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody(t, rec)
	if resp["success"] != true {
		t.Fatalf("expected success, got %+v", resp)
	}

	after := downloadTestBackup(t)
	var beforeDoc, afterDoc configbackup.Document
	json.Unmarshal(before, &beforeDoc)
	json.Unmarshal(after, &afterDoc)
	if beforeDoc.SectionChecksums["configuration"] != afterDoc.SectionChecksums["configuration"] {
		t.Error("expected configuration section to be byte-identical after a no-op restore")
	}
	if beforeDoc.SectionChecksums["calibrationProfiles"] != afterDoc.SectionChecksums["calibrationProfiles"] {
		t.Error("expected calibration-profiles section to be byte-identical after a no-op restore")
	}

	active, err := store.Active()
	if err != nil {
		t.Fatalf("expected an active profile after a no-op restore, got error: %v", err)
	}
	if active.ID != beforeDoc.ActiveCalibrationProfileID {
		t.Errorf("expected the active profile to be unchanged, got %q want %q", active.ID, beforeDoc.ActiveCalibrationProfileID)
	}
}

func TestHandleApplyConfigurationBackup_BenignSettingChangeAndRestore(t *testing.T) {
	withConfigBackupTestEnv(t)
	original := downloadTestBackup(t)

	// Change one benign, persisted alert setting through the real
	// supported settings path - not calibration, radio, or network.
	settings := loadAlertSettings()
	originalVolume := settings.AudioVolume
	settings.AudioVolume = 0.9
	if originalVolume == 0.9 {
		settings.AudioVolume = 0.1
	}
	if err := saveAlertSettings(settings); err != nil {
		t.Fatalf("could not change the benign setting: %v", err)
	}
	if got := loadAlertSettings().AudioVolume; got == originalVolume {
		t.Fatal("benign setting change did not persist")
	}

	// Validate the ORIGINAL backup against this now-changed state -
	// preview must show exactly one expected difference.
	token, backup := validateAndGetToken(t, original)

	configBackupMu.Lock()
	preview := configBackupPending.Preview
	configBackupMu.Unlock()
	if len(preview.AlertSettingsChanges) != 1 || preview.AlertSettingsChanges[0].Field != "audioVolume" {
		t.Fatalf("expected exactly one audioVolume change in the preview, got %+v", preview.AlertSettingsChanges)
	}
	if len(preview.ConfigurationChanges) != 0 {
		t.Errorf("expected no unrelated configuration changes, got %+v", preview.ConfigurationChanges)
	}

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got := loadAlertSettings().AudioVolume; got != originalVolume {
		t.Errorf("expected audioVolume restored to %v, got %v", originalVolume, got)
	}
}

func TestHandleApplyConfigurationBackup_MissingToken(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, "", json.RawMessage(body))))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_UnknownToken(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, "cfgrestore-doesnotexist", json.RawMessage(body))))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_ReusedTokenRejected(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)

	req1 := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec1 := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first apply should succeed, got %d: %s", rec1.Code, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec2 := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec2, req2)
	if rec2.Code != http.StatusGone {
		t.Fatalf("expected 410 for a reused token, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_ExpiredToken(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)

	configBackupMu.Lock()
	configBackupPending.Record.ExpiresAtMonotonic = monotonicSeconds() - 1
	configBackupMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410 for an expired token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_BackupChangedAfterPreview(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, _ := validateAndGetToken(t, body)

	var doc configbackup.Document
	json.Unmarshal(body, &doc)
	doc.Configuration.RegionSelected++ // change content without re-validating
	tampered, _ := json.Marshal(doc)

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, tampered)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	// The re-validate step inside apply rejects this as a checksum
	// mismatch (400) before token verification ever gets a chance to
	// classify it as "content changed" (409) - either way, it must
	// never be silently applied.
	if rec.Code == http.StatusOK {
		t.Fatalf("a backup modified after preview must never be silently applied, got 200: %s", rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_CurrentConfigurationChangedAfterPreview(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)

	// Mutate live state through a completely different, real API after
	// the preview was computed.
	settings := loadAlertSettings()
	settings.AudioVolume = settings.AudioVolume/2 + 0.05
	if err := saveAlertSettings(settings); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 when current configuration changed after preview, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_RejectedDuringActiveRecording(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)

	recMu.Lock()
	origCurrent := recCurrent
	recCurrent = &recordingSession{State: recordingStateActive}
	recMu.Unlock()
	t.Cleanup(func() {
		recMu.Lock()
		recCurrent = origCurrent
		recMu.Unlock()
	})

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 while a recording is active, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_RejectedDuringOTA(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)

	state := ota.State{Stage: ota.StageInstalling}
	if err := ota.SaveState(otaDir, state, time.Now()); err != nil {
		t.Fatalf("could not stage a fake OTA-in-progress state: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 during an in-progress OTA update, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_ConcurrentApplyRejected(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	token, backup := validateAndGetToken(t, body)

	configBackupMu.Lock()
	configBackupState = restoreStateApplying
	configBackupMu.Unlock()
	t.Cleanup(func() {
		configBackupMu.Lock()
		configBackupState = restoreStateIdle
		configBackupMu.Unlock()
	})

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a concurrent apply, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleApplyConfigurationBackup_AddedAndActivatedProfile(t *testing.T) {
	store := withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	var doc configbackup.Document
	json.Unmarshal(body, &doc)

	newProfile := calprofile.Profile{
		ID:               calprofile.NewID(),
		Name:             "Second Aircraft",
		SensorQuaternion: [4]float64{0, 0, 0, 1},
		D:                [3]float64{0.1, 0.1, 0.1},
		Kind:             calprofile.KindUser,
		SchemaVersion:    calprofile.SchemaVersion,
		CreatedAt:        time.Now().UTC(),
		ModifiedAt:       time.Now().UTC(),
	}
	newProfile.RecomputeValidity()
	doc.CalibrationProfiles = append(doc.CalibrationProfiles, newProfile)
	doc.ActiveCalibrationProfileID = newProfile.ID
	rebuilt, err := configbackup.BuildDocument(configbackup.BuildInputs{
		SourceVersion: doc.SourceVersion, SourceCommit: doc.SourceCommit, CreatedAtUTC: doc.CreatedAtUTC,
		Configuration: doc.Configuration, CalibrationProfiles: doc.CalibrationProfiles,
		ActiveCalibrationProfileID: doc.ActiveCalibrationProfileID, AlertSettings: doc.AlertSettings,
		// Carry over the downloaded backup's own AutoRecordSettings too,
		// exactly like AlertSettings above - a real device-produced
		// backup always has this section populated (loadAutoRecordSettings
		// never returns the bare Go zero value; see
		// autoRecordSettingsSectionFromCurrent's caller), so a document
		// built here without it would not faithfully represent a real
		// backup and would spuriously fail apply's stricter
		// autorecord.Settings.Validate() (0 knots is not strictly less
		// than 0 knots) - a test-construction gap, not a real restore
		// scenario.
		AutoRecordSettings: doc.AutoRecordSettings,
		// Same reasoning as AutoRecordSettings above, for the same
		// zero-value-fails-Validate() trap - see
		// applyFISBCacheSettingsSection's own doc comment.
		FISBCacheSettings: doc.FISBCacheSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	modifiedBody, err := json.Marshal(rebuilt)
	if err != nil {
		t.Fatal(err)
	}

	token, backup := validateAndGetToken(t, modifiedBody)
	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	got, err := store.Get(newProfile.ID)
	if err != nil {
		t.Fatalf("expected the new profile to have been added: %v", err)
	}
	if got.Name != "Second Aircraft" {
		t.Errorf("unexpected profile name %q", got.Name)
	}
	active, err := store.ActiveID()
	if err != nil || active != newProfile.ID {
		t.Errorf("expected the new profile to be active, got %q (err=%v)", active, err)
	}
	// The original migrated profile must still exist - a restore must
	// never delete a profile merely because it's absent from a
	// different backup revision (only this test's own additive backup
	// was applied, which still names it).
	if _, err := store.Get(doc.CalibrationProfiles[0].ID); err != nil {
		t.Errorf("expected the original profile to remain, got error: %v", err)
	}
}

// --- status -------------------------------------------------------

func TestHandleGetConfigurationRestoreStatus_Idle(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/getConfigurationRestoreStatus", nil)
	rec := httptest.NewRecorder()
	handleGetConfigurationRestoreStatusRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	resp := decodeJSONBody(t, rec)
	if resp["state"] != string(restoreStateIdle) {
		t.Errorf("expected idle state, got %+v", resp)
	}
}

func TestHandleGetConfigurationRestoreStatus_WrongMethod(t *testing.T) {
	withConfigBackupTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/getConfigurationRestoreStatus", nil)
	rec := httptest.NewRecorder()
	handleGetConfigurationRestoreStatusRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// --- diagnostics summary --------------------------------------------

func TestConfigBackupDiagnosticsSummary_NeverIncludesBackupContent(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := downloadTestBackup(t)
	validateAndGetToken(t, body)

	summary := configBackupDiagnosticsSummary()
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"sensorQuaternion", "calibrationProfiles", "contentChecksum"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Errorf("diagnostics summary must never include %q, got %s", forbidden, encoded)
		}
	}
}

// --- legacy (pre-Automatic-Flight-Recording) backup compatibility -----

// TestLegacyDefaultAutoRecordSettingsMatchesAutoRecordPackageDefault
// cross-checks configbackup's own independently-restated legacy default
// (it cannot import autorecord - see configbackup's package doc comment)
// against the real autorecord.DefaultSettings() - the one place both
// packages are already imported together, so a future change to either
// default without updating the other fails here rather than silently
// drifting.
func TestLegacyDefaultAutoRecordSettingsMatchesAutoRecordPackageDefault(t *testing.T) {
	got := configbackup.LegacyDefaultAutoRecordSettings()
	want := autorecord.DefaultSettings()
	if got.Enabled != want.Enabled ||
		got.StartGroundspeedKnots != want.StartGroundspeedKnots ||
		got.StartDwellSeconds != want.StartDwellSeconds ||
		got.StopGroundspeedKnots != want.StopGroundspeedKnots ||
		got.StopDwellSeconds != want.StopDwellSeconds ||
		got.GPSLossGraceSeconds != want.GPSLossGraceSeconds ||
		got.RestartCooldownSeconds != want.RestartCooldownSeconds ||
		got.MinimumRecordingDurationSeconds != want.MinimumRecordingDurationSeconds {
		t.Fatalf("configbackup.LegacyDefaultAutoRecordSettings() = %+v has drifted from autorecord.DefaultSettings() = %+v - update legacy.go's legacyDefaultAutoRecordSettings to match", got, want)
	}
}

// TestLegacyDefaultFISBCacheSettingsMatchesFISBCachePackageDefault
// cross-checks configbackup's own independently-restated legacy default
// (it cannot import main - see configbackup's package doc comment)
// against the real DefaultFISBCacheSettings() - the one place both are
// already available together, so a future change to either default
// without updating the other fails here rather than silently drifting.
// Mirrors TestLegacyDefaultAutoRecordSettingsMatchesAutoRecordPackageDefault.
func TestLegacyDefaultFISBCacheSettingsMatchesFISBCachePackageDefault(t *testing.T) {
	got := configbackup.LegacyDefaultFISBCacheSettings()
	want := DefaultFISBCacheSettings()
	if got.Enabled != want.Enabled ||
		got.PersistenceEnabled != want.PersistenceEnabled ||
		got.ReplayEnabled != want.ReplayEnabled ||
		got.MaxCacheBytes != want.MaxCacheBytes ||
		got.MaxEntries != want.MaxEntries {
		t.Fatalf("configbackup.LegacyDefaultFISBCacheSettings() = %+v has drifted from DefaultFISBCacheSettings() = %+v - update legacy.go's legacyDefaultFISBCacheSettings to match", got, want)
	}
}

// loadLegacyPreAutoRecordFixtureBody reads the same authentic,
// historically-generated fixture the configbackup package's own tests
// use - see configbackup/testdata/README.md.
func loadLegacyPreAutoRecordFixtureBody(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "configbackup", "testdata", "legacy-pre-autorecord-backup.json"))
	if err != nil {
		t.Fatalf("reading legacy fixture: %v", err)
	}
	return body
}

// TestHandleValidateConfigurationBackup_LegacyPreAutoRecordSucceeds is
// the end-to-end (real HTTP handlers, real settings files) proof that a
// genuine pre-Automatic-Flight-Recording backup validates successfully
// through the live restore path, not just the configbackup package's
// own pure-function tests.
func TestHandleValidateConfigurationBackup_LegacyPreAutoRecordSucceeds(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := loadLegacyPreAutoRecordFixtureBody(t)

	req := httptest.NewRequest(http.MethodPost, "/validateConfigurationBackup", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleValidateConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected a genuine legacy backup to validate, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody(t, rec)
	if resp["success"] != true {
		t.Fatalf("expected success, got %+v", resp)
	}
}

// TestHandleApplyConfigurationBackup_LegacyPreAutoRecordAppliesDisabled
// restores a genuine legacy backup end-to-end: Automatic Flight
// Recording was previously enabled on this "device" (simulating an
// operator who turned it on after the legacy backup was taken); after
// restoring the legacy backup, it must come back disabled - the
// documented safe default - while unrelated settings from the legacy
// backup (alertSettings.masterEnabled/audioVolume) are genuinely
// applied.
func TestHandleApplyConfigurationBackup_LegacyPreAutoRecordAppliesDisabled(t *testing.T) {
	withConfigBackupTestEnv(t)
	body := loadLegacyPreAutoRecordFixtureBody(t)

	// Simulate the feature having been turned on since this legacy
	// backup was taken.
	enabled := autorecord.DefaultSettings()
	enabled.Enabled = true
	enabled.StartGroundspeedKnots = 15
	enabled.StopGroundspeedKnots = 5
	if err := saveAutoRecordSettings(enabled); err != nil {
		t.Fatalf("could not pre-enable automatic recording: %v", err)
	}
	if !loadAutoRecordSettings().Enabled {
		t.Fatal("setup failed: automatic recording should be enabled before this test's restore")
	}
	// Also change the live alert settings so the legacy backup's own
	// alertSettings actually has something to restore.
	live := loadAlertSettings()
	live.MasterEnabled = false
	live.AudioVolume = 0.9
	if err := saveAlertSettings(live); err != nil {
		t.Fatalf("could not change live alert settings: %v", err)
	}

	token, backup := validateAndGetToken(t, body)
	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected a genuine legacy backup to apply, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody(t, rec)
	if resp["success"] != true {
		t.Fatalf("expected success, got %+v", resp)
	}

	after := loadAutoRecordSettings()
	if after.Enabled {
		t.Fatal("restoring a legacy backup that predates Automatic Flight Recording must leave it disabled, not carry over the live enabled state")
	}
	if after != autorecord.DefaultSettings() {
		t.Errorf("restored automatic-recording settings = %+v, want exactly autorecord.DefaultSettings() = %+v", after, autorecord.DefaultSettings())
	}

	// The legacy backup's own alertSettings (masterEnabled: true,
	// audioVolume: 0.5 - see testdata/README.md's generation inputs)
	// must have been genuinely applied, proving this restore is not
	// merely "succeeding while doing nothing."
	restoredAlert := loadAlertSettings()
	if !restoredAlert.MasterEnabled {
		t.Error("expected the legacy backup's alertSettings.masterEnabled=true to have been applied")
	}
	if restoredAlert.AudioVolume != 0.5 {
		t.Errorf("expected the legacy backup's alertSettings.audioVolume=0.5 to have been applied, got %v", restoredAlert.AudioVolume)
	}
}

// TestHandleApplyConfigurationBackup_LegacyPreAutoRecordNoPartialMutationOnFailure
// proves a legacy restore that fails partway (simulated: an active
// recording conflict, the same guard every restore already respects)
// leaves automatic-recording settings untouched - no partial mutation.
func TestHandleApplyConfigurationBackup_LegacyPreAutoRecordNoPartialMutationOnFailure(t *testing.T) {
	withConfigBackupTestEnv(t)
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestPreflightStore(t)
	withTestRecordingsDir(t)

	body := loadLegacyPreAutoRecordFixtureBody(t)
	token, backup := validateAndGetToken(t, body)

	before := loadAutoRecordSettings()

	recMu.Lock()
	recCurrent = &recordingSession{ID: "rec-legacy-conflict", State: recordingStateActive}
	recMu.Unlock()
	t.Cleanup(func() {
		recMu.Lock()
		recCurrent = nil
		recMu.Unlock()
	})

	req := httptest.NewRequest(http.MethodPost, "/applyConfigurationBackup", bytes.NewReader(applyRequestBody(t, token, backup)))
	rec := httptest.NewRecorder()
	handleApplyConfigurationBackupRequest(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected the restore to be rejected while a recording is active, got 200: %s", rec.Body.String())
	}

	after := loadAutoRecordSettings()
	if after != before {
		t.Errorf("a rejected restore must not have touched automatic-recording settings at all: before=%+v after=%+v", before, after)
	}
}
