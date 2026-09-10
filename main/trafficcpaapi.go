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
// Known, disclosed limitation (see docs/traffic-cpa-alerting.md's "Known
// limitations" section): this codebase has no dedicated validity flag
// for Vvel (unlike Speed_valid for Speed) - a genuinely level target and
// a target whose vertical rate was simply never reported are both
// represented as Vvel==0. VerticalRateValid is gated on Speed_valid
// alone (the same message class that populates Vvel for every live
// traffic source in this project) rather than also excluding Vvel==0,
// which would wrongly treat every genuinely level target - the single
// most common case - as unknown. This is a deliberate, conservative-in-
// the-other-direction choice: a wrongly-trusted zero vertical rate for a
// target that is actually climbing/descending without having reported it
// yet would, at worst, predict a vertical separation that itself decays
// back toward the target's CURRENT (always trustworthy) vertical
// separation as fresher updates arrive - it is not persisted, and it can
// never on its own escalate past a single tier (see
// alerting.cpaWarrantsEscalation).
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
		VerticalRateValid:   ti.Speed_valid,
		AgeSeconds:          ti.Age,
	}
}

// trafficCPAConfigForCurrentAlertingEnvelope returns a trafficcpa.Config
// built from the persisted CPA settings, EXCEPT
// MaxHorizontalSeparationMeters, which is instead derived dynamically
// from alertCfg's CURRENT MonitoringHorizontalMeters - this project's
// alerting monitoring envelope and this package's own computation
// envelope must never be allowed to drift apart (there is no value in
// computing a CPA trend for a target the alerting system would never
// evaluate to begin with, and a narrower CPA envelope than the alerting
// one would silently disable escalation for a target near the outer edge
// of what alerting itself still monitors).
func trafficCPAConfigForCurrentAlertingEnvelope(s TrafficCPASettings, alertCfg alerting.Config) trafficcpa.Config {
	cfg := trafficcpa.DefaultConfig()
	cfg.HorizonSeconds = s.HorizonSeconds
	cfg.MinRelativeSpeedKnots = s.MinRelativeSpeedKnots
	cfg.MaxHorizontalSeparationMeters = alertCfg.MonitoringHorizontalMeters
	return cfg
}

// computeTrafficCPA is buildTrafficObservation's own one call site into
// this package - returns nil only if trafficcpa.Compute itself panics
// (never expected: it is a pure function over already-constructed Track
// values), so a defect here can never propagate into, or stop, the
// surrounding traffic-evaluation cycle. See this file's own package doc
// comment for why running this inline is safe.
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

	alertSettings := loadAlertSettings()
	alertCfg := mergedAlertingConfig(alertSettings, alertSettings.BrowserAudioEnabled, alertSettings.SystemAudioEnabled)
	cpaSettings := currentTrafficCPASettings()
	cfg := trafficCPAConfigForCurrentAlertingEnvelope(cpaSettings, alertCfg)

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
