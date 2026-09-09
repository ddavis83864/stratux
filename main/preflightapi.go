/*
preflightapi.go: wires the preflight package into the running daemon.

Responsibilities:
  - generate a random boot/session identity and construct the in-memory
    manual-acknowledgement store, before the HTTP server starts (see
    initPreflight, called from main())
  - assemble a preflight.Input from already-existing state (globalHealth,
    the active calibration profile, the recording subsystem) and call
    preflight.BuildReport - see buildPreflightReport
  - a minimal, additive HTTP API (get report / acknowledge / clear / reset),
    following the same conventions main/calprofilesapi.go and
    main/recordingapi.go already established
  - capture a preflight summary into a recording session at start time
    (see populateSessionPreflightSummary, called from
    main/recordingapi.go's handleStartRecordingRequest)
  - feed the current preflight report into diagnostic bundles (see
    main/diagnosticsapi.go)

Endpoints (all new, none replace or rename an existing one):

	GET  /getPreflightReport              - the current preflight report
	POST /acknowledgePreflightCheck?id=... - acknowledge one manual check
	POST /clearPreflightCheck?id=...      - clear one manual acknowledgement
	POST /resetPreflightChecks            - clear every manual acknowledgement

Failure isolation: a panic anywhere inside preflight.BuildReport (or this
file's own glue) is recovered in buildPreflightReport and reported as an
honest StateUnknown result with an explanation, rather than being allowed
to propagate - per the mission's explicit requirement, a failure in this
subsystem must never interrupt 978/1090/GPS/GNSS-time/FIS-B/GDL90/Wi-Fi/
AHRS/barometer/fan-control/diagnostics/recording.

This feature is purely observational: it never sends a command to any
other subsystem, and nothing here can affect ADS-B/GDL90 operation - see
docs/preflight-readiness.md.
*/
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/preflight"
)

// maxPreflightRequestBytes bounds every preflight POST body - these
// requests only ever carry a small, fixed-shape JSON object, so a body
// anywhere near this size is already a misbehaving or hostile client.
const maxPreflightRequestBytes = 4096

var (
	// preflightMu guards preflightAckStore/preflightSessionID during
	// startup, matching profilesMu's pattern in main/calprofilesapi.go.
	// Neither is ever reassigned after initPreflight() runs once, but a
	// handler invoked unusually early (or a future code path) should
	// still see a consistent, never-torn value.
	preflightMu        sync.Mutex
	preflightAckStore  *preflight.ManualAckStore
	preflightSessionID string
)

// newPreflightSessionID returns a random, opaque session identifier -
// same construction as calprofile.NewID() (crypto/rand, hex-encoded), but
// kept as its own tiny function rather than importing calprofile just for
// this, since the two identifier spaces are unrelated (a calibration
// profile ID and a preflight boot/session ID mean different things and
// must never be confused for one another).
func newPreflightSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Matches calprofile.NewID()'s documented stance: crypto/rand
		// failing means the system's entropy source is broken, which is
		// far more serious than anything this function could safely paper
		// over - see calprofile/store.go's NewID for the same reasoning.
		panic("preflight: crypto/rand failed: " + err.Error())
	}
	return "preflight-" + hex.EncodeToString(b[:])
}

// initPreflight constructs the manual-acknowledgement store and session
// identity. Must run before initNetwork()/managementInterface() start
// serving requests, and after stratuxClock is initialized (main()'s very
// first statement) since the store's clock source is stratuxClock.Time() -
// see main/gen_gdl90.go's main() for the exact call site.
func initPreflight() {
	sessionID := newPreflightSessionID()
	store := preflight.NewManualAckStore(sessionID, func() time.Time { return stratuxClock.Time() })
	preflightMu.Lock()
	preflightSessionID = sessionID
	preflightAckStore = store
	preflightMu.Unlock()
	log.Printf("preflight: session %s initialized\n", sessionID)
}

func currentPreflightAckStore() *preflight.ManualAckStore {
	preflightMu.Lock()
	defer preflightMu.Unlock()
	return preflightAckStore
}

// profileSummaryForPreflight derives preflight.ProfileSummary from the
// same profile-subsystem state main/calprofilesapi.go's
// activeProfileHealthInfo already exposes - this package never
// re-derives calibration-profile logic itself. CalibrationValid is
// derived from Kind rather than duplicating calprofile's own
// RecomputeValidity logic: a profile that made it out of migration/
// calibration as anything other than KindUncalibrated is, by
// construction, one whose CalibrationComplete() was already true - see
// calprofile/migrate.go and calprofile/profile.go.
func profileSummaryForPreflight() preflight.ProfileSummary {
	info := activeProfileHealthInfo()
	return preflight.ProfileSummary{
		Available:        info.Available,
		ID:               info.ID,
		Name:             info.Name,
		Kind:             info.Kind,
		CalibrationValid: info.Available && info.Kind != "" && info.Kind != calprofile.KindUncalibrated,
	}
}

