package alerting

import "math"

// Config holds every named, documented threshold and timing constant the
// evaluator uses. Every value here is a project default chosen for
// awareness, not a certified separation standard - see docs/alerting.md's
// "Threshold policy" section.
type Config struct {
	// MasterEnabled gates every alert of every category - false produces
	// no alerts at all (but EvaluateTraffic/EvaluateHealth still update
	// internal bookkeeping so state doesn't jump when re-enabled).
	MasterEnabled bool
	// VisualEnabled gates traffic-notice visual alerts specifically (a
	// user setting distinct from system-health visual alerts, which use
	// SystemVisualEnabled) - muting audio must never affect this.
	VisualEnabled bool
	// SystemVisualEnabled gates system-health visual alerts.
	SystemVisualEnabled bool
	// TrafficAudioEnabled/SystemAudioEnabled independently gate whether
	// traffic or system-health events are ever AudioEligible - both
	// default false until a user gesture enables them (see
	// docs/alerting.md's browser-audio section).
	TrafficAudioEnabled bool
	SystemAudioEnabled  bool

	// Monitoring* bound the outer envelope: a target outside this is
	// never evaluated at all.
	MonitoringHorizontalMeters float64
	MonitoringVerticalFeet     float64

	// Notice*/Caution*/HighCaution* are entry thresholds for each tier,
	// each requiring BOTH the horizontal and vertical bound to be met
	// (conservative AND, not OR).
	NoticeHorizontalMeters float64
	NoticeVerticalFeet     float64

	CautionHorizontalMeters float64
	CautionVerticalFeet     float64

	HighCautionHorizontalMeters float64
	HighCautionVerticalFeet     float64

	// ExitHysteresisFactor widens every entry threshold to produce the
	// corresponding exit threshold (must be > 1.0).
	ExitHysteresisFactor float64

	// DeescalateDwellSeconds is how long a target must remain outside its
	// current tier's exit threshold before the tier actually drops.
	DeescalateDwellSeconds float64

	// ExpireAfterSeconds is how long with no refreshed observation before
	// a target's state is removed entirely (clearing acknowledgement and
	// cooldown memory - see "re-entry" in docs/alerting.md).
	ExpireAfterSeconds float64

	// StaleAfterSeconds is the per-observation freshness bound - an
	// observation older than this is ineligible for evaluation this
	// cycle (distinct from ExpireAfterSeconds, which governs removing
	// tracked state entirely).
	StaleAfterSeconds float64

	// MaxTrackedTargets bounds in-memory target state - matches this
	// project's own documented ~500-target practical ForeFlight limit
	// (see main/traffic.go's sendTrafficUpdates comment).
	MaxTrackedTargets int

	// AudioCooldownSeconds[level] is the minimum spacing between two
	// audio-eligible events for the SAME target at the same level.
	AudioCooldownNoticeSeconds  float64
	AudioCooldownCautionSeconds float64

	// GlobalMinAudioSpacingSeconds bounds how close together ANY two
	// audio-eligible events (any target, any level) may be signaled.
	GlobalMinAudioSpacingSeconds float64

	// SuppressGroundTraffic, when true, makes a target reported OnGround
	// ineligible for CAUTION tiers (still eligible for NOTICE) - off by
	// default, since OnGround reliability varies by source.
	SuppressGroundTraffic bool

	// MaxEventHistory bounds the recent-event list returned by Snapshot.
	MaxEventHistory int
}

// DefaultConfig returns the project's documented conservative defaults -
// see docs/alerting.md's threshold-policy table.
func DefaultConfig() Config {
	const nm = 1852.0 // meters per nautical mile
	return Config{
		MonitoringHorizontalMeters: 5 * nm,
		MonitoringVerticalFeet:     2000,

		NoticeHorizontalMeters: 3 * nm,
		NoticeVerticalFeet:     1200,

		CautionHorizontalMeters: 2 * nm,
		CautionVerticalFeet:     800,

		HighCautionHorizontalMeters: 1 * nm,
		HighCautionVerticalFeet:     500,

		ExitHysteresisFactor: 1.2,

		DeescalateDwellSeconds: 10,
		ExpireAfterSeconds:     30,
		StaleAfterSeconds:      6, // matches sendTrafficUpdates()'s own non-extrapolated "current" window

		MaxTrackedTargets: 500,

		AudioCooldownNoticeSeconds:  60,
		AudioCooldownCautionSeconds: 20,

		GlobalMinAudioSpacingSeconds: 3,

		SuppressGroundTraffic: false,

		MaxEventHistory: 200,
	}
}

