package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/calprofile"
)

// withTestProfilesStore points the package-level profilesStore at a fresh
// temp directory for the duration of one test, and restores the previous
// value afterward - tests must not leak state into each other or into the
// real profilesDir.
func withTestProfilesStore(t *testing.T) *calprofile.Store {
	t.Helper()
	origStore := profilesStore
	origErr := profilesInitError
	origSettings := globalSettings
	store := calprofile.NewStore(t.TempDir())
	profilesMu.Lock()
	profilesStore = store
	profilesInitError = nil
	profilesMu.Unlock()
	t.Cleanup(func() {
		profilesMu.Lock()
		profilesStore = origStore
		profilesInitError = origErr
		profilesMu.Unlock()
		globalSettings = origSettings
	})
	return store
}

func TestInitCalibrationProfiles_FirstRunMigratesAndAppliesToGlobalSettings(t *testing.T) {
	origSettings := globalSettings
	origStore := profilesStore
	origErr := profilesInitError
	defer func() {
		globalSettings = origSettings
		profilesMu.Lock()
		profilesStore = origStore
		profilesInitError = origErr
		profilesMu.Unlock()
	}()

	globalSettings.SensorQuaternion = [4]float64{1, 0, 0, 0}
	globalSettings.D = [3]float64{0.1, 0.1, 0.1}
	globalSettings.IMUMapping = [2]int{-1, 0}

	// Point profilesDir-equivalent at a temp store by constructing it the
	// same way initCalibrationProfiles does, but against a temp dir -
	// initCalibrationProfiles itself always uses the real profilesDir
	// constant, so this test exercises the same logic via direct calls
	// rather than the real constant path (covered instead by the live
	// hardware-validation checklist in the docs).
	store := calprofile.NewStore(t.TempDir())
	legacy := calprofile.LegacyCalibration{
		IMUMapping:       globalSettings.IMUMapping,
		SensorQuaternion: globalSettings.SensorQuaternion,
		C:                globalSettings.C,
		D:                globalSettings.D,
	}
	profilesMu.Lock()
	profilesStore = store
	active, err := calprofile.EnsureMigrated(store, legacy, time.Now().UTC())
	if err != nil {
		profilesMu.Unlock()
		t.Fatalf("EnsureMigrated: %v", err)
	}
	applyProfileToGlobalSettingsLocked(active)
	profilesMu.Unlock()

	if active.Name != "Current Installation" {
		t.Errorf("Name = %q", active.Name)
	}
	if globalSettings.SensorQuaternion != active.SensorQuaternion {
		t.Error("globalSettings.SensorQuaternion should mirror the active profile after migration")
	}
}

func TestActiveProfileHealthInfo_Unavailable_NoStore(t *testing.T) {
	origStore := profilesStore
	origErr := profilesInitError
	defer func() {
		profilesMu.Lock()
		profilesStore = origStore
		profilesInitError = origErr
		profilesMu.Unlock()
	}()
	profilesMu.Lock()
	profilesStore = nil
	profilesInitError = nil
	profilesMu.Unlock()

	info := activeProfileHealthInfo()
	if info.Available {
		t.Error("Available should be false when profilesStore is nil")
	}
}

func TestActiveProfileHealthInfo_Unavailable_InitError(t *testing.T) {
	withTestProfilesStore(t)
	profilesMu.Lock()
	profilesInitError = errors.New("simulated init failure")
	profilesMu.Unlock()

	info := activeProfileHealthInfo()
	if info.Available {
		t.Error("Available should be false when profilesInitError is set")
	}
	if info.Error == "" {
		t.Error("Error should be populated")
	}
}

func TestActiveProfileHealthInfo_Available(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{
		ID:            calprofile.NewID(),
		Name:          "Cherokee Six",
		Kind:          calprofile.KindUser,
		SchemaVersion: calprofile.SchemaVersion,
		CreatedAt:     now,
		ModifiedAt:    now,
	}
	store.Save(p)
	store.SetActiveID(p.ID, now)

	info := activeProfileHealthInfo()
	if !info.Available {
		t.Fatalf("expected Available, got error: %s", info.Error)
	}
	if info.Name != "Cherokee Six" {
		t.Errorf("Name = %q", info.Name)
	}
}

