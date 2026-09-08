package autorecord

import "fmt"

// Settings is the full, user-configurable, persisted Automatic Flight
// Recording configuration - see main/autorecordsettings.go for the
// atomically-persisted file this backs, mirroring AlertSettings's own
// established schema-versioned pattern.
//
// Defaults (see DefaultSettings) are deliberately conservative and
// documented in docs/automatic-flight-recording.md's "Threshold and
// dwell rationale" section - they are not lifted verbatim from any
// external convention, but derived from: this project's own GPS update
// rate (roughly 1 Hz - see main/gps.go), typical GPS ground-speed jitter
// on a stationary receiver with a good fix (observed well under 2 kt),
// and the difference between "taxiing" (the earliest point a recording
// should plausibly start) and "a person walking near the aircraft with
// the receiver already powered" (which should not).
type Settings struct {
	SchemaVersion int `json:"schemaVersion"`

	// Enabled is false by default - see docs/automatic-flight-recording.md's
	// explicit "disabled by default" statement. Automatic recording never
	// runs unless the owner explicitly turns this on.
	Enabled bool `json:"enabled"`

	// StartGroundspeedKnots/StartDwellSeconds: a start candidate begins
	// the moment groundspeed is at or above StartGroundspeedKnots: the
	// candidate must then hold continuously for StartDwellSeconds before
	// a recording actually starts. Any sample below the threshold (or
	// invalid/stale) resets the candidate back to StateArmedWaiting - the
	// dwell never partially carries over.
	StartGroundspeedKnots float64 `json:"startGroundspeedKnots"`
	StartDwellSeconds     float64 `json:"startDwellSeconds"`

	// StopGroundspeedKnots/StopDwellSeconds: the stop-side equivalent -
	// must be strictly lower than StartGroundspeedKnots (see Validate) so
	// the two thresholds never overlap, which is this feature's whole
	// hysteresis guarantee against flapping right at one boundary value.
	StopGroundspeedKnots float64 `json:"stopGroundspeedKnots"`
	StopDwellSeconds     float64 `json:"stopDwellSeconds"`

	// GPSLossGraceSeconds: how long a recording stays active with no
	// valid GPS sample before the machine treats the loss as extended -
	// see docs/automatic-flight-recording.md's GPS-loss policy for what
	// happens at each side of this boundary. A recording is never
	// stopped merely because GPS became invalid; only groundspeed
	// dropping to/below the stop threshold (which requires a valid
	// sample to even observe) ever triggers a stop.
	GPSLossGraceSeconds float64 `json:"gpsLossGraceSeconds"`

	// RestartCooldownSeconds: after a manual stop of an automatic
	// recording, or certain error recoveries, the machine will not start
	// a new automatic recording again until this many seconds have
	// elapsed, even if the start condition is already satisfied - see
	// docs/automatic-flight-recording.md's manual-coexistence section.
	RestartCooldownSeconds float64 `json:"restartCooldownSeconds"`

	// MinimumRecordingDurationSeconds is optional (0 disables it): if
	// set, an automatic stop candidate that completes its dwell before
	// the recording has been active this long is deferred (the machine
	// stays in StateRecording, not StateStopCandidate/StateFinalizing)
	// until the minimum is reached. Never applies to a manual stop or a
	// shutdown-triggered stop, which the owner can always do immediately.
	MinimumRecordingDurationSeconds float64 `json:"minimumRecordingDurationSeconds"`
}

// DefaultSettings returns the conservative, disabled-by-default
// configuration - see Settings' own doc comment for the rationale behind
// each value.
func DefaultSettings() Settings {
	return Settings{
		SchemaVersion:                   SettingsSchemaVersion,
		Enabled:                         false,
		StartGroundspeedKnots:           8,
		StartDwellSeconds:               30,
		StopGroundspeedKnots:            4,
		StopDwellSeconds:                120,
		GPSLossGraceSeconds:             30,
		RestartCooldownSeconds:          300,
		MinimumRecordingDurationSeconds: 0,
	}
}

// SettingsSchemaVersion is bumped whenever Settings gains, removes, or
// changes the meaning of a field a consumer should notice - mirroring
// AlertSettingsSchemaVersion/preflight.SchemaVersion's convention.
const SettingsSchemaVersion = 1

// maxBoundSeconds bounds every duration field this package accepts -
// generous enough for any real configuration (2 hours), small enough that
// a fat-fingered or malicious value cannot make the machine effectively
// never start/stop/recover.
const maxBoundSeconds = 2 * 60 * 60

// maxGroundspeedKnots bounds the threshold fields - well above anything
// a piston GA aircraft's ground movement or takeoff/landing groundspeed
// would ever be, so a nonsensical value is rejected rather than silently
// accepted as "never triggers."
const maxGroundspeedKnots = 500

// Validate reports the first problem found with s, or nil if s is safe to
// persist and use. Called before every settings mutation (see
// main/autorecordsettings.go's saveSettings) so an invalid update never
// partially applies.
func (s Settings) Validate() error {
	for name, v := range map[string]float64{
		"startGroundspeedKnots": s.StartGroundspeedKnots,
		"stopGroundspeedKnots":  s.StopGroundspeedKnots,
	} {
		if v < 0 || v > maxGroundspeedKnots {
			return fmt.Errorf("autorecord: %s must be between 0 and %v knots", name, maxGroundspeedKnots)
		}
	}
	for name, v := range map[string]float64{
		"startDwellSeconds":               s.StartDwellSeconds,
		"stopDwellSeconds":                s.StopDwellSeconds,
		"gpsLossGraceSeconds":             s.GPSLossGraceSeconds,
		"restartCooldownSeconds":          s.RestartCooldownSeconds,
		"minimumRecordingDurationSeconds": s.MinimumRecordingDurationSeconds,
	} {
		if v < 0 || v > maxBoundSeconds {
			return fmt.Errorf("autorecord: %s must be between 0 and %v seconds", name, maxBoundSeconds)
		}
	}
	if s.StopGroundspeedKnots >= s.StartGroundspeedKnots {
		return fmt.Errorf("autorecord: stopGroundspeedKnots (%v) must be strictly lower than startGroundspeedKnots (%v)", s.StopGroundspeedKnots, s.StartGroundspeedKnots)
	}
	return nil
}
