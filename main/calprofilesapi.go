/*
calprofilesapi.go: wires the calprofile package into the running daemon.

Responsibilities:
  - initialize the profile store under the persistent data partition and
    run one-time legacy-calibration migration, before any sensor code
    starts (see initCalibrationProfiles, called from main())
  - keep globalSettings.C/D/SensorQuaternion/IMUMapping (which
    main/sensors.go's existing, unmodified calibration engine reads) in
    sync with whichever profile is active
  - capture the result of an existing Set Level/Zero Drift action into the
    active profile (see captureActiveProfileCalibration, called from
    main/sensors.go's sensorAttitudeSender right after its existing
    calibration retry loop completes - the retry loop itself is untouched)
  - a minimal, additive HTTP API (list/get-active/create/update/activate/
    delete/capture/status), following the same conventions
    main/recordingapi.go and main/diagnosticsapi.go already established
  - feed readiness.AHRSProfileInfo so /getHealth's AHRS tile reflects
    profile state - see activeProfileHealthInfo

Endpoints (all new, none replace or rename an existing one):

	GET  /getCalibrationProfiles          - list all profiles + active id
	GET  /getActiveCalibrationProfile     - the active profile
	GET  /getCalibrationProfileStatus     - active profile + subsystem health
	POST /createCalibrationProfile        - create a new, uncalibrated profile
	POST /updateCalibrationProfile?id=... - edit metadata only (name/registration/aircraftType/mountingNote)
	POST /activateCalibrationProfile?id=... - make a profile active
	POST /deleteCalibrationProfile?id=... - delete an inactive profile
	POST /captureCalibrationProfile?id=... - snapshot the CURRENT live calibration into a profile

Failure of this subsystem must never interrupt 978/1090/GPS/FIS-B/GDL90/
Wi-Fi/diagnostics/existing recordings/fan control/barometer operation -
every failure path here is reported (profilesInitError, HTTP error
responses) rather than allowed to panic or block startup.
*/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/readiness"
)

// profilesDir is the persistent, application-owned directory for
// calibration profiles - a sibling of recordingsDir/exportsDir/
// diagnosticsDir (main/recordingapi.go, main/diagnosticsapi.go), never
// the temporary root overlay.
const profilesDir = PersistentDataPath + "/calibration-profiles"

var (
	// profilesMu guards the composite "apply a profile to the live,
	// running system" operation (SetActiveID + copying calibration into
	// globalSettings) as one atomic step from this daemon's point of
	// view - distinct from calprofile.Store's own internal mutex, which
	// only ever guards raw file I/O. Also guards profilesInitError.
	profilesMu sync.Mutex

	profilesStore     *calprofile.Store
	profilesInitError error // non-nil if initCalibrationProfiles could not establish an active profile
)

// initCalibrationProfiles initializes the profile store and runs one-time
// migration. Must be called after readSettings() has populated
// globalSettings and before initI2CSensors() starts any goroutine that
// reads globalSettings.C/D/SensorQuaternion/IMUMapping - see main()'s
// call site.
//
// A failure here is recorded in profilesInitError and logged, never
// panics: main/sensors.go's existing calibration engine keeps reading
// whatever is already in globalSettings (the legacy, pre-profile values
// readSettings() just loaded from stratux.conf) exactly as it always has,
// so AHRS/978/1090/GPS/etc. all continue working - only the new
// profile-aware layer on top degrades (see activeProfileHealthInfo,
// which readiness.BuildAHRSHealth turns into an honest DEGRADED, never a
// silently-ignored READY or a false NOT_READY).
func initCalibrationProfiles() {
	profilesMu.Lock()
	defer profilesMu.Unlock()

	profilesStore = calprofile.NewStore(profilesDir)
	legacy := calprofile.LegacyCalibration{
		IMUMapping:       globalSettings.IMUMapping,
		SensorQuaternion: globalSettings.SensorQuaternion,
		C:                globalSettings.C,
		D:                globalSettings.D,
	}
	active, err := calprofile.EnsureMigrated(profilesStore, legacy, time.Now().UTC())
	if err != nil {
		profilesInitError = err
		log.Printf("calibration profiles: could not initialize/migrate: %s\n", err)
		return
	}
	applyProfileToGlobalSettingsLocked(active)
	log.Printf("calibration profiles: active profile %q (%s)\n", active.Name, active.ID)
}

