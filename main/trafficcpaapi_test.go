package main

import (
	"testing"

	"github.com/stratux/stratux/alerting"
)

// withOwnshipGPSValidForCPA puts mySituation/globalStatus into a state
// isGPSValid()/isGPSGroundTrackValid() both accept, and restores the
// previous values afterward - the minimum ownship fixture computeTrafficCPA
// needs to get past its own leading isGPSValid() gate.
func withOwnshipGPSValidForCPA(t *testing.T) {
	t.Helper()
	ensureStratuxClockForTest()
	origSituation := mySituation
	origConnected := globalStatus.GPS_connected
	mySituation.GPSLastFixLocalTime = stratuxClock.Time()
	mySituation.GPSFixQuality = 1
	mySituation.GPSHorizontalAccuracy = 4
	mySituation.GPSVerticalAccuracy = 6
	mySituation.GPSLatitude = 40.0
	mySituation.GPSLongitude = -80.0
	mySituation.GPSTrueCourse = 90
	mySituation.GPSGroundSpeed = 120
	globalStatus.GPS_connected = true
	t.Cleanup(func() {
		mySituation = origSituation
		globalStatus.GPS_connected = origConnected
	})
}

// baseTrafficInfoForCPA returns one fresh, position-valid, speed-valid
// target a reasonable distance from the ownship fixture above - close
// enough to sit inside DefaultConfig()'s monitoring envelope.
func baseTrafficInfoForCPA() TrafficInfo {
	return TrafficInfo{
		Icao_addr:      0xABCDEF,
		Position_valid: true,
		Lat:            40.01,
		Lng:            -80.0,
		Alt:            1500,
		Track:          270,
		Speed:          120,
		Speed_valid:    true,
		Vvel:           0,
		Age:            1,
	}
}

// TestTargetTrackForCPA_VerticalRateNeverValid is a regression test for a
// vertical-velocity-validity correction made during this mission: this
// codebase cannot prove a target's Vvel end-to-end for ANY live traffic
// source (978 UAT's "no data" sentinel collapses to a genuine numeric
// zero with no bit preserved to tell them apart; 1090ES's own
// nil/non-nil distinction is discarded at the TrafficInfo merge; Ping
// sets Vvel unconditionally even when it just forced Speed_valid false;
// OGN/FLARM hardcode Speed_valid=true regardless of whether their own
// vertical-rate field was meaningful that update). targetTrackForCPA
// previously gated VerticalRateValid on Speed_valid alone - this test
// would have FAILED against that prior implementation for every case
// below where Speed_valid is true, and passes now that VerticalRateValid
// is unconditionally false for every target.
func TestTargetTrackForCPA_VerticalRateNeverValid(t *testing.T) {
	cases := []struct {
		name       string
		speedValid bool
		vvel       int16
	}{
		{"speed valid, level (Vvel=0) - the ambiguous case", true, 0},
		{"speed valid, nonzero Vvel", true, 1200},
		{"speed invalid, Vvel=0", false, 0},
		{"speed invalid, nonzero Vvel (mirrors ping.go's own decoupled case)", false, -800},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ti := baseTrafficInfoForCPA()
			ti.Speed_valid = tc.speedValid
			ti.Vvel = tc.vvel
			track := targetTrackForCPA(ti)
			if track.VerticalRateValid {
				t.Errorf("VerticalRateValid = true, want false (target vertical rate can never be proven valid) for Speed_valid=%v Vvel=%d", tc.speedValid, tc.vvel)
			}
			// The raw value is still carried through (not zeroed) - only
			// the Valid flag changes - matching trafficcpa.Track's own
			// "never a magic zero value" convention: consumers must check
			// Valid, not infer meaning from the number alone.
			if track.VerticalRateFPM != float64(tc.vvel) {
				t.Errorf("VerticalRateFPM = %v, want %v (raw value should still be passed through)", track.VerticalRateFPM, tc.vvel)
			}
		})
	}
}

// TestTargetTrackForCPA_OwnshipVerticalRateUnaffected confirms the fix
// above is scoped to the TARGET side only - ownship's own vertical rate
// validity (backed by real sensor-freshness checks, isTempPressValid/
// isGPSValid, not a borrowed proxy field) is unrelated and unchanged.
func TestTargetTrackForCPA_OwnshipVerticalRateUnaffected(t *testing.T) {
	withOwnshipGPSValidForCPA(t)
	ti := baseTrafficInfoForCPA()
	own := ownshipTrackForCPA(ti)
	if !own.VerticalRateValid {
		t.Error("expected ownship VerticalRateValid=true with a valid GPS fix and no baro data (falls back to GPS-derived vertical rate)")
	}
}

