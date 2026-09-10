package trafficcpa

import (
	"math"
	"testing"
)

// Illustrative, deliberately-rounded coordinates representative of a
// northern-latitude operating area (not any specific real address) - see
// this project's own mission constraints against exact real coordinates
// in tests.
const (
	testLat = 47.0
	testLon = -116.0
)

func validOwnship() Track {
	return Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
		AgeSeconds: 0,
	}
}

func TestCompute_HeadOnConvergence(t *testing.T) {
	own := validOwnship() // heading north (0), 100kt
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, // north of ownship
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true, // heading south, toward ownship
		AgeSeconds: 0,
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.Trend != TrendConverging {
		t.Errorf("expected converging, got %v", res.Trend)
	}
	if res.HorizontalClosureRateKnots <= 0 {
		t.Errorf("expected positive closure rate, got %v", res.HorizontalClosureRateKnots)
	}
	if !res.TCPAValid || res.TCPASeconds <= 0 {
		t.Errorf("expected a positive, valid TCPA, got %v (valid=%v)", res.TCPASeconds, res.TCPAValid)
	}
	if res.PredictedHorizontalSeparationMeters > 100 {
		t.Errorf("expected head-on targets to nearly meet at CPA, got %.1fm separation", res.PredictedHorizontalSeparationMeters)
	}
}

func TestCompute_SameCourseOvertake(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 150, GroundVelocityValid: true,
	}
	// Target ahead (north), same course, slower - ownship overtakes.
	target := Track{
		LatitudeDeg: testLat + 0.05, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.Trend != TrendConverging {
		t.Errorf("expected converging (overtake), got %v", res.Trend)
	}
}

func TestCompute_CrossingPaths(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true, // heading north
	}
	target := Track{
		LatitudeDeg: testLat + 0.02, LongitudeDeg: testLon - 0.02, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 90, SpeedKnots: 100, GroundVelocityValid: true, // heading east, crosses ownship's path
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if !res.TCPAValid {
		t.Error("expected a valid TCPA for genuinely crossing paths")
	}
}

func TestCompute_ParallelEqualSpeedTracksRejectedAsLowRelativeSpeed(t *testing.T) {
	own := validOwnship()
	target := Track{
		LatitudeDeg: testLat + 0.05, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true, // identical track & speed
	}
	res := Compute(own, target, DefaultConfig())
	if res.Valid {
		t.Fatal("expected parallel equal-speed tracks (zero relative velocity) to be rejected")
	}
	if res.RejectReason != ReasonRelativeSpeedTooLow {
		t.Errorf("expected ReasonRelativeSpeedTooLow, got %v", res.RejectReason)
	}
}

func TestCompute_DivergingTracks(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true, // heading south
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true, // heading north, away from ownship
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.Trend != TrendDiverging {
		t.Errorf("expected diverging, got %v", res.Trend)
	}
	if res.HorizontalClosureRateKnots >= 0 {
		t.Errorf("expected negative closure rate, got %v", res.HorizontalClosureRateKnots)
	}
}

func TestCompute_NegativeUnclampedPastCPAReportsZeroButTrendIsDiverging(t *testing.T) {
	// The true, unclamped t_CPA for this geometry is negative (closest
	// point already passed) - TCPASeconds must report 0, never negative,
	// while Trend still honestly reports diverging.
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 180, SpeedKnots: 120, GroundVelocityValid: true,
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 0, SpeedKnots: 120, GroundVelocityValid: true,
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.TCPASeconds != 0 {
		t.Errorf("expected TCPASeconds clamped to 0 for a past CPA, got %v", res.TCPASeconds)
	}
	if res.Trend != TrendDiverging {
		t.Errorf("expected Trend=DIVERGING despite TCPASeconds=0, got %v", res.Trend)
	}
}

func TestCompute_StationaryOwnship(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 0, SpeedKnots: 0, GroundVelocityValid: true,
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true,
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result with a stationary ownship, got RejectReason=%v", res.RejectReason)
	}
	if res.Trend != TrendConverging {
		t.Errorf("expected converging, got %v", res.Trend)
	}
}

func TestCompute_StationaryTarget(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 0, SpeedKnots: 0, GroundVelocityValid: true,
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result with a stationary target, got RejectReason=%v", res.RejectReason)
	}
	if res.Trend != TrendConverging {
		t.Errorf("expected converging, got %v", res.Trend)
	}
}

