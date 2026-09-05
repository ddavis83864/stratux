package readiness

import (
	"math"
	"time"
)

// AHRSHealth is the health record for the attitude/heading reference
// system (IMU-derived pitch/roll/g-load). It replaces the former
// FutureHardwareHealth placeholder now that an ICM-20948-based AHRS board
// is a real, supported configuration.
//
// AHRS information here is supplemental and non-certified, matching the
// project's existing framing everywhere else attitude data is surfaced
// (GDL90 AHRS reports, the AHRS browser page) - this health record changes
// nothing about that; it only reports whether the supplemental data can
// currently be trusted.
type AHRSHealth struct {
	State  ComponentState
	Reason string

	// Enabled reflects globalSettings.IMU_Sensor_Enabled - operator
	// configuration, not hardware presence.
	Enabled bool
	// Connected reflects globalStatus.IMUConnected - whether the IMU
	// device is currently open and responding.
	Connected bool
	// DetectionBasis names what evidence Connected/State are derived
	// from, so a caller never has to guess how strong the claim is.
	DetectionBasis string

	// RawStatus is mySituation.AHRSStatus, the bitfield main/sensors.go's
	// updateAHRSStatus already computes, passed through verbatim so a
	// client with the bit meanings documented (docs/readiness-and-time-trust.md)
	// can inspect it directly rather than only trusting this package's
	// own classification.
	RawStatus uint8

	// LastMeasurementTime/LastMeasurementAgeSeconds describe the most
	// recent valid attitude solution. Age is computed on the monotonic
	// clock domain by BuildAHRSHealth, immune to a wall-clock step, the
	// same technique as RadioHealth.LastFrameAgeSeconds. Both are
	// unavailable (null/nil) if no solution has ever been produced.
	LastMeasurementTime       OptionalTime
	LastMeasurementAgeSeconds *float64
	// Stale is true once LastMeasurementAgeSeconds exceeds the caller's
	// staleAfter threshold, or if AHRS is connected but has never
	// produced a solution.
	Stale bool

	// PitchDeg/RollDeg/GLoad are nil whenever the source reading is the
	// AHRS library's sentinel "invalid" value (goflying/ahrs.Invalid,
	// ~3276.7) - see main/sensors.go's isAHRSInvalidValue. The sentinel
	// itself never reaches this package or the API as if it were a real
	// measurement; callers convert it to nil before calling
	// BuildAHRSHealth.
	PitchDeg *float64
	RollDeg  *float64
	GLoad    *float64

	PitchAvailable bool
	RollAvailable  bool
	GLoadAvailable bool

	// HeadingSupported is always false: main/sensors.go deliberately never
	// computes a calibrated magnetic heading (see the TODO at
	// mySituation.AHRSMagHeading = ahrs.Invalid in sensorAttitudeSender),
	// so readiness must never be conditioned on heading - see
	// BuildAHRSHealth, which does not even accept a heading parameter.
	HeadingSupported bool

	// LevelCalibrated/GyroCalibrated report whether a level reference
	// (Set Level -> globalSettings.SensorQuaternion) and a gyro zero-bias
	// (Zero Drift -> globalSettings.D) have been established.
	LevelCalibrated bool
	GyroCalibrated  bool

	// IMUMapping is globalSettings.IMUMapping, passed through for
	// diagnostic/dashboard display.
	IMUMapping [2]int

	// Profile carries which named aircraft calibration profile (see the
	// calprofile package) is active, if the profile subsystem itself is
	// available. A profile-subsystem problem (missing/corrupt store) is
	// reported here, not folded silently into the AHRS hardware State -
	// see BuildAHRSHealth's doc comment for the resulting rollup rule.
	Profile AHRSProfileInfo
}

// AHRSProfileInfo describes the calprofile-package state BuildAHRSHealth
// was given - never that package's own type, so readiness stays free of a
// dependency on calprofile (matching the existing pattern where readiness
// depends on nothing outside itself and the standard library; main/'s
// glue does the translation).
type AHRSProfileInfo struct {
	// Available is false if the profile subsystem itself could not be
	// consulted (missing/corrupt profile store, no active profile set) -
	// distinct from "a profile is active but uncalibrated", which is a
	// normal, expected state reflected in Kind below, not an Available
	// failure.
	Available bool
	// Error explains why Available is false. Empty whenever Available is
	// true.
	Error string

	ID   string
	Name string
	// Kind is one of calprofile.KindMigrated/KindUser/KindUncalibrated,
	// passed through as a plain string so this package need not import
	// calprofile for three constants.
	Kind string
	// LastCalibratedAt is unavailable (null) until this profile's
	// calibration has actually succeeded at least once.
	LastCalibratedAt OptionalTime
}