// TestComputeTrafficCPA_NeverReadsSettingsFromDisk is a regression test
// for a filesystem-I/O defect found during this mission: computeTrafficCPA
// used to call loadAlertSettings(), which performs a real os.ReadFile
// every call - and computeTrafficCPA runs once per non-ownship target,
// every ~1Hz sendTrafficUpdates cycle, WHILE sendTrafficUpdates holds
// trafficMutex. That is genuine filesystem I/O, repeated per target, in
// the one path this feature's own design explicitly requires to stay
// filesystem-free.
//
// This test proves the fix by deliberately making the on-disk
// AlertSettings file disagree with alertEvaluator's own in-memory
// Config: it writes a WIDE monitoring envelope to disk (which would
// admit the test target) but sets a NARROW one on the live evaluator
// (which would reject it as outside-envelope). If computeTrafficCPA read
// the file, the target would be admitted; since it must instead use only
// the in-memory Evaluator.Config(), the target is correctly rejected as
// outside the (narrow) envelope.
func TestComputeTrafficCPA_NeverReadsSettingsFromDisk(t *testing.T) {
	withTestAlertSettingsPath(t)
	e := withTestAlertEvaluator(t)
	withOwnshipGPSValidForCPA(t)
	withTestTrafficCPASettingsCache(t)

	// On-disk AlertSettings: a WIDE monitoring envelope (100 NM) that
	// would happily admit a target ~0.69 NM away.
	wide := DefaultAlertSettings()
	wide.MonitoringHorizontalNM = 100
	if err := saveAlertSettings(wide); err != nil {
		t.Fatalf("saveAlertSettings: %v", err)
	}

	// Live evaluator Config: a NARROW monitoring envelope (93m) that must
	// reject that same target as outside the approximation/monitoring
	// envelope - every entry threshold below it is scaled down too, to
	// keep Config.Validate()'s own required
	// monitoring >= notice >= caution >= high caution nesting intact.
	narrow := alerting.DefaultConfig()
	narrow.MasterEnabled = true
	narrow.MonitoringHorizontalMeters = 93
	narrow.NoticeHorizontalMeters = 60
	narrow.CautionHorizontalMeters = 40
	narrow.HighCautionHorizontalMeters = 20
	if err := e.SetConfig(narrow); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	ti := baseTrafficInfoForCPA() // ~0.69 NM (~1280m) from ownship
	res := computeTrafficCPA(ti)
	if res == nil {
		t.Fatal("computeTrafficCPA returned nil, want a non-nil (even if invalid) Result")
	}
	if res.Valid {
		t.Error("expected the target to be rejected as outside the envelope using the LIVE (narrow) evaluator config - a pass here means computeTrafficCPA read the wider on-disk settings file instead")
	}
}

// TestComputeTrafficCPA_FallsBackToDefaultEnvelopeWhenEvaluatorNil covers
// the early-startup window before initAlerting has run: computeTrafficCPA
// must not panic or read the settings file - it degrades to
// alerting.DefaultConfig()'s own envelope.
func TestComputeTrafficCPA_FallsBackToDefaultEnvelopeWhenEvaluatorNil(t *testing.T) {
	withTestAlertSettingsPath(t)
	withOwnshipGPSValidForCPA(t)
	withTestTrafficCPASettingsCache(t)

	origEval := alertEvaluator
	alertEvaluator = nil
	t.Cleanup(func() { alertEvaluator = origEval })

	ti := baseTrafficInfoForCPA()
	res := computeTrafficCPA(ti)
	if res == nil {
		t.Fatal("computeTrafficCPA returned nil with alertEvaluator == nil, want a non-nil Result computed against the default envelope")
	}
}

// withTestTrafficCPASettingsCache points the package-level
// trafficCPASettingsCache at DefaultTrafficCPASettings() for the duration
// of one test and restores the previous value afterward - mirrors
// withTestAlertEvaluator's pattern for the sibling settings cache.
func withTestTrafficCPASettingsCache(t *testing.T) {
	t.Helper()
	trafficCPAMu.Lock()
	orig := trafficCPASettingsCache
	trafficCPASettingsCache = DefaultTrafficCPASettings()
	trafficCPAMu.Unlock()
	t.Cleanup(func() {
		trafficCPAMu.Lock()
		trafficCPASettingsCache = orig
		trafficCPAMu.Unlock()
	})
}