// applyProfileToGlobalSettingsLocked copies p's calibration into
// globalSettings - the exact fields main/sensors.go's sensorAttitudeSender
// already reads every loop iteration. Deliberately does not call
// saveSettings(): the profile store, not stratux.conf, is now the
// authoritative source for calibration, so this is a live in-memory
// mirror refreshed from the active profile at startup and on every
// activation/calibration - see docs/aircraft-calibration-profiles.md's
// "why globalSettings still exists" note. Caller must hold profilesMu.
func applyProfileToGlobalSettingsLocked(p calprofile.Profile) {
	globalSettings.IMUMapping = p.IMUMapping
	globalSettings.SensorQuaternion = p.SensorQuaternion
	globalSettings.C = p.C
	globalSettings.D = p.D
}

// activeProfileHealthInfo derives readiness.AHRSProfileInfo from the
// current profile-subsystem state, for main/fancontrolstatus.go's
// buildAHRSHealth to pass into readiness.BuildAHRSHealth.
func activeProfileHealthInfo() readiness.AHRSProfileInfo {
	profilesMu.Lock()
	initErr := profilesInitError
	store := profilesStore
	profilesMu.Unlock()

	if initErr != nil || store == nil {
		errText := "profile store not initialized"
		if initErr != nil {
			errText = initErr.Error()
		}
		return readiness.AHRSProfileInfo{Available: false, Error: errText}
	}
	active, err := store.Active()
	if err != nil {
		return readiness.AHRSProfileInfo{Available: false, Error: err.Error()}
	}
	var lastCal readiness.OptionalTime
	if active.LastCalibratedAt != nil {
		lastCal = readiness.SomeTime(*active.LastCalibratedAt)
	}
	return readiness.AHRSProfileInfo{
		Available:        true,
		ID:               active.ID,
		Name:             active.Name,
		Kind:             active.Kind,
		LastCalibratedAt: lastCal,
	}
}

// captureActiveProfileCalibration snapshots globalSettings.C/D/
// SensorQuaternion/IMUMapping (which main/sensors.go's calibration retry
// loop just finished writing, unchanged - see that file's single
// additive call site) into the active profile, atomically, and re-syncs
// globalSettings from the saved result. action is "level" or "cal",
// exactly the same strings the existing cal channel already uses.
//
// Never called while holding mySituation.muAttitude or any other lock
// main/sensors.go holds - it only reads globalSettings, matching every
// other unsynchronized globalSettings access in the existing codebase
// (see calprofile's package doc comment for why this package does not
// try to fix that pre-existing, broader gap).
func captureActiveProfileCalibration(action string) {
	profilesMu.Lock()
	defer profilesMu.Unlock()
	if profilesStore == nil {
		return
	}
	active, err := profilesStore.Active()
	if err != nil {
		log.Printf("calibration profiles: could not capture %s result: no active profile (%s)\n", action, err)
		return
	}
	now := time.Now().UTC()
	updated := calprofile.ApplyCalibration(active, action, globalSettings.IMUMapping, globalSettings.SensorQuaternion, globalSettings.C, globalSettings.D, now)
	if err := profilesStore.Save(updated); err != nil {
		log.Printf("calibration profiles: could not save %s result to profile %q: %s\n", action, active.ID, err)
		return
	}
	applyProfileToGlobalSettingsLocked(updated)
}

// --- HTTP API --------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	setNoCache(w)
	setJSONHeaders(w)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeProfileError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]interface{}{"success": false, "error": err.Error()})
}

