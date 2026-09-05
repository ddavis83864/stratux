package main

import (
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/readiness"
)

// ensureSituationLocks makes mySituation's mutex-protected sections usable
// from a test without running main()'s full startup (which touches real
// hardware/network). Safe to call more than once - it just (re)installs
// fresh, unlocked mutexes, and nothing else in a plain `go test` run holds
// a reference to the old ones.
func ensureSituationLocks() {
	if mySituation.muAttitude == nil {
		mySituation.muAttitude = &sync.Mutex{}
	}
	if mySituation.muBaro == nil {
		mySituation.muBaro = &sync.Mutex{}
	}
}

func TestBuildAHRSHealth_WiresLiveSituationState(t *testing.T) {
	ensureSituationLocks()
	// buildAHRSHealth now also reports calibration-profile subsystem
	// state (see readiness.AHRSProfileInfo) - a test exercising the
	// hardware-healthy/READY path needs a real active profile, the same
	// way it needs globalSettings.IMU_Sensor_Enabled set; otherwise the
	// profile-unavailable case correctly (not spuriously) downgrades an
	// otherwise-READY result to DEGRADED.
	store := withTestProfilesStore(t)
	now := time.Now().UTC()
	profile := calprofile.Profile{
		ID: calprofile.NewID(), Name: "Test", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion,
		CreatedAt: now, ModifiedAt: now,
	}
	store.Save(profile)
	store.SetActiveID(profile.ID, now)

	origSettings := globalSettings
	origStatus := globalStatus
	origSituation := mySituation
	defer func() {
		globalSettings = origSettings
		globalStatus = origStatus
		mySituation.AHRSPitch = origSituation.AHRSPitch
		mySituation.AHRSRoll = origSituation.AHRSRoll
		mySituation.AHRSGLoad = origSituation.AHRSGLoad
		mySituation.AHRSStatus = origSituation.AHRSStatus
		mySituation.AHRSLastAttitudeTime = origSituation.AHRSLastAttitudeTime
	}()

	globalSettings.IMU_Sensor_Enabled = true
	globalStatus.IMUConnected = true
	globalSettings.SensorQuaternion = [4]float64{1, 0, 0, 0}
	globalSettings.D = [3]float64{0.1, 0.1, 0.1}
	globalSettings.IMUMapping = [2]int{-1, 0}

	mono := time.Now()
	mySituation.muAttitude.Lock()
	mySituation.AHRSPitch = 2.5
	mySituation.AHRSRoll = -1.5
	mySituation.AHRSGLoad = 1.01
	mySituation.AHRSStatus = 0x1F
	mySituation.AHRSLastAttitudeTime = mono
	mySituation.muAttitude.Unlock()

	h := buildAHRSHealth(mono, time.Now().UTC())
	if h.State != readiness.StateReady {
		t.Errorf("State = %q, want READY: %s", h.State, h.Reason)
	}
	if h.PitchDeg == nil || *h.PitchDeg != 2.5 {
		t.Errorf("PitchDeg = %v, want 2.5", h.PitchDeg)
	}
	if h.RollDeg == nil || *h.RollDeg != -1.5 {
		t.Errorf("RollDeg = %v, want -1.5", h.RollDeg)
	}
	if !h.LevelCalibrated || !h.GyroCalibrated {
		t.Error("LevelCalibrated/GyroCalibrated should be true given a non-zero quaternion/D")
	}
	if h.HeadingSupported {
		t.Error("HeadingSupported must always be false")
	}
}

func TestBuildAHRSHealth_DisabledWiring(t *testing.T) {
	ensureSituationLocks()
	origSettings := globalSettings
	defer func() { globalSettings = origSettings }()
	globalSettings.IMU_Sensor_Enabled = false

	h := buildAHRSHealth(time.Time{}, time.Now().UTC())
	if h.State != readiness.StateNotInstalled {
		t.Errorf("State = %q, want NOT_INSTALLED", h.State)
	}
}

func TestBuildAHRSHealth_InvalidSentinelBecomesNil(t *testing.T) {
	ensureSituationLocks()
	origSettings := globalSettings
	origStatus := globalStatus
	origSituation := mySituation
	defer func() {
		globalSettings = origSettings
		globalStatus = origStatus
		mySituation.AHRSPitch = origSituation.AHRSPitch
		mySituation.AHRSRoll = origSituation.AHRSRoll
		mySituation.AHRSLastAttitudeTime = origSituation.AHRSLastAttitudeTime
	}()

	globalSettings.IMU_Sensor_Enabled = true
	globalStatus.IMUConnected = true

	mono := time.Now()
	mySituation.muAttitude.Lock()
	mySituation.AHRSPitch = ahrsInvalidForTest()
	mySituation.AHRSRoll = ahrsInvalidForTest()
	mySituation.AHRSLastAttitudeTime = mono
	mySituation.muAttitude.Unlock()

	h := buildAHRSHealth(mono, time.Now().UTC())
	if h.PitchDeg != nil || h.RollDeg != nil {
		t.Error("the AHRS library's invalid sentinel must become nil, never a real measurement")
	}
}