// BuildAHRSHealth derives an AHRSHealth from already-gathered IMU/AHRS
// signals. It performs no I/O and touches no sentinel values directly -
// pitchDeg/rollDeg/gLoad must already be nil wherever the underlying
// reading equals the AHRS library's invalid sentinel, so that sentinel
// (e.g. 3276.7) is never represented here or in the API as a real
// measurement.
//
// lastMeasurementMono and nowMono must be on the same monotonic clock
// domain (main/'s stratuxClock.Time) - age is computed as their
// difference, immune to a wall-clock step, matching BuildRadioHealth.
// lastMeasurementWall is the best-effort wall-clock display value; pass
// NoTime() if none can be derived yet even when lastMeasurementMono is
// non-zero.
//
// profile carries the active calibration-profile state (see
// AHRSProfileInfo). A profile subsystem problem (profile.Available ==
// false) never turns a genuine hardware failure into something milder,
// and never turns a genuinely absent/disconnected IMU into DEGRADED - it
// only ever downgrades what would otherwise have been StateReady, since
// "the hardware is fine but this software layer on top of it has a
// problem" is exactly DEGRADED, not NOT_READY or a silently-ignored
// READY. This is the "missing/corrupt profile store with working legacy
// fallback -> honest DEGRADED" rule the profile-integration mission
// requires.
func BuildAHRSHealth(enabled, connected bool, rawStatus uint8, pitchDeg, rollDeg, gLoad *float64, lastMeasurementMono, nowMono time.Time, lastMeasurementWall OptionalTime, levelCalibrated, gyroCalibrated bool, imuMapping [2]int, staleAfter time.Duration, profile AHRSProfileInfo) AHRSHealth {
	h := AHRSHealth{
		Enabled:             enabled,
		Connected:           connected,
		DetectionBasis:      "IMU connection flag (globalStatus.IMUConnected) and the last attitude-solution timestamp from main/sensors.go's sensorAttitudeSender",
		RawStatus:           rawStatus,
		LastMeasurementTime: lastMeasurementWall,
		PitchDeg:            pitchDeg,
		RollDeg:             rollDeg,
		GLoad:               gLoad,
		PitchAvailable:      pitchDeg != nil,
		RollAvailable:       rollDeg != nil,
		GLoadAvailable:      gLoad != nil,
		HeadingSupported:    false,
		LevelCalibrated:     levelCalibrated,
		GyroCalibrated:      gyroCalibrated,
		IMUMapping:          imuMapping,
		Profile:             profile,
	}
	if !lastMeasurementMono.IsZero() {
		age := nowMono.Sub(lastMeasurementMono).Seconds()
		h.LastMeasurementAgeSeconds = &age
		h.Stale = age > staleAfter.Seconds()
	} else if connected {
		// Connected but never produced a solution reads the same as
		// stale for display purposes - there is nothing to trust yet.
		h.Stale = true
	}

	switch {
	case !enabled:
		h.State = StateNotInstalled
		h.Reason = "AHRS disabled by configuration"
	case !connected:
		h.State = StateNotReady
		h.Reason = "AHRS enabled but IMU is not connected"
	case lastMeasurementMono.IsZero():
		h.State = StateNotReady
		h.Reason = "AHRS connected but has not produced an attitude solution yet"
	case h.Stale:
		h.State = StateDegraded
		h.Reason = "AHRS connected but the last attitude solution is stale"
	case !h.PitchAvailable || !h.RollAvailable:
		h.State = StateDegraded
		h.Reason = "AHRS connected and recent but pitch/roll are unavailable (invalid sensor value)"
	case !levelCalibrated:
		h.State = StateDegraded
		h.Reason = "AHRS connected and reporting but no level reference has been set (use Set Level)"
	case !gyroCalibrated:
		h.State = StateDegraded
		h.Reason = "AHRS connected and reporting but no gyro zero-drift bias has been set (use Zero Drift)"
	case !profile.Available:
		h.State = StateDegraded
		h.Reason = "AHRS hardware is healthy, but the calibration-profile subsystem is unavailable: " + profile.Error
	default:
		h.State = StateReady
		h.Reason = "AHRS connected and producing recent, valid attitude measurements"
	}
	return h
}

// BaroSourceTypeName renders the BaroSourceType byte main/gps.go's
// BARO_TYPE_* constants define as a short human string, without this
// package needing to import main or duplicate the constant values as
// magic numbers at every call site. The numeric values mirror
// main/gps.go's BARO_TYPE_NONE(0)/BARO_TYPE_BMP280(1)/
// BARO_TYPE_OGNTRACKER(2)/BARO_TYPE_NMEA(3)/BARO_TYPE_ADSBESTIMATE(4)
// exactly - see that file for the authoritative definitions.
func BaroSourceTypeName(sourceType uint8) string {
	switch sourceType {
	case 0:
		return "none"
	case 1:
		return "BMP280 (onboard Stratux AHRS module)"
	case 2:
		return "OGN Tracker (external NMEA)"
	case 3:
		return "other NMEA provider (e.g. SoftRF $PGRMZ)"
	case 4:
		return "estimated from ADS-B target HAE/baro difference"
	default:
		return "unknown"
	}
}