// recordingReadinessForPreflight derives preflight.RecordingReadiness
// from the existing recording subsystem's own state - never re-derives
// storage/time-trust logic itself (that already lives in
// readiness.StorageHealth/TimeHealth, read via globalHealth below).
func recordingReadinessForPreflight(storageAllowed, timeAllowed bool) preflight.RecordingReadiness {
	// TryLock, never Lock: this function is reachable from
	// buildPreflightReport(), which handleStartRecordingRequest calls via
	// populateSessionPreflightSummary/applyPreflightSummaryToSession -
	// normally *before* acquiring recMu (see those functions' doc
	// comments), but sync.Mutex is not reentrant, so if anything ever
	// calls buildPreflightReport() while already holding recMu, blocking
	// here would deadlock that goroutine against itself rather than
	// against another goroutine. TryLock turns that into a graceful
	// "state temporarily unknown" instead of a daemon-wide hang - this
	// exact deadlock was caught live during hardware validation.
	state := "idle"
	if recMu.TryLock() {
		if recCurrent != nil {
			state = string(recCurrent.State)
		}
		recMu.Unlock()
	} else {
		state = "unknown"
	}
	return preflight.RecordingReadiness{
		StorageAvailable: storageAllowed,
		Permitted:        storageAllowed && timeAllowed,
		State:            state,
	}
}

// buildPreflightReport assembles a preflight.Input from current daemon
// state and calls preflight.BuildReport. Recovers from any panic in that
// call (or in gathering the input) and returns an honest StateUnknown
// report instead - see this file's package comment on failure isolation.
// This bypasses preflight.BuildReport's own normal three-state
// (READY/CAUTION/NOT_READY) contract for Report.Overall by design: an
// outright failure to compute a report at all is exactly the
// "insufficient trustworthy information" case preflight.StateUnknown
// exists for (see preflight.State's doc comment), which is a distinct
// situation from anything BuildReport's documented decision policy
// itself ever produces.
func buildPreflightReport() (report preflight.Report) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("preflight: report generation panicked, reporting UNKNOWN: %v\n", rec)
			report = preflight.Report{
				SchemaVersion: preflight.SchemaVersion,
				Overall:       preflight.StateUnknown,
				Summary:       "preflight status could not be determined - see server logs",
				BootSessionID: preflightSessionID,
				Disclaimer:    preflight.Disclaimer,
			}
		}
	}()

	// stratuxClock.Time() starts at the Go zero time.Time and advances
	// exactly 10ms per tick from the moment NewMonotonic() runs (main()'s
	// very first statement) - so its elapsed time since the zero value
	// *is* process uptime, with no separate "process start" timestamp to
	// track. See main/monotonic.go.
	mono := stratuxClock.Time()
	uptimeSeconds := mono.Sub(time.Time{}).Seconds()
	globalHealthMutex.Lock()
	health := globalHealth
	globalHealthMutex.Unlock()

	store := currentPreflightAckStore()
	var acks map[preflight.ManualCheckID]*preflight.ManualAck
	sessionID := preflightSessionID
	if store != nil {
		acks = store.Snapshot(preflight.DefaultAckExpiration)
		sessionID = store.SessionID()
	}

	var generatedAtUTC *time.Time
	if health.Time.CurrentUTC.Valid {
		t := health.Time.CurrentUTC.Time
		generatedAtUTC = &t
	}

	powerPreviousSessionMu.Lock()
	previousSession := powerPreviousSession
	powerPreviousSessionMu.Unlock()

	in := preflight.Input{
		Health:                       health,
		UptimeSeconds:                uptimeSeconds,
		Profile:                      profileSummaryForPreflight(),
		Recording:                    recordingReadinessForPreflight(health.Storage.RecordingAllowed, health.Time.RecordingAllowed),
		ManualAcks:                   acks,
		BootSessionID:                sessionID,
		GeneratedAtUTC:               generatedAtUTC,
		PreviousSessionAvailable:     previousSession.Available,
		PreviousSessionEndedCleanly:  previousSession.EndedCleanly,
		PreviousSessionNote:          previousSession.Note,
		StorageLifecycleHasInventory: health.StorageLifecycle.HasInventory,
		StorageLifecyclePressure:     health.StorageLifecycle.Pressure,
		StorageLifecycleStale:        health.StorageLifecycle.Stale,
		StorageLifecycleReason:       health.StorageLifecycle.Reason,
		AutoRecordEnabled:            health.AutoRecord.Enabled,
		AutoRecordMachineState:       health.AutoRecord.MachineState,
		AutoRecordReason:             health.AutoRecord.Reason,
		FISBCacheEnabled:             health.FISBCache.Enabled,
		FISBCacheState:               health.FISBCache.CacheState,
		FISBCacheReason:              health.FISBCache.Reason,
		FISBCacheTotalEntries:        health.FISBCache.TotalEntries,
	}
	return preflight.BuildReport(in)
}

