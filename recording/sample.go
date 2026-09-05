// Package recording defines the data schema and storage interface for
// future flight recording. This is a foundation only: nothing in this
// package is wired into the running daemon, and no automatic recording is
// enabled by its existence. It exists so the schema, storage layout, and
// export interfaces can be reviewed and tested ahead of actually turning
// recording on, which additionally requires trusted time
// (readiness.TimeTrust reaching TimeGNSSSynced or TimeNetworkSynced) to be
// validated on real hardware first.
package recording

import "time"

// Sample is one recorded data point. Every field the mission's schema
// requires is present; fields that depend on the AHRS/barometer board
// being installed, enabled, connected, and currently producing a valid
// reading are pointers so their absence serializes as JSON null, not a
// misleading zero value - a bank angle of exactly 0 is a real, meaningful
// reading, so "no valid AHRS measurement right now" must never be
// represented the same way.
type Sample struct {
	UTC time.Time
	// TimeTrustState is the readiness.TimeTrustState value in effect when
	// this sample was taken (e.g. "GNSS_SYNCED", "UNSYNCED") - the
	// steady-state trust level, distinct from TimeSourceTransition below,
	// which only marks the samples where that state actually changed.
	TimeTrustState string

	Latitude          float64
	Longitude         float64
	GPSAltitudeFt     float64
	GPSAccuracyMeters float64
	GroundspeedKt     float64
	CourseDeg         float64

	// Unavailable (nil) whenever the AHRS/barometer board is disabled,
	// disconnected, or has not yet produced a valid reading - see
	// main/recordingapi.go's appendRecordingSample, which nils these out
	// under exactly the same conditions readiness.AHRSHealth/BaroHealth
	// itself would report DEGRADED/NOT_READY for. No UI or export may
	// render these as zero/operational while nil.
	PressureAltitudeFt *float64
	PitchDeg           *float64
	BankDeg            *float64
	VerticalAccelG     *float64
	GLoad              *float64
	// BaroVerticalSpeedFPM is the barometric vertical speed, feet per
	// minute (mySituation.BaroVerticalSpeed is already in ft/min - see
	// main/sensors.go's tempAndPressureSender).
	BaroVerticalSpeedFPM *float64
	// GLoadMin/GLoadMax are the running min/max g-load since the AHRS
	// last reset (main/sensors.go's mySituation.AHRSGLoadMin/Max) -
	// distinct from GLoad, the instantaneous reading at sample time.
	GLoadMin *float64
	GLoadMax *float64
	// AHRSStatus is mySituation.AHRSStatus, the raw bitfield
	// main/sensors.go's updateAHRSStatus computes (see
	// docs/readiness-and-time-trust.md for the bit meanings).
	AHRSStatus *uint8
	// AHRSCalibrationState is the readiness.AHRSHealth.State in effect at
	// sample time (e.g. "READY", "DEGRADED") - the calibration/readiness
	// judgment, distinct from the raw AHRSStatus bitfield above.
	AHRSCalibrationState *string
	// AHRSMeasurementAgeSeconds is how old the AHRS attitude solution
	// backing this sample's Pitch/Bank/GLoad fields was at sample time,
	// mirroring readiness.AHRSHealth.LastMeasurementAgeSeconds - nil
	// under the same "never produced a solution" condition that field is
	// nil for.
	AHRSMeasurementAgeSeconds *float64

	UAT978MessageRateLastMinute float64
	ES1090MessageRateLastMinute float64
	FISBTowerCount              int
	FISBProductCounts           map[string]int

	// Empty unless this sample coincides with the named transition, so a
	// consumer can locate exactly where in a recording a health or
	// time-trust state changed without needing a separate event stream.
	SystemHealthTransition string
	TimeSourceTransition   string
}

// HasAHRS reports whether s carries any AHRS-derived field. Every
// AHRS-derived field is set together or not at all - BuildSample (the
// intended constructor once recording is enabled) never sets one without
// the others - so checking any single field is representative, but this
// helper exists so callers do not need to know or duplicate that
// invariant themselves.
func (s Sample) HasAHRS() bool {
	return s.PitchDeg != nil
}

// HasPressureAltitude reports whether s carries a barometer-derived
// pressure altitude.
func (s Sample) HasPressureAltitude() bool {
	return s.PressureAltitudeFt != nil
}
