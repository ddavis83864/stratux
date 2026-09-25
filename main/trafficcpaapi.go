/*
trafficcpaapi.go: wires the pure trafficcpa package into the running
daemon as an ADDITIVE input to the existing operational-alerting
subsystem - see docs/traffic-cpa-alerting.md for the full design.

This file is the one place existing TrafficInfo/mySituation fields are
translated into trafficcpa.Track inputs - mirrors
main/alertingapi.go's buildTrafficObservation, which is where the
resulting trafficcpa.Result is attached to the alerting.TrafficObservation
this file's own computeTrafficCPA is called from.

Endpoints (all new, none replace or rename an existing one):

	GET  /getTrafficCPASettings  - current persisted settings
	POST /setTrafficCPASettings  - validate, persist, and apply new settings

CPA calculation itself runs INLINE, synchronously, inside
buildTrafficObservation - i.e. on the same goroutine and within the same
sendTrafficUpdates() cycle every other traffic observation is already
built on (main/traffic.go, ~1Hz). This is safe specifically because
trafficcpa.Compute is pure CPU-bound arithmetic (no filesystem, no
network, no lock beyond what the traffic loop already implicitly holds
via trafficMutex) with a small, fixed cost per target - the same
reasoning that already justifies buildTrafficObservation's own inline
relativeAltitudeFeetForAlerting/ClockDirectionFromRelativeBearing calls
immediately next to it. A CPA calculation failure can never stop traffic
processing: computeTrafficCPA never panics on its own (trafficcpa.Compute
is a pure function over already-validated Go values) and a genuinely
unexpected panic is still recovered per-target here, defense in depth.
*/
package main

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/stratux/stratux/alerting"
	"github.com/stratux/stratux/trafficcpa"
)

var (
	trafficCPAMu            sync.Mutex
	trafficCPASettingsCache TrafficCPASettings
)

// --- bounded, sanitized operational counters -----------------------------
//
// trafficCPAStatsMu guards every field below - a plain mutex, not atomics,
// since these are only ever touched once per computeTrafficCPA call
// (~1Hz times the current target count, never a hot per-message path) and
// a map is the simplest correct way to hold a small, fixed-vocabulary
// rejection-reason tally. Never persisted; reset only by a process
// restart, matching alerting.Counters' own "in-memory only" convention.
var (
	trafficCPAStatsMu            sync.Mutex
	trafficCPAEvaluations        uint64
	trafficCPAValidResults       uint64
	trafficCPARejectionsByReason map[trafficcpa.RejectReason]uint64
	trafficCPAEscalationCount    uint64 // CPA-triggered notices/cautions - see trafficCPARecordEscalation
	trafficCPAPanicCount         uint64
	trafficCPALastEvalMonotonic  float64
)

// trafficCPARecordEvaluation updates the bounded counters above from one
// completed Compute() call - never itself performs I/O.
func trafficCPARecordEvaluation(res trafficcpa.Result) {
	trafficCPAStatsMu.Lock()
	defer trafficCPAStatsMu.Unlock()
	trafficCPAEvaluations++
	trafficCPALastEvalMonotonic = monotonicSeconds()
	if res.Valid {
		trafficCPAValidResults++
		return
	}
	if trafficCPARejectionsByReason == nil {
		trafficCPARejectionsByReason = make(map[trafficcpa.RejectReason]uint64)
	}
	trafficCPARejectionsByReason[res.RejectReason]++
}

// trafficCPARecordRejection records a rejection decided BEFORE
// trafficcpa.Compute was even called (e.g. ownship GPS not valid at all)
// - kept as a distinct, explicit counter update (mirroring
// trafficCPARecordEvaluation's own rejection-by-reason bucket) so a
// caller-side precondition failure is never silently indistinguishable
// from one trafficcpa.Compute itself decided.
func trafficCPARecordRejection(reason trafficcpa.RejectReason) {
	trafficCPAStatsMu.Lock()
	defer trafficCPAStatsMu.Unlock()
	trafficCPAEvaluations++
	trafficCPALastEvalMonotonic = monotonicSeconds()
	if trafficCPARejectionsByReason == nil {
		trafficCPARejectionsByReason = make(map[trafficcpa.RejectReason]uint64)
	}
	trafficCPARejectionsByReason[reason]++
}