func TestCompute_BothNearlyStationaryRejected(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 0, SpeedKnots: 1, GroundVelocityValid: true,
	}
	target := Track{
		LatitudeDeg: testLat + 0.05, LongitudeDeg: testLon, PositionValid: true,
		TrackDegTrue: 90, SpeedKnots: 1, GroundVelocityValid: true,
	}
	res := Compute(own, target, DefaultConfig())
	if res.Valid {
		t.Fatal("expected two nearly-stationary tracks to be rejected as low relative speed")
	}
	if res.RejectReason != ReasonRelativeSpeedTooLow {
		t.Errorf("expected ReasonRelativeSpeedTooLow, got %v", res.RejectReason)
	}
}

func TestCompute_ZeroRelativeVelocityRejected(t *testing.T) {
	own := validOwnship()
	target := own // identical kinematic state -> exactly zero relative velocity
	target.LatitudeDeg += 0.02
	res := Compute(own, target, DefaultConfig())
	if res.Valid {
		t.Fatal("expected exactly-zero relative velocity to be rejected")
	}
	if res.RejectReason != ReasonRelativeSpeedTooLow {
		t.Errorf("expected ReasonRelativeSpeedTooLow, got %v", res.RejectReason)
	}
}

func TestCompute_NearlyZeroRelativeVelocityBoundary(t *testing.T) {
	cfg := DefaultConfig()
	own := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true}
	// Target's speed just above/below the threshold difference from ownship's.
	below := Track{LatitudeDeg: testLat + 0.05, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100 + cfg.MinRelativeSpeedKnots*0.5, GroundVelocityValid: true}
	above := Track{LatitudeDeg: testLat + 0.05, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100 + cfg.MinRelativeSpeedKnots*2, GroundVelocityValid: true}

	if res := Compute(own, below, cfg); res.Valid {
		t.Error("expected relative speed below the minimum to be rejected")
	}
	if res := Compute(own, above, cfg); !res.Valid {
		t.Errorf("expected relative speed above the minimum to be accepted, got RejectReason=%v", res.RejectReason)
	}
}

func TestCompute_CPANow(t *testing.T) {
	// Coincident horizontal positions - CPA is exactly now.
	own := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true}
	target := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 90, SpeedKnots: 100, GroundVelocityValid: true}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result at coincident positions, got RejectReason=%v", res.RejectReason)
	}
	if res.TCPASeconds != 0 {
		t.Errorf("expected TCPASeconds=0 at coincident positions, got %v", res.TCPASeconds)
	}
	if res.CurrentHorizontalSeparationMeters != 0 {
		t.Errorf("expected zero current separation, got %v", res.CurrentHorizontalSeparationMeters)
	}
	if isNonFinite(res.HorizontalClosureRateKnots) {
		t.Errorf("expected a finite (not NaN) closure rate at coincident positions, got %v", res.HorizontalClosureRateKnots)
	}
}

func TestCompute_CPABeyondHorizonClamped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HorizonSeconds = 60
	cfg.MaxHorizontalSeparationMeters = 200000 // this test's own point is the horizon clamp, not the envelope
	// Very slow closure over a large distance -> true TCPA far exceeds 60s.
	own := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true}
	target := Track{LatitudeDeg: testLat + 1.0, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: cfg.MinRelativeSpeedKnots + 1, GroundVelocityValid: true}
	res := Compute(own, target, cfg)
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if !res.TCPAClampedToHorizon {
		t.Error("expected TCPA to be clamped to the horizon")
	}
	if res.TCPASeconds != cfg.HorizonSeconds {
		t.Errorf("expected TCPASeconds == HorizonSeconds when clamped, got %v", res.TCPASeconds)
	}
}

func TestCompute_ExactHorizonBoundaryNotClamped(t *testing.T) {
	// Construct a geometry whose true TCPA is exactly the horizon.
	cfg := DefaultConfig()
	own := Track{LatitudeDeg: 0, LongitudeDeg: 0, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 0, GroundVelocityValid: true}
	// Target due north, closing speed chosen so that distance/speed == horizon exactly.
	closureKnots := 100.0
	distMeters := closureKnots * knotsToMetersPerSecond * cfg.HorizonSeconds
	distDeg := distMeters / earthRadiusMeters * (180 / math.Pi)
	target := Track{LatitudeDeg: distDeg, LongitudeDeg: 0, PositionValid: true, TrackDegTrue: 180, SpeedKnots: closureKnots, GroundVelocityValid: true}
	res := Compute(own, target, cfg)
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.TCPAClampedToHorizon {
		t.Errorf("expected TCPA exactly at the horizon to NOT be reported as clamped, got TCPASeconds=%v", res.TCPASeconds)
	}
	if math.Abs(res.TCPASeconds-cfg.HorizonSeconds) > 0.5 {
		t.Errorf("expected TCPASeconds close to the horizon (%v), got %v", cfg.HorizonSeconds, res.TCPASeconds)
	}
}

