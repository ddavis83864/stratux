package trafficcpa

// Config bounds what this package's own computation considers computable
// at all - distinct from any alerting ESCALATION policy threshold (see
// alerting.Config for those, e.g. how close a predicted CPA must be to
// actually raise a tier). This Config only controls whether a TCPA/CPA
// estimate is attempted and trusted in the first place.
type Config struct {
	// HorizonSeconds bounds the maximum useful TCPA - a converging
	// target predicted to meet beyond this is reported with
	// TCPAClampedToHorizon=true rather than an arbitrarily-distant,
	// low-value prediction.
	HorizonSeconds float64

	// MinRelativeSpeedKnots is the minimum relative (closure) speed
	// required before a TCPA/CPA estimate is trusted at all - below
	// this, two nearly-equal-velocity or nearly-stationary-relative
	// tracks would produce numerically unstable, low-value predictions.
	// See docs/traffic-cpa-alerting.md's "Numerical stability" section.
	MinRelativeSpeedKnots float64

	// MaxAgeSeconds bounds how old either track's own AgeSeconds may be
	// before this package refuses to compute at all.
	MaxAgeSeconds float64

	// MaxHorizontalSeparationMeters/MaxAbsoluteLatitudeDeg define this
	// package's own documented, conservative approximation envelope
	// (see docs/traffic-cpa-alerting.md's "Coordinate model" section) -
	// inputs outside it are rejected outright rather than silently
	// producing false precision from a flat-earth approximation that no
	// longer holds.
	MaxHorizontalSeparationMeters float64
	MaxAbsoluteLatitudeDeg        float64
}

// DefaultConfig returns conservative defaults. HorizonSeconds/
// MinRelativeSpeedKnots are also the persisted, user-adjustable defaults
// main.DefaultTrafficCPASettings() starts from - see that function's own
// doc comment for the rationale (matched against this project's existing
// alerting thresholds, not invented independently).
func DefaultConfig() Config {
	const nm = 1852.0
	return Config{
		HorizonSeconds:                180, // 3 minutes
		MinRelativeSpeedKnots:         20,
		MaxAgeSeconds:                 6,      // matches alerting.DefaultConfig().StaleAfterSeconds
		MaxHorizontalSeparationMeters: 5 * nm, // matches alerting.DefaultConfig().MonitoringHorizontalMeters
		MaxAbsoluteLatitudeDeg:        80,
	}
}

// Validate reports whether every field is a finite, sane value.
func (c Config) Validate() error {
	checks := []struct {
		name string
		v    float64
	}{
		{"HorizonSeconds", c.HorizonSeconds},
		{"MinRelativeSpeedKnots", c.MinRelativeSpeedKnots},
		{"MaxAgeSeconds", c.MaxAgeSeconds},
		{"MaxHorizontalSeparationMeters", c.MaxHorizontalSeparationMeters},
		{"MaxAbsoluteLatitudeDeg", c.MaxAbsoluteLatitudeDeg},
	}
	for _, chk := range checks {
		if isNonFinite(chk.v) {
			return &ConfigError{Field: chk.name, Reason: "must be a finite number"}
		}
		if chk.v <= 0 {
			return &ConfigError{Field: chk.name, Reason: "must be positive"}
		}
	}
	if c.MaxAbsoluteLatitudeDeg > 90 {
		return &ConfigError{Field: "MaxAbsoluteLatitudeDeg", Reason: "must not exceed 90"}
	}
	return nil
}

// ConfigError reports which Config field failed validation and why.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return "trafficcpa: invalid " + e.Field + ": " + e.Reason
}