// trafficCPARecordPanic records that computeTrafficCPA's own recover()
// caught something - see that function's own doc comment for why this is
// never expected to actually increment in production.
func trafficCPARecordPanic() {
	trafficCPAStatsMu.Lock()
	defer trafficCPAStatsMu.Unlock()
	trafficCPAPanicCount++
}

// trafficCPARecordEscalation is called once per alerting.Event whose own
// CPAEscalated field is true - see submitTrafficObservationsForAlerting's
// caller in main/traffic.go, which inspects the events EvaluateTraffic
// returns immediately after calling it.
func trafficCPARecordEscalation() {
	trafficCPAStatsMu.Lock()
	defer trafficCPAStatsMu.Unlock()
	trafficCPAEscalationCount++
}

// TrafficCPADiagnosticsSummary is the bounded, sanitized summary embedded
// in diagnostic bundles - see docs/traffic-cpa-alerting.md's
// "Diagnostics" section. Never a raw target list or coordinates.
type TrafficCPADiagnosticsSummary struct {
	SchemaVersion            int                `json:"schemaVersion"`
	EscalationEnabled        bool               `json:"escalationEnabled"`
	Evaluations              uint64             `json:"evaluations"`
	ValidResults             uint64             `json:"validResults"`
	RejectionsByReason       map[string]uint64  `json:"rejectionsByReason,omitempty"`
	FallbackToExistingCount  uint64             `json:"fallbackToExistingPolicyCount"`
	EscalatedNoticeCount     uint64             `json:"escalatedAlertCount"`
	PanicCount               uint64             `json:"panicCount"`
	LastEvaluationAgeSeconds *float64           `json:"lastEvaluationAgeSeconds,omitempty"`
	Settings                 TrafficCPASettings `json:"settings"`
}

// trafficCPADiagnosticsSummary builds the bounded summary above - never
// panics (a zero-value settings struct plus zeroed counters if this
// feature has somehow not been initialized yet is still a valid,
// honestly-empty summary).
func trafficCPADiagnosticsSummary() TrafficCPADiagnosticsSummary {
	s := currentTrafficCPASettings()
	trafficCPAStatsMu.Lock()
	defer trafficCPAStatsMu.Unlock()
	sum := TrafficCPADiagnosticsSummary{
		SchemaVersion:           TrafficCPASettingsSchemaVersion,
		EscalationEnabled:       s.EscalationEnabled,
		Evaluations:             trafficCPAEvaluations,
		ValidResults:            trafficCPAValidResults,
		FallbackToExistingCount: trafficCPAEvaluations - trafficCPAValidResults,
		EscalatedNoticeCount:    trafficCPAEscalationCount,
		PanicCount:              trafficCPAPanicCount,
		Settings:                s,
	}
	if len(trafficCPARejectionsByReason) > 0 {
		sum.RejectionsByReason = make(map[string]uint64, len(trafficCPARejectionsByReason))
		for reason, count := range trafficCPARejectionsByReason {
			sum.RejectionsByReason[string(reason)] = count
		}
	}
	if trafficCPAEvaluations > 0 {
		age := monotonicSeconds() - trafficCPALastEvalMonotonic
		sum.LastEvaluationAgeSeconds = &age
	}
	return sum
}

// initTrafficCPA loads persisted settings into the process-lifetime cache
// - called from main() alongside initAlerting (order does not matter
// between the two; initTrafficCPA does not itself touch alertEvaluator,
// see mergedAlertingConfig, called separately by initAlerting/
// handleSetAlertSettingsRequest/handleSetTrafficCPASettingsRequest).
func initTrafficCPA() {
	trafficCPAMu.Lock()
	trafficCPASettingsCache = loadTrafficCPASettings()
	trafficCPAMu.Unlock()
}

func currentTrafficCPASettings() TrafficCPASettings {
	trafficCPAMu.Lock()
	defer trafficCPAMu.Unlock()
	return trafficCPASettingsCache
}

// mergedAlertingConfig builds the complete alerting.Config from BOTH
// AlertSettings and TrafficCPASettings - the two settings files are
// independently persisted and independently validated, but
// alerting.Evaluator holds exactly one Config, so every call site that
// used to call toAlertingConfig alone (main/alertingapi.go's
// initAlerting and handleSetAlertSettingsRequest) now calls this instead,
// and this file's own handleSetTrafficCPASettingsRequest calls it too -
// see docs/traffic-cpa-alerting.md's "Settings and defaults" section for
// why a single merged Config, rather than a second SetConfig-like call,
// is what keeps this consistent no matter which settings file changed
// most recently.
func mergedAlertingConfig(alertSettings AlertSettings, trafficAudio, systemAudio bool) alerting.Config {
	cfg := toAlertingConfig(alertSettings, trafficAudio, systemAudio)
	cpaSettings := currentTrafficCPASettings()
	cfg.CPAEscalationEnabled = cpaSettings.EscalationEnabled
	cfg.CPAMinClosureRateKnots = cpaSettings.MinClosureRateKnots
	return cfg
}

