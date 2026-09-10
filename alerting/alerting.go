// Package alerting implements conservative, supplemental operational
// alerting for nearby traffic and Stratux system-health transitions - see
// docs/alerting.md for the full design.
//
// This package is deliberately pure: it opens no sockets, performs no disk
// I/O, and never reads or touches global daemon state, GDL90 output, or
// FLARM output directly. It only ever sees data explicitly passed into
// EvaluateTraffic/EvaluateHealth, and returns Alert values for the caller
// (main/alertingapi.go) to persist, serve, and display. It never issues
// maneuver guidance - every alert is a neutral, supplemental notice.
package alerting

import "time"

// SchemaVersion is bumped whenever Config, Alert, or Event gains, removes,
// or changes the meaning of a field a consumer should notice.
// SchemaVersion 2 added Config.CPAEscalationEnabled/CPAMinClosureRateKnots
// and Alert's CPA* fields (see docs/traffic-cpa-alerting.md) - purely
// additive: a version-1 reader ignores the new Alert fields, and the new
// Config fields default to their safe (disabled) zero values.
const SchemaVersion = 2

// Level is the operator-facing alert severity - deliberately a small,
// fixed vocabulary that can never be confused with maneuver guidance (no
// RESOLUTION_ADVISORY, no CLIMB/DESCEND/TURN/AVOID).
type Level string

const (
	LevelInformation    Level = "INFORMATION"
	LevelTrafficNotice  Level = "TRAFFIC_NOTICE"
	LevelTrafficCaution Level = "TRAFFIC_CAUTION"
	LevelSystemCaution  Level = "SYSTEM_CAUTION"
	LevelSystemNotReady Level = "SYSTEM_NOT_READY"
)

// Valid reports whether l is one of the fixed set of defined levels.
func (l Level) Valid() bool {
	switch l {
	case LevelInformation, LevelTrafficNotice, LevelTrafficCaution, LevelSystemCaution, LevelSystemNotReady:
		return true
	}
	return false
}

// Category distinguishes traffic alerts from system-health alerts.
type Category string

const (
	CategoryTraffic Category = "traffic"
	CategorySystem  Category = "system"
)

// ClockDirection is a coarse, human-readable relative bearing - never a
// precision instrument reading, only ever included when the underlying
// bearing is itself valid and fresh.
type ClockDirection string

// clockDirections is the fixed 12-point clock-face vocabulary.
var clockDirections = [12]ClockDirection{
	"12 o'clock", "1 o'clock", "2 o'clock", "3 o'clock", "4 o'clock", "5 o'clock",
	"6 o'clock", "7 o'clock", "8 o'clock", "9 o'clock", "10 o'clock", "11 o'clock",
}

// ClockDirectionFromRelativeBearing converts a relative bearing (target
// bearing minus ownship track, degrees, any range) into a 12-point clock
// direction. Always succeeds - callers are responsible for only calling
// this when the underlying bearing/track values are themselves valid.
func ClockDirectionFromRelativeBearing(relativeBearingDeg float64) ClockDirection {
	norm := relativeBearingDeg
	for norm < 0 {
		norm += 360
	}
	for norm >= 360 {
		norm -= 360
	}
	idx := int(norm/30.0+0.5) % 12
	return clockDirections[idx]
}