// populateSessionPreflightSummary captures session-level preflight
// metadata at recording start - mirrors
// main/recordingapi.go's populateSessionCalibrationProfile. Deliberately
// summary-only (not the full Report) - see recordingSession's
// Preflight* fields for exactly what is captured and why.
//
// Must never be called while the caller already holds recMu:
// buildPreflightReport() -> recordingReadinessForPreflight() locks recMu
// itself, and sync.Mutex is not reentrant - main/recordingapi.go's
// handleStartRecordingRequest computes the report via buildPreflightReport()
// before acquiring recMu, then applies it with applyPreflightSummaryToSession
// below, specifically to avoid this. Callers with no lock already held
// (e.g. tests) may still use this convenience wrapper directly.
func populateSessionPreflightSummary(session *recordingSession) {
	applyPreflightSummaryToSession(session, buildPreflightReport())
}

// applyPreflightSummaryToSession copies the session-relevant fields out of
// an already-built preflight.Report - a pure, lock-free operation, safe to
// call from within a recMu-locked section (see populateSessionPreflightSummary's
// doc comment for why that distinction matters).
func applyPreflightSummaryToSession(session *recordingSession, r preflight.Report) {
	session.PreflightOverallState = string(r.Overall)
	session.PreflightRequiredActionCount = r.RequiredActionCount
	session.PreflightCautionCount = r.CautionCount
	session.PreflightManualChecksComplete = true
	for _, c := range r.Manual {
		if c.State != preflight.StateReady {
			session.PreflightManualChecksComplete = false
			break
		}
	}
}

// --- HTTP API ----------------------------------------------------------

func handlePreflightReportRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePreflightError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	writeJSON(w, http.StatusOK, buildPreflightReport())
}

type preflightCheckRequest struct {
	ID string `json:"id"`
}

func decodePreflightCheckID(w http.ResponseWriter, r *http.Request) (preflight.ManualCheckID, bool) {
	// Accept the id from either a query parameter (convenient for a
	// simple POST from the dashboard, matching
	// /activateCalibrationProfile?id=... conventions) or a JSON body -
	// whichever the caller used, never both silently disagreeing.
	if q := r.URL.Query().Get("id"); q != "" {
		return preflight.ManualCheckID(q), true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPreflightRequestBytes)
	var req preflightCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writePreflightError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return "", false
	}
	if req.ID == "" {
		writePreflightError(w, http.StatusBadRequest, fmt.Errorf("missing required field: id"))
		return "", false
	}
	return preflight.ManualCheckID(req.ID), true
}

func requirePreflightStore(w http.ResponseWriter) *preflight.ManualAckStore {
	store := currentPreflightAckStore()
	if store == nil {
		writePreflightError(w, http.StatusServiceUnavailable, fmt.Errorf("preflight subsystem not initialized"))
		return nil
	}
	return store
}

// handleAcknowledgePreflightCheckRequest serves
// POST /acknowledgePreflightCheck?id=... - idempotent: acknowledging an
// already-acknowledged check simply refreshes its timestamp, the same
// way filling in an already-checked checklist box again changes nothing
// meaningful.
func handleAcknowledgePreflightCheckRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePreflightError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requirePreflightStore(w)
	if store == nil {
		return
	}
	id, ok := decodePreflightCheckID(w, r)
	if !ok {
		return
	}
	var utcNow *time.Time
	globalHealthMutex.Lock()
	trusted := globalHealth.Time.CurrentUTC
	globalHealthMutex.Unlock()
	if trusted.Valid {
		t := trusted.Time
		utcNow = &t
	}
	ack, err := store.Ack(id, utcNow)
	if err != nil {
		writePreflightError(w, statusForPreflightError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "acknowledgement": ack})
}

// handleClearPreflightCheckRequest serves POST /clearPreflightCheck?id=...
func handleClearPreflightCheckRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePreflightError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requirePreflightStore(w)
	if store == nil {
		return
	}
	id, ok := decodePreflightCheckID(w, r)
	if !ok {
		return
	}
	if err := store.Clear(id); err != nil {
		writePreflightError(w, statusForPreflightError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// handleResetPreflightChecksRequest serves POST /resetPreflightChecks -
// the dashboard's "Reset checklist" control.
func handleResetPreflightChecksRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePreflightError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	store := requirePreflightStore(w)
	if store == nil {
		return
	}
	store.ResetAll()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func writePreflightError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]interface{}{"success": false, "error": err.Error()})
}

func statusForPreflightError(err error) int {
	if err == preflight.ErrUnknownManualCheck {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