func TestCompute_HorizontalMiss(t *testing.T) {
	// A target well off to the side, moving roughly parallel - the
	// predicted CPA separation should stay close to the current
	// separation, not shrink toward zero the way a converging pair
	// would.
	own := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true}
	target := Track{LatitudeDeg: testLat, LongitudeDeg: testLon + 0.05, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 130, GroundVelocityValid: true}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.PredictedHorizontalSeparationMeters < 3000 {
		t.Errorf("expected the predicted miss distance to stay close to the ~3.9km current separation, got %.0fm", res.PredictedHorizontalSeparationMeters)
	}
}

func TestCompute_VerticalCrossingWithReliableRates(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: 0, VerticalRateValid: true,
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 4000, AltitudeValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: 1000, VerticalRateValid: true, // climbing toward ownship's altitude
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.Confidence != ConfidenceHigh {
		t.Errorf("expected ConfidenceHigh with both vertical rates reliable, got %v", res.Confidence)
	}
	if !res.PredictedVerticalSeparationValid {
		t.Fatal("expected a valid predicted vertical separation")
	}
	if math.Abs(res.PredictedVerticalSeparationFeet) >= math.Abs(res.CurrentVerticalSeparationFeet) {
		t.Errorf("expected predicted vertical separation magnitude to shrink (target climbing toward ownship), current=%v predicted=%v",
			res.CurrentVerticalSeparationFeet, res.PredictedVerticalSeparationFeet)
	}
}

func TestCompute_LevelTraffic(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: 0, VerticalRateValid: true,
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: 0, VerticalRateValid: true,
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if res.PredictedVerticalSeparationFeet != 0 {
		t.Errorf("expected zero predicted vertical separation for level traffic, got %v", res.PredictedVerticalSeparationFeet)
	}
}

func TestCompute_OpposingClimbDescent(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: 500, VerticalRateValid: true, // ownship climbing
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 6000, AltitudeValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: -500, VerticalRateValid: true, // target descending toward ownship
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result, got RejectReason=%v", res.RejectReason)
	}
	if !res.PredictedVerticalSeparationValid {
		t.Fatal("expected valid predicted vertical separation")
	}
	if res.PredictedVerticalSeparationFeet >= res.CurrentVerticalSeparationFeet {
		t.Errorf("expected vertical separation to shrink under opposing climb/descent, current=%v predicted=%v",
			res.CurrentVerticalSeparationFeet, res.PredictedVerticalSeparationFeet)
	}
}

func TestCompute_MissingVerticalRateStillValidHorizontally(t *testing.T) {
	own := Track{
		LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5000, AltitudeValid: true,
		TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateValid: false,
	}
	target := Track{
		LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true,
		AltitudeFeet: 5200, AltitudeValid: true,
		TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true,
		VerticalRateFPM: 500, VerticalRateValid: true, // target's own rate IS valid, but ownship's is not
	}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid (horizontal) result despite missing ownship vertical rate, got RejectReason=%v", res.RejectReason)
	}
	if res.Confidence != ConfidenceMedium {
		t.Errorf("expected ConfidenceMedium, got %v", res.Confidence)
	}
	if res.PredictedVerticalSeparationValid {
		t.Error("expected predicted vertical separation to be UNAVAILABLE, not silently assuming a zero rate")
	}
	if !res.CurrentVerticalSeparationValid {
		t.Error("expected current vertical separation to still be reported")
	}
	if res.VerticalClosureRateValid {
		t.Error("expected vertical closure rate to be invalid when a vertical rate is missing")
	}
}