// Alert is one currently-active or recently-active alert, sanitized for
// direct API/dashboard/diagnostics exposure - see docs/alerting.md's API
// section for the exact exclusions (no exact coordinates, no MAC
// addresses, no secrets).
type Alert struct {
	ID       string   `json:"id"`
	Category Category `json:"category"`
	Level    Level    `json:"level"`

	// TargetID is a sanitized, already-public identifier (the same
	// hex-ICAO-shaped string already broadcast in the clear by ADS-B and
	// already shown on the existing Traffic page) for traffic alerts;
	// empty for system alerts.
	TargetID string `json:"targetId,omitempty"`
	// Component names the health component for a system alert (e.g.
	// "storage", "gps"); empty for traffic alerts.
	Component string `json:"component,omitempty"`

	Message string `json:"message"`

	// DistanceMeters/RelativeAltitudeFeet/ClockDirection are omitted
	// (zero value + *Valid flag false) whenever the underlying data is
	// not valid or not fresh - never a fabricated number.
	DistanceMeters        float64        `json:"distanceMeters,omitempty"`
	DistanceValid         bool           `json:"distanceValid"`
	RelativeAltitudeFeet  float64        `json:"relativeAltitudeFeet,omitempty"`
	RelativeAltitudeValid bool           `json:"relativeAltitudeValid"`
	ClockDirection        ClockDirection `json:"clockDirection,omitempty"`
	ClockDirectionValid   bool           `json:"clockDirectionValid"`

	// CPA* fields carry this target's closure-rate/closest-point-of-
	// approach trend estimate, when one was computed (see
	// TrafficObservation.CPA and docs/traffic-cpa-alerting.md) - always
	// present with CPAValid=false/omitted numeric values for a system
	// alert, or for a traffic alert with no CPA estimate available.
	// Never a fabricated number: every numeric field here is paired with
	// its own *Valid flag exactly like DistanceMeters/
	// RelativeAltitudeFeet above.
	CPAValid        bool   `json:"cpaValid"`
	CPAConfidence   string `json:"cpaConfidence,omitempty"`
	CPARejectReason string `json:"cpaRejectReason,omitempty"`

	CPAClosureRateKnots float64 `json:"cpaClosureRateKnots,omitempty"`
	CPAClosureRateValid bool    `json:"cpaClosureRateValid"`

	CPATCPASeconds          float64 `json:"cpaTcpaSeconds,omitempty"`
	CPATCPAValid            bool    `json:"cpaTcpaValid"`
	CPATCPAClampedToHorizon bool    `json:"cpaTcpaClampedToHorizon,omitempty"`

	CPAPredictedHorizontalMeters float64 `json:"cpaPredictedHorizontalMeters,omitempty"`
	CPAPredictedHorizontalValid  bool    `json:"cpaPredictedHorizontalValid"`
	CPAPredictedVerticalFeet     float64 `json:"cpaPredictedVerticalFeet,omitempty"`
	CPAPredictedVerticalValid    bool    `json:"cpaPredictedVerticalValid"`

	CPATrend string `json:"cpaTrend,omitempty"`
	// CPAEscalated is true only when a valid CPA estimate actually raised
	// this alert's tier above what distance/altitude alone produced -
	// see classifyTier's own doc comment. Always false when
	// CPAEscalationEnabled is false or CPA is unavailable/invalid.
	CPAEscalated bool `json:"cpaEscalated"`

	FirstSeenAtMono   time.Time `json:"-"`
	LastUpdatedAtMono time.Time `json:"-"`
	// FirstSeenAgeSeconds/LastUpdatedAgeSeconds are populated at snapshot
	// time (relative to "now"), not stored absolute monotonic instants,
	// so a client never needs to reason about the server's own monotonic
	// epoch.
	FirstSeenAgeSeconds   float64 `json:"firstSeenAgeSeconds"`
	LastUpdatedAgeSeconds float64 `json:"lastUpdatedAgeSeconds"`

	Acknowledged bool `json:"acknowledged"`

	// AudioEligible reports only that this alert currently qualifies to
	// produce a tone under the configured cooldowns - never a claim that
	// any sound was physically produced or heard.
	AudioEligible bool `json:"audioEligible"`
	// SuppressionReason is set (and AudioEligible false) when this alert
	// was rate-limited rather than silent by configuration - e.g.
	// "per-target-cooldown", "global-min-spacing", "audio-disabled",
	// "muted".
	SuppressionReason string `json:"suppressionReason,omitempty"`

	// DataValidity summarizes why this alert's numeric fields are or are
	// not populated - "fresh", "stale-excluded", "altitude-unavailable".
	DataValidity string `json:"dataValidity"`

	Disclaimer string `json:"disclaimer"`
}

// Disclaimer is the exact required safety statement, included on every
// Alert and every Snapshot so no consumer can drift from it.
const Disclaimer = "Supplemental, non-certified situational-awareness notice. Not collision avoidance, not TCAS/ACAS, not a resolution advisory, and never a substitute for visual scanning, ATC, ForeFlight, or pilot judgment. Issues no maneuver guidance."

// Event is one bounded, historical record of a material alert transition
// (a new alert, an escalation, a de-escalation, or a recovery) - not a
// per-cycle heartbeat.
type Event struct {
	Alert
	Kind string `json:"kind"` // "new", "escalated", "deescalated", "cleared", "recovered", "expired"
	// Seq is a per-Evaluator, monotonically increasing sequence number
	// assigned when the event is recorded - never reused, never reset
	// except by a daemon restart (a fresh Evaluator). Lets a polling
	// client reliably tell "already saw this" from "new since last poll"
	// without relying on Alert.ID (which is reused across many distinct
	// events for the same target over its lifetime) or on any
	// age/timestamp field (which changes every poll by construction).
	Seq int64 `json:"seq"`
}
