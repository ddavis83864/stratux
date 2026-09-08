/*
powerapi.go: HTTP glue for the power package (see power/) - debounced
power/thermal health (readiness.ThrottleStatus wrapped with an honest
capability model), the previous-session clean/unclean marker, and a
manual, two-step confirmed controlled-shutdown flow.

Endpoints:

	GET  /getPowerHealth     - debounced power-health reading + previous-session assessment
	GET  /getShutdownStatus  - current controlled-shutdown stage
	POST /requestShutdown    - step 1: preconditions + issue a confirmation token
	POST /confirmShutdown    - step 2: consume the token, flush, sync, power off

See docs/power-shutdown-resilience.md for the full design: the explicit
non-goals (no battery percentage, no automatic/unattended shutdown, no
GPIO reservation, no UPS HAT integration), the two-step confirmation UX,
and the hardware-validation checklist reserved for a future,
owner-authorized mission - this feature ships built and tested, but NOT
deployed, on its own draft PR.

This file never touches the pre-existing, unconfirmed POST /shutdown
(handleShutdownRequest in managementinterface.go) - that endpoint is left
completely unchanged.
*/
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/stratux/stratux/ota"
	"github.com/stratux/stratux/power"
)

var (
	// powerSessionMarkerPath is a var, not a const, purely so tests can
	// redirect it to a temp file - same rationale as otaDir/
	// alertSettingsPath.
	powerSessionMarkerPath = PersistentDataPath + "/power-session.json"

	powerMonitorMu sync.Mutex
	powerMonitor   = power.NewMonitor(3) // 3 consecutive samples before the debounced reading changes - see power.Monitor

	powerPreviousSessionMu sync.Mutex
	powerPreviousSession   power.PreviousSessionAssessment
	powerCurrentSessionID  string

	shutdownManager *power.Manager
)

// currentBootOrSessionID prefers Linux's own per-boot random id (stable
// across a daemon restart within the same boot, so a crashed-and-
// restarted stratux process is never confused with an actual power
// cycle) and falls back to this daemon's own random per-process session
// id (preflightSessionID) wherever that file cannot be read (a non-Linux
// dev build, or a container/chroot without /proc mounted).
func currentBootOrSessionID() string {
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	preflightMu.Lock()
	id := preflightSessionID
	preflightMu.Unlock()
	return id
}

// initPower reads and evaluates the previous session's marker (before
// overwriting it), writes a fresh "session started, not yet closed"
// marker for this boot, and constructs the shutdown Manager with this
// project's real executor and preconditions. Must run after
// initPreflight() (needs preflightSessionID as a fallback session id and
// as the shutdown-token boot-session binding) and after readSettings()
// (PersistentDataPath must already be the real, final path by then).
func initPower() {
	id := currentBootOrSessionID()

	previous, exists, err := power.ReadSessionMarker(powerSessionMarkerPath)
	if err != nil {
		log.Printf("power: could not read previous session marker (treating as absent): %s\n", err)
		exists = false
	}
	assessment := power.EvaluatePreviousSession(previous, exists)

	powerPreviousSessionMu.Lock()
	powerPreviousSession = assessment
	powerCurrentSessionID = id
	powerPreviousSessionMu.Unlock()

	if err := power.WriteSessionMarkerAtomic(powerSessionMarkerPath, power.SessionMarker{
		SessionID:            id,
		ClosedCleanly:        false,
		UpdatedAtMonoSeconds: monotonicSeconds(),
	}); err != nil {
		log.Printf("power: could not write session marker: %s\n", err)
	}

	preflightMu.Lock()
	bootSessionID := preflightSessionID
	preflightMu.Unlock()

	shutdownManager = power.NewManager(bootSessionID, monotonicSeconds, []power.Precondition{
		otaNotBusyPrecondition,
		configBackupNotBusyPrecondition,
	}, func() error {
		gracefulShutdown()
		return nil
	}, realShutdownExecutor{})

	log.Printf("power: session %s initialized (previous session available: %v, ended cleanly: %v)\n", id, assessment.Available, assessment.EndedCleanly)
}

