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
// requires is present; fields that depend on hardware not yet installed
// (AHRS, barometer) are pointers so their absence serializes as JSON null,
// not a misleading zero value - a bank angle of exactly 0 is a real,
// meaningful reading, so "no AHRS installed" must never be represented
// the same way.
type Sample struct {
	UTC time.Time

	Latitude          float64
	Longitude         float64
	GPSAltitudeFt     float64
	GPSAccuracyMeters float64
	GroundspeedKt     float64
	CourseDeg         float64

	// Unavailable (nil) until the AHRS board is installed and validated.
	// No UI or export may render these as zero/operational while nil.
	PressureAltitudeFt *float64
	PitchDeg           *float64
	BankDeg            *float64
	VerticalAccelG     *float64
	GLoad              *float64

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