// Validate reports whether every field is a finite, sane value - rejecting
// NaN, +/-Inf, negative distances/durations, an exit factor <= 1.0 (which
// would make hysteresis a no-op or inverted), and a zero/negative
// MaxTrackedTargets or MaxEventHistory (which would make the evaluator
// unusable). Mirrors the numeric-range validation
// main/alertsettings.go applies to the persisted settings that produce a
// Config.
func (c Config) Validate() error {
	checks := []struct {
		name string
		v    float64
	}{
		{"MonitoringHorizontalMeters", c.MonitoringHorizontalMeters},
		{"MonitoringVerticalFeet", c.MonitoringVerticalFeet},
		{"NoticeHorizontalMeters", c.NoticeHorizontalMeters},
		{"NoticeVerticalFeet", c.NoticeVerticalFeet},
		{"CautionHorizontalMeters", c.CautionHorizontalMeters},
		{"CautionVerticalFeet", c.CautionVerticalFeet},
		{"HighCautionHorizontalMeters", c.HighCautionHorizontalMeters},
		{"HighCautionVerticalFeet", c.HighCautionVerticalFeet},
		{"DeescalateDwellSeconds", c.DeescalateDwellSeconds},
		{"ExpireAfterSeconds", c.ExpireAfterSeconds},
		{"StaleAfterSeconds", c.StaleAfterSeconds},
		{"AudioCooldownNoticeSeconds", c.AudioCooldownNoticeSeconds},
		{"AudioCooldownCautionSeconds", c.AudioCooldownCautionSeconds},
		{"GlobalMinAudioSpacingSeconds", c.GlobalMinAudioSpacingSeconds},
	}
	for _, chk := range checks {
		if math.IsNaN(chk.v) || math.IsInf(chk.v, 0) {
			return &ConfigError{Field: chk.name, Reason: "must be a finite number"}
		}
		if chk.v < 0 {
			return &ConfigError{Field: chk.name, Reason: "must not be negative"}
		}
	}
	if c.ExitHysteresisFactor <= 1.0 || math.IsNaN(c.ExitHysteresisFactor) || math.IsInf(c.ExitHysteresisFactor, 0) {
		return &ConfigError{Field: "ExitHysteresisFactor", Reason: "must be finite and greater than 1.0"}
	}
	if c.MaxTrackedTargets <= 0 {
		return &ConfigError{Field: "MaxTrackedTargets", Reason: "must be positive"}
	}
	if c.MaxEventHistory <= 0 {
		return &ConfigError{Field: "MaxEventHistory", Reason: "must be positive"}
	}
	// Envelopes must nest: monitoring >= notice >= caution >= high caution,
	// in both dimensions, or the tier logic below would be incoherent.
	if !(c.MonitoringHorizontalMeters >= c.NoticeHorizontalMeters &&
		c.NoticeHorizontalMeters >= c.CautionHorizontalMeters &&
		c.CautionHorizontalMeters >= c.HighCautionHorizontalMeters) {
		return &ConfigError{Field: "horizontal thresholds", Reason: "must satisfy monitoring >= notice >= caution >= high caution"}
	}
	if !(c.MonitoringVerticalFeet >= c.NoticeVerticalFeet &&
		c.NoticeVerticalFeet >= c.CautionVerticalFeet &&
		c.CautionVerticalFeet >= c.HighCautionVerticalFeet) {
		return &ConfigError{Field: "vertical thresholds", Reason: "must satisfy monitoring >= notice >= caution >= high caution"}
	}
	return nil
}

// ConfigError reports which Config field failed validation and why -
// deliberately structured (not just a string) so callers (e.g. the
// settings API) can attribute the error to a specific field.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return "alerting: invalid " + e.Field + ": " + e.Reason
}