// ahrsInvalidForTest returns the same sentinel main/sensors.go's
// isAHRSInvalidValue checks for, without importing the goflying package
// directly into the test (isAHRSInvalidValue already does that check for
// us - this just needs a value that satisfies it).
func ahrsInvalidForTest() float64 {
	return 3276.7
}

func TestBuildBaroHealth_WiresLiveSituationState(t *testing.T) {
	ensureSituationLocks()
	origSettings := globalSettings
	origStatus := globalStatus
	origSituation := mySituation
	defer func() {
		globalSettings = origSettings
		globalStatus = origStatus
		mySituation.BaroTemperature = origSituation.BaroTemperature
		mySituation.BaroPressureAltitude = origSituation.BaroPressureAltitude
		mySituation.BaroVerticalSpeed = origSituation.BaroVerticalSpeed
		mySituation.BaroLastMeasurementTime = origSituation.BaroLastMeasurementTime
		mySituation.BaroSourceType = origSituation.BaroSourceType
	}()

	globalSettings.BMP_Sensor_Enabled = true
	globalStatus.BMPConnected = true

	mono := time.Now()
	mySituation.muBaro.Lock()
	mySituation.BaroTemperature = 21.5
	mySituation.BaroPressureAltitude = 1234.0
	mySituation.BaroVerticalSpeed = -50.0
	mySituation.BaroLastMeasurementTime = mono
	mySituation.BaroSourceType = BARO_TYPE_BMP280
	mySituation.muBaro.Unlock()

	h := buildBaroHealth(mono, time.Now().UTC())
	if h.State != readiness.StateReady {
		t.Errorf("State = %q, want READY: %s", h.State, h.Reason)
	}
	if h.PressureAltitudeFt == nil || *h.PressureAltitudeFt != 1234.0 {
		t.Errorf("PressureAltitudeFt = %v, want 1234", h.PressureAltitudeFt)
	}
	if h.VerticalSpeedFPM == nil || *h.VerticalSpeedFPM != -50.0 {
		t.Errorf("VerticalSpeedFPM = %v, want -50", h.VerticalSpeedFPM)
	}
}

func TestBuildBaroHealth_DisconnectedIsNilNotZero(t *testing.T) {
	ensureSituationLocks()
	origSettings := globalSettings
	origStatus := globalStatus
	origSituation := mySituation
	defer func() {
		globalSettings = origSettings
		globalStatus = origStatus
		mySituation.BaroLastMeasurementTime = origSituation.BaroLastMeasurementTime
	}()

	globalSettings.BMP_Sensor_Enabled = true
	globalStatus.BMPConnected = false
	mySituation.muBaro.Lock()
	mySituation.BaroLastMeasurementTime = time.Time{}
	mySituation.muBaro.Unlock()

	h := buildBaroHealth(time.Time{}, time.Now().UTC())
	if h.State != readiness.StateNotReady {
		t.Errorf("State = %q, want NOT_READY (enabled but disconnected)", h.State)
	}
	if h.PressureAltitudeFt != nil {
		t.Error("PressureAltitudeFt must be nil when disconnected, not a fabricated 0")
	}
}

func TestBuildFanHealth_NoServiceNoStatusFile(t *testing.T) {
	// In this build/test environment there is no stratux_fancontrol
	// systemd unit and no /run/stratux-fancontrol/status.json - both
	// UnitActiveState and ReadFanControllerStatus degrade gracefully
	// rather than erroring the test process.
	if _, err := common.ReadFanControllerStatus(common.FanControllerStatusPath); err == nil {
		t.Skip("a real fancontrol status file exists in this environment; skipping the absence-only assertion")
	}
	h := buildFanHealth(time.Now().UTC())
	if h.State != readiness.StateNotInstalled && h.State != readiness.StateNotReady {
		t.Errorf("State = %q, want NOT_INSTALLED (no unit) or NOT_READY (unit installed but inactive) in a test environment with no fancontrol running", h.State)
	}
	if h.TachometerSupported {
		t.Error("TachometerSupported must always be false")
	}
}
