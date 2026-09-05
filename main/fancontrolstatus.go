/*
fancontrolstatus.go: gathers live AHRS, barometer, and fan-controller
signals and turns them into readiness.AHRSHealth/BaroHealth/FanHealth. All
I/O (mutex-protected globals, the fancontrol status file, systemctl) happens
here; the readiness package itself stays pure so its classification logic
is exercised directly by go test without real hardware.
*/
package main

import (
	"os"
	"time"

	"github.com/stratux/stratux/common"
	"github.com/stratux/stratux/readiness"
)

const (
	// ahrsStaleAfter/baroStaleAfter bound how old the last measurement
	// may be before the corresponding component reads DEGRADED rather
	// than READY. ahrsStaleAfter is a little looser than the existing
	// isAHRSValid() 1-second threshold (main/gps.go) to avoid the health
	// tile flapping between healthUpdateInterval's 5-second ticks on a
	// momentary hiccup; baroStaleAfter matches the existing
	// isTempPressValid() threshold exactly.
	ahrsStaleAfter = 2 * time.Second
	baroStaleAfter = 15 * time.Second

	// fanControllerServiceName is the systemd unit installed by
	// debian/stratux_fancontrol.service.
	fanControllerServiceName = "stratux_fancontrol"
	// fanControllerStaleAfter allows a couple of missed 1Hz status-file
	// writes (see fancontrol_main/fancontrol.go's updateStats) before
	// reading DEGRADED, without masking a genuinely wedged/dead process
	// for long.
	fanControllerStaleAfter = 10 * time.Second
)

// buildAHRSHealth gathers live IMU/AHRS signals under mySituation's own
// existing attitude lock and derives a readiness.AHRSHealth. mono/wallNow
// must be a (stratuxClock, wall-clock) reading pair taken at the same
// instant, the same convention updateHealth already uses for the radio/GPS
// tiles.
func buildAHRSHealth(mono, wallNow time.Time) readiness.AHRSHealth {
	mySituation.muAttitude.Lock()
	lastAttitudeMono := mySituation.AHRSLastAttitudeTime
	pitch := mySituation.AHRSPitch
	roll := mySituation.AHRSRoll
	gLoad := mySituation.AHRSGLoad
	rawStatus := mySituation.AHRSStatus
	mySituation.muAttitude.Unlock()

	// isAHRSInvalidValue (main/sensors.go) is the existing check against
	// the AHRS library's sentinel (goflying/ahrs.Invalid, ~3276.7) - the
	// sentinel is converted to nil here, before it ever reaches the
	// readiness package or the API, so it can never be mistaken for a
	// real measurement (e.g. a level 0.0-degree pitch).
	var pitchPtr, rollPtr, gLoadPtr *float64
	if !isAHRSInvalidValue(pitch) {
		pitchPtr = &pitch
	}
	if !isAHRSInvalidValue(roll) {
		rollPtr = &roll
	}
	if !isAHRSInvalidValue(gLoad) {
		gLoadPtr = &gLoad
	}

	q := globalSettings.SensorQuaternion
	levelCalibrated := q[0] != 0 || q[1] != 0 || q[2] != 0 || q[3] != 0
	d := globalSettings.D
	gyroCalibrated := d[0] != 0 || d[1] != 0 || d[2] != 0

	return readiness.BuildAHRSHealth(
		globalSettings.IMU_Sensor_Enabled, globalStatus.IMUConnected, rawStatus,
		pitchPtr, rollPtr, gLoadPtr,
		lastAttitudeMono, mono, monoToWallOptional(lastAttitudeMono, mono, wallNow),
		levelCalibrated, gyroCalibrated, globalSettings.IMUMapping,
		ahrsStaleAfter,
	)
}

// buildBaroHealth gathers live barometer signals under mySituation's own
// existing baro lock and derives a readiness.BaroHealth.
func buildBaroHealth(mono, wallNow time.Time) readiness.BaroHealth {
	mySituation.muBaro.Lock()
	lastMeasurementMono := mySituation.BaroLastMeasurementTime
	temp := float64(mySituation.BaroTemperature)
	alt := float64(mySituation.BaroPressureAltitude)
	vs := float64(mySituation.BaroVerticalSpeed)
	sourceType := mySituation.BaroSourceType
	mySituation.muBaro.Unlock()

	connected := globalStatus.BMPConnected
	var tempPtr, altPtr, vsPtr *float64
	// Only report values once a real measurement has actually been taken
	// this connection - otherwise these are just zero-valued struct
	// fields, not real readings, and must stay nil/unavailable rather
	// than serialize as a fabricated 0.
	if connected && !lastMeasurementMono.IsZero() {
		tempPtr = &temp
		altPtr = &alt
		vsPtr = &vs
	}

	return readiness.BuildBaroHealth(
		globalSettings.BMP_Sensor_Enabled, connected,
		tempPtr, altPtr, vsPtr, readiness.BaroSourceTypeName(sourceType),
		lastMeasurementMono, mono, monoToWallOptional(lastMeasurementMono, mono, wallNow),
		baroStaleAfter,
	)
}

// buildFanHealth reads the fancontrol daemon's own self-reported runtime
// status (common.FanControllerStatusPath, a small RAM-backed /run file -
// see common/fanstatus.go) and the stratux_fancontrol systemd unit's
// load/active state, and derives a readiness.FanHealth. now is a plain
// wall-clock reading: the fancontrol daemon is a separate process with no
// access to stratuxrun's stratuxClock, so its status file's UpdatedAt is
// necessarily wall-clock, like GPSHealth.LastUpdateAge.
func buildFanHealth(now time.Time) readiness.FanHealth {
	state := readiness.UnitActiveState(fanControllerServiceName)
	installed := readiness.UnitInstalled(state)
	active := state == "active"

	status, err := common.ReadFanControllerStatus(common.FanControllerStatusPath)
	available := err == nil
	malformed := err != nil && !os.IsNotExist(err)

	var cpuTempC, tempTargetC *float64
	var dutyMin, requestedDuty, freq *uint32
	var lastUpdate time.Time
	controllerState := ""
	controllerErr := ""
	if available {
		t := status.CPUTempC
		cpuTempC = &t
		tt := status.TempTargetC
		tempTargetC = &tt
		dm := status.PWMDutyMinPercent
		dutyMin = &dm
		rd := status.RequestedDutyPercent
		requestedDuty = &rd
		f := status.PWMFrequencyHz
		freq = &f
		lastUpdate = status.UpdatedAt
		controllerState = status.ControllerState
		controllerErr = status.Error
	}

	return readiness.BuildFanHealth(
		installed, active, available, malformed,
		controllerState, controllerErr,
		cpuTempC, tempTargetC, dutyMin, requestedDuty, freq,
		lastUpdate, now, fanControllerStaleAfter,
	)
}