func TestCaptureActiveProfileCalibration_SavesAndResyncs(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{ID: calprofile.NewID(), Name: "Test", Kind: calprofile.KindUncalibrated, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(p)
	store.SetActiveID(p.ID, now)

	globalSettings.SensorQuaternion = [4]float64{1, 0, 0, 0}
	globalSettings.C = [3]float64{1, 0, 0}
	captureActiveProfileCalibration("level")

	updated, err := store.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SensorQuaternion != [4]float64{1, 0, 0, 0} {
		t.Error("capture should have saved the level calibration to the active profile")
	}

	globalSettings.D = [3]float64{0.1, 0.1, 0.1}
	captureActiveProfileCalibration("cal")
	updated, _ = store.Get(p.ID)
	if !updated.CalibrationComplete() {
		t.Error("after both level and cal captures, the profile should be calibration-complete")
	}
	if updated.LastCalibratedAt == nil {
		t.Error("LastCalibratedAt should be set once calibration is complete")
	}
}

func TestCaptureActiveProfileCalibration_NoActiveProfile_NoPanic(t *testing.T) {
	withTestProfilesStore(t) // empty store, no active profile
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("captureActiveProfileCalibration must not panic with no active profile: %v", r)
		}
	}()
	captureActiveProfileCalibration("level")
}

// --- HTTP handlers -----------------------------------------------------

func TestHandleListCalibrationProfilesRequest(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(p)
	store.SetActiveID(p.ID, now)

	req := httptest.NewRequest(http.MethodGet, "/getCalibrationProfiles", nil)
	w := httptest.NewRecorder()
	handleListCalibrationProfilesRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["activeProfileId"] != p.ID {
		t.Errorf("activeProfileId = %v, want %v", resp["activeProfileId"], p.ID)
	}
}

func TestHandleCreateCalibrationProfileRequest(t *testing.T) {
	withTestProfilesStore(t)
	body := strings.NewReader(`{"name":"Cherokee Six","registration":"N432NC","aircraftType":"PA-32-300","mountingNote":"Rear-left window"}`)
	req := httptest.NewRequest(http.MethodPost, "/createCalibrationProfile", body)
	w := httptest.NewRecorder()
	handleCreateCalibrationProfileRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	profile := resp["profile"].(map[string]interface{})
	if profile["name"] != "Cherokee Six" {
		t.Errorf("created profile name = %v", profile["name"])
	}
	if profile["kind"] != calprofile.KindUncalibrated {
		t.Errorf("a freshly created profile must start uncalibrated, got kind=%v", profile["kind"])
	}

	// Must NOT be auto-activated.
	activeID, _ := profilesStore.ActiveID()
	if activeID == profile["id"] {
		t.Error("a newly created profile must not be automatically activated")
	}
}

func TestHandleCreateCalibrationProfileRequest_MissingBody(t *testing.T) {
	withTestProfilesStore(t)
	req := httptest.NewRequest(http.MethodPost, "/createCalibrationProfile", strings.NewReader(""))
	w := httptest.NewRecorder()
	handleCreateCalibrationProfileRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid/empty JSON body", w.Code)
	}
}

func TestHandleCreateCalibrationProfileRequest_WrongMethod(t *testing.T) {
	withTestProfilesStore(t)
	req := httptest.NewRequest(http.MethodGet, "/createCalibrationProfile", nil)
	w := httptest.NewRecorder()
	handleCreateCalibrationProfileRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleActivateCalibrationProfileRequest(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	a := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now, SensorQuaternion: [4]float64{1, 0, 0, 0}}
	b := calprofile.Profile{ID: calprofile.NewID(), Name: "B", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now, SensorQuaternion: [4]float64{0, 1, 0, 0}}
	store.Save(a)
	store.Save(b)
	store.SetActiveID(a.ID, now)

	req := httptest.NewRequest(http.MethodPost, "/activateCalibrationProfile?id="+b.ID, nil)
	w := httptest.NewRecorder()
	handleActivateCalibrationProfileRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	activeID, _ := store.ActiveID()
	if activeID != b.ID {
		t.Errorf("active id = %q, want %q", activeID, b.ID)
	}
	if globalSettings.SensorQuaternion != b.SensorQuaternion {
		t.Error("activating a profile should copy its calibration into globalSettings")
	}
}

