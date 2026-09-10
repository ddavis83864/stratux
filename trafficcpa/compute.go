package trafficcpa

import "math"

// earthRadiusMeters matches this project's own common.DistRect's constant
// (mean earth radius) - kept as an independent constant here, rather than
// importing the common package, so this package's dependency graph stays
// limited to the Go standard library (see the package doc comment).
const earthRadiusMeters = 6371008.8

// normalizeDegrees folds d into (-180, 180] - the same wraparound
// normalization this project's common.RadiansRel already applies before
// converting a longitude (or latitude) DIFFERENCE to radians, so a
// delta computed across the antimeridian (e.g. 179.9 to -179.9) comes out
// as a small number, never a ~360-degree one.
func normalizeDegrees(d float64) float64 {
	for d > 180 {
		d -= 360
	}
	for d <= -180 {
		d += 360
	}
	return d
}

func radians(deg float64) float64 { return deg * math.Pi / 180.0 }

// relativePositionMeters returns target's position relative to origin
// (ownship) as (north, east) meters, using the same equirectangular
// tangent-plane approximation this project's own common.DistRect already
// uses for short-range traffic distance/bearing (see that function's own
// doc comment and docs/traffic-cpa-alerting.md's "Coordinate model"
// section) - reimplemented here, not imported, to keep this package
// dependency-free (see the package doc comment).
func relativePositionMeters(originLatDeg, originLonDeg, latDeg, lonDeg float64) (north, east float64) {
	dLat := normalizeDegrees(latDeg - originLatDeg)
	avgLat := (latDeg + originLatDeg) / 2
	dLon := normalizeDegrees(lonDeg - originLonDeg)
	north = radians(dLat) * earthRadiusMeters
	east = radians(dLon) * earthRadiusMeters * math.Cos(radians(avgLat))
	return
}

// velocityComponents converts a ground track (degrees true, 0 = north,
// clockwise positive) and speed (knots) into (north, east) meters/second
// components.
func velocityComponents(trackDegTrue, speedKnots float64) (vNorth, vEast float64) {
	speedMPS := speedKnots * knotsToMetersPerSecond
	rad := radians(trackDegTrue)
	vNorth = speedMPS * math.Cos(rad)
	vEast = speedMPS * math.Sin(rad)
	return
}

