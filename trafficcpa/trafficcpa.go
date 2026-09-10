// Package trafficcpa computes closure-rate and closest-point-of-approach
// (CPA) trend estimates from two aircraft's kinematic states - ownship and
// one traffic target. It is a pure, deterministic mathematics package:
// no networking, filesystem, hardware, global state, or wall-clock
// access, and no dependency beyond the Go standard library. Every input,
// including data freshness, is passed in explicitly by the caller
// (main/trafficcpaapi.go builds a Track from an existing main.TrafficInfo
// and mySituation) and every output makes explicit whether it is
// trustworthy - see docs/traffic-cpa-alerting.md for the full design.
//
// This package computes a TREND, not a prediction to be acted on. It
// never issues maneuver guidance and is not a collision-avoidance,
// TCAS/ACAS, or resolution-advisory system. Its only purpose is to answer,
// as an ADDITIVE, conservative input to this project's existing
// distance/altitude traffic alerting (see the alerting package): "is this
// target's own reported trajectory converging with ownship's, and if so,
// roughly when and how close." A Result with Valid=false must never be
// treated as "no target," "zero separation," or "zero closure rate" -
// callers must fall back to whatever their own policy already does
// without a CPA estimate (see alerting.classifyTier's own CPA-escalation
// logic, which never lowers a tier and never invents one from an invalid
// Result).
package trafficcpa

import "math"

// Track is one aircraft's (ownship or target) kinematic state, as this
// package needs it. Every field that can be absent, stale, or
// untrustworthy is paired with an explicit *Valid flag - never a magic
// zero value (see docs/traffic-cpa-alerting.md's "Required inputs"
// section).
type Track struct {
	LatitudeDeg   float64
	LongitudeDeg  float64
	PositionValid bool

	// AltitudeFeet is whatever single altitude reference the caller has
	// already chosen CONSISTENTLY for both ownship and target (pressure
	// or GPS) - see docs/traffic-cpa-alerting.md's "Data sources"
	// section for why mixing references between the two tracks would
	// silently corrupt the vertical separation this package computes.
	// AltitudeValid may be true independently of PositionValid/
	// GroundVelocityValid (e.g. Mode-S altitude without a position fix).
	AltitudeFeet  float64
	AltitudeValid bool

	// TrackDegTrue/SpeedKnots are this aircraft's ground-referenced
	// heading (0-360, true north) and speed. GroundVelocityValid must be
	// true only when the caller already trusts BOTH values together -
	// e.g. ownship gated by GPS ground-track quality, a target gated by
	// its own reported Speed_valid.
	TrackDegTrue        float64
	SpeedKnots          float64
	GroundVelocityValid bool

	// VerticalRateFPM is feet per minute, positive = climbing. Only used
	// when VerticalRateValid is true for BOTH ownship and target - see
	// Result.PredictedVerticalSeparationValid.
	VerticalRateFPM   float64
	VerticalRateValid bool

	// AgeSeconds is the caller's own already-computed freshness for this
	// track (monotonic-clock-based, e.g. stratuxClock.Since(...)) - this
	// package never reads a clock itself.
	AgeSeconds float64
}

// Trend classifies the current horizontal closure rate - never a claim
// about the vertical dimension, and never itself gated on how far in the
// future TCPA falls.
type Trend string

const (
	// TrendUnknown is the zero value - never returned by Compute for a
	// Valid result (a valid result always has a defined Trend); returned
	// alongside Valid=false so a caller can never mistake it for a real
	// "steady" classification.
	TrendUnknown    Trend = ""
	TrendConverging Trend = "CONVERGING"
	TrendSteady     Trend = "STEADY"
	TrendDiverging  Trend = "DIVERGING"
)

// Confidence classifies how much of Result is populated.
type Confidence string

const (
	// ConfidenceNone: Result.Valid is false - see RejectReason.
	ConfidenceNone Confidence = "NONE"
	// ConfidenceMedium: horizontal CPA/closure-rate is valid; vertical
	// prediction is not available (missing/unreliable vertical rate on
	// either track) - CurrentVerticalSeparation may still be valid.
	ConfidenceMedium Confidence = "MEDIUM"
	// ConfidenceHigh: horizontal AND vertical prediction are both valid.
	ConfidenceHigh Confidence = "HIGH"
)