func TestHandleActivateCalibrationProfileRequest_RejectedWhileRecording(t *testing.T) {
	withTestProfilesStore(t)
	store := profilesStore
	now := time.Now().UTC()
	a := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(a)
	store.SetActiveID(a.ID, now)

	recMu.Lock()
	origRecCurrent := recCurrent
	recCurrent = &recordingSession{State: recordingStateActive}
	recMu.Unlock()
	defer func() {
		recMu.Lock()
		recCurrent = origRecCurrent
		recMu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/activateCalibrationProfile?id="+a.ID, nil)
	w := httptest.NewRecorder()
	handleActivateCalibrationProfileRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 while a recording is active", w.Code)
	}
}

func TestHandleActivateCalibrationProfileRequest_IdempotentReActivation(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	a := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(a)
	store.SetActiveID(a.ID, now)

	req := httptest.NewRequest(http.MethodPost, "/activateCalibrationProfile?id="+a.ID, nil)
	w := httptest.NewRecorder()
	handleActivateCalibrationProfileRequest(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("re-activating the already-active profile should succeed, got status %d", w.Code)
	}
}

func TestHandleActivateCalibrationProfileRequest_UnknownID(t *testing.T) {
	withTestProfilesStore(t)
	req := httptest.NewRequest(http.MethodPost, "/activateCalibrationProfile?id="+calprofile.NewID(), nil)
	w := httptest.NewRecorder()
	handleActivateCalibrationProfileRequest(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown profile id", w.Code)
	}
}

func TestHandleDeleteCalibrationProfileRequest_RejectsActive(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	a := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(a)
	store.SetActiveID(a.ID, now)

	req := httptest.NewRequest(http.MethodPost, "/deleteCalibrationProfile?id="+a.ID, nil)
	w := httptest.NewRecorder()
	handleDeleteCalibrationProfileRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 when deleting the active profile", w.Code)
	}
}