// BaroHealth is the health record for the barometric pressure sensor
// (temperature, pressure altitude, vertical speed). It replaces the
// former FutureHardwareHealth placeholder now that a BMP280-based
// barometer is a real, supported configuration.
type BaroHealth struct {
	State  ComponentState
	Reason string

	Enabled        bool
	Connected      bool
	DetectionBasis string

	LastMeasurementTime       OptionalTime
	LastMeasurementAgeSeconds *float64
	Stale                     bool

	TemperatureC       *float64
	PressureAltitudeFt *float64
	VerticalSpeedFPM   *float64
	SourceType         string

	// NonFinite is true if any present value is NaN/Inf - always a
	// hardware/read problem, never a plausible reading.
	NonFinite bool
	// Implausible is true if a present, finite value falls outside a
	// generous structural bound (see BuildBaroHealth) - not merely
	// unusual cabin-pressure variation, which stays within bounds.
	Implausible bool
}

// Structural plausibility bounds for barometer readings. These are
// deliberately generous - ordinary cabin-pressure/temperature variation in
// a light aircraft must never trip them - and only exist to catch a
// clearly broken sensor (e.g. a stuck ADC or bad I2C read), the same
// intent as the existing >70000ft guard in main/sensors.go's
// tempAndPressureSender.
const (
	baroMinPlausibleAltitudeFt = -2000.0
	baroMaxPlausibleAltitudeFt = 60000.0
	baroMaxPlausibleVSFPM      = 10000.0
	// BMP280/BMP388 datasheet operating range.
	baroMinPlausibleTempC = -40.0
	baroMaxPlausibleTempC = 85.0
)

// BuildBaroHealth derives a BaroHealth from already-gathered barometer
// signals. temperatureC/pressureAltitudeFt/verticalSpeedFPM are nil
// wherever the caller has no reading (e.g. sensor not connected).
//
// lastMeasurementMono/nowMono/lastMeasurementWall follow the same
// monotonic-age convention as BuildAHRSHealth/BuildRadioHealth.
func BuildBaroHealth(enabled, connected bool, temperatureC, pressureAltitudeFt, verticalSpeedFPM *float64, sourceType string, lastMeasurementMono, nowMono time.Time, lastMeasurementWall OptionalTime, staleAfter time.Duration) BaroHealth {
	h := BaroHealth{
		Enabled:             enabled,
		Connected:           connected,
		DetectionBasis:      "pressure-sensor connection flag (globalStatus.BMPConnected) and the last-measurement timestamp from main/sensors.go's tempAndPressureSender",
		LastMeasurementTime: lastMeasurementWall,
		TemperatureC:        temperatureC,
		PressureAltitudeFt:  pressureAltitudeFt,
		VerticalSpeedFPM:    verticalSpeedFPM,
		SourceType:          sourceType,
	}
	if !lastMeasurementMono.IsZero() {
		age := nowMono.Sub(lastMeasurementMono).Seconds()
		h.LastMeasurementAgeSeconds = &age
		h.Stale = age > staleAfter.Seconds()
	} else if connected {
		h.Stale = true
	}

	for _, v := range []*float64{temperatureC, pressureAltitudeFt, verticalSpeedFPM} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			h.NonFinite = true
		}
	}
	if !h.NonFinite {
		if pressureAltitudeFt != nil && (*pressureAltitudeFt < baroMinPlausibleAltitudeFt || *pressureAltitudeFt > baroMaxPlausibleAltitudeFt) {
			h.Implausible = true
		}
		if verticalSpeedFPM != nil && math.Abs(*verticalSpeedFPM) > baroMaxPlausibleVSFPM {
			h.Implausible = true
		}
		if temperatureC != nil && (*temperatureC < baroMinPlausibleTempC || *temperatureC > baroMaxPlausibleTempC) {
			h.Implausible = true
		}
	}

	switch {
	case !enabled:
		h.State = StateNotInstalled
		h.Reason = "barometer disabled by configuration"
	case !connected:
		h.State = StateNotReady
		h.Reason = "barometer enabled but not connected"
	case lastMeasurementMono.IsZero():
		h.State = StateNotReady
		h.Reason = "barometer connected but has not produced a measurement yet"
	case h.NonFinite:
		h.State = StateNotReady
		h.Reason = "barometer reported a non-finite value (NaN/Inf) - likely a failed read"
	case h.Stale:
		h.State = StateDegraded
		h.Reason = "barometer connected but the last measurement is stale"
	case h.Implausible:
		h.State = StateDegraded
		h.Reason = "barometer connected but reported a structurally implausible value"
	default:
		h.State = StateReady
		h.Reason = "barometer connected and producing recent, plausible measurements"
	}
	return h
}

