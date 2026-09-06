/*
alertingapi.go: wires the pure alerting package into the running daemon -
see docs/alerting.md for the full design.

Endpoints (all new, none replace or rename an existing one):

	GET  /getAlerts             - current active alerts + bounded recent history
	GET  /getAlertSettings      - current persisted settings
	POST /setAlertSettings      - replace persisted settings (validated, atomic)
	POST /acknowledgeAlert?id=... - acknowledge one traffic target or health component
	POST /muteAlerts            - mute audio (optionally timed, bounded)
	POST /unmuteAlerts          - clear mute
	POST /testAlertSound        - client-side only; server just confirms audio is armed

Traffic ingestion (main/traffic.go's sendTrafficUpdates, 1Hz) never blocks on
alert evaluation: observations are sent over a bounded, nonblocking channel
to a single background goroutine that owns the alertEvaluator. A full
channel drops the cycle (counted, never an error) rather than stall
sendTrafficUpdates - see docs/alerting.md's "Failure isolation" section.
*/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/stratux/stratux/alerting"
)

const maxAlertRequestBytes = 4096

// alertTrafficChanCapacity is deliberately small (1) - alert evaluation
// only ever needs the MOST RECENT cycle's observations; queuing stale
// cycles would only delay, not improve, alerting.
const alertTrafficChanCapacity = 1

var (
	alertEvaluator    *alerting.Evaluator
	alertTrafficChan  chan []alerting.TrafficObservation
	alertSettingsRWMu sync.Mutex // guards read-modify-write of the persisted settings file only
)

// initAlerting constructs the evaluator from persisted settings, re-arms
// any still-valid persisted mute, and starts the background evaluation
// goroutine. Called from main() after initPreflight() - see
// main/gen_gdl90.go.
func initAlerting() {
	settings := loadAlertSettings()
	cfg := toAlertingConfig(settings, settings.BrowserAudioEnabled, settings.SystemAudioEnabled)
	alertEvaluator = alerting.NewEvaluator(cfg, func() time.Time { return stratuxClock.Time })

	if settings.Muted {
		if settings.MutedIndefinitely {
			alertEvaluator.SetMuted(true, time.Time{})
		} else if settings.MuteUntilUnixSeconds > time.Now().Unix() {
			remaining := time.Duration(settings.MuteUntilUnixSeconds-time.Now().Unix()) * time.Second
			alertEvaluator.SetMuted(true, stratuxClock.Time.Add(remaining))
		}
	}

	alertTrafficChan = make(chan []alerting.TrafficObservation, alertTrafficChanCapacity)
	go alertTrafficEvaluationLoop()
	go alertHealthEvaluationLoop()
	log.Println("alerting: subsystem initialized")
}

// alertTrafficEvaluationLoop is the sole goroutine that ever calls
// alertEvaluator.EvaluateTraffic - keeps the Evaluator's own internal
// locking uncontended by anything but this loop and the HTTP handlers
// below (which only ever call read-only/idempotent methods).
func alertTrafficEvaluationLoop() {
	for observations := range alertTrafficChan {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("alerting: traffic evaluation panicked (recovered): %v\n", r)
				}
			}()
			alertEvaluator.EvaluateTraffic(observations, isGPSValid())
		}()
	}
}

// alertHealthEvaluationLoop periodically evaluates system-health
// transitions - matches readiness's own healthUpdateInterval cadence
// (5s) since there is no value polling more often than the underlying
// health report itself is recomputed.
func alertHealthEvaluationLoop() {
	ticker := time.NewTicker(healthUpdateInterval)
	defer ticker.Stop()
	for range ticker.C {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("alerting: health evaluation panicked (recovered): %v\n", r)
				}
			}()
			snap := buildHealthSnapshotForAlerting()
			alertEvaluator.EvaluateHealth(snap)
		}()
	}
}