// ownshipAltitudeAndVerticalRateForCPA mirrors
// relativeAltitudeFeetForAlerting's own altitude-source precedence
// EXACTLY (AltIsGNSS+GPS, else pressure, else GPS) so the SAME reference
// backs both the existing current-relative-altitude calculation and this
// function's own vertical-rate pairing - see this file's package doc
// comment and docs/traffic-cpa-alerting.md's "Data sources" section for
// why mixing references would silently corrupt the vertical geometry.
// GPSVerticalSpeed is stored in feet PER SECOND (see mySituation's own
// field comment) and is converted to feet per minute here;
// BaroVerticalSpeed is already feet per minute (see main/gps.go's own
// NMEA-parsing call site).
func ownshipAltitudeAndVerticalRateForCPA(ti TrafficInfo) (altFeet float64, altValid bool, vertRateFPM float64, vertRateValid bool) {
	if ti.AltIsGNSS && isGPSValid() {
		return float64(mySituation.GPSHeightAboveEllipsoid), true, float64(mySituation.GPSVerticalSpeed) * 60, true
	}
	if isTempPressValid() {
		return float64(mySituation.BaroPressureAltitude), true, float64(mySituation.BaroVerticalSpeed), true
	}
	if isGPSValid() {
		return float64(mySituation.GPSAltitudeMSL), true, float64(mySituation.GPSVerticalSpeed) * 60, true
	}
	return 0, false, 0, false
}

// ownshipTrackForCPA builds ownship's trafficcpa.Track for evaluating
// against ti - the altitude/vertical-rate portion is recomputed per
// target (it depends on ti.AltIsGNSS, exactly like
// relativeAltitudeFeetForAlerting); position/ground-velocity do not vary
// within one sendTrafficUpdates() cycle but are cheap enough to recompute
// per call rather than thread an extra parameter through.
//
// GroundVelocityValid uses isGPSGroundTrackValid (accuracy < 30m, main/
// gps.go) - a stricter bar than plain isGPSValid, since an inaccurate
// ground track would silently corrupt the whole relative-velocity vector
// this package's entire calculation depends on.
func ownshipTrackForCPA(ti TrafficInfo) trafficcpa.Track {
	altFeet, altValid, vertFPM, vertValid := ownshipAltitudeAndVerticalRateForCPA(ti)
	return trafficcpa.Track{
		LatitudeDeg:         float64(mySituation.GPSLatitude),
		LongitudeDeg:        float64(mySituation.GPSLongitude),
		PositionValid:       isGPSValid(),
		AltitudeFeet:        altFeet,
		AltitudeValid:       altValid,
		TrackDegTrue:        float64(mySituation.GPSTrueCourse),
		SpeedKnots:          mySituation.GPSGroundSpeed,
		GroundVelocityValid: isGPSGroundTrackValid(),
		VerticalRateFPM:     vertFPM,
		VerticalRateValid:   vertValid,
		AgeSeconds:          stratuxClock.Since(mySituation.GPSLastFixLocalTime).Seconds(),
	}
}