// FanHealth is the health record for the AHRS board's dual-fan PWM
// controller (fancontrol, a separate daemon - see fancontrol_main/). It
// replaces the former FutureHardwareHealth placeholder now that a fan
// controller is physically installed.
//
// This hardware has no tachometer or other rotation-feedback pin -
// TachometerSupported is always false, and State/Reason must never claim
// the physical fan is spinning; they can only ever report that the
// controller is running and what it is commanding.
type FanHealth struct {
	State  ComponentState
	Reason string

	// ServiceInstalled reports whether the stratux_fancontrol systemd unit
	// is loaded at all (LoadState != "not-found"). False on a platform
	// where the unit was never installed/enabled (e.g. a non-Pi dev
	// build, or an older image predating the fan controller) - this is a
	// configuration/platform fact, not a failure, matching how a
	// disabled AHRS/barometer reads NOT_INSTALLED rather than NOT_READY.
	ServiceInstalled bool
	// ServiceActive reports whether the stratux_fancontrol systemd unit
	// is active (running) - the first-order "is the controller process
	// even present" signal. Only meaningful when ServiceInstalled is
	// true.
	ServiceActive bool
	// StatusAvailable reports whether a runtime status file
	// (/run/stratux-fancontrol/status.json) has been read successfully.
	StatusAvailable bool
	// Malformed is true if a status file exists but could not be parsed.
	Malformed bool

	// ControllerState is the fancontrol daemon's own self-reported state
	// string (e.g. "STARTING", "COMMANDING", "IDLE") - see
	// common.FanControllerStatus.
	ControllerState string
	// ControllerError is the daemon's own self-reported error string
	// (e.g. a GPIO open failure), empty when it reports none.
	ControllerError string

	CPUTempC             *float64
	TempTargetC          *float64
	PWMDutyMinPercent    *uint32
	RequestedDutyPercent *uint32
	PWMFrequencyHz       *uint32

	LastUpdateTime       OptionalTime
	LastUpdateAgeSeconds *float64
	Stale                bool

	// TachometerSupported is always false on this hardware revision: no
	// tachometer or other physical rotation-feedback pin exists, so
	// actual fan rotation can never be confirmed, only that PWM commands
	// are being issued. See Reason/dashboard text, which must say so
	// explicitly rather than implying the fan is confirmed spinning.
	TachometerSupported bool
}

// BuildFanHealth derives a FanHealth from the fancontrol systemd unit's
// load/active state and its self-reported runtime status file, already
// read by the caller (main/fancontrolstatus.go). It performs no I/O.
func BuildFanHealth(serviceInstalled, serviceActive, statusAvailable, malformed bool, controllerState, controllerError string, cpuTempC, tempTargetC *float64, pwmDutyMin, requestedDuty, pwmFrequency *uint32, lastUpdateTime, now time.Time, staleAfter time.Duration) FanHealth {
	h := FanHealth{
		ServiceInstalled:     serviceInstalled,
		ServiceActive:        serviceActive,
		StatusAvailable:      statusAvailable,
		Malformed:            malformed,
		ControllerState:      controllerState,
		ControllerError:      controllerError,
		CPUTempC:             cpuTempC,
		TempTargetC:          tempTargetC,
		PWMDutyMinPercent:    pwmDutyMin,
		RequestedDutyPercent: requestedDuty,
		PWMFrequencyHz:       pwmFrequency,
		LastUpdateTime:       SomeTime(lastUpdateTime),
		TachometerSupported:  false,
	}
	if !lastUpdateTime.IsZero() {
		age := now.Sub(lastUpdateTime).Seconds()
		h.LastUpdateAgeSeconds = &age
		h.Stale = age > staleAfter.Seconds()
	}

	switch {
	case !serviceInstalled:
		h.State = StateNotInstalled
		h.Reason = "fan-controller service is not installed on this system"
	case !serviceActive:
		h.State = StateNotReady
		h.Reason = "fan-controller service is installed but not active"
	case !statusAvailable:
		h.State = StateDegraded
		h.Reason = "fan-controller service active but no runtime status has been observed yet"
	case malformed:
		h.State = StateDegraded
		h.Reason = "fan-controller status file present but malformed"
	case controllerError != "":
		h.State = StateNotReady
		h.Reason = "fan controller reported an error: " + controllerError
	case h.Stale:
		h.State = StateDegraded
		h.Reason = "fan-controller status is stale"
	default:
		h.State = StateReady
		h.Reason = "fan controller active (" + controllerState + "); rotation feedback unavailable - no tachometer is installed on this hardware, so physical fan rotation cannot be confirmed"
	}
	return h
}