func invalidLatLon(lat, lon float64) bool {
	return isNonFinite(lat) || isNonFinite(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180
}

// Compute derives Result from ownship and target's current kinematic
// state, per Config's bounds. Pure, deterministic, and side-effect-free:
// calling Compute twice with identical arguments always returns an
// identical Result. See the package doc comment and
// docs/traffic-cpa-alerting.md for the full model.
//
// Every rejection path below returns as early as possible with its own
// specific RejectReason - callers needing to know exactly why a
// particular target never escalated should inspect this field rather
// than treat every invalid Result identically.
func Compute(ownship, target Track, cfg Config) Result {
	res := Result{
		Confidence:        ConfidenceNone,
		OwnshipAgeSeconds: ownship.AgeSeconds,
		TargetAgeSeconds:  target.AgeSeconds,
	}

	if !ownship.PositionValid {
		res.RejectReason = ReasonOwnshipPositionInvalid
		return res
	}
	if !target.PositionValid {
		res.RejectReason = ReasonTargetPositionInvalid
		return res
	}
	if invalidLatLon(ownship.LatitudeDeg, ownship.LongitudeDeg) || invalidLatLon(target.LatitudeDeg, target.LongitudeDeg) {
		res.RejectReason = ReasonInvalidLatitudeLongitude
		return res
	}
	if math.Abs(ownship.LatitudeDeg) > cfg.MaxAbsoluteLatitudeDeg || math.Abs(target.LatitudeDeg) > cfg.MaxAbsoluteLatitudeDeg {
		res.RejectReason = ReasonOutsideEnvelope
		return res
	}
	if isNonFinite(ownship.AgeSeconds) || ownship.AgeSeconds < 0 || ownship.AgeSeconds > cfg.MaxAgeSeconds {
		res.RejectReason = ReasonOwnshipStale
		return res
	}
	if isNonFinite(target.AgeSeconds) || target.AgeSeconds < 0 || target.AgeSeconds > cfg.MaxAgeSeconds {
		res.RejectReason = ReasonTargetStale
		return res
	}

	rNorth, rEast := relativePositionMeters(ownship.LatitudeDeg, ownship.LongitudeDeg, target.LatitudeDeg, target.LongitudeDeg)
	currentHoriz := math.Hypot(rNorth, rEast)
	if isNonFinite(currentHoriz) {
		res.RejectReason = ReasonNonFinite
		return res
	}
	res.CurrentHorizontalSeparationMeters = currentHoriz
	res.CurrentHorizontalSeparationValid = true

	if currentHoriz > cfg.MaxHorizontalSeparationMeters {
		res.RejectReason = ReasonOutsideEnvelope
		return res
	}

	// Current vertical separation, independent of velocity availability -
	// a caller may still want "how far apart right now" even when no
	// trend can be computed.
	if ownship.AltitudeValid && target.AltitudeValid &&
		!isNonFinite(ownship.AltitudeFeet) && !isNonFinite(target.AltitudeFeet) {
		res.CurrentVerticalSeparationFeet = target.AltitudeFeet - ownship.AltitudeFeet
		res.CurrentVerticalSeparationValid = true
	}

	if !ownship.GroundVelocityValid {
		res.RejectReason = ReasonOwnshipVelocityInvalid
		return res
	}
	if isNonFinite(ownship.TrackDegTrue) || isNonFinite(ownship.SpeedKnots) || ownship.SpeedKnots < 0 {
		res.RejectReason = ReasonOwnshipVelocityInvalid
		return res
	}
	if !target.GroundVelocityValid {
		res.RejectReason = ReasonTargetVelocityInvalid
		return res
	}
	if isNonFinite(target.TrackDegTrue) || isNonFinite(target.SpeedKnots) || target.SpeedKnots < 0 {
		res.RejectReason = ReasonTargetVelocityInvalid
		return res
	}

	ovN, ovE := velocityComponents(ownship.TrackDegTrue, ownship.SpeedKnots)
	tvN, tvE := velocityComponents(target.TrackDegTrue, target.SpeedKnots)
	// v is TARGET velocity relative to OWNSHIP.
	vN := tvN - ovN
	vE := tvE - ovE

	relSpeedMPS := math.Hypot(vN, vE)
	relSpeedKnots := relSpeedMPS / knotsToMetersPerSecond
	if isNonFinite(relSpeedKnots) {
		res.RejectReason = ReasonNonFinite
		return res
	}
	if relSpeedKnots < cfg.MinRelativeSpeedKnots {
		res.RejectReason = ReasonRelativeSpeedTooLow
		return res
	}

	// Closure rate: the negative of the rate of change of horizontal
	// separation at t=0, i.e. -(d/dt |r + vt|) evaluated at t=0, which
	// equals -(r . v)/|r|. Positive = closing (separation shrinking),
	// negative = opening. At r=0 (coincident positions) the direction of
	// "closing" is not physically meaningful, but the rate of change of
	// separation is: d/dt(|r+vt|^2) at t=0 is 2(r.v) = 0 when r=0, so 0
	// is the mathematically correct value, not an arbitrary placeholder.
	rDotV := rNorth*vN + rEast*vE
	var closureRateMPS float64
	if currentHoriz > 0 {
		closureRateMPS = -rDotV / currentHoriz
	}
	if isNonFinite(closureRateMPS) {
		res.RejectReason = ReasonNonFinite
		return res
	}
	res.HorizontalClosureRateValid = true
	res.HorizontalClosureRateKnots = closureRateMPS / knotsToMetersPerSecond

	switch {
	case res.HorizontalClosureRateKnots > 0.5:
		res.Trend = TrendConverging
	case res.HorizontalClosureRateKnots < -0.5:
		res.Trend = TrendDiverging
	default:
		res.Trend = TrendSteady
	}

	// t_CPA = -(r.v)/(v.v) - see docs/traffic-cpa-alerting.md's
	// "TCPA/CPA equations" section. v.v is relSpeedMPS^2, already
	// verified finite and >= (MinRelativeSpeedKnots-derived) positive
	// above, so this division is safe.
	vDotV := relSpeedMPS * relSpeedMPS
	tcpaRaw := -rDotV / vDotV
	if isNonFinite(tcpaRaw) {
		res.RejectReason = ReasonNonFinite
		return res
	}

	tcpa := tcpaRaw
	if tcpa < 0 {
		// The true closest point of approach already occurred - report
		// "now" per the package doc comment; Trend (computed above from
		// the raw, unclamped closure rate) is the honest signal for
		// "this was actually diverging."
		tcpa = 0
	}
	clampedToHorizon := tcpa > cfg.HorizonSeconds
	if clampedToHorizon {
		tcpa = cfg.HorizonSeconds
	}
	res.TCPAValid = true
	res.TCPASeconds = tcpa
	res.TCPAClampedToHorizon = clampedToHorizon

	predN := rNorth + vN*tcpa
	predE := rEast + vE*tcpa
	predHoriz := math.Hypot(predN, predE)
	if isNonFinite(predHoriz) {
		res.RejectReason = ReasonNonFinite
		res.TCPAValid = false
		return res
	}
	res.PredictedHorizontalSeparationMeters = predHoriz
	res.PredictedHorizontalSeparationValid = true
	res.Confidence = ConfidenceMedium

	if res.CurrentVerticalSeparationValid && ownship.VerticalRateValid && target.VerticalRateValid &&
		!isNonFinite(ownship.VerticalRateFPM) && !isNonFinite(target.VerticalRateFPM) {
		vertClosureFPM := target.VerticalRateFPM - ownship.VerticalRateFPM
		predVert := res.CurrentVerticalSeparationFeet + vertClosureFPM*(tcpa/60.0)
		if !isNonFinite(vertClosureFPM) && !isNonFinite(predVert) {
			res.VerticalClosureRateValid = true
			res.VerticalClosureRateFPM = vertClosureFPM
			res.PredictedVerticalSeparationFeet = predVert
			res.PredictedVerticalSeparationValid = true
			res.Confidence = ConfidenceHigh
		}
	}

	res.Valid = true
	res.RejectReason = ReasonNone
	return res
}