func TestHandleDeleteCalibrationProfileRequest_Inactive(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	a := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	b := calprofile.Profile{ID: calprofile.NewID(), Name: "B", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(a)
	store.Save(b)
	store.SetActiveID(a.ID, now)

	req := httptest.NewRequest(http.MethodPost, "/deleteCalibrationProfile?id="+b.ID, nil)
	w := httptest.NewRecorder()
	handleDeleteCalibrationProfileRequest(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 deleting an inactive profile", w.Code)
	}
}

func TestHandleUpdateCalibrationProfileRequest_MetadataOnly(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{
		ID: calprofile.NewID(), Name: "Old Name", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion,
		CreatedAt: now, ModifiedAt: now, SensorQuaternion: [4]float64{1, 0, 0, 0},
	}
	store.Save(p)

	body := strings.NewReader(`{"name":"New Name","registration":"N123AB"}`)
	req := httptest.NewRequest(http.MethodPost, "/updateCalibrationProfile?id="+p.ID, body)
	w := httptest.NewRecorder()
	handleUpdateCalibrationProfileRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	updated, _ := store.Get(p.ID)
	if updated.Name != "New Name" || updated.Registration != "N123AB" {
		t.Errorf("metadata not updated: %+v", updated)
	}
	if updated.SensorQuaternion != p.SensorQuaternion {
		t.Error("updating metadata must never touch the calibration vectors")
	}
}

func TestHandleCalibrationProfileStatusRequest(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{ID: calprofile.NewID(), Name: "Test", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(p)
	store.SetActiveID(p.ID, now)

	req := httptest.NewRequest(http.MethodGet, "/getCalibrationProfileStatus", nil)
	w := httptest.NewRecorder()
	handleCalibrationProfileStatusRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["profileName"] != "Test" {
		t.Errorf("profileName = %v", resp["profileName"])
	}
}

// --- Recording integration ----------------------------------------------

func TestPopulateSessionCalibrationProfile_CalibratedProfile(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{
		ID: calprofile.NewID(), Name: "Cherokee Six", Registration: "N432NC", AircraftType: "PA-32-300",
		MountingNote: "Rear-left window", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion,
		CreatedAt: now, ModifiedAt: now, SensorQuaternion: [4]float64{1, 0, 0, 0}, D: [3]float64{0.1, 0.1, 0.1},
		LastCalibratedAt: &now,
	}
	store.Save(p)
	store.SetActiveID(p.ID, now)

	session := &recordingSession{}
	populateSessionCalibrationProfile(session)

	if !session.CalibrationProfileAvailable {
		t.Fatal("expected CalibrationProfileAvailable")
	}
	if session.CalibrationProfileName != "Cherokee Six" || session.CalibrationRegistration != "N432NC" {
		t.Errorf("session metadata not captured: %+v", session)
	}
	if !session.CalibrationValid {
		t.Error("a fully-calibrated active profile should set CalibrationValid")
	}
	if session.CalibrationLastCalibratedAt == nil {
		t.Error("CalibrationLastCalibratedAt should be captured")
	}
}

func TestPopulateSessionCalibrationProfile_UncalibratedProfile(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	p := calprofile.Profile{ID: calprofile.NewID(), Name: "New Plane", Kind: calprofile.KindUncalibrated, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(p)
	store.SetActiveID(p.ID, now)

	session := &recordingSession{}
	populateSessionCalibrationProfile(session)

	if !session.CalibrationProfileAvailable {
		t.Fatal("expected CalibrationProfileAvailable even for an uncalibrated profile")
	}
	if session.CalibrationValid {
		t.Error("an uncalibrated active profile must not report CalibrationValid")
	}
}

func TestPopulateSessionCalibrationProfile_MissingProfile(t *testing.T) {
	withTestProfilesStore(t) // empty store, no active profile
	session := &recordingSession{}
	populateSessionCalibrationProfile(session)

	if session.CalibrationProfileAvailable {
		t.Error("CalibrationProfileAvailable must be false with no active profile")
	}
	if session.CalibrationProfileID != "" || session.CalibrationProfileName != "" {
		t.Error("Calibration* fields must stay at their zero value when no profile is available")
	}
}

func TestPopulateSessionCalibrationProfile_NilStore_NoPanic(t *testing.T) {
	origStore := profilesStore
	defer func() {
		profilesMu.Lock()
		profilesStore = origStore
		profilesMu.Unlock()
	}()
	profilesMu.Lock()
	profilesStore = nil
	profilesMu.Unlock()

	session := &recordingSession{}
	populateSessionCalibrationProfile(session)
	if session.CalibrationProfileAvailable {
		t.Error("expected CalibrationProfileAvailable=false when profilesStore is nil")
	}
}

// TestConcurrentActivateRequests exercises profilesMu under concurrent
// HTTP-level activation requests - no data race, and the final active
// profile must be one of the two candidates, never a corrupted mix.
func TestConcurrentActivateRequests(t *testing.T) {
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	a := calprofile.Profile{ID: calprofile.NewID(), Name: "A", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	b := calprofile.Profile{ID: calprofile.NewID(), Name: "B", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion, CreatedAt: now, ModifiedAt: now}
	store.Save(a)
	store.Save(b)
	store.SetActiveID(a.ID, now)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		id := a.ID
		if i%2 == 0 {
			id = b.ID
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/activateCalibrationProfile?id="+id, nil)
			w := httptest.NewRecorder()
			handleActivateCalibrationProfileRequest(w, req)
		}(id)
	}
	wg.Wait()

	activeID, err := store.ActiveID()
	if err != nil {
		t.Fatal(err)
	}
	if activeID != a.ID && activeID != b.ID {
		t.Errorf("active id after concurrent activation = %q, must be one of the two valid profiles", activeID)
	}
}