// submitTrafficObservationsForAlerting is called once per
// sendTrafficUpdates() cycle (main/traffic.go) with every non-ignored
// target's freshly-recomputed observation. Never blocks: a full channel
// means the previous cycle hasn't been picked up yet, so this cycle is
// dropped (counted) rather than stall traffic ingestion.
func submitTrafficObservationsForAlerting(observations []alerting.TrafficObservation) {
	if alertTrafficChan == nil { // alerting not yet initialized (e.g. in a test binary)
		return
	}
	select {
	case alertTrafficChan <- observations:
	default:
		if alertEvaluator != nil {
			alertEvaluator.RecordDroppedEvaluation()
		}
	}
}

// relativeAltitudeFeetForAlerting derives ti's altitude relative to
// ownship, in feet, using the same altitude-source precedence as
// main/flarm-nmea.go's computeRelativeVertical (AltIsGNSS+GPS, else
// pressure, else GPS) - but, unlike that function, explicitly reports
// invalid (rather than silently comparing against a zero-value ownship
// altitude) whenever no trustworthy ownship altitude source is available.
func relativeAltitudeFeetForAlerting(ti TrafficInfo) (float64, bool) {
	var ownAlt float32
	have := false
	if ti.AltIsGNSS && isGPSValid() {
		ownAlt = mySituation.GPSHeightAboveEllipsoid
		have = true
	} else if isTempPressValid() {
		ownAlt = mySituation.BaroPressureAltitude
		have = true
	} else if isGPSValid() {
		ownAlt = mySituation.GPSAltitudeMSL
		have = true
	}
	if !have || ti.Alt == 0 {
		return 0, false
	}
	return float64(ti.Alt) - float64(ownAlt), true
}

// buildTrafficObservation converts one TrafficInfo (plus the caller's
// already-computed ownship-exclusion result) into an
// alerting.TrafficObservation - the one place existing traffic fields are
// translated into alerting inputs. See docs/alerting.md's "Traffic data
// inputs" section for the field-by-field justification.
func buildTrafficObservation(ti TrafficInfo, isOwnshipTi bool) alerting.TrafficObservation {
	relAlt, relAltValid := relativeAltitudeFeetForAlerting(ti)

	clockValid := false
	var clockDir alerting.ClockDirection
	if ti.BearingDist_valid && isGPSValid() {
		relBearing := ti.Bearing - float64(mySituation.GPSTrueCourse)
		clockDir = alerting.ClockDirectionFromRelativeBearing(relBearing)
		clockValid = true
	}

	return alerting.TrafficObservation{
		TargetID:              fmt.Sprintf("%06X", ti.Icao_addr&0xFFFFFF),
		IsOwnship:             isOwnshipTi,
		PositionValid:         ti.Position_valid,
		DistanceValid:         ti.BearingDist_valid,
		DistanceMeters:        ti.Distance,
		RelativeAltitudeValid: relAltValid,
		RelativeAltitudeFeet:  relAlt,
		AgeSeconds:            ti.Age,
		OnGround:              ti.OnGround,
		ClockDirectionValid:   clockValid,
		ClockDirection:        clockDir,
	}
}

// AlertingDiagnosticsSummary is the bounded, sanitized summary embedded in
// diagnostic bundles - see docs/alerting.md's "Diagnostics" section. Never
// the full active-alert or event list.
type AlertingDiagnosticsSummary struct {
	SchemaVersion         int            `json:"schemaVersion"`
	MasterEnabled         bool           `json:"masterEnabled"`
	BrowserAudioEnabled   bool           `json:"browserAudioEnabled"`
	SystemAudioEnabled    bool           `json:"systemAudioEnabled"`
	Muted                 bool           `json:"muted"`
	CountsByLevel         map[string]int `json:"countsByLevel"`
	RecentEventCount      int            `json:"recentEventCount"`
	SuppressedEvents      int            `json:"suppressedEvents"`
	DroppedEvaluations    int            `json:"droppedEvaluations"`
	StaleTargetRejections int            `json:"staleTargetRejections"`
	TrackedTargetCount    int            `json:"trackedTargetCount"`
}