// markSessionClosed atomically records that this session ended via a
// deliberate, recorded shutdown/reboot path. Called immediately before
// main/'s own reboot handler issues its command, and immediately after
// this package's own confirmed-shutdown flow reaches COMMAND_ISSUED - in
// both cases strictly before the actual reboot/poweroff command runs. A
// failure here is logged, not fatal: it only means the *next* boot's
// previous-session assessment will be the conservative "did not record a
// clean close" case rather than blocking the reboot/shutdown an operator
// explicitly requested.
func markSessionClosed(reason string) {
	powerPreviousSessionMu.Lock()
	id := powerCurrentSessionID
	powerPreviousSessionMu.Unlock()
	if id == "" {
		id = currentBootOrSessionID()
	}
	if err := power.WriteSessionMarkerAtomic(powerSessionMarkerPath, power.SessionMarker{
		SessionID:            id,
		ClosedCleanly:        true,
		ClosedReason:         reason,
		UpdatedAtMonoSeconds: monotonicSeconds(),
	}); err != nil {
		log.Printf("power: could not record clean session close (%s): %s\n", reason, err)
	}
}

// otaNotBusyPrecondition mirrors the exact OTA-busy check
// main/configbackupapi.go's restore-apply path already uses.
func otaNotBusyPrecondition() error {
	if otaState, err := ota.LoadState(otaDir); err == nil && otaState.Stage != ota.StageIdle && !otaState.Stage.Terminal() {
		return fmt.Errorf("an OTA update is in progress")
	}
	return nil
}

// configBackupNotBusyPrecondition mirrors the exact configuration-
// restore-busy check main/configbackupapi.go's own handlers use.
func configBackupNotBusyPrecondition() error {
	configBackupMu.Lock()
	busy := configBackupState == restoreStateApplying || configBackupState == restoreStateVerifying || configBackupState == restoreStateRollingBack
	configBackupMu.Unlock()
	if busy {
		return fmt.Errorf("a configuration restore is in progress")
	}
	return nil
}

// realShutdownExecutor is the only part of this feature that touches real
// hardware - every test in power/ and this file's own test file injects a
// fake instead. Sync/PowerOff mirror the exact commands
// handleShutdownRequest (the pre-existing, unconfirmed /shutdown endpoint
// - left entirely unchanged by this feature) already uses.
type realShutdownExecutor struct{}

func (realShutdownExecutor) Sync() error {
	syscall.Sync()
	return nil
}

func (realShutdownExecutor) PowerOff() error {
	return exec.Command("systemctl", "poweroff").Run()
}

func newShutdownToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("power: crypto/rand failed: " + err.Error())
	}
	return "shutdown-" + hex.EncodeToString(b[:])
}

func statusForShutdownError(err error) int {
	switch {
	case errors.Is(err, power.ErrPreconditionFailed), errors.Is(err, power.ErrAlreadyInProgress):
		return http.StatusConflict
	case errors.Is(err, power.ErrTokenNotFound), errors.Is(err, power.ErrTokenUsed), errors.Is(err, power.ErrTokenExpired), errors.Is(err, power.ErrTokenBootSessionChanged):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// --- HTTP handlers ------------------------------------------------------

type powerHealthResponse struct {
	Severity             string `json:"severity"`
	Reason               string `json:"reason"`
	UndervoltageNow      bool   `json:"undervoltageNow"`
	ThrottledNow         bool   `json:"throttledNow"`
	UndervoltageOccurred bool   `json:"undervoltageOccurred"`
	ThrottledOccurred    bool   `json:"throttledOccurred"`

	// HasTrustedBatterySignal/HasRuntimeEstimate are always false on the
	// hardware this feature ships for - see power.Health's doc comment.
	HasTrustedBatterySignal bool     `json:"hasTrustedBatterySignal"`
	HasRuntimeEstimate      bool     `json:"hasRuntimeEstimate"`
	Notes                   []string `json:"notes"`

	PreviousSessionAvailable    bool   `json:"previousSessionAvailable"`
	PreviousSessionEndedCleanly bool   `json:"previousSessionEndedCleanly"`
	PreviousSessionNote         string `json:"previousSessionNote"`
}

func currentPowerHealth() power.Health {
	powerMonitorMu.Lock()
	defer powerMonitorMu.Unlock()
	return powerMonitor.Current()
}

// handleGetPowerHealthRequest serves GET /getPowerHealth: the debounced
// power-health reading (see power.Monitor, sampled once per health tick
// in main/health.go's updateHealth) plus the previous-session assessment
// captured once at startup - see initPower.
func handleGetPowerHealthRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	h := currentPowerHealth()
	powerPreviousSessionMu.Lock()
	assessment := powerPreviousSession
	powerPreviousSessionMu.Unlock()

	json.NewEncoder(w).Encode(powerHealthResponse{
		Severity:                    string(h.Severity),
		Reason:                      h.Reason,
		UndervoltageNow:             h.Throttle.UndervoltageNow,
		ThrottledNow:                h.Throttle.ThrottledNow,
		UndervoltageOccurred:        h.Throttle.UndervoltageOccurred,
		ThrottledOccurred:           h.Throttle.ThrottledOccurred,
		HasTrustedBatterySignal:     h.HasTrustedBatterySignal,
		HasRuntimeEstimate:          h.HasRuntimeEstimate,
		Notes:                       h.Notes,
		PreviousSessionAvailable:    assessment.Available,
		PreviousSessionEndedCleanly: assessment.EndedCleanly,
		PreviousSessionNote:         assessment.Note,
	})
}

// handleGetShutdownStatusRequest serves GET /getShutdownStatus.
func handleGetShutdownStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if shutdownManager == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"stage": string(power.StageIdle)})
		return
	}
	stage, lastErr := shutdownManager.Status()
	json.NewEncoder(w).Encode(map[string]interface{}{"stage": string(stage), "lastError": lastErr})
}