func TestCompute_MissingTrackRejected(t *testing.T) {
	own := validOwnship()
	target := Track{LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, GroundVelocityValid: false}
	res := Compute(own, target, DefaultConfig())
	if res.Valid {
		t.Fatal("expected missing target track/velocity to be rejected")
	}
	if res.RejectReason != ReasonTargetVelocityInvalid {
		t.Errorf("expected ReasonTargetVelocityInvalid, got %v", res.RejectReason)
	}
}

func TestCompute_MissingSpeedRejected(t *testing.T) {
	own := validOwnship()
	own.GroundVelocityValid = false
	target := Track{LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true}
	res := Compute(own, target, DefaultConfig())
	if res.Valid {
		t.Fatal("expected missing ownship velocity to be rejected")
	}
	if res.RejectReason != ReasonOwnshipVelocityInvalid {
		t.Errorf("expected ReasonOwnshipVelocityInvalid, got %v", res.RejectReason)
	}
}

func TestCompute_StaleOwnshipRejected(t *testing.T) {
	cfg := DefaultConfig()
	own := validOwnship()
	own.AgeSeconds = cfg.MaxAgeSeconds + 1
	target := Track{LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true}
	res := Compute(own, target, cfg)
	if res.Valid || res.RejectReason != ReasonOwnshipStale {
		t.Errorf("expected ReasonOwnshipStale, got valid=%v reason=%v", res.Valid, res.RejectReason)
	}
}

func TestCompute_StaleTargetRejected(t *testing.T) {
	cfg := DefaultConfig()
	own := validOwnship()
	target := Track{LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true, AgeSeconds: cfg.MaxAgeSeconds + 1}
	res := Compute(own, target, cfg)
	if res.Valid || res.RejectReason != ReasonTargetStale {
		t.Errorf("expected ReasonTargetStale, got valid=%v reason=%v", res.Valid, res.RejectReason)
	}
}

func TestCompute_InvalidLatitudeLongitudeRejected(t *testing.T) {
	cfg := DefaultConfig()
	own := validOwnship()
	cases := []Track{
		{LatitudeDeg: 91, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true},
		{LatitudeDeg: -91, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true},
		{LatitudeDeg: testLat, LongitudeDeg: 181, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true},
		{LatitudeDeg: math.NaN(), LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true},
	}
	for i, target := range cases {
		res := Compute(own, target, cfg)
		if res.Valid || res.RejectReason != ReasonInvalidLatitudeLongitude {
			t.Errorf("case %d: expected ReasonInvalidLatitudeLongitude, got valid=%v reason=%v", i, res.Valid, res.RejectReason)
		}
	}
}

func TestCompute_LongitudeWraparound(t *testing.T) {
	own := Track{LatitudeDeg: 45, LongitudeDeg: 179.98, PositionValid: true, TrackDegTrue: 90, SpeedKnots: 100, GroundVelocityValid: true}
	target := Track{LatitudeDeg: 45, LongitudeDeg: -179.98, PositionValid: true, TrackDegTrue: 270, SpeedKnots: 100, GroundVelocityValid: true}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected valid result across the antimeridian, got RejectReason=%v", res.RejectReason)
	}
	// The true separation is small (~0.2 degrees of longitude), not
	// ~360 degrees' worth - if wraparound were handled incorrectly this
	// would be enormous (and would have already failed the envelope
	// check above).
	if res.CurrentHorizontalSeparationMeters > 50000 {
		t.Errorf("expected a small separation across the antimeridian, got %.0fm (wraparound bug?)", res.CurrentHorizontalSeparationMeters)
	}
}

func TestCompute_HighLatitudeApproximationBoundary(t *testing.T) {
	cfg := DefaultConfig()
	own := Track{LatitudeDeg: cfg.MaxAbsoluteLatitudeDeg, LongitudeDeg: 0, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true}
	target := Track{LatitudeDeg: cfg.MaxAbsoluteLatitudeDeg - 0.05, LongitudeDeg: 0, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 130, GroundVelocityValid: true}
	if res := Compute(own, target, cfg); !res.Valid {
		t.Errorf("expected latitude exactly at the boundary to be accepted, got RejectReason=%v", res.RejectReason)
	}
	beyond := own
	beyond.LatitudeDeg = cfg.MaxAbsoluteLatitudeDeg + 0.1
	if res := Compute(beyond, target, cfg); res.Valid || res.RejectReason != ReasonOutsideEnvelope {
		t.Errorf("expected latitude just beyond the boundary to be rejected, got valid=%v reason=%v", res.Valid, res.RejectReason)
	}
}