// alertingDiagnosticsSummary builds the bounded summary above from the
// current evaluator snapshot and persisted settings - never the full
// active-alert list. Returns a zero-value summary (not nil, never a panic)
// if alerting has not been initialized yet (e.g. very early startup).
func alertingDiagnosticsSummary() AlertingDiagnosticsSummary {
	settings := loadAlertSettings()
	sum := AlertingDiagnosticsSummary{
		SchemaVersion:       AlertSettingsSchemaVersion,
		MasterEnabled:       settings.MasterEnabled,
		BrowserAudioEnabled: settings.BrowserAudioEnabled,
		SystemAudioEnabled:  settings.SystemAudioEnabled,
		CountsByLevel:       map[string]int{},
	}
	if alertEvaluator == nil {
		return sum
	}
	snap := alertEvaluator.Snapshot()
	sum.Muted = snap.Muted
	sum.RecentEventCount = len(snap.RecentHistory)
	sum.SuppressedEvents = snap.Counters.SuppressedEvents
	sum.DroppedEvaluations = snap.Counters.DroppedEvaluations
	sum.StaleTargetRejections = snap.Counters.StaleTargetRejections
	sum.TrackedTargetCount = snap.TrackedTargetCount
	for _, a := range snap.Active {
		sum.CountsByLevel[string(a.Level)]++
	}
	return sum
}

// alertingSnapshotForRecording returns the small set of alerting fields
// main/recordingmetadataapi.go's buildSessionSnapshot embeds into a
// recording's immutable start snapshot - see docs/alerting.md's "Recording
// integration" section. Pure read, no recMu involvement (recMu is a
// recording-package concept this file never touches).
func alertingSnapshotForRecording() (schemaVersion int, masterEnabled, visualEnabled, audioArmed, systemEnabled, muted bool, countsByLevel map[string]int) {
	settings := loadAlertSettings()
	schemaVersion = AlertSettingsSchemaVersion
	masterEnabled = settings.MasterEnabled
	visualEnabled = settings.VisualTrafficNoticesEnabled
	systemEnabled = settings.SystemVisualEnabled
	countsByLevel = map[string]int{}
	if alertEvaluator == nil {
		return
	}
	snap := alertEvaluator.Snapshot()
	muted = snap.Muted
	audioArmed = settings.BrowserAudioEnabled && !muted
	for _, a := range snap.Active {
		countsByLevel[string(a.Level)]++
	}
	return
}

// maxRecordingAlertEvents bounds the alert-event tail captured into a
// recording's finalization metadata - see docs/alerting.md's "Recording
// integration" section. Deliberately smaller than the evaluator's own
// MaxEventHistory: a single recording's worth of context is enough,
// bounding the metadata file size regardless of how long the daemon has
// been running overall.
const maxRecordingAlertEvents = 50

// recentAlertEventsForRecording returns a bounded tail of the alerting
// subsystem's recent event history, for embedding into
// recording.SessionFinalization.AlertEvents at recording stop time. Pure
// read; never touches recMu.
func recentAlertEventsForRecording() []alerting.Event {
	if alertEvaluator == nil {
		return nil
	}
	hist := alertEvaluator.Snapshot().RecentHistory
	if len(hist) > maxRecordingAlertEvents {
		hist = hist[len(hist)-maxRecordingAlertEvents:]
	}
	return hist
}

// --- HTTP API ------------------------------------------------------------

func handleGetAlertsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if alertEvaluator == nil {
		http.Error(w, "alerting subsystem not initialized", http.StatusServiceUnavailable)
		return
	}
	json.NewEncoder(w).Encode(alertEvaluator.Snapshot())
}

func handleGetAlertSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(loadAlertSettings())
}

func handleSetAlertSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	alertSettingsRWMu.Lock()
	defer alertSettingsRWMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, maxAlertRequestBytes)
	var s AlertSettings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return
	}
	s.SchemaVersion = AlertSettingsSchemaVersion
	if err := s.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if err := saveAlertSettings(s); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if alertEvaluator != nil {
		cfg := toAlertingConfig(s, s.BrowserAudioEnabled, s.SystemAudioEnabled)
		if err := alertEvaluator.SetConfig(cfg); err != nil {
			// Should never happen - Validate() above already checked this
			// exact config - but never silently ignore a real error.
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "settings": s})
}

func handleAcknowledgeAlertRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if alertEvaluator == nil {
		http.Error(w, "alerting subsystem not initialized", http.StatusServiceUnavailable)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "missing required parameter: id"})
		return
	}
	// Idempotent: acknowledging an unknown/already-expired id is a
	// harmless no-op (200, success:false is not an error state a client
	// need retry-loop over), matching this project's existing
	// idempotent-acknowledge conventions (see preflight's manual checks).
	found := alertEvaluator.Acknowledge(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "found": found})
}

type muteRequest struct {
	DurationSeconds float64 `json:"durationSeconds,omitempty"` // 0 or omitted = indefinite
}

func handleMuteAlertsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if alertEvaluator == nil {
		http.Error(w, "alerting subsystem not initialized", http.StatusServiceUnavailable)
		return
	}
	var req muteRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxAlertRequestBytes)
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
			return
		}
	}
	if math.IsNaN(req.DurationSeconds) || math.IsInf(req.DurationSeconds, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "durationSeconds must be a finite number"})
		return
	}
	if req.DurationSeconds < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "durationSeconds must not be negative"})
		return
	}
	// Compared directly in float64 seconds, never converted to a
	// nanosecond-based time.Duration first: an excessively large
	// client-supplied value would overflow that conversion (see
	// main/alertsettings.go's identical fix, caught by
	// TestAlertSettings_ExcessiveMuteDurationRejected) and could wrap
	// around to a small value that wrongly passes this check.
	if req.DurationSeconds > maxMuteDuration.Seconds() {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": fmt.Sprintf("durationSeconds exceeds the maximum of %s", maxMuteDuration)})
		return
	}
	dur := time.Duration(req.DurationSeconds * float64(time.Second))

	settings := loadAlertSettings()
	settings.Muted = true
	nowUnix := time.Now().Unix()
	if dur <= 0 {
		settings.MutedIndefinitely = true
		settings.MuteUntilUnixSeconds = 0
		alertEvaluator.SetMuted(true, time.Time{})
	} else {
		settings.MutedIndefinitely = false
		settings.MuteUntilUnixSeconds = nowUnix + int64(dur.Seconds())
		alertEvaluator.SetMuted(true, stratuxClock.Time.Add(dur))
	}
	if err := saveAlertSettings(settings); err != nil {
		log.Printf("alerting: could not persist mute state: %s\n", err)
		// Persistence failure must not prevent the mute itself from
		// taking effect immediately (already applied above).
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "muted": true})
}

func handleUnmuteAlertsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if alertEvaluator == nil {
		http.Error(w, "alerting subsystem not initialized", http.StatusServiceUnavailable)
		return
	}
	alertEvaluator.SetMuted(false, time.Time{})
	settings := loadAlertSettings()
	settings.Muted = false
	settings.MutedIndefinitely = false
	settings.MuteUntilUnixSeconds = 0
	if err := saveAlertSettings(settings); err != nil {
		log.Printf("alerting: could not persist unmute state: %s\n", err)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "muted": false})
}

// handleTestAlertSoundRequest is deliberately a no-op beyond confirming
// the subsystem is reachable: the actual tone is generated entirely
// client-side (Web Audio API) and must never create a real alert event -
// see docs/alerting.md's browser-audio section.
func handleTestAlertSoundRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}