// handleRequestShutdownRequest serves POST /requestShutdown - step one of
// the two-step confirmed shutdown flow. Never mutates system state; only
// issues a short-lived, single-use, boot-session-bound confirmation token
// a client must present back to POST /confirmShutdown. Blocked (409)
// while an OTA update or configuration restore is in progress.
func handleRequestShutdownRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if shutdownManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "shutdown subsystem not initialized"})
		return
	}
	tok, err := shutdownManager.RequestConfirmation(newShutdownToken)
	if err != nil {
		writeJSON(w, statusForShutdownError(err), map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":            true,
		"token":              tok.Token,
		"expiresAtMonotonic": tok.ExpiresAtMonotonic,
	})
}

type confirmShutdownRequest struct {
	Token string `json:"token"`
}

// handleConfirmShutdownRequest serves POST /confirmShutdown - step two.
// The response describing the outcome is fully written and flushed to
// the client BEFORE this handler issues the actual power-off command, per
// this feature's explicit ordering requirement - see
// power.Manager.Confirm's doc comment. Blocked (409, without touching
// anything) if an OTA update or configuration restore has started since
// the matching requestShutdown call.
func handleConfirmShutdownRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if shutdownManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "shutdown subsystem not initialized"})
		return
	}
	var req confirmShutdownRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsRequestBytes) // reuse the same generous, already-established body-size bound
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "missing required field: token"})
		return
	}

	stage, err := shutdownManager.Confirm(req.Token)
	if err != nil {
		writeJSON(w, statusForShutdownError(err), map[string]interface{}{"success": false, "error": err.Error(), "stage": string(stage)})
		return
	}

	// stage is StageCommandIssued here. Record the clean close and send
	// the success response - flushed all the way to the client - before
	// actually issuing the power-off command below.
	markSessionClosed("controlled-shutdown")
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "stage": string(stage)})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	if err := shutdownManager.IssuePowerOff(); err != nil {
		log.Printf("power: IssuePowerOff failed: %s\n", err)
	}
}

// --- Diagnostics/recording-metadata integration -------------------------

// powerDiagnosticsSummary returns a bounded, sanitized summary for
// readiness.DiagnosticBundle.PowerSummary - never raw throttle register
// values beyond the four already-public booleans, and never anything
// about the shutdown flow's outstanding token.
func powerDiagnosticsSummary() interface{} {
	h := currentPowerHealth()
	powerPreviousSessionMu.Lock()
	assessment := powerPreviousSession
	powerPreviousSessionMu.Unlock()
	stage := power.StageIdle
	if shutdownManager != nil {
		stage, _ = shutdownManager.Status()
	}
	return map[string]interface{}{
		"severity":                    string(h.Severity),
		"undervoltageNow":             h.Throttle.UndervoltageNow,
		"throttledNow":                h.Throttle.ThrottledNow,
		"undervoltageOccurred":        h.Throttle.UndervoltageOccurred,
		"throttledOccurred":           h.Throttle.ThrottledOccurred,
		"previousSessionAvailable":    assessment.Available,
		"previousSessionEndedCleanly": assessment.EndedCleanly,
		"shutdownStage":               string(stage),
	}
}

// powerSnapshotForRecording returns the small set of power-health fields
// recording.SessionSnapshot's Power* fields capture once at recording
// start - see main/recordingmetadataapi.go's buildSessionSnapshot.
func powerSnapshotForRecording() (severity string, undervoltageNow, throttledNow bool, previousSessionEndedCleanly bool, previousSessionAvailable bool) {
	h := currentPowerHealth()
	powerPreviousSessionMu.Lock()
	assessment := powerPreviousSession
	powerPreviousSessionMu.Unlock()
	return string(h.Severity), h.Throttle.UndervoltageNow, h.Throttle.ThrottledNow, assessment.EndedCleanly, assessment.Available
}