// statusForProfileError maps a calprofile package error to the HTTP
// status code that best represents it, so every handler reports the same
// error consistently rather than each guessing its own mapping.
func statusForProfileError(err error) int {
	switch err {
	case calprofile.ErrNotFound, calprofile.ErrNoActiveProfile:
		return http.StatusNotFound
	case calprofile.ErrInvalidID:
		return http.StatusBadRequest
	case calprofile.ErrDuplicateName:
		return http.StatusConflict
	case calprofile.ErrTooManyProfiles:
		return http.StatusInsufficientStorage
	case calprofile.ErrActiveProfile:
		return http.StatusConflict
	default:
		if strings.Contains(err.Error(), "calprofile: invalid profile field") {
			return http.StatusBadRequest
		}
		return http.StatusInternalServerError
	}
}

func requireProfilesStore(w http.ResponseWriter) *calprofile.Store {
	profilesMu.Lock()
	store := profilesStore
	initErr := profilesInitError
	profilesMu.Unlock()
	if store == nil {
		writeProfileError(w, http.StatusServiceUnavailable, fmt.Errorf("calibration-profile subsystem not initialized"))
		return nil
	}
	if initErr != nil {
		writeProfileError(w, http.StatusServiceUnavailable, initErr)
		return nil
	}
	return store
}

// handleListCalibrationProfilesRequest serves GET /getCalibrationProfiles.
func handleListCalibrationProfilesRequest(w http.ResponseWriter, r *http.Request) {
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	profiles, err := store.List()
	if err != nil {
		writeProfileError(w, http.StatusInternalServerError, err)
		return
	}
	if profiles == nil {
		profiles = []calprofile.Profile{}
	}
	activeID, _ := store.ActiveID()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"profiles":        profiles,
		"activeProfileId": activeID,
	})
}

// handleActiveCalibrationProfileRequest serves GET /getActiveCalibrationProfile.
func handleActiveCalibrationProfileRequest(w http.ResponseWriter, r *http.Request) {
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	active, err := store.Active()
	if err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "profile": active})
}

// handleCalibrationProfileStatusRequest serves GET /getCalibrationProfileStatus:
// the active profile plus subsystem health, for a single-request dashboard
// summary widget.
func handleCalibrationProfileStatusRequest(w http.ResponseWriter, r *http.Request) {
	info := activeProfileHealthInfo()
	resp := map[string]interface{}{
		"success":   info.Available,
		"available": info.Available,
	}
	if !info.Available {
		resp["error"] = info.Error
	} else {
		resp["profileId"] = info.ID
		resp["profileName"] = info.Name
		resp["kind"] = info.Kind
		if !info.LastCalibratedAt.IsZero() {
			resp["lastCalibratedAt"] = info.LastCalibratedAt
		} else {
			resp["lastCalibratedAt"] = nil
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// createCalibrationProfileRequest is the request body for
// /createCalibrationProfile and the metadata-only fields
// /updateCalibrationProfile accepts.
type calibrationProfileMetadataRequest struct {
	Name         string `json:"name"`
	Registration string `json:"registration"`
	AircraftType string `json:"aircraftType"`
	MountingNote string `json:"mountingNote"`
}

// handleCreateCalibrationProfileRequest serves POST /createCalibrationProfile.
// The new profile is created uncalibrated (Kind: uncalibrated) and NOT
// activated automatically - an explicit /activateCalibrationProfile call
// is required, so a half-configured profile never silently becomes what
// the AHRS engine is using.
func handleCreateCalibrationProfileRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProfileError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	var req calibrationProfileMetadataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProfileError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	now := time.Now().UTC()
	p := calprofile.Profile{
		ID:            calprofile.NewID(),
		Name:          req.Name,
		Registration:  req.Registration,
		AircraftType:  req.AircraftType,
		MountingNote:  req.MountingNote,
		Kind:          calprofile.KindUncalibrated,
		SchemaVersion: calprofile.SchemaVersion,
		CreatedAt:     now,
		ModifiedAt:    now,
	}
	if err := store.Save(p); err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	log.Printf("calibration profiles: created profile %q (%s)\n", p.Name, p.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "profile": p})
}

// handleUpdateCalibrationProfileRequest serves
// POST /updateCalibrationProfile?id=... - metadata fields only; never
// touches the profile's calibration vectors.
func handleUpdateCalibrationProfileRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProfileError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	id := r.URL.Query().Get("id")
	p, err := store.Get(id)
	if err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	var req calibrationProfileMetadataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProfileError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	p.Name = req.Name
	p.Registration = req.Registration
	p.AircraftType = req.AircraftType
	p.MountingNote = req.MountingNote
	p.ModifiedAt = time.Now().UTC()
	if err := store.Save(p); err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "profile": p})
}