// RejectReason names exactly why Compute produced an invalid Result -
// never a generic "error", so a caller (or a diagnostics summary) can
// report which specific precondition failed without string-matching an
// error message.
type RejectReason string

const (
	ReasonNone                     RejectReason = ""
	ReasonOwnshipPositionInvalid   RejectReason = "ownship-position-invalid"
	ReasonTargetPositionInvalid    RejectReason = "target-position-invalid"
	ReasonInvalidLatitudeLongitude RejectReason = "invalid-latitude-longitude"
	ReasonOutsideEnvelope          RejectReason = "outside-approximation-envelope"
	ReasonOwnshipStale             RejectReason = "ownship-stale"
	ReasonTargetStale              RejectReason = "target-stale"
	ReasonOwnshipVelocityInvalid   RejectReason = "ownship-velocity-invalid"
	ReasonTargetVelocityInvalid    RejectReason = "target-velocity-invalid"
	ReasonRelativeSpeedTooLow      RejectReason = "relative-speed-too-low"
	ReasonNonFinite                RejectReason = "non-finite-result"
)

// Result is CPA's complete, sanitized output for one ownship/target pair
// at one instant. See docs/traffic-cpa-alerting.md for the full field-by-
// field description. The short version: Valid=false means none of the
// numeric fields below (other than the Current* separation fields, which
// carry their own independent *Valid flags and may be populated even when
// the overall trend/prediction is not) may be used for anything but
// diagnostics - a caller must never substitute zero for an unavailable
// estimate.
type Result struct {
	Valid        bool
	Confidence   Confidence
	RejectReason RejectReason

	CurrentHorizontalSeparationMeters float64
	CurrentHorizontalSeparationValid  bool

	CurrentVerticalSeparationFeet  float64
	CurrentVerticalSeparationValid bool

	// HorizontalClosureRateKnots: positive means closing, negative means
	// opening - see docs/traffic-cpa-alerting.md's "Sign convention".
	HorizontalClosureRateKnots float64
	HorizontalClosureRateValid bool

	// VerticalClosureRateFPM is the rate of change of (target altitude -
	// ownship altitude): positive means the target is gaining on ownship
	// vertically (climbing relative to ownship).
	VerticalClosureRateFPM   float64
	VerticalClosureRateValid bool

	// TCPASeconds is always in [0, Config.HorizonSeconds] when TCPAValid
	// - a raw, unclamped negative result (the true closest point of
	// approach already occurred) is reported as 0 ("CPA is now, from
	// this point forward the relative geometry is only ever computed
	// looking ahead"), not as a negative number - see
	// docs/traffic-cpa-alerting.md's "TCPA/CPA equations" section. Trend
	// is the separate, correct signal for "this was actually diverging".
	TCPASeconds float64
	TCPAValid   bool
	// TCPAClampedToHorizon is true when the true (unclamped) TCPA
	// exceeds Config.HorizonSeconds - the predicted-separation fields
	// below then describe the geometry at the horizon, not at the true
	// CPA.
	TCPAClampedToHorizon bool

	PredictedHorizontalSeparationMeters float64
	PredictedHorizontalSeparationValid  bool

	// PredictedVerticalSeparationFeet is only valid when both tracks'
	// vertical rates are valid (Confidence == ConfidenceHigh) - a missing
	// or unreliable vertical rate on either side NEVER silently assumes
	// a zero climb/descent rate; it leaves this field invalid instead
	// (see docs/traffic-cpa-alerting.md's "Vertical-rate behavior").
	PredictedVerticalSeparationFeet  float64
	PredictedVerticalSeparationValid bool

	Trend Trend

	// OwnshipAgeSeconds/TargetAgeSeconds echo the input ages, for a
	// caller's own diagnostics/logging - this package performs the
	// freshness gate itself (see Config.MaxAgeSeconds) but never hides
	// the values it gated on.
	OwnshipAgeSeconds float64
	TargetAgeSeconds  float64
}

const knotsToMetersPerSecond = 0.514444

// isNonFinite reports whether f is NaN or +/-Inf.
func isNonFinite(f float64) bool {
	return math.IsNaN(f) || math.IsInf(f, 0)
}