// targetTrackForCPA builds one target's trafficcpa.Track from its
// TrafficInfo.
//
// VerticalRateValid is unconditionally false for every target - see
// "Target vertical-rate validity" below for why this codebase cannot
// prove a target's Vvel end-to-end for any live traffic source, and
// docs/traffic-cpa-alerting.md's "Vertical-rate behavior" section for the
// full design rationale. This means trafficcpa.Compute never uses a
// target's vertical rate: PredictedVerticalSeparationFeet/
// VerticalClosureRateFPM always report as explicitly unavailable
// (Confidence never exceeds ConfidenceMedium for a target-vertical
// reason), while CurrentVerticalSeparationFeet - which needs only both
// altitudes, not either vertical rate - is unaffected and still reported
// whenever available. Horizontal CPA calculation and escalation are also
// unaffected: cpaWarrantsEscalation's Confidence==Medium path already
// permits horizontal-only escalation.
//
// # Target vertical-rate validity
//
// An earlier version of this function gated VerticalRateValid on
// Speed_valid alone, reasoning that Speed_valid was the "same message
// class" that populates Vvel. A source-and-protocol audit for this
// mission found that reasoning does not hold end-to-end for any live
// traffic source this project decodes:
//
//   - 978 UAT (main/traffic.go's parseDownlinkReport): the DO-282
//     "vertical rate not available" sentinel is the raw 9-bit field
//     being all-zero, which this code converts to the plain numeric
//     value 0 - indistinguishable from a genuine zero climb rate, and no
//     bit survives anywhere to tell the two apart. Speed_valid is derived
//     from the N/S and E/W velocity subfields, an entirely independent
//     part of the message from vertical rate.
//   - 1090ES (main/traffic.go's parseDump1090Message): the dump978/
//     dump1090 JSON boundary DOES distinguish "vertical rate absent"
//     (nil) from "reported" via a nilable *int16 - but that distinction
//     is discarded at the merge into TrafficInfo.Vvel (a plain,
//     non-nilable int16) without ever setting a validity flag, so the
//     information is lost before this function ever sees it.
//   - Ping/uAvionix (main/ping.go): Vvel is set unconditionally from
//     ver_velocity even on the exact branch that just forced
//     Speed_valid=false (hor_velocity<=0) - proving Speed_valid and Vvel
//     reliability are set INDEPENDENTLY for this source, not coupled at
//     all, let alone coupled in the assumed direction.
//   - OGN (main/ogn.go) and FLARM (main/flarm-nmea.go): both hardcode
//     Speed_valid=true unconditionally alongside every Vvel update,
//     which asserts nothing about whether that update's own climb_mps/
//     vspeed field was itself meaningful.
//
// Since no live source can be shown to prove target vertical-rate
// validity, and no source can be excluded either (the failure mode is
// present on every source examined), the conservative correction is to
// never let a target's vertical rate reach trafficcpa at all, rather
// than trust an unproven per-source or per-field heuristic. This is a
// real reduction in the feature's predicted-vertical-separation coverage
// for targets (it was previously computed whenever Speed_valid was
// true), traded for not silently treating an unprovable value as
// trustworthy input to a safety-adjacent estimate.
//
// ti.Alt==0 is treated as "altitude unknown," mirroring
// relativeAltitudeFeetForAlerting's own identical, already-accepted
// convention in this codebase.
func targetTrackForCPA(ti TrafficInfo) trafficcpa.Track {
	return trafficcpa.Track{
		LatitudeDeg:         float64(ti.Lat),
		LongitudeDeg:        float64(ti.Lng),
		PositionValid:       ti.Position_valid,
		AltitudeFeet:        float64(ti.Alt),
		AltitudeValid:       ti.Alt != 0,
		TrackDegTrue:        float64(ti.Track),
		SpeedKnots:          float64(ti.Speed),
		GroundVelocityValid: ti.Speed_valid,
		VerticalRateFPM:     float64(ti.Vvel),
		VerticalRateValid:   false,
		AgeSeconds:          ti.Age,
	}
}

// trafficCPAConfigForCurrentAlertingEnvelope returns a trafficcpa.Config
// built from the persisted CPA settings, EXCEPT
// MaxHorizontalSeparationMeters, which is instead derived dynamically
// from the CURRENT alerting.Config's own MonitoringHorizontalMeters -
// this project's alerting monitoring envelope and this package's own
// computation envelope must never be allowed to drift apart (there is no
// value in computing a CPA trend for a target the alerting system would
// never evaluate to begin with, and a narrower CPA envelope than the
// alerting one would silently disable escalation for a target near the
// outer edge of what alerting itself still monitors).
//
// monitoringHorizontalMeters is passed in already-resolved by the caller
// (computeTrafficCPA reads it from alertEvaluator's own in-memory Config
// via Evaluator.Config() - never a fresh settings-file read) so this
// function itself stays a pure, allocation-only translation with no
// opinion on where the value came from.
func trafficCPAConfigForCurrentAlertingEnvelope(s TrafficCPASettings, monitoringHorizontalMeters float64) trafficcpa.Config {
	cfg := trafficcpa.DefaultConfig()
	cfg.HorizonSeconds = s.HorizonSeconds
	cfg.MinRelativeSpeedKnots = s.MinRelativeSpeedKnots
	cfg.MaxHorizontalSeparationMeters = monitoringHorizontalMeters
	return cfg
}