// handleActivateCalibrationProfileRequest serves
// POST /activateCalibrationProfile?id=...
//
// Refuses (409) while a recording is active - see recMu/recCurrent
// (main/recordingapi.go) - the mission's preferred, simpler-and-safer
// choice over an in-recording calibration-change event. Re-activating
// the already-active profile is a safe, idempotent no-op that still
// returns success. On any failure, the currently active calibration is
// left completely unchanged - never partially applied.
func handleActivateCalibrationProfileRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProfileError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	id := r.URL.Query().Get("id")

	recMu.Lock()
	recordingActive := recCurrent != nil && recCurrent.State == recordingStateActive
	recMu.Unlock()
	if recordingActive {
		writeProfileError(w, http.StatusConflict, fmt.Errorf("cannot change the active calibration profile while a recording is active"))
		return
	}

	target, err := store.Get(id)
	if err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}

	profilesMu.Lock()
	defer profilesMu.Unlock()
	now := time.Now().UTC()
	if err := store.SetActiveID(target.ID, now); err != nil {
		// Nothing was applied to globalSettings yet - the running
		// system's calibration is unchanged.
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	applyProfileToGlobalSettingsLocked(target)
	log.Printf("calibration profiles: activated profile %q (%s)\n", target.Name, target.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "profile": target})
}

// handleDeleteCalibrationProfileRequest serves
// POST /deleteCalibrationProfile?id=... - refuses to delete the active
// profile (calprofile.ErrActiveProfile -> 409); the store itself enforces
// this, this handler only translates the error.
func handleDeleteCalibrationProfileRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProfileError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	id := r.URL.Query().Get("id")
	if err := store.Delete(id); err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	log.Printf("calibration profiles: deleted profile %s\n", id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleCaptureCalibrationProfileRequest serves
// POST /captureCalibrationProfile?id=... - snapshots the CURRENT live
// globalSettings.C/D/SensorQuaternion/IMUMapping into the given profile
// (defaulting to the active profile if id is omitted), independent of an
// actual Set Level/Zero Drift action having just run. Useful for manually
// assigning an already-good live calibration to a specific profile.
func handleCaptureCalibrationProfileRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProfileError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requireProfilesStore(w)
	if store == nil {
		return
	}
	id := r.URL.Query().Get("id")
	var target calprofile.Profile
	var err error
	if id == "" {
		target, err = store.Active()
	} else {
		target, err = store.Get(id)
	}
	if err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}

	profilesMu.Lock()
	defer profilesMu.Unlock()
	now := time.Now().UTC()
	updated := calprofile.ApplyCalibration(target, "level", globalSettings.IMUMapping, globalSettings.SensorQuaternion, globalSettings.C, globalSettings.D, now)
	updated = calprofile.ApplyCalibration(updated, "cal", globalSettings.IMUMapping, globalSettings.SensorQuaternion, globalSettings.C, globalSettings.D, now)
	if err := store.Save(updated); err != nil {
		writeProfileError(w, statusForProfileError(err), err)
		return
	}
	// Only re-sync globalSettings if this WAS the active profile -
	// capturing into an inactive profile must never change what the
	// running system is currently using.
	activeID, _ := store.ActiveID()
	if activeID == updated.ID {
		applyProfileToGlobalSettingsLocked(updated)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "profile": updated})
}