func TestCompute_CoincidentHorizontalPositions(t *testing.T) {
	own := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true}
	target := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 90, SpeedKnots: 100, GroundVelocityValid: true}
	res := Compute(own, target, DefaultConfig())
	if !res.Valid {
		t.Fatalf("expected a valid (not a crash/NaN) result at coincident positions, got RejectReason=%v", res.RejectReason)
	}
	if math.IsNaN(res.HorizontalClosureRateKnots) || math.IsInf(res.HorizontalClosureRateKnots, 0) {
		t.Errorf("expected a finite closure rate, got %v", res.HorizontalClosureRateKnots)
	}
}

func TestCompute_NaNAndInfinityNeverPanicsOrValidates(t *testing.T) {
	cfg := DefaultConfig()
	bad := []Track{
		{LatitudeDeg: math.NaN(), LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true},
		{LatitudeDeg: testLat, LongitudeDeg: math.Inf(1), PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true},
		{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: math.NaN(), SpeedKnots: 100, GroundVelocityValid: true},
		{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: math.Inf(1), GroundVelocityValid: true},
		{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: -1, GroundVelocityValid: true},
	}
	own := validOwnship()
	for i, target := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d: Compute panicked: %v", i, r)
				}
			}()
			res := Compute(own, target, cfg)
			if res.Valid {
				t.Errorf("case %d: expected a NaN/Inf/negative-speed input to be rejected, not validated", i)
			}
		}()
	}
}

func TestCompute_UnitConversion(t *testing.T) {
	if math.Abs(knotsToMetersPerSecond-0.514444) > 1e-9 {
		t.Errorf("expected the standard knots-to-m/s conversion factor, got %v", knotsToMetersPerSecond)
	}
}

func TestCompute_SignConventionClosingVsOpening(t *testing.T) {
	closing := Compute(
		Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true},
		Track{LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true},
		DefaultConfig(),
	)
	opening := Compute(
		Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 180, SpeedKnots: 100, GroundVelocityValid: true},
		Track{LatitudeDeg: testLat + 0.03, LongitudeDeg: testLon, PositionValid: true, TrackDegTrue: 0, SpeedKnots: 100, GroundVelocityValid: true},
		DefaultConfig(),
	)
	if closing.HorizontalClosureRateKnots <= 0 {
		t.Errorf("expected positive closure rate for a closing pair, got %v", closing.HorizontalClosureRateKnots)
	}
	if opening.HorizontalClosureRateKnots >= 0 {
		t.Errorf("expected negative closure rate for an opening pair, got %v", opening.HorizontalClosureRateKnots)
	}
}

func TestCompute_Determinism(t *testing.T) {
	own := Track{LatitudeDeg: testLat, LongitudeDeg: testLon, PositionValid: true, AltitudeFeet: 5000, AltitudeValid: true, TrackDegTrue: 30, SpeedKnots: 110, GroundVelocityValid: true, VerticalRateFPM: 200, VerticalRateValid: true}
	target := Track{LatitudeDeg: testLat + 0.07, LongitudeDeg: testLon - 0.03, PositionValid: true, AltitudeFeet: 5500, AltitudeValid: true, TrackDegTrue: 210, SpeedKnots: 95, GroundVelocityValid: true, VerticalRateFPM: -150, VerticalRateValid: true}
	cfg := DefaultConfig()
	a := Compute(own, target, cfg)
	b := Compute(own, target, cfg)
	if a != b {
		t.Errorf("expected identical results for identical inputs, got %+v vs %+v", a, b)
	}
}

func TestConfig_ValidateRejectsNonPositiveAndOutOfRange(t *testing.T) {
	base := DefaultConfig()
	cases := []func(c *Config){
		func(c *Config) { c.HorizonSeconds = 0 },
		func(c *Config) { c.HorizonSeconds = -1 },
		func(c *Config) { c.MinRelativeSpeedKnots = 0 },
		func(c *Config) { c.MaxAgeSeconds = math.NaN() },
		func(c *Config) { c.MaxHorizontalSeparationMeters = math.Inf(1) },
		func(c *Config) { c.MaxAbsoluteLatitudeDeg = 91 },
	}
	for i, mutate := range cases {
		c := base
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected validation error, got none", i)
		}
	}
	if err := base.Validate(); err != nil {
		t.Errorf("expected the default config to validate cleanly: %v", err)
	}
}