// computeTrafficCPA is buildTrafficObservation's own one call site into
// this package - returns nil only if trafficcpa.Compute itself panics
// (never expected: it is a pure function over already-constructed Track
// values), so a defect here can never propagate into, or stop, the
// surrounding traffic-evaluation cycle. See this file's own package doc
// comment for why running this inline is safe.
//
// Reads ONLY in-memory state - currentTrafficCPASettings() (this
// process's own settings cache) and, when alertEvaluator has already
// been constructed, alertEvaluator.Config() (a mutex-guarded copy of its
// own already-applied Config, never re-read from disk). This function is
// called once per non-ownship target, every ~1Hz sendTrafficUpdates
// cycle, while sendTrafficUpdates itself holds trafficMutex - a prior
// version of this function called loadAlertSettings(), which performs a
// real os.ReadFile per call, meaning a genuine filesystem read under
// trafficMutex for every tracked target, every cycle. That was a defect
// against this feature's own zero-filesystem-I/O-in-the-traffic-
// evaluation-path requirement, found and fixed before deployment; there
// is no need to reconstruct AlertSettings at all here, since the only
// value ever needed from it (MonitoringHorizontalMeters) is already
// sitting in alertEvaluator's own in-memory Config, kept current by
// every settings-change call site (initAlerting,
// handleSetAlertSettingsRequest, handleSetTrafficCPASettingsRequest),
// each of which legitimately reads the settings file once, off this hot
// path, exactly as before.
func computeTrafficCPA(ti TrafficInfo) (result *trafficcpa.Result) {
	defer func() {
		if r := recover(); r != nil {
			trafficCPARecordPanic()
			result = nil
		}
	}()
	if !isGPSValid() {
		trafficCPARecordRejection(trafficcpa.ReasonOwnshipPositionInvalid)
		return nil
	}
	own := ownshipTrackForCPA(ti)
	target := targetTrackForCPA(ti)

	monitoringHorizontalMeters := alerting.DefaultConfig().MonitoringHorizontalMeters
	if alertEvaluator != nil {
		monitoringHorizontalMeters = alertEvaluator.Config().MonitoringHorizontalMeters
	}
	cpaSettings := currentTrafficCPASettings()
	cfg := trafficCPAConfigForCurrentAlertingEnvelope(cpaSettings, monitoringHorizontalMeters)

	res := trafficcpa.Compute(own, target, cfg)
	trafficCPARecordEvaluation(res)
	return &res
}

// trafficCPASnapshotForRecording returns the small set of CPA
// configuration fields main/recordingmetadataapi.go's buildSessionSnapshot
// embeds into a recording's immutable start snapshot - see
// docs/traffic-cpa-alerting.md's "Recording integration" section. Pure
// read, no locks beyond currentTrafficCPASettings' own short-held one.
func trafficCPASnapshotForRecording() (schemaVersion int, escalationEnabled bool, horizonSeconds, minClosureRateKnots float64) {
	s := currentTrafficCPASettings()
	return TrafficCPASettingsSchemaVersion, s.EscalationEnabled, s.HorizonSeconds, s.MinClosureRateKnots
}

// --- Settings API --------------------------------------------------------

const maxTrafficCPARequestBytes = 4096

func handleGetTrafficCPASettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(currentTrafficCPASettings())
}

func handleSetTrafficCPASettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTrafficCPARequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var s TrafficCPASettings
	if err := dec.Decode(&s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "request body must contain exactly one JSON value"})
		return
	}
	s.SchemaVersion = TrafficCPASettingsSchemaVersion
	if err := s.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if err := saveTrafficCPASettings(s); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	trafficCPAMu.Lock()
	trafficCPASettingsCache = s
	trafficCPAMu.Unlock()

	// Apply immediately, matching every other settings endpoint in this
	// project - never any disk I/O beyond the settings file just written
	// above, and never a block on the traffic-evaluation path (SetConfig
	// only replaces alertEvaluator's own in-memory Config under its own
	// short-held mutex).
	if alertEvaluator != nil {
		alertSettings := loadAlertSettings()
		cfg := mergedAlertingConfig(alertSettings, alertSettings.BrowserAudioEnabled, alertSettings.SystemAudioEnabled)
		if err := alertEvaluator.SetConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "settings": s})
}
